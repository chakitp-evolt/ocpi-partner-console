//go:build integration

package emsp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

func integrationDatabase(t *testing.T) *sql.DB {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required for integration tests")
	}
	if !strings.Contains(databaseURL, "/partner_console_test") {
		t.Fatalf("refusing to reset a database other than partner_console_test: %q", databaseURL)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(context.Background(), `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset isolated integration database: %v", err)
	}
	schema, err := os.ReadFile("../../scripts/db/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), string(schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db
}

func TestBlackBoxJourney(t *testing.T) {
	db := integrationDatabase(t)
	repository := store.New(db, "integration-test-encryption-key")

	application, err := New(repository, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{DefaultPublicURL: "http://pending"})
	if err != nil {
		t.Fatal(err)
	}
	partner := httptest.NewServer(application.Handler())
	defer partner.Close()

	fixture := newCPOFixture(t)
	defer fixture.Close()

	callAPI(t, http.MethodPut, partner.URL+"/api/emsp/config", map[string]any{
		"name": "integration journey", "evolt_versions_url": fixture.URL + "/ocpi/versions", "token_a": fixture.tokenA,
		"partner_public_url": partner.URL, "country_code": "TH", "party_id": "TST", "business_name": "Spec eMSP", "strict": true,
	}, http.StatusOK)
	callAPI(t, http.MethodPost, partner.URL+"/api/emsp/actions/discover", map[string]any{}, http.StatusOK)
	callAPI(t, http.MethodPost, partner.URL+"/api/emsp/actions/register", map[string]any{}, http.StatusCreated)

	callAPI(t, http.MethodPost, partner.URL+"/api/emsp/tokens", map[string]any{
		"uid": "RFID-001", "type": "RFID", "contract_id": "TH-TST-C0001", "issuer": "Spec eMSP", "valid": true, "whitelist": "ALLOWED",
	}, http.StatusCreated)
	callAPI(t, http.MethodPost, partner.URL+"/api/emsp/actions/push-token", map[string]any{"uid": "RFID-001", "type": "RFID"}, http.StatusOK)
	callAPI(t, http.MethodPost, partner.URL+"/api/emsp/actions/pull", map[string]any{"module": "locations"}, http.StatusOK)

	callAPI(t, http.MethodPost, partner.URL+"/api/emsp/actions/command", map[string]any{"type": "START_SESSION", "payload": map[string]any{
		"token_uid": "RFID-001", "token_type": "RFID", "location_id": "LOC1", "evse_uid": "EVSE1", "connector_id": "1", "authorization_reference": "AUTH-001",
	}}, http.StatusOK)

	fixture.push(t, partner.URL+"/ocpi/emsp/2.2.1/sessions/TH/EVT/SESSION1", http.MethodPut, map[string]any{
		"country_code": "TH", "party_id": "EVT", "id": "SESSION1", "start_date_time": now(), "kwh": 0.0,
		"cdr_token":   map[string]any{"country_code": "TH", "party_id": "TST", "uid": "RFID-001", "type": "RFID", "contract_id": "TH-TST-C0001"},
		"auth_method": "COMMAND", "authorization_reference": "AUTH-001", "location_id": "LOC1", "evse_uid": "EVSE1", "connector_id": "1",
		"currency": "THB", "status": "ACTIVE", "last_updated": now(),
	}, http.StatusCreated)

	callAPI(t, http.MethodPost, partner.URL+"/api/emsp/actions/command", map[string]any{"type": "STOP_SESSION", "payload": map[string]any{"session_id": "SESSION1"}}, http.StatusOK)

	fixture.push(t, partner.URL+"/ocpi/emsp/2.2.1/sessions/TH/EVT/SESSION1", http.MethodPatch, map[string]any{
		"end_date_time": now(), "kwh": 5.25, "status": "COMPLETED", "last_updated": now(),
	}, http.StatusOK)
	fixture.push(t, partner.URL+"/ocpi/emsp/2.2.1/cdrs", http.MethodPost, fixtureCDR(), http.StatusCreated)

	time.Sleep(150 * time.Millisecond)
	state := callAPI(t, http.MethodGet, partner.URL+"/api/emsp/state", nil, http.StatusOK)
	assertArrayLength(t, state, "endpoints", 7)
	assertArrayLength(t, state, "tokens", 1)
	assertArrayLength(t, state, "locations", 1)
	assertArrayLength(t, state, "sessions", 1)
	assertArrayLength(t, state, "cdrs", 1)
	commands := array(state["commands"])
	if len(commands) != 2 {
		t.Fatalf("commands length = %d, want 2", len(commands))
	}
	for _, item := range commands {
		command := item.(map[string]any)
		if command["async_result"] == nil {
			t.Fatalf("command %v has no async result", command["id"])
		}
	}
	exchanges := array(state["exchanges"])
	if len(exchanges) < 12 {
		t.Fatalf("exchanges length = %d, want at least 12", len(exchanges))
	}
	for _, item := range exchanges {
		exchange := item.(map[string]any)
		if findings, ok := exchange["findings"].([]any); ok {
			for _, raw := range findings {
				finding := raw.(map[string]any)
				if finding["level"] == "error" {
					t.Fatalf("unexpected conformance error: %#v", finding)
				}
			}
		}
	}
	if fixture.pushedTokenUID != "RFID-001" {
		t.Fatalf("fake CPO did not receive Token, got %q", fixture.pushedTokenUID)
	}
	assertArrayLength(t, state, "setup", 1)
	if len(array(state["secrets"])) < 3 {
		t.Fatalf("secret metadata length = %d, want at least 3", len(array(state["secrets"])))
	}
	for _, raw := range array(state["secrets"]) {
		secret := raw.(map[string]any)
		if _, exposed := secret["ciphertext"]; exposed {
			t.Fatalf("secret metadata exposed ciphertext: %#v", secret)
		}
		if secret["encrypted"] != true {
			t.Fatalf("secret metadata is not marked encrypted: %#v", secret)
		}
	}

	callAPI(t, http.MethodPost, partner.URL+"/api/emsp/reset", map[string]any{"confirmation": "wrong"}, http.StatusBadRequest)
	oldRunID := state["run"].(map[string]any)["id"]
	callAPI(t, http.MethodPost, partner.URL+"/api/emsp/reset", map[string]any{"confirmation": "RESET"}, http.StatusOK)
	clean := callAPI(t, http.MethodGet, partner.URL+"/api/emsp/state", nil, http.StatusOK)
	newRun := clean["run"].(map[string]any)
	if newRun["id"] == oldRunID || newRun["status"] != "SETUP" {
		t.Fatalf("reset run = %#v, old id %v", newRun, oldRunID)
	}
	for _, key := range []string{"secrets", "endpoints", "tokens", "locations", "tariffs", "sessions", "cdrs", "commands", "exchanges"} {
		assertArrayLength(t, clean, key, 0)
	}
	assertArrayLength(t, clean, "setup", 1)

	callAPI(t, http.MethodPut, partner.URL+"/api/emsp/config", map[string]any{
		"name": "Evolt-initiated registration", "evolt_versions_url": fixture.URL + "/ocpi/versions",
		"partner_public_url": partner.URL, "country_code": "TH", "party_id": "TST", "business_name": "Spec eMSP", "strict": true,
	}, http.StatusOK)
	initial := callAPI(t, http.MethodPost, partner.URL+"/api/emsp/initial-token", map[string]any{}, http.StatusCreated)
	initialToken := fmt.Sprint(initial["token"])
	if initialToken == "" || initialToken == "<nil>" {
		t.Fatalf("initial token was not returned once: %#v", initial)
	}
	credentialsBody, _ := json.Marshal(map[string]any{
		"token": fixture.tokenA, "url": fixture.URL + "/ocpi/versions",
		"roles": []map[string]any{{"role": "CPO", "party_id": "EVT", "country_code": "TH", "business_details": map[string]any{"name": "Spec CPO"}}},
	})
	credentialsRequest, _ := http.NewRequest(http.MethodPost, partner.URL+"/ocpi/emsp/2.2.1/credentials", bytes.NewReader(credentialsBody))
	credentialsRequest.Header.Set("Authorization", ocpi.EncodeCredential(initialToken))
	credentialsRequest.Header.Set("Content-Type", "application/json")
	credentialsRequest.Header.Set("X-Request-ID", ocpi.NewID())
	credentialsRequest.Header.Set("X-Correlation-ID", ocpi.NewID())
	credentialsResponse, err := http.DefaultClient.Do(credentialsRequest)
	if err != nil {
		t.Fatalf("Evolt-initiated credentials: %v", err)
	}
	defer credentialsResponse.Body.Close()
	if credentialsResponse.StatusCode != http.StatusCreated {
		responseBody, _ := io.ReadAll(credentialsResponse.Body)
		t.Fatalf("Evolt-initiated credentials HTTP %d: %s", credentialsResponse.StatusCode, responseBody)
	}
	var registeredCredentials ocpi.Envelope
	if err := json.NewDecoder(credentialsResponse.Body).Decode(&registeredCredentials); err != nil {
		t.Fatal(err)
	}
	mockToken := fmt.Sprint(registeredCredentials.Data.(map[string]any)["token"])
	inboundState := callAPI(t, http.MethodGet, partner.URL+"/api/emsp/state", nil, http.StatusOK)
	if inboundState["run"].(map[string]any)["status"] != "REGISTERED" {
		t.Fatalf("Evolt-initiated registration did not register: %#v", inboundState["run"])
	}
	assertArrayLength(t, inboundState, "endpoints", 7)
	for _, raw := range array(inboundState["secrets"]) {
		if raw.(map[string]any)["key"] == "mock_initial_token" {
			t.Fatalf("initial Token A was not invalidated: %#v", raw)
		}
	}
	for i := 0; i < 2; i++ {
		fixture.tokenA = fmt.Sprintf("rotated-evolt-token-%d", i)
		rotateBody, _ := json.Marshal(map[string]any{
			"token": fixture.tokenA, "url": fixture.URL + "/ocpi/versions",
			"roles": []map[string]any{{"role": "CPO", "party_id": "EVT", "country_code": "TH", "business_details": map[string]any{"name": "Spec CPO"}}},
		})
		rotateRequest, _ := http.NewRequest(http.MethodPut, partner.URL+"/ocpi/emsp/2.2.1/credentials", bytes.NewReader(rotateBody))
		rotateRequest.Header.Set("Authorization", ocpi.EncodeCredential(mockToken))
		rotateRequest.Header.Set("Content-Type", "application/json")
		rotateRequest.Header.Set("X-Request-ID", ocpi.NewID())
		rotateRequest.Header.Set("X-Correlation-ID", ocpi.NewID())
		rotateResponse, err := http.DefaultClient.Do(rotateRequest)
		if err != nil {
			t.Fatal(err)
		}
		var rotatedCredentials ocpi.Envelope
		decodeErr := json.NewDecoder(rotateResponse.Body).Decode(&rotatedCredentials)
		rotateResponse.Body.Close()
		if decodeErr != nil || rotateResponse.StatusCode != http.StatusOK || rotatedCredentials.StatusCode != 1000 {
			t.Fatalf("rotation %d: HTTP %d, response %#v, decode error %v", i+1, rotateResponse.StatusCode, rotatedCredentials, decodeErr)
		}
		mockToken = fmt.Sprint(rotatedCredentials.Data.(map[string]any)["token"])
		state := callAPI(t, http.MethodGet, partner.URL+"/api/emsp/state", nil, http.StatusOK)
		if got := state["run"].(map[string]any)["status"]; got != "REGISTERED" {
			t.Fatalf("rotation %d left mock status %v, want REGISTERED", i+1, got)
		}
	}

	// Evolt started this handshake, and the partner rotates it anyway: §7.1.2 makes PUT symmetric.
	// Both tokens have to move, because one PUT carries the new Token B and answers with a new Token C
	// — a rotation that changed only one direction would leave half the connection on a dead key.
	before := secretFingerprints(t, partner.URL)
	callAPI(t, http.MethodPost, partner.URL+"/api/emsp/actions/rotate-credentials", map[string]any{}, http.StatusOK)
	after := secretFingerprints(t, partner.URL)
	for _, key := range []string{"inbound_token_b", "remote_token_c"} {
		if before[key] == "" || after[key] == "" {
			t.Fatalf("%s is missing around the rotation: before=%q after=%q", key, before[key], after[key])
		}
		if before[key] == after[key] {
			t.Fatalf("%s did not change across a partner-initiated rotation", key)
		}
	}
}

// secretFingerprints reads the revealed secrets the way the console does, and keeps only what is
// safe to compare: the fingerprint, never the token itself.
func secretFingerprints(t *testing.T, baseURL string) map[string]string {
	t.Helper()
	state := callAPI(t, http.MethodGet, baseURL+"/api/emsp/state?reveal_secrets=1", nil, http.StatusOK)
	fingerprints := map[string]string{}
	for _, raw := range array(state["secrets"]) {
		row := raw.(map[string]any)
		fingerprints[fmt.Sprint(row["key"])] = fmt.Sprint(row["fingerprint"])
	}
	return fingerprints
}

type cpoFixture struct {
	*httptest.Server
	t              *testing.T
	tokenA         string
	tokenB         string
	tokenC         string
	mu             sync.Mutex
	pushedTokenUID string
}

func newCPOFixture(t *testing.T) *cpoFixture {
	fixture := &cpoFixture{t: t, tokenA: "fixture-token-a", tokenC: "fixture-token-c"}
	fixture.Server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	return fixture
}

func (f *cpoFixture) handle(w http.ResponseWriter, r *http.Request) {
	functional := strings.Contains(r.URL.Path, "/locations") || strings.Contains(r.URL.Path, "/tokens") || strings.Contains(r.URL.Path, "/commands") || strings.Contains(r.URL.Path, "/sessions") || strings.Contains(r.URL.Path, "/cdrs") || strings.Contains(r.URL.Path, "/tariffs")
	expectedToken := f.tokenA
	if functional {
		expectedToken = f.tokenC
	}
	provided, err := ocpi.DecodeCredential(r.Header.Get("Authorization"), false)
	if err != nil || provided != expectedToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch {
	case r.URL.Path == "/ocpi/versions" && r.Method == http.MethodGet:
		f.respond(w, r, false, http.StatusOK, ocpi.Success([]map[string]any{{"version": "2.2.1", "url": f.URL + "/ocpi/2.2.1"}}))
	case r.URL.Path == "/ocpi/2.2.1" && r.Method == http.MethodGet:
		base := f.URL + "/ocpi/cpo/2.2.1"
		f.respond(w, r, false, http.StatusOK, ocpi.Success(map[string]any{"version": "2.2.1", "endpoints": []map[string]any{
			{"identifier": "credentials", "role": "SENDER", "url": base + "/credentials"},
			{"identifier": "tokens", "role": "RECEIVER", "url": base + "/tokens"},
			{"identifier": "commands", "role": "RECEIVER", "url": base + "/commands"},
			{"identifier": "locations", "role": "SENDER", "url": base + "/locations"},
			{"identifier": "tariffs", "role": "SENDER", "url": base + "/tariffs"},
			{"identifier": "sessions", "role": "SENDER", "url": base + "/sessions"},
			{"identifier": "cdrs", "role": "SENDER", "url": base + "/cdrs"},
		}}))
	case r.URL.Path == "/ocpi/cpo/2.2.1/credentials" && r.Method == http.MethodPost:
		var credentials map[string]any
		_ = json.NewDecoder(r.Body).Decode(&credentials)
		f.tokenB = fmt.Sprint(credentials["token"])
		partnerVersions := fmt.Sprint(credentials["url"])
		f.callbackGET(partnerVersions)
		f.callbackGET(strings.TrimSuffix(partnerVersions, "/versions") + "/2.2.1")
		f.respond(w, r, false, http.StatusCreated, ocpi.Success(map[string]any{
			"token": f.tokenC, "url": f.URL + "/ocpi/versions", "roles": []map[string]any{{"role": "CPO", "party_id": "EVT", "country_code": "TH", "business_details": map[string]any{"name": "Spec CPO"}}},
		}))
	// Partner-initiated rotation. OCPI 2.2.1 §7.1.2 makes PUT symmetric, so the side that did not
	// start the handshake may still re-key it; one PUT replaces both directions.
	case r.URL.Path == "/ocpi/cpo/2.2.1/credentials" && r.Method == http.MethodPut:
		var credentials map[string]any
		_ = json.NewDecoder(r.Body).Decode(&credentials)
		partnerVersions := fmt.Sprint(credentials["url"])
		f.mu.Lock()
		f.tokenB = fmt.Sprint(credentials["token"])
		f.tokenC = "rotated-" + f.tokenC
		rotated := f.tokenC
		f.mu.Unlock()
		// The receiver must verify the newly offered token by calling the sender's Versions endpoints
		// before it accepts the PUT. This catches rotations that expose the token only after the call.
		f.callbackGET(partnerVersions)
		f.callbackGET(strings.TrimSuffix(partnerVersions, "/versions") + "/2.2.1")
		f.respond(w, r, false, http.StatusOK, ocpi.Success(map[string]any{
			"token": rotated, "url": f.URL + "/ocpi/versions", "roles": []map[string]any{{"role": "CPO", "party_id": "EVT", "country_code": "TH", "business_details": map[string]any{"name": "Spec CPO"}}},
		}))
	case strings.HasPrefix(r.URL.Path, "/ocpi/cpo/2.2.1/tokens/") && r.Method == http.MethodPut:
		var token map[string]any
		_ = json.NewDecoder(r.Body).Decode(&token)
		f.mu.Lock()
		f.pushedTokenUID = fmt.Sprint(token["uid"])
		f.mu.Unlock()
		f.respond(w, r, functional, http.StatusCreated, ocpi.Success(nil))
	case r.URL.Path == "/ocpi/cpo/2.2.1/locations" && r.Method == http.MethodGet:
		f.respond(w, r, functional, http.StatusOK, ocpi.Success([]map[string]any{fixtureLocation()}))
	case strings.HasPrefix(r.URL.Path, "/ocpi/cpo/2.2.1/commands/") && r.Method == http.MethodPost:
		var command map[string]any
		_ = json.NewDecoder(r.Body).Decode(&command)
		responseURL := fmt.Sprint(command["response_url"])
		go func() {
			time.Sleep(25 * time.Millisecond)
			f.push(f.t, responseURL, http.MethodPost, map[string]any{"result": "ACCEPTED"}, http.StatusOK)
		}()
		f.respond(w, r, functional, http.StatusOK, ocpi.Success(map[string]any{"result": "ACCEPTED", "timeout": 30}))
	default:
		f.respond(w, r, functional, http.StatusNotFound, ocpi.Error(2000, "fixture endpoint not found"))
	}
}

func (f *cpoFixture) callbackGET(target string) {
	request, _ := http.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Authorization", ocpi.EncodeCredential(f.tokenB))
	request.Header.Set("X-Request-ID", ocpi.NewID())
	request.Header.Set("X-Correlation-ID", ocpi.NewID())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatalf("fixture callback GET %s: %v", target, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		f.t.Fatalf("fixture callback GET %s: HTTP %d", target, response.StatusCode)
	}
}

func (f *cpoFixture) push(t *testing.T, target, method string, body any, wantStatus int) {
	raw, _ := json.Marshal(body)
	request, _ := http.NewRequest(method, target, bytes.NewReader(raw))
	request.Header.Set("Authorization", ocpi.EncodeCredential(f.tokenB))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", ocpi.NewID())
	request.Header.Set("X-Correlation-ID", ocpi.NewID())
	request.Header.Set("OCPI-to-country-code", "TH")
	request.Header.Set("OCPI-to-party-id", "TST")
	request.Header.Set("OCPI-from-country-code", "TH")
	request.Header.Set("OCPI-from-party-id", "EVT")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("push %s: %v", target, err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("push %s HTTP %d, want %d: %s", target, response.StatusCode, wantStatus, responseBody)
	}
}

func (f *cpoFixture) respond(w http.ResponseWriter, r *http.Request, functional bool, status int, envelope ocpi.Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", r.Header.Get("X-Request-ID"))
	w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
	if functional {
		w.Header().Set("OCPI-to-country-code", r.Header.Get("OCPI-from-country-code"))
		w.Header().Set("OCPI-to-party-id", r.Header.Get("OCPI-from-party-id"))
		w.Header().Set("OCPI-from-country-code", r.Header.Get("OCPI-to-country-code"))
		w.Header().Set("OCPI-from-party-id", r.Header.Get("OCPI-to-party-id"))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope)
}

func callAPI(t *testing.T, method, target string, body any, wantStatus int) map[string]any {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	request, _ := http.NewRequest(method, target, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer response.Body.Close()
	var result map[string]any
	_ = json.NewDecoder(response.Body).Decode(&result)
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s HTTP %d, want %d: %#v", method, target, response.StatusCode, wantStatus, result)
	}
	return result
}

func fixtureLocation() map[string]any {
	return map[string]any{
		"country_code": "TH", "party_id": "EVT", "id": "LOC1", "publish": true, "name": "Spec Station", "address": "1 Test Road", "city": "Bangkok", "country": "THA",
		"coordinates": map[string]any{"latitude": "13.7563", "longitude": "100.5018"}, "time_zone": "Asia/Bangkok", "last_updated": now(),
		"evses": []map[string]any{{"uid": "EVSE1", "evse_id": "TH*EVT*E0001", "status": "AVAILABLE", "last_updated": now(), "connectors": []map[string]any{{"id": "1", "standard": "IEC_62196_T2", "format": "SOCKET", "power_type": "AC_3_PHASE", "max_voltage": 400, "max_amperage": 32, "last_updated": now()}}}},
	}
}

func fixtureCDR() map[string]any {
	return map[string]any{
		"country_code": "TH", "party_id": "EVT", "id": "CDR1", "session_id": "SESSION1", "start_date_time": now(), "end_date_time": now(),
		"cdr_token":   map[string]any{"country_code": "TH", "party_id": "TST", "uid": "RFID-001", "type": "RFID", "contract_id": "TH-TST-C0001"},
		"auth_method": "COMMAND", "authorization_reference": "AUTH-001", "currency": "THB",
		"cdr_location":     map[string]any{"id": "LOC1", "name": "Spec Station", "address": "1 Test Road", "city": "Bangkok", "country": "THA", "coordinates": map[string]any{"latitude": "13.7563", "longitude": "100.5018"}, "evse_uid": "EVSE1", "evse_id": "TH*EVT*E0001", "connector_id": "1", "connector_standard": "IEC_62196_T2", "connector_format": "SOCKET", "connector_power_type": "AC_3_PHASE"},
		"charging_periods": []map[string]any{{"start_date_time": now(), "dimensions": []map[string]any{{"type": "ENERGY", "volume": 5.25}}}},
		"total_cost":       map[string]any{"excl_vat": 42.0, "incl_vat": 44.94}, "total_energy": 5.25, "total_time": 0.5, "last_updated": now(),
	}
}

func assertArrayLength(t *testing.T, state map[string]any, key string, want int) {
	t.Helper()
	if got := len(array(state[key])); got != want {
		t.Fatalf("%s length = %d, want %d", key, got, want)
	}
}
func array(value any) []any {
	if value == nil {
		return nil
	}
	return value.([]any)
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
