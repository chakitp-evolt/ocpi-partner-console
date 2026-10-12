package cpo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

func TestManualStopWaitsForUnplugAndLeavesParkingCDRToTester(t *testing.T) {
	var postedCDR map[string]any
	var statusMu sync.Mutex
	var evseStatuses []string
	var peerURL string
	var failCDROnce atomic.Bool
	var failAvailableOnce atomic.Bool
	var cdrPosts atomic.Int32
	failCDROnce.Store(true)
	failAvailableOnce.Store(true)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		location := ""
		if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/locations/") {
			var patch map[string]any
			_ = json.NewDecoder(r.Body).Decode(&patch)
			statusMu.Lock()
			status := fieldText(patch, "status")
			evseStatuses = append(evseStatuses, status)
			statusMu.Unlock()
			if status == "AVAILABLE" && failAvailableOnce.Swap(false) {
				_ = json.NewEncoder(w).Encode(ocpi.Error(2000, "temporary Location failure"))
				return
			}
		}
		if r.Method == http.MethodPost && r.URL.Path == "/cdrs" {
			cdrPosts.Add(1)
			_ = json.NewDecoder(r.Body).Decode(&postedCDR)
			if failCDROnce.Swap(false) {
				_ = json.NewEncoder(w).Encode(ocpi.Error(2000, "temporary CDR failure"))
				return
			}
			location = peerURL + "/cdrs/stored"
		}
		writePeerResponse(w, r, http.StatusOK, nil, location)
	}))
	defer peer.Close()
	peerURL = peer.URL
	c := New(peer.URL)
	c.mu.Lock()
	c.state.TokenC = "test-token"
	c.state.Endpoints = []Endpoint{
		{Identifier: "sessions", Role: "RECEIVER", URL: peer.URL + "/sessions"},
		{Identifier: "cdrs", Role: "RECEIVER", URL: peer.URL + "/cdrs"},
		{Identifier: "locations", Role: "RECEIVER", URL: peer.URL + "/locations"},
	}
	c.mu.Unlock()
	created, err := c.callSessionAction(t.Context(), "create", map[string]any{"location_id": "BKK-ARI", "evse_uid": "ARI-EVSE-1", "connector_id": "1"})
	if err != nil {
		t.Fatal(err)
	}
	session := created["object"].(map[string]any)
	id := fieldText(session, "id")
	if err := c.callSend(t.Context(), "sessions", id, http.MethodPut); err != nil {
		t.Fatal(err)
	}
	if got := testEVSEStatus(c.state.Objects["locations"]["BKK-ARI"].Body, "ARI-EVSE-1"); got != "CHARGING" {
		t.Fatalf("manual initial Session PUT must PATCH EVSE CHARGING, got %s", got)
	}
	c.mu.Lock()
	c.state.Objects["locations"]["BKK-ARI"].Body["name"] = "Location renamed after start"
	c.state.Objects["tariffs"]["BKK-MIXED"].Body["elements"] = []any{map[string]any{"price_components": []any{map[string]any{"type": "PARKING_TIME", "price": 999.0, "step_size": 1}}}}
	c.state.Commands["manual-stop"] = &CommandRecord{ID: "manual-stop", Type: "STOP_SESSION", Status: "ACCEPTED", CreatedAt: now(), Request: map[string]any{"session_id": id}, Result: map[string]any{"result": "ACCEPTED"}}
	c.mu.Unlock()
	if _, err := c.callSessionAction(t.Context(), "complete", map[string]any{"session_id": id}); err == nil {
		t.Fatal("Complete must not bypass a pending STOP_SESSION")
	}
	if _, err := c.callSessionAction(t.Context(), "stop", map[string]any{"session_id": id, "target_kwh": 2.0}); err != nil {
		t.Fatal(err)
	}
	c.mu.RLock()
	stopped := c.state.Objects["sessions"][id]
	idleStarted := fieldText(c.state.Commands["manual-stop"].Result, "idle_started_at")
	if fieldText(stopped.Body, "status") != "ACTIVE" || stopped.Body["end_date_time"] != nil || c.state.Objects["cdrs"]["CDR-"+id] != nil {
		t.Errorf("STOP must leave active Session and no CDR: %+v", stopped.Body)
	}
	c.mu.RUnlock()
	if idleStarted == "" {
		t.Fatal("STOP did not start the idle timer")
	}
	backdateSession(c, id, "manual-stop", 2*time.Minute)
	if got := testEVSEStatus(c.state.Objects["locations"]["BKK-ARI"].Body, "ARI-EVSE-1"); got != "CHARGING" {
		t.Errorf("EVSE must remain CHARGING while plugged in, got %s", got)
	}
	mux := http.NewServeMux()
	c.RegisterRoutes(mux)
	previewResponse := httptest.NewRecorder()
	mux.ServeHTTP(previewResponse, httptest.NewRequest(http.MethodGet, "https://partner.example/api/cpo/actions/session/"+id+"/parking", nil))
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("parking preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var preview map[string]any
	_ = json.Unmarshal(previewResponse.Body.Bytes(), &preview)
	if number(preview["parking_cost"].(map[string]any)["excl_vat"]) != 7.5 {
		t.Errorf("parking preview does not use tariff step_size: %+v", preview)
	}
	if _, err := c.callSessionAction(t.Context(), "advance", map[string]any{"session_id": id, "kwh": 1.0}); err == nil {
		t.Fatal("meter advanced after charging stopped")
	}
	first, err := c.callSessionAction(t.Context(), "unplug", map[string]any{"session_id": id})
	if err != nil {
		t.Fatal(err)
	}
	if first["manual_cdr"] != true || first["cdr_id"] != nil || fieldText(first, "evse_status_error") == "" {
		t.Fatalf("first unplug must close the Session, report the EVSE failure and leave the CDR: %+v", first)
	}
	c.mu.RLock()
	firstPeriods, _ := c.state.Objects["sessions"][id].Body["charging_periods"].([]any)
	firstCount := len(firstPeriods)
	firstCost := c.state.Objects["sessions"][id].Body["total_cost"]
	c.mu.RUnlock()
	if _, err := c.callSessionAction(t.Context(), "unplug", map[string]any{"session_id": id}); err != nil {
		t.Fatal(err)
	}
	c.mu.RLock()
	completed := c.state.Objects["sessions"][id]
	autoCDR := c.state.Objects["cdrs"]["CDR-"+id]
	c.mu.RUnlock()
	if fieldText(completed.Body, "status") != "COMPLETED" || completed.Delivery != "SENT" || autoCDR != nil || cdrPosts.Load() != 0 {
		t.Fatalf("manual unplug must complete Session without a CDR: session=%+v cdr=%+v posts=%d", completed, autoCDR, cdrPosts.Load())
	}
	if len(completed.Body["charging_periods"].([]any)) != firstCount || number(completed.Body["total_cost"].(map[string]any)["excl_vat"]) != number(firstCost.(map[string]any)["excl_vat"]) {
		t.Error("retry must not extend parking or add periods")
	}
	if _, err := c.callSessionAction(t.Context(), "cdr", map[string]any{"session_id": id}); err != nil {
		t.Fatal(err)
	}
	if err := c.callSend(t.Context(), "cdrs", "CDR-"+id, http.MethodPost); err == nil {
		t.Fatal("first CDR POST should fail so the tester can resend it")
	}
	if err := c.callSend(t.Context(), "cdrs", "CDR-"+id, http.MethodPost); err != nil {
		t.Fatal(err)
	}
	c.mu.RLock()
	cdrRecord := c.state.Objects["cdrs"]["CDR-"+id]
	c.mu.RUnlock()
	if cdrRecord == nil || cdrRecord.Delivery != "SENT" || postedCDR == nil || cdrPosts.Load() != 2 {
		t.Fatalf("manual CDR send must reach the peer: cdr=%+v posts=%d", cdrRecord, cdrPosts.Load())
	}
	cdr := cdrRecord.Body
	periods, _ := cdr["charging_periods"].([]any)
	if sumDimension(periods, "PARKING_TIME") <= 0 || number(cdr["total_parking_time"]) <= 0 {
		t.Errorf("CDR lacks PARKING_TIME: %+v", cdr)
	}
	if number(cdr["total_parking_cost"].(map[string]any)["excl_vat"]) != 7.5 {
		t.Errorf("parking step_size=900 must charge 15 minutes at 30 THB/hour: %+v", cdr["total_parking_cost"])
	}
	if number(cdr["total_cost"].(map[string]any)["excl_vat"]) != 33.5 {
		t.Errorf("total cost must include FLAT + ENERGY + PARKING_TIME: %+v", cdr["total_cost"])
	}
	if number(cdr["total_cost"].(map[string]any)["incl_vat"]) != number(completed.Body["total_cost"].(map[string]any)["incl_vat"]) {
		t.Error("Session and CDR totals differ")
	}
	if len(cdr["tariffs"].([]any)) != 1 || postedCDR["total_parking_cost"] == nil {
		t.Error("POSTed CDR must include tariff snapshot and parking breakdown")
	}
	if fieldText(cdr["cdr_location"].(map[string]any), "name") != "Evolt Partner Ari" {
		t.Errorf("CDR Location changed after Session start: %+v", cdr["cdr_location"])
	}
	if number(cdr["total_time"]) < number(cdr["total_parking_time"]) {
		t.Error("CDR parking time exceeds total Session time")
	}
	if got := testEVSEStatus(c.state.Objects["locations"]["BKK-ARI"].Body, "ARI-EVSE-1"); got != "AVAILABLE" {
		t.Errorf("EVSE after unplug = %s", got)
	}
	statusMu.Lock()
	if len(evseStatuses) != 3 || evseStatuses[0] != "CHARGING" || evseStatuses[1] != "AVAILABLE" || evseStatuses[2] != "AVAILABLE" {
		t.Errorf("EVSE PATCH statuses = %v, want [CHARGING AVAILABLE AVAILABLE retry]", evseStatuses)
	}
	statusMu.Unlock()
	if fieldText(c.state.Commands["manual-stop"].Result, "evse_status_error") != "" {
		t.Error("successful retry did not clear the EVSE status error")
	}
	for _, finding := range cdrRecord.Findings {
		if finding.Level == "error" {
			t.Errorf("invalid CDR: %+v", finding)
		}
	}
}

func TestParkingPricingSplitsAtTariffBoundaryAndRoundsOnce(t *testing.T) {
	start := time.Date(2026, 10, 5, 17, 55, 0, 0, time.UTC)
	tariff := map[string]any{"elements": []any{
		map[string]any{"price_components": []any{map[string]any{"type": "PARKING_TIME", "price": 30.0, "step_size": 900}}, "restrictions": map[string]any{"start_time": "18:00", "end_time": "23:59"}},
		map[string]any{"price_components": []any{map[string]any{"type": "PARKING_TIME", "price": 10.0, "step_size": 900}}},
	}}
	segments := parkingSegments(tariff, start, 10.0/60, start, 0)
	if len(segments) != 2 || segments[1].Start.Format("15:04") != "18:00" {
		t.Fatalf("parking periods must split at 18:00: %+v", segments)
	}
	price := priceSessionWithParking(tariff, "T", nil, start, start, 10.0/60)
	if price.ExclVAT != 5.83 || len(price.Lines) != 2 {
		t.Fatalf("5 min at 10 + 10 billed min at 30 = 5.83, got %+v", price)
	}
	withTime := map[string]any{"elements": []any{map[string]any{"price_components": []any{
		map[string]any{"type": "TIME", "price": 60.0, "step_size": 600},
		map[string]any{"type": "PARKING_TIME", "price": 30.0, "step_size": 900},
	}}}}
	price = priceSessionWithParking(withTime, "T", []chargeSpan{{Start: start, End: start.Add(21 * time.Minute)}}, start, start.Add(21*time.Minute), 16.0/60)
	if price.ExclVAT != 36 {
		t.Fatalf("TIME 21 min stays unrounded; PARKING_TIME 16 min rounds to 30: %+v", price)
	}
}

func TestManualStopWithoutParkingPriceCompletesWithoutCDR(t *testing.T) {
	var cdrPosts atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/cdrs" {
			cdrPosts.Add(1)
		}
		writePeerResponse(w, r, http.StatusOK, nil, "")
	}))
	defer peer.Close()
	c := New(peer.URL)
	c.mu.Lock()
	c.state.TokenC = "test-token"
	c.state.Endpoints = []Endpoint{
		{Identifier: "sessions", Role: "RECEIVER", URL: peer.URL + "/sessions"},
		{Identifier: "cdrs", Role: "RECEIVER", URL: peer.URL + "/cdrs"},
		{Identifier: "locations", Role: "RECEIVER", URL: peer.URL + "/locations"},
	}
	c.mu.Unlock()
	created, err := c.callSessionAction(t.Context(), "create", map[string]any{"location_id": "BKK-ARI", "evse_uid": "ARI-EVSE-1", "connector_id": "1"})
	if err != nil {
		t.Fatal(err)
	}
	id := fieldText(created["object"].(map[string]any), "id")
	if err := c.callSend(t.Context(), "sessions", id, http.MethodPut); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state.Objects["sessions"][id].Snapshot["tariff"] = map[string]any{"id": "ENERGY-ONLY", "elements": []any{map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 8.0, "step_size": 1}}}}}
	c.state.Commands["manual-stop"] = &CommandRecord{ID: "manual-stop", Type: "STOP_SESSION", Status: "ACCEPTED", CreatedAt: now(), Request: map[string]any{"session_id": id}, Result: map[string]any{"result": "ACCEPTED"}}
	c.mu.Unlock()

	mux := http.NewServeMux()
	c.RegisterRoutes(mux)
	stateResponse := httptest.NewRecorder()
	mux.ServeHTTP(stateResponse, httptest.NewRequest(http.MethodGet, "/api/cpo/state", nil))
	var state struct {
		Objects map[string][]map[string]any `json:"objects"`
	}
	_ = json.Unmarshal(stateResponse.Body.Bytes(), &state)
	if len(state.Objects["sessions"]) != 1 || state.Objects["sessions"][0]["bills_parking"] != false {
		t.Fatalf("state must tell the UI the Session bills no parking: %+v", state.Objects["sessions"])
	}

	result, err := c.callSessionAction(t.Context(), "stop", map[string]any{"session_id": id, "target_kwh": 3.0})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.RLock()
	session := c.state.Objects["sessions"][id]
	cdr := c.state.Objects["cdrs"]["CDR-"+id]
	manual := c.state.Commands["manual-stop"].Result["manual_cdr"]
	c.mu.RUnlock()
	if result["completed"] != true || fieldText(session.Body, "status") != "COMPLETED" || session.Delivery != "SENT" {
		t.Fatalf("stop without a parking price must complete the Session: result=%+v session=%+v", result, session)
	}
	if cdr != nil || cdrPosts.Load() != 0 || manual != true {
		t.Fatalf("manual stop must leave the CDR to the tester: cdr=%+v posts=%d manual=%v", cdr, cdrPosts.Load(), manual)
	}
}

func TestSessionEditorPreservesStartTariffSnapshot(t *testing.T) {
	c := New("https://partner.example")
	created, err := c.callSessionAction(t.Context(), "create", map[string]any{"location_id": "BKK-ARI", "evse_uid": "ARI-EVSE-1", "connector_id": "1"})
	if err != nil {
		t.Fatal(err)
	}
	session := created["object"].(map[string]any)
	id := fieldText(session, "id")
	c.mu.RLock()
	original := cloneMap(c.state.Objects["sessions"][id].Snapshot)
	c.mu.RUnlock()
	raw, _ := json.Marshal(session)
	request := httptest.NewRequest(http.MethodPut, "/api/cpo/objects/sessions/"+id, strings.NewReader(string(raw)))
	request.SetPathValue("module", "sessions")
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	c.apiSaveObject(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("save Session: %s", response.Body.String())
	}
	c.mu.RLock()
	saved := c.state.Objects["sessions"][id].Snapshot
	c.mu.RUnlock()
	if !reflect.DeepEqual(saved, original) {
		t.Fatalf("Session editor discarded the Location/Tariff snapshot: saved=%v original=%v", saved, original)
	}
}

// backdateSession moves a stopped Session's clock back, so parking has elapsed time without sleeping.
func backdateSession(c *Controller, sessionID, stopID string, by time.Duration) {
	shift := func(value any) string {
		at, err := parseOCPITime(fmt.Sprint(value))
		if err != nil {
			return fmt.Sprint(value)
		}
		return ocpi.FormatTime(at.Add(-by))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	body := c.state.Objects["sessions"][sessionID].Body
	for _, field := range []string{"start_date_time", "last_updated"} {
		body[field] = shift(body[field])
	}
	periods, _ := body["charging_periods"].([]any)
	for _, raw := range periods {
		period, _ := raw.(map[string]any)
		period["start_date_time"] = shift(period["start_date_time"])
	}
	result := c.state.Commands[stopID].Result
	result["idle_started_at"] = shift(result["idle_started_at"])
}

func parkingPeer(t *testing.T) (*Controller, *[]map[string]any, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var patches []map[string]any
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		location := ""
		if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/sessions/") {
			var patch map[string]any
			_ = json.NewDecoder(r.Body).Decode(&patch)
			mu.Lock()
			patches = append(patches, patch)
			mu.Unlock()
		}
		if r.Method == http.MethodPost && r.URL.Path == "/cdrs" {
			location = "http://peer/cdrs/stored"
		}
		writePeerResponse(w, r, http.StatusOK, nil, location)
	}))
	t.Cleanup(peer.Close)
	c := New(peer.URL)
	c.state.TokenC = "test-token"
	c.state.Config.AutoCommandFlow = true
	c.state.Endpoints = []Endpoint{
		{Identifier: "sessions", Role: "RECEIVER", URL: peer.URL + "/sessions"},
		{Identifier: "cdrs", Role: "RECEIVER", URL: peer.URL + "/cdrs"},
		{Identifier: "locations", Role: "RECEIVER", URL: peer.URL + "/locations"},
	}
	return c, &patches, &mu
}

func parkedSession(t *testing.T, c *Controller) string {
	t.Helper()
	created, err := c.callSessionAction(t.Context(), "create", map[string]any{"location_id": "BKK-ARI", "evse_uid": "ARI-EVSE-1", "connector_id": "1"})
	if err != nil {
		t.Fatal(err)
	}
	id := fieldText(created["object"].(map[string]any), "id")
	if err := c.callSend(t.Context(), "sessions", id, http.MethodPut); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state.Commands["stop"] = &CommandRecord{ID: "stop", Type: "STOP_SESSION", CreatedAt: now(), Request: map[string]any{"session_id": id}, Result: map[string]any{"result": "ACCEPTED"}}
	c.mu.Unlock()
	if err := c.beginIdle(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	backdateSession(c, id, "stop", 20*time.Minute)
	return id
}

// While the car is still plugged in after STOP_SESSION, the eMSP receives PARKING_TIME and the growing
// total_cost (OCPI 2.2.1 §9.1.1), and the closing update does not count that parking a second time.
func TestParkingIsReportedWhileStillPluggedIn(t *testing.T) {
	c, patches, mu := parkingPeer(t)
	id := parkedSession(t, c)
	if !c.advanceParking(id, time.Now()) {
		t.Fatalf("parking update was not sent: %+v", c.state.Commands["stop"].Result)
	}
	mu.Lock()
	last := (*patches)[len(*patches)-1]
	mu.Unlock()
	periods, _ := last["charging_periods"].([]any)
	if parked := sumDimension(periods, "PARKING_TIME"); parked < 19.9/60 || last["status"] != nil {
		t.Fatalf("parking PATCH must append about 20 min of PARKING_TIME and keep the Session ACTIVE: %+v", last)
	}
	if got := number(last["total_cost"].(map[string]any)["excl_vat"]); got != 35 {
		t.Fatalf("total_cost = %v, want FLAT 10 + 1.25 kWh at 8 + 30 min of parking at 30/h", got)
	}
	if _, err := c.callSessionAction(t.Context(), "unplug", map[string]any{"session_id": id}); err != nil {
		t.Fatal(err)
	}
	c.mu.RLock()
	session := c.state.Objects["sessions"][id].Body
	cdr := c.state.Objects["cdrs"]["CDR-"+id].Body
	c.mu.RUnlock()
	all, _ := session["charging_periods"].([]any)
	if diff := sumDimension(all, "PARKING_TIME") - number(cdr["total_parking_time"]); diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("Session periods carry %v h of parking but the CDR bills %v h", sumDimension(all, "PARKING_TIME"), cdr["total_parking_time"])
	}
	if fieldText(session, "status") != "COMPLETED" || number(cdr["total_cost"].(map[string]any)["excl_vat"]) != 35 {
		t.Fatalf("unplug must close the Session and bill the same parking: session=%+v cdr=%+v", session, cdr["total_cost"])
	}
}

// A Session without a priced Tariff still ends when the car leaves; only its CDR cannot be produced.
func TestUnpricedIdleSessionStillCompletesOnUnplug(t *testing.T) {
	c, patches, mu := parkingPeer(t)
	delete(c.state.Objects["tariffs"], "BKK-MIXED")
	id := parkedSession(t, c)
	if _, err := c.callSessionAction(t.Context(), "unplug", map[string]any{"session_id": id}); err == nil {
		t.Fatal("an unpriced Session must not produce a CDR")
	}
	mu.Lock()
	last := (*patches)[len(*patches)-1]
	mu.Unlock()
	periods, _ := last["charging_periods"].([]any)
	if last["status"] != "COMPLETED" || last["total_cost"] != nil || sumDimension(periods, "PARKING_TIME") <= 0 {
		t.Fatalf("closing PATCH must complete the Session with its parking time and no total_cost: %+v", last)
	}
	if got := testEVSEStatus(c.state.Objects["locations"]["BKK-ARI"].Body, "ARI-EVSE-1"); got != "AVAILABLE" {
		t.Fatalf("EVSE after unplug = %s, want AVAILABLE", got)
	}
}

// By default every meter tick reports its Charging Period; with a longer period_interval_seconds the
// ticks in between update kwh and total_cost only (OCPI 2.2.1 §9.3.1).
func TestAutoMeterReportsPeriodsOnlyEveryPeriodInterval(t *testing.T) {
	c, patches, mu := parkingPeer(t)
	if !c.state.Config.periodDue(time.Now(), time.Now()) {
		t.Fatal("by default every meter tick must report its Charging Period")
	}
	c.state.Config.PeriodIntervalSeconds = 300
	created, err := c.callSessionAction(t.Context(), "create", map[string]any{"location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1", "connector_id": "1"})
	if err != nil {
		t.Fatal(err)
	}
	id := fieldText(created["object"].(map[string]any), "id")
	if err := c.callSend(t.Context(), "sessions", id, http.MethodPut); err != nil {
		t.Fatal(err)
	}
	started, _ := parseOCPITime(fieldText(c.state.Objects["sessions"][id].Body, "start_date_time"))
	if err := c.autoMeterTick(id, 0.5, started.Add(15*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := c.autoMeterTick(id, 1.0, started.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	first, second := (*patches)[len(*patches)-2], (*patches)[len(*patches)-1]
	mu.Unlock()
	if first["charging_periods"] != nil || number(first["kwh"]) != 0.5 || first["total_cost"] == nil {
		t.Fatalf("tick inside the interval must update kwh and total_cost only: %+v", first)
	}
	periods, _ := second["charging_periods"].([]any)
	if got := sumDimension(periods, "ENERGY"); got < 0.999999 || got > 1.000001 || sumDimension(periods, "TIME") < 0.0999 {
		t.Fatalf("tick after the interval must report the 6 minutes and 1 kWh since the start: %+v", second)
	}
}

// With ParkingAtUnplugOnly the parked Session is not updated until the car leaves, and the closing
// update carries all the parking at once (OCPI 2.2.1 §9.3.1 leaves the period cadence to the CPO).
func TestParkingCanBeReportedOnlyAtUnplug(t *testing.T) {
	c, patches, mu := parkingPeer(t)
	c.state.Config.ParkingAtUnplugOnly = true
	id := parkedSession(t, c)
	mu.Lock()
	before := len(*patches)
	mu.Unlock()
	if !c.advanceParking(id, time.Now()) {
		t.Fatal("parking tick must keep the meter loop running")
	}
	mu.Lock()
	if len(*patches) != before {
		t.Fatalf("parking was reported before unplug: %+v", (*patches)[before:])
	}
	mu.Unlock()
	if _, err := c.callSessionAction(t.Context(), "unplug", map[string]any{"session_id": id}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	last := (*patches)[len(*patches)-1]
	mu.Unlock()
	periods, _ := last["charging_periods"].([]any)
	if last["status"] != "COMPLETED" || sumDimension(periods, "PARKING_TIME") < 19.9/60 {
		t.Fatalf("closing PATCH must carry the 20 minutes of parking: %+v", last)
	}
}
