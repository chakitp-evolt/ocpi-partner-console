package cpo

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

func TestCDRCorrectionSendsCreditBeforeEditableReplacement(t *testing.T) {
	controller := New("https://partner.example")
	original := map[string]any{
		"id": "original-1", "country_code": "TH", "party_id": "TST", "session_id": "session-1",
		"total_cost":   map[string]any{"excl_vat": 100.0, "incl_vat": 107.0},
		"total_energy": 10.0, "last_updated": "2026-09-29T00:00:00Z",
	}
	controller.mu.Lock()
	controller.state.Objects["cdrs"]["original-1"] = &Record{Body: cloneMap(original), Delivery: "SENT", CDRLocation: "https://evolt.example/cdrs/original-1"}
	controller.state.TokenC = "evolt-issued-token"
	controller.state.Endpoints = []Endpoint{{Identifier: "cdrs", Role: "RECEIVER", URL: "https://evolt.example/cdrs"}}
	controller.mu.Unlock()

	var received []map[string]any
	controller.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://evolt.example/cdrs" {
			t.Fatalf("unexpected outbound request %s %s", request.Method, request.URL)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		received = append(received, body)
		raw, _ := json.Marshal(ocpi.Success(nil))
		headers := http.Header{"Content-Type": []string{"application/json"}, "Location": []string{"https://evolt.example/cdrs/" + body["id"].(string)}}
		return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}
	mux := http.NewServeMux()
	controller.RegisterRoutes(mux)

	call := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		response := serveFunctional(t, mux, method, "https://partner.example"+path, body, "")
		if response.Code != want {
			t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, response.Code, want, response.Body.String())
		}
		var data map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return data
	}

	if result := call(http.MethodPut, "/api/cpo/objects/cdrs/original-1", original, http.StatusConflict); result["error"] == "" {
		t.Fatal("sent original must be immutable")
	}
	call(http.MethodDelete, "/api/cpo/objects/cdrs/original-1", nil, http.StatusConflict)
	creditID := call(http.MethodPost, "/api/cpo/cdrs/original-1/credit-draft", nil, http.StatusCreated)["id"].(string)
	credit := controller.state.Objects["cdrs"][creditID].Body
	if creditID != "original-1-C" || credit["credit"] != true || credit["credit_reference_id"] != "original-1" {
		t.Fatalf("invalid Credit CDR: %#v", credit)
	}
	if !reflect.DeepEqual(credit["total_cost"], map[string]any{"excl_vat": -100.0, "incl_vat": -107.0}) {
		t.Fatalf("credit total_cost = %#v", credit["total_cost"])
	}
	if credit["last_updated"] != original["last_updated"] || credit["session_id"] != original["session_id"] {
		t.Fatal("Credit CDR must copy the original values")
	}
	call(http.MethodPost, "/api/cpo/cdrs/"+creditID+"/replacement-draft", nil, http.StatusConflict)
	call(http.MethodPost, "/api/cpo/actions/send", map[string]any{"module": "cdrs", "id": creditID, "method": "POST"}, http.StatusOK)
	replacementID := call(http.MethodPost, "/api/cpo/cdrs/"+creditID+"/replacement-draft", nil, http.StatusCreated)["id"].(string)
	replacement := cloneMap(controller.state.Objects["cdrs"][replacementID].Body)
	if replacementID == creditID || replacementID == "original-1" || replacement["credit"] != nil || replacement["credit_reference_id"] != nil {
		t.Fatalf("invalid replacement: %#v", replacement)
	}
	replacement["total_cost"] = map[string]any{"excl_vat": 80.0, "incl_vat": 85.6}
	call(http.MethodPut, "/api/cpo/objects/cdrs/"+replacementID, replacement, http.StatusOK)
	call(http.MethodPost, "/api/cpo/actions/send", map[string]any{"module": "cdrs", "id": replacementID, "method": "POST"}, http.StatusOK)
	if len(received) != 2 || received[0]["id"] != creditID || received[1]["id"] != replacementID {
		t.Fatalf("outbound CDR order = %#v", received)
	}
	if !reflect.DeepEqual(controller.state.Objects["cdrs"]["original-1"].Body, cloneMap(original)) {
		t.Fatal("correction changed the original CDR")
	}
	call(http.MethodPost, "/api/cpo/actions/send", map[string]any{"module": "cdrs", "id": creditID, "method": "POST"}, http.StatusUnprocessableEntity)
}
