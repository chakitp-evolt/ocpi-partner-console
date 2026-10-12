package cpo

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

func TestSeededStateIsInMemoryAndIndependent(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.RLock()
	defer controller.mu.RUnlock()
	if got := len(controller.state.Objects["locations"]); got != 3 {
		t.Fatalf("locations = %d, want 3", got)
	}
	if got := len(controller.state.Objects["tariffs"]); got != 2 {
		t.Fatalf("tariffs = %d, want 2", got)
	}
	if controller.state.TokenA != "" || controller.state.TokenB != "" || controller.state.TokenC != "" {
		t.Fatal("fresh state must not contain credentials")
	}
	for module, records := range controller.state.Objects {
		for id, record := range records {
			if len(record.Findings) != 0 {
				t.Errorf("seed %s/%s has findings: %#v", module, id, record.Findings)
			}
		}
	}
}

func TestEvoltCanInitiateCredentialsRegistration(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.InitialToken = "partner-token-a"
	controller.mu.Unlock()
	controller.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var data any
		switch request.URL.Path {
		case "/versions":
			data = []map[string]any{{"version": "2.2.1", "url": "https://evolt.example/2.2.1"}}
		case "/2.2.1":
			data = map[string]any{"version": "2.2.1", "endpoints": []map[string]any{{"identifier": "sessions", "role": "RECEIVER", "url": "https://evolt.example/sessions"}}}
		default:
			t.Fatalf("unexpected callback URL %s", request.URL)
		}
		raw, _ := json.Marshal(ocpi.Success(data))
		headers := http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{request.Header.Get("X-Request-ID")}, "X-Correlation-Id": []string{request.Header.Get("X-Correlation-ID")}}
		return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	credentials := map[string]any{"token": "evolt-token-b", "url": "https://evolt.example/versions", "roles": []map[string]any{{"role": "EMSP", "country_code": "TH", "party_id": "EVT", "business_details": map[string]any{"name": "Evolt"}}}}
	recorder := serveFunctional(t, mux, http.MethodPost, "https://partner.example/ocpi/cpo/2.2.1/credentials", credentials, "partner-token-a")
	var envelope ocpi.Envelope
	_ = json.Unmarshal(recorder.Body.Bytes(), &envelope)
	if recorder.Code != http.StatusOK || envelope.StatusCode != 1000 {
		t.Fatalf("registration response HTTP %d: %#v", recorder.Code, envelope)
	}
	controller.mu.RLock()
	defer controller.mu.RUnlock()
	if controller.state.Status != "REGISTERED" || controller.state.TokenC != "evolt-token-b" || controller.state.TokenB == "" {
		t.Fatalf("credentials state = %#v", controller.state)
	}
	if controller.state.InitialToken != "" {
		t.Fatal("Token A must be invalidated after successful registration")
	}
	if endpointURL(controller.state.Endpoints, "sessions", "RECEIVER") == "" {
		t.Fatal("callback Version Details endpoints were not stored")
	}
}

func TestEvoltInitiatedRotationReplacesBothCredentialDirections(t *testing.T) {
	controller := New("https://partner.example")
	controller.state.Status = "REGISTERED"
	controller.state.TokenB = "old-partner-issued-token"
	controller.state.TokenC = "old-evolt-issued-token"
	controller.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var data any
		switch request.URL.Path {
		case "/versions":
			data = []map[string]any{{"version": "2.2.1", "url": "https://evolt.example/2.2.1"}}
		case "/2.2.1":
			data = map[string]any{"version": "2.2.1", "endpoints": []map[string]any{{"identifier": "sessions", "role": "RECEIVER", "url": "https://evolt.example/sessions"}}}
		default:
			t.Fatalf("unexpected callback URL %s", request.URL)
		}
		raw, _ := json.Marshal(ocpi.Success(data))
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	payload := map[string]any{"token": "new-evolt-issued-token", "url": "https://evolt.example/versions", "roles": []map[string]any{{"role": "EMSP", "country_code": "TH", "party_id": "EVT", "business_details": map[string]any{"name": "Evolt"}}}}
	recorder := serveFunctional(t, mux, http.MethodPut, "https://partner.example/ocpi/cpo/2.2.1/credentials", payload, "old-partner-issued-token")
	var envelope ocpi.Envelope
	_ = json.Unmarshal(recorder.Body.Bytes(), &envelope)
	if recorder.Code != http.StatusOK || envelope.StatusCode != 1000 {
		t.Fatalf("rotation response HTTP %d: %#v", recorder.Code, envelope)
	}
	controller.mu.RLock()
	if controller.state.Status != "REGISTERED" || controller.state.TokenC != "new-evolt-issued-token" {
		t.Fatalf("rotation did not store the new Evolt-issued token: %#v", controller.state)
	}
	if controller.state.TokenB == "" || controller.state.TokenB == "old-partner-issued-token" {
		t.Fatalf("rotation did not mint a new Partner-issued token: %q", controller.state.TokenB)
	}
	response, _ := envelope.Data.(map[string]any)
	if response["token"] != controller.state.TokenB {
		t.Fatalf("Credentials response token = %v, want %q", response["token"], controller.state.TokenB)
	}
	nextToken := controller.state.TokenB
	controller.mu.RUnlock()
	payload["token"] = "second-evolt-issued-token"
	recorder = serveFunctional(t, mux, http.MethodPut, "https://partner.example/ocpi/cpo/2.2.1/credentials", payload, nextToken)
	if recorder.Code != http.StatusOK {
		t.Fatalf("second rotation response HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	controller.mu.RLock()
	if controller.state.Status != "REGISTERED" || controller.state.TokenC != "second-evolt-issued-token" {
		t.Fatalf("second rotation did not keep the connection registered: %#v", controller.state)
	}
	controller.mu.RUnlock()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestVersionDetailsAdvertisesCPORolesFromSpec(t *testing.T) {
	controller := New("https://partner.example")
	response := controller.versionDetails(nil, nil)
	raw, _ := json.Marshal(response.Envelope.Data)
	var details struct {
		Endpoints []Endpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"locations": "SENDER", "tariffs": "SENDER", "sessions": "SENDER", "cdrs": "SENDER", "tokens": "RECEIVER", "commands": "RECEIVER"}
	for module, role := range want {
		if endpointURL(details.Endpoints, module, role) == "" {
			t.Errorf("missing %s %s endpoint", module, role)
		}
	}
}

func TestTokenReceiverStoresAndReturnsEMSPToken(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.TokenB = "partner-issued-token"
	controller.mu.Unlock()
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	token := map[string]any{"country_code": "TH", "party_id": "EVT", "uid": "RFID-001", "type": "RFID", "contract_id": "TH-EVT-C1", "issuer": "Evolt", "valid": true, "whitelist": "ALWAYS", "last_updated": "2026-09-07T12:00:00Z"}
	recorder := serveFunctional(t, mux, http.MethodPut, "https://partner.example/ocpi/cpo/2.2.1/tokens/TH/EVT/RFID-001", token, "partner-issued-token")
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = serveFunctional(t, mux, http.MethodGet, "https://partner.example/ocpi/cpo/2.2.1/tokens/TH/EVT/RFID-001", nil, "partner-issued-token")
	var envelope ocpi.Envelope
	_ = json.Unmarshal(recorder.Body.Bytes(), &envelope)
	data, _ := envelope.Data.(map[string]any)
	if envelope.StatusCode != 1000 || data["uid"] != "RFID-001" {
		t.Fatalf("GET response = %#v", envelope)
	}
}

func TestPartnerCanPullTokensFromEMSPWithPagination(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.TokenC = "evolt-issued-token"
	controller.state.Config.RemoteIdentity = map[string]any{"country_code": "TH", "party_id": "EVT"}
	controller.state.Endpoints = []Endpoint{{Identifier: "tokens", Role: "SENDER", URL: "https://evolt.example/tokens"}}
	controller.state.Objects["tokens"]["TH/EVT/STALE/RFID"] = &Record{Body: map[string]any{"country_code": "TH", "party_id": "EVT", "uid": "STALE", "type": "RFID", "valid": true}, Delivery: "RECEIVED", UpdatedAt: now()}
	controller.mu.Unlock()
	controller.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Query().Get("offset") != "0" || request.URL.Query().Get("limit") != "10" {
			t.Fatalf("unexpected pagination query: %s", request.URL.RawQuery)
		}
		if request.Header.Get("Authorization") == "" || request.Header.Get("OCPI-from-party-id") != "TST" || request.Header.Get("OCPI-to-party-id") != "EVT" {
			t.Fatalf("missing functional request headers: %#v", request.Header)
		}
		items := []map[string]any{
			{"country_code": "TH", "party_id": "EVT", "uid": "RFID-001", "type": "RFID", "contract_id": "TH-EVT-C1", "issuer": "Evolt", "valid": true, "whitelist": "ALWAYS", "last_updated": "2026-09-08T00:00:00Z"},
			{"country_code": "TH", "party_id": "EVT", "uid": "RFID-002", "type": "RFID", "contract_id": "TH-EVT-C2", "issuer": "Evolt", "valid": true, "whitelist": "ALLOWED", "last_updated": "2026-09-08T00:00:00Z"},
		}
		raw, _ := json.Marshal(ocpi.Success(items))
		headers := http.Header{
			"Content-Type":           []string{"application/json"},
			"X-Request-Id":           []string{request.Header.Get("X-Request-ID")},
			"X-Correlation-Id":       []string{request.Header.Get("X-Correlation-ID")},
			"OCPI-from-country-code": []string{"TH"},
			"OCPI-from-party-id":     []string{"EVT"},
			"OCPI-to-country-code":   []string{"TH"},
			"OCPI-to-party-id":       []string{"TST"},
			"X-Total-Count":          []string{"2"},
			"X-Limit":                []string{"10"},
		}
		return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "https://partner.example/api/cpo/actions/pull-tokens", strings.NewReader("{}"))
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	controller.mu.RLock()
	defer controller.mu.RUnlock()
	if got := len(controller.state.Objects["tokens"]); got != 3 {
		t.Fatalf("tokens = %d, want 3", got)
	}
	if controller.state.Objects["tokens"]["TH/EVT/RFID-001/RFID"].Delivery != "PULLED" {
		t.Fatal("pulled Token was not stored")
	}
	if valid, _ := controller.state.Objects["tokens"]["TH/EVT/STALE/RFID"].Body["valid"].(bool); valid {
		t.Fatal("Token omitted from a full pull must be invalidated")
	}
}

func TestCommandsReceiverReturnsDirectResponseAndStoresCommand(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.TokenB = "partner-issued-token"
	controller.mu.Unlock()
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	body := map[string]any{"response_url": "https://emsp.example/commands/result/1", "session_id": "SESSION-1"}
	recorder := serveFunctional(t, mux, http.MethodPost, "https://partner.example/ocpi/cpo/2.2.1/commands/STOP_SESSION", body, "partner-issued-token")
	var envelope ocpi.Envelope
	_ = json.Unmarshal(recorder.Body.Bytes(), &envelope)
	data, _ := envelope.Data.(map[string]any)
	if envelope.StatusCode != 1000 || data["result"] != "ACCEPTED" {
		t.Fatalf("CommandResponse = %#v", envelope)
	}
	controller.mu.RLock()
	defer controller.mu.RUnlock()
	if len(controller.state.Commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(controller.state.Commands))
	}
}

func serveFunctional(t *testing.T, handler http.Handler, method, target string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&payload).Encode(body)
	}
	request := httptest.NewRequest(method, target, &payload)
	request.Header.Set("Authorization", ocpi.EncodeCredential(token))
	request.Header.Set("X-Request-ID", "request-1")
	request.Header.Set("X-Correlation-ID", "correlation-1")
	request.Header.Set("OCPI-from-country-code", "TH")
	request.Header.Set("OCPI-from-party-id", "EVT")
	request.Header.Set("OCPI-to-country-code", "TH")
	request.Header.Set("OCPI-to-party-id", "TST")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestSenderLocationsPullPagination(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.TokenB = "partner-token-b"
	controller.mu.Unlock()
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	request := httptest.NewRequest(http.MethodGet, "https://partner.example/ocpi/cpo/2.2.1/locations?limit=2", nil)
	request.Header.Set("Authorization", ocpi.EncodeCredential("partner-token-b"))
	request.Header.Set("X-Request-ID", "request-1")
	request.Header.Set("X-Correlation-ID", "correlation-1")
	request.Header.Set("OCPI-from-country-code", "TH")
	request.Header.Set("OCPI-from-party-id", "EVT")
	request.Header.Set("OCPI-to-country-code", "TH")
	request.Header.Set("OCPI-to-party-id", "TST")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Total-Count"); got != "3" {
		t.Fatalf("X-Total-Count = %q", got)
	}
	if got := recorder.Header().Get("X-Limit"); got != "2" {
		t.Fatalf("X-Limit = %q", got)
	}
	if recorder.Header().Get("Link") == "" {
		t.Fatal("first page is missing Link header")
	}
	var envelope ocpi.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	items, ok := envelope.Data.([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("data = %#v, want two Locations", envelope.Data)
	}
}

func TestSessionsPullRequiresDateFrom(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.TokenB = "partner-token-b"
	controller.mu.Unlock()
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	request := httptest.NewRequest(http.MethodGet, "https://partner.example/ocpi/cpo/2.2.1/sessions", nil)
	request.Header.Set("Authorization", ocpi.EncodeCredential("partner-token-b"))
	request.Header.Set("X-Request-ID", "request-1")
	request.Header.Set("X-Correlation-ID", "correlation-1")
	request.Header.Set("OCPI-from-country-code", "TH")
	request.Header.Set("OCPI-from-party-id", "EVT")
	request.Header.Set("OCPI-to-country-code", "TH")
	request.Header.Set("OCPI-to-party-id", "TST")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	var envelope ocpi.Envelope
	_ = json.Unmarshal(recorder.Body.Bytes(), &envelope)
	if envelope.StatusCode != 2001 {
		t.Fatalf("status_code = %d, want 2001", envelope.StatusCode)
	}
}

func TestTokensEndpointServesTheOutboundTokenReadyToPaste(t *testing.T) {
	controller := New("https://partner.example")
	controller.state.Status = "REGISTERED"
	controller.state.TokenB = "evolt-calls-partner"
	controller.state.TokenC = "partner-calls-evolt"

	recorder := httptest.NewRecorder()
	controller.apiTokens(recorder, httptest.NewRequest(http.MethodGet, "/api/cpo/tokens", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	body := map[string]map[string]any{}
	for _, key := range []string{"partner_to_evolt", "evolt_to_partner", "registration"} {
		entry, ok := payload[key].(map[string]any)
		if !ok {
			t.Fatalf("%s is missing from the response", key)
		}
		body[key] = entry
	}
	// Token C in mock terms is the one the partner presents to Evolt — the token QA pastes.
	outbound := body["partner_to_evolt"]
	if outbound["token"] != "partner-calls-evolt" {
		t.Fatalf("partner_to_evolt token = %v, want the token Evolt issued", outbound["token"])
	}
	if outbound["authorization"] != "Token "+base64.StdEncoding.EncodeToString([]byte("partner-calls-evolt")) {
		t.Fatalf("authorization = %v, want the base64 header form", outbound["authorization"])
	}
	if body["evolt_to_partner"]["token"] != "evolt-calls-partner" {
		t.Fatal("evolt_to_partner must carry the token the mock issued")
	}
	if present, _ := body["registration"]["present"].(bool); present {
		t.Fatal("Token A is gone after registration; it must not be reported as present")
	}
}

func createSessionDraft(t *testing.T, controller *Controller, payload map[string]any) map[string]any {
	t.Helper()
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	var body bytes.Buffer
	_ = json.NewEncoder(&body).Encode(payload)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "https://partner.example/api/cpo/actions/session/create", &body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("create session = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Object map[string]any `json:"object"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response.Object
}

func TestSessionDraftFromStartSessionCommandUsesTheCommandToken(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.Commands["command-1"] = &CommandRecord{
		ID: "command-1", Type: "START_SESSION", Status: "ACCEPTED",
		Request: map[string]any{
			"response_url": "https://evolt.example/ocpi/emsp/2.2.1/commands/START_SESSION/1",
			"token": map[string]any{
				"country_code": "TH", "party_id": "EVT", "uid": "RFID-EVOLT-1", "type": "RFID",
				"contract_id": "TH-EVT-C00000009", "issuer": "Evolt", "valid": true, "whitelist": "ALWAYS",
			},
			"location_id": "BKK-ARI", "evse_uid": "ARI-EVSE-1", "connector_id": "1",
			"authorization_reference": "AUTH-REF-9",
		},
	}
	controller.mu.Unlock()

	session := createSessionDraft(t, controller, map[string]any{"command_id": "command-1"})

	if got := session["auth_method"]; got != "COMMAND" {
		t.Errorf("auth_method = %v, want COMMAND for a session a remote Command started", got)
	}
	if got := session["authorization_reference"]; got != "AUTH-REF-9" {
		t.Errorf("authorization_reference = %v, want the value the Command carried", got)
	}
	for field, want := range map[string]string{"location_id": "BKK-ARI", "evse_uid": "ARI-EVSE-1", "connector_id": "1"} {
		if got := session[field]; got != want {
			t.Errorf("%s = %v, want %s", field, got, want)
		}
	}
	cdrToken, ok := session["cdr_token"].(map[string]any)
	if !ok {
		t.Fatalf("cdr_token = %#v, want an object", session["cdr_token"])
	}
	for field, want := range map[string]string{"country_code": "TH", "party_id": "EVT", "uid": "RFID-EVOLT-1", "type": "RFID", "contract_id": "TH-EVT-C00000009"} {
		if got := cdrToken[field]; got != want {
			t.Errorf("cdr_token.%s = %v, want %s", field, got, want)
		}
	}
	for _, field := range []string{"issuer", "valid", "whitelist"} {
		if _, found := cdrToken[field]; found {
			t.Errorf("cdr_token must not carry the Token field %q", field)
		}
	}
}

func TestSessionDraftWithoutCommandKeepsTheManualPicker(t *testing.T) {
	controller := New("https://partner.example")
	session := createSessionDraft(t, controller, map[string]any{"location_id": "BKK-ARI", "evse_uid": "ARI-EVSE-1", "connector_id": "1"})
	if got := session["auth_method"]; got != "WHITELIST" {
		t.Errorf("auth_method = %v, want WHITELIST when no Command was chosen", got)
	}
	if got := session["location_id"]; got != "BKK-ARI" {
		t.Errorf("location_id = %v, want BKK-ARI", got)
	}
	if _, found := session["authorization_reference"]; found {
		t.Error("authorization_reference must be absent when no Command was chosen")
	}
}

func TestSessionDraftRejectsACommandThatIsNotStartSession(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.Commands["command-2"] = &CommandRecord{ID: "command-2", Type: "STOP_SESSION", Request: map[string]any{"session_id": "SESSION-1"}}
	controller.mu.Unlock()
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	var body bytes.Buffer
	_ = json.NewEncoder(&body).Encode(map[string]any{"command_id": "command-2"})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "https://partner.example/api/cpo/actions/session/create", &body))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a command that is not START_SESSION", recorder.Code)
	}
}

// OCPI 2.2.1 §7.1.2: PUT and DELETE act on a connection that already exists, and a peer that has
// only reached Token A has not opened one yet. The spec asks for 405 rather than 401 here so the
// caller learns the method is wrong for its state, not that its token is bad — and without it PUT
// becomes a second way to register, which defeats the once-only rule POST is given.
func TestCredentialsRefusesPutAndDeleteBeforeRegistration(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			controller := New("https://partner.example")
			controller.state.Status = "PENDING"
			controller.state.InitialToken = "issued-token-a"
			mux := http.NewServeMux()
			controller.RegisterRoutes(mux)

			payload := map[string]any{"token": "evolt-token", "url": "https://evolt.example/versions", "roles": []map[string]any{{"role": "EMSP", "country_code": "TH", "party_id": "EVT"}}}
			recorder := serveFunctional(t, mux, method, "https://partner.example/ocpi/cpo/2.2.1/credentials", payload, "issued-token-a")

			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s before registration answered HTTP %d, want 405: %s", method, recorder.Code, recorder.Body)
			}
			controller.mu.RLock()
			defer controller.mu.RUnlock()
			if controller.state.Status != "PENDING" || controller.state.TokenB != "" || controller.state.TokenC != "" {
				t.Fatalf("%s registered the peer through the back door: %#v", method, controller.state)
			}
		})
	}
}

func TestPartnerInitiatedRotationAcceptsTheNewTokenDuringEvoltCallback(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.Status = "REGISTERED"
	controller.state.TokenB = "old-partner-token"
	controller.state.TokenC = "old-evolt-token"
	controller.state.Endpoints = []Endpoint{{Identifier: "credentials", Role: "SENDER", URL: "https://evolt.example/credentials"}}
	controller.mu.Unlock()

	var offeredToken string
	controller.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPut || request.URL.String() != "https://evolt.example/credentials" {
			t.Fatalf("request = %s %s, want PUT credentials", request.Method, request.URL)
		}
		var payload struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		offeredToken = payload.Token
		if err := controller.authorize(ocpi.EncodeCredential(offeredToken)); err != nil {
			t.Fatalf("Evolt callback could not use the token offered in PUT: %v", err)
		}
		raw, _ := json.Marshal(ocpi.Success(map[string]any{
			"token": "new-evolt-token",
			"url":   "https://evolt.example/versions",
			"roles": []map[string]any{{"role": "CPO", "country_code": "TH", "party_id": "EVT", "business_details": map[string]any{"name": "Evolt"}}},
		}))
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})}

	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "https://partner.example/api/cpo/actions/credentials/rotate", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("rotate response HTTP %d: %s", recorder.Code, recorder.Body.String())
	}

	controller.mu.RLock()
	defer controller.mu.RUnlock()
	if offeredToken == "" || controller.state.TokenB != offeredToken || controller.state.TokenC != "new-evolt-token" || controller.state.StagedTokenB != "" || controller.state.Status != "REGISTERED" {
		t.Fatalf("credential state after rotate = %#v, offered token = %q", controller.state, offeredToken)
	}
}

func TestSaveObjectRefreshesLastUpdatedOnlyWhenContentChanged(t *testing.T) {
	c := New("https://partner.example")
	save := func(body map[string]any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPut, "/api/cpo/objects/tariffs/T1", bytes.NewReader(raw))
		r.SetPathValue("module", "tariffs")
		r.SetPathValue("id", "T1")
		w := httptest.NewRecorder()
		c.apiSaveObject(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("save status=%d body=%s", w.Code, w.Body.String())
		}
		return c.state.Objects["tariffs"]["T1"].Body
	}
	stamp := "2026-10-01T06:36:13Z"
	tariff := map[string]any{"id": "T1", "currency": "THB", "elements": []any{}, "last_updated": stamp}
	if got := fieldText(save(tariff), "last_updated"); got != stamp {
		t.Fatalf("first save changed last_updated to %s", got)
	}
	if got := fieldText(save(tariff), "last_updated"); got != stamp {
		t.Fatalf("unchanged save changed last_updated to %s", got)
	}
	edited := cloneMap(tariff)
	edited["currency"] = "EUR"
	if got := fieldText(save(edited), "last_updated"); got == stamp {
		t.Fatal("edit that kept last_updated was not refreshed")
	}
	chosen := cloneMap(edited)
	chosen["currency"] = "THB"
	chosen["last_updated"] = "2026-10-02T00:00:00Z"
	if got := fieldText(save(chosen), "last_updated"); got != "2026-10-02T00:00:00Z" {
		t.Fatalf("explicit last_updated was overwritten with %s", got)
	}
}
