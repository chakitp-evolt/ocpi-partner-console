package ocpi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMountPrefixRecoversWhatStripPrefixRemoved(t *testing.T) {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, MountPrefix(r)) })
	stg := http.StripPrefix("/stg", echo)
	stgDual := http.StripPrefix("/stg", http.StripPrefix("/dual", echo))
	cases := []struct {
		name    string
		handler http.Handler
		target  string
		want    string
	}{
		{"not mounted", echo, "/ocpi/cpo/2.2.1/locations?offset=1", ""},
		{"env", stg, "/stg/ocpi/cpo/2.2.1/locations?offset=1&limit=1", "/stg"},
		{"env and dual", stgDual, "/stg/dual/ocpi/2.2.1/locations", "/stg/dual"},
		{"escaped id", stg, "/stg/ocpi/emsp/2.2.1/locations/TH/TST/LOC%201", "/stg"},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		tc.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.target, nil))
		if got := recorder.Body.String(); got != tc.want {
			t.Errorf("%s: MountPrefix = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := MountPrefix(&http.Request{URL: httptest.NewRequest(http.MethodGet, "/ocpi/x", nil).URL}); got != "" {
		t.Errorf("request without RequestURI: MountPrefix = %q, want empty", got)
	}
}
