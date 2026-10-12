package cpo

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConnectionIdentityUpdatesOnlyUnsentCatalogueDrafts(t *testing.T) {
	c := New("https://partner.example")
	c.state.Objects["locations"]["BKK-ARI"].Delivery = "SENT"
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/cpo/config", strings.NewReader(`{"country_code":"MY","party_id":"ABC"}`))
	c.apiConfig(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("config update = HTTP %d: %s", w.Code, w.Body.String())
	}
	location := c.state.Objects["locations"]["BKK-SILOM"].Body
	tariff := c.state.Objects["tariffs"]["BKK-ENERGY"].Body
	if location["country_code"] != "MY" || location["party_id"] != "ABC" || tariff["country_code"] != "MY" || tariff["party_id"] != "ABC" {
		t.Errorf("unsent catalogue identity was not updated: location=%v tariff=%v", location, tariff)
	}
	evses, _ := location["evses"].([]any)
	evse, _ := evses[0].(map[string]any)
	if evse["evse_id"] != "MY*ABC*ESIL001" {
		t.Errorf("EVSE ID = %v, want MY*ABC*ESIL001", evse["evse_id"])
	}
	if sent := c.state.Objects["locations"]["BKK-ARI"].Body; sent["party_id"] != "TST" {
		t.Errorf("sent Location was changed: %v", sent["party_id"])
	}
}
