package cpo

import (
	"context"
	"strings"
	"testing"
)

func TestUnpricedSessionDoesNotClaimFreeCharge(t *testing.T) {
	c := New("https://partner.example")
	delete(c.state.Objects["tariffs"], "BKK-ENERGY")
	result, err := c.callSessionAction(context.Background(), "create", map[string]any{"location_id": "BKK-SILOM", "evse_uid": "SIL-EVSE-1", "connector_id": "1"})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := result["object"].(map[string]any)
	if _, exists := session["total_cost"]; exists {
		t.Fatalf("unpriced Session must omit total_cost: %v", session["total_cost"])
	}
	id := fieldText(session, "id")
	if _, err := c.callSessionAction(context.Background(), "cdr", map[string]any{"session_id": id}); err == nil || !strings.Contains(err.Error(), "complete the Session") {
		t.Fatalf("active Session must not create CDR: %v", err)
	}
	c.state.Objects["sessions"][id].Body["status"] = "COMPLETED"
	if _, err := c.callSessionAction(context.Background(), "cdr", map[string]any{"session_id": id}); err == nil || !strings.Contains(err.Error(), "total_cost is unknown") {
		t.Fatalf("unpriced Session must not create zero-cost CDR: %v", err)
	}
}
