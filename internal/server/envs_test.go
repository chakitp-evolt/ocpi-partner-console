package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type alwaysUp struct{}

func (alwaysUp) Ping(context.Context) error { return nil }

func echoEnv(id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, id+" "+r.URL.Path)
	})
}

func TestEnvsAreServedUnderTheirPrefix(t *testing.T) {
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), alwaysUp{}, []Env{
		{ID: "dev", Handler: echoEnv("dev")},
		{ID: "stg", Prefix: "/stg", Handler: echoEnv("stg")},
	})
	cases := []struct {
		path, wantBody, wantRedirect string
	}{
		{path: "/emsp", wantBody: "dev /emsp"},
		{path: "/dual/ocpi/versions", wantBody: "dev /dual/ocpi/versions"},
		{path: "/stg/emsp", wantBody: "stg /emsp"},
		{path: "/stg/ocpi/emsp/versions", wantBody: "stg /ocpi/emsp/versions"},
		{path: "/stg/dual/", wantBody: "stg /dual/"},
		{path: "/stg/dual/api/emsp/state", wantBody: "stg /dual/api/emsp/state"},
		{path: "/stagings", wantBody: "dev /stagings"},
		{path: "/stg", wantRedirect: "/stg/emsp"},
		{path: "/stg/", wantRedirect: "/stg/emsp"},
		{path: "/stg/dual", wantRedirect: "/stg/dual/"},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if tc.wantRedirect != "" {
			if recorder.Code != http.StatusTemporaryRedirect || recorder.Header().Get("Location") != tc.wantRedirect {
				t.Errorf("%s: HTTP %d Location %q, want redirect to %q", tc.path, recorder.Code, recorder.Header().Get("Location"), tc.wantRedirect)
			}
			continue
		}
		if body := recorder.Body.String(); body != tc.wantBody {
			t.Errorf("%s: body %q, want %q", tc.path, body, tc.wantBody)
		}
	}
}

func TestConsoleEnvsScriptListsEveryEnv(t *testing.T) {
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), alwaysUp{}, []Env{
		{ID: "dev", Handler: echoEnv("dev")},
		{ID: "stg", Prefix: "/stg", Handler: echoEnv("stg")},
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/console-envs.js", nil))
	want := `window.CONSOLE_ENVS=[{"id":"dev","prefix":""},{"id":"stg","prefix":"/stg"}];`
	if got := strings.TrimSpace(recorder.Body.String()); got != want {
		t.Fatalf("console-envs.js = %q, want %q", got, want)
	}
	if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("Content-Type = %q", recorder.Header().Get("Content-Type"))
	}
}

func TestOCPIPathsUnderEveryPrefixStayUncompressed(t *testing.T) {
	for path, want := range map[string]bool{
		"/ocpi/emsp/versions":         true,
		"/dual/ocpi/versions":         true,
		"/stg/ocpi/cpo/2.2.1/tariffs": true,
		"/stg/dual/ocpi/2.2.1":        true,
		"/stg/api/emsp/state":         false,
		"/stg/emsp":                   false,
	} {
		if got := isOCPIPath(path); got != want {
			t.Errorf("isOCPIPath(%q) = %v, want %v", path, got, want)
		}
	}
}
