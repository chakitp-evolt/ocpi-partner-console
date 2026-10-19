package config

import (
	"strings"
	"testing"
)

func TestParseEnvs(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    []Env
		wantErr string
	}{
		{
			name: "no CONSOLE_ENVS keeps dev only",
			env:  map[string]string{"EVOLT_VERSIONS_URL": "http://dev/versions"},
			want: []Env{{ID: "dev", EvoltVersionsURL: "http://dev/versions", PublicBaseURL: "https://console"}},
		},
		{
			name: "each env is served under its id with its own Versions URL",
			env:  map[string]string{"EVOLT_VERSIONS_URL": "http://dev/versions", "CONSOLE_ENVS": " stg , uat-1 ", "EVOLT_VERSIONS_URL_STG": "http://stg/versions", "EVOLT_VERSIONS_URL_UAT_1": "http://uat/versions"},
			want: []Env{
				{ID: "dev", EvoltVersionsURL: "http://dev/versions", PublicBaseURL: "https://console"},
				{ID: "stg", Prefix: "/stg", EvoltVersionsURL: "http://stg/versions", PublicBaseURL: "https://console/stg"},
				{ID: "uat-1", Prefix: "/uat-1", EvoltVersionsURL: "http://uat/versions", PublicBaseURL: "https://console/uat-1"},
			},
		},
		{
			name:    "an env without its Versions URL never falls back to dev",
			env:     map[string]string{"EVOLT_VERSIONS_URL": "http://dev/versions", "CONSOLE_ENVS": "stg"},
			wantErr: "EVOLT_VERSIONS_URL_STG is required",
		},
		{
			name:    "an id the console already serves is rejected",
			env:     map[string]string{"CONSOLE_ENVS": "dual", "EVOLT_VERSIONS_URL_DUAL": "http://x"},
			wantErr: "reserved",
		},
		{
			name:    "an id listed twice is rejected",
			env:     map[string]string{"CONSOLE_ENVS": "stg,stg", "EVOLT_VERSIONS_URL_STG": "http://x"},
			wantErr: "listed twice",
		},
		{
			name:    "an id that is not a lowercase path segment is rejected",
			env:     map[string]string{"CONSOLE_ENVS": "Stg", "EVOLT_VERSIONS_URL_STG": "http://x"},
			wantErr: "lowercase",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t, "CONSOLE_ENVS", "EVOLT_VERSIONS_URL", "EVOLT_VERSIONS_URL_STG", "EVOLT_VERSIONS_URL_UAT_1", "EVOLT_VERSIONS_URL_DUAL")
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			got, err := parseEnvs("https://console")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d envs %#v, want %#v", len(got), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("env %d = %#v, want %#v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
