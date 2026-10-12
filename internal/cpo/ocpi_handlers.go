package cpo

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

type inboundResponse struct {
	HTTPStatus int
	Envelope   ocpi.Envelope
	Headers    http.Header
	Findings   []ocpi.Finding
}

type inboundHandler func(*http.Request, map[string]any) inboundResponse

func (c *Controller) inbound(module string, functional bool, handler inboundHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		body, bodyErr := readBody(r)
		findings := ocpi.ValidateRequestHeaders(r.Method, r.Header, functional)
		if bodyErr != nil {
			findings = append(findings, ocpi.Finding{Level: "error", Rule: "OCPI 2.2.1 §4.1.4", Message: "invalid JSON: " + bodyErr.Error()})
		}
		if err := c.authorize(r.Header.Get("Authorization")); err != nil {
			response := inboundResponse{HTTPStatus: http.StatusUnauthorized, Envelope: ocpi.Error(2000, err.Error()), Findings: findings}
			c.finishInbound(w, r, module, functional, started, body, response)
			return
		}
		if bodyErr != nil {
			response := inboundResponse{HTTPStatus: http.StatusBadRequest, Envelope: ocpi.Error(2001, "invalid JSON"), Findings: findings}
			c.finishInbound(w, r, module, functional, started, body, response)
			return
		}
		response := handler(r, body)
		response.Findings = append(findings, response.Findings...)
		c.finishInbound(w, r, module, functional, started, body, response)
	})
}

func (c *Controller) finishInbound(w http.ResponseWriter, r *http.Request, module string, functional bool, started time.Time, requestBody map[string]any, response inboundResponse) {
	if response.HTTPStatus == 0 {
		response.HTTPStatus = http.StatusOK
	}
	if response.Headers == nil {
		response.Headers = http.Header{}
	}
	response.Headers.Set("Content-Type", "application/json")
	response.Headers.Set("X-Request-ID", r.Header.Get("X-Request-ID"))
	response.Headers.Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
	if functional {
		response.Headers.Set("OCPI-to-country-code", r.Header.Get("OCPI-from-country-code"))
		response.Headers.Set("OCPI-to-party-id", r.Header.Get("OCPI-from-party-id"))
		response.Headers.Set("OCPI-from-country-code", r.Header.Get("OCPI-to-country-code"))
		response.Headers.Set("OCPI-from-party-id", r.Header.Get("OCPI-to-party-id"))
	}
	for key, values := range response.Headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.HTTPStatus)
	_ = json.NewEncoder(w).Encode(response.Envelope)
	c.addExchange(Exchange{
		ID: ocpi.NewID(), Direction: "INBOUND", Module: module, Method: r.Method, URL: requestURL(r), HTTPStatus: response.HTTPStatus, OCPIStatusCode: response.Envelope.StatusCode,
		RequestHeaders: ocpi.Headers(r.Header), RequestBody: requestBody, ResponseHeaders: ocpi.Headers(response.Headers), ResponseBody: envelopeAsMap(response.Envelope),
		Findings: response.Findings, DurationMS: time.Since(started).Milliseconds(), CreatedAt: now(),
	})
}

func (c *Controller) authorize(header string) error {
	c.mu.RLock()
	expected, staged, initial, allowRaw := c.state.TokenB, c.state.StagedTokenB, c.state.InitialToken, c.state.Config.RawTokenCompatibility
	c.mu.RUnlock()
	if expected == "" && staged == "" && initial == "" {
		return fmt.Errorf("Partner CPO has no active credentials")
	}
	provided, err := ocpi.DecodeCredential(header, allowRaw)
	if err != nil {
		return err
	}
	activeMatch := expected != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
	stagedMatch := staged != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(staged)) == 1
	initialMatch := initial != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(initial)) == 1
	if !activeMatch && !stagedMatch && !initialMatch {
		return fmt.Errorf("unknown credentials token")
	}
	return nil
}

func (c *Controller) versions(_ *http.Request, _ map[string]any) inboundResponse {
	c.mu.RLock()
	base := strings.TrimRight(c.state.Config.PartnerPublicURL, "/")
	c.mu.RUnlock()
	return inboundResponse{Envelope: ocpi.Success([]map[string]any{{"version": ocpi.Version, "url": base + "/ocpi/cpo/2.2.1"}})}
}

func (c *Controller) versionDetails(_ *http.Request, _ map[string]any) inboundResponse {
	c.mu.RLock()
	base := strings.TrimRight(c.state.Config.PartnerPublicURL, "/") + "/ocpi/cpo/2.2.1"
	c.mu.RUnlock()
	return inboundResponse{Envelope: ocpi.Success(map[string]any{
		"version": ocpi.Version,
		"endpoints": []map[string]any{
			{"identifier": "credentials", "role": "SENDER", "url": base + "/credentials"},
			{"identifier": "locations", "role": "SENDER", "url": base + "/locations"},
			{"identifier": "tariffs", "role": "SENDER", "url": base + "/tariffs"},
			{"identifier": "sessions", "role": "SENDER", "url": base + "/sessions"},
			{"identifier": "cdrs", "role": "SENDER", "url": base + "/cdrs"},
			{"identifier": "tokens", "role": "RECEIVER", "url": base + "/tokens"},
			{"identifier": "commands", "role": "RECEIVER", "url": base + "/commands"},
		},
	})}
}

func (c *Controller) credentials(r *http.Request, body map[string]any) inboundResponse {
	c.mu.RLock()
	status, config, localToken := c.state.Status, c.state.Config, c.state.TokenB
	c.mu.RUnlock()
	switch r.Method {
	case http.MethodGet:
		if status != "REGISTERED" {
			return inboundResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "not registered")}
		}
		return inboundResponse{Envelope: ocpi.Success(credentialsPayload(config, localToken))}
	case http.MethodPost:
		if status == "REGISTERED" {
			return inboundResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "already registered")}
		}
		return c.acceptRemoteCredentials(r, body)
	case http.MethodPut:
		// OCPI 2.2.1 §7.1.2: PUT re-keys a connection, it does not open one. Without this a peer
		// holding only Token A could register through the back door and skip POST entirely, which is
		// the one method the spec allows exactly once.
		if status != "REGISTERED" {
			return inboundResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "not registered")}
		}
		return c.acceptRemoteCredentials(r, body)
	case http.MethodDelete:
		// Same rule: unregistering something that was never registered is 405, not a silent success.
		if status != "REGISTERED" {
			return inboundResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "not registered")}
		}
		c.mu.Lock()
		c.state.InitialToken, c.state.TokenB, c.state.TokenC, c.state.Status = "", "", "", "UNREGISTERED"
		c.mu.Unlock()
		return inboundResponse{Envelope: ocpi.Success(nil)}
	default:
		return inboundResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "method not allowed")}
	}
}

func (c *Controller) acceptRemoteCredentials(r *http.Request, body map[string]any) inboundResponse {
	remoteToken, versionsURL := fmt.Sprint(body["token"]), fmt.Sprint(body["url"])
	roles, ok := body["roles"].([]any)
	if remoteToken == "" || versionsURL == "" || !ok || len(roles) == 0 {
		return inboundResponse{Envelope: ocpi.Error(2001, "invalid Credentials object")}
	}
	endpoints, err := c.discoverCredentialPeer(r, versionsURL, remoteToken)
	if err != nil {
		return inboundResponse{Envelope: ocpi.Error(3001, "client callback discovery failed: "+err.Error())}
	}
	remoteIdentity := map[string]any{}
	for _, raw := range roles {
		role, _ := raw.(map[string]any)
		if strings.EqualFold(fmt.Sprint(role["role"]), "EMSP") {
			remoteIdentity = cloneMap(role)
			break
		}
	}
	localToken := ocpi.NewToken()
	c.mu.Lock()
	c.state.TokenB = localToken
	c.state.TokenC = remoteToken
	c.state.InitialToken = ""
	c.state.Endpoints = endpoints
	c.state.Credentials = cloneMap(body)
	c.state.Config.RemoteIdentity = remoteIdentity
	c.state.Status = "REGISTERED"
	config := c.state.Config
	c.mu.Unlock()
	return inboundResponse{Envelope: ocpi.Success(credentialsPayload(config, localToken))}
}

func (c *Controller) discoverCredentialPeer(r *http.Request, versionsURL, token string) ([]Endpoint, error) {
	versionsResult, err := c.doOCPI(r.Context(), "versions", http.MethodGet, versionsURL, token, false, nil)
	if err != nil || versionsResult.Envelope == nil {
		return nil, firstError(err, "invalid Versions response")
	}
	var versions []struct{ Version, URL string }
	if err := decodeData(versionsResult.Envelope, &versions); err != nil {
		return nil, err
	}
	detailsURL := ""
	for _, version := range versions {
		if version.Version == ocpi.Version {
			detailsURL = version.URL
			break
		}
	}
	if detailsURL == "" {
		return nil, fmt.Errorf("peer did not advertise OCPI 2.2.1")
	}
	detailsResult, err := c.doOCPI(r.Context(), "versions", http.MethodGet, detailsURL, token, false, nil)
	if err != nil || detailsResult.Envelope == nil {
		return nil, firstError(err, "invalid Version Details response")
	}
	var details struct {
		Version   string     `json:"version"`
		Endpoints []Endpoint `json:"endpoints"`
	}
	if err := decodeData(detailsResult.Envelope, &details); err != nil {
		return nil, err
	}
	return details.Endpoints, nil
}

func (c *Controller) senderGet(r *http.Request, _ map[string]any) inboundResponse {
	module := moduleFromPath(r.URL.Path)
	c.mu.RLock()
	records := c.state.Objects[module]
	c.mu.RUnlock()
	base := "/ocpi/cpo/2.2.1/" + module
	remaining := strings.Trim(strings.TrimPrefix(r.URL.Path, base), "/")
	if remaining != "" {
		if module != "locations" {
			return inboundResponse{HTTPStatus: http.StatusNotFound, Envelope: ocpi.Error(2003, "object not found")}
		}
		parts := strings.Split(remaining, "/")
		c.mu.RLock()
		record := records[parts[0]]
		c.mu.RUnlock()
		if record == nil {
			return inboundResponse{HTTPStatus: http.StatusNotFound, Envelope: ocpi.Error(2003, "Location not found")}
		}
		selected, err := selectLocation(record.Body, parts[1:])
		if err != nil {
			return inboundResponse{HTTPStatus: http.StatusNotFound, Envelope: ocpi.Error(2003, err.Error())}
		}
		return inboundResponse{Envelope: ocpi.Success(selected)}
	}
	if module == "sessions" && r.URL.Query().Get("date_from") == "" {
		return inboundResponse{Envelope: ocpi.Error(2001, "date_from is required by Sessions Sender GET"), Findings: []ocpi.Finding{{Level: "error", Rule: "OCPI 2.2.1 §9.2.1.1", Message: "missing date_from"}}}
	}
	dateFrom, dateTo, offset, limit, findings := parsePagination(r)
	if hasErrors(findings) {
		return inboundResponse{Envelope: ocpi.Error(2001, "invalid pagination parameters"), Findings: findings}
	}
	items := make([]map[string]any, 0, len(records))
	c.mu.RLock()
	for _, record := range records {
		updated, err := parseOCPITime(fmt.Sprint(record.Body["last_updated"]))
		if err != nil || (dateFrom != nil && updated.Before(*dateFrom)) || (dateTo != nil && !updated.Before(*dateTo)) {
			continue
		}
		items = append(items, cloneMap(record.Body))
	}
	c.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool {
		return fmt.Sprint(items[i]["last_updated"]) < fmt.Sprint(items[j]["last_updated"])
	})
	total := len(items)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	headers := http.Header{}
	headers.Set("X-Total-Count", strconv.Itoa(total))
	headers.Set("X-Limit", strconv.Itoa(limit))
	if end < total {
		next := *r.URL
		query := next.Query()
		query.Set("offset", strconv.Itoa(end))
		query.Set("limit", strconv.Itoa(limit))
		next.RawQuery = query.Encode()
		headers.Set("Link", "<"+absoluteURL(r, &next)+">; rel=\"next\"")
	}
	return inboundResponse{Envelope: ocpi.Success(items[offset:end]), Headers: headers, Findings: findings}
}

func parsePagination(r *http.Request) (*time.Time, *time.Time, int, int, []ocpi.Finding) {
	query := r.URL.Query()
	offset, limit := 0, 100
	var findings []ocpi.Finding
	if raw := query.Get("offset"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			findings = append(findings, ocpi.Finding{Level: "error", Rule: "OCPI 2.2.1 §4.1.4.1", Message: "invalid offset"})
		} else {
			offset = value
		}
	}
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			findings = append(findings, ocpi.Finding{Level: "error", Rule: "OCPI 2.2.1 §4.1.4.1", Message: "invalid limit"})
		} else if value < limit {
			limit = value
		}
	}
	var from, to *time.Time
	for name, target := range map[string]**time.Time{"date_from": &from, "date_to": &to} {
		if raw := query.Get(name); raw != "" {
			value, err := parseOCPITime(raw)
			if err != nil {
				findings = append(findings, ocpi.Finding{Level: "error", Rule: "OCPI 2.2.1 §4.1.4.1", Message: "invalid " + name})
			} else {
				*target = &value
			}
		}
	}
	return from, to, offset, limit, findings
}

func parseOCPITime(value string) (time.Time, error) {
	if !strings.HasSuffix(value, "Z") {
		value += "Z"
	}
	return time.Parse(time.RFC3339Nano, value)
}

func selectLocation(location map[string]any, parts []string) (any, error) {
	if len(parts) == 0 {
		return location, nil
	}
	evses, _ := location["evses"].([]any)
	for _, rawEVSE := range evses {
		evse, _ := rawEVSE.(map[string]any)
		if fmt.Sprint(evse["uid"]) != parts[0] {
			continue
		}
		if len(parts) == 1 {
			return evse, nil
		}
		connectors, _ := evse["connectors"].([]any)
		for _, rawConnector := range connectors {
			connector, _ := rawConnector.(map[string]any)
			if fmt.Sprint(connector["id"]) == parts[1] {
				return connector, nil
			}
		}
		return nil, fmt.Errorf("Connector not found")
	}
	return nil, fmt.Errorf("EVSE not found")
}

func readBody(r *http.Request) (map[string]any, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return map[string]any{}, nil
	}
	var value map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, ocpi.MaxBody+1)).Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func moduleFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 4 {
		return parts[3]
	}
	return ""
}

func requestURL(r *http.Request) string {
	return absoluteURL(r, r.URL)
}

func absoluteURL(r *http.Request, target *url.URL) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + target.RequestURI()
}

func envelopeAsMap(envelope ocpi.Envelope) map[string]any {
	raw, _ := json.Marshal(envelope)
	var result map[string]any
	_ = json.Unmarshal(raw, &result)
	return result
}

func hasErrors(findings []ocpi.Finding) bool {
	for _, finding := range findings {
		if finding.Level == "error" {
			return true
		}
	}
	return false
}
