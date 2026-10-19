package emsp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

func (a *Controller) versions(_ http.ResponseWriter, _ *http.Request, _ store.Run, config Config, _ map[string]any) handlerResponse {
	base := strings.TrimRight(config.PartnerPublicURL, "/")
	return handlerResponse{Envelope: ocpi.Success([]map[string]any{{"version": ocpi.Version, "url": base + a.ocpiRoot() + "/2.2.1"}})}
}

func (a *Controller) versionDetails(_ http.ResponseWriter, _ *http.Request, _ store.Run, config Config, _ map[string]any) handlerResponse {
	if a.profile == "dual" {
		return handlerResponse{Envelope: ocpi.Success(dualVersionDetails(config))}
	}
	return handlerResponse{Envelope: ocpi.Success(mockVersionDetails(config))}
}

func dualVersionDetails(config Config) map[string]any {
	base := strings.TrimRight(config.PartnerPublicURL, "/")
	endpoints := []map[string]any{{"identifier": "credentials", "role": "SENDER", "url": base + "/ocpi/emsp/2.2.1/credentials"}}
	for _, module := range []string{"locations", "tariffs", "sessions", "cdrs"} {
		endpoints = append(endpoints,
			map[string]any{"identifier": module, "role": "SENDER", "url": base + "/ocpi/cpo/2.2.1/" + module},
			map[string]any{"identifier": module, "role": "RECEIVER", "url": base + "/ocpi/emsp/2.2.1/" + module})
	}
	endpoints = append(endpoints,
		map[string]any{"identifier": "tokens", "role": "SENDER", "url": base + "/ocpi/emsp/2.2.1/tokens"},
		map[string]any{"identifier": "tokens", "role": "RECEIVER", "url": base + "/ocpi/cpo/2.2.1/tokens"},
		map[string]any{"identifier": "commands", "role": "SENDER", "url": base + "/ocpi/emsp/2.2.1/commands/callback"},
		map[string]any{"identifier": "commands", "role": "RECEIVER", "url": base + "/ocpi/cpo/2.2.1/commands"})
	return map[string]any{"version": ocpi.Version, "endpoints": endpoints}
}

func (a *Controller) localRoles(config Config) []map[string]any {
	role := func(name string) map[string]any {
		return map[string]any{"role": name, "party_id": config.PartyID, "country_code": config.CountryCode, "business_details": map[string]any{"name": config.BusinessName}}
	}
	if a.profile == "dual" {
		return []map[string]any{role("CPO"), role("EMSP")}
	}
	return []map[string]any{role("EMSP")}
}

func mockVersionDetails(config Config) map[string]any {
	base := strings.TrimRight(config.PartnerPublicURL, "/") + "/ocpi/emsp/2.2.1"
	return map[string]any{
		"version": ocpi.Version,
		"endpoints": []map[string]any{
			{"identifier": "credentials", "role": "SENDER", "url": base + "/credentials"},
			{"identifier": "locations", "role": "RECEIVER", "url": base + "/locations"},
			{"identifier": "tariffs", "role": "RECEIVER", "url": base + "/tariffs"},
			{"identifier": "sessions", "role": "RECEIVER", "url": base + "/sessions"},
			{"identifier": "cdrs", "role": "RECEIVER", "url": base + "/cdrs"},
			{"identifier": "tokens", "role": "SENDER", "url": base + "/tokens"},
			{"identifier": "commands", "role": "SENDER", "url": base + "/commands/callback"},
		},
	}
}

func (a *Controller) credentials(_ http.ResponseWriter, r *http.Request, run store.Run, config Config, body map[string]any) handlerResponse {
	credentials := func(token string) map[string]any {
		return map[string]any{
			"token": token,
			"url":   a.versionsURL(config),
			"roles": a.localRoles(config),
		}
	}
	switch r.Method {
	case http.MethodGet:
		if a.profile == "dual" && run.Status != "REGISTERED" {
			return handlerResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "not registered")}
		}
		token, err := a.store.GetSecret(r.Context(), run.ID, "inbound_token_b")
		if err != nil {
			return handlerResponse{HTTPStatus: 404, Envelope: ocpi.Error(2000, "credentials not registered")}
		}
		return handlerResponse{Envelope: ocpi.Success(credentials(token))}
	case http.MethodPost:
		if run.Status == "REGISTERED" {
			return handlerResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "already registered")}
		}
		return a.acceptRemoteCredentials(r, run, config, body, credentials, false)
	case http.MethodPut:
		if run.Status != "REGISTERED" {
			return handlerResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "not registered")}
		}
		return a.acceptRemoteCredentials(r, run, config, body, credentials, true)
	case http.MethodDelete:
		if run.Status != "REGISTERED" {
			return handlerResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "not registered")}
		}
		if err := a.store.UpdateRun(r.Context(), run.ID, run.Name, "UNREGISTERED", config); err != nil {
			return storageError(err)
		}
		if a.profile == "dual" {
			for _, key := range []string{"inbound_token_b", "remote_token_c"} {
				if err := a.store.PutSecret(r.Context(), run.ID, key, ""); err != nil {
					return storageError(err)
				}
			}
			if err := a.syncDualConnection(r.Context()); err != nil {
				return storageError(err)
			}
		}
		return handlerResponse{Envelope: ocpi.Success(nil)}
	default:
		return handlerResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "method not allowed")}
	}
}

func (a *Controller) acceptRemoteCredentials(r *http.Request, run store.Run, config Config, body map[string]any, response func(string) map[string]any, update bool) handlerResponse {
	findings := ocpi.Required(body, "S1 §7.3.1", "token", "url", "roles")
	if ocpi.HasErrors(findings) {
		return handlerResponse{Envelope: ocpi.Error(2001, "invalid Credentials object"), Findings: findings}
	}
	remoteToken := fmt.Sprint(body["token"])
	_, _, discoveryFindings, err := a.fetchRemoteDetails(r.Context(), run, config, fmt.Sprint(body["url"]), remoteToken)
	findings = append(findings, discoveryFindings...)
	if err != nil {
		findings = append(findings, ocpi.Finding{Level: "error", Rule: "S1 §7.1.5", Message: "cannot fetch peer versions and version details: " + err.Error()})
		return handlerResponse{Envelope: ocpi.Error(3001, "cannot fetch peer OCPI endpoints"), Findings: findings}
	}
	if err := a.store.PutSecret(r.Context(), run.ID, "remote_token_c", remoteToken); err != nil {
		return handlerResponse{HTTPStatus: 500, Envelope: ocpi.Error(3000, "cannot store credentials")}
	}
	inboundToken := ocpi.NewToken()
	if err := a.store.PutSecret(r.Context(), run.ID, "inbound_token_b", inboundToken); err != nil {
		return handlerResponse{HTTPStatus: 500, Envelope: ocpi.Error(3000, "cannot generate credentials")}
	}
	config.RemoteRoles = nil
	if roles, ok := body["roles"].([]any); ok {
		for _, raw := range roles {
			if role, ok := raw.(map[string]any); ok {
				config.RemoteRoles = append(config.RemoteRoles, role)
				if config.RemoteIdentity == nil || strings.EqualFold(fmt.Sprint(role["role"]), "CPO") {
					config.RemoteIdentity = role
				}
			}
		}
	}
	httpStatus := http.StatusCreated
	if update {
		httpStatus = http.StatusOK
	}
	if err := a.store.UpdateRunAndDeleteSecret(r.Context(), run.ID, run.Name, "REGISTERED", config, "mock_initial_token"); err != nil {
		return storageError(err)
	}
	if a.profile == "dual" {
		if err := a.syncDualConnection(r.Context()); err != nil {
			return storageError(err)
		}
	}
	return handlerResponse{HTTPStatus: httpStatus, Envelope: ocpi.Success(response(inboundToken)), Findings: findings}
}

func (a *Controller) locations(_ http.ResponseWriter, r *http.Request, run store.Run, config Config, body map[string]any) handlerResponse {
	parts := pathParts(r.URL.Path, "/ocpi/emsp/2.2.1/locations/")
	if len(parts) < 3 || len(parts) > 5 {
		return clientError("invalid Locations URL", "S1 §8.2.2")
	}
	cc, pid, locationID := parts[0], parts[1], parts[2]
	findings := validateRemoteOwner(config, cc, pid)
	if r.Method == http.MethodGet {
		object, err := a.store.Object(r.Context(), run.ID, "locations", cc, pid, locationID)
		if err != nil {
			return notFound("Location not found")
		}
		selected, err := selectLocationObject(object, parts[3:])
		if err != nil {
			return notFound(err.Error())
		}
		return handlerResponse{Envelope: ocpi.Success(selected), Findings: findings}
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		return methodNotAllowed()
	}
	patch := r.Method == http.MethodPatch
	if patch {
		findings = append(findings, ocpi.Required(body, "S1 §8.2.2.3", "last_updated")...)
	}
	if len(parts) == 3 {
		_, existingErr := a.store.Object(r.Context(), run.ID, "locations", cc, pid, locationID)
		created := existingErr == sql.ErrNoRows
		if !patch {
			findings = append(findings, ocpi.Required(body, "S1 §8.3.1", "country_code", "party_id", "id", "publish", "address", "city", "country", "coordinates", "time_zone", "last_updated")...)
		}
		findings = append(findings, ocpi.OwnerMatches(body, cc, pid, locationID, "id", "S1 §8.2.2.2")...)
		if config.Strict && ocpi.HasErrors(findings) {
			return handlerResponse{Envelope: ocpi.Error(2001, "invalid Location"), Findings: findings}
		}
		if err := a.store.PutObject(r.Context(), run.ID, "locations", cc, pid, locationID, body, patch); err != nil {
			return storageError(err)
		}
		return createdOrOK(created, findings)
	}
	location, err := a.store.Object(r.Context(), run.ID, "locations", cc, pid, locationID)
	if err != nil {
		return notFound("parent Location not found")
	}
	_, childErr := selectLocationObject(location, parts[3:])
	created := childErr != nil
	if len(parts) == 4 {
		if !patch {
			findings = append(findings, ocpi.Required(body, "S1 §8.3.2", "uid", "status", "connectors", "last_updated")...)
		}
		if fmt.Sprint(body["uid"]) != "<nil>" && fmt.Sprint(body["uid"]) != parts[3] {
			findings = append(findings, ocpi.Finding{Level: "error", Rule: "S1 §8.2.2.2", Message: "EVSE uid does not match URL"})
		}
	} else {
		if !patch {
			findings = append(findings, ocpi.Required(body, "S1 §8.3.3", "id", "standard", "format", "power_type", "max_voltage", "max_amperage", "last_updated")...)
		}
		if fmt.Sprint(body["id"]) != "<nil>" && fmt.Sprint(body["id"]) != parts[4] {
			findings = append(findings, ocpi.Finding{Level: "error", Rule: "S1 §8.2.2.2", Message: "Connector id does not match URL"})
		}
	}
	if config.Strict && ocpi.HasErrors(findings) {
		return handlerResponse{Envelope: ocpi.Error(2001, "invalid Location child"), Findings: findings}
	}
	if err := mutateLocation(location, parts[3:], body, patch); err != nil {
		return clientError(err.Error(), "S1 §8.2.2")
	}
	if updated := body["last_updated"]; updated != nil {
		location["last_updated"] = updated
	}
	if err := a.store.PutObject(r.Context(), run.ID, "locations", cc, pid, locationID, location, false); err != nil {
		return storageError(err)
	}
	return createdOrOK(created, findings)
}

func (a *Controller) tariffs(_ http.ResponseWriter, r *http.Request, run store.Run, config Config, body map[string]any) handlerResponse {
	parts := pathParts(r.URL.Path, "/ocpi/emsp/2.2.1/tariffs/")
	if len(parts) != 3 {
		return clientError("invalid Tariffs URL", "S1 §11.2.2")
	}
	cc, pid, id := parts[0], parts[1], parts[2]
	findings := validateRemoteOwner(config, cc, pid)
	switch r.Method {
	case http.MethodGet:
		object, err := a.store.Object(r.Context(), run.ID, "tariffs", cc, pid, id)
		if err != nil {
			return notFound("Tariff not found")
		}
		return handlerResponse{Envelope: ocpi.Success(object), Findings: findings}
	case http.MethodPut:
		_, existingErr := a.store.Object(r.Context(), run.ID, "tariffs", cc, pid, id)
		created := existingErr == sql.ErrNoRows
		findings = append(findings, ocpi.Required(body, "S1 §11.3.1", "country_code", "party_id", "id", "currency", "elements", "last_updated")...)
		findings = append(findings, ocpi.OwnerMatches(body, cc, pid, id, "id", "S1 §11.2.2.2")...)
		if config.Strict && ocpi.HasErrors(findings) {
			return handlerResponse{Envelope: ocpi.Error(2001, "invalid Tariff"), Findings: findings}
		}
		if err := a.store.PutObject(r.Context(), run.ID, "tariffs", cc, pid, id, body, false); err != nil {
			return storageError(err)
		}
		return createdOrOK(created, findings)
	case http.MethodDelete:
		if err := a.store.DeleteObject(r.Context(), run.ID, "tariffs", cc, pid, id); err != nil {
			return storageError(err)
		}
		return handlerResponse{Envelope: ocpi.Success(nil), Findings: findings}
	default:
		return methodNotAllowed()
	}
}

func (a *Controller) sessions(_ http.ResponseWriter, r *http.Request, run store.Run, config Config, body map[string]any) handlerResponse {
	parts := pathParts(r.URL.Path, "/ocpi/emsp/2.2.1/sessions/")
	if len(parts) != 3 {
		return clientError("invalid Sessions URL", "S1 §9.2.2")
	}
	cc, pid, id := parts[0], parts[1], parts[2]
	findings := validateRemoteOwner(config, cc, pid)
	if r.Method == http.MethodGet {
		object, err := a.store.Object(r.Context(), run.ID, "sessions", cc, pid, id)
		if err != nil {
			return notFound("Session not found")
		}
		return handlerResponse{Envelope: ocpi.Success(object), Findings: findings}
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		return methodNotAllowed()
	}
	patch := r.Method == http.MethodPatch
	current, existingErr := a.store.Object(r.Context(), run.ID, "sessions", cc, pid, id)
	created := existingErr == sql.ErrNoRows
	if existingErr == nil && fmt.Sprint(current["status"]) == "COMPLETED" {
		findings = append(findings, ocpi.Finding{Level: "error", Rule: "S1 §9.1.1", Message: "COMPLETED Session is final and cannot be modified"})
	}
	if patch {
		findings = append(findings, ocpi.Required(body, "S1 §9.2.2.3", "last_updated")...)
	} else {
		findings = append(findings, ocpi.Required(body, "S1 §9.3.1", "country_code", "party_id", "id", "start_date_time", "kwh", "cdr_token", "auth_method", "location_id", "evse_uid", "connector_id", "currency", "status", "last_updated")...)
		findings = append(findings, ocpi.OwnerMatches(body, cc, pid, id, "id", "S1 §9.2.2.2")...)
	}
	if config.Strict && ocpi.HasErrors(findings) {
		return handlerResponse{Envelope: ocpi.Error(2001, "invalid Session"), Findings: findings}
	}
	if err := a.store.PutObject(r.Context(), run.ID, "sessions", cc, pid, id, body, patch); err != nil {
		if err == sql.ErrNoRows {
			return notFound("Session not found for PATCH")
		}
		return storageError(err)
	}
	return createdOrOK(created, findings)
}

func (a *Controller) cdrs(_ http.ResponseWriter, r *http.Request, run store.Run, config Config, body map[string]any) handlerResponse {
	base := "/ocpi/emsp/2.2.1/cdrs"
	parts := pathParts(r.URL.Path, base+"/")
	if r.Method == http.MethodPost && (r.URL.Path == base || r.URL.Path == base+"/") {
		findings := ocpi.Required(body, "S1 §10.3.1", "country_code", "party_id", "id", "start_date_time", "end_date_time", "cdr_token", "auth_method", "cdr_location", "currency", "charging_periods", "total_cost", "total_energy", "total_time", "last_updated")
		cc, pid, id := fmt.Sprint(body["country_code"]), fmt.Sprint(body["party_id"]), fmt.Sprint(body["id"])
		findings = append(findings, validateRemoteOwner(config, cc, pid)...)
		if credit, _ := body["credit"].(bool); credit && fmt.Sprint(body["credit_reference_id"]) == "<nil>" {
			findings = append(findings, ocpi.Finding{Level: "error", Rule: "S1 §10.1.1", Message: "Credit CDR requires credit_reference_id"})
		}
		if config.Strict && ocpi.HasErrors(findings) {
			return handlerResponse{Envelope: ocpi.Error(2001, "invalid CDR"), Findings: findings}
		}
		if err := a.store.PutObject(r.Context(), run.ID, "cdrs", cc, pid, id, body, false); err != nil {
			return handlerResponse{Envelope: ocpi.Error(2000, "CDR already exists or cannot be stored"), Findings: append(findings, ocpi.Finding{Level: "error", Rule: "S1 §10.1", Message: "CDR is immutable: " + err.Error()})}
		}
		headers := http.Header{}
		headers.Set("Location", strings.TrimRight(config.PartnerPublicURL, "/")+base+"/"+url.PathEscape(id))
		return handlerResponse{HTTPStatus: http.StatusCreated, Envelope: ocpi.Success(nil), Headers: headers, Findings: findings}
	}
	if r.Method == http.MethodGet && len(parts) == 1 {
		object, err := a.findCDRByID(r, run.ID, parts[0])
		if err != nil {
			return notFound("CDR not found")
		}
		return handlerResponse{Envelope: ocpi.Success(object)}
	}
	if r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		return handlerResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "CDRs cannot be changed or deleted")}
	}
	return methodNotAllowed()
}

func (a *Controller) tokens(_ http.ResponseWriter, r *http.Request, run store.Run, _ Config, body map[string]any) handlerResponse {
	base := "/ocpi/emsp/2.2.1/tokens"
	if r.Method == http.MethodGet && (r.URL.Path == base || r.URL.Path == base+"/") {
		dateFrom, dateTo, offset, limit, findings := ocpi.ParsePagination(r)
		if ocpi.HasErrors(findings) {
			return handlerResponse{Envelope: ocpi.Error(2001, "invalid pagination"), Findings: findings}
		}
		items, total, err := a.store.Tokens(r.Context(), run.ID, dateFrom, dateTo, offset, limit)
		if err != nil {
			return storageError(err)
		}
		headers := paginationHeaders(r, total, offset, limit, len(items))
		return handlerResponse{Envelope: ocpi.Success(items), Headers: headers, Findings: findings}
	}
	parts := pathParts(r.URL.Path, base+"/")
	if r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "authorize" {
		tokenType := r.URL.Query().Get("type")
		if tokenType == "" {
			tokenType = "RFID"
		}
		token, err := a.store.Token(r.Context(), run.ID, parts[0], tokenType)
		if err != nil {
			return handlerResponse{HTTPStatus: http.StatusNotFound, Envelope: ocpi.Error(2004, "Unknown Token")}
		}
		allowed := "ALLOWED"
		if valid, ok := token["valid"].(bool); ok && !valid {
			allowed = "BLOCKED"
		}
		data := map[string]any{"allowed": allowed, "token": token, "authorization_reference": ocpi.NewID()}
		if len(body) > 0 {
			data["location"] = body
		}
		return handlerResponse{Envelope: ocpi.Success(data)}
	}
	return methodNotAllowed()
}

func (a *Controller) commandCallback(_ http.ResponseWriter, r *http.Request, run store.Run, _ Config, body map[string]any) handlerResponse {
	if r.Method != http.MethodPost {
		return methodNotAllowed()
	}
	parts := pathParts(r.URL.Path, "/ocpi/emsp/2.2.1/commands/callback/")
	if len(parts) != 1 {
		return clientError("invalid command callback URL", "S1 §13.2.2")
	}
	findings := ocpi.Required(body, "S1 §13.3.3", "result")
	if ocpi.HasErrors(findings) {
		return handlerResponse{Envelope: ocpi.Error(2001, "invalid CommandResult"), Findings: findings}
	}
	if err := a.store.UpdateCommandAsync(r.Context(), run.ID, parts[0], fmt.Sprint(body["result"]), body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("command not found")
		}
		return storageError(err)
	}
	return handlerResponse{Envelope: ocpi.Success(nil), Findings: findings}
}

func (a *Controller) findCDRByID(r *http.Request, runID, id string) (map[string]any, error) {
	// A CDR lookup has no business decrypting credentials, so it never asks for them.
	state, err := a.store.State(r.Context(), runID, false)
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(state["cdrs"], &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if fmt.Sprint(row["object_id"]) == id {
			if body, ok := row["body"].(map[string]any); ok {
				return body, nil
			}
		}
	}
	return nil, sql.ErrNoRows
}

func validateRemoteOwner(config Config, cc, pid string) []ocpi.Finding {
	if config.RemoteIdentity == nil {
		return nil
	}
	findings := []ocpi.Finding{}
	if expected := fmt.Sprint(config.RemoteIdentity["country_code"]); expected != "" && expected != "<nil>" && cc != expected {
		findings = append(findings, ocpi.Finding{Level: "error", Rule: "S1 §4.1.5", Message: "country_code is not owned by registered CPO"})
	}
	if expected := fmt.Sprint(config.RemoteIdentity["party_id"]); expected != "" && expected != "<nil>" && pid != expected {
		findings = append(findings, ocpi.Finding{Level: "error", Rule: "S1 §4.1.5", Message: "party_id is not owned by registered CPO"})
	}
	return findings
}

func mutateLocation(location map[string]any, childPath []string, body map[string]any, patch bool) error {
	if len(childPath) < 1 || len(childPath) > 2 {
		return fmt.Errorf("invalid Location child path")
	}
	evses, _ := location["evses"].([]any)
	evseIndex := findObject(evses, "uid", childPath[0])
	if len(childPath) == 1 {
		if evseIndex < 0 {
			if patch {
				return fmt.Errorf("EVSE not found for PATCH")
			}
			evses = append(evses, body)
		} else if patch {
			evses[evseIndex] = store.Merge(evses[evseIndex].(map[string]any), body, false)
		} else {
			evses[evseIndex] = body
		}
		location["evses"] = evses
		return nil
	}
	if evseIndex < 0 {
		return fmt.Errorf("parent EVSE not found")
	}
	evse := evses[evseIndex].(map[string]any)
	connectors, _ := evse["connectors"].([]any)
	connectorIndex := findObject(connectors, "id", childPath[1])
	if connectorIndex < 0 {
		if patch {
			return fmt.Errorf("Connector not found for PATCH")
		}
		connectors = append(connectors, body)
	} else if patch {
		connectors[connectorIndex] = store.Merge(connectors[connectorIndex].(map[string]any), body, false)
	} else {
		connectors[connectorIndex] = body
	}
	if updated := body["last_updated"]; updated != nil {
		evse["last_updated"] = updated
	}
	evse["connectors"] = connectors
	evses[evseIndex] = evse
	location["evses"] = evses
	return nil
}

func selectLocationObject(location map[string]any, childPath []string) (any, error) {
	if len(childPath) == 0 {
		return location, nil
	}
	evses, _ := location["evses"].([]any)
	evseIndex := findObject(evses, "uid", childPath[0])
	if evseIndex < 0 {
		return nil, fmt.Errorf("EVSE not found")
	}
	evse := evses[evseIndex].(map[string]any)
	if len(childPath) == 1 {
		return evse, nil
	}
	connectors, _ := evse["connectors"].([]any)
	connectorIndex := findObject(connectors, "id", childPath[1])
	if connectorIndex < 0 {
		return nil, fmt.Errorf("Connector not found")
	}
	return connectors[connectorIndex], nil
}

func findObject(items []any, field, value string) int {
	for index, item := range items {
		if object, ok := item.(map[string]any); ok && fmt.Sprint(object[field]) == value {
			return index
		}
	}
	return -1
}

func pathParts(path, prefix string) []string {
	remainder := strings.TrimPrefix(path, prefix)
	if remainder == path || remainder == "" {
		return nil
	}
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	for index := range parts {
		decoded, _ := url.PathUnescape(parts[index])
		parts[index] = decoded
	}
	return parts
}

func paginationHeaders(r *http.Request, total, offset, limit, returned int) http.Header {
	headers := http.Header{}
	headers.Set("X-Total-Count", strconv.Itoa(total))
	headers.Set("X-Limit", strconv.Itoa(limit))
	if offset+returned < total {
		next := *r.URL
		query := next.Query()
		query.Set("offset", strconv.Itoa(offset+limit))
		query.Set("limit", strconv.Itoa(limit))
		next.RawQuery = query.Encode()
		headers.Set("Link", "<"+absoluteURL(r, &next)+">; rel=\"next\"")
	}
	return headers
}

func createdOrOK(created bool, findings []ocpi.Finding) handlerResponse {
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return handlerResponse{HTTPStatus: status, Envelope: ocpi.Success(nil), Findings: findings}
}

func clientError(message, rule string) handlerResponse {
	return handlerResponse{Envelope: ocpi.Error(2001, message), Findings: []ocpi.Finding{{Level: "error", Rule: rule, Message: message}}}
}
func notFound(message string) handlerResponse {
	return handlerResponse{HTTPStatus: http.StatusNotFound, Envelope: ocpi.Error(2000, message)}
}
func methodNotAllowed() handlerResponse {
	return handlerResponse{HTTPStatus: http.StatusMethodNotAllowed, Envelope: ocpi.Error(2000, "method not allowed")}
}
func storageError(err error) handlerResponse {
	return handlerResponse{HTTPStatus: http.StatusInternalServerError, Envelope: ocpi.Error(3000, "storage error"), Findings: []ocpi.Finding{{Level: "error", Rule: "storage", Message: err.Error()}}}
}
