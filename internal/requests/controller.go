package requests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpicore/rules"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

var fixtureIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type Controller struct {
	store  *store.Store
	client *http.Client
}

func New(repository *store.Store) (*Controller, error) {
	controller := &Controller{store: repository, client: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := controller.ensureFixtures(context.Background()); err != nil {
		return nil, err
	}
	return controller, nil
}

func (c *Controller) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/requests/state", c.apiState)
	mux.HandleFunc("PUT /api/requests/fixtures/{id}", c.apiSaveFixture)
	mux.HandleFunc("POST /api/requests/fixtures/{id}/validate", c.apiValidateFixture)
	mux.HandleFunc("POST /api/requests/fixtures/{id}/send", c.apiSendFixture)
}

func (c *Controller) apiState(w http.ResponseWriter, r *http.Request) {
	fixtures, err := c.store.ListRequestFixtures(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	exchanges, err := c.store.RecentRequestExchanges(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fixtures": fixtures, "fixture_exchanges": exchanges, "storage": "postgresql"})
}

func (c *Controller) apiSaveFixture(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Lane         string            `json:"lane"`
		Module       string            `json:"module"`
		Name         string            `json:"name"`
		Content      string            `json:"content"`
		Method       string            `json:"method"`
		TargetURL    string            `json:"target_url"`
		Headers      map[string]string `json:"headers"`
		UseAuth      bool              `json:"use_auth"`
		ExpectedHTTP *int              `json:"expected_http"`
		ExpectedOCPI *int              `json:"expected_ocpi"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	input.Lane = strings.ToUpper(strings.TrimSpace(input.Lane))
	input.Module = strings.ToLower(strings.TrimSpace(input.Module))
	input.Method = strings.ToUpper(strings.TrimSpace(input.Method))
	if input.Method == "" {
		input.Method = http.MethodPost
	}
	if !oneOf(input.Method, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete) {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("method must be GET, POST, PUT, PATCH or DELETE"))
		return
	}
	if !oneOf(input.Lane, "EMSP", "CPO", "DUAL_EMSP", "DUAL_CPO", "COMMON") || input.Module == "" || input.Name == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("lane, module and name are required"))
		return
	}
	valid, findings := validateFixture(input.Module, input.Content)
	findingsRaw, _ := json.Marshal(findings)
	for key := range input.Headers {
		if strings.EqualFold(key, "Authorization") {
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("Authorization is managed by the selected lane and cannot be stored in plaintext headers"))
			return
		}
	}
	headersRaw, _ := json.Marshal(input.Headers)
	fixture := store.RequestFixture{ID: r.PathValue("id"), Lane: input.Lane, Module: input.Module, Name: input.Name, Content: input.Content, Method: input.Method, TargetURL: strings.TrimSpace(input.TargetURL), Headers: headersRaw, UseAuth: input.UseAuth, ExpectedHTTP: input.ExpectedHTTP, ExpectedOCPI: input.ExpectedOCPI, ValidJSON: valid, Findings: findingsRaw}
	if !fixtureIDPattern.MatchString(fixture.ID) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("fixture id must use letters, numbers, dash or underscore and be at most 64 characters"))
		return
	}
	if err := c.store.UpsertRequestFixture(r.Context(), fixture); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "valid": valid, "findings": findings})
}

func (c *Controller) apiSendFixture(w http.ResponseWriter, r *http.Request) {
	var options struct {
		ExactHeaders bool `json:"exact_headers"`
	}
	if r.Body != nil {
		if err := decodeJSON(r, &options); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid send options"))
			return
		}
	}
	fixture, err := c.store.GetRequestFixture(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("fixture not found"))
		return
	}
	target, err := url.Parse(fixture.TargetURL)
	if err != nil || target.Host == "" || target.User != nil || !oneOf(strings.ToLower(target.Scheme), "http", "https") {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("a full http(s) target URL is required"))
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), fixture.Method, target.String(), bytes.NewBufferString(fixture.Content))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var customHeaders map[string]string
	if err := json.Unmarshal(fixture.Headers, &customHeaders); err != nil {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("invalid headers JSON"))
		return
	}
	for key, value := range customHeaders {
		if strings.EqualFold(key, "Authorization") {
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("Authorization cannot be loaded from fixture headers"))
			return
		}
		request.Header.Set(key, value)
	}
	applyFixtureHeaderDefaults(request, options.ExactHeaders)
	if fixture.UseAuth {
		if err := c.authorizeCredentialTarget(r.Context(), fixture.Lane, target); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		token, err := c.laneToken(r.Context(), fixture.Lane)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		request.Header.Set("Authorization", ocpi.EncodeCredential(token))
	}
	started := time.Now()
	response, err := c.client.Do(request)
	duration := time.Since(started)
	requestEvidence := map[string]any{"method": fixture.Method, "url": fixture.TargetURL, "headers": ocpi.Headers(request.Header), "body": jsonOrText(fixture.Content)}
	if err != nil {
		responseEvidence := map[string]any{"error": err.Error()}
		c.saveFixtureExchange(r.Context(), fixture.ID, requestEvidence, responseEvidence, "ERROR", duration)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ERROR", "request": requestEvidence, "response": responseEvidence, "duration_ms": duration.Milliseconds()})
		return
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, ocpi.MaxBody+1))
	if readErr != nil || len(raw) > ocpi.MaxBody {
		writeError(w, http.StatusBadGateway, fmt.Errorf("response body exceeds limit or cannot be read"))
		return
	}
	responseBody := jsonOrText(string(raw))
	ocpiStatus := statusCode(responseBody)
	status := "RECORDED"
	if fixture.ExpectedHTTP != nil || fixture.ExpectedOCPI != nil {
		status = "PASS"
		if fixture.ExpectedHTTP != nil && response.StatusCode != *fixture.ExpectedHTTP {
			status = "FAIL"
		}
		if fixture.ExpectedOCPI != nil && ocpiStatus != *fixture.ExpectedOCPI {
			status = "FAIL"
		}
	}
	responseEvidence := map[string]any{"http_status": response.StatusCode, "headers": ocpi.Headers(response.Header), "body": responseBody, "ocpi_status_code": ocpiStatus}
	if err := c.saveFixtureExchange(r.Context(), fixture.ID, requestEvidence, responseEvidence, status, duration); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "request": requestEvidence, "response": responseEvidence, "duration_ms": duration.Milliseconds()})
}

func (c *Controller) authorizeCredentialTarget(ctx context.Context, lane string, target *url.URL) error {
	endpointURLs, err := c.store.LaneEndpointURLs(ctx, lane)
	if err != nil {
		return err
	}
	targetOrigin := strings.ToLower(target.Scheme + "://" + target.Host)
	for _, endpointURL := range endpointURLs {
		endpoint, parseErr := url.Parse(endpointURL)
		if parseErr == nil && endpoint.User == nil && strings.EqualFold(endpoint.Scheme+"://"+endpoint.Host, targetOrigin) {
			return nil
		}
	}
	return fmt.Errorf("credential target must use the same scheme and host as an endpoint discovered for the %s lane", lane)
}

func applyFixtureHeaderDefaults(request *http.Request, exact bool) {
	if exact {
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodDelete && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, key := range []string{"X-Request-ID", "X-Correlation-ID"} {
		if request.Header.Get(key) == "" {
			request.Header.Set(key, ocpi.NewID())
		}
	}
}

func (c *Controller) laneToken(ctx context.Context, lane string) (string, error) {
	switch lane {
	case "EMSP", "DUAL_EMSP":
		profile := "emsp"
		if lane == "DUAL_EMSP" {
			profile = "dual"
		}
		run, err := c.store.CurrentRunProfile(ctx, profile)
		if err != nil {
			return "", err
		}
		token, err := c.store.GetSecret(ctx, run.ID, "remote_token_c")
		if err != nil || token == "" {
			return "", fmt.Errorf("eMSP mock is not registered; complete Credentials first or disable Use lane credential for a negative auth case")
		}
		return token, nil
	case "CPO", "DUAL_CPO":
		profile := "cpo"
		if lane == "DUAL_CPO" {
			profile = "dual"
		}
		snapshot, err := c.store.LoadCPOProfile(ctx, profile)
		if err != nil {
			return "", err
		}
		if snapshot.Secrets["evolt_issued_token"] == "" {
			return "", fmt.Errorf("CPO mock is not registered; complete Credentials first or disable Use lane credential for a negative auth case")
		}
		return snapshot.Secrets["evolt_issued_token"], nil
	default:
		return "", fmt.Errorf("COMMON fixtures cannot select a lane credential")
	}
}

func (c *Controller) saveFixtureExchange(ctx context.Context, fixtureID string, request, response any, status string, duration time.Duration) error {
	requestRaw, _ := json.Marshal(request)
	responseRaw, _ := json.Marshal(response)
	return c.store.AddRequestExchange(ctx, store.RequestExchange{FixtureID: fixtureID, Request: requestRaw, Response: responseRaw, Status: status, DurationMS: duration.Milliseconds()})
}

func jsonOrText(value string) any {
	var result any
	if json.Unmarshal([]byte(value), &result) == nil {
		return result
	}
	return value
}

func statusCode(value any) int {
	object, _ := value.(map[string]any)
	switch typed := object["status_code"].(type) {
	case json.Number:
		value, _ := typed.Int64()
		return int(value)
	case float64:
		return int(typed)
	}
	return 0
}

func (c *Controller) apiValidateFixture(w http.ResponseWriter, r *http.Request) {
	fixtures, err := c.store.ListRequestFixtures(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, fixture := range fixtures {
		if fixture.ID == r.PathValue("id") {
			valid, findings := validateFixture(fixture.Module, fixture.Content)
			writeJSON(w, http.StatusOK, map[string]any{"valid": valid, "findings": findings})
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("fixture not found"))
}

func (c *Controller) ensureFixtures(ctx context.Context) error {
	fixtures, err := c.store.ListRequestFixtures(ctx)
	if err != nil || len(fixtures) > 0 {
		return err
	}
	for _, fixture := range seedFixtures() {
		valid, findings := validateFixture(fixture.Module, fixture.Content)
		fixture.ValidJSON = valid
		fixture.Findings, _ = json.Marshal(findings)
		if err := c.store.UpsertRequestFixture(ctx, fixture); err != nil {
			return err
		}
	}
	return nil
}

func validateFixture(module, content string) (bool, []ocpi.Finding) {
	var body map[string]any
	if err := json.Unmarshal([]byte(content), &body); err != nil {
		return false, []ocpi.Finding{{Level: "error", Rule: "JSON", Message: err.Error()}}
	}
	supported := map[string]bool{"locations": true, "tariffs": true, "sessions": true, "cdrs": true, "tokens": true}
	if !supported[module] {
		return true, []ocpi.Finding{{Level: "info", Rule: "QA", Message: "Valid JSON; submit-time endpoint rules apply to this module"}}
	}
	owner := rules.Owner{CountryCode: fmt.Sprint(body["country_code"]), PartyID: fmt.Sprint(body["party_id"])}
	findings := rules.Validate(module, body, owner)
	for _, finding := range findings {
		if finding.Level == "error" {
			return false, findings
		}
	}
	return true, findings
}

func seedFixtures() []store.RequestFixture {
	now := time.Now().UTC().Format(time.RFC3339)
	return []store.RequestFixture{
		{ID: "emsp-token", Lane: "EMSP", Module: "tokens", Name: "Partner eMSP Token", Method: http.MethodPut, UseAuth: true, Content: fmt.Sprintf(`{"country_code":"TH","party_id":"TST","uid":"QA-RFID-001","type":"RFID","contract_id":"TH-TST-QA001","issuer":"QA Partner","valid":true,"whitelist":"ALLOWED","last_updated":%q}`, now)},
		{ID: "emsp-start", Lane: "EMSP", Module: "commands", Name: "START_SESSION", Method: http.MethodPost, UseAuth: true, Content: `{"response_url":"https://partner.example/command-result/qa-1","token":{"country_code":"TH","party_id":"TST","uid":"QA-RFID-001","type":"RFID","contract_id":"TH-TST-QA001","issuer":"QA Partner","valid":true,"whitelist":"ALLOWED","last_updated":"2026-09-08T00:00:00Z"},"location_id":"","evse_uid":""}`},
		{ID: "cpo-location", Lane: "CPO", Module: "locations", Name: "Partner CPO Location", Method: http.MethodPut, UseAuth: true, Content: fmt.Sprintf(`{"country_code":"TH","party_id":"TST","id":"QA-LOCATION-1","publish":true,"address":"QA Road","city":"Bangkok","country":"THA","coordinates":{"latitude":"13.75630","longitude":"100.50180"},"time_zone":"Asia/Bangkok","evses":[{"uid":"QA-EVSE-1","status":"AVAILABLE","connectors":[{"id":"1","standard":"IEC_62196_T2","format":"SOCKET","power_type":"AC_3_PHASE","max_voltage":400,"max_amperage":32,"last_updated":%q}],"last_updated":%q}],"last_updated":%q}`, now, now, now)},
		{ID: "cpo-tariff", Lane: "CPO", Module: "tariffs", Name: "Partner CPO Tariff", Method: http.MethodPut, UseAuth: true, Content: fmt.Sprintf(`{"country_code":"TH","party_id":"TST","id":"QA-TARIFF-1","currency":"THB","elements":[{"price_components":[{"type":"ENERGY","price":7.5,"vat":7,"step_size":1}]}],"last_updated":%q}`, now)},
		{ID: "cpo-session", Lane: "CPO", Module: "sessions", Name: "Partner CPO Session", Method: http.MethodPut, UseAuth: true, Content: fmt.Sprintf(`{"country_code":"TH","party_id":"TST","id":"QA-SESSION-1","start_date_time":%q,"kwh":0,"cdr_token":{"country_code":"TH","party_id":"EVT","uid":"QA-EVOLT-1","type":"RFID","contract_id":"TH-EVT-QA001"},"auth_method":"WHITELIST","location_id":"QA-LOCATION-1","evse_uid":"QA-EVSE-1","connector_id":"1","currency":"THB","status":"ACTIVE","last_updated":%q}`, now, now)},
		{ID: "cpo-cdr", Lane: "CPO", Module: "cdrs", Name: "Partner CPO CDR", Method: http.MethodPost, UseAuth: true, Content: fmt.Sprintf(`{"country_code":"TH","party_id":"TST","id":"QA-CDR-1","start_date_time":%q,"end_date_time":%q,"cdr_token":{"country_code":"TH","party_id":"EVT","uid":"QA-EVOLT-1","type":"RFID","contract_id":"TH-EVT-QA001"},"auth_method":"WHITELIST","cdr_location":{"id":"QA-LOCATION-1","name":"QA Location","address":"QA Road","city":"Bangkok","country":"THA","coordinates":{"latitude":"13.75630","longitude":"100.50180"},"evse_uid":"QA-EVSE-1","evse_id":"TH*TST*EQA1","connector_id":"1","connector_standard":"IEC_62196_T2","connector_format":"SOCKET","connector_power_type":"AC_3_PHASE"},"currency":"THB","charging_periods":[{"start_date_time":%q,"dimensions":[{"type":"ENERGY","volume":1.25}]}],"total_cost":{"excl_vat":8.75,"incl_vat":9.36},"total_energy":1.25,"total_time":0.25,"last_updated":%q}`, now, now, now, now)},
		{ID: "common-credentials", Lane: "COMMON", Module: "credentials", Name: "Credentials body", Method: http.MethodPost, UseAuth: false, Content: `{"token":"<generated-at-send>","url":"https://partner.example/ocpi/versions","roles":[{"role":"EMSP","party_id":"TST","country_code":"TH","business_details":{"name":"QA Partner"}}]}`},
	}
}

func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nilResponseWriter{}, r.Body, ocpi.MaxBody))
	decoder.UseNumber()
	return decoder.Decode(target)
}

type nilResponseWriter struct{}

func (nilResponseWriter) Header() http.Header       { return http.Header{} }
func (nilResponseWriter) Write([]byte) (int, error) { return 0, nil }
func (nilResponseWriter) WriteHeader(int)           {}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}
