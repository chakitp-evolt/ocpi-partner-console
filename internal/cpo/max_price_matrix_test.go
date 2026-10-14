package cpo

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

// maxPriceCase is one way a charge can meet max_price under one console setting.
type maxPriceCase struct {
	mode         string
	stop, exact  bool
	idle         bool // max_price is met while parked rather than while charging
	parkAtUnplug bool
}

func (mc maxPriceCase) String() string {
	phase := "charging"
	if mc.idle {
		phase = "idle"
		if mc.parkAtUnplug {
			phase += "/parking-at-unplug"
		}
	}
	return fmt.Sprintf("%s %s stop=%v exact=%v", maxPriceModeOf(mc.mode), phase, mc.stop, mc.exact)
}

// Every mode, stop setting and phase: the CDR keeps max_price, the excl/incl ratio and the lines as the mode
// says, matches the Session it bills, and the Session ends only where the setting ends it.
func TestMaxPriceMatrix(t *testing.T) {
	var cases []maxPriceCase
	for _, mode := range []string{maxPriceFreezeAll, maxPriceTotalOnly, maxPricePerSide} {
		cases = append(cases,
			maxPriceCase{mode: mode},
			maxPriceCase{mode: mode, stop: true},
			maxPriceCase{mode: mode, stop: true, exact: true},
		)
		for _, parkAtUnplug := range []bool{false, true} {
			cases = append(cases,
				maxPriceCase{mode: mode, idle: true, parkAtUnplug: parkAtUnplug},
				maxPriceCase{mode: mode, idle: true, parkAtUnplug: parkAtUnplug, stop: true},
			)
		}
	}
	reportKWh := 0.0
	for _, mc := range cases {
		t.Run(mc.String(), func(t *testing.T) {
			session, cdr, endedByMax := runMaxPriceCase(t, mc)
			maxSide := 0.2
			if mc.idle {
				maxSide = 0.5
			}
			// An exact stop ends the charge where incl_vat met the cap: nothing has run past it yet.
			cutExact := !mc.idle && mc.stop && mc.exact
			total, _ := cdr["total_cost"].(map[string]any)
			excl, incl := number(total["excl_vat"]), number(total["incl_vat"])
			if incl != maxSide {
				t.Errorf("CDR incl_vat = %v, want the %v max_price", incl, maxSide)
			}
			switch mc.mode {
			case maxPricePerSide:
				if cutExact {
					if want := roundMoney(maxSide / 1.07); math.Abs(excl-want) > 0.011 {
						t.Errorf("per side exact excl_vat = %v, want about %v, short of its own cap", excl, want)
					}
				} else if excl != maxSide {
					t.Errorf("per side excl_vat = %v, want its own %v cap", excl, maxSide)
				}
			default:
				if want := roundMoney(maxSide / 1.07); math.Abs(excl-want) > 0.011 {
					t.Errorf("excl_vat = %v, want about %v (7%% kept)", excl, want)
				}
			}
			lineExcl, lineIncl := 0.0, 0.0
			for _, field := range []string{"total_fixed_cost", "total_energy_cost", "total_time_cost", "total_parking_cost"} {
				if line, ok := cdr[field].(map[string]any); ok {
					lineExcl, lineIncl = lineExcl+number(line["excl_vat"]), lineIncl+number(line["incl_vat"])
				}
			}
			lineExcl, lineIncl = roundMoney(lineExcl), roundMoney(lineIncl)
			if mc.mode == maxPriceFreezeAll || cutExact {
				if lineExcl != excl || lineIncl != incl {
					t.Errorf("lines %v / %v, want them to add up to the total %v / %v", lineExcl, lineIncl, excl, incl)
				}
			} else if lineIncl <= incl {
				t.Errorf("lines incl %v, want them to run past the total %v", lineIncl, incl)
			}
			sessionCost, _ := session["total_cost"].(map[string]any)
			if number(sessionCost["excl_vat"]) != excl || number(sessionCost["incl_vat"]) != incl {
				t.Errorf("Session total_cost %v, CDR %v; want the same", sessionCost, total)
			}
			if endedByMax != mc.stop {
				t.Errorf("Session ended by max_price = %v, want %v", endedByMax, mc.stop)
			}
			if !mc.idle && mc.stop {
				kwh := number(cdr["total_energy"])
				if mc.exact {
					if kwh >= reportKWh {
						t.Errorf("exact stop at %v kWh, want before the %v kWh report stop", kwh, reportKWh)
					}
				} else {
					reportKWh = kwh
				}
			}
		})
	}
}

// runMaxPriceCase drives a START, a charge and, for the idle phase, a STOP into parking, until max_price
// is met; without the stop setting it ends the Session the way a driver would. It returns the Session, the
// CDR it produced, and whether max_price ended the Session.
func runMaxPriceCase(t *testing.T, mc maxPriceCase) (map[string]any, map[string]any, bool) {
	t.Helper()
	var peerURL string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		location := ""
		if r.Method == http.MethodPost && r.URL.Path == "/cdrs" {
			location = peerURL + "/cdrs/stored"
		}
		writePeerResponse(w, r, http.StatusOK, nil, location)
	}))
	t.Cleanup(peer.Close)
	peerURL = peer.URL
	c := New(peer.URL)
	c.mu.Lock()
	c.state.TokenC = "test-token"
	c.state.Config.AutoCommandFlow = true
	c.state.Config.TestIdleFee = mc.idle
	c.state.Config.ParkingAtUnplugOnly = mc.parkAtUnplug
	c.state.Config.StopAtMaxPrice = mc.stop
	c.state.Config.StopAtMaxPriceExact = mc.exact
	c.state.Config.MaxPriceMode = mc.mode
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
	c.stopAutoRun(fieldText(start.Result, "session_id"))

	// Charging: 0.03 kWh at 7.50/kWh (0.22/0.24) passes 0.20/0.20. Idle: charging stays under 0.50/0.50 and a
	// minute parked at 600/h passes it.
	c.mu.Lock()
	sessionID := fieldText(start.Result, "session_id")
	session := c.state.Objects["sessions"][sessionID]
	tariff, _ := session.Snapshot["tariff"].(map[string]any)
	tariff["max_price"] = map[string]any{"excl_vat": 0.2, "incl_vat": 0.2}
	if mc.idle {
		tariff["max_price"] = map[string]any{"excl_vat": 0.5, "incl_vat": 0.5}
		elements, _ := tariff["elements"].([]any)
		tariff["elements"] = append(elements, map[string]any{"price_components": []any{map[string]any{"type": "PARKING_TIME", "price": 600.0, "vat": 7.0, "step_size": 1}}})
	}
	checkpoint, _ := time.Parse(time.RFC3339Nano, fieldText(session.Body, "last_updated"))
	c.mu.Unlock()

	stopCommand := func() *CommandRecord {
		c.mu.Lock()
		c.state.Objects["sessions"][sessionID].Body["last_updated"] = time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339Nano)
		stop := &CommandRecord{ID: "stop-1", Type: "STOP_SESSION", Request: map[string]any{"session_id": sessionID}, ResponseURL: peer.URL + "/callback/stop"}
		c.state.Commands[stop.ID] = stop
		c.mu.Unlock()
		c.autoStopCommand(t.Context(), stop)
		c.stopAutoRun(sessionID)
		return stop
	}

	endedByMax := false
	if !mc.idle {
		continued := c.advanceAutoMeter(sessionID, checkpoint.Add(15*time.Second))
		endedByMax = !continued
		if continued {
			stopCommand()
		}
	} else {
		stop := stopCommand()
		if fieldText(stop.Result, "idle_started_at") == "" {
			t.Fatalf("STOP did not start idle: %+v", stop.Result)
		}
		c.mu.Lock()
		stop.Result["idle_started_at"] = ocpi.FormatTime(time.Now().Add(-time.Minute).UTC())
		c.mu.Unlock()
		continued := c.advanceAutoMeter(sessionID, time.Now())
		endedByMax = !continued
		if continued {
			if _, err := c.callSessionAction(t.Context(), "unplug", map[string]any{"session_id": sessionID}); err != nil {
				t.Fatalf("unplug: %v", err)
			}
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.RLock()
		body := cloneMap(c.state.Objects["sessions"][sessionID].Body)
		var cdr map[string]any
		if record := c.state.Objects["cdrs"]["CDR-"+sessionID]; record != nil && record.Delivery == "SENT" {
			cdr = cloneMap(record.Body)
		}
		c.mu.RUnlock()
		if fieldText(body, "status") == "COMPLETED" && cdr != nil {
			return body, cdr, endedByMax
		}
		if time.Now().After(deadline) {
			t.Fatalf("Session %s ended without a sent CDR", fieldText(body, "status"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
