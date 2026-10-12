package emsp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

type ocpiResult struct {
	HTTPStatus int
	Envelope   *ocpi.Envelope
	Headers    http.Header
	RawBody    []byte
	Findings   []ocpi.Finding
}

type handlerResponse struct {
	HTTPStatus int
	Envelope   ocpi.Envelope
	Headers    http.Header
	Findings   []ocpi.Finding
}

type ocpiHandler func(http.ResponseWriter, *http.Request, store.Run, Config, map[string]any) handlerResponse

func (a *Controller) inbound(module string, functional bool, handler ocpiHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		run, err := a.run(r)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "load run", err)
			return
		}
		config, err := configFromRun(run)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "parse config", err)
			return
		}

		requestBody, bodyError := readJSONBody(r)
		findings := ocpi.ValidateRequestHeaders(r.Method, r.Header, functional)
		if bodyError != nil {
			findings = append(findings, ocpi.Finding{Level: "error", Rule: "S1 §5", Message: "invalid JSON: " + bodyError.Error()})
		}

		authError := a.authorizeInbound(r.Context(), run.ID, r.Header.Get("Authorization"), config.RawTokenCompatibility)
		if authError != nil {
			response := handlerResponse{HTTPStatus: http.StatusUnauthorized, Envelope: ocpi.Error(2000, authError.Error()), Findings: append(findings, ocpi.Finding{Level: "error", Rule: "S1 §4.1.2", Message: authError.Error()})}
			a.finishInbound(w, r, run.ID, module, started, requestBody, response)
			return
		}
		if bodyError != nil {
			response := handlerResponse{HTTPStatus: http.StatusBadRequest, Envelope: ocpi.Error(2001, "invalid JSON"), Findings: findings}
			a.finishInbound(w, r, run.ID, module, started, requestBody, response)
			return
		}
		if config.Strict && ocpi.HasErrors(findings) {
			response := handlerResponse{HTTPStatus: http.StatusOK, Envelope: ocpi.Error(2001, "required OCPI headers are missing or invalid"), Findings: findings}
			a.finishInbound(w, r, run.ID, module, started, requestBody, response)
			return
		}
		response := handler(w, r, run, config, requestBody)
		response.Findings = append(findings, response.Findings...)
		a.finishInbound(w, r, run.ID, module, started, requestBody, response)
	})
}

func (a *Controller) finishInbound(w http.ResponseWriter, r *http.Request, runID, module string, started time.Time, requestBody map[string]any, response handlerResponse) {
	if response.HTTPStatus == 0 {
		response.HTTPStatus = http.StatusOK
	}
	if response.Headers == nil {
		response.Headers = http.Header{}
	}
	response.Headers.Set("Content-Type", "application/json")
	response.Headers.Set("X-Request-ID", r.Header.Get("X-Request-ID"))
	response.Headers.Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
	if module != "versions" && module != "credentials" {
		response.Headers.Set("OCPI-to-party-id", r.Header.Get("OCPI-from-party-id"))
		response.Headers.Set("OCPI-to-country-code", r.Header.Get("OCPI-from-country-code"))
		response.Headers.Set("OCPI-from-party-id", r.Header.Get("OCPI-to-party-id"))
		response.Headers.Set("OCPI-from-country-code", r.Header.Get("OCPI-to-country-code"))
	}
	for key, values := range response.Headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	responseBody, _ := json.Marshal(response.Envelope)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.HTTPStatus)
	_, _ = w.Write(responseBody)
	status := response.Envelope.StatusCode
	_ = a.store.AddExchange(r.Context(), store.Exchange{
		RunID: runID, Direction: "INBOUND", Module: module, Method: r.Method, URL: absoluteRequestURL(r), HTTPStatus: response.HTTPStatus,
		RequestHeaders: ocpi.Headers(r.Header), RequestBody: requestBody, ResponseHeaders: ocpi.Headers(response.Headers), ResponseBody: envelopeMap(response.Envelope),
		OCPIStatusCode: &status, Findings: response.Findings, Duration: time.Since(started),
	})
	a.changed()
}

func (a *Controller) authorizeInbound(ctx context.Context, runID, header string, allowRaw bool) error {
	provided, err := ocpi.DecodeCredential(header, allowRaw)
	if err != nil {
		return err
	}
	keys := []string{"inbound_token_b", "mock_initial_token"}
	for _, key := range keys {
		expected, secretErr := a.store.GetSecret(ctx, runID, key)
		if secretErr == nil && subtleEqual(provided, expected) {
			return nil
		}
	}
	a.stagedInboundMu.RLock()
	staged := a.stagedInboundToken
	a.stagedInboundMu.RUnlock()
	if staged != "" && subtleEqual(provided, staged) {
		return nil
	}
	return fmt.Errorf("unknown credentials token")
}

func (a *Controller) stageInboundToken(token string) func() {
	a.stagedInboundMu.Lock()
	a.stagedInboundToken = token
	a.stagedInboundMu.Unlock()
	if a.profile == "dual" {
		a.cpo.StageInboundToken(token)
	}
	return func() {
		a.stagedInboundMu.Lock()
		if a.stagedInboundToken == token {
			a.stagedInboundToken = ""
		}
		a.stagedInboundMu.Unlock()
		if a.profile == "dual" {
			a.cpo.StageInboundToken("")
		}
	}
}

func (a *Controller) doOCPI(ctx context.Context, run store.Run, config Config, module, method, target, token string, functional bool, payload any) (ocpiResult, error) {
	started := time.Now()
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return ocpiResult{}, fmt.Errorf("invalid OCPI endpoint URL")
	}
	var body io.Reader
	var requestBody any
	if payload != nil {
		raw, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return ocpiResult{}, marshalErr
		}
		body = bytes.NewReader(raw)
		requestBody = payload
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return ocpiResult{}, err
	}
	if config.RawTokenCompatibility {
		request.Header.Set("Authorization", "Token "+token)
	} else {
		request.Header.Set("Authorization", ocpi.EncodeCredential(token))
	}
	requestID, correlationID := ocpi.MessageHeaders()
	request.Header.Set("X-Request-ID", requestID)
	request.Header.Set("X-Correlation-ID", correlationID)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if functional {
		request.Header.Set("OCPI-from-country-code", config.CountryCode)
		request.Header.Set("OCPI-from-party-id", config.PartyID)
		remoteCC, remotePID := remoteIdentity(run)
		request.Header.Set("OCPI-to-country-code", remoteCC)
		request.Header.Set("OCPI-to-party-id", remotePID)
	}
	response, err := a.client.Do(request)
	if err != nil {
		finding := []ocpi.Finding{{Level: "error", Rule: "transport", Message: err.Error()}}
		_ = a.store.AddExchange(ctx, store.Exchange{RunID: run.ID, Direction: "OUTBOUND", Module: module, Method: method, URL: target, HTTPStatus: 0, RequestHeaders: ocpi.Headers(request.Header), RequestBody: requestBody, ResponseHeaders: map[string]any{}, Findings: finding, Duration: time.Since(started)})
		a.changed()
		return ocpiResult{Findings: finding}, err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, ocpi.MaxBody+1)
	responseBody, readErr := io.ReadAll(limited)
	if readErr != nil {
		return ocpiResult{}, readErr
	}
	if len(responseBody) > ocpi.MaxBody {
		return ocpiResult{}, fmt.Errorf("OCPI response exceeds 1 MiB")
	}
	findings, envelope := ocpi.ValidateResponse(request.Header, response.Header, functional, responseBody, response.StatusCode)
	var responseJSON any
	_ = json.Unmarshal(responseBody, &responseJSON)
	var status *int
	if envelope != nil {
		status = &envelope.StatusCode
	}
	_ = a.store.AddExchange(ctx, store.Exchange{
		RunID: run.ID, Direction: "OUTBOUND", Module: module, Method: method, URL: target, HTTPStatus: response.StatusCode,
		RequestHeaders: ocpi.Headers(request.Header), RequestBody: requestBody, ResponseHeaders: ocpi.Headers(response.Header), ResponseBody: responseJSON,
		OCPIStatusCode: status, Findings: findings, Duration: time.Since(started),
	})
	a.changed()
	result := ocpiResult{HTTPStatus: response.StatusCode, Envelope: envelope, Headers: response.Header.Clone(), RawBody: responseBody, Findings: findings}
	if envelope == nil {
		return result, fmt.Errorf("remote response is not an OCPI envelope")
	}
	return result, nil
}

func readJSONBody(r *http.Request) (map[string]any, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return map[string]any{}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, ocpi.MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > ocpi.MaxBody {
		return nil, fmt.Errorf("request body exceeds 1 MiB")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	return body, nil
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var result byte
	for index := range a {
		result |= a[index] ^ b[index]
	}
	return result == 0
}

func absoluteRequestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

func envelopeMap(envelope ocpi.Envelope) map[string]any {
	body, _ := json.Marshal(envelope)
	var result map[string]any
	_ = json.Unmarshal(body, &result)
	return result
}

func remoteIdentity(run store.Run) (string, string) {
	var config map[string]any
	_ = json.Unmarshal(run.Config, &config)
	if remote, ok := config["remote_identity"].(map[string]any); ok {
		return fmt.Sprint(remote["country_code"]), fmt.Sprint(remote["party_id"])
	}
	return "TH", "EVT"
}

func jsonData(envelope *ocpi.Envelope, target any) error {
	if envelope == nil {
		return fmt.Errorf("missing OCPI envelope")
	}
	if envelope.StatusCode < 1000 || envelope.StatusCode >= 2000 {
		return fmt.Errorf("OCPI status %d: %s", envelope.StatusCode, envelope.StatusMessage)
	}
	raw, err := json.Marshal(envelope.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func secretOrError(ctx context.Context, repository *store.Store, runID, key string) (string, error) {
	value, err := repository.GetSecret(ctx, runID, key)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("%s is not configured", key)
		}
		return "", err
	}
	return value, nil
}
