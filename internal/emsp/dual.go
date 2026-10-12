package emsp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/cpo"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

func (a *Controller) syncDualConnection(ctx context.Context) error {
	if a.profile != "dual" {
		return nil
	}
	run, err := a.store.CurrentRunProfile(ctx, "dual")
	if err != nil {
		return err
	}
	config, err := configFromRun(run)
	if err != nil {
		return err
	}
	secret := func(key string) (string, error) {
		value, err := a.store.GetSecret(ctx, run.ID, key)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return value, err
	}
	tokenA, err := secret("remote_token_a")
	if err != nil {
		return err
	}
	initial, err := secret("mock_initial_token")
	if err != nil {
		return err
	}
	tokenB, err := secret("inbound_token_b")
	if err != nil {
		return err
	}
	tokenC, err := secret("remote_token_c")
	if err != nil {
		return err
	}
	rawEndpoints, err := a.store.RemoteEndpoints(ctx, run.ID)
	if err != nil {
		return err
	}
	endpoints := make([]cpo.Endpoint, 0, len(rawEndpoints))
	for _, item := range rawEndpoints {
		endpoints = append(endpoints, cpo.Endpoint{Identifier: item["identifier"].(string), Role: item["role"].(string), URL: item["url"].(string)})
	}
	remote := map[string]any{}
	for _, role := range config.RemoteRoles {
		if strings.EqualFold(fmt.Sprint(role["role"]), "EMSP") {
			remote = role
			break
		}
	}
	if len(remote) == 0 {
		remote = config.RemoteIdentity
	}
	a.cpo.SyncConnection(cpo.Config{
		EvoltVersionsURL:      config.EvoltVersionsURL,
		PartnerPublicURL:      config.PartnerPublicURL,
		CountryCode:           config.CountryCode,
		PartyID:               config.PartyID,
		BusinessName:          config.BusinessName,
		RawTokenCompatibility: config.RawTokenCompatibility,
		RemoteIdentity:        remote,
	}, run.Status, tokenA, initial, tokenB, tokenC, endpoints)
	return nil
}

func (a *Controller) dualCredentialsAction(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("action") {
	case "get":
		a.checkCredentials(w, r)
	case "rotate":
		a.rotateCredentials(w, r)
	case "unregister":
		a.unregisterDual(w, r)
	default:
		writeAPIError(w, http.StatusNotFound, "unknown credentials action", fmt.Errorf("%s", r.PathValue("action")))
	}
}

func (a *Controller) unregisterDual(w http.ResponseWriter, r *http.Request) {
	run, config, tokenC, ok := a.actionContext(w, r, "remote_token_c")
	if !ok {
		return
	}
	if run.Status != "REGISTERED" {
		writeAPIError(w, http.StatusUnprocessableEntity, "connection is not registered", fmt.Errorf("current status is %s", run.Status))
		return
	}
	endpoint, err := a.store.Endpoint(r.Context(), run.ID, "credentials", "")
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "remote credentials endpoint not discovered", err)
		return
	}
	result, err := a.doOCPI(r.Context(), run, config, "credentials", http.MethodDelete, endpoint, tokenC, false, nil)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "credentials disconnect failed", err)
		return
	}
	if result.Envelope == nil || result.Envelope.StatusCode >= 2000 {
		writeAPIError(w, http.StatusUnprocessableEntity, "remote rejected disconnect", fmt.Errorf("OCPI response did not succeed"))
		return
	}
	for _, key := range []string{"inbound_token_b", "remote_token_c"} {
		if err := a.store.PutSecret(r.Context(), run.ID, key, ""); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "clear credentials", err)
			return
		}
	}
	if err := a.store.UpdateRun(r.Context(), run.ID, run.Name, "UNREGISTERED", config); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save connection", err)
		return
	}
	a.changed()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "response": ocpi.Success(nil)})
}
