//go:build integration

package emsp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/server"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

type envConsole struct {
	t       *testing.T
	baseURL string
	dev     *store.Store
	stg     *store.Store
}

func newEnvConsole(t *testing.T) *envConsole {
	t.Helper()
	db := integrationDatabase(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dev := store.New(db, "integration-test-encryption-key")
	stg := dev.ForEnv("stg", "http://evolt-stg.example/versions")
	devConsole, err := New(dev, logger, Options{DefaultPublicURL: "http://console.example"})
	if err != nil {
		t.Fatal(err)
	}
	stgConsole, err := New(stg, logger, Options{DefaultPublicURL: "http://console.example/stg", MountPath: "/stg"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = devConsole.Close(context.Background())
		_ = stgConsole.Close(context.Background())
	})
	srv := httptest.NewServer(server.New(logger, dev, []server.Env{
		{ID: "dev", Handler: devConsole.Handler()},
		{ID: "stg", Prefix: "/stg", Handler: stgConsole.Handler()},
	}))
	t.Cleanup(srv.Close)
	return &envConsole{t: t, baseURL: srv.URL, dev: dev, stg: stg}
}

func (c *envConsole) runConfig(path string) (string, map[string]any) {
	c.t.Helper()
	state := callAPI(c.t, http.MethodGet, c.baseURL+path, nil, http.StatusOK)
	run := state["run"].(map[string]any)
	return run["id"].(string), run["config"].(map[string]any)
}

func (c *envConsole) initialToken(path string) (string, string) {
	c.t.Helper()
	answer := callAPI(c.t, http.MethodPost, c.baseURL+path, nil, http.StatusCreated)
	return answer["token"].(string), answer["versions_url"].(string)
}

func (c *envConsole) versionsStatus(path, token string) int {
	c.t.Helper()
	request, _ := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	request.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(token)))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

func TestEnvsStartWithTheirOwnURLs(t *testing.T) {
	c := newEnvConsole(t)

	devID, devConfig := c.runConfig("/api/emsp/state")
	stgID, stgConfig := c.runConfig("/stg/api/emsp/state")
	if devID == stgID {
		t.Fatal("dev and stg share one eMSP connection")
	}
	if stgConfig["evolt_versions_url"] != "http://evolt-stg.example/versions" || stgConfig["partner_public_url"] != "http://console.example/stg" {
		t.Fatalf("stg eMSP config = %#v", stgConfig)
	}
	if devConfig["evolt_versions_url"] == stgConfig["evolt_versions_url"] || devConfig["partner_public_url"] != "http://console.example" {
		t.Fatalf("dev eMSP config = %#v", devConfig)
	}
	_, dualConfig := c.runConfig("/stg/dual/api/emsp/state")
	if dualConfig["partner_public_url"] != "http://console.example/stg/dual" {
		t.Fatalf("stg dual Partner public URL = %v", dualConfig["partner_public_url"])
	}

	cpoState := callAPI(t, http.MethodGet, c.baseURL+"/stg/api/cpo/state", nil, http.StatusOK)
	cpoConfig := cpoState["config"].(map[string]any)
	if cpoConfig["evolt_versions_url"] != "http://evolt-stg.example/versions" || cpoConfig["partner_public_url"] != "http://console.example/stg" {
		t.Fatalf("stg CPO config = %#v", cpoConfig)
	}

	stgSnapshot, err := c.stg.LoadCPOProfile(context.Background(), "cpo")
	if err != nil {
		t.Fatal(err)
	}
	devSnapshot, err := c.dev.LoadCPOProfile(context.Background(), "cpo")
	if err != nil {
		t.Fatal(err)
	}
	if stgSnapshot.RunID == devSnapshot.RunID {
		t.Fatal("dev and stg load the same CPO connection")
	}
}

func TestTokensOfOneEnvAreRejectedByTheOther(t *testing.T) {
	c := newEnvConsole(t)

	devToken, devVersions := c.initialToken("/api/emsp/initial-token")
	stgToken, stgVersions := c.initialToken("/stg/api/emsp/initial-token")
	_, stgDualVersions := c.initialToken("/stg/dual/api/emsp/initial-token")
	if devVersions != "http://console.example/ocpi/emsp/versions" || stgVersions != "http://console.example/stg/ocpi/emsp/versions" || stgDualVersions != "http://console.example/stg/dual/ocpi/versions" {
		t.Fatalf("Versions URLs handed out: dev %q stg %q stg dual %q", devVersions, stgVersions, stgDualVersions)
	}

	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/ocpi/emsp/versions", devToken, http.StatusOK},
		{"/stg/ocpi/emsp/versions", stgToken, http.StatusOK},
		{"/stg/ocpi/emsp/versions", devToken, http.StatusUnauthorized},
		{"/ocpi/emsp/versions", stgToken, http.StatusUnauthorized},
	} {
		if got := c.versionsStatus(tc.path, tc.token); got != tc.want {
			t.Errorf("%s answered %d, want %d", tc.path, got, tc.want)
		}
	}
}

func TestResetOfOneEnvKeepsTheOther(t *testing.T) {
	c := newEnvConsole(t)
	devID, _ := c.runConfig("/api/emsp/state")
	stgID, _ := c.runConfig("/stg/api/emsp/state")

	callAPI(t, http.MethodPost, c.baseURL+"/stg/api/emsp/reset", map[string]any{"confirmation": "RESET"}, http.StatusOK)

	if after, _ := c.runConfig("/api/emsp/state"); after != devID {
		t.Fatalf("stg reset replaced the dev connection %s with %s", devID, after)
	}
	if after, _ := c.runConfig("/stg/api/emsp/state"); after == stgID {
		t.Fatal("stg reset kept the old stg connection")
	}
}

func TestPartnerPublicURLStaysOnTheEnvPage(t *testing.T) {
	c := newEnvConsole(t)
	callAPI(t, http.MethodPut, c.baseURL+"/stg/api/emsp/config", map[string]any{"partner_public_url": "https://typed.example/"}, http.StatusOK)
	callAPI(t, http.MethodPut, c.baseURL+"/stg/dual/api/emsp/config", map[string]any{"partner_public_url": "https://typed.example"}, http.StatusOK)
	callAPI(t, http.MethodPut, c.baseURL+"/stg/api/cpo/config", map[string]any{"partner_public_url": "https://typed.example"}, http.StatusOK)

	if _, config := c.runConfig("/stg/api/emsp/state"); config["partner_public_url"] != "https://typed.example/stg" {
		t.Fatalf("stg eMSP Partner public URL = %v", config["partner_public_url"])
	}
	if _, config := c.runConfig("/stg/dual/api/emsp/state"); config["partner_public_url"] != "https://typed.example/stg/dual" {
		t.Fatalf("stg dual Partner public URL = %v", config["partner_public_url"])
	}
	cpoConfig := callAPI(t, http.MethodGet, c.baseURL+"/stg/api/cpo/state", nil, http.StatusOK)["config"].(map[string]any)
	if cpoConfig["partner_public_url"] != "https://typed.example/stg" {
		t.Fatalf("stg CPO Partner public URL = %v", cpoConfig["partner_public_url"])
	}
	if _, config := c.runConfig("/api/emsp/state"); config["partner_public_url"] != "http://console.example" {
		t.Fatalf("dev Partner public URL changed to %v", config["partner_public_url"])
	}
}

func TestSavedRequestsAreSharedButTheirHistoryAndLanesAreNot(t *testing.T) {
	c := newEnvConsole(t)
	ctx := context.Background()

	devFixtures, err := c.dev.ListRequestFixtures(ctx)
	if err != nil || len(devFixtures) == 0 {
		t.Fatalf("saved requests: %v (%d)", err, len(devFixtures))
	}
	stgFixtures, err := c.stg.ListRequestFixtures(ctx)
	if err != nil || len(stgFixtures) != len(devFixtures) {
		t.Fatalf("stg sees %d saved requests, dev %d: %v", len(stgFixtures), len(devFixtures), err)
	}

	request, _ := json.Marshal(map[string]any{"method": "GET", "url": "http://evolt-stg.example/x"})
	response, _ := json.Marshal(map[string]any{"http_status": 200})
	if err := c.stg.AddRequestExchange(ctx, store.RequestExchange{FixtureID: devFixtures[0].ID, Request: request, Response: response, Status: "RECORDED"}); err != nil {
		t.Fatal(err)
	}
	stgHistory, err := c.stg.RecentRequestExchanges(ctx)
	if err != nil || len(stgHistory) != 1 {
		t.Fatalf("stg history = %d, %v", len(stgHistory), err)
	}
	if devHistory, err := c.dev.RecentRequestExchanges(ctx); err != nil || len(devHistory) != 0 {
		t.Fatalf("a stg send shows in dev history: %d, %v", len(devHistory), err)
	}

	stgRun, err := c.stg.CurrentRunProfile(ctx, "emsp")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.stg.ReplaceEndpoints(ctx, stgRun.ID, "remote", []map[string]any{{"identifier": "tokens", "role": "RECEIVER", "url": "http://evolt-stg.example/tokens"}}); err != nil {
		t.Fatal(err)
	}
	if urls, err := c.stg.LaneEndpointURLs(ctx, "EMSP"); err != nil || len(urls) != 1 {
		t.Fatalf("stg lane endpoints = %v, %v", urls, err)
	}
	if urls, err := c.dev.LaneEndpointURLs(ctx, "EMSP"); err != nil || len(urls) != 0 {
		t.Fatalf("dev lane sees stg endpoints: %v, %v", urls, err)
	}
}
