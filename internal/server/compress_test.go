package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompressDashboard(t *testing.T) {
	body := strings.Repeat(`{"id":"LOC-1","status":"AVAILABLE"},`, 200)
	jsonHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, body)
	})

	cases := []struct {
		name         string
		path         string
		accept       string
		noGzip       bool
		wantEncoding string
	}{
		{name: "dashboard api is gzipped", path: "/api/cpo/state", wantEncoding: "gzip"},
		{name: "dual dashboard api is gzipped", path: "/dual/api/cpo/state", wantEncoding: "gzip"},
		// Evolt must keep seeing the OCPI responses exactly as before.
		{name: "ocpi stays plain", path: "/ocpi/emsp/2.2.1/locations"},
		{name: "dual ocpi stays plain", path: "/dual/ocpi/cpo/2.2.1/locations"},
		{name: "probe stays plain", path: "/health"},
		{name: "client without gzip gets plain", path: "/api/cpo/state", noGzip: true},
		{name: "event stream stays plain", path: "/api/cpo/events", accept: "text/event-stream"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, testCase.path, nil)
			if !testCase.noGzip {
				request.Header.Set("Accept-Encoding", "gzip, deflate, br")
			}
			if testCase.accept != "" {
				request.Header.Set("Accept", testCase.accept)
			}
			recorder := httptest.NewRecorder()
			compressDashboard(jsonHandler).ServeHTTP(recorder, request)

			if got := recorder.Header().Get("Content-Encoding"); got != testCase.wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, testCase.wantEncoding)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			reader := io.Reader(recorder.Body)
			if testCase.wantEncoding == "gzip" {
				gz, err := gzip.NewReader(recorder.Body)
				if err != nil {
					t.Fatalf("gzip reader: %v", err)
				}
				reader = gz
			}
			decoded, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !strings.Contains(string(decoded), "LOC-1") {
				t.Fatalf("decoded body lost its content: %.80s", decoded)
			}
		})
	}
}

func TestCompressDashboardKeepsStatusAndSniffedType(t *testing.T) {
	handler := compressDashboard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "<!doctype html><html><body>partner</body></html>")
	}))
	request := httptest.NewRequest(http.MethodGet, "/cpo", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusCreated)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html sniffed from the plain body", got)
	}
}

func TestCompressDashboardLeavesEmptyResponsesAlone(t *testing.T) {
	handler := compressDashboard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodDelete, "/api/cpo/objects/locations/LOC-1", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent || recorder.Header().Get("Content-Encoding") != "" || recorder.Body.Len() != 0 {
		t.Fatalf("got status %d, encoding %q, %d body bytes", recorder.Code, recorder.Header().Get("Content-Encoding"), recorder.Body.Len())
	}
}
