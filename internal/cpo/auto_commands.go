package cpo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

type autoMeterRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (c *Controller) apiAutomation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Enabled                bool    `json:"enabled"`
		TestIdleFee            bool    `json:"test_idle_fee"`
		IdleUnplugAfterSeconds int     `json:"idle_unplug_after_seconds"`
		Rate                   float64 `json:"rate_kwh_per_hour"`
		// Omitted fields keep their current value, so a client that does not know them changes nothing.
		ParkingAtUnplugOnly   *bool `json:"parking_at_unplug_only"`
		PeriodIntervalSeconds *int  `json:"period_interval_seconds"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if input.Rate < 0.001 || input.Rate > 350 {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("rate_kwh_per_hour must be between 0.001 and 350"))
		return
	}
	if input.IdleUnplugAfterSeconds < 0 || input.IdleUnplugAfterSeconds > 86400 {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("idle_unplug_after_seconds must be between 0 and 86400"))
		return
	}
	if input.PeriodIntervalSeconds != nil && (*input.PeriodIntervalSeconds < minPeriodInterval || *input.PeriodIntervalSeconds > maxPeriodInterval) {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("period_interval_seconds must be between %d and %d", minPeriodInterval, maxPeriodInterval))
		return
	}
	if !input.TestIdleFee {
		input.IdleUnplugAfterSeconds = 0
	}
	c.mu.Lock()
	c.state.Config.AutoCommandFlow = input.Enabled
	c.state.Config.TestIdleFee = input.TestIdleFee
	c.state.Config.IdleUnplugAfterSeconds = input.IdleUnplugAfterSeconds
	c.state.Config.AutoMeterRateKWh = input.Rate
	if input.ParkingAtUnplugOnly != nil {
		c.state.Config.ParkingAtUnplugOnly = *input.ParkingAtUnplugOnly
	}
	if input.PeriodIntervalSeconds != nil {
		c.state.Config.PeriodIntervalSeconds = *input.PeriodIntervalSeconds
	}
	config := c.state.Config
	c.mu.Unlock()
	c.changed()
	if input.Enabled {
		c.resumeAutoRuns()
	} else {
		c.stopAllAutoRuns()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": input.Enabled, "test_idle_fee": input.TestIdleFee, "idle_unplug_after_seconds": input.IdleUnplugAfterSeconds, "rate_kwh_per_hour": input.Rate, "parking_at_unplug_only": config.ParkingAtUnplugOnly, "period_interval_seconds": config.periodInterval().Seconds()})
}

func (c *Controller) autoCommand(commandID string) {
	timer := time.NewTimer(750 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	c.mu.RLock()
	command := c.state.Commands[commandID]
	enabled := c.state.Config.AutoCommandFlow
	c.mu.RUnlock()
	if command == nil {
		return
	}
	if !enabled {
		_ = c.sendCommandResult(context.Background(), commandID, "ACCEPTED")
		return
	}
	ctx := context.Background()
	switch command.Type {
	case "START_SESSION":
		c.autoStartCommand(ctx, command)
	case "STOP_SESSION":
		c.autoStopCommand(ctx, command)
	default:
		_ = c.sendCommandResult(ctx, commandID, "ACCEPTED")
	}
}

func (c *Controller) autoStartCommand(ctx context.Context, command *CommandRecord) {
	target, err := c.autoStartTarget(command)
	if err != nil {
		_ = c.sendCommandResult(ctx, command.ID, "FAILED")
		c.recordAutoError(command.ID, err)
		return
	}
	_ = c.sendCommandResult(ctx, command.ID, "ACCEPTED")
	result, err := c.callSessionAction(ctx, "create", map[string]any{"command_id": command.ID, "evse_uid": target["evse_uid"], "connector_id": target["connector_id"]})
	if err != nil {
		c.recordAutoError(command.ID, err)
		return
	}
	object, _ := result["object"].(map[string]any)
	sessionID := fieldText(object, "id")
	c.mu.Lock()
	if saved := c.state.Commands[command.ID]; saved != nil {
		if saved.Result == nil {
			saved.Result = map[string]any{}
		}
		saved.Result["session_id"] = sessionID
	}
	c.mu.Unlock()
	c.changed()
	if err := c.callSend(ctx, "sessions", sessionID, http.MethodPut); err != nil {
		c.recordAutoError(command.ID, err)
		return
	}
	if err := c.ensureEVSEStatus(ctx, fieldText(command.Request, "location_id"), target["evse_uid"], "CHARGING"); err != nil {
		c.recordAutoEVSEStatusError(command.ID, "CHARGING", err)
	}
	c.startAutoRun(sessionID)
}

func (c *Controller) autoStopCommand(ctx context.Context, command *CommandRecord) {
	sessionID := fieldText(command.Request, "session_id")
	_ = c.sendCommandResult(ctx, command.ID, "ACCEPTED")
	c.stopAutoRun(sessionID)
	target, ok := c.autoMeterTarget(sessionID, time.Now())
	if !ok {
		c.recordAutoError(command.ID, fmt.Errorf("Session is no longer active"))
		return
	}
	c.mu.RLock()
	testIdleFee := c.state.Config.TestIdleFee
	billsParking := c.sessionBillsParkingLocked(sessionID)
	c.mu.RUnlock()
	if !testIdleFee || !billsParking {
		if err := c.completeStoppedSession(ctx, sessionID, command.ID, target); err != nil {
			c.recordAutoError(command.ID, err)
		}
		return
	}
	if err := c.beginIdle(ctx, sessionID, &target); err != nil {
		c.recordAutoError(command.ID, err)
		return
	}
	c.startAutoRun(sessionID)
}

func (c *Controller) completeStoppedSession(ctx context.Context, sessionID, commandID string, target float64) error {
	c.mu.Lock()
	record := c.state.Objects["sessions"][sessionID]
	if record == nil || fieldText(record.Body, "status") != "ACTIVE" || record.Delivery != "SENT" || c.idleCommandLocked(sessionID) != nil {
		c.mu.Unlock()
		return fmt.Errorf("an ACTIVE, sent Session without idle time is required")
	}
	current := number(record.Body["kwh"])
	if target < current {
		c.mu.Unlock()
		return fmt.Errorf("final meter is below the current meter reading")
	}
	patch := c.meterUpdate(record, target-current, true, now(), true)
	cost, _ := record.Body["total_cost"].(map[string]any)
	price := sessionPrice{Priced: cost != nil, ExclVAT: number(cost["excl_vat"]), InclVAT: number(cost["incl_vat"])}
	c.mu.Unlock()
	c.changed()
	r := httptest.NewRequest(http.MethodPost, "/api/cpo/actions/session/complete", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	c.finishUnplug(w, r, sessionID, commandID, patch, 0, price)
	_, err := autoActionResult(w)
	return err
}

func (c *Controller) autoStartTarget(command *CommandRecord) (map[string]string, error) {
	c.mu.RLock()
	location := c.state.Objects["locations"][fieldText(command.Request, "location_id")]
	c.mu.RUnlock()
	if location == nil {
		return nil, fmt.Errorf("START_SESSION Location is not in the Partner catalogue")
	}
	evseUID := fieldText(command.Request, "evse_uid")
	evses, _ := location.Body["evses"].([]any)
	if evseUID == "" {
		candidates := make([]string, 0, len(evses))
		for _, raw := range evses {
			evse, _ := raw.(map[string]any)
			connectors, _ := evse["connectors"].([]any)
			if fieldText(evse, "status") == "AVAILABLE" && len(connectors) > 0 {
				candidates = append(candidates, fieldText(evse, "uid"))
			}
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("START_SESSION Location has no available EVSE with a connector")
		}
		if len(candidates) > 1 {
			return nil, fmt.Errorf("START_SESSION has %d available EVSEs; specify evse_uid or wait for the Charge Point to identify the connected EVSE", len(candidates))
		}
		evseUID = candidates[0]
	}
	var selectedEVSE map[string]any
	for _, raw := range evses {
		evse, _ := raw.(map[string]any)
		if fieldText(evse, "uid") == evseUID {
			selectedEVSE = evse
			if fieldText(evse, "status") != "AVAILABLE" {
				return nil, fmt.Errorf("START_SESSION EVSE %s is %s", evseUID, fieldText(evse, "status"))
			}
			break
		}
	}
	if selectedEVSE == nil {
		return nil, fmt.Errorf("START_SESSION EVSE %s is not in the Location", evseUID)
	}
	connectorID := fieldText(command.Request, "connector_id")
	if connectorID == "" {
		capabilities, _ := selectedEVSE["capabilities"].([]any)
		for _, capability := range capabilities {
			if capability == "START_SESSION_CONNECTOR_REQUIRED" {
				return nil, fmt.Errorf("START_SESSION EVSE %s requires connector_id", evseUID)
			}
		}
		connectors, _ := selectedEVSE["connectors"].([]any)
		if len(connectors) != 1 {
			return nil, fmt.Errorf("START_SESSION EVSE %s has %d connectors; specify connector_id or wait for the Charge Point to identify the connected connector", evseUID, len(connectors))
		}
		connector, _ := connectors[0].(map[string]any)
		connectorID = fieldText(connector, "id")
	}
	connector, err := connectorFromLocation(location.Body, evseUID, connectorID)
	if err != nil {
		return nil, err
	}
	return map[string]string{"evse_uid": evseUID, "connector_id": fieldText(connector, "id")}, nil
}

func (c *Controller) recordAutoError(commandID string, err error) {
	c.mu.Lock()
	if command := c.state.Commands[commandID]; command != nil {
		if command.Result == nil {
			command.Result = map[string]any{}
		}
		command.Result["automation_error"] = err.Error()
		command.UpdatedAt = now()
	}
	c.mu.Unlock()
	c.changed()
}

func (c *Controller) recordAutoEVSEStatusError(commandID, status string, err error) {
	c.mu.Lock()
	if command := c.state.Commands[commandID]; command != nil {
		if command.Result == nil {
			command.Result = map[string]any{}
		}
		command.Result["evse_status_error"] = fmt.Sprintf("%s: %v", status, err)
		command.UpdatedAt = now()
	}
	c.mu.Unlock()
	c.changed()
}

func (c *Controller) callSessionAction(ctx context.Context, action string, input map[string]any) (map[string]any, error) {
	raw, _ := json.Marshal(input)
	r := httptest.NewRequest(http.MethodPost, "/api/cpo/actions/session/"+action, bytes.NewReader(raw)).WithContext(ctx)
	r.SetPathValue("action", action)
	w := httptest.NewRecorder()
	c.apiSessionAction(w, r)
	return autoActionResult(w)
}

func (c *Controller) callSend(ctx context.Context, module, id, method string) error {
	raw, _ := json.Marshal(sendInput{Module: module, ID: id, Method: method})
	r := httptest.NewRequest(http.MethodPost, "/api/cpo/actions/send", bytes.NewReader(raw)).WithContext(ctx)
	w := httptest.NewRecorder()
	c.apiSend(w, r)
	_, err := autoActionResult(w)
	return err
}

func (c *Controller) callEVSEStatus(ctx context.Context, locationID, evseUID, status string) error {
	raw, _ := json.Marshal(map[string]string{"location_id": locationID, "evse_uid": evseUID, "status": status})
	r := httptest.NewRequest(http.MethodPost, "/api/cpo/actions/evse-status", bytes.NewReader(raw)).WithContext(ctx)
	w := httptest.NewRecorder()
	c.apiEVSEStatus(w, r)
	_, err := autoActionResult(w)
	return err
}

func autoActionResult(w *httptest.ResponseRecorder) (map[string]any, error) {
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		return nil, err
	}
	if w.Code < 200 || w.Code >= 300 {
		return nil, fmt.Errorf("HTTP %d: %v", w.Code, result["error"])
	}
	return result, nil
}

func (c *Controller) startAutoRun(sessionID string) {
	c.autoMu.Lock()
	if c.autoRuns[sessionID] != nil {
		c.autoMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &autoMeterRun{cancel: cancel, done: make(chan struct{})}
	c.autoRuns[sessionID] = run
	c.autoMu.Unlock()
	go c.autoMeterLoop(ctx, sessionID, run)
}

func (c *Controller) autoMeterLoop(ctx context.Context, sessionID string, run *autoMeterRun) {
	defer close(run.done)
	defer func() {
		c.autoMu.Lock()
		if c.autoRuns[sessionID] == run {
			delete(c.autoRuns, sessionID)
		}
		c.autoMu.Unlock()
	}()
	ticker := time.NewTicker(autoMeterTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !c.advanceAutoMeter(sessionID, time.Now()) {
				return
			}
		}
	}
}

func (c *Controller) autoMeterTarget(sessionID string, at time.Time) (float64, bool) {
	c.mu.RLock()
	record := c.state.Objects["sessions"][sessionID]
	enabled, rate := c.state.Config.AutoCommandFlow, c.state.Config.AutoMeterRateKWh
	active := record != nil && fieldText(record.Body, "status") == "ACTIVE" && record.Delivery == "SENT"
	if !enabled || !active {
		c.mu.RUnlock()
		return 0, false
	}
	current := number(record.Body["kwh"])
	lastUpdated := fieldText(record.Body, "last_updated")
	c.mu.RUnlock()
	checkpoint, err := time.Parse(time.RFC3339Nano, lastUpdated)
	if err != nil {
		return 0, false
	}
	elapsed := at.Sub(checkpoint)
	if elapsed < 0 {
		elapsed = 0
	}
	target := float64(int64((current+rate*elapsed.Hours())*1e6+0.5)) / 1e6
	return target, true
}

// Charging Period cadence of the automatic meter. OCPI 2.2.1 §9.3.1 leaves it to the CPO. Test charges are
// short, so by default every meter tick reports its period; period_interval_seconds spaces them out (the
// spec calls one per minute too much for AC and one per 30 minutes too little for DC).
const (
	autoMeterTickInterval = 15 * time.Second
	defaultPeriodInterval = autoMeterTickInterval
	minPeriodInterval     = 15
	maxPeriodInterval     = 1800
)

func (config Config) periodInterval() time.Duration {
	if config.PeriodIntervalSeconds <= 0 {
		return defaultPeriodInterval
	}
	return time.Duration(config.PeriodIntervalSeconds) * time.Second
}

// periodDue says whether a meter tick at `at` reports Charging Periods. An interval no longer than a tick
// reports on every tick, so ticker jitter cannot skip one.
func (config Config) periodDue(at, reported time.Time) bool {
	interval := config.periodInterval()
	return interval <= autoMeterTickInterval || at.Sub(reported) >= interval
}

func (c *Controller) advanceAutoMeter(sessionID string, at time.Time) bool {
	c.mu.RLock()
	parked := c.idleCommandLocked(sessionID) != nil
	c.mu.RUnlock()
	if parked {
		return c.advanceParking(sessionID, at)
	}
	target, ok := c.autoMeterTarget(sessionID, at)
	if !ok {
		return false
	}
	if err := c.autoMeterTick(sessionID, target, at); err != nil {
		c.recordAutoErrorForSession(sessionID, err)
		return false
	}
	return true
}

// autoMeterTick reports the meter of a charging Session: kwh and total_cost on every tick, and the
// Charging Periods since the last report once the period interval has passed.
func (c *Controller) autoMeterTick(sessionID string, target float64, at time.Time) error {
	at = at.UTC().Truncate(time.Millisecond)
	c.mu.Lock()
	record := c.state.Objects["sessions"][sessionID]
	if record == nil || fieldText(record.Body, "status") != "ACTIVE" || c.idleCommandLocked(sessionID) != nil {
		c.mu.Unlock()
		return fmt.Errorf("only an ACTIVE charging Session can update the meter")
	}
	increment := target - number(record.Body["kwh"])
	if increment < 0 {
		increment = 0
	}
	flush := c.state.Config.periodDue(at, periodsEnd(record.Body))
	patch := c.meterUpdate(record, increment, false, ocpi.FormatTime(at), flush)
	c.mu.Unlock()
	c.changed()
	recorder := httptest.NewRecorder()
	c.sendInline(recorder, httptest.NewRequest(http.MethodPost, "/api/cpo/actions/session/advance", nil), sendInput{Module: "sessions", ID: sessionID, Method: http.MethodPatch, Body: patch})
	if recorder.Code < 200 || recorder.Code >= 300 {
		return fmt.Errorf("meter Session PATCH failed: %s", recorder.Body.String())
	}
	return nil
}

func (c *Controller) recordAutoErrorForSession(sessionID string, err error) {
	c.mu.RLock()
	var commandID string
	for id, command := range c.state.Commands {
		if command.Type == "START_SESSION" && fieldText(command.Result, "session_id") == sessionID {
			commandID = id
			break
		}
	}
	c.mu.RUnlock()
	if commandID != "" {
		c.recordAutoError(commandID, err)
	}
}

func (c *Controller) stopAutoRun(sessionID string) {
	c.autoMu.Lock()
	run := c.autoRuns[sessionID]
	if run != nil {
		run.cancel()
	}
	c.autoMu.Unlock()
	if run != nil {
		<-run.done
	}
}

func (c *Controller) stopAllAutoRuns() {
	c.autoMu.Lock()
	runs := make([]*autoMeterRun, 0, len(c.autoRuns))
	for _, run := range c.autoRuns {
		run.cancel()
		runs = append(runs, run)
	}
	c.autoMu.Unlock()
	for _, run := range runs {
		<-run.done
	}
}

func (c *Controller) resumeAutoRuns() {
	c.mu.RLock()
	if !c.state.Config.AutoCommandFlow {
		c.mu.RUnlock()
		return
	}
	var sessions []string
	for _, command := range c.state.Commands {
		if command.Type != "START_SESSION" {
			continue
		}
		id := fieldText(command.Result, "session_id")
		record := c.state.Objects["sessions"][id]
		if record != nil && fieldText(record.Body, "status") == "ACTIVE" && record.Delivery == "SENT" {
			sessions = append(sessions, id)
		}
	}
	c.mu.RUnlock()
	for _, id := range sessions {
		c.startAutoRun(id)
	}
}
