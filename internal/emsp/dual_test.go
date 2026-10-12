package emsp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/cpo"
)

func TestDualAutomationSettingsDoNotChangeOutboundSettings(t *testing.T) {
	base := http.NewServeMux()
	cpo.New("https://base.example").RegisterRoutes(base)
	dual := http.NewServeMux()
	cpo.New("https://dual.example").RegisterRoutesWithConnectionHandlers(dual, nil)
	router := routeDual(base, dual)
	set := func(path, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPut, path, strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, w.Code)
		}
	}
	interval := func(path string) int {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		var data struct {
			Config struct {
				SendInterval int `json:"send_interval_seconds"`
			} `json:"config"`
		}
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, w.Code)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return data.Config.SendInterval
	}
	set("/dual/api/cpo/automation", `{"enabled":false,"rate_kwh_per_hour":7.2,"send_interval_seconds":60}`)
	if interval("/dual/api/cpo/state") != 60 || interval("/api/cpo/state") != 0 {
		t.Fatal("Dual settings changed the Outbound default")
	}
	set("/api/cpo/automation", `{"enabled":false,"rate_kwh_per_hour":7.2,"send_interval_seconds":5}`)
	if interval("/api/cpo/state") != 5 || interval("/dual/api/cpo/state") != 60 {
		t.Fatal("Outbound settings changed Dual")
	}
}

func TestDualRouteDispatch(t *testing.T) {
	base := http.NewServeMux()
	base.HandleFunc("GET /dual", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dual/", http.StatusTemporaryRedirect)
	})
	base.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("base " + r.URL.Path))
	})
	dual := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("dual " + r.Method + " " + r.URL.Path))
	})
	router := routeDual(base, dual)
	for _, tc := range []struct {
		method, path, want string
	}{
		{http.MethodGet, "/", "base /"},
		{http.MethodGet, "/emsp", "base /emsp"},
		{http.MethodGet, "/dual/", "dual GET /"},
		{http.MethodPost, "/dual/ocpi/emsp/2.2.1/cdrs", "dual POST /ocpi/emsp/2.2.1/cdrs"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if body := recorder.Body.String(); body != tc.want {
			t.Errorf("%s %s: body = %q, want %q", tc.method, tc.path, body, tc.want)
		}
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dual", nil))
	if recorder.Code != http.StatusTemporaryRedirect || recorder.Header().Get("Location") != "/dual/" {
		t.Fatalf("GET /dual: status = %d, location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestDualRoleCredentialsAndVersionDetails(t *testing.T) {
	config := Config{PartnerPublicURL: "https://partner.example/dual", CountryCode: "TH", PartyID: "TST", BusinessName: "Test Partner"}
	roles := (&Controller{profile: "dual"}).localRoles(config)
	if len(roles) != 2 || roles[0]["role"] != "CPO" || roles[1]["role"] != "EMSP" {
		t.Fatalf("expected one CPO and one EMSP role, got %#v", roles)
	}
	for _, role := range roles {
		if role["country_code"] != "TH" || role["party_id"] != "TST" {
			t.Fatalf("roles must use one partner identity, got %#v", role)
		}
	}
	details := dualVersionDetails(config)
	endpoints := details["endpoints"].([]map[string]any)
	seen := map[string]string{}
	for _, endpoint := range endpoints {
		key := endpoint["identifier"].(string) + "/" + endpoint["role"].(string)
		if seen[key] != "" {
			t.Fatalf("duplicate module interface %s", key)
		}
		url := endpoint["url"].(string)
		if !strings.HasPrefix(url, config.PartnerPublicURL+"/ocpi/") {
			t.Fatalf("endpoint escaped the dual connection: %s", url)
		}
		seen[key] = url
	}
	for _, module := range []string{"locations", "tariffs", "sessions", "cdrs", "tokens", "commands"} {
		if seen[module+"/SENDER"] == "" || seen[module+"/RECEIVER"] == "" {
			t.Fatalf("%s must expose both OCPI interfaces", module)
		}
	}
	if seen["credentials/SENDER"] == "" || seen["credentials/RECEIVER"] != "" {
		t.Fatal("one connection must advertise one Credentials interface")
	}
}
