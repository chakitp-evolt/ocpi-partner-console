package requests

import (
	"encoding/json"
	"testing"
)

func TestValidateFixtureAcceptsSpecTokenAndRejectsMalformedJSON(t *testing.T) {
	validToken := `{"country_code":"TH","party_id":"TST","uid":"QA","type":"RFID","contract_id":"TH-TST-QA","issuer":"QA","valid":true,"whitelist":"ALLOWED","last_updated":"2026-09-08T00:00:00Z"}`
	valid, findings := validateFixture("tokens", validToken)
	if !valid || len(findings) != 0 {
		t.Fatalf("valid token rejected: %#v", findings)
	}
	valid, findings = validateFixture("tokens", `{"uid":`)
	if valid || len(findings) == 0 || findings[0].Rule != "JSON" {
		t.Fatalf("malformed JSON result = %v %#v", valid, findings)
	}
}

func TestRequestFixtureEvidencePreservesCredentials(t *testing.T) {
	body := jsonOrText(`{"token":"live-secret","nested":{"authorization":"Token secret"}}`)
	preserved, _ := json.Marshal(body)
	if string(preserved) != `{"nested":{"authorization":"Token secret"},"token":"live-secret"}` {
		t.Fatalf("credential evidence changed: %s", preserved)
	}
}
