package rules

import "testing"

func TestValidateDateTime(t *testing.T) {
	valid := []string{"2026-09-07T08:00:00Z", "2026-09-07T08:00:00", "2026-09-07T08:00:00.123Z"}
	invalid := []string{"2026-09-07T15:00:00+07:00", "2026-09-07T08:00:00+00:00", "2026-09-07 08:00:00Z"}
	for _, value := range valid {
		if err := ValidateDateTime(value); err != nil {
			t.Errorf("ValidateDateTime(%q) = %v", value, err)
		}
	}
	for _, value := range invalid {
		if err := ValidateDateTime(value); err == nil {
			t.Errorf("ValidateDateTime(%q) unexpectedly passed", value)
		}
	}
}

func TestValidateSessionRequiredFields(t *testing.T) {
	findings := Validate("sessions", map[string]any{
		"country_code": "TH", "party_id": "TST", "id": "S1",
		"start_date_time": "2026-09-07T08:00:00Z", "last_updated": "2026-09-07T08:00:00Z",
	}, Owner{CountryCode: "TH", PartyID: "TST"})
	if len(findings) == 0 {
		t.Fatal("incomplete Session unexpectedly passed")
	}
}
