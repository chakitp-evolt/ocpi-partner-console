package cpo

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpicore/rules"
)

// defaultCdrToken is the demo RFID used when no token or START_SESSION command is selected.
func defaultCdrToken() map[string]any {
	return map[string]any{"country_code": "TH", "party_id": "EVT", "uid": "DEMO-RFID-001", "type": "RFID", "contract_id": "TH-EVT-C00000001"}
}

func (c *Controller) apiSessionAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	var input struct {
		SessionID   string   `json:"session_id"`
		CommandID   string   `json:"command_id"`
		TokenID     string   `json:"token_id"`
		LocationID  string   `json:"location_id"`
		EVSEUID     string   `json:"evse_uid"`
		ConnectorID string   `json:"connector_id"`
		KWh         float64  `json:"kwh"`
		TargetKWh   *float64 `json:"target_kwh"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if action == "unplug" {
		c.unplugSession(w, r, input.SessionID)
		return
	}
	if action == "stop" {
		c.stopSessionManually(w, r, input.SessionID, input.TargetKWh)
		return
	}
	c.mu.Lock()
	config := c.state.Config
	owner := rules.Owner{CountryCode: config.CountryCode, PartyID: config.PartyID}
	timestamp := now()
	var response map[string]any
	var price sessionPrice
	switch action {
	case "create":
		cdrToken := defaultCdrToken()
		authMethod := "WHITELIST"
		authorizationReference := ""
		if input.CommandID != "" {
			command := c.state.Commands[input.CommandID]
			if command == nil || command.Type != "START_SESSION" {
				c.mu.Unlock()
				writeError(w, http.StatusNotFound, fmt.Errorf("START_SESSION command not found"))
				return
			}
			if token, ok := command.Request["token"].(map[string]any); ok {
				cdrToken = cdrTokenFromToken(token)
			}
			authMethod = "COMMAND"
			authorizationReference = fieldText(command.Request, "authorization_reference")
			if value := fieldText(command.Request, "location_id"); value != "" {
				input.LocationID = value
			}
			if value := fieldText(command.Request, "evse_uid"); value != "" {
				input.EVSEUID = value
			}
			if value := fieldText(command.Request, "connector_id"); value != "" {
				input.ConnectorID = value
			}
		} else if input.TokenID != "" {
			token := c.state.Objects["tokens"][input.TokenID]
			if token == nil {
				c.mu.Unlock()
				writeError(w, http.StatusNotFound, fmt.Errorf("selected Token is no longer in the partner catalogue; sync tokens and select again"))
				return
			}
			cdrToken = cdrTokenFromToken(token.Body)
		}
		location := c.state.Objects["locations"][input.LocationID]
		if location == nil {
			c.mu.Unlock()
			writeError(w, http.StatusNotFound, fmt.Errorf("Location %q not found in the partner catalogue", input.LocationID))
			return
		}
		connector, err := connectorFromLocation(location.Body, input.EVSEUID, input.ConnectorID)
		if err != nil {
			c.mu.Unlock()
			writeError(w, http.StatusNotFound, err)
			return
		}
		// A START_SESSION may leave connector_id out (OCPI 2.2.1 §13.2); the Session object it
		// results in may not, so the resolved connector is written back onto the draft.
		input.ConnectorID = fieldText(connector, "id")
		tariffID, note := tariffIDForConnector(connector, c.state.Objects["tariffs"], time.Now())
		var tariff map[string]any
		if record := c.state.Objects["tariffs"][tariffID]; record != nil {
			tariff = record.Body
		}
		price = priceSession(tariff, tariffID, 0, 0, time.Now().In(sessionZone(location.Body)))
		if price.Note == "" {
			price.Note = note
		}
		if input.SessionID == "" {
			input.SessionID = "SESSION-" + strings.ToUpper(strings.ReplaceAll(ocpiShortID(), "-", ""))
		}
		body := map[string]any{
			"country_code": config.CountryCode, "party_id": config.PartyID, "id": input.SessionID, "start_date_time": timestamp, "kwh": 0.0,
			"cdr_token":   cdrToken,
			"auth_method": authMethod, "location_id": input.LocationID, "evse_uid": input.EVSEUID, "connector_id": input.ConnectorID,
			"currency": tariffCurrency(tariff, "THB"), "charging_periods": []any{},
			"status": "ACTIVE", "last_updated": timestamp,
		}
		if price.Priced {
			body["total_cost"] = price.totalCost()
		}
		if authorizationReference != "" {
			body["authorization_reference"] = authorizationReference
		}
		snapshot := map[string]any{"location": cloneMap(location.Body), "tariff_id": tariffID}
		if tariff != nil {
			snapshot["tariff"] = cloneMap(tariff)
		}
		c.state.Objects["sessions"][input.SessionID] = &Record{Body: body, Snapshot: snapshot, Delivery: "NOT_SENT", Findings: rules.Validate("sessions", body, owner), UpdatedAt: timestamp}
		response = body
	case "advance", "complete", "cdr":
		record := c.state.Objects["sessions"][input.SessionID]
		if record == nil {
			c.mu.Unlock()
			writeError(w, http.StatusNotFound, fmt.Errorf("Session not found"))
			return
		}
		if action == "advance" || action == "complete" {
			if c.idleCommandLocked(input.SessionID) != nil || action == "complete" && c.stopCommandLocked(input.SessionID) != nil {
				c.mu.Unlock()
				writeError(w, http.StatusConflict, fmt.Errorf("charging has stopped; wait for connector unplug"))
				return
			}
			if fieldText(record.Body, "status") != "ACTIVE" {
				c.mu.Unlock()
				writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("only ACTIVE Sessions can update the meter"))
				return
			}
			increment := 0.0
			if action == "advance" {
				increment = input.KWh
				if input.TargetKWh == nil && increment <= 0 {
					increment = 1.25
				}
			}
			if input.TargetKWh != nil {
				current := number(record.Body["kwh"])
				if *input.TargetKWh < current || *input.TargetKWh < 0 {
					c.mu.Unlock()
					writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("target_kwh must be at least the current meter reading (%g kWh)", current))
					return
				}
				increment = *input.TargetKWh - current
			}
			if action == "advance" {
				at, err := parseOCPITime(timestamp)
				if err != nil {
					at = time.Now().UTC()
				}
				if stopAt, kwh, reached := c.maxPriceStopLocked(record, number(record.Body["kwh"])+increment, at); reached {
					c.mu.Unlock()
					if err := c.completeSessionAt(r.Context(), input.SessionID, "", kwh, ocpi.FormatTime(stopAt)); err != nil {
						writeError(w, http.StatusBadGateway, fmt.Errorf("stop at max_price: %w", err))
						return
					}
					writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session_id": input.SessionID, "stopped_at_max_price": true, "kwh": kwh})
					return
				}
			}
			// A reading entered by hand is reported as its own Charging Period.
			patch := c.meterUpdate(record, increment, action == "complete", timestamp, true)
			c.mu.Unlock()
			c.changed()
			c.sendInline(w, r, sendInput{Module: "sessions", ID: input.SessionID, Method: http.MethodPatch, Body: patch})
			return
		}
		if fieldText(record.Body, "status") != "COMPLETED" {
			c.mu.Unlock()
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("complete the Session before creating its CDR"))
			return
		}
		location := c.state.Objects["locations"][fmt.Sprint(record.Body["location_id"])]
		var locationBody map[string]any
		if location != nil {
			locationBody = location.Body
		}
		if snapshot, ok := record.Snapshot["location"].(map[string]any); ok {
			locationBody = snapshot
		}
		if locationBody == nil {
			c.mu.Unlock()
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("Session Location not found"))
			return
		}
		if record.Body["total_cost"] == nil {
			c.mu.Unlock()
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("CDR total_cost is unknown; link a priced Tariff and complete the Session first"))
			return
		}
		cdrID := "CDR-" + input.SessionID
		if existing := c.state.Objects["cdrs"][cdrID]; existing != nil && existing.Delivery == "SENT" {
			c.mu.Unlock()
			writeError(w, http.StatusConflict, fmt.Errorf("sent CDR is immutable"))
			return
		}
		cdr := cdrFromSession(cdrID, record.Body, locationBody, timestamp)
		// CDR.tariffs carries the Tariffs the cost was calculated with (OCPI 2.2.1 §10.3) so the
		// eMSP can re-derive the amount instead of trusting total_cost blind.
		tariffID, tariff, zone, _ := c.tariffForSession(record.Body)
		periods, _ := record.Body["charging_periods"].([]any)
		parkingHours := sumDimension(periods, "PARKING_TIME")
		parkingAt, _ := parseOCPITime(fmt.Sprint(cdr["end_date_time"]))
		parkingAt = parkingAt.Add(-time.Duration(parkingHours * float64(time.Hour)))
		if stop := c.idleCommandLocked(input.SessionID); stop != nil {
			parkingAt, _ = parseOCPITime(fieldText(stop.Result, "idle_started_at"))
			parkingHours = elapsedHours(fieldText(stop.Result, "idle_started_at"), fieldText(stop.Result, "idle_finished_at"))
			if snapshot, ok := stop.Result["tariff_snapshot"].(map[string]any); ok {
				tariff = snapshot
				tariffID = fieldText(snapshot, "id")
			}
		}
		if tariff != nil {
			cdr["tariffs"] = []any{cloneMap(tariff)}
			startAt, startErr := parseOCPITime(fieldText(record.Body, "start_date_time"))
			if startErr == nil {
				price := priceSessionAs(c.state.Config.MaxPriceMode, tariff, tariffID, sessionSpans(record.Body, parkingAt), startAt.In(zone), parkingAt, parkingHours)
				for _, line := range price.Lines {
					cost := map[string]any{"excl_vat": line.ExclVAT, "incl_vat": line.InclVAT}
					switch line.Dimension {
					case "FLAT":
						cdr["total_fixed_cost"] = addCost(cdr["total_fixed_cost"], cost)
					case "ENERGY":
						cdr["total_energy_cost"] = addCost(cdr["total_energy_cost"], cost)
					case "TIME":
						cdr["total_time_cost"] = addCost(cdr["total_time_cost"], cost)
					case "PARKING_TIME":
						cdr["total_parking_cost"] = addCost(cdr["total_parking_cost"], cost)
					}
				}
				if parkingHours > 0 {
					cdr["total_parking_time"] = parkingHours
				}
			}
		}
		c.state.Objects["cdrs"][cdrID] = &Record{Body: cdr, Delivery: "NOT_SENT", Findings: rules.Validate("cdrs", cdr, owner), UpdatedAt: timestamp}
		response = cdr
	default:
		c.mu.Unlock()
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown session action"))
		return
	}
	c.mu.Unlock()
	c.changed()
	result := map[string]any{"ok": true, "object": response}
	if action == "create" {
		result["price"] = price
	}
	writeJSON(w, http.StatusOK, result)
}

func addCost(existing any, next map[string]any) map[string]any {
	previous, _ := existing.(map[string]any)
	return map[string]any{"excl_vat": roundMoney(number(previous["excl_vat"]) + number(next["excl_vat"])), "incl_vat": roundMoney(number(previous["incl_vat"]) + number(next["incl_vat"]))}
}

// meterUpdate moves a Session's meter to kwh+increment and re-prices it against its Tariff. With flush
// (and always on complete) the charging since the reported Charging Periods ended is reported as new
// periods, cut at tariff boundaries; otherwise only kwh, total_cost and last_updated change, and the
// periods follow on a later flush. The local Session keeps all periods, while OCPI PATCH appends only new
// ones (OCPI 2.2.1 §9.2.2.3). Caller holds c.mu.
func (c *Controller) meterUpdate(record *Record, increment float64, complete bool, timestamp string, flush bool) map[string]any {
	tariffID, tariff, zone, note := c.tariffForSession(record.Body)
	start := fieldText(record.Body, "start_date_time")
	kwh := number(record.Body["kwh"]) + increment
	at, err := parseOCPITime(timestamp)
	if err != nil {
		at = time.Now()
	}
	startTime, err := parseOCPITime(start)
	if err != nil {
		startTime = at
	}
	record.Body["kwh"] = kwh
	var added []any
	if flush || complete {
		added = chargePeriods(record.Body, tariff, tariffID, zone, at)
		periods, _ := record.Body["charging_periods"].([]any)
		// A CDR needs at least one Charging Period (OCPI 2.2.1 §10.3.1), even for a session with nothing to report.
		if complete && len(periods) == 0 && len(added) == 0 {
			added = []any{chargingPeriod(start, tariffID, map[string]float64{"ENERGY": 0})}
		}
		record.Body["charging_periods"] = append(periods, added...)
	}
	price := priceSessionAs(c.state.Config.MaxPriceMode, tariff, tariffID, sessionSpans(record.Body, at), startTime.In(zone), at, 0)
	if price.Note == "" {
		price.Note = note
	}
	patch := map[string]any{"kwh": kwh, "last_updated": timestamp}
	if price.Priced {
		patch["total_cost"] = price.totalCost()
		record.Body["total_cost"] = patch["total_cost"]
	} else {
		delete(record.Body, "total_cost")
	}
	if len(added) > 0 {
		patch["charging_periods"] = added
	}
	if complete {
		patch["status"], patch["end_date_time"] = "COMPLETED", timestamp
		record.Body["status"], record.Body["end_date_time"] = "COMPLETED", timestamp
	}
	record.Body["last_updated"] = timestamp
	record.Delivery, record.UpdatedAt = "CHANGED_AFTER_SEND", timestamp
	return patch
}

// apiChargingPreview prices a charging Session as of now at the live meter reading in ?kwh=, the way the
// next meter update would, without changing the Session. A reading below the stored kwh is ignored.
func (c *Controller) apiChargingPreview(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	live := -1.0
	if raw := r.URL.Query().Get("kwh"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("kwh must be a non-negative number"))
			return
		}
		live = parsed
	}
	c.mu.RLock()
	record := c.state.Objects["sessions"][sessionID]
	if record == nil || fieldText(record.Body, "status") != "ACTIVE" || c.idleCommandLocked(sessionID) != nil {
		c.mu.RUnlock()
		writeError(w, http.StatusConflict, fmt.Errorf("Session is not charging"))
		return
	}
	body := cloneMap(record.Body)
	tariffID, tariff, zone, note := c.tariffForSession(record.Body)
	if tariff != nil {
		tariff = cloneMap(tariff)
	}
	maxPriceMode := c.state.Config.MaxPriceMode
	c.mu.RUnlock()
	if live > number(body["kwh"]) {
		body["kwh"] = live
	}
	at := time.Now().UTC()
	startAt, err := parseOCPITime(fieldText(body, "start_date_time"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("invalid Session start"))
		return
	}
	if at.Before(startAt) {
		at = startAt
	}
	price := priceSessionAs(maxPriceMode, tariff, tariffID, sessionSpans(body, at), startAt.In(zone), at, 0)
	if price.Note == "" {
		price.Note = note
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "as_of": at.Format(time.RFC3339Nano), "kwh": number(body["kwh"]), "total_cost": price.totalCost(), "priced": price.Priced, "note": price.Note})
}

// tariffForSession uses the Location and Tariff captured when the Session started. Caller holds c.mu.
func (c *Controller) tariffForSession(session map[string]any) (string, map[string]any, *time.Location, string) {
	if record := c.state.Objects["sessions"][fieldText(session, "id")]; record != nil {
		if location, ok := record.Snapshot["location"].(map[string]any); ok {
			tariff, _ := record.Snapshot["tariff"].(map[string]any)
			if tariff == nil {
				return fieldText(record.Snapshot, "tariff_id"), nil, sessionZone(location), "no Tariff object when Session started"
			}
			return fieldText(record.Snapshot, "tariff_id"), tariff, sessionZone(location), ""
		}
	}
	location := c.state.Objects["locations"][fieldText(session, "location_id")]
	if location == nil {
		return "", nil, time.UTC, "session location is not in the partner catalogue"
	}
	zone := sessionZone(location.Body)
	connector, err := connectorFromLocation(location.Body, fieldText(session, "evse_uid"), fieldText(session, "connector_id"))
	if err != nil {
		return "", nil, zone, err.Error()
	}
	startedAt, err := parseOCPITime(fieldText(session, "start_date_time"))
	if err != nil {
		startedAt = time.Now()
	}
	id, note := tariffIDForConnector(connector, c.state.Objects["tariffs"], startedAt)
	if record := c.state.Objects["tariffs"][id]; record != nil {
		return id, record.Body, zone, note
	}
	return id, nil, zone, note
}

func tariffCurrency(tariff map[string]any, fallback string) string {
	if currency := fieldText(tariff, "currency"); currency != "" {
		return currency
	}
	return fallback
}

func roundHours(value float64) float64 {
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', 9, 64), 64)
	if err != nil {
		return value
	}
	return rounded
}

func (c *Controller) sendInline(w http.ResponseWriter, r *http.Request, input sendInput) {
	request, err := c.buildSend(input)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	c.mu.RLock()
	token := c.state.TokenC
	c.mu.RUnlock()
	result, err := c.doOCPI(r.Context(), request.Module, request.Method, request.URL, token, true, request.Body)
	if err != nil || result.Envelope == nil || result.Envelope.StatusCode >= 2000 {
		c.markDelivery(input.Module, input.ID, "FAILED", "")
		if err == nil && result.Envelope != nil {
			err = fmt.Errorf("Evolt returned OCPI %d: %s", result.Envelope.StatusCode, result.Envelope.StatusMessage)
		}
		writeError(w, http.StatusBadGateway, firstError(err, "invalid OCPI response"))
		return
	}
	c.markDelivery(input.Module, input.ID, "SENT", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "envelope": result.Envelope, "findings": result.Findings})
}

func (c *Controller) apiEVSEStatus(w http.ResponseWriter, r *http.Request) {
	var input struct {
		LocationID string `json:"location_id"`
		EVSEUID    string `json:"evse_uid"`
		Status     string `json:"status"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	input.Status = strings.ToUpper(input.Status)
	if !oneOf(input.Status, "AVAILABLE", "CHARGING", "OUTOFORDER", "INOPERATIVE", "REMOVED") {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("unsupported EVSE status"))
		return
	}
	timestamp := now()
	c.mu.Lock()
	record := c.state.Objects["locations"][input.LocationID]
	if record == nil {
		c.mu.Unlock()
		writeError(w, http.StatusNotFound, fmt.Errorf("Location not found"))
		return
	}
	evses, _ := record.Body["evses"].([]any)
	found := false
	for _, raw := range evses {
		evse, _ := raw.(map[string]any)
		if fmt.Sprint(evse["uid"]) == input.EVSEUID {
			evse["status"], evse["last_updated"] = input.Status, timestamp
			found = true
			break
		}
	}
	if !found {
		c.mu.Unlock()
		writeError(w, http.StatusNotFound, fmt.Errorf("EVSE not found"))
		return
	}
	record.Body["last_updated"], record.Delivery, record.UpdatedAt = timestamp, "CHANGED_AFTER_SEND", timestamp
	endpoint := endpointURL(c.state.Endpoints, "locations", "RECEIVER")
	config, token := c.state.Config, c.state.TokenC
	c.mu.Unlock()
	c.changed()
	if endpoint == "" || token == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("register and discover Locations RECEIVER first"))
		return
	}
	target := strings.TrimRight(endpoint, "/") + "/" + config.CountryCode + "/" + config.PartyID + "/" + input.LocationID + "/" + input.EVSEUID
	patch := map[string]any{"status": input.Status, "last_updated": timestamp}
	result, err := c.doOCPI(r.Context(), "locations", http.MethodPatch, target, token, true, patch)
	if err != nil || result.Envelope == nil || result.Envelope.StatusCode >= 2000 {
		c.markDelivery("locations", input.LocationID, "FAILED", "")
		if err == nil && result.Envelope != nil {
			err = fmt.Errorf("Evolt returned OCPI %d: %s", result.Envelope.StatusCode, result.Envelope.StatusMessage)
		}
		writeError(w, http.StatusBadGateway, firstError(err, "invalid OCPI response"))
		return
	}
	c.markDelivery("locations", input.LocationID, "SENT", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "envelope": result.Envelope})
}

// cdrTokenFromToken projects a selected or command Token onto the CdrToken a Session holds:
// the Token object has fields (issuer, valid, whitelist) a CdrToken must not repeat.
func cdrTokenFromToken(token map[string]any) map[string]any {
	result := map[string]any{}
	for _, field := range []string{"country_code", "party_id", "uid", "type", "contract_id"} {
		if value := fieldText(token, field); value != "" {
			result[field] = value
		}
	}
	return result
}

func fieldText(source map[string]any, field string) string {
	value, ok := source[field]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// connectorFromLocation finds the Connector a Session is about to use. connector_id is optional in
// a StartSession command unless the EVSE declares START_SESSION_CONNECTOR_REQUIRED (OCPI 2.2.1
// §13.2), so an EVSE with exactly one Connector resolves without being told which.
func connectorFromLocation(location map[string]any, evseUID, connectorID string) (map[string]any, error) {
	evses, _ := location["evses"].([]any)
	known := make([]string, 0, len(evses))
	for _, rawEVSE := range evses {
		evse, _ := rawEVSE.(map[string]any)
		uid := fieldText(evse, "uid")
		known = append(known, uid)
		if uid != evseUID {
			continue
		}
		connectors, _ := evse["connectors"].([]any)
		ids := make([]string, 0, len(connectors))
		for _, rawConnector := range connectors {
			connector, _ := rawConnector.(map[string]any)
			id := fieldText(connector, "id")
			ids = append(ids, id)
			if id == connectorID {
				return connector, nil
			}
		}
		if connectorID != "" {
			return nil, fmt.Errorf("connector %q not found on EVSE %s (has %s)", connectorID, evseUID, strings.Join(ids, ", "))
		}
		if len(connectors) == 1 {
			connector, _ := connectors[0].(map[string]any)
			return connector, nil
		}
		if len(connectors) == 0 {
			return nil, fmt.Errorf("EVSE %s has no connectors", evseUID)
		}
		return nil, fmt.Errorf("EVSE %s has %d connectors (%s) and the command did not send connector_id — pick one", evseUID, len(connectors), strings.Join(ids, ", "))
	}
	return nil, fmt.Errorf("EVSE %q not found in location %s (has %s)", evseUID, fieldText(location, "id"), strings.Join(known, ", "))
}

func cdrFromSession(id string, session, location map[string]any, timestamp string) map[string]any {
	connector, _ := connectorFromLocation(location, fmt.Sprint(session["evse_uid"]), fmt.Sprint(session["connector_id"]))
	if connector == nil {
		connector = map[string]any{}
	}
	end := fmt.Sprint(session["end_date_time"])
	if end == "<nil>" || end == "" {
		end = timestamp
	}
	periods, _ := session["charging_periods"].([]any)
	if len(periods) == 0 {
		periods = []any{map[string]any{"start_date_time": session["start_date_time"], "dimensions": []any{map[string]any{"type": "ENERGY", "volume": session["kwh"]}}}}
	}
	cdr := map[string]any{
		"country_code": session["country_code"], "party_id": session["party_id"], "id": id, "session_id": session["id"],
		"start_date_time": session["start_date_time"], "end_date_time": end, "cdr_token": session["cdr_token"], "auth_method": session["auth_method"],
		"cdr_location": map[string]any{"id": location["id"], "name": location["name"], "address": location["address"], "city": location["city"], "postal_code": location["postal_code"], "country": location["country"], "coordinates": location["coordinates"], "evse_uid": session["evse_uid"], "evse_id": findEVSEID(location, fmt.Sprint(session["evse_uid"])), "connector_id": session["connector_id"], "connector_standard": connector["standard"], "connector_format": connector["format"], "connector_power_type": connector["power_type"]},
		"currency":     session["currency"], "charging_periods": periods, "total_cost": session["total_cost"], "total_energy": session["kwh"], "total_time": elapsedHours(fmt.Sprint(session["start_date_time"]), end), "last_updated": timestamp,
	}
	if reference := fieldText(session, "authorization_reference"); reference != "" {
		cdr["authorization_reference"] = reference
	}
	return cdr
}

func findEVSEID(location map[string]any, uid string) any {
	evses, _ := location["evses"].([]any)
	for _, raw := range evses {
		evse, _ := raw.(map[string]any)
		if fmt.Sprint(evse["uid"]) == uid {
			return evse["evse_id"]
		}
	}
	return nil
}

func elapsedHours(start, end string) float64 {
	startTime, startErr := parseOCPITime(start)
	endTime, endErr := parseOCPITime(end)
	if startErr != nil || endErr != nil || endTime.Before(startTime) {
		return 0
	}
	return endTime.Sub(startTime).Hours()
}

// number reads an OCPI numeric field. Bodies decoded from a browser request carry json.Number while
// bodies restored from PostgreSQL carry float64, so both have to work or a tariff edited in the UI
// would silently price at zero.
func number(value any) float64 {
	switch current := value.(type) {
	case float64:
		return current
	case float32:
		return float64(current)
	case int:
		return float64(current)
	case int64:
		return float64(current)
	case json.Number:
		parsed, err := current.Float64()
		if err != nil {
			return 0
		}
		return parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(current), 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func ocpiShortID() string {
	return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
}
