package cpo

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/defaults"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpicore/rules"
)

type Config struct {
	EvoltVersionsURL       string  `json:"evolt_versions_url"`
	PartnerPublicURL       string  `json:"partner_public_url"`
	CountryCode            string  `json:"country_code"`
	PartyID                string  `json:"party_id"`
	BusinessName           string  `json:"business_name"`
	RawTokenCompatibility  bool    `json:"raw_token_compatibility"`
	AutoCommandFlow        bool    `json:"auto_command_flow"`
	TestIdleFee            bool    `json:"test_idle_fee"`
	IdleUnplugAfterSeconds int     `json:"idle_unplug_after_seconds"`
	AutoMeterRateKWh       float64 `json:"auto_meter_rate_kwh"`
	// ParkingAtUnplugOnly holds a parked Session's PARKING_TIME and cost until the car leaves instead of
	// reporting them while it stays plugged in; OCPI 2.2.1 §9.3.1 leaves that choice to the CPO.
	ParkingAtUnplugOnly bool `json:"parking_at_unplug_only"`
	// StopAtMaxPrice ends a Session once its total reaches the Tariff's max_price and sends the CDR: while
	// charging, from Auto Command Flow or the page's meter alike, with no idle time; while parked, as an unplug. By default the stop lands on the meter
	// report that saw the total there, as a backend pricing periodic meter values does; StopAtMaxPriceExact
	// stops at the moment the total met it instead, as a charger pricing the charge itself does.
	StopAtMaxPrice      bool `json:"stop_at_max_price"`
	StopAtMaxPriceExact bool `json:"stop_at_max_price_exact"`
	// MaxPriceMode is how the partner caps a session at max_price: FREEZE_ALL (default) freezes the total and
	// every line where the first side met it, TOTAL_ONLY freezes the total there but lets the lines run on,
	// PER_SIDE caps the excl and incl totals apart at their own bound and lets the lines run on.
	MaxPriceMode string `json:"max_price_mode,omitempty"`
	// PeriodIntervalSeconds sets the Charging Period cadence; 0 defaults to 15 seconds.
	// Intermediate ticks only update kwh and total_cost.
	PeriodIntervalSeconds int `json:"period_interval_seconds"`
	// SendIntervalSeconds is how often a running meter or a parked Session sends its Session PATCH; 0 means
	// the default 15 seconds.
	SendIntervalSeconds int            `json:"send_interval_seconds"`
	RemoteIdentity      map[string]any `json:"remote_identity,omitempty"`
}

type Endpoint struct {
	Identifier string `json:"identifier"`
	Role       string `json:"role"`
	URL        string `json:"url"`
}

type Record struct {
	Body        map[string]any `json:"body"`
	Snapshot    map[string]any `json:"-"`
	Delivery    string         `json:"delivery"`
	Findings    []ocpi.Finding `json:"findings"`
	CDRLocation string         `json:"cdr_location,omitempty"`
	UpdatedAt   string         `json:"updated_at"`
}

type Exchange struct {
	ID              string         `json:"id"`
	Direction       string         `json:"direction"`
	Module          string         `json:"module"`
	Method          string         `json:"method"`
	URL             string         `json:"url"`
	HTTPStatus      int            `json:"http_status"`
	OCPIStatusCode  int            `json:"ocpi_status_code,omitempty"`
	RequestHeaders  map[string]any `json:"request_headers"`
	RequestBody     any            `json:"request_body,omitempty"`
	ResponseHeaders map[string]any `json:"response_headers"`
	ResponseBody    any            `json:"response_body,omitempty"`
	Findings        []ocpi.Finding `json:"findings"`
	DurationMS      int64          `json:"duration_ms"`
	CreatedAt       string         `json:"created_at"`
}

type CommandRecord struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Status      string         `json:"status"`
	Request     map[string]any `json:"request"`
	ResponseURL string         `json:"response_url"`
	Result      map[string]any `json:"result,omitempty"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
}

type memoryState struct {
	Config       Config
	Status       string
	RunID        string
	TokenA       string
	InitialToken string
	TokenB       string
	StagedTokenB string
	TokenC       string
	Endpoints    []Endpoint
	Objects      map[string]map[string]*Record
	Exchanges    []Exchange
	Commands     map[string]*CommandRecord
	Credentials  map[string]any
}

type Controller struct {
	mu               sync.RWMutex
	client           *http.Client
	defaultPublicURL string
	evoltVersionsURL string
	mountPath        string
	state            memoryState
	subscribers      map[chan struct{}]struct{}
	repository       persistence
	profile          string
	persistenceError string
	saveMu           sync.Mutex
	saveDelay        time.Duration
	saves            chan struct{}
	stopSaving       chan struct{}
	savingDone       chan struct{}
	stopSavingOnce   sync.Once
	autoMu           sync.Mutex
	autoRuns         map[string]*autoMeterRun
	idleRuns         map[string]*autoMeterRun
	unplugging       map[string]bool
}

func New(defaultPublicURL string) *Controller {
	c := &Controller{
		client:           &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		defaultPublicURL: defaultPublicURL,
		evoltVersionsURL: defaults.EvoltVersionsURL(),
		subscribers:      map[chan struct{}]struct{}{},
		autoRuns:         map[string]*autoMeterRun{},
		idleRuns:         map[string]*autoMeterRun{},
		unplugging:       map[string]bool{},
		profile:          "cpo",
		saveDelay:        defaultSaveDelay,
	}
	c.reset(true)
	return c
}

func (c *Controller) RegisterRoutes(mux *http.ServeMux) {
	c.RegisterRoutesWithConnectionHandlers(mux, nil)
}

func (c *Controller) RegisterRoutesWithConnectionHandlers(mux *http.ServeMux, handlers map[string]http.HandlerFunc) {
	connection := func(name string, fallback http.HandlerFunc) http.HandlerFunc {
		if handler := handlers[name]; handler != nil {
			return handler
		}
		return fallback
	}
	mux.HandleFunc("GET /api/cpo/state", c.apiState)
	mux.HandleFunc("GET /api/cpo/db", c.apiDatabase)
	mux.HandleFunc("GET /api/cpo/tokens", c.apiTokens)
	mux.HandleFunc("GET /api/cpo/export", c.apiExport)
	mux.HandleFunc("PUT /api/cpo/config", connection("config", c.apiConfig))
	mux.HandleFunc("PUT /api/cpo/automation", c.apiAutomation)
	mux.HandleFunc("POST /api/cpo/reset", connection("reset", c.apiReset))
	mux.HandleFunc("POST /api/cpo/seeds", c.apiSeeds)
	mux.HandleFunc("POST /api/cpo/initial-token", connection("initial-token", c.apiInitialToken))
	mux.HandleFunc("GET /api/cpo/events", c.apiEvents)
	mux.HandleFunc("PUT /api/cpo/objects/{module}/{id}", c.apiSaveObject)
	mux.HandleFunc("POST /api/cpo/validate/{module}", c.apiValidateObject)
	mux.HandleFunc("DELETE /api/cpo/objects/{module}/{id}", c.apiDeleteObject)
	mux.HandleFunc("POST /api/cpo/cdrs/{id}/{action}", c.apiCDRCorrectionDraft)
	mux.HandleFunc("POST /api/cpo/actions/discover", connection("discover", c.apiDiscover))
	mux.HandleFunc("POST /api/cpo/actions/register", connection("register", c.apiRegister))
	mux.HandleFunc("POST /api/cpo/actions/credentials/{action}", connection("credentials", c.apiCredentialsAction))
	mux.HandleFunc("POST /api/cpo/actions/pull-tokens", c.apiPullTokens)
	mux.HandleFunc("POST /api/cpo/actions/preview", c.apiPreview)
	mux.HandleFunc("POST /api/cpo/actions/send", c.apiSend)
	mux.HandleFunc("POST /api/cpo/actions/session/{action}", c.apiSessionAction)
	mux.HandleFunc("GET /api/cpo/actions/session/{id}/parking", c.apiParkingPreview)
	mux.HandleFunc("GET /api/cpo/actions/session/{id}/price", c.apiChargingPreview)
	mux.HandleFunc("POST /api/cpo/actions/evse-status", c.apiEVSEStatus)
	mux.HandleFunc("POST /api/cpo/actions/command-result/{id}", c.apiCommandResult)
	// Tokens are keyed by country/party/uid/type, so the record id spans several path segments.

	protocol := func(name string, fallback http.Handler) http.Handler {
		if handler := handlers[name]; handler != nil {
			return handler
		}
		return fallback
	}
	mux.Handle("GET /ocpi/cpo/versions", protocol("protocol-versions", c.inbound("versions", false, c.versions)))
	mux.Handle("GET /ocpi/cpo/2.2.1", protocol("protocol-details", c.inbound("versions", false, c.versionDetails)))
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		mux.Handle(method+" /ocpi/cpo/2.2.1/credentials", protocol("protocol-credentials", c.inbound("credentials", false, c.credentials)))
	}
	for _, module := range []string{"locations", "tariffs", "sessions", "cdrs"} {
		mux.Handle("GET /ocpi/cpo/2.2.1/"+module, c.inbound(module, true, c.senderGet))
		mux.Handle("GET /ocpi/cpo/2.2.1/"+module+"/", c.inbound(module, true, c.senderGet))
	}
	mux.Handle("PUT /ocpi/cpo/2.2.1/sessions/{session_id}/charging_preferences", c.inbound("sessions", true, c.sessionChargingPreferences))
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch} {
		mux.Handle(method+" /ocpi/cpo/2.2.1/tokens/{country_code}/{party_id}/{uid}", c.inbound("tokens", true, c.tokensReceiver))
	}
	mux.Handle("POST /ocpi/cpo/2.2.1/commands/{command}", c.inbound("commands", true, c.commandsReceiver))
}

func (c *Controller) reset(withSeeds bool) {
	c.state = memoryState{
		Config: Config{EvoltVersionsURL: c.evoltVersionsURL, PartnerPublicURL: strings.TrimRight(c.defaultPublicURL, "/"), CountryCode: "TH", PartyID: "TST", BusinessName: "Evolt Partner CPO Mock", AutoMeterRateKWh: 7.2},
		Status: "SETUP", RunID: ocpi.NewID(), Endpoints: []Endpoint{}, Exchanges: []Exchange{}, Objects: emptyObjects(), Commands: map[string]*CommandRecord{}, Credentials: map[string]any{},
	}
	if withSeeds {
		loadSeeds(&c.state)
	}
}

func emptyObjects() map[string]map[string]*Record {
	return map[string]map[string]*Record{"locations": {}, "tariffs": {}, "sessions": {}, "cdrs": {}, "tokens": {}}
}

// SetMountPath tells the controller where its page lives, so a typed Partner public URL stays on it.
func (c *Controller) SetMountPath(mountPath string) {
	c.mu.Lock()
	c.mountPath = mountPath
	c.mu.Unlock()
}

func (c *Controller) StageInboundToken(token string) {
	c.mu.Lock()
	c.state.StagedTokenB = token
	c.mu.Unlock()
}

func (c *Controller) SyncConnection(config Config, status, tokenA, initial, tokenB, tokenC string, endpoints []Endpoint) {
	c.mu.Lock()
	config.AutoCommandFlow = c.state.Config.AutoCommandFlow
	config.TestIdleFee = c.state.Config.TestIdleFee
	config.IdleUnplugAfterSeconds = c.state.Config.IdleUnplugAfterSeconds
	config.AutoMeterRateKWh = c.state.Config.AutoMeterRateKWh
	config.ParkingAtUnplugOnly = c.state.Config.ParkingAtUnplugOnly
	config.StopAtMaxPrice = c.state.Config.StopAtMaxPrice
	config.StopAtMaxPriceExact = c.state.Config.StopAtMaxPriceExact
	config.MaxPriceMode = c.state.Config.MaxPriceMode
	config.PeriodIntervalSeconds = c.state.Config.PeriodIntervalSeconds
	config.SendIntervalSeconds = c.state.Config.SendIntervalSeconds
	if reflect.DeepEqual(c.state.Config, config) && c.state.Status == status && c.state.TokenA == tokenA && c.state.InitialToken == initial && c.state.TokenB == tokenB && c.state.TokenC == tokenC && reflect.DeepEqual(c.state.Endpoints, endpoints) {
		c.mu.Unlock()
		return
	}
	previousOwner := rules.Owner{CountryCode: c.state.Config.CountryCode, PartyID: c.state.Config.PartyID}
	c.state.Config = config
	c.rebaseDraftOwner(previousOwner)
	c.state.Status = status
	c.state.TokenA = tokenA
	c.state.InitialToken = initial
	c.state.TokenB = tokenB
	c.state.TokenC = tokenC
	c.state.Endpoints = endpoints
	c.mu.Unlock()
	c.changed()
}

func (c *Controller) ResetData(ctx context.Context) error {
	c.stopAllAutoRuns()
	c.stopAllIdleRuns()
	if err := c.replaceRun(ctx); err != nil {
		return err
	}
	c.changed()
	return nil
}

// replaceRun holds saveMu so a queued save of the old run cannot land after its delete.
func (c *Controller) replaceRun(ctx context.Context) error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.RLock()
	runID := c.state.RunID
	c.mu.RUnlock()
	if c.repository != nil {
		// A run replaced less than saveDelay ago has no row yet, so there is nothing to delete.
		if err := c.repository.DeleteCPO(ctx, runID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	c.mu.Lock()
	c.reset(true)
	c.mu.Unlock()
	return nil
}

func (c *Controller) apiState(w http.ResponseWriter, _ *http.Request) {
	// Read before c.mu: the meter loops take autoMu without holding c.mu.
	nextSends := c.nextSendTimes()
	c.mu.RLock()
	defer c.mu.RUnlock()
	objects := map[string][]map[string]any{}
	for module, records := range c.state.Objects {
		ids := make([]string, 0, len(records))
		for id := range records {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			record := records[id]
			row := map[string]any{"id": id, "body": record.Body, "delivery": record.Delivery, "findings": record.Findings, "cdr_location": record.CDRLocation, "updated_at": record.UpdatedAt}
			if module == "sessions" {
				// Tells the UI whether a manual stop parks the Session or closes it right away.
				row["bills_parking"] = c.sessionBillsParkingLocked(id)
				// Set while the server sends this Session's updates on a timer, for the page's countdown.
				if at, ok := nextSends[id]; ok {
					row["next_send_at"] = ocpi.FormatTime(at)
				}
			}
			objects[module] = append(objects[module], row)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": c.state.RunID, "status": c.state.Status, "config": c.state.Config, "endpoints": c.state.Endpoints,
		"objects": objects, "commands": commandList(c.state.Commands), "exchanges": c.state.Exchanges,
		"secrets": map[string]any{"evolt_token_a": c.state.TokenA != "", "partner_initial_token_a": c.state.InitialToken != "", "partner_issued_token": c.state.TokenB != "", "evolt_issued_token": c.state.TokenC != ""},
		"storage": map[string]any{"mode": storageMode(c.repository), "persistent": c.repository != nil, "reset_on_restart": c.repository == nil, "error": c.persistenceError},
	})
}

func (c *Controller) apiInitialToken(w http.ResponseWriter, _ *http.Request) {
	token := ocpi.NewToken()
	c.mu.Lock()
	if c.state.Status == "REGISTERED" {
		c.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("reset or unregister before starting a new registration"))
		return
	}
	c.state.InitialToken = token
	versionsURL := strings.TrimRight(c.state.Config.PartnerPublicURL, "/") + "/ocpi/cpo/versions"
	c.mu.Unlock()
	c.changed()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "versions_url": versionsURL, "shown_once": true})
}

func (c *Controller) apiDatabase(w http.ResponseWriter, r *http.Request) {
	if c.repository == nil {
		writeJSON(w, http.StatusOK, map[string]any{"mode": "memory-test", "tables": map[string]any{}})
		return
	}
	c.mu.RLock()
	runID := c.state.RunID
	c.mu.RUnlock()
	tables, err := c.repository.CPOTables(r.Context(), runID, r.URL.Query().Get("reveal_secrets") == "1")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	result := map[string]any{}
	for name, raw := range tables {
		var rows any
		if err := json.Unmarshal(raw, &rows); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		result[name] = rows
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": "postgresql", "run_id": runID, "tables": result})
}

func (c *Controller) apiExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Disposition", `attachment; filename="ocpi-outbound-black-box-report.json"`)
	c.apiState(w, r)
}

func (c *Controller) apiConfig(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Config
		TokenA string `json:"token_a"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c.mu.Lock()
	previousOwner := rules.Owner{CountryCode: c.state.Config.CountryCode, PartyID: c.state.Config.PartyID}
	if input.EvoltVersionsURL != "" {
		c.state.Config.EvoltVersionsURL = strings.TrimSpace(input.EvoltVersionsURL)
	}
	if input.PartnerPublicURL != "" {
		c.state.Config.PartnerPublicURL = defaults.PublicURLAt(input.PartnerPublicURL, c.mountPath)
	}
	if input.CountryCode != "" {
		c.state.Config.CountryCode = strings.ToUpper(strings.TrimSpace(input.CountryCode))
	}
	if input.PartyID != "" {
		c.state.Config.PartyID = strings.ToUpper(strings.TrimSpace(input.PartyID))
	}
	c.rebaseDraftOwner(previousOwner)
	if input.BusinessName != "" {
		c.state.Config.BusinessName = strings.TrimSpace(input.BusinessName)
	}
	c.state.Config.RawTokenCompatibility = input.RawTokenCompatibility
	if input.TokenA != "" {
		c.state.TokenA = input.TokenA
	}
	c.mu.Unlock()
	c.changed()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (c *Controller) apiReset(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Confirmation string `json:"confirmation"`
	}
	if err := decodeJSON(r, &input); err != nil || input.Confirmation != "RESET" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("confirmation must be RESET"))
		return
	}
	if err := c.ResetData(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "seeded": true, "evolt_console_credentials_cleared": false})
}

func (c *Controller) apiSeeds(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	loadSeeds(&c.state)
	c.mu.Unlock()
	c.changed()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (c *Controller) apiSaveObject(w http.ResponseWriter, r *http.Request) {
	module, id := r.PathValue("module"), r.PathValue("id")
	if !supportedModule(module) || id == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("unsupported object path"))
		return
	}
	var body map[string]any
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c.mu.Lock()
	if module == "cdrs" {
		if body["id"] != id {
			c.mu.Unlock()
			writeError(w, http.StatusBadRequest, fmt.Errorf("CDR body id must match record id"))
			return
		}
		if previous := c.state.Objects[module][id]; previous != nil && previous.Delivery == "SENT" {
			c.mu.Unlock()
			writeError(w, http.StatusConflict, fmt.Errorf("sent CDRs are immutable; create a Credit CDR"))
			return
		}
	}
	// An edit that kept the old last_updated would reach the peer as no change at all.
	if previous := c.state.Objects[module][id]; previous != nil && contentChangedKeepingTimestamp(previous.Body, body) {
		body["last_updated"] = now()
	}
	owner := rules.Owner{CountryCode: c.state.Config.CountryCode, PartyID: c.state.Config.PartyID}
	findings := rules.Validate(module, body, owner)
	delivery := "NOT_SENT"
	var snapshot map[string]any
	if previous := c.state.Objects[module][id]; previous != nil {
		snapshot = previous.Snapshot
		if previous.Delivery == "SENT" {
			delivery = "CHANGED_AFTER_SEND"
		}
	}
	c.state.Objects[module][id] = &Record{Body: cloneMap(body), Snapshot: snapshot, Delivery: delivery, Findings: findings, UpdatedAt: now()}
	c.mu.Unlock()
	c.changed()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "findings": findings})
}

func (c *Controller) apiValidateObject(w http.ResponseWriter, r *http.Request) {
	module := r.PathValue("module")
	if !supportedModule(module) {
		writeError(w, http.StatusNotFound, fmt.Errorf("unsupported module"))
		return
	}
	var body map[string]any
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c.mu.RLock()
	owner := rules.Owner{CountryCode: c.state.Config.CountryCode, PartyID: c.state.Config.PartyID}
	c.mu.RUnlock()
	findings := rules.Validate(module, body, owner)
	writeJSON(w, http.StatusOK, map[string]any{"valid": !ocpi.HasErrors(findings), "findings": findings})
}

func (c *Controller) apiDeleteObject(w http.ResponseWriter, r *http.Request) {
	module, id := r.PathValue("module"), r.PathValue("id")
	if !supportedModule(module) {
		writeError(w, http.StatusNotFound, fmt.Errorf("unsupported module"))
		return
	}
	c.mu.Lock()
	if module == "cdrs" {
		if previous := c.state.Objects[module][id]; previous != nil && previous.Delivery == "SENT" {
			c.mu.Unlock()
			writeError(w, http.StatusConflict, fmt.Errorf("sent CDRs cannot be deleted"))
			return
		}
	}
	delete(c.state.Objects[module], id)
	c.mu.Unlock()
	c.changed()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (c *Controller) apiCDRCorrectionDraft(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	c.mu.Lock()
	source := c.state.Objects["cdrs"][id]
	if source == nil || source.Delivery != "SENT" {
		c.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("select a sent CDR first"))
		return
	}
	body := cloneMap(source.Body)
	cost, ok := body["total_cost"].(map[string]any)
	if !ok {
		c.mu.Unlock()
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("source CDR has no total_cost"))
		return
	}
	switch action {
	case "credit-draft":
		if body["credit"] == true || body["id"] != id {
			c.mu.Unlock()
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("select an original sent CDR"))
			return
		}
		for _, record := range c.state.Objects["cdrs"] {
			if record.Body["credit"] == true && record.Body["credit_reference_id"] == id {
				c.mu.Unlock()
				writeError(w, http.StatusConflict, fmt.Errorf("a Credit CDR already exists for this CDR"))
				return
			}
		}
		if err := negateCDRCost(cost); err != nil {
			c.mu.Unlock()
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		body["credit"], body["credit_reference_id"] = true, id
	case "replacement-draft":
		if body["credit"] != true {
			c.mu.Unlock()
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("select a sent Credit CDR"))
			return
		}
		originalID, _ := body["credit_reference_id"].(string)
		if originalID == "" || c.state.Objects["cdrs"][originalID] == nil || c.state.Objects["cdrs"][originalID].Delivery != "SENT" {
			c.mu.Unlock()
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("original CDR is not available"))
			return
		}
		if err := negateCDRCost(cost); err != nil {
			c.mu.Unlock()
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		delete(body, "credit")
		delete(body, "credit_reference_id")
	default:
		c.mu.Unlock()
		writeError(w, http.StatusNotFound, fmt.Errorf("unsupported CDR action"))
		return
	}
	newID := ""
	if action == "credit-draft" && len(id)+2 <= 39 {
		candidate := id + "-C"
		available := true
		for existingID := range c.state.Objects["cdrs"] {
			if strings.EqualFold(existingID, candidate) {
				available = false
				break
			}
		}
		if available {
			newID = candidate
		}
	}
	if newID == "" {
		newID = ocpi.NewID()
		for c.state.Objects["cdrs"][newID] != nil {
			newID = ocpi.NewID()
		}
	}
	body["id"] = newID
	if action == "replacement-draft" {
		body["last_updated"] = now()
	}
	owner := rules.Owner{CountryCode: c.state.Config.CountryCode, PartyID: c.state.Config.PartyID}
	record := &Record{Body: body, Delivery: "NOT_SENT", Findings: rules.Validate("cdrs", body, owner), UpdatedAt: now()}
	c.state.Objects["cdrs"][newID] = record
	c.mu.Unlock()
	c.changed()
	writeJSON(w, http.StatusCreated, map[string]any{"id": newID, "object": record})
}

func negateCDRCost(cost map[string]any) error {
	if _, ok := cost["excl_vat"].(float64); !ok {
		return fmt.Errorf("total_cost.excl_vat must be a number")
	}
	for _, field := range []string{"excl_vat", "incl_vat"} {
		if value, exists := cost[field]; exists {
			number, ok := value.(float64)
			if !ok {
				return fmt.Errorf("total_cost.%s must be a number", field)
			}
			cost[field] = -number
		}
	}
	return nil
}

func (c *Controller) apiDiscover(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	config, token := c.state.Config, c.state.TokenA
	c.mu.RUnlock()
	if config.EvoltVersionsURL == "" || token == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("save Evolt Versions URL and Token A first"))
		return
	}
	var versions []struct{ Version, URL string }
	result, err := c.doOCPI(r.Context(), "versions", http.MethodGet, config.EvoltVersionsURL, token, false, nil)
	if err != nil || result.Envelope == nil {
		writeError(w, http.StatusBadGateway, firstError(err, "invalid Versions response"))
		return
	}
	if err := decodeData(result.Envelope, &versions); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	detailsURL := ""
	for _, version := range versions {
		if version.Version == ocpi.Version {
			detailsURL = version.URL
		}
	}
	if detailsURL == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("Evolt did not advertise OCPI 2.2.1"))
		return
	}
	var details struct {
		Version   string     `json:"version"`
		Endpoints []Endpoint `json:"endpoints"`
	}
	detailResult, err := c.doOCPI(r.Context(), "versions", http.MethodGet, detailsURL, token, false, nil)
	if err != nil || detailResult.Envelope == nil {
		writeError(w, http.StatusBadGateway, firstError(err, "invalid Version Details response"))
		return
	}
	if err := decodeData(detailResult.Envelope, &details); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	c.mu.Lock()
	c.state.Endpoints = details.Endpoints
	c.state.Status = "DISCOVERED"
	c.mu.Unlock()
	c.changed()
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions, "details": details, "readiness": readiness(details.Endpoints)})
}

func (c *Controller) apiRegister(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	if c.state.TokenB == "" {
		c.state.TokenB = ocpi.NewToken()
	}
	config, tokenA, tokenB := c.state.Config, c.state.TokenA, c.state.TokenB
	credentialsURL := endpointURL(c.state.Endpoints, "credentials", "")
	c.mu.Unlock()
	if credentialsURL == "" || tokenA == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("run Discover before Register"))
		return
	}
	payload := credentialsPayload(config, tokenB)
	result, err := c.doOCPI(r.Context(), "credentials", http.MethodPost, credentialsURL, tokenA, false, payload)
	if err != nil || result.Envelope == nil {
		writeError(w, http.StatusBadGateway, firstError(err, "invalid Credentials response"))
		return
	}
	var remote struct {
		Token string           `json:"token"`
		URL   string           `json:"url"`
		Roles []map[string]any `json:"roles"`
	}
	if err := decodeData(result.Envelope, &remote); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if remote.Token == "" {
		writeError(w, http.StatusBadGateway, fmt.Errorf("Credentials response is missing token"))
		return
	}
	c.mu.Lock()
	c.state.TokenC = remote.Token
	c.state.Credentials = map[string]any{"url": remote.URL, "roles": remote.Roles}
	for _, role := range remote.Roles {
		if strings.EqualFold(fmt.Sprint(role["role"]), "EMSP") {
			c.state.Config.RemoteIdentity = cloneMap(role)
			break
		}
	}
	c.state.Status = "REGISTERED"
	c.mu.Unlock()
	c.changed()
	writeJSON(w, http.StatusCreated, map[string]any{"status": "REGISTERED", "remote": map[string]any{"url": remote.URL, "roles": remote.Roles, "token": remote.Token}})
}

func (c *Controller) apiCredentialsAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	c.mu.RLock()
	config, token := c.state.Config, c.state.TokenC
	endpoint := endpointURL(c.state.Endpoints, "credentials", "")
	c.mu.RUnlock()
	if endpoint == "" || token == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("registration is not complete"))
		return
	}
	method := http.MethodGet
	var payload any
	newTokenB := ""
	switch action {
	case "get":
	case "rotate":
		method = http.MethodPut
		newTokenB = ocpi.NewToken()
		payload = credentialsPayload(config, newTokenB)
		c.mu.Lock()
		if c.state.StagedTokenB != "" {
			c.mu.Unlock()
			writeError(w, http.StatusConflict, fmt.Errorf("credentials rotation is already in progress"))
			return
		}
		// Evolt discovers our endpoints with the token in this PUT before it sends the response.
		c.state.StagedTokenB = newTokenB
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			if c.state.StagedTokenB == newTokenB {
				c.state.StagedTokenB = ""
			}
			c.mu.Unlock()
		}()
	case "unregister":
		method = http.MethodDelete
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown credentials action"))
		return
	}
	result, err := c.doOCPI(r.Context(), "credentials", method, endpoint, token, false, payload)
	if err != nil || result.Envelope == nil {
		writeError(w, http.StatusBadGateway, firstError(err, "invalid Credentials response"))
		return
	}
	if result.Envelope.StatusCode >= 2000 {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("remote returned OCPI %d: %s", result.Envelope.StatusCode, result.Envelope.StatusMessage))
		return
	}
	if action == "rotate" {
		var remote struct {
			Token string `json:"token"`
		}
		if err := decodeData(result.Envelope, &remote); err != nil || remote.Token == "" {
			writeError(w, http.StatusBadGateway, firstError(err, "Credentials response is missing token"))
			return
		}
		c.mu.Lock()
		c.state.TokenB, c.state.StagedTokenB, c.state.TokenC = newTokenB, "", remote.Token
		c.state.Status = "REGISTERED"
		c.mu.Unlock()
	}
	if action == "unregister" {
		c.mu.Lock()
		c.state.TokenB, c.state.TokenC, c.state.Status = "", "", "UNREGISTERED"
		c.mu.Unlock()
	}
	c.changed()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "response": result.Envelope})
}

func (c *Controller) apiPullTokens(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	endpoint := endpointURL(c.state.Endpoints, "tokens", "SENDER")
	token := c.state.TokenC
	remote := cloneMap(c.state.Config.RemoteIdentity)
	c.mu.RUnlock()
	if endpoint == "" || token == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("registration is incomplete or Evolt did not advertise tokens SENDER"))
		return
	}

	const pageSize = 10
	pulled := map[string]*Record{}
	pages := 0
	finished := false
	const maxPages = 10000
	for offset := 0; pages < maxPages; {
		target, err := tokenPageURL(endpoint, offset, pageSize)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		response, err := c.doOCPI(r.Context(), "tokens", http.MethodGet, target, token, true, nil)
		if err != nil || response.Envelope == nil {
			writeError(w, http.StatusBadGateway, firstError(err, "invalid Tokens response"))
			return
		}
		if response.Envelope.StatusCode >= 2000 {
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("Evolt returned OCPI %d: %s", response.Envelope.StatusCode, response.Envelope.StatusMessage))
			return
		}
		var items []map[string]any
		if err := decodeData(response.Envelope, &items); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Errorf("invalid Token list: %w", err))
			return
		}
		pages++
		for _, item := range items {
			countryCode := strings.ToUpper(fmt.Sprint(item["country_code"]))
			partyID := strings.ToUpper(fmt.Sprint(item["party_id"]))
			uid := fmt.Sprint(item["uid"])
			tokenType := strings.ToUpper(fmt.Sprint(item["type"]))
			if tokenType == "" {
				tokenType = "RFID"
			}
			key := tokenObjectID(countryCode, partyID, uid, tokenType)
			owner := rules.Owner{CountryCode: strings.ToUpper(fmt.Sprint(remote["country_code"])), PartyID: strings.ToUpper(fmt.Sprint(remote["party_id"]))}
			pulled[key] = &Record{Body: cloneMap(item), Delivery: "PULLED", Findings: rules.Validate("tokens", item, owner), UpdatedAt: now()}
		}

		if len(items) < pageSize {
			finished = true
			break
		}
		offset += len(items)
		if total, err := strconv.Atoi(response.Headers.Get("X-Total-Count")); err == nil && offset >= total {
			finished = true
			break
		}
	}
	if !finished {
		writeError(w, http.StatusBadGateway, fmt.Errorf("Tokens pagination exceeded %d pages", maxPages))
		return
	}

	c.mu.Lock()
	invalidated := 0
	for key, record := range c.state.Objects["tokens"] {
		if _, exists := pulled[key]; exists {
			continue
		}
		stale := cloneMap(record.Body)
		stale["valid"] = false
		c.state.Objects["tokens"][key] = &Record{Body: stale, Delivery: "INVALIDATED_BY_PULL", Findings: record.Findings, UpdatedAt: now()}
		invalidated++
	}
	for key, record := range pulled {
		c.state.Objects["tokens"][key] = record
	}
	c.mu.Unlock()
	c.changed()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pulled": len(pulled), "invalidated": invalidated, "pages": pages, "page_size": pageSize})
}

func tokenPageURL(endpoint string, offset, limit int) (string, error) {
	target, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid tokens endpoint: %w", err)
	}
	query := target.Query()
	query.Set("offset", strconv.Itoa(offset))
	query.Set("limit", strconv.Itoa(limit))
	target.RawQuery = query.Encode()
	return target.String(), nil
}

type sendInput struct {
	Module string         `json:"module"`
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Body   map[string]any `json:"body,omitempty"`
}

type preview struct {
	Module  string         `json:"module"`
	Method  string         `json:"method"`
	URL     string         `json:"url"`
	Headers map[string]any `json:"headers"`
	Body    any            `json:"body,omitempty"`
	Curl    string         `json:"curl"`
}

func (c *Controller) apiPreview(w http.ResponseWriter, r *http.Request) {
	var input sendInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	request, err := c.buildSend(input)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, request)
}

func (c *Controller) apiSend(w http.ResponseWriter, r *http.Request) {
	var input sendInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	request, err := c.buildSend(input)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	c.mu.RLock()
	token := c.state.TokenC
	c.mu.RUnlock()
	result, err := c.doOCPI(r.Context(), request.Module, request.Method, request.URL, token, true, request.Body)
	if err != nil || result.Envelope == nil {
		c.markDelivery(input.Module, input.ID, "FAILED", "")
		writeError(w, http.StatusBadGateway, firstError(err, "invalid OCPI response"))
		return
	}
	if result.Envelope.StatusCode >= 2000 || (input.Module == "cdrs" && (result.HTTPStatus < 200 || result.HTTPStatus >= 300 || result.Envelope.StatusCode != 1000)) {
		c.markDelivery(input.Module, input.ID, "FAILED", "")
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("Evolt returned HTTP %d, OCPI %d: %s", result.HTTPStatus, result.Envelope.StatusCode, result.Envelope.StatusMessage))
		return
	}
	cdrLocation := ""
	if input.Module == "cdrs" {
		cdrLocation = result.Headers.Get("Location")
		if request.Method == http.MethodPost && cdrLocation == "" {
			c.markDelivery(input.Module, input.ID, "FAILED", "")
			writeError(w, http.StatusBadGateway, fmt.Errorf("Evolt accepted CDR without a Location header"))
			return
		}
	}
	c.markDelivery(input.Module, input.ID, "SENT", cdrLocation)
	response := map[string]any{"ok": true, "http_status": result.HTTPStatus, "envelope": result.Envelope, "location": cdrLocation, "findings": result.Findings}
	if request.Module == "sessions" && request.Method == http.MethodPut {
		c.mu.RLock()
		record := c.state.Objects["sessions"][input.ID]
		active := record != nil && fieldText(record.Body, "status") == "ACTIVE"
		locationID, evseUID := "", ""
		if record != nil {
			locationID, evseUID = fieldText(record.Body, "location_id"), fieldText(record.Body, "evse_uid")
		}
		c.mu.RUnlock()
		if active {
			if err := c.ensureEVSEStatus(r.Context(), locationID, evseUID, "CHARGING"); err != nil {
				response["evse_status_error"] = err.Error()
			}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (c *Controller) buildSend(input sendInput) (preview, error) {
	input.Module = strings.ToLower(input.Module)
	input.Method = strings.ToUpper(input.Method)
	if input.Method == "" {
		input.Method = http.MethodPut
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state.TokenC == "" {
		return preview{}, fmt.Errorf("register credentials first")
	}
	endpoint := endpointURL(c.state.Endpoints, input.Module, "RECEIVER")
	if endpoint == "" {
		return preview{}, fmt.Errorf("Evolt did not advertise %s RECEIVER", input.Module)
	}
	record := c.state.Objects[input.Module][input.ID]
	if record == nil {
		return preview{}, fmt.Errorf("%s %q not found", input.Module, input.ID)
	}
	if input.Module == "cdrs" {
		if input.Method != http.MethodPost && input.Method != http.MethodGet {
			return preview{}, fmt.Errorf("CDRs support POST and GET only")
		}
		if input.Method == http.MethodPost && record.Delivery == "SENT" {
			return preview{}, fmt.Errorf("sent CDRs are immutable; create a Credit CDR")
		}
		if input.Body != nil {
			return preview{}, fmt.Errorf("save the CDR draft before sending")
		}
	}
	body := any(record.Body)
	if input.Body != nil {
		body = input.Body
	}
	target := strings.TrimRight(endpoint, "/")
	if input.Module != "cdrs" {
		target += "/" + url.PathEscape(c.state.Config.CountryCode) + "/" + url.PathEscape(c.state.Config.PartyID) + "/" + url.PathEscape(input.ID)
	}
	if input.Method == http.MethodGet && input.Module == "cdrs" {
		if record.CDRLocation == "" {
			return preview{}, fmt.Errorf("CDR response has no Location URL to verify")
		}
		target, body = record.CDRLocation, nil
	}
	if input.Method == http.MethodGet || input.Method == http.MethodDelete {
		body = nil
	}
	headers := c.previewHeaders(true, body != nil)
	return preview{Module: input.Module, Method: input.Method, URL: target, Headers: headers, Body: body, Curl: curlFor(input.Method, target, headers, body)}, nil
}

func (c *Controller) markDelivery(module, id, delivery, cdrLocation string) {
	c.mu.Lock()
	if record := c.state.Objects[module][id]; record != nil {
		record.Delivery = delivery
		if cdrLocation != "" {
			record.CDRLocation = cdrLocation
		}
	}
	c.mu.Unlock()
	c.changed()
}

type result struct {
	HTTPStatus int
	Envelope   *ocpi.Envelope
	Headers    http.Header
	Findings   []ocpi.Finding
}

func (c *Controller) doOCPI(ctx context.Context, module, method, target, token string, functional bool, payload any) (result, error) {
	started := time.Now()
	c.mu.RLock()
	config := c.state.Config
	c.mu.RUnlock()
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return result{}, fmt.Errorf("invalid endpoint URL")
	}
	var body io.Reader
	if payload != nil {
		raw, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return result{}, marshalErr
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return result{}, err
	}
	if config.RawTokenCompatibility {
		request.Header.Set("Authorization", "Token "+token)
	} else {
		request.Header.Set("Authorization", ocpi.EncodeCredential(token))
	}
	requestID, correlationID := ocpi.MessageHeaders()
	request.Header.Set("X-Request-ID", requestID)
	request.Header.Set("X-Correlation-ID", correlationID)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if functional {
		request.Header.Set("OCPI-from-country-code", config.CountryCode)
		request.Header.Set("OCPI-from-party-id", config.PartyID)
		request.Header.Set("OCPI-to-country-code", fmt.Sprint(config.RemoteIdentity["country_code"]))
		request.Header.Set("OCPI-to-party-id", fmt.Sprint(config.RemoteIdentity["party_id"]))
	}
	response, requestErr := c.client.Do(request)
	if requestErr != nil {
		c.addExchange(Exchange{ID: ocpi.NewID(), Direction: "OUTBOUND", Module: module, Method: method, URL: target, RequestHeaders: ocpi.Headers(request.Header), RequestBody: payload, ResponseHeaders: map[string]any{}, Findings: []ocpi.Finding{{Level: "error", Rule: "transport", Message: requestErr.Error()}}, DurationMS: time.Since(started).Milliseconds(), CreatedAt: now()})
		return result{}, requestErr
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, ocpi.MaxBody+1))
	if err != nil {
		return result{}, err
	}
	findings, envelope := ocpi.ValidateResponse(request.Header, response.Header, functional, raw, response.StatusCode)
	var responseBody any
	_ = json.Unmarshal(raw, &responseBody)
	status := 0
	if envelope != nil {
		status = envelope.StatusCode
	}
	c.addExchange(Exchange{ID: ocpi.NewID(), Direction: "OUTBOUND", Module: module, Method: method, URL: target, HTTPStatus: response.StatusCode, OCPIStatusCode: status, RequestHeaders: ocpi.Headers(request.Header), RequestBody: payload, ResponseHeaders: ocpi.Headers(response.Header), ResponseBody: responseBody, Findings: findings, DurationMS: time.Since(started).Milliseconds(), CreatedAt: now()})
	if envelope == nil {
		return result{HTTPStatus: response.StatusCode, Headers: response.Header.Clone(), Findings: findings}, fmt.Errorf("response is not an OCPI envelope")
	}
	return result{HTTPStatus: response.StatusCode, Envelope: envelope, Headers: response.Header.Clone(), Findings: findings}, nil
}

func (c *Controller) previewHeaders(functional, hasBody bool) map[string]any {
	token := c.state.TokenC
	authorization := ocpi.EncodeCredential(token)
	if c.state.Config.RawTokenCompatibility {
		authorization = "Token " + token
	}
	headers := map[string]any{"Authorization": authorization, "X-Request-ID": "<generated on send>", "X-Correlation-ID": "<generated on send>"}
	if hasBody {
		headers["Content-Type"] = "application/json"
	}
	if functional {
		headers["OCPI-from-country-code"] = c.state.Config.CountryCode
		headers["OCPI-from-party-id"] = c.state.Config.PartyID
		headers["OCPI-to-country-code"] = c.state.Config.RemoteIdentity["country_code"]
		headers["OCPI-to-party-id"] = c.state.Config.RemoteIdentity["party_id"]
	}
	return headers
}

func credentialsPayload(config Config, token string) map[string]any {
	return map[string]any{
		"token": token,
		"url":   strings.TrimRight(config.PartnerPublicURL, "/") + "/ocpi/cpo/versions",
		"roles": []map[string]any{{"role": "CPO", "party_id": config.PartyID, "country_code": config.CountryCode, "business_details": map[string]any{"name": config.BusinessName}}},
	}
}

func readiness(endpoints []Endpoint) map[string]bool {
	result := map[string]bool{}
	for _, module := range []string{"credentials", "locations", "tariffs", "sessions", "cdrs"} {
		role := "RECEIVER"
		if module == "credentials" {
			role = ""
		}
		result[module] = endpointURL(endpoints, module, role) != ""
	}
	return result
}

func endpointURL(endpoints []Endpoint, identifier, role string) string {
	for _, endpoint := range endpoints {
		if strings.EqualFold(endpoint.Identifier, identifier) && (role == "" || strings.EqualFold(endpoint.Role, role)) {
			return endpoint.URL
		}
	}
	return ""
}

func supportedModule(module string) bool {
	return module == "locations" || module == "tariffs" || module == "sessions" || module == "cdrs" || module == "tokens"
}

// contentChangedKeepingTimestamp reports an edit whose fields changed while last_updated did not.
func contentChangedKeepingTimestamp(previous, next map[string]any) bool {
	if fieldText(previous, "last_updated") != fieldText(next, "last_updated") {
		return false
	}
	before, after := cloneMap(previous), cloneMap(next)
	delete(before, "last_updated")
	delete(after, "last_updated")
	return !reflect.DeepEqual(before, after)
}

func cloneMap(value map[string]any) map[string]any {
	raw, _ := json.Marshal(value)
	var copy map[string]any
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func decodeData(envelope *ocpi.Envelope, target any) error {
	if envelope == nil {
		return fmt.Errorf("missing OCPI envelope")
	}
	if envelope.StatusCode < 1000 || envelope.StatusCode >= 2000 {
		return fmt.Errorf("OCPI %d: %s", envelope.StatusCode, envelope.StatusMessage)
	}
	raw, _ := json.Marshal(envelope.Data)
	return json.Unmarshal(raw, target)
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, ocpi.MaxBody+1))
	decoder.UseNumber()
	return decoder.Decode(target)
}

// apiTokens hands QA the tokens of the current registration so a request can be fired from Postman
// or curl without decrypting them out of the database. /api/* has no auth, so the console must stay on an internal network.
func (c *Controller) apiTokens(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	status, tokenA, initial, tokenB, tokenC := c.state.Status, c.state.TokenA, c.state.InitialToken, c.state.TokenB, c.state.TokenC
	c.mu.RUnlock()

	registrationToken := tokenA
	if registrationToken == "" {
		registrationToken = initial
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": status,
		// The one QA actually needs: what the partner presents when it calls Evolt.
		"partner_to_evolt": tokenEntry(tokenC, "Partner → Evolt",
			"Evolt เรียกใบนี้ว่า Token B · ใส่ในตัวแปร token_inbound ของ Postman collection Roaming Out"),
		"evolt_to_partner": tokenEntry(tokenB, "Evolt → Partner",
			"token ที่ mock ออกให้ Evolt ใช้ยิงเข้ามา · ใช้ตรวจว่า callback ของ Evolt ถือใบถูกไหม"),
		"registration": tokenEntry(registrationToken, "Handshake only",
			"Token A ใช้ได้เฉพาะช่วง registration หลัง REGISTERED แล้วจะใช้ไม่ได้"),
	})
}

// tokenEntry ships the raw token plus the header value it turns into, because OCPI sends
// Authorization: Token <base64(token)> and doing that by hand is where QA loses time.
func tokenEntry(token, direction, note string) map[string]any {
	entry := map[string]any{"direction": direction, "note": note, "present": token != ""}
	if token != "" {
		entry["token"] = token
		entry["authorization"] = "Token " + base64.StdEncoding.EncodeToString([]byte(token))
	}
	return entry
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func firstError(err error, fallback string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%s", fallback)
}

func now() string { return ocpi.FormatTime(time.Now()) }

func curlFor(method, target string, headers map[string]any, body any) string {
	parts := []string{"curl -X " + method + " '" + strings.ReplaceAll(target, "'", "'\\''") + "'"}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, "-H '"+key+": "+fmt.Sprint(headers[key])+"'")
	}
	if body != nil {
		raw, _ := json.Marshal(body)
		parts = append(parts, "--data '"+strings.ReplaceAll(string(raw), "'", "'\\''")+"'")
	}
	return strings.Join(parts, " \\\n  ")
}

func (c *Controller) addExchange(exchange Exchange) {
	c.mu.Lock()
	c.state.Exchanges = append([]Exchange{exchange}, c.state.Exchanges...)
	if len(c.state.Exchanges) > 300 {
		c.state.Exchanges = c.state.Exchanges[:300]
	}
	c.mu.Unlock()
	c.changed()
}

func (c *Controller) changed() {
	c.scheduleSave()
	c.notify()
}

func (c *Controller) notify() {
	c.mu.RLock()
	channels := make([]chan struct{}, 0, len(c.subscribers))
	for channel := range c.subscribers {
		channels = append(channels, channel)
	}
	c.mu.RUnlock()
	for _, channel := range channels {
		select {
		case channel <- struct{}{}:
		default:
		}
	}
}

func (c *Controller) apiEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("streaming unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	channel := make(chan struct{}, 1)
	c.mu.Lock()
	c.subscribers[channel] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.subscribers, channel)
		c.mu.Unlock()
	}()
	_, _ = io.WriteString(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	// A proxy drops a stream that stays silent; the comment line keeps it open without waking the page.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		case <-channel:
			_, _ = io.WriteString(w, "event: changed\ndata: {}\n\n")
			flusher.Flush()
		}
	}
}
