package cpo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpicore/rules"
)

func (c *Controller) tokensReceiver(r *http.Request, body map[string]any) inboundResponse {
	countryCode := strings.ToUpper(r.PathValue("country_code"))
	partyID := strings.ToUpper(r.PathValue("party_id"))
	uid := r.PathValue("uid")
	tokenType := strings.ToUpper(r.URL.Query().Get("type"))
	if tokenType == "" {
		tokenType = "RFID"
	}
	key := tokenObjectID(countryCode, partyID, uid, tokenType)
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.state.Objects["tokens"][key]
	switch r.Method {
	case http.MethodGet:
		if current == nil {
			return inboundResponse{HTTPStatus: http.StatusNotFound, Envelope: ocpi.Error(2004, "Unknown Token")}
		}
		return inboundResponse{Envelope: ocpi.Success(cloneMap(current.Body))}
	case http.MethodPut:
		findings := validateTokenPath(body, countryCode, partyID, uid, tokenType)
		if ocpi.HasErrors(findings) {
			return inboundResponse{Envelope: ocpi.Error(2001, "invalid Token"), Findings: findings}
		}
		c.state.Objects["tokens"][key] = &Record{Body: cloneMap(body), Delivery: "RECEIVED", Findings: findings, UpdatedAt: now()}
		return inboundResponse{Envelope: ocpi.Success(nil), Findings: findings}
	case http.MethodPatch:
		if current == nil {
			return inboundResponse{HTTPStatus: http.StatusNotFound, Envelope: ocpi.Error(2003, "Token not found")}
		}
		if body["last_updated"] == nil {
			return inboundResponse{Envelope: ocpi.Error(2001, "last_updated is required"), Findings: []ocpi.Finding{{Level: "error", Rule: "OCPI 2.2.1 §12.2.1.3", Message: "PATCH requires last_updated"}}}
		}
		merged := mergeObject(current.Body, body)
		findings := validateTokenPath(merged, countryCode, partyID, uid, tokenType)
		if ocpi.HasErrors(findings) {
			return inboundResponse{Envelope: ocpi.Error(2001, "invalid Token patch"), Findings: findings}
		}
		current.Body, current.Delivery, current.Findings, current.UpdatedAt = merged, "RECEIVED", findings, now()
		return inboundResponse{Envelope: ocpi.Success(nil), Findings: findings}
	default:
		return inboundResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "method not allowed")}
	}
}

func validateTokenPath(body map[string]any, countryCode, partyID, uid, tokenType string) []ocpi.Finding {
	findings := rules.Validate("tokens", body, rules.Owner{CountryCode: countryCode, PartyID: partyID})
	for field, expected := range map[string]string{"country_code": countryCode, "party_id": partyID, "uid": uid, "type": tokenType} {
		if actual := strings.ToUpper(fmt.Sprint(body[field])); actual != strings.ToUpper(expected) {
			findings = append(findings, ocpi.Finding{Level: "error", Rule: "OCPI 2.2.1 §12.2.1.2", Message: fmt.Sprintf("%s in URL and body must match", field)})
		}
	}
	return findings
}

func (c *Controller) commandsReceiver(r *http.Request, body map[string]any) inboundResponse {
	commandType := strings.ToUpper(r.PathValue("command"))
	if !oneOf(commandType, "CANCEL_RESERVATION", "RESERVE_NOW", "START_SESSION", "STOP_SESSION", "UNLOCK_CONNECTOR") {
		return inboundResponse{Envelope: ocpi.Success(map[string]any{"result": "NOT_SUPPORTED", "timeout": 0})}
	}
	findings := validateCommand(commandType, body)
	if ocpi.HasErrors(findings) {
		return inboundResponse{Envelope: ocpi.Error(2001, "invalid Command request"), Findings: findings}
	}
	c.mu.Lock()
	auto := c.state.Config.AutoCommandFlow
	if auto && commandType == "STOP_SESSION" {
		session := c.state.Objects["sessions"][fieldText(body, "session_id")]
		if session == nil {
			c.mu.Unlock()
			return inboundResponse{Envelope: ocpi.Success(map[string]any{"result": "UNKNOWN_SESSION", "timeout": 0}), Findings: findings}
		}
		if fieldText(session.Body, "status") != "ACTIVE" {
			c.mu.Unlock()
			return inboundResponse{Envelope: ocpi.Success(map[string]any{"result": "REJECTED", "timeout": 0}), Findings: findings}
		}
		for _, existing := range c.state.Commands {
			if existing.Type == "STOP_SESSION" && fieldText(existing.Request, "session_id") == fieldText(body, "session_id") && fieldText(existing.Result, "automation_error") == "" && fieldText(existing.Result, "cdr_id") == "" {
				c.mu.Unlock()
				return inboundResponse{Envelope: ocpi.Success(map[string]any{"result": "REJECTED", "timeout": 0}), Findings: findings}
			}
		}
	}
	commandID := ocpi.NewID()
	timestamp := now()
	command := &CommandRecord{ID: commandID, Type: commandType, Status: "ACCEPTED", Request: cloneMap(body), ResponseURL: fmt.Sprint(body["response_url"]), CreatedAt: timestamp, UpdatedAt: timestamp}
	c.state.Commands[commandID] = command
	c.mu.Unlock()
	c.changed()
	if auto && oneOf(commandType, "START_SESSION", "STOP_SESSION") {
		go c.autoCommand(commandID)
	} else {
		go func() {
			timer := time.NewTimer(750 * time.Millisecond)
			defer timer.Stop()
			<-timer.C
			_ = c.sendCommandResult(context.Background(), commandID, "ACCEPTED")
		}()
	}
	return inboundResponse{Envelope: ocpi.Success(map[string]any{"result": "ACCEPTED", "timeout": 30}), Findings: findings}
}

func validateCommand(commandType string, body map[string]any) []ocpi.Finding {
	required := map[string][]string{
		"CANCEL_RESERVATION": {"response_url", "reservation_id"},
		"RESERVE_NOW":        {"response_url", "token", "expiry_date", "reservation_id", "location_id"},
		"START_SESSION":      {"response_url", "token", "location_id"},
		"STOP_SESSION":       {"response_url", "session_id"},
		"UNLOCK_CONNECTOR":   {"response_url", "location_id", "evse_uid", "connector_id"},
	}
	var findings []ocpi.Finding
	for _, field := range required[commandType] {
		if body[field] == nil || fmt.Sprint(body[field]) == "" {
			findings = append(findings, ocpi.Finding{Level: "error", Rule: "OCPI 2.2.1 §13.3", Message: "missing required field: " + field})
		}
	}
	if commandType == "START_SESSION" && fieldText(body, "connector_id") != "" && fieldText(body, "evse_uid") == "" {
		findings = append(findings, ocpi.Finding{Level: "error", Rule: "OCPI 2.2.1 §13.3.5", Message: "evse_uid is required when connector_id is set"})
	}
	if raw := fmt.Sprint(body["response_url"]); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
			findings = append(findings, ocpi.Finding{Level: "error", Rule: "OCPI 2.2.1 §13.3", Message: "response_url must be an absolute HTTP(S) URL"})
		}
	}
	return findings
}

func (c *Controller) apiCommandResult(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Result string `json:"result"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	input.Result = strings.ToUpper(input.Result)
	if !oneOf(input.Result, "ACCEPTED", "CANCELED_RESERVATION", "EVSE_OCCUPIED", "EVSE_INOPERATIVE", "FAILED", "NOT_SUPPORTED", "REJECTED", "TIMEOUT", "UNKNOWN_RESERVATION") {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("invalid CommandResultType"))
		return
	}
	if err := c.sendCommandResult(r.Context(), r.PathValue("id"), input.Result); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (c *Controller) sendCommandResult(ctx context.Context, id, commandResult string) error {
	c.mu.RLock()
	command := c.state.Commands[id]
	token := c.state.TokenC
	c.mu.RUnlock()
	if command == nil {
		return fmt.Errorf("Command not found")
	}
	if token == "" {
		return fmt.Errorf("no active Token C")
	}
	payload := map[string]any{"result": commandResult}
	result, err := c.doOCPI(ctx, "commands", http.MethodPost, command.ResponseURL, token, true, payload)
	if err != nil || result.Envelope == nil || result.Envelope.StatusCode >= 2000 {
		return firstError(err, "invalid CommandResult response")
	}
	c.mu.Lock()
	if command.Result == nil {
		command.Result = map[string]any{}
	}
	command.Status, command.Result["result"], command.UpdatedAt = commandResult, commandResult, now()
	c.mu.Unlock()
	c.changed()
	return nil
}

func tokenObjectID(countryCode, partyID, uid, tokenType string) string {
	return strings.Join([]string{countryCode, partyID, uid, tokenType}, "/")
}

func mergeObject(current, patch map[string]any) map[string]any {
	result := cloneMap(current)
	for key, value := range patch {
		if oldMap, ok := result[key].(map[string]any); ok {
			if newMap, ok := value.(map[string]any); ok {
				result[key] = mergeObject(oldMap, newMap)
				continue
			}
		}
		result[key] = value
	}
	return result
}

func (c *Controller) sessionChargingPreferences(r *http.Request, body map[string]any) inboundResponse {
	profileType := strings.ToUpper(fmt.Sprint(body["profile_type"]))
	if !oneOf(profileType, "CHEAP", "FAST", "GREEN", "REGULAR") {
		return inboundResponse{Envelope: ocpi.Success("PROFILE_TYPE_NOT_SUPPORTED")}
	}
	c.mu.RLock()
	session := c.state.Objects["sessions"][r.PathValue("session_id")]
	if session == nil {
		c.mu.RUnlock()
		return inboundResponse{HTTPStatus: http.StatusNotFound, Envelope: ocpi.Error(2003, "Session not found")}
	}
	location := c.state.Objects["locations"][fmt.Sprint(session.Body["location_id"])]
	capable := location != nil && evseHasCapability(location.Body, fmt.Sprint(session.Body["evse_uid"]), "CHARGING_PREFERENCES_CAPABLE")
	c.mu.RUnlock()
	if !capable {
		return inboundResponse{HTTPStatus: http.StatusNotFound, Envelope: ocpi.Error(2001, "EVSE does not support Charging Preferences")}
	}
	if departure := fmt.Sprint(body["departure_time"]); departure != "" && departure != "<nil>" {
		if err := rules.ValidateDateTime(departure); err != nil {
			return inboundResponse{Envelope: ocpi.Error(2001, "invalid departure_time"), Findings: []ocpi.Finding{{Level: "error", Rule: "OCPI 2.2.1 §9.3.2", Message: err.Error()}}}
		}
	}
	return inboundResponse{Envelope: ocpi.Success("ACCEPTED")}
}

func evseHasCapability(location map[string]any, uid, capability string) bool {
	evses, _ := location["evses"].([]any)
	for _, rawEVSE := range evses {
		evse, _ := rawEVSE.(map[string]any)
		if fmt.Sprint(evse["uid"]) != uid {
			continue
		}
		capabilities, _ := evse["capabilities"].([]any)
		for _, value := range capabilities {
			if fmt.Sprint(value) == capability {
				return true
			}
		}
	}
	return false
}
