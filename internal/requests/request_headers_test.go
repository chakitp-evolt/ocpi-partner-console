package requests

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExactHeadersPreserveMalformedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, key := range []string{"Content-Type", "X-Request-ID", "X-Correlation-ID", "Authorization"} {
			if r.Header.Get(key) != "" {
				t.Errorf("unexpected default %s", key)
			}
		}
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != `{"broken":` {
			t.Errorf("body was changed: %s", raw)
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{"broken":`))
	if err != nil {
		t.Fatal(err)
	}
	applyFixtureHeaderDefaults(req, true)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatal(response.StatusCode)
	}
}

func TestFixtureDefaultHeadersRemainBackwardCompatible(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.test", nil)
	req.Header.Set("X-Request-ID", "qa-custom-id")
	req.Header.Set("Content-Type", "text/plain")
	applyFixtureHeaderDefaults(req, false)
	if req.Header.Get("X-Request-ID") != "qa-custom-id" || req.Header.Get("Content-Type") != "text/plain" || req.Header.Get("X-Correlation-ID") == "" {
		t.Fatal("defaults replaced supplied headers or omitted correlation ID")
	}
	get, _ := http.NewRequest(http.MethodGet, "https://example.test", nil)
	applyFixtureHeaderDefaults(get, false)
	if get.Header.Get("Content-Type") != "" {
		t.Fatal("GET must keep existing behavior")
	}
}
