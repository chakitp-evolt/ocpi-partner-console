package cpo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

func TestPartnerCPOHappyFlowAgainstBlackBoxEMSP(t *testing.T) {
	var partnerURL string
	var evoltURL string
	var mu sync.Mutex
	received := []string{}

	evolt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Fatal("outbound OCPI request is missing Authorization")
		}
		switch r.URL.Path {
		case "/ocpi/versions":
			writePeerResponse(w, r, http.StatusOK, []map[string]any{{"version": "2.2.1", "url": evoltURL + "/ocpi/2.2.1"}}, "")
		case "/ocpi/2.2.1":
			base := evoltURL + "/ocpi/emsp/2.2.1"
			writePeerResponse(w, r, http.StatusOK, map[string]any{"version": "2.2.1", "endpoints": []map[string]any{
				{"identifier": "credentials", "role": "SENDER", "url": base + "/credentials"},
				{"identifier": "locations", "role": "RECEIVER", "url": base + "/locations"},
				{"identifier": "tariffs", "role": "RECEIVER", "url": base + "/tariffs"},
				{"identifier": "sessions", "role": "RECEIVER", "url": base + "/sessions"},
				{"identifier": "cdrs", "role": "RECEIVER", "url": base + "/cdrs"},
			}}, "")
		case "/ocpi/emsp/2.2.1/credentials":
			var credentials map[string]any
			_ = json.NewDecoder(r.Body).Decode(&credentials)
			tokenB := fmt.Sprint(credentials["token"])
			callback, _ := http.NewRequest(http.MethodGet, fmt.Sprint(credentials["url"]), nil)
			callback.Header.Set("Authorization", ocpi.EncodeCredential(tokenB))
			callback.Header.Set("X-Request-ID", "callback-request")
			callback.Header.Set("X-Correlation-ID", "callback-correlation")
			response, err := http.DefaultClient.Do(callback)
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("partner callback discovery failed: response=%v err=%v", response, err)
			}
			_ = response.Body.Close()
			writePeerResponse(w, r, http.StatusOK, map[string]any{"token": "evolt-token-c", "url": evoltURL + "/ocpi/versions", "roles": []map[string]any{{"role": "EMSP", "country_code": "TH", "party_id": "EVT", "business_details": map[string]any{"name": "Black-box eMSP"}}}}, "")
		default:
			if strings.HasPrefix(r.URL.Path, "/ocpi/emsp/2.2.1/") {
				mu.Lock()
				received = append(received, r.Method+" "+r.URL.Path)
				mu.Unlock()
				location := ""
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cdrs") {
					location = evoltURL + "/ocpi/emsp/2.2.1/cdrs/stored-cdr"
				}
				writePeerResponse(w, r, http.StatusOK, nil, location)
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer evolt.Close()
	evoltURL = evolt.URL

	controller := New("")
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	partner := httptest.NewServer(mux)
	defer partner.Close()
	partnerURL = partner.URL
	controller.mu.Lock()
	controller.state.Config.EvoltVersionsURL = evolt.URL + "/ocpi/versions"
	controller.state.Config.PartnerPublicURL = partnerURL
	controller.state.TokenA = "evolt-token-a"
	controller.mu.Unlock()

	callPartnerAPI(t, partner.URL+"/api/cpo/actions/discover", map[string]any{})
	callPartnerAPI(t, partner.URL+"/api/cpo/actions/register", map[string]any{})
	callPartnerAPI(t, partner.URL+"/api/cpo/actions/send", map[string]any{"module": "tariffs", "id": "BKK-ENERGY", "method": "PUT"})
	callPartnerAPI(t, partner.URL+"/api/cpo/actions/send", map[string]any{"module": "locations", "id": "BKK-SILOM", "method": "PUT"})

	created := callPartnerAPI(t, partner.URL+"/api/cpo/actions/session/create", map[string]any{"location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1", "connector_id": "1"})
	session := created["object"].(map[string]any)
	sessionID := fmt.Sprint(session["id"])
	callPartnerAPI(t, partner.URL+"/api/cpo/actions/send", map[string]any{"module": "sessions", "id": sessionID, "method": "PUT"})
	callPartnerAPI(t, partner.URL+"/api/cpo/actions/session/advance", map[string]any{"session_id": sessionID, "kwh": 2.5})
	callPartnerAPI(t, partner.URL+"/api/cpo/actions/session/complete", map[string]any{"session_id": sessionID})
	cdrCreated := callPartnerAPI(t, partner.URL+"/api/cpo/actions/session/cdr", map[string]any{"session_id": sessionID})
	cdrID := fmt.Sprint(cdrCreated["object"].(map[string]any)["id"])
	callPartnerAPI(t, partner.URL+"/api/cpo/actions/send", map[string]any{"module": "cdrs", "id": cdrID, "method": "POST"})

	controller.mu.RLock()
	if controller.state.Status != "REGISTERED" {
		t.Errorf("status = %s, want REGISTERED", controller.state.Status)
	}
	if controller.state.TokenC != "evolt-token-c" {
		t.Error("Token C was not stored")
	}
	if controller.state.Objects["cdrs"][cdrID].CDRLocation == "" {
		t.Error("CDR Location response header was not stored")
	}
	controller.mu.RUnlock()
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"PUT /ocpi/emsp/2.2.1/tariffs/TH/TST/BKK-ENERGY", "PUT /ocpi/emsp/2.2.1/locations/TH/TST/BKK-SILOM", "PUT /ocpi/emsp/2.2.1/sessions/TH/TST/", "PATCH /ocpi/emsp/2.2.1/sessions/TH/TST/", "POST /ocpi/emsp/2.2.1/cdrs"} {
		found := false
		for _, actual := range received {
			if strings.HasPrefix(actual, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing request %q in %#v", want, received)
		}
	}
}

func callPartnerAPI(t *testing.T, target string, body any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	response, err := http.Post(target, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseRaw, _ := io.ReadAll(response.Body)
	var result map[string]any
	_ = json.Unmarshal(responseRaw, &result)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("POST %s: HTTP %d: %s", target, response.StatusCode, responseRaw)
	}
	return result
}

func writePeerResponse(w http.ResponseWriter, r *http.Request, status int, data any, location string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", r.Header.Get("X-Request-ID"))
	w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
	if r.Header.Get("OCPI-from-party-id") != "" {
		w.Header().Set("OCPI-to-country-code", r.Header.Get("OCPI-from-country-code"))
		w.Header().Set("OCPI-to-party-id", r.Header.Get("OCPI-from-party-id"))
		w.Header().Set("OCPI-from-country-code", r.Header.Get("OCPI-to-country-code"))
		w.Header().Set("OCPI-from-party-id", r.Header.Get("OCPI-to-party-id"))
	}
	if location != "" {
		w.Header().Set("Location", location)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ocpi.Success(data))
}
