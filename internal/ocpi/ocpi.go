package ocpi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	Version = "2.2.1"
	MaxBody = 1 << 20
)

type Envelope struct {
	Data          any    `json:"data,omitempty"`
	StatusCode    int    `json:"status_code"`
	StatusMessage string `json:"status_message,omitempty"`
	Timestamp     string `json:"timestamp"`
}

type Finding struct {
	Level   string `json:"level"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// DateTimeLayout is the OCPI 2.2.1 §16.2 DateTime: UTC, string(25), so at most millisecond precision.
const DateTimeLayout = "2006-01-02T15:04:05.000Z"

// FormatTime renders t as an OCPI DateTime.
func FormatTime(t time.Time) string { return t.UTC().Format(DateTimeLayout) }

func Success(data any) Envelope {
	return Envelope{Data: data, StatusCode: 1000, StatusMessage: "Success", Timestamp: FormatTime(time.Now())}
}

func Error(code int, message string) Envelope {
	return Envelope{StatusCode: code, StatusMessage: message, Timestamp: FormatTime(time.Now())}
}

func EncodeCredential(token string) string {
	return "Token " + base64.StdEncoding.EncodeToString([]byte(token))
}

func DecodeCredential(header string, allowRaw bool) (string, error) {
	if !strings.HasPrefix(header, "Token ") {
		return "", fmt.Errorf("Authorization must use Token scheme")
	}
	value := strings.TrimSpace(strings.TrimPrefix(header, "Token "))
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return string(decoded), nil
	}
	if allowRaw {
		return value, nil
	}
	return "", fmt.Errorf("credentials token is not RFC 4648 Base64")
}

func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func NewToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func MessageHeaders() (string, string) { return NewID(), NewID() }

func ValidateMessageHeaders(headers http.Header, functional bool) []Finding {
	findings := []Finding{}
	if headers.Get("X-Request-ID") == "" {
		findings = append(findings, Finding{Level: "error", Rule: "S1 §4.2", Message: "missing X-Request-ID"})
	}
	if headers.Get("X-Correlation-ID") == "" {
		findings = append(findings, Finding{Level: "error", Rule: "S1 §4.2", Message: "missing X-Correlation-ID"})
	}
	if functional {
		for _, name := range []string{"OCPI-to-party-id", "OCPI-to-country-code", "OCPI-from-party-id", "OCPI-from-country-code"} {
			if headers.Get(name) == "" {
				findings = append(findings, Finding{Level: "error", Rule: "S1 §4.1.8.2", Message: "missing routing header " + name})
			}
		}
	} else {
		for _, name := range []string{"OCPI-to-party-id", "OCPI-to-country-code", "OCPI-from-party-id", "OCPI-from-country-code"} {
			if headers.Get(name) != "" {
				findings = append(findings, Finding{Level: "error", Rule: "S1 §4.1.8.2", Message: "routing header must not be used for configuration module: " + name})
			}
		}
	}
	return findings
}

func ValidateRequestHeaders(method string, headers http.Header, functional bool) []Finding {
	findings := ValidateMessageHeaders(headers, functional)
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		contentType := strings.ToLower(headers.Get("Content-Type"))
		if !strings.HasPrefix(contentType, "application/json") {
			findings = append(findings, Finding{Level: "error", Rule: "S1 §4.1.4", Message: "request with a body must use Content-Type: application/json"})
		}
	}
	return findings
}

func ValidateResponse(requestHeaders, responseHeaders http.Header, functional bool, body []byte, httpStatus int) ([]Finding, *Envelope) {
	findings := []Finding{}
	if !strings.HasPrefix(strings.ToLower(responseHeaders.Get("Content-Type")), "application/json") {
		findings = append(findings, Finding{Level: "error", Rule: "S1 §4.1.7", Message: "response Content-Type must be application/json"})
	}
	if got := responseHeaders.Get("X-Request-ID"); got != requestHeaders.Get("X-Request-ID") {
		findings = append(findings, Finding{Level: "error", Rule: "S1 §4.2", Message: "response X-Request-ID does not echo request"})
	}
	if got := responseHeaders.Get("X-Correlation-ID"); got != requestHeaders.Get("X-Correlation-ID") {
		findings = append(findings, Finding{Level: "error", Rule: "S1 §4.2", Message: "response X-Correlation-ID does not echo request"})
	}
	if functional {
		pairs := [][2]string{
			{"OCPI-to-party-id", "OCPI-from-party-id"},
			{"OCPI-to-country-code", "OCPI-from-country-code"},
			{"OCPI-from-party-id", "OCPI-to-party-id"},
			{"OCPI-from-country-code", "OCPI-to-country-code"},
		}
		for _, pair := range pairs {
			if responseHeaders.Get(pair[0]) != requestHeaders.Get(pair[1]) {
				findings = append(findings, Finding{Level: "error", Rule: "S1 §4.1.8.6", Message: pair[0] + " does not reverse request routing"})
			}
		}
	}
	var envelope Envelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		findings = append(findings, Finding{Level: "error", Rule: "S1 §4.1.7", Message: "response is not a valid OCPI envelope"})
		return findings, nil
	}
	if envelope.StatusCode < 1000 || envelope.StatusCode > 4999 {
		findings = append(findings, Finding{Level: "error", Rule: "S1 §5", Message: "status_code is not a standard four-digit OCPI code"})
	}
	if _, err := time.Parse(time.RFC3339Nano, envelope.Timestamp); err != nil {
		findings = append(findings, Finding{Level: "error", Rule: "S1 §4.1.7", Message: "missing or invalid response timestamp"})
	}
	if httpStatus >= 200 && httpStatus < 300 && envelope.StatusCode >= 2000 {
		findings = append(findings, Finding{Level: "info", Rule: "S1 §5", Message: "HTTP succeeded but OCPI status reports an error; evaluate status_code"})
	}
	return findings, &envelope
}

func Headers(headers http.Header) map[string]any {
	result := map[string]any{}
	for key, values := range headers {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func Required(object map[string]any, rule string, fields ...string) []Finding {
	findings := []Finding{}
	for _, field := range fields {
		value, ok := object[field]
		if !ok || value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" {
			findings = append(findings, Finding{Level: "error", Rule: rule, Message: "missing required field: " + field})
		}
	}
	return findings
}

func OwnerMatches(object map[string]any, countryCode, partyID, objectID, idField, rule string) []Finding {
	findings := []Finding{}
	checks := map[string]string{"country_code": countryCode, "party_id": partyID, idField: objectID}
	for field, expected := range checks {
		if actual := fmt.Sprint(object[field]); actual != expected {
			findings = append(findings, Finding{Level: "error", Rule: rule, Message: fmt.Sprintf("URL %s=%q does not match object value %q", field, expected, actual)})
		}
	}
	return findings
}

func ParsePagination(request *http.Request) (dateFrom, dateTo *time.Time, offset, limit int, findings []Finding) {
	query := request.URL.Query()
	offset = 0
	limit = 100
	if raw := query.Get("offset"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			findings = append(findings, Finding{Level: "error", Rule: "S1 §4.1.4.1", Message: "invalid offset"})
		} else {
			offset = value
		}
	}
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			findings = append(findings, Finding{Level: "error", Rule: "S1 §4.1.4.1", Message: "invalid limit"})
		} else if value < limit {
			limit = value
		}
	}
	for name, target := range map[string]**time.Time{"date_from": &dateFrom, "date_to": &dateTo} {
		if raw := query.Get(name); raw != "" {
			value, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				findings = append(findings, Finding{Level: "error", Rule: "S1 §4.1.4.1", Message: "invalid " + name})
			} else {
				*target = &value
			}
		}
	}
	return
}

// MountPrefix is the path http.StripPrefix removed before the handler ran ("/dual", "/stg", "/stg/dual"),
// so a URL built from r.URL can point back at the same page.
func MountPrefix(r *http.Request) string {
	original, err := url.ParseRequestURI(r.RequestURI)
	if err != nil || !strings.HasSuffix(original.Path, r.URL.Path) {
		return ""
	}
	return strings.TrimSuffix(original.Path, r.URL.Path)
}

func HasErrors(findings []Finding) bool {
	for _, finding := range findings {
		if finding.Level == "error" {
			return true
		}
	}
	return false
}
