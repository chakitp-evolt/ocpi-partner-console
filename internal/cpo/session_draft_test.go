package cpo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func callSessionAction(t *testing.T, controller *Controller, action string, input map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	var payload bytes.Buffer
	_ = json.NewEncoder(&payload).Encode(input)
	request := httptest.NewRequest(http.MethodPost, "https://partner.example/api/cpo/actions/session/"+action, &payload)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	result := map[string]any{}
	_ = json.Unmarshal(recorder.Body.Bytes(), &result)
	return recorder, result
}

// BKK-SILOM's first EVSE has one connector; the second has two.
func TestDraftFromStartSessionResolvesTheConnectorEvoltDidNotSend(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.Commands["cmd-1"] = &CommandRecord{
		ID: "cmd-1", Type: "START_SESSION", Status: "WAITING",
		Request: map[string]any{
			"response_url": "https://evolt.example/cb",
			"location_id":  "BKK-SILOM", "evse_uid": "SIL-EVSE-1",
			"token": map[string]any{"country_code": "TH", "party_id": "EVT", "uid": "DRIVER-1", "type": "APP_USER", "contract_id": "TH-EVT-C1A2B3C4D-2", "valid": true, "whitelist": "NEVER"},
		},
	}
	controller.mu.Unlock()

	recorder, result := callSessionAction(t, controller, "create", map[string]any{"command_id": "cmd-1"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s — a START_SESSION without connector_id must still draft", recorder.Code, recorder.Body)
	}
	object := result["object"].(map[string]any)
	if object["connector_id"] != "1" {
		t.Errorf("connector_id = %v, want the single connector of SIL-EVSE-1", object["connector_id"])
	}
	if object["auth_method"] != "COMMAND" {
		t.Errorf("auth_method = %v, want COMMAND", object["auth_method"])
	}
	token := object["cdr_token"].(map[string]any)
	if token["uid"] != "DRIVER-1" || token["contract_id"] != "TH-EVT-C1A2B3C4D-2" {
		t.Errorf("cdr_token = %#v, want the token the command carried", token)
	}
	if _, repeated := token["whitelist"]; repeated {
		t.Error("CdrToken must not repeat Token-only fields")
	}
}

func TestDraftFromStartSessionAsksWhichHeadWhenTheEVSEHasSeveral(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.Commands["cmd-2"] = &CommandRecord{
		ID: "cmd-2", Type: "START_SESSION", Status: "WAITING",
		Request: map[string]any{"location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-2"},
	}
	controller.mu.Unlock()

	recorder, result := callSessionAction(t, controller, "create", map[string]any{"command_id": "cmd-2"})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with an explanation", recorder.Code)
	}
	message, _ := result["error"].(string)
	if message == "" || message == "EVSE/Connector not found" {
		t.Fatalf("error = %q — the tester has to be told which connectors exist", message)
	}

	recorder, result = callSessionAction(t, controller, "create", map[string]any{"command_id": "cmd-2", "connector_id": "2"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s — the picked connector must be accepted", recorder.Code, recorder.Body)
	}
	if object := result["object"].(map[string]any); object["connector_id"] != "2" {
		t.Errorf("connector_id = %v, want the connector the tester picked", object["connector_id"])
	}
}

func TestDraftPricesTheSessionFromTheConnectorTariff(t *testing.T) {
	controller := New("https://partner.example")
	_, result := callSessionAction(t, controller, "create", map[string]any{"location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1", "connector_id": "1"})
	price := result["price"].(map[string]any)
	if price["tariff_id"] != "BKK-ENERGY" {
		t.Fatalf("tariff_id = %v, want the tariff the connector points at", price["tariff_id"])
	}
	object := result["object"].(map[string]any)
	periods := object["charging_periods"].([]any)
	if len(periods) != 0 {
		t.Fatalf("charging_periods = %#v, want no period before the first measured interval", periods)
	}
}

func TestAdvanceRepricesFromTheTariffAndRecordsTheDeltaNotTheRunningTotal(t *testing.T) {
	controller := New("https://partner.example")
	_, created := callSessionAction(t, controller, "create", map[string]any{"location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1", "connector_id": "1"})
	sessionID := created["object"].(map[string]any)["id"].(string)

	controller.mu.Lock()
	record := controller.state.Objects["sessions"][sessionID]
	started, err := parseOCPITime(fieldText(record.Body, "start_date_time"))
	if err != nil {
		t.Fatal(err)
	}
	firstEnd := started.Add(15 * time.Second).Format(time.RFC3339Nano)
	secondEnd := started.Add(30 * time.Second).Format(time.RFC3339Nano)
	patchOne := controller.meterUpdate(record, 2, false, firstEnd, true)
	patchTwo := controller.meterUpdate(record, 3, false, secondEnd, true)
	controller.mu.Unlock()

	if patchOne["total_cost"].(map[string]any)["excl_vat"] != 15.0 {
		t.Errorf("first total_cost = %#v, want 2 kWh × 7.5", patchOne["total_cost"])
	}
	if patchTwo["total_cost"].(map[string]any)["excl_vat"] != 37.5 {
		t.Errorf("second total_cost = %#v, want 5 kWh × 7.5", patchTwo["total_cost"])
	}
	if got := sumDimension(patchOne["charging_periods"].([]any), "ENERGY"); got != 2 {
		t.Errorf("first PATCH ENERGY = %v, want only its 2 kWh delta", got)
	}
	if got := sumDimension(patchTwo["charging_periods"].([]any), "ENERGY"); got != 3 {
		t.Errorf("second PATCH ENERGY = %v, want only its 3 kWh delta", got)
	}
	if got := patchOne["charging_periods"].([]any)[0].(map[string]any)["start_date_time"]; got != fieldText(record.Body, "start_date_time") {
		t.Errorf("first period starts at %v, want Session start", got)
	}
	if got, _ := parseOCPITime(fmt.Sprint(patchTwo["charging_periods"].([]any)[0].(map[string]any)["start_date_time"])); !got.Equal(started.Add(15 * time.Second)) {
		t.Errorf("second period starts at %v, want previous meter update %s", got, firstEnd)
	}
	periods := record.Body["charging_periods"].([]any)
	if got := sumDimension(periods, "ENERGY"); got != 5 {
		t.Errorf("stored ENERGY across periods = %v, want 5 kWh", got)
	}
	if len(periods) != 2 {
		t.Errorf("stored charging_periods = %d, want one period per meter update", len(periods))
	}
}

func TestMeterTargetSendsOnlyNewPeriodsAndCompletesAtTheLiveReading(t *testing.T) {
	patches := []map[string]any{}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode PATCH: %v", err)
		}
		patches = append(patches, body)
		writePeerResponse(w, r, http.StatusOK, nil, "")
	}))
	defer peer.Close()
	controller := New("")
	controller.mu.Lock()
	controller.state.TokenC = "test-token"
	controller.state.Config.RemoteIdentity = map[string]any{"country_code": "TH", "party_id": "EVT"}
	controller.state.Endpoints = []Endpoint{{Identifier: "sessions", Role: "RECEIVER", URL: peer.URL + "/sessions"}}
	controller.mu.Unlock()
	_, created := callSessionAction(t, controller, "create", map[string]any{"location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1", "connector_id": "1"})
	sessionID := created["object"].(map[string]any)["id"].(string)
	controller.mu.Lock()
	controller.state.Objects["sessions"][sessionID].Delivery = "SENT"
	controller.mu.Unlock()
	for _, target := range []float64{4.5, 6.0} {
		recorder, _ := callSessionAction(t, controller, "advance", map[string]any{"session_id": sessionID, "target_kwh": target})
		if recorder.Code != http.StatusOK {
			t.Fatalf("advance to %g: HTTP %d %s", target, recorder.Code, recorder.Body.String())
		}
	}
	recorder, _ := callSessionAction(t, controller, "advance", map[string]any{"session_id": sessionID, "target_kwh": 5.0})
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("decreasing cumulative meter: HTTP %d, want 422", recorder.Code)
	}
	recorder, _ = callSessionAction(t, controller, "complete", map[string]any{"session_id": sessionID, "target_kwh": 6.25})
	if recorder.Code != http.StatusOK {
		t.Fatalf("complete: HTTP %d %s", recorder.Code, recorder.Body.String())
	}
	if len(patches) != 3 {
		t.Fatalf("PATCH count = %d, want 3", len(patches))
	}
	for i, want := range []float64{4.5, 1.5, 0.25} {
		periods, ok := patches[i]["charging_periods"].([]any)
		if !ok || len(periods) != 1 || sumDimension(periods, "ENERGY") != want {
			t.Errorf("PATCH %d periods = %#v, want one new period with %g kWh", i, patches[i]["charging_periods"], want)
		}
	}
	if patches[2]["status"] != "COMPLETED" || patches[2]["kwh"] != 6.25 {
		t.Errorf("complete PATCH = %#v, want COMPLETED at 6.25 kWh", patches[2])
	}
	controller.mu.RLock()
	record := controller.state.Objects["sessions"][sessionID]
	if record.Body["kwh"] != 6.25 || len(record.Body["charging_periods"].([]any)) != 3 {
		t.Errorf("stored Session = %#v, want cumulative 6.25 kWh and three periods", record.Body)
	}
	controller.mu.RUnlock()
}

func TestCompleteWithoutMeterAdvanceStillCreatesACDRPeriod(t *testing.T) {
	controller := New("")
	_, created := callSessionAction(t, controller, "create", map[string]any{"location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1", "connector_id": "1"})
	sessionID := created["object"].(map[string]any)["id"].(string)
	controller.mu.Lock()
	record := controller.state.Objects["sessions"][sessionID]
	patch := controller.meterUpdate(record, 0, true, fieldText(record.Body, "start_date_time"), true)
	controller.mu.Unlock()
	periods := record.Body["charging_periods"].([]any)
	if len(periods) != 1 || len(patch["charging_periods"].([]any)) != 1 {
		t.Fatalf("zero-energy completion must keep one ChargingPeriod for the CDR: %#v", periods)
	}
	if got := sumDimension(periods, "ENERGY"); got != 0 {
		t.Errorf("zero-energy completion records %g kWh, want 0", got)
	}
}

func TestFavoriteDriverSuppliesTheCdrTokenAndRemembersWhereItCharged(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.state.Objects["tokens"]["TH/EVT/DRIVER-9/APP_USER"] = &Record{Body: map[string]any{
		"country_code": "TH", "party_id": "EVT", "uid": "DRIVER-9", "type": "APP_USER",
		"contract_id": "TH-EVT-C9Z8Y7X6W-5", "valid": true, "whitelist": "NEVER",
	}}
	controller.mu.Unlock()

	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "https://partner.example/api/cpo/favorites/TH/EVT/DRIVER-9/APP_USER", bytes.NewBufferString(`{}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("favourite PUT = %d body = %s", recorder.Code, recorder.Body)
	}

	_, result := callSessionAction(t, controller, "create", map[string]any{"favorite_id": "TH/EVT/DRIVER-9/APP_USER", "location_id": "BKK-ARI", "evse_uid": "ARI-EVSE-1", "connector_id": "1"})
	token := result["object"].(map[string]any)["cdr_token"].(map[string]any)
	if token["uid"] != "DRIVER-9" || token["contract_id"] != "TH-EVT-C9Z8Y7X6W-5" {
		t.Fatalf("cdr_token = %#v, want the favourite driver", token)
	}
	controller.mu.RLock()
	favorite := controller.favorites["TH/EVT/DRIVER-9/APP_USER"]
	controller.mu.RUnlock()
	if favorite.LocationID != "BKK-ARI" || favorite.EVSEUID != "ARI-EVSE-1" || favorite.ConnectorID != "1" {
		t.Errorf("favourite = %#v, want the last place it was drafted against", favorite)
	}

	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "https://partner.example/api/cpo/favorites/TH/EVT/DRIVER-9/APP_USER", nil))
	controller.mu.RLock()
	remaining := len(controller.favorites)
	controller.mu.RUnlock()
	if recorder.Code != http.StatusOK || remaining != 0 {
		t.Errorf("delete = %d, favourites left = %d", recorder.Code, remaining)
	}
}

func TestFavouritesSurviveARunReset(t *testing.T) {
	controller := New("https://partner.example")
	controller.mu.Lock()
	controller.favorites["TH/EVT/DRIVER-9/APP_USER"] = &FavoriteToken{ID: "TH/EVT/DRIVER-9/APP_USER", Token: map[string]any{"uid": "DRIVER-9"}}
	controller.reset(true)
	remaining := len(controller.favorites)
	controller.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("favourites after reset = %d, want the shortlist to outlive the run", remaining)
	}
}
