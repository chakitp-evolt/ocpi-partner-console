package cpo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

func (c *Controller) stopCommandLocked(sessionID string) *CommandRecord {
	var selected *CommandRecord
	for _, command := range c.state.Commands {
		if command.Type != "STOP_SESSION" || fieldText(command.Request, "session_id") != sessionID {
			continue
		}
		if selected == nil || command.CreatedAt > selected.CreatedAt {
			selected = command
		}
	}
	return selected
}

func (c *Controller) idleCommandLocked(sessionID string) *CommandRecord {
	command := c.stopCommandLocked(sessionID)
	if command != nil && fieldText(command.Result, "idle_started_at") != "" {
		return command
	}
	return nil
}

func evseStatus(location map[string]any, evseUID string) string {
	evses, _ := location["evses"].([]any)
	for _, raw := range evses {
		evse, _ := raw.(map[string]any)
		if fieldText(evse, "uid") == evseUID {
			return fieldText(evse, "status")
		}
	}
	return ""
}

func (c *Controller) ensureEVSEStatus(ctx context.Context, locationID, evseUID, status string) error {
	c.mu.RLock()
	location := c.state.Objects["locations"][locationID]
	if location == nil || evseStatus(location.Body, evseUID) == "" {
		c.mu.RUnlock()
		return fmt.Errorf("Session EVSE %s in Location %s is unavailable", evseUID, locationID)
	}
	needed := evseStatus(location.Body, evseUID) != status || location.Delivery != "SENT"
	c.mu.RUnlock()
	if !needed {
		return nil
	}
	return c.callEVSEStatus(ctx, locationID, evseUID, status)
}

func (c *Controller) beginIdle(ctx context.Context, sessionID string, target *float64) error {
	c.mu.RLock()
	command := c.stopCommandLocked(sessionID)
	record := c.state.Objects["sessions"][sessionID]
	valid := command != nil && record != nil && fieldText(record.Body, "status") == "ACTIVE" && record.Delivery == "SENT" && fieldText(command.Result, "idle_started_at") == ""
	c.mu.RUnlock()
	if !valid {
		return fmt.Errorf("an ACTIVE, sent Session and an unprocessed STOP_SESSION are required")
	}
	input := map[string]any{"session_id": sessionID, "target_kwh": target}
	if target == nil {
		delete(input, "target_kwh")
		input["kwh"] = 0.0
	}
	if _, err := c.callSessionAction(ctx, "advance", input); err != nil {
		return err
	}
	c.mu.Lock()
	locationID, evseUID := "", ""
	var unplugAt time.Time
	if saved := c.state.Commands[command.ID]; saved != nil {
		if saved.Result == nil {
			saved.Result = map[string]any{}
		}
		if current := c.state.Objects["sessions"][sessionID]; current != nil {
			saved.Result["idle_started_at"] = fieldText(current.Body, "last_updated")
			locationID, evseUID = fieldText(current.Body, "location_id"), fieldText(current.Body, "evse_uid")
			_, tariff, _, _ := c.tariffForSession(current.Body)
			if tariff != nil {
				saved.Result["tariff_snapshot"] = cloneMap(tariff)
			}
			if c.state.Config.TestIdleFee && c.state.Config.IdleUnplugAfterSeconds > 0 {
				idleAt, _ := parseOCPITime(fieldText(current.Body, "last_updated"))
				unplugAt = idleAt.Add(time.Duration(c.state.Config.IdleUnplugAfterSeconds) * time.Second)
				saved.Result["auto_unplug_at"] = ocpi.FormatTime(unplugAt)
			}
		}
		saved.UpdatedAt = now()
	}
	c.mu.Unlock()
	c.changed()
	if err := c.ensureEVSEStatus(ctx, locationID, evseUID, "CHARGING"); err != nil {
		c.recordAutoEVSEStatusError(command.ID, "CHARGING", err)
	}
	if !unplugAt.IsZero() {
		c.startIdleRun(sessionID, command.ID, unplugAt)
	}
	return nil
}

func (c *Controller) apiParkingPreview(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	c.mu.RLock()
	record := c.state.Objects["sessions"][sessionID]
	stop := c.idleCommandLocked(sessionID)
	if record == nil || stop == nil || fieldText(record.Body, "status") != "ACTIVE" {
		c.mu.RUnlock()
		writeError(w, http.StatusConflict, fmt.Errorf("Session is not waiting for connector unplug"))
		return
	}
	idleAt, idleErr := parseOCPITime(fieldText(stop.Result, "idle_started_at"))
	startAt, startErr := parseOCPITime(fieldText(record.Body, "start_date_time"))
	if idleErr != nil || startErr != nil {
		c.mu.RUnlock()
		writeError(w, http.StatusInternalServerError, fmt.Errorf("invalid Session timestamp"))
		return
	}
	timestamp := time.Now().UTC()
	if timestamp.Before(idleAt) {
		timestamp = idleAt
	}
	parkingHours := timestamp.Sub(idleAt).Hours()
	tariffID, tariff, zone, _ := c.tariffForSession(record.Body)
	if snapshot, ok := stop.Result["tariff_snapshot"].(map[string]any); ok {
		tariff, tariffID = snapshot, fieldText(snapshot, "id")
	}
	price := priceSessionAs(c.state.Config.MaxPriceMode, tariff, tariffID, sessionSpans(record.Body, idleAt), startAt.In(zone), idleAt, parkingHours)
	c.mu.RUnlock()
	parkingCost := map[string]any{"excl_vat": 0.0, "incl_vat": 0.0}
	for _, line := range price.Lines {
		if line.Dimension == "PARKING_TIME" {
			parkingCost = addCost(parkingCost, map[string]any{"excl_vat": line.ExclVAT, "incl_vat": line.InclVAT})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "idle_started_at": idleAt.Format(time.RFC3339Nano), "as_of": timestamp.Format(time.RFC3339Nano), "parking_hours": parkingHours, "parking_cost": parkingCost, "total_cost": price.totalCost(), "priced": price.Priced, "note": price.Note})
}

// sessionBillsParkingLocked says whether the Session's Tariff has any PARKING_TIME component. A time
// window that has not started yet still counts: the car may stay plugged in until it opens. A Session
// without a Tariff keeps the idle step, so unpriced parking can still be tested. Caller holds c.mu.
func (c *Controller) sessionBillsParkingLocked(sessionID string) bool {
	record := c.state.Objects["sessions"][sessionID]
	if record == nil {
		return true
	}
	_, tariff, _, _ := c.tariffForSession(record.Body)
	if tariff == nil {
		return true
	}
	elements, _ := tariff["elements"].([]any)
	for _, raw := range elements {
		element, _ := raw.(map[string]any)
		if priceComponentOfType(element, "PARKING_TIME") != nil {
			return true
		}
	}
	return false
}

func (c *Controller) stopSessionManually(w http.ResponseWriter, r *http.Request, sessionID string, target *float64) {
	c.mu.Lock()
	billsParking := c.sessionBillsParkingLocked(sessionID)
	stop := c.stopCommandLocked(sessionID)
	current := 0.0
	if record := c.state.Objects["sessions"][sessionID]; record != nil {
		current = number(record.Body["kwh"])
	}
	// A hand-driven stop leaves the CDR for the tester to create and send.
	if stop != nil && fieldText(stop.Result, "idle_started_at") == "" {
		if stop.Result == nil {
			stop.Result = map[string]any{}
		}
		stop.Result["manual_cdr"] = true
	}
	c.mu.Unlock()
	// Nothing bills parking, so the stop closes the Session right away.
	if !billsParking && stop != nil {
		final := current
		if target != nil {
			final = *target
		}
		if err := c.completeStoppedSession(r.Context(), sessionID, stop.ID, final); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session_id": sessionID, "completed": true})
		return
	}
	if err := c.beginIdle(r.Context(), sessionID, target); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	// The parking is reported while the car stays plugged in, as after an automatic stop.
	c.startAutoRun(sessionID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session_id": sessionID})
}

// unplugSession closes a charge only after Evolt's STOP_SESSION stopped the meter. The connector
// remained occupied during this interval, so its ChargingPeriod uses PARKING_TIME rather than TIME.
func (c *Controller) unplugSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	c.mu.Lock()
	if c.unplugging[sessionID] {
		c.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("Session unplug is already in progress"))
		return
	}
	c.unplugging[sessionID] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.unplugging, sessionID)
		c.mu.Unlock()
		c.cancelIdleRun(sessionID)
	}()
	c.stopAutoRun(sessionID)
	c.mu.Lock()
	record := c.state.Objects["sessions"][sessionID]
	stop := c.idleCommandLocked(sessionID)
	if record == nil || stop == nil {
		c.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("Session is not waiting for connector unplug after STOP_SESSION"))
		return
	}
	if fieldText(record.Body, "status") == "COMPLETED" && fieldText(stop.Result, "idle_finished_at") != "" {
		patch, _ := stop.Result["final_patch"].(map[string]any)
		if patch == nil {
			c.mu.Unlock()
			writeError(w, http.StatusInternalServerError, fmt.Errorf("final Session PATCH is missing"))
			return
		}
		cost, _ := record.Body["total_cost"].(map[string]any)
		price := sessionPrice{Priced: cost != nil, ExclVAT: number(cost["excl_vat"]), InclVAT: number(cost["incl_vat"])}
		parkingHours := elapsedHours(fieldText(stop.Result, "idle_started_at"), fieldText(stop.Result, "idle_finished_at"))
		stopID := stop.ID
		c.mu.Unlock()
		c.finishUnplug(w, r, sessionID, stopID, patch, parkingHours, price)
		return
	}
	if fieldText(record.Body, "status") != "ACTIVE" {
		c.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("Session is not waiting for connector unplug after STOP_SESSION"))
		return
	}
	// A parking PATCH that never arrived leaves Evolt behind; the closing update then resends the whole Session.
	resync := record.Delivery != "SENT"
	patch, price, parkingHours, err := c.parkingUpdate(record, stop, time.Now(), true)
	if err != nil {
		c.mu.Unlock()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	timestamp := patch["last_updated"]
	patch["status"], patch["end_date_time"] = "COMPLETED", timestamp
	record.Body["status"], record.Body["end_date_time"] = "COMPLETED", timestamp
	stop.Result["idle_finished_at"] = timestamp
	stop.Result["final_patch"] = cloneMap(patch)
	if resync {
		stop.Result["final_resync"] = true
	}
	stopID := stop.ID
	c.mu.Unlock()
	c.changed()
	c.finishUnplug(w, r, sessionID, stopID, patch, parkingHours, price)
}

// parkingUpdate re-prices a parked Session up to `at`. With flush it also reports the parking since the
// Charging Periods ended as PARKING_TIME periods, split at tariff boundaries. An unpriced Session still
// reports the parking time; it just carries no total_cost. Caller holds c.mu.
func (c *Controller) parkingUpdate(record *Record, stop *CommandRecord, at time.Time, flush bool) (map[string]any, sessionPrice, float64, error) {
	idleAt, err := parseOCPITime(fieldText(stop.Result, "idle_started_at"))
	if err != nil {
		return nil, sessionPrice{}, 0, fmt.Errorf("invalid stop timestamp: %w", err)
	}
	startAt, err := parseOCPITime(fieldText(record.Body, "start_date_time"))
	if err != nil {
		return nil, sessionPrice{}, 0, fmt.Errorf("invalid Session start: %w", err)
	}
	checkpoint := periodsEnd(record.Body)
	if checkpoint.Before(idleAt) {
		checkpoint = idleAt
	}
	at = at.UTC().Truncate(time.Millisecond)
	if at.Before(checkpoint) {
		at = checkpoint
	}
	tariffID, tariff, zone, note := c.tariffForSession(record.Body)
	if snapshot, ok := stop.Result["tariff_snapshot"].(map[string]any); ok {
		tariff, tariffID = snapshot, fieldText(snapshot, "id")
	}
	kwh := number(record.Body["kwh"])
	parkingHours := at.Sub(idleAt).Hours()
	price := priceSessionAs(c.state.Config.MaxPriceMode, tariff, tariffID, sessionSpans(record.Body, idleAt), startAt.In(zone), idleAt, parkingHours)
	if price.Note == "" {
		price.Note = note
	}
	var segments []parkingSegment
	if flush {
		segments = parkingSegments(tariff, checkpoint.In(zone), at.Sub(checkpoint).Hours(), startAt.In(zone), kwh)
	}
	added := make([]any, 0, len(segments))
	for _, segment := range segments {
		added = append(added, chargingPeriod(ocpi.FormatTime(segment.Start), tariffID, map[string]float64{"PARKING_TIME": roundHours(segment.End.Sub(segment.Start).Hours())}))
	}
	timestamp := ocpi.FormatTime(at)
	patch := map[string]any{"last_updated": timestamp}
	if price.Priced {
		patch["total_cost"] = price.totalCost()
		record.Body["total_cost"] = patch["total_cost"]
	} else {
		delete(record.Body, "total_cost")
	}
	if len(added) > 0 {
		periods, _ := record.Body["charging_periods"].([]any)
		record.Body["charging_periods"] = append(periods, added...)
		patch["charging_periods"] = added
	}
	record.Body["last_updated"] = timestamp
	record.Delivery, record.UpdatedAt = "CHANGED_AFTER_SEND", timestamp
	return patch, price, parkingHours, nil
}

// advanceParking reports a parked Session while the car is still plugged in: total_cost on every tick,
// and PARKING_TIME Charging Periods once the period interval has passed (OCPI 2.2.1 §9.1.1, §9.3.1).
// With ParkingAtUnplugOnly the parking waits for the unplug instead; both are valid CPO behaviour. It runs
// for a Session stopped by hand as well, so it does not depend on Auto Command Flow.
func (c *Controller) advanceParking(sessionID string, at time.Time) bool {
	at = at.UTC().Truncate(time.Millisecond)
	c.mu.Lock()
	record := c.state.Objects["sessions"][sessionID]
	stop := c.idleCommandLocked(sessionID)
	if record == nil || stop == nil || fieldText(record.Body, "status") != "ACTIVE" || record.Delivery != "SENT" || c.unplugging[sessionID] {
		c.mu.Unlock()
		return false
	}
	stopID := stop.ID
	if c.state.Config.ParkingAtUnplugOnly {
		atMax := c.idleAtMaxPriceLocked(record, stop, at)
		c.mu.Unlock()
		if atMax {
			c.unplugAtMaxPrice(sessionID, stopID)
			return false
		}
		return true
	}
	flush := c.state.Config.periodDue(at, periodsEnd(record.Body))
	patch, _, _, err := c.parkingUpdate(record, stop, at, flush)
	atMax := err == nil && c.idleAtMaxPriceLocked(record, stop, at)
	c.mu.Unlock()
	if err != nil {
		c.recordAutoError(stopID, err)
		return false
	}
	c.changed()
	recorder := httptest.NewRecorder()
	c.sendInline(recorder, httptest.NewRequest(http.MethodPost, "/api/cpo/actions/session/park", nil), sendInput{Module: "sessions", ID: sessionID, Method: http.MethodPatch, Body: patch})
	if recorder.Code < 200 || recorder.Code >= 300 {
		c.recordAutoError(stopID, fmt.Errorf("parking Session PATCH failed: %s", recorder.Body.String()))
		return false
	}
	if atMax {
		c.unplugAtMaxPrice(sessionID, stopID)
		return false
	}
	return true
}

// idleAtMaxPriceLocked reports whether a parked Session set to stop at max_price has reached it by at.
// Caller holds c.mu.
func (c *Controller) idleAtMaxPriceLocked(record *Record, stop *CommandRecord, at time.Time) bool {
	if !c.state.Config.StopAtMaxPrice {
		return false
	}
	idleAt, err := parseOCPITime(fieldText(stop.Result, "idle_started_at"))
	if err != nil || at.Before(idleAt) {
		return false
	}
	startAt, err := parseOCPITime(fieldText(record.Body, "start_date_time"))
	if err != nil {
		return false
	}
	tariffID, tariff, zone, _ := c.tariffForSession(record.Body)
	if snapshot, ok := stop.Result["tariff_snapshot"].(map[string]any); ok {
		tariff, tariffID = snapshot, fieldText(snapshot, "id")
	}
	price := priceSessionAs(c.state.Config.MaxPriceMode, tariff, tariffID, sessionSpans(record.Body, idleAt), startAt.In(zone), idleAt, at.Sub(idleAt).Hours())
	return reachedMaxPrice(tariff, price.totalCost())
}

func (c *Controller) finishUnplug(w http.ResponseWriter, r *http.Request, sessionID, stopID string, patch map[string]any, parkingHours float64, price sessionPrice) {
	c.mu.RLock()
	record := c.state.Objects["sessions"][sessionID]
	delivery := record.Delivery
	locationID, evseUID := fieldText(record.Body, "location_id"), fieldText(record.Body, "evse_uid")
	send := sendInput{Module: "sessions", ID: sessionID, Method: http.MethodPatch, Body: patch}
	if stop := c.state.Commands[stopID]; stop != nil && stop.Result["final_resync"] == true {
		send = sendInput{Module: "sessions", ID: sessionID, Method: http.MethodPut}
	}
	c.mu.RUnlock()
	if delivery != "SENT" {
		recorder := httptest.NewRecorder()
		c.sendInline(recorder, r, send)
		if recorder.Code < 200 || recorder.Code >= 300 {
			writeError(w, http.StatusBadGateway, fmt.Errorf("final Session PATCH failed: %s", recorder.Body.String()))
			return
		}
	}
	evseStatusError := ""
	if err := c.ensureEVSEStatus(r.Context(), locationID, evseUID, "AVAILABLE"); err != nil {
		c.recordAutoEVSEStatusError(stopID, "AVAILABLE", err)
		evseStatusError = err.Error()
	} else {
		c.mu.Lock()
		if saved := c.state.Commands[stopID]; saved != nil {
			delete(saved.Result, "evse_status_error")
		}
		c.mu.Unlock()
	}
	c.mu.RLock()
	manualCDR := false
	if stop := c.state.Commands[stopID]; stop != nil {
		manualCDR = stop.Result["manual_cdr"] == true
	}
	c.mu.RUnlock()
	if manualCDR {
		c.changed()
		response := map[string]any{"ok": true, "session_id": sessionID, "manual_cdr": true, "parking_hours": parkingHours, "price": price}
		if evseStatusError != "" {
			response["evse_status_error"] = evseStatusError
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	cdrID := "CDR-" + sessionID
	c.mu.RLock()
	cdr := c.state.Objects["cdrs"][cdrID]
	c.mu.RUnlock()
	if cdr == nil {
		if _, err := c.callSessionAction(r.Context(), "cdr", map[string]any{"session_id": sessionID}); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
	}
	c.mu.RLock()
	cdr = c.state.Objects["cdrs"][cdrID]
	cdrDelivery := cdr.Delivery
	c.mu.RUnlock()
	if cdrDelivery != "SENT" {
		if err := c.callSend(r.Context(), "cdrs", cdrID, http.MethodPost); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Errorf("CDR draft %s created but POST failed: %w", cdrID, err))
			return
		}
	}
	c.mu.Lock()
	if saved := c.state.Commands[stopID]; saved != nil {
		saved.Result["cdr_id"] = cdrID
	}
	c.mu.Unlock()
	c.changed()
	response := map[string]any{"ok": true, "session_id": sessionID, "cdr_id": cdrID, "parking_hours": parkingHours, "price": price}
	if evseStatusError != "" {
		response["evse_status_error"] = evseStatusError
	}
	writeJSON(w, http.StatusOK, response)
}
