package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConsoleAuth(t *testing.T) {
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), alwaysUp{}, []Env{
		{ID: "dev", Handler: echoEnv("dev")},
		{ID: "stg", Prefix: "/stg", Handler: echoEnv("stg")},
	}, WithConsoleAuth("admin", "test-password"))
	for _, path := range []string{
		"/emsp", "/cpo", "/dual/", "/api/cpo/tokens", "/api/emsp/export",
		"/dual/api/emsp/state", "/stg/emsp", "/stg/dual/api/cpo/tokens",
		"/stg/api/requests/state", "/console-envs.js", "/emsp.js",
	} {
		for _, password := range []string{"", "incorrect", "test-password"} {
			t.Run(path+"/"+password, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, path, nil)
				if password != "" {
					request.SetBasicAuth("admin", password)
				}
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				want := http.StatusUnauthorized
				if password == "test-password" {
					want = http.StatusOK
				}
				if recorder.Code != want {
					t.Fatalf("status = %d, want %d", recorder.Code, want)
				}
				if want == http.StatusUnauthorized && recorder.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("missing Basic Auth challenge")
				}
			})
		}
	}
	for _, path := range []string{"/health", "/readiness", "/ocpi/emsp/versions", "/ocpi/cpo/2.2.1/commands/START_SESSION", "/dual/ocpi/2.2.1/credentials", "/stg/ocpi/cpo/versions", "/stg/dual/ocpi/versions"} {
		t.Run("protocol"+path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Token partner-token")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			if request.Header.Get("Authorization") != "Token partner-token" {
				t.Fatal("OCPI Authorization was modified")
			}
		})
	}
}

func TestConsoleAuthRejectsCrossOriginMutation(t *testing.T) {
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), alwaysUp{}, []Env{{ID: "dev", Handler: echoEnv("dev")}}, WithConsoleAuth("admin", "test-password"))
	for _, tc := range []struct {
		origin string
		want   int
	}{
		{"", http.StatusOK},
		{"https://console.example.com", http.StatusOK},
		{"https://other.example.com", http.StatusForbidden},
		{"null", http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPost, "https://console.example.com/api/emsp/reset", nil)
		request.SetBasicAuth("admin", "test-password")
		request.Header.Set("Origin", tc.origin)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != tc.want {
			t.Errorf("Origin %q: status = %d, want %d", tc.origin, recorder.Code, tc.want)
		}
	}
}
