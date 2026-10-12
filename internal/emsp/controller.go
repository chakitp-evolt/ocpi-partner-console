package emsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/cpo"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/defaults"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/requests"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/web"
)

type Options struct {
	DefaultPublicURL string
}

type Controller struct {
	store              *store.Store
	logger             *slog.Logger
	client             *http.Client
	hub                *eventHub
	cpo                *cpo.Controller
	dual               *Controller
	profile            string
	requests           *requests.Controller
	options            Options
	inboundRotationMu  sync.Mutex
	stagedInboundMu    sync.RWMutex
	stagedInboundToken string
}

type Config struct {
	EvoltVersionsURL      string           `json:"evolt_versions_url"`
	PartnerPublicURL      string           `json:"partner_public_url"`
	CountryCode           string           `json:"country_code"`
	PartyID               string           `json:"party_id"`
	BusinessName          string           `json:"business_name"`
	Strict                bool             `json:"strict"`
	RawTokenCompatibility bool             `json:"raw_token_compatibility"`
	RemoteIdentity        map[string]any   `json:"remote_identity,omitempty"`
	RemoteRoles           []map[string]any `json:"remote_roles,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

func New(repository *store.Store, logger *slog.Logger, options Options) (*Controller, error) {
	if _, err := repository.EnsureRun(context.Background(), options.DefaultPublicURL); err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout:       25 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	partner, err := cpo.NewPersistent(repository, options.DefaultPublicURL)
	if err != nil {
		return nil, fmt.Errorf("initialize Partner CPO database: %w", err)
	}
	requestsController, err := requests.New(repository)
	if err != nil {
		return nil, fmt.Errorf("initialize saved requests: %w", err)
	}
	app := &Controller{store: repository, logger: logger, client: client, hub: newEventHub(), cpo: partner, requests: requestsController, options: options, profile: "emsp"}
	dualURL := strings.TrimRight(options.DefaultPublicURL, "/") + "/dual"
	if _, err := repository.EnsureRunProfile(context.Background(), "dual", dualURL); err != nil {
		return nil, err
	}
	dualPartner, err := cpo.NewPersistentProfile(repository, dualURL, "dual")
	if err != nil {
		return nil, fmt.Errorf("initialize dual-role partner: %w", err)
	}
	app.dual = &Controller{store: repository, logger: logger, client: client, hub: newEventHub(), cpo: dualPartner, requests: requestsController, options: options, profile: "dual"}
	if err := app.dual.syncDualConnection(context.Background()); err != nil {
		return nil, fmt.Errorf("restore dual-role connection: %w", err)
	}
	return app, nil
}

func (a *Controller) run(r *http.Request) (store.Run, error) {
	return a.store.CurrentRunProfile(r.Context(), a.profile)
}

// Close writes the partner state that is still waiting for its debounced save.
func (a *Controller) Close(ctx context.Context) error {
	err := a.cpo.Flush(ctx)
	if a.dual != nil {
		err = errors.Join(err, a.dual.cpo.Flush(ctx))
	}
	return err
}

func (a *Controller) Handler() http.Handler {
	return a.handler(true)
}

func (a *Controller) handler(includeDual bool) http.Handler {
	mux := http.NewServeMux()
	if includeDual && a.dual != nil {
		mux.HandleFunc("GET /dual", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dual/", http.StatusTemporaryRedirect)
		})
	}
	if a.profile == "dual" {
		a.cpo.RegisterRoutesWithConnectionHandlers(mux, map[string]http.HandlerFunc{
			"config": a.updateConfig, "reset": a.resetRun, "initial-token": a.createInitialToken,
			"discover": a.discover, "register": a.register, "credentials": a.dualCredentialsAction,
			"protocol-versions": func(w http.ResponseWriter, r *http.Request) { a.inbound("versions", false, a.versions).ServeHTTP(w, r) },
			"protocol-details": func(w http.ResponseWriter, r *http.Request) {
				a.inbound("versions", false, a.versionDetails).ServeHTTP(w, r)
			},
			"protocol-credentials": func(w http.ResponseWriter, r *http.Request) {
				a.inbound("credentials", false, a.credentials).ServeHTTP(w, r)
			},
		})
	} else {
		a.cpo.RegisterRoutes(mux)
	}
	a.requests.RegisterRoutes(mux)
	mux.HandleFunc("GET /api/emsp/state", a.state)
	mux.HandleFunc("PUT /api/emsp/config", a.updateConfig)
	mux.HandleFunc("POST /api/emsp/reset", a.resetRun)
	mux.HandleFunc("POST /api/emsp/initial-token", a.createInitialToken)
	mux.HandleFunc("POST /api/emsp/tokens", a.createToken)
	mux.HandleFunc("POST /api/emsp/actions/discover", a.discover)
	mux.HandleFunc("POST /api/emsp/actions/register", a.register)
	mux.HandleFunc("POST /api/emsp/actions/check-credentials", a.checkCredentials)
	mux.HandleFunc("POST /api/emsp/actions/rotate-credentials", a.rotateCredentials)
	mux.HandleFunc("POST /api/emsp/actions/push-token", a.pushToken)
	mux.HandleFunc("POST /api/emsp/actions/pull", a.pullModule)
	mux.HandleFunc("POST /api/emsp/actions/command", a.sendCommand)
	mux.HandleFunc("GET /api/emsp/events", a.events)
	mux.HandleFunc("GET /api/emsp/export", a.export)

	mux.Handle("GET "+a.ocpiRoot()+"/versions", a.inbound("versions", false, a.versions))
	mux.Handle("GET "+a.ocpiRoot()+"/2.2.1", a.inbound("versions", false, a.versionDetails))
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		mux.Handle(method+" /ocpi/emsp/2.2.1/credentials", a.inbound("credentials", false, a.credentials))
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch} {
		mux.Handle(method+" /ocpi/emsp/2.2.1/locations/", a.inbound("locations", true, a.locations))
		mux.Handle(method+" /ocpi/emsp/2.2.1/sessions/", a.inbound("sessions", true, a.sessions))
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		mux.Handle(method+" /ocpi/emsp/2.2.1/tariffs/", a.inbound("tariffs", true, a.tariffs))
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		mux.Handle(method+" /ocpi/emsp/2.2.1/cdrs", a.inbound("cdrs", true, a.cdrs))
		mux.Handle(method+" /ocpi/emsp/2.2.1/cdrs/", a.inbound("cdrs", true, a.cdrs))
	}
	mux.Handle("GET /ocpi/emsp/2.2.1/tokens", a.inbound("tokens", true, a.tokens))
	mux.Handle("GET /ocpi/emsp/2.2.1/tokens/", a.inbound("tokens", true, a.tokens))
	mux.Handle("POST /ocpi/emsp/2.2.1/tokens/", a.inbound("tokens", true, a.tokens))
	mux.Handle("POST /ocpi/emsp/2.2.1/commands/callback/", a.inbound("commands", true, a.commandCallback))

	webRoot := web.FS
	mux.HandleFunc("GET /cpo", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, webRoot, "cpo.html")
	})
	if a.profile == "dual" {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, webRoot, "dual.html") })
	} else {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/emsp", http.StatusTemporaryRedirect)
		})
	}
	mux.HandleFunc("GET /emsp", func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, webRoot, "emsp.html") })
	mux.Handle("GET /", http.FileServer(http.FS(webRoot)))
	var router http.Handler = mux
	if includeDual && a.dual != nil {
		router = routeDual(mux, a.dual.handler(false))
	}
	return router
}

// ocpiRoot is where Versions lives: the eMSP mock sits next to the CPO mock under /ocpi/emsp,
// while the dual-role partner announces both roles from one /ocpi root.
func (a *Controller) ocpiRoot() string {
	if a.profile == "dual" {
		return "/ocpi"
	}
	return "/ocpi/emsp"
}

// versionsURL is the Versions URL this mock hands out in its Credentials.
func (a *Controller) versionsURL(config Config) string {
	return strings.TrimRight(config.PartnerPublicURL, "/") + a.ocpiRoot() + "/versions"
}

func routeDual(base, dual http.Handler) http.Handler {
	dual = http.StripPrefix("/dual", dual)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dual/") {
			dual.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

func (a *Controller) createInitialToken(w http.ResponseWriter, r *http.Request) {
	run, err := a.run(r)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load run", err)
		return
	}
	if run.Status == "REGISTERED" {
		writeAPIError(w, http.StatusConflict, "connection is already registered", fmt.Errorf("reset the run before starting a new registration"))
		return
	}
	config, err := configFromRun(run)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "parse config", err)
		return
	}
	token := ocpi.NewToken()
	if err := a.store.PutSecret(r.Context(), run.ID, "mock_initial_token", token); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "store initial token", err)
		return
	}
	a.changed()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "versions_url": a.versionsURL(config), "shown_once": true})
}

func (a *Controller) resetRun(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Confirmation string `json:"confirmation"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON", err)
		return
	}
	if input.Confirmation != "RESET" {
		writeAPIError(w, http.StatusBadRequest, "reset confirmation must be exactly RESET", fmt.Errorf("confirmation rejected"))
		return
	}
	run, err := a.run(r)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load run", err)
		return
	}
	defaultURL := a.options.DefaultPublicURL
	if a.profile == "dual" {
		defaultURL = strings.TrimRight(defaultURL, "/") + "/dual"
	}
	clean, err := a.store.ResetRunProfile(r.Context(), run.ID, a.profile, defaultURL)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reset current partner run", err)
		return
	}
	if a.profile == "dual" {
		if err := a.cpo.ResetData(r.Context()); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "reset dual CPO data", err)
			return
		}
	}
	a.changed()
	writeJSON(w, http.StatusOK, map[string]any{"status": clean.Status, "old_run_id": run.ID, "new_run_id": clean.ID})
}

func (a *Controller) state(w http.ResponseWriter, r *http.Request) {
	run, err := a.run(r)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load run", err)
		return
	}
	state, err := a.store.State(r.Context(), run.ID, r.URL.Query().Get("reveal_secrets") == "1")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load state", err)
		return
	}
	// through configFromRun so the console shows the default URLs when the run has none saved
	config, err := configFromRun(run)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "parse config", err)
		return
	}
	response := map[string]any{
		"run":      map[string]any{"id": run.ID, "name": run.Name, "status": run.Status, "config": config},
		"security": map[string]any{"development_encryption_key": a.store.WeakEncryptionKey()},
	}
	for key, raw := range state {
		var value any
		_ = json.Unmarshal(raw, &value)
		response[key] = value
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *Controller) export(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Disposition", `attachment; filename="ocpi-black-box-report.json"`)
	a.state(w, r)
}

func (a *Controller) updateConfig(w http.ResponseWriter, r *http.Request) {
	run, err := a.run(r)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load run", err)
		return
	}
	current, err := configFromRun(run)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "parse config", err)
		return
	}
	var input struct {
		Name   string `json:"name"`
		TokenA string `json:"token_a"`
		Config
	}
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON", err)
		return
	}
	if input.PartnerPublicURL != "" {
		current.PartnerPublicURL = strings.TrimRight(input.PartnerPublicURL, "/")
		if a.profile == "dual" && !strings.HasSuffix(current.PartnerPublicURL, "/dual") {
			current.PartnerPublicURL += "/dual"
		}
	}
	if input.EvoltVersionsURL != "" {
		current.EvoltVersionsURL = input.EvoltVersionsURL
	}
	if input.CountryCode != "" {
		current.CountryCode = strings.ToUpper(input.CountryCode)
	}
	if input.PartyID != "" {
		current.PartyID = strings.ToUpper(input.PartyID)
	}
	if input.BusinessName != "" {
		current.BusinessName = input.BusinessName
	}
	current.Strict = input.Strict
	current.RawTokenCompatibility = input.RawTokenCompatibility
	name := run.Name
	if input.Name != "" {
		name = input.Name
	}
	if input.TokenA != "" {
		if err := a.store.PutSecret(r.Context(), run.ID, "remote_token_a", input.TokenA); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "store Token A", err)
			return
		}
	}
	if err := a.store.UpdateRun(r.Context(), run.ID, name, run.Status, current); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save config", err)
		return
	}
	a.changed()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token_a": input.TokenA})
}

func (a *Controller) createToken(w http.ResponseWriter, r *http.Request) {
	run, err := a.run(r)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load run", err)
		return
	}
	config, _ := configFromRun(run)
	var body map[string]any
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid token JSON", err)
		return
	}
	if body["country_code"] == nil {
		body["country_code"] = config.CountryCode
	}
	if body["party_id"] == nil {
		body["party_id"] = config.PartyID
	}
	if body["type"] == nil {
		body["type"] = "RFID"
	}
	if body["issuer"] == nil {
		body["issuer"] = config.BusinessName
	}
	if body["valid"] == nil {
		body["valid"] = true
	}
	if body["whitelist"] == nil {
		body["whitelist"] = "ALLOWED"
	}
	body["last_updated"] = ocpi.FormatTime(time.Now())
	findings := ocpi.Required(body, "S1 §12.3.2", "country_code", "party_id", "uid", "type", "contract_id", "issuer", "valid", "whitelist", "last_updated")
	if ocpi.HasErrors(findings) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid Token", "findings": findings})
		return
	}
	if err := a.store.UpsertToken(r.Context(), run.ID, body); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "store token", err)
		return
	}
	a.changed()
	writeJSON(w, http.StatusCreated, body)
}

func (a *Controller) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	channel := a.hub.subscribe()
	defer a.hub.unsubscribe(channel)
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-channel:
			fmt.Fprintf(w, "event: changed\ndata: {\"at\":%q}\n\n", time.Now().UTC().Format(time.RFC3339Nano))
			flusher.Flush()
		}
	}
}

func (a *Controller) changed() {
	if a.profile == "dual" {
		if err := a.syncDualConnection(context.Background()); err != nil {
			a.logger.Error("sync dual-role connection", "error", err)
		}
	}
	a.hub.broadcast()
}

func configFromRun(run store.Run) (Config, error) {
	var config Config
	if err := json.Unmarshal(run.Config, &config); err != nil {
		return config, err
	}
	// A run saved before these defaults existed keeps its own values; only blanks are filled.
	if config.EvoltVersionsURL == "" {
		config.EvoltVersionsURL = defaults.EvoltVersionsURL()
	}
	if config.PartnerPublicURL == "" {
		config.PartnerPublicURL = defaults.PartnerPublicURL()
	}
	return config, nil
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nilResponseWriter{}, r.Body, ocpi.MaxBody))
	decoder.UseNumber()
	return decoder.Decode(target)
}

type nilResponseWriter struct{}

func (nilResponseWriter) Header() http.Header       { return http.Header{} }
func (nilResponseWriter) Write([]byte) (int, error) { return 0, nil }
func (nilResponseWriter) WriteHeader(int)           {}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, message string, err error) {
	writeJSON(w, status, apiError{Message: message, Detail: err.Error()})
}

type eventHub struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
}

func newEventHub() *eventHub { return &eventHub{clients: map[chan struct{}]struct{}{}} }
func (h *eventHub) subscribe() chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{}, 1)
	h.clients[ch] = struct{}{}
	return ch
}
func (h *eventHub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, ch)
	close(ch)
}
func (h *eventHub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
