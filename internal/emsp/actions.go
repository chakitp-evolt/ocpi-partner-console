package emsp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

type versionInfo struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

type endpointInfo struct {
	Identifier string `json:"identifier"`
	Role       string `json:"role"`
	URL        string `json:"url"`
}

type versionDetailsData struct {
	Version   string         `json:"version"`
	Endpoints []endpointInfo `json:"endpoints"`
}

type credentialsData struct {
	Token string           `json:"token"`
	URL   string           `json:"url"`
	Roles []map[string]any `json:"roles"`
}

func (a *Controller) discover(w http.ResponseWriter, r *http.Request) {
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
	secretKey := "remote_token_a"
	nextStatus := "DISCOVERED"
	if run.Status == "REGISTERED" {
		secretKey = "remote_token_c"
		nextStatus = run.Status
	}
	token, err := secretOrError(r.Context(), a.store, run.ID, secretKey)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "missing credentials", err)
		return
	}
	if config.EvoltVersionsURL == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "Evolt versions URL is required", fmt.Errorf("save setup first"))
		return
	}

	versions, details, findings, err := a.fetchRemoteDetails(r.Context(), run, config, config.EvoltVersionsURL, token)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "version discovery failed", err)
		return
	}
	if err := a.store.UpdateRun(r.Context(), run.ID, run.Name, nextStatus, config); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "update run", err)
		return
	}
	a.changed()
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions, "details": details, "findings": findings})
}

func (a *Controller) register(w http.ResponseWriter, r *http.Request) {
	run, config, tokenA, ok := a.actionContext(w, r, "remote_token_a")
	if !ok {
		return
	}
	credentialsURL, err := a.store.Endpoint(r.Context(), run.ID, "credentials", "")
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "remote credentials endpoint not discovered", err)
		return
	}
	tokenB := ocpi.NewToken()
	if err := a.store.PutSecret(r.Context(), run.ID, "inbound_token_b", tokenB); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "store Token B", err)
		return
	}
	payload := credentialsData{
		Token: tokenB,
		URL:   a.versionsURL(config),
		Roles: a.localRoles(config),
	}
	result, err := a.doOCPI(r.Context(), run, config, "credentials", http.MethodPost, credentialsURL, tokenA, false, payload)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "credentials registration failed", err)
		return
	}
	var remote credentialsData
	if err := jsonData(result.Envelope, &remote); err != nil {
		writeAPIError(w, http.StatusBadGateway, "registration rejected", err)
		return
	}
	if remote.Token == "" || remote.URL == "" || len(remote.Roles) == 0 {
		writeAPIError(w, http.StatusBadGateway, "invalid Credentials response", fmt.Errorf("token, url and roles are required"))
		return
	}
	if err := a.store.PutSecret(r.Context(), run.ID, "remote_token_c", remote.Token); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "store Token C", err)
		return
	}
	config.RemoteRoles = remote.Roles
	for _, role := range remote.Roles {
		if strings.EqualFold(fmt.Sprint(role["role"]), "CPO") {
			config.RemoteIdentity = map[string]any{"country_code": role["country_code"], "party_id": role["party_id"], "role": "CPO", "business_details": role["business_details"]}
			break
		}
	}
	if err := a.store.UpdateRun(r.Context(), run.ID, run.Name, "REGISTERED", config); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "update registration state", err)
		return
	}
	a.changed()
	writeJSON(w, http.StatusCreated, map[string]any{"status": "REGISTERED", "remote": map[string]any{"url": remote.URL, "roles": remote.Roles, "token": remote.Token}, "findings": result.Findings})
}

func (a *Controller) pushToken(w http.ResponseWriter, r *http.Request) {
	run, config, tokenC, ok := a.actionContext(w, r, "remote_token_c")
	if !ok {
		return
	}
	var input struct {
		UID   string         `json:"uid"`
		Type  string         `json:"type"`
		Patch map[string]any `json:"patch,omitempty"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON", err)
		return
	}
	if input.Type == "" {
		input.Type = "RFID"
	}
	token, err := a.store.Token(r.Context(), run.ID, input.UID, input.Type)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "token not found", err)
		return
	}
	endpoint, err := a.store.Endpoint(r.Context(), run.ID, "tokens", "RECEIVER")
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "remote Tokens RECEIVER endpoint not discovered", err)
		return
	}
	method := http.MethodPut
	payload := any(token)
	if input.Patch != nil {
		method = http.MethodPatch
		input.Patch["last_updated"] = ocpi.FormatTime(time.Now())
		payload = input.Patch
	}
	target := strings.TrimRight(endpoint, "/") + "/" + url.PathEscape(fmt.Sprint(token["country_code"])) + "/" + url.PathEscape(fmt.Sprint(token["party_id"])) + "/" + url.PathEscape(input.UID) + "?type=" + url.QueryEscape(input.Type)
	result, err := a.doOCPI(r.Context(), run, config, "tokens", method, target, tokenC, true, payload)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "push Token failed", err)
		return
	}
	if result.Envelope.StatusCode >= 2000 {
		writeAPIError(w, http.StatusUnprocessableEntity, "remote rejected Token", fmt.Errorf("OCPI %d: %s", result.Envelope.StatusCode, result.Envelope.StatusMessage))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remote_status": result.Envelope, "findings": result.Findings})
}

func (a *Controller) pullModule(w http.ResponseWriter, r *http.Request) {
	run, config, tokenC, ok := a.actionContext(w, r, "remote_token_c")
	if !ok {
		return
	}
	var input struct {
		Module   string `json:"module"`
		DateFrom string `json:"date_from,omitempty"`
		DateTo   string `json:"date_to,omitempty"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON", err)
		return
	}
	if input.Module != "locations" && input.Module != "tariffs" && input.Module != "sessions" && input.Module != "cdrs" {
		writeAPIError(w, http.StatusUnprocessableEntity, "unsupported pull module", fmt.Errorf("%q", input.Module))
		return
	}
	endpoint, err := a.store.Endpoint(r.Context(), run.ID, input.Module, "SENDER")
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "remote Sender endpoint not discovered", err)
		return
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	if input.DateFrom != "" {
		query.Set("date_from", input.DateFrom)
	} else if input.Module == "sessions" {
		query.Set("date_from", "1970-01-01T00:00:00Z")
	}
	if input.DateTo != "" {
		query.Set("date_to", input.DateTo)
	}
	query.Set("limit", "10")
	parsed.RawQuery = query.Encode()
	objects := []map[string]any{}
	findings := []ocpi.Finding{}
	next := parsed.String()
	pages := 0
	for next != "" {
		pages++
		if pages > 1000 {
			writeAPIError(w, http.StatusBadGateway, "pull pagination failed", fmt.Errorf("more than 1000 pages"))
			return
		}
		result, requestErr := a.doOCPI(r.Context(), run, config, input.Module, http.MethodGet, next, tokenC, true, nil)
		if requestErr != nil {
			writeAPIError(w, http.StatusBadGateway, "pull failed", requestErr)
			return
		}
		var page []map[string]any
		if err := jsonData(result.Envelope, &page); err != nil {
			writeAPIError(w, http.StatusBadGateway, "remote rejected pull", err)
			return
		}
		objects = append(objects, page...)
		findings = append(findings, result.Findings...)
		next = nextPageURL(result.Headers.Get("Link"), next)
	}
	stored := 0
	for _, object := range objects {
		cc, pid, id := fmt.Sprint(object["country_code"]), fmt.Sprint(object["party_id"]), fmt.Sprint(object["id"])
		if cc == "<nil>" || pid == "<nil>" || id == "<nil>" {
			continue
		}
		if err := a.store.PutObject(r.Context(), run.ID, input.Module, cc, pid, id, object, false); err == nil {
			stored++
		}
	}
	a.changed()
	writeJSON(w, http.StatusOK, map[string]any{"received": len(objects), "stored": stored, "pages": pages, "findings": findings})
}

func (a *Controller) fetchRemoteDetails(ctx context.Context, run store.Run, config Config, versionsURL, token string) ([]versionInfo, versionDetailsData, []ocpi.Finding, error) {
	result, err := a.doOCPI(ctx, run, config, "versions", http.MethodGet, versionsURL, token, false, nil)
	if err != nil {
		return nil, versionDetailsData{}, nil, err
	}
	var versions []versionInfo
	if err := jsonData(result.Envelope, &versions); err != nil {
		return nil, versionDetailsData{}, result.Findings, err
	}
	detailsURL := ""
	for _, version := range versions {
		if version.Version == ocpi.Version {
			detailsURL = version.URL
			break
		}
	}
	if detailsURL == "" {
		return versions, versionDetailsData{}, result.Findings, fmt.Errorf("OCPI 2.2.1 is not advertised")
	}
	detailsResult, err := a.doOCPI(ctx, run, config, "versions", http.MethodGet, detailsURL, token, false, nil)
	if err != nil {
		return versions, versionDetailsData{}, result.Findings, err
	}
	findings := append(result.Findings, detailsResult.Findings...)
	var details versionDetailsData
	if err := jsonData(detailsResult.Envelope, &details); err != nil {
		return versions, details, findings, err
	}
	if details.Version != ocpi.Version {
		return versions, details, findings, fmt.Errorf("Version Details mismatch: expected 2.2.1, got %q", details.Version)
	}
	endpoints := make([]map[string]any, 0, len(details.Endpoints))
	for _, endpoint := range details.Endpoints {
		endpoints = append(endpoints, map[string]any{"identifier": endpoint.Identifier, "role": endpoint.Role, "url": endpoint.URL})
	}
	if err := a.store.ReplaceEndpoints(ctx, run.ID, "remote", endpoints); err != nil {
		return versions, details, findings, err
	}
	return versions, details, findings, nil
}

func nextPageURL(link, current string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) && !strings.Contains(part, "rel=next") {
			continue
		}
		start, end := strings.Index(part, "<"), strings.Index(part, ">")
		if start >= 0 && end > start {
			target, err := url.Parse(strings.TrimSpace(part[start+1 : end]))
			if err != nil {
				return ""
			}
			base, err := url.Parse(current)
			if err != nil {
				return ""
			}
			return base.ResolveReference(target).String()
		}
	}
	return ""
}

func (a *Controller) sendCommand(w http.ResponseWriter, r *http.Request) {
	run, config, tokenC, ok := a.actionContext(w, r, "remote_token_c")
	if !ok {
		return
	}
	var input struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON", err)
		return
	}
	input.Type = strings.ToUpper(input.Type)
	if input.Type != "START_SESSION" && input.Type != "STOP_SESSION" {
		writeAPIError(w, http.StatusUnprocessableEntity, "only START_SESSION and STOP_SESSION are exposed in the walkthrough", fmt.Errorf("got %q", input.Type))
		return
	}
	endpoint, err := a.store.Endpoint(r.Context(), run.ID, "commands", "RECEIVER")
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "remote Commands RECEIVER endpoint not discovered", err)
		return
	}
	id := ocpi.NewID()
	responseURL := strings.TrimRight(config.PartnerPublicURL, "/") + "/ocpi/emsp/2.2.1/commands/callback/" + id
	input.Payload["response_url"] = responseURL
	if input.Type == "START_SESSION" {
		uid, _ := input.Payload["token_uid"].(string)
		tokenType, _ := input.Payload["token_type"].(string)
		if tokenType == "" {
			tokenType = "APP_USER"
		}
		token, tokenErr := a.store.Token(r.Context(), run.ID, uid, tokenType)
		if tokenErr != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "command Token not found", tokenErr)
			return
		}
		delete(input.Payload, "token_uid")
		delete(input.Payload, "token_type")
		input.Payload["token"] = token
	}
	if err := a.store.CreateCommand(r.Context(), id, run.ID, input.Type, responseURL, input.Payload); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "store command", err)
		return
	}
	target := strings.TrimRight(endpoint, "/") + "/" + input.Type
	result, err := a.doOCPI(r.Context(), run, config, "commands", http.MethodPost, target, tokenC, true, input.Payload)
	if err != nil {
		_ = a.store.UpdateCommandDirect(r.Context(), id, "TRANSPORT_ERROR", 0, map[string]any{"error": err.Error()})
		writeAPIError(w, http.StatusBadGateway, "command failed", err)
		return
	}
	var direct map[string]any
	if err := jsonData(result.Envelope, &direct); err != nil {
		_ = a.store.UpdateCommandDirect(r.Context(), id, "REJECTED", 0, envelopeMap(*result.Envelope))
		writeAPIError(w, http.StatusUnprocessableEntity, "command rejected", err)
		return
	}
	timeout := intNumber(direct["timeout"])
	status := fmt.Sprint(direct["result"])
	if err := a.store.UpdateCommandDirect(r.Context(), id, status, timeout, direct); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "update command", err)
		return
	}
	a.changed()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "direct_response": direct, "response_url": responseURL, "findings": result.Findings})
}

func (a *Controller) actionContext(w http.ResponseWriter, r *http.Request, secretKey string) (store.Run, Config, string, bool) {
	run, err := a.run(r)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load run", err)
		return run, Config{}, "", false
	}
	config, err := configFromRun(run)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "parse config", err)
		return run, config, "", false
	}
	secret, err := secretOrError(r.Context(), a.store, run.ID, secretKey)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "missing credentials", err)
		return run, config, "", false
	}
	return run, config, secret, true
}

func intNumber(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case json.Number:
		result, _ := number.Int64()
		return int(result)
	default:
		return 0
	}
}

// checkCredentials reads the Credentials object Evolt currently exposes for this registered
// connection. It is a diagnostic GET only: it neither issues nor replaces either direction's token.
func (a *Controller) checkCredentials(w http.ResponseWriter, r *http.Request) {
	run, config, tokenC, ok := a.actionContext(w, r, "remote_token_c")
	if !ok {
		return
	}
	if run.Status != "REGISTERED" {
		writeAPIError(w, http.StatusUnprocessableEntity, "credentials check needs a registered connection", fmt.Errorf("current status is %s", run.Status))
		return
	}
	credentialsURL, err := a.store.Endpoint(r.Context(), run.ID, "credentials", "")
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "remote credentials endpoint not discovered", err)
		return
	}
	result, err := a.doOCPI(r.Context(), run, config, "credentials", http.MethodGet, credentialsURL, tokenC, false, nil)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "credentials check failed", err)
		return
	}
	var remote credentialsData
	if err := jsonData(result.Envelope, &remote); err != nil {
		writeAPIError(w, http.StatusBadGateway, "invalid Credentials response", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "CURRENT",
		"remote":   map[string]any{"token": remote.Token, "url": remote.URL, "roles": remote.Roles},
		"findings": result.Findings,
	})
}

// rotateCredentials is the Inbound half of what the CPO lab already offers: the partner, not Evolt,
// re-keys a connection that is already registered. OCPI 2.2.1 §7.1.2 makes PUT symmetric — once
// registered, either side may rotate, and the initiator of the original handshake does not matter.
// One PUT replaces both directions at once: we mint a new Token B, Evolt answers with a new Token C.
func (a *Controller) rotateCredentials(w http.ResponseWriter, r *http.Request) {
	run, config, tokenC, ok := a.actionContext(w, r, "remote_token_c")
	if !ok {
		return
	}
	// The spec answers 405 for a PUT on a connection that was never registered, so there is nothing
	// to gain by sending one — refusing here keeps the mock from producing a request it knows is bad.
	if run.Status != "REGISTERED" {
		writeAPIError(w, http.StatusUnprocessableEntity, "rotate needs a registered connection", fmt.Errorf("current status is %s", run.Status))
		return
	}
	credentialsURL, err := a.store.Endpoint(r.Context(), run.ID, "credentials", "")
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "remote credentials endpoint not discovered", err)
		return
	}
	a.inboundRotationMu.Lock()
	defer a.inboundRotationMu.Unlock()

	tokenB := ocpi.NewToken()
	payload := credentialsData{
		Token: tokenB,
		URL:   a.versionsURL(config),
		Roles: a.localRoles(config),
	}
	clearStagedToken := a.stageInboundToken(tokenB)
	defer clearStagedToken()
	// Authenticated with the token Evolt issued us, carrying the token we are issuing Evolt.
	result, err := a.doOCPI(r.Context(), run, config, "credentials", http.MethodPut, credentialsURL, tokenC, false, payload)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "credentials rotation failed", err)
		return
	}
	var remote credentialsData
	if err := jsonData(result.Envelope, &remote); err != nil {
		writeAPIError(w, http.StatusBadGateway, "rotation rejected", err)
		return
	}
	if remote.Token == "" {
		writeAPIError(w, http.StatusBadGateway, "invalid Credentials response", fmt.Errorf("token is required"))
		return
	}

	// The staged token is memory-only and the current credential is replaced only after Evolt accepts
	// the PUT. A failed rotation therefore leaves the previous connection usable.
	if err := a.store.PutSecret(r.Context(), run.ID, "inbound_token_b", tokenB); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "store Token B", err)
		return
	}
	if err := a.store.PutSecret(r.Context(), run.ID, "remote_token_c", remote.Token); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "store Token C", err)
		return
	}
	a.changed()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ROTATED", "findings": result.Findings})
}
