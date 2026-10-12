package cpo

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleAutomationOptionsAndConnectionSync(t *testing.T) {
	c := New("https://partner.example")
	if c.state.Config.TestIdleFee || c.state.Config.IdleUnplugAfterSeconds != 0 {
		t.Fatal("idle testing must be opt-in")
	}
	apply := func(body string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		c.apiAutomation(w, httptest.NewRequest(http.MethodPut, "/api/cpo/automation", strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("automation status=%d body=%s", w.Code, w.Body.String())
		}
	}
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":90}`, 200)
	c.SyncConnection(Config{CountryCode: "TH", PartyID: "TST"}, "REGISTERED", "", "", "", "", nil)
	if !c.state.Config.TestIdleFee || c.state.Config.IdleUnplugAfterSeconds != 90 {
		t.Fatal("connection synchronization discarded idle options")
	}
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":-1}`, 422)
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":86401}`, 422)
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":false,"idle_unplug_after_seconds":90}`, 200)
	if c.state.Config.TestIdleFee || c.state.Config.IdleUnplugAfterSeconds != 0 {
		t.Fatal("turning off idle tests must disable automatic unplug")
	}

	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":0,"parking_at_unplug_only":true,"period_interval_seconds":300}`, 200)
	c.SyncConnection(Config{CountryCode: "TH", PartyID: "TST"}, "REGISTERED", "", "", "", "", nil)
	if !c.state.Config.ParkingAtUnplugOnly || c.state.Config.PeriodIntervalSeconds != 300 {
		t.Fatal("parking and period options must survive the save and a connection synchronization")
	}
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":0}`, 200)
	if !c.state.Config.ParkingAtUnplugOnly || c.state.Config.PeriodIntervalSeconds != 300 {
		t.Fatal("a save without the parking and period options must keep them")
	}
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":0,"period_interval_seconds":4}`, 422)
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":0,"period_interval_seconds":1801}`, 422)

	if c.state.Config.sendInterval() != 15*time.Second {
		t.Fatalf("default send interval = %v, want 15s", c.state.Config.sendInterval())
	}
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":0,"send_interval_seconds":5}`, 200)
	c.SyncConnection(Config{CountryCode: "TH", PartyID: "TST"}, "REGISTERED", "", "", "", "", nil)
	if c.state.Config.sendInterval() != 5*time.Second {
		t.Fatal("send interval must survive the save and a connection synchronization")
	}
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":0}`, 200)
	if c.state.Config.SendIntervalSeconds != 5 {
		t.Fatal("a save without send_interval_seconds must keep it")
	}
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":0,"send_interval_seconds":4}`, 422)
	apply(`{"enabled":false,"rate_kwh_per_hour":7.2,"test_idle_fee":true,"idle_unplug_after_seconds":0,"send_interval_seconds":1801}`, 422)
}

func TestIdleUnplugTimerManualOverrideRestoreAndReset(t *testing.T) {
	for _, mode := range []string{"timer", "manual", "restore", "reset"} {
		t.Run(mode, func(t *testing.T) {
			var posts atomic.Int32
			var peerURL string
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				location := ""
				if r.Method == http.MethodPost && r.URL.Path == "/cdrs" {
					posts.Add(1)
					location = peerURL + "/cdrs/stored"
				}
				writePeerResponse(w, r, 200, nil, location)
			}))
			defer peer.Close()
			peerURL = peer.URL
			c := New(peer.URL)
			defer c.stopAllIdleRuns()
			c.state.TokenC = "test-token"
			c.state.Config.TestIdleFee = true
			c.state.Config.IdleUnplugAfterSeconds = 3600
			if mode == "timer" {
				c.state.Config.IdleUnplugAfterSeconds = 1
			}
			c.state.Endpoints = []Endpoint{{Identifier: "sessions", Role: "RECEIVER", URL: peer.URL + "/sessions"}, {Identifier: "locations", Role: "RECEIVER", URL: peer.URL + "/locations"}, {Identifier: "cdrs", Role: "RECEIVER", URL: peer.URL + "/cdrs"}}
			created, err := c.callSessionAction(t.Context(), "create", map[string]any{"location_id": "BKK-ARI", "evse_uid": "ARI-EVSE-1", "connector_id": "1"})
			if err != nil {
				t.Fatal(err)
			}
			id := fieldText(created["object"].(map[string]any), "id")
			if err := c.callSend(t.Context(), "sessions", id, http.MethodPut); err != nil {
				t.Fatal(err)
			}
			c.state.Commands["stop"] = &CommandRecord{ID: "stop", Type: "STOP_SESSION", Request: map[string]any{"session_id": id}, Result: map[string]any{}}
			if err := c.beginIdle(t.Context(), id, nil); err != nil {
				t.Fatal(err)
			}
			c.autoMu.Lock()
			run := c.idleRuns[id]
			c.autoMu.Unlock()
			if run == nil {
				t.Fatal("idle timer was not started")
			}
			if mode == "manual" {
				backdateSession(c, id, "stop", 2*time.Minute)
				if _, err := c.callSessionAction(t.Context(), "unplug", map[string]any{"session_id": id}); err != nil {
					t.Fatal(err)
				}
			} else if mode == "restore" {
				c.stopAllIdleRuns()
				c.mu.Lock()
				c.state.Commands["stop"].Result["auto_unplug_at"] = time.Now().Add(100 * time.Millisecond).UTC().Format(time.RFC3339Nano)
				c.mu.Unlock()
				c.resumeIdleRuns()
				c.autoMu.Lock()
				run = c.idleRuns[id]
				c.autoMu.Unlock()
				if run == nil {
					t.Fatal("saved idle timer was not resumed")
				}
			} else if mode == "reset" {
				if err := c.ResetData(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-run.done:
			case <-time.After(5 * time.Second):
				t.Fatal("idle timer did not finish or cancel")
			}
			if mode == "reset" {
				if posts.Load() != 0 || c.state.Objects["sessions"][id] != nil {
					t.Fatal("reset allowed a delayed CDR to escape")
				}
				return
			}
			c.mu.RLock()
			cdr := c.state.Objects["cdrs"]["CDR-"+id]
			status := fieldText(c.state.Objects["sessions"][id].Body, "status")
			c.mu.RUnlock()
			if posts.Load() != 1 || status != "COMPLETED" || cdr == nil || cdr.Delivery != "SENT" {
				t.Fatalf("unplug did not send exactly one final CDR: posts=%d status=%s cdr=%+v", posts.Load(), status, cdr)
			}
			if number(cdr.Body["total_parking_time"]) <= 0 || number(cdr.Body["total_parking_cost"].(map[string]any)["excl_vat"]) != 7.5 {
				t.Fatalf("scheduled/manual idle did not apply Tariff parking step: %+v", cdr.Body)
			}
		})
	}
}
