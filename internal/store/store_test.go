package store

import "testing"

func TestCPOSecretRowHidesEverythingUntilItIsRevealed(t *testing.T) {
	hidden := cpoLabSecretRow("partner_issued_token", "2026-09-18T10:00:00Z", "", false)
	for _, field := range []string{"value", "fingerprint", "authorization"} {
		if _, leaked := hidden[field]; leaked {
			t.Fatalf("a row that was not revealed carries %q", field)
		}
	}
	if hidden["encrypted"] != true || hidden["key"] != "partner_issued_token" {
		t.Fatalf("a hidden row must still name its key: %#v", hidden)
	}

	shown := cpoLabSecretRow("partner_issued_token", "2026-09-18T10:00:00Z", "token-after", true)
	if shown["value"] != "token-after" {
		t.Fatalf("revealed row value = %v", shown["value"])
	}
	if shown["fingerprint"] != secretFingerprint("token-after") {
		t.Fatalf("revealed row fingerprint = %v", shown["fingerprint"])
	}
	if shown["authorization"] != "Token dG9rZW4tYWZ0ZXI=" {
		t.Fatalf("revealed row must carry the OCPI header value, got %v", shown["authorization"])
	}
}

func TestSecretFingerprintIsStableAndChangesWithTheSecret(t *testing.T) {
	first := secretFingerprint("token-before")
	if first != secretFingerprint("token-before") {
		t.Fatal("the same secret must keep the same fingerprint")
	}
	if first == secretFingerprint("token-after") {
		t.Fatal("a rotated secret must have a different fingerprint")
	}
}

func TestMergePatch(t *testing.T) {
	t.Parallel()
	current := map[string]any{"status": "ACTIVE", "total_cost": map[string]any{"excl_vat": 2.0, "incl_vat": 2.14}, "unchanged": true}
	patch := map[string]any{"status": "COMPLETED", "total_cost": map[string]any{"excl_vat": 3.0}}
	result := Merge(current, patch, false)
	if result["status"] != "COMPLETED" || result["unchanged"] != true {
		t.Fatalf("unexpected merge result: %#v", result)
	}
	cost := result["total_cost"].(map[string]any)
	if cost["excl_vat"] != 3.0 || cost["incl_vat"] != 2.14 {
		t.Fatalf("nested merge failed: %#v", cost)
	}
}

func TestMergeSessionChargingPeriodsAppends(t *testing.T) {
	t.Parallel()
	current := map[string]any{"charging_periods": []any{map[string]any{"start_date_time": "one"}}}
	patch := map[string]any{"charging_periods": []any{map[string]any{"start_date_time": "two"}}}
	result := Merge(current, patch, true)
	periods := result["charging_periods"].([]any)
	if len(periods) != 2 {
		t.Fatalf("got %d periods, want 2", len(periods))
	}
	if periods[0].(map[string]any)["start_date_time"] != "one" || periods[1].(map[string]any)["start_date_time"] != "two" {
		t.Fatalf("wrong append order: %#v", periods)
	}
}
