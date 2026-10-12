package ocpi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCredentialHeaderRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		header   string
		allowRaw bool
		want     string
		wantErr  bool
	}{
		{name: "base64", header: EncodeCredential("example-token"), want: "example-token"},
		{name: "raw compatibility", header: "Token example-token", allowRaw: true, want: "example-token"},
		{name: "raw strict", header: "Token example-token", wantErr: true},
		{name: "wrong scheme", header: "Bearer abc", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeCredential(test.header, test.allowRaw)
			if (err != nil) != test.wantErr {
				t.Fatalf("DecodeCredential() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("DecodeCredential() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestValidateMessageHeaders(t *testing.T) {
	t.Parallel()
	headers := http.Header{}
	findings := ValidateMessageHeaders(headers, true)
	if len(findings) != 6 {
		t.Fatalf("got %d findings, want 6", len(findings))
	}
	headers.Set("X-Request-ID", "request")
	headers.Set("X-Correlation-ID", "correlation")
	headers.Set("OCPI-to-party-id", "CPO")
	headers.Set("OCPI-to-country-code", "TH")
	headers.Set("OCPI-from-party-id", "TST")
	headers.Set("OCPI-from-country-code", "TH")
	if got := ValidateMessageHeaders(headers, true); len(got) != 0 {
		t.Fatalf("got findings for valid headers: %+v", got)
	}
	if got := ValidateMessageHeaders(headers, false); len(got) != 4 {
		t.Fatalf("configuration module got %d routing findings, want 4", len(got))
	}
}

func TestValidateRequestHeaders(t *testing.T) {
	t.Parallel()
	headers := http.Header{
		"X-Request-Id": []string{"request"}, "X-Correlation-Id": []string{"correlation"},
		"Content-Type": []string{"application/json; charset=utf-8"},
	}
	if got := ValidateRequestHeaders(http.MethodPost, headers, false); len(got) != 0 {
		t.Fatalf("valid request findings: %+v", got)
	}
	headers.Del("Content-Type")
	if got := ValidateRequestHeaders(http.MethodPost, headers, false); len(got) != 1 {
		t.Fatalf("request without Content-Type got %d findings, want 1", len(got))
	}
}

func TestValidateResponse(t *testing.T) {
	t.Parallel()
	requestHeaders := http.Header{
		"X-Request-Id":           []string{"request"},
		"X-Correlation-Id":       []string{"correlation"},
		"Ocpi-To-Party-Id":       []string{"EVT"},
		"Ocpi-To-Country-Code":   []string{"TH"},
		"Ocpi-From-Party-Id":     []string{"TST"},
		"Ocpi-From-Country-Code": []string{"TH"},
	}
	responseHeaders := http.Header{
		"Content-Type":           []string{"application/json"},
		"X-Request-Id":           []string{"request"},
		"X-Correlation-Id":       []string{"correlation"},
		"Ocpi-To-Party-Id":       []string{"TST"},
		"Ocpi-To-Country-Code":   []string{"TH"},
		"Ocpi-From-Party-Id":     []string{"EVT"},
		"Ocpi-From-Country-Code": []string{"TH"},
	}
	body, _ := json.Marshal(Success(map[string]any{"ok": true}))
	findings, envelope := ValidateResponse(requestHeaders, responseHeaders, true, body, 200)
	if len(findings) != 0 {
		t.Fatalf("valid response findings: %+v", findings)
	}
	if envelope == nil || envelope.StatusCode != 1000 {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}

	bad, envelope := ValidateResponse(requestHeaders, http.Header{}, true, []byte("not-json"), 200)
	if len(bad) < 7 {
		t.Fatalf("malformed response got %d findings, want at least 7", len(bad))
	}
	if envelope != nil {
		t.Fatal("malformed response must not return envelope")
	}
}

func TestHeadersPreserveAuthorization(t *testing.T) {
	t.Parallel()
	headers := http.Header{"Authorization": []string{"Token secret"}, "X-Request-ID": []string{"safe"}}
	result := Headers(headers)
	if result["Authorization"].([]string)[0] != "Token secret" {
		t.Fatalf("Authorization was changed: %#v", result)
	}
}

func TestPagination(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/tokens?date_from=2026-01-01T00:00:00Z&date_to=2026-02-01T00:00:00Z&offset=10&limit=25", nil)
	from, to, offset, limit, findings := ParsePagination(request)
	if len(findings) != 0 || from == nil || to == nil || offset != 10 || limit != 25 {
		t.Fatalf("unexpected pagination: from=%v to=%v offset=%d limit=%d findings=%+v", from, to, offset, limit, findings)
	}
	if from.Format(time.RFC3339) != "2026-01-01T00:00:00Z" {
		t.Fatalf("unexpected date_from: %s", from)
	}

	bad := httptest.NewRequest(http.MethodGet, "/tokens?offset=-1&limit=0&date_from=nope", nil)
	_, _, _, _, findings = ParsePagination(bad)
	if len(findings) != 3 {
		t.Fatalf("got %d invalid pagination findings, want 3", len(findings))
	}
}

func TestOwnerMatchesAndRequired(t *testing.T) {
	t.Parallel()
	object := map[string]any{"country_code": "TH", "party_id": "EVT", "id": "LOC1"}
	if findings := OwnerMatches(object, "TH", "EVT", "LOC1", "id", "rule"); len(findings) != 0 {
		t.Fatalf("valid owner findings: %+v", findings)
	}
	if findings := OwnerMatches(object, "NL", "EVT", "LOC2", "id", "rule"); len(findings) != 2 {
		t.Fatalf("got %d owner findings, want 2", len(findings))
	}
	if findings := Required(object, "rule", "country_code", "missing"); len(findings) != 1 {
		t.Fatalf("got %d required findings, want 1", len(findings))
	}
}
