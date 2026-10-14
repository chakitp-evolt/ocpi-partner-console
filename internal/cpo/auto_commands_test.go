package cpo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

func TestAutoCommandFlowSendsSessionMeterAndCDR(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		testIdleFee, noParking bool
	}{
		{"default immediate completion", false, false},
		{"idle waits for unplug", true, false},
		{"idle without a parking price completes immediately", true, true},
	} {
		testIdleFee := tc.testIdleFee
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			var evseStatuses []string
			var peerURL string
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/locations/") {
					var patch map[string]any
					_ = json.NewDecoder(r.Body).Decode(&patch)
					evseStatuses = append(evseStatuses, fieldText(patch, "status"))
				}
				mu.Unlock()
				location := ""
				if r.Method == http.MethodPost && r.URL.Path == "/cdrs" {
					location = peerURL + "/cdrs/stored"
				}
				writePeerResponse(w, r, http.StatusOK, nil, location)
			}))
			defer peer.Close()
			peerURL = peer.URL
			c := New(peer.URL)
			c.mu.Lock()
			c.state.TokenC = "test-token"
			c.state.Config.AutoCommandFlow = true
			c.state.Config.TestIdleFee = testIdleFee
			c.state.Config.AutoMeterRateKWh = 7.2
			c.state.Endpoints = []Endpoint{
				{Identifier: "sessions", Role: "RECEIVER", URL: peer.URL + "/sessions"},
				{Identifier: "cdrs", Role: "RECEIVER", URL: peer.URL + "/cdrs"},
				{Identifier: "locations", Role: "RECEIVER", URL: peer.URL + "/locations"},
			}
			start := &CommandRecord{ID: "start-1", Type: "START_SESSION", Request: map[string]any{
				"response_url": peer.URL + "/callback/start", "location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1",
				"token": map[string]any{"country_code": "TH", "party_id": "EVT", "uid": "TEST-1", "type": "RFID", "contract_id": "TH-EVT-C1"},
			}, ResponseURL: peer.URL + "/callback/start"}
			c.state.Commands[start.ID] = start
			c.mu.Unlock()

			c.autoStartCommand(t.Context(), start)
			c.mu.RLock()
			sessionID := fieldText(c.state.Commands[start.ID].Result, "session_id")
			session := c.state.Objects["sessions"][sessionID]
			c.mu.RUnlock()
			if sessionID == "" || session == nil || session.Delivery != "SENT" {
				t.Fatalf("auto START did not send Session: id=%q session=%+v result=%+v", sessionID, session, start.Result)
			}
			if tc.testIdleFee && !tc.noParking {
				// BKK-ENERGY prices energy only; idle needs a parking price to wait for the unplug.
				c.mu.Lock()
				tariff, _ := session.Snapshot["tariff"].(map[string]any)
				if tariff == nil {
					c.mu.Unlock()
					t.Fatal("session started without a Tariff snapshot")
				}
				elements, _ := tariff["elements"].([]any)
				tariff["elements"] = append(elements, map[string]any{"price_components": []any{map[string]any{"type": "PARKING_TIME", "price": 30.0, "vat": 7.0, "step_size": 60}}})
				c.mu.Unlock()
			}
			if session.Body["evse_uid"] != "SIL-EVSE-1" || session.Body["connector_id"] != "1" {
				t.Errorf("auto START did not choose the available connector: evse=%v connector=%v", session.Body["evse_uid"], session.Body["connector_id"])
			}
			if got := testEVSEStatus(c.state.Objects["locations"]["BKK-SILOM"].Body, "SIL-EVSE-1"); got != "CHARGING" {
				t.Errorf("auto START EVSE status = %s, want CHARGING", got)
			}
			checkpoint, _ := time.Parse(time.RFC3339Nano, fieldText(session.Body, "last_updated"))
			if !c.advanceAutoMeter(sessionID, checkpoint.Add(15*time.Second)) {
				t.Fatalf("auto meter did not send PATCH: %+v", start.Result)
			}
			c.mu.RLock()
			kwh := number(c.state.Objects["sessions"][sessionID].Body["kwh"])
			c.mu.RUnlock()
			if kwh != 0.03 {
				t.Errorf("meter = %g kWh, want 0.03", kwh)
			}
			stop := &CommandRecord{ID: "stop-1", Type: "STOP_SESSION", Request: map[string]any{"session_id": sessionID}, ResponseURL: peer.URL + "/callback/stop"}
			c.mu.Lock()
			c.state.Objects["sessions"][sessionID].Body["last_updated"] = time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339Nano)
			c.state.Commands[stop.ID] = stop
			c.mu.Unlock()
			c.autoStopCommand(t.Context(), stop)
			c.mu.RLock()
			status := fieldText(c.state.Objects["sessions"][sessionID].Body, "status")
			finalKWh := number(c.state.Objects["sessions"][sessionID].Body["kwh"])
			cdr := c.state.Objects["cdrs"]["CDR-"+sessionID]
			c.mu.RUnlock()
			if finalKWh <= kwh {
				t.Errorf("STOP did not include live meter after last PATCH: initial=%g final=%g", kwh, finalKWh)
			}
			if testIdleFee && !tc.noParking {
				if status != "ACTIVE" || cdr != nil || fieldText(stop.Result, "idle_started_at") == "" {
					t.Fatalf("idle STOP must wait for unplug: status=%s cdr=%+v result=%+v", status, cdr, stop.Result)
				}
				if got := testEVSEStatus(c.state.Objects["locations"]["BKK-SILOM"].Body, "SIL-EVSE-1"); got != "CHARGING" {
					t.Errorf("idle STOP EVSE status = %s, want CHARGING until unplug", got)
				}
				if _, err := c.callSessionAction(t.Context(), "unplug", map[string]any{"session_id": sessionID}); err != nil {
					t.Fatalf("unplug failed: %v", err)
				}
			} else {
				if status != "COMPLETED" || cdr == nil || cdr.Delivery != "SENT" || fieldText(stop.Result, "idle_started_at") != "" {
					t.Fatalf("default STOP must complete immediately: status=%s cdr=%+v result=%+v", status, cdr, stop.Result)
				}
				periods, _ := cdr.Body["charging_periods"].([]any)
				if sumDimension(periods, "PARKING_TIME") != 0 || cdr.Body["total_parking_cost"] != nil {
					t.Error("default flow unexpectedly added idle fees")
				}
			}
			c.mu.RLock()
			status = fieldText(c.state.Objects["sessions"][sessionID].Body, "status")
			cdr = c.state.Objects["cdrs"]["CDR-"+sessionID]
			c.mu.RUnlock()
			if status != "COMPLETED" || cdr == nil || cdr.Delivery != "SENT" {
				t.Errorf("unplug did not complete Session and send CDR: status=%s cdr=%+v result=%+v", status, cdr, stop.Result)
			}
			if got := testEVSEStatus(c.state.Objects["locations"]["BKK-SILOM"].Body, "SIL-EVSE-1"); got != "AVAILABLE" {
				t.Errorf("unplug EVSE status = %s, want AVAILABLE", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(evseStatuses) != 2 || evseStatuses[0] != "CHARGING" || evseStatuses[1] != "AVAILABLE" {
				t.Errorf("EVSE PATCH statuses = %v, want [CHARGING AVAILABLE]", evseStatuses)
			}
			for _, want := range []string{"POST /callback/start", "PUT /sessions/", "PATCH /locations/", "PATCH /sessions/", "POST /callback/stop", "POST /cdrs"} {
				found := false
				for _, got := range requests {
					if strings.HasPrefix(got, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing %s in %v", want, requests)
				}
			}
		})
	}
}

func testEVSEStatus(location map[string]any, uid string) string {
	evses, _ := location["evses"].([]any)
	for _, raw := range evses {
		evse, _ := raw.(map[string]any)
		if fieldText(evse, "uid") == uid {
			return fieldText(evse, "status")
		}
	}
	return ""
}

func TestAutoCommandFlowContinuesWhenEVSEStatusPatchFails(t *testing.T) {
	var peerURL string
	var cdrSent atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/locations/") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ocpi.Error(2001, "EVSE status rejected"))
			return
		}
		location := ""
		if r.Method == http.MethodPost && r.URL.Path == "/cdrs" {
			cdrSent.Store(true)
			location = peerURL + "/cdrs/stored"
		}
		writePeerResponse(w, r, http.StatusOK, nil, location)
	}))
	defer peer.Close()
	peerURL = peer.URL
	c := New(peer.URL)
	c.mu.Lock()
	c.state.TokenC = "test-token"
	c.state.Config.AutoCommandFlow = true
	c.state.Config.AutoMeterRateKWh = 7.2
	c.state.Endpoints = []Endpoint{
		{Identifier: "sessions", Role: "RECEIVER", URL: peer.URL + "/sessions"},
		{Identifier: "cdrs", Role: "RECEIVER", URL: peer.URL + "/cdrs"},
		{Identifier: "locations", Role: "RECEIVER", URL: peer.URL + "/locations"},
	}
	start := &CommandRecord{ID: "start-failed-status", Type: "START_SESSION", Request: map[string]any{
		"response_url": peer.URL + "/callback/start", "location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1",
		"token": map[string]any{"country_code": "TH", "party_id": "EVT", "uid": "TEST-1", "type": "RFID", "contract_id": "TH-EVT-C1"},
	}, ResponseURL: peer.URL + "/callback/start"}
	c.state.Commands[start.ID] = start
	c.mu.Unlock()

	c.autoStartCommand(t.Context(), start)
	c.mu.RLock()
	sessionID := fieldText(c.state.Commands[start.ID].Result, "session_id")
	startError := fieldText(c.state.Commands[start.ID].Result, "evse_status_error")
	session := c.state.Objects["sessions"][sessionID]
	c.mu.RUnlock()
	if sessionID == "" || session == nil || session.Delivery != "SENT" || startError == "" {
		t.Fatalf("auto START must keep Session running and report status error: session=%+v error=%q", session, startError)
	}
	stop := &CommandRecord{ID: "stop-failed-status", Type: "STOP_SESSION", Request: map[string]any{"session_id": sessionID}, ResponseURL: peer.URL + "/callback/stop"}
	c.mu.Lock()
	c.state.Objects["sessions"][sessionID].Body["last_updated"] = time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339Nano)
	c.state.Commands[stop.ID] = stop
	c.mu.Unlock()
	c.autoStopCommand(t.Context(), stop)
	c.mu.RLock()
	stopError := fieldText(c.state.Commands[stop.ID].Result, "evse_status_error")
	cdrID := fieldText(c.state.Commands[stop.ID].Result, "cdr_id")
	cdr := c.state.Objects["cdrs"][cdrID]
	c.mu.RUnlock()
	if stopError == "" || !cdrSent.Load() || cdr == nil || cdr.Delivery != "SENT" {
		t.Fatalf("auto STOP must send CDR and report status error: cdr=%+v sent=%v error=%q", cdr, cdrSent.Load(), stopError)
	}
}

func TestAutoStopRejectsUnknownAndDuplicateSession(t *testing.T) {
	c := New("https://partner.example")
	c.mu.Lock()
	c.state.Config.AutoCommandFlow = true
	c.state.Objects["sessions"]["active-1"] = &Record{Body: map[string]any{"status": "ACTIVE"}}
	c.state.Commands["stop-first"] = &CommandRecord{Type: "STOP_SESSION", Request: map[string]any{"session_id": "active-1"}}
	c.mu.Unlock()
	r := httptest.NewRequest(http.MethodPost, "/ocpi/cpo/2.2.1/commands/STOP_SESSION", nil)
	r.SetPathValue("command", "STOP_SESSION")
	for _, item := range []struct {
		id   string
		want string
	}{{"missing", "UNKNOWN_SESSION"}, {"active-1", "REJECTED"}} {
		response := c.commandsReceiver(r, map[string]any{"response_url": "https://emsp.example/callback", "session_id": item.id})
		data, _ := response.Envelope.Data.(map[string]any)
		if data["result"] != item.want {
			t.Errorf("STOP_SESSION %s = %v, want %s", item.id, data["result"], item.want)
		}
	}
}

func TestAutoStartDoesNotGuessAmbiguousConnector(t *testing.T) {
	c := New("https://partner.example")
	command := &CommandRecord{Type: "START_SESSION", Request: map[string]any{"location_id": "BKK-SILOM"}}
	if _, err := c.autoStartTarget(command); err == nil || !strings.Contains(err.Error(), "2 available EVSEs") {
		t.Fatalf("missing EVSE should be ambiguous: %v", err)
	}
	command.Request["evse_uid"] = "SIL-EVSE-2"
	if _, err := c.autoStartTarget(command); err == nil || !strings.Contains(err.Error(), "requires connector_id") {
		t.Fatalf("connector-required EVSE should reject missing connector: %v", err)
	}
	command.Request["connector_id"] = "2"
	if target, err := c.autoStartTarget(command); err != nil || target["connector_id"] != "2" {
		t.Fatalf("explicit connector should resolve: target=%v err=%v", target, err)
	}
	findings := validateCommand("START_SESSION", map[string]any{"response_url": "https://emsp.example/callback", "token": map[string]any{"uid": "1"}, "location_id": "BKK-SILOM", "connector_id": "2"})
	if len(findings) != 1 || !strings.Contains(findings[0].Message, "evse_uid") {
		t.Fatalf("connector_id without evse_uid should be invalid: %v", findings)
	}
}

func TestAutoMeterStopsAtMaxPrice(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		stopAtMaxPrice, exact bool
		mode                  string
	}{
		{"report stop completes on the tick that saw max_price", true, false, ""},
		{"exact stop completes where the total met max_price", true, true, ""},
		{"without the switch the charge continues at max_price", false, false, ""},
		{"total only the charge continues at 0.19/0.20", false, false, maxPriceTotalOnly},
		{"per side the charge continues at 0.20/0.20", false, false, maxPricePerSide},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var peerURL string
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				location := ""
				if r.Method == http.MethodPost && r.URL.Path == "/cdrs" {
					location = peerURL + "/cdrs/stored"
				}
				writePeerResponse(w, r, http.StatusOK, nil, location)
			}))
			defer peer.Close()
			peerURL = peer.URL
			c := New(peer.URL)
			c.mu.Lock()
			c.state.TokenC = "test-token"
			c.state.Config.AutoCommandFlow = true
			c.state.Config.TestIdleFee = true
			c.state.Config.StopAtMaxPrice = tc.stopAtMaxPrice
			c.state.Config.StopAtMaxPriceExact = tc.exact
			c.state.Config.MaxPriceMode = tc.mode
			c.state.Config.AutoMeterRateKWh = 7.2
			c.state.Endpoints = []Endpoint{
				{Identifier: "sessions", Role: "RECEIVER", URL: peer.URL + "/sessions"},
				{Identifier: "cdrs", Role: "RECEIVER", URL: peer.URL + "/cdrs"},
				{Identifier: "locations", Role: "RECEIVER", URL: peer.URL + "/locations"},
			}
			start := &CommandRecord{ID: "start-1", Type: "START_SESSION", Request: map[string]any{
				"response_url": peer.URL + "/callback/start", "location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1",
				"token": map[string]any{"country_code": "TH", "party_id": "EVT", "uid": "TEST-1", "type": "RFID", "contract_id": "TH-EVT-C1"},
			}, ResponseURL: peer.URL + "/callback/start"}
			c.state.Commands[start.ID] = start
			c.mu.Unlock()

			c.autoStartCommand(t.Context(), start)
			c.mu.Lock()
			sessionID := fieldText(start.Result, "session_id")
			session := c.state.Objects["sessions"][sessionID]
			if session == nil {
				c.mu.Unlock()
				t.Fatalf("auto START did not create a Session: %+v", start.Result)
			}
			// The 15 s tick reaches 0.03 kWh at 7.50/kWh (0.22/0.24), past max_price 0.20/0.20, so the price
			// freezes where incl_vat met 0.20. The parking price proves the stop skips idle with idle testing on.
			tariff, _ := session.Snapshot["tariff"].(map[string]any)
			tariff["max_price"] = map[string]any{"excl_vat": 0.2, "incl_vat": 0.2}
			elements, _ := tariff["elements"].([]any)
			tariff["elements"] = append(elements, map[string]any{"price_components": []any{map[string]any{"type": "PARKING_TIME", "price": 30.0, "vat": 7.0, "step_size": 60}}})
			checkpoint, _ := time.Parse(time.RFC3339Nano, fieldText(session.Body, "last_updated"))
			c.mu.Unlock()

			continued := c.advanceAutoMeter(sessionID, checkpoint.Add(15*time.Second))
			c.mu.RLock()
			defer c.mu.RUnlock()
			body := c.state.Objects["sessions"][sessionID].Body
			cdr := c.state.Objects["cdrs"]["CDR-"+sessionID]
			cost, _ := body["total_cost"].(map[string]any)
			// Frozen where incl_vat met 0.20, excl_vat stays at 0.19 whichever way the charge goes on; per
			// side, excl_vat runs on to its own 0.20.
			wantExcl := 0.19
			if tc.mode == maxPricePerSide {
				wantExcl = 0.2
			}
			if number(cost["excl_vat"]) != wantExcl || number(cost["incl_vat"]) != 0.2 {
				t.Errorf("total_cost = %v, want %.2f/0.20", cost, wantExcl)
			}
			if !tc.stopAtMaxPrice {
				if !continued || fieldText(body, "status") != "ACTIVE" || cdr != nil {
					t.Errorf("charge must continue: continued=%v status=%s cdr=%v", continued, fieldText(body, "status"), cdr != nil)
				}
				return
			}
			if continued {
				t.Error("meter loop must end once the Session completed at max_price")
			}
			if fieldText(body, "status") != "COMPLETED" {
				t.Errorf("Session status = %s, want COMPLETED", fieldText(body, "status"))
			}
			// Exact: incl_vat meets 0.20 once the 1 Wh step reaches 25 Wh (0.025 x 8.025 = 0.2006), past
			// 0.024 kWh at 12 s. Report: the stop lands on the 15 s tick at 0.03 kWh. The lines stay frozen at the total.
			wantMinKWh, wantMaxKWh, wantEnd := 0.03, 0.03, 15*time.Second
			if tc.exact {
				wantMinKWh, wantMaxKWh, wantEnd = 0.024001, 0.0241, 12*time.Second
			}
			wantEnergy := map[string]float64{"excl_vat": 0.19, "incl_vat": 0.2}
			if kwh := number(body["kwh"]); kwh < wantMinKWh || kwh > wantMaxKWh {
				t.Errorf("meter stopped at %g kWh, want %g..%g", kwh, wantMinKWh, wantMaxKWh)
			}
			end, _ := parseOCPITime(fieldText(body, "end_date_time"))
			if got := end.Sub(checkpoint); got < wantEnd || got > wantEnd+5*time.Millisecond {
				t.Errorf("Session ended %s after the last report, want %s", got, wantEnd)
			}
			if cdr == nil || cdr.Delivery != "SENT" {
				t.Fatalf("CDR not sent: %+v", cdr)
			}
			energy, _ := cdr.Body["total_energy_cost"].(map[string]any)
			if number(energy["excl_vat"]) != wantEnergy["excl_vat"] || number(energy["incl_vat"]) != wantEnergy["incl_vat"] {
				t.Errorf("total_energy_cost = %v, want %v", energy, wantEnergy)
			}
			periods, _ := cdr.Body["charging_periods"].([]any)
			if sumDimension(periods, "PARKING_TIME") != 0 || cdr.Body["total_parking_cost"] != nil {
				t.Error("stop at max_price must not add idle time")
			}
			if got := testEVSEStatus(c.state.Objects["locations"]["BKK-SILOM"].Body, "SIL-EVSE-1"); got != "AVAILABLE" {
				t.Errorf("EVSE status = %s, want AVAILABLE", got)
			}
			if start.Result["automation_error"] != nil {
				t.Errorf("stop at max_price recorded an automation error: %v", start.Result["automation_error"])
			}
		})
	}
}

func TestMeterAdvanceStopsAtMaxPrice(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		exact                bool
		wantMinKWh, wantKWhs float64
	}{
		{"report stop keeps the entered reading", false, 0.03, 0.03},
		{"exact stop cuts the reading where the total met max_price", true, 0.024001, 0.0241},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var peerURL string
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				location := ""
				if r.Method == http.MethodPost && r.URL.Path == "/cdrs" {
					location = peerURL + "/cdrs/stored"
				}
				writePeerResponse(w, r, http.StatusOK, nil, location)
			}))
			defer peer.Close()
			peerURL = peer.URL
			c := New(peer.URL)
			c.mu.Lock()
			c.state.TokenC = "test-token"
			c.state.Config.AutoCommandFlow = true
			c.state.Config.AutoMeterRateKWh = 7.2
			c.state.Endpoints = []Endpoint{
				{Identifier: "sessions", Role: "RECEIVER", URL: peer.URL + "/sessions"},
				{Identifier: "cdrs", Role: "RECEIVER", URL: peer.URL + "/cdrs"},
				{Identifier: "locations", Role: "RECEIVER", URL: peer.URL + "/locations"},
			}
			start := &CommandRecord{ID: "start-1", Type: "START_SESSION", Request: map[string]any{
				"response_url": peer.URL + "/callback/start", "location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1",
				"token": map[string]any{"country_code": "TH", "party_id": "EVT", "uid": "TEST-1", "type": "RFID", "contract_id": "TH-EVT-C1"},
			}, ResponseURL: peer.URL + "/callback/start"}
			c.state.Commands[start.ID] = start
			c.mu.Unlock()
			c.autoStartCommand(t.Context(), start)
			// The page's meter drives this charge: the server loop is off and the switch is set afterwards.
			c.stopAllAutoRuns()
			c.mu.Lock()
			c.state.Config.AutoCommandFlow = false
			c.state.Config.StopAtMaxPrice = true
			c.state.Config.StopAtMaxPriceExact = tc.exact
			sessionID := fieldText(start.Result, "session_id")
			tariff, _ := c.state.Objects["sessions"][sessionID].Snapshot["tariff"].(map[string]any)
			tariff["max_price"] = map[string]any{"excl_vat": 0.2, "incl_vat": 0.2}
			c.mu.Unlock()

			result, err := c.callSessionAction(t.Context(), "advance", map[string]any{"session_id": sessionID, "target_kwh": 0.03})
			if err != nil {
				t.Fatal(err)
			}
			if result["stopped_at_max_price"] != true {
				t.Errorf("advance result = %v, want stopped_at_max_price", result)
			}
			c.mu.RLock()
			defer c.mu.RUnlock()
			body := c.state.Objects["sessions"][sessionID].Body
			cdr := c.state.Objects["cdrs"]["CDR-"+sessionID]
			if kwh := number(body["kwh"]); fieldText(body, "status") != "COMPLETED" || kwh < tc.wantMinKWh || kwh > tc.wantKWhs {
				t.Errorf("Session = %s at %g kWh, want COMPLETED at %g..%g", fieldText(body, "status"), kwh, tc.wantMinKWh, tc.wantKWhs)
			}
			if cdr == nil || cdr.Delivery != "SENT" {
				t.Errorf("CDR not sent: %+v", cdr)
			}
		})
	}
}

func TestIdleEndsAtMaxPrice(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parkAtUnplug  bool
		stopAtMaxOn   bool
		wantCompleted bool
	}{
		{"parking reported each tick ends at max_price", false, true, true},
		{"parking held until unplug ends at max_price", true, true, true},
		{"without the switch the parked Session waits for the unplug", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var peerURL string
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				location := ""
				if r.Method == http.MethodPost && r.URL.Path == "/cdrs" {
					location = peerURL + "/cdrs/stored"
				}
				writePeerResponse(w, r, http.StatusOK, nil, location)
			}))
			defer peer.Close()
			peerURL = peer.URL
			c := New(peer.URL)
			c.mu.Lock()
			c.state.TokenC = "test-token"
			c.state.Config.AutoCommandFlow = true
			c.state.Config.TestIdleFee = true
			c.state.Config.StopAtMaxPrice = tc.stopAtMaxOn
			c.state.Config.ParkingAtUnplugOnly = tc.parkAtUnplug
			c.state.Config.AutoMeterRateKWh = 7.2
			c.state.Endpoints = []Endpoint{
				{Identifier: "sessions", Role: "RECEIVER", URL: peer.URL + "/sessions"},
				{Identifier: "cdrs", Role: "RECEIVER", URL: peer.URL + "/cdrs"},
				{Identifier: "locations", Role: "RECEIVER", URL: peer.URL + "/locations"},
			}
			start := &CommandRecord{ID: "start-1", Type: "START_SESSION", Request: map[string]any{
				"response_url": peer.URL + "/callback/start", "location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1",
				"token": map[string]any{"country_code": "TH", "party_id": "EVT", "uid": "TEST-1", "type": "RFID", "contract_id": "TH-EVT-C1"},
			}, ResponseURL: peer.URL + "/callback/start"}
			c.state.Commands[start.ID] = start
			c.mu.Unlock()
			c.autoStartCommand(t.Context(), start)

			// Charging stays under max_price 0.50/0.50; a minute parked at 600/h passes it.
			c.mu.Lock()
			sessionID := fieldText(start.Result, "session_id")
			session := c.state.Objects["sessions"][sessionID]
			tariff, _ := session.Snapshot["tariff"].(map[string]any)
			tariff["max_price"] = map[string]any{"excl_vat": 0.5, "incl_vat": 0.5}
			elements, _ := tariff["elements"].([]any)
			tariff["elements"] = append(elements, map[string]any{"price_components": []any{map[string]any{"type": "PARKING_TIME", "price": 600.0, "vat": 7.0, "step_size": 1}}})
			session.Body["last_updated"] = time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339Nano)
			stop := &CommandRecord{ID: "stop-1", Type: "STOP_SESSION", Request: map[string]any{"session_id": sessionID}, ResponseURL: peer.URL + "/callback/stop"}
			c.state.Commands[stop.ID] = stop
			c.mu.Unlock()
			c.autoStopCommand(t.Context(), stop)
			c.stopAutoRun(sessionID)
			if fieldText(stop.Result, "idle_started_at") == "" {
				t.Fatalf("STOP did not start idle: %+v", stop.Result)
			}

			// A minute parked: the idle started a minute ago, as the unplug prices the parking up to now.
			c.mu.Lock()
			stop.Result["idle_started_at"] = ocpi.FormatTime(time.Now().Add(-time.Minute).UTC())
			c.mu.Unlock()
			continued := c.advanceAutoMeter(sessionID, time.Now())
			if continued == tc.wantCompleted {
				t.Errorf("advanceAutoMeter = %v, want %v", continued, !tc.wantCompleted)
			}
			deadline := time.Now().Add(2 * time.Second)
			var status string
			var cdr *Record
			for {
				c.mu.RLock()
				status = fieldText(c.state.Objects["sessions"][sessionID].Body, "status")
				cdr = c.state.Objects["cdrs"]["CDR-"+sessionID]
				c.mu.RUnlock()
				if !tc.wantCompleted || status == "COMPLETED" && cdr != nil && cdr.Delivery == "SENT" || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !tc.wantCompleted {
				if status != "ACTIVE" || cdr != nil {
					t.Fatalf("Session = %s, CDR %v; want ACTIVE until the unplug", status, cdr != nil)
				}
				return
			}
			if status != "COMPLETED" || cdr == nil || cdr.Delivery != "SENT" {
				t.Fatalf("Session = %s, CDR %+v; want COMPLETED with the CDR sent", status, cdr)
			}
			cost, _ := cdr.Body["total_cost"].(map[string]any)
			if number(cost["incl_vat"]) != 0.5 {
				t.Errorf("CDR total_cost = %v, want incl_vat at the 0.50 max_price", cost)
			}
			if stop.Result["automation_error"] != nil {
				t.Errorf("automation error: %v", stop.Result["automation_error"])
			}
		})
	}
}
