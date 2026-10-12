package cpo

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

// FavoriteToken is a driver the tester keeps at hand. It is a copy of a Token, not a pointer into
// the Tokens module: a run reset wipes the synced Tokens but the shortlist has to survive, because
// the whole point is to stop re-typing the same uid/contract_id for every draft Session.
//
// ID is the Tokens-module record id (country/party/uid/type), not the bare OCPI Token uid — two
// parties may issue the same uid, and the record id is what the catalogue is keyed by.
type FavoriteToken struct {
	ID          string         `json:"id"`
	Label       string         `json:"label"`
	Token       map[string]any `json:"token"`
	LocationID  string         `json:"location_id,omitempty"`
	EVSEUID     string         `json:"evse_uid,omitempty"`
	ConnectorID string         `json:"connector_id,omitempty"`
	CreatedAt   string         `json:"created_at"`
}

func (c *Controller) apiSaveFavorite(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("token record id is required"))
		return
	}
	var input struct {
		Label       string         `json:"label"`
		Token       map[string]any `json:"token"`
		LocationID  string         `json:"location_id"`
		EVSEUID     string         `json:"evse_uid"`
		ConnectorID string         `json:"connector_id"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c.mu.Lock()
	token := input.Token
	if len(token) == 0 {
		if record := c.state.Objects["tokens"][id]; record != nil {
			token = record.Body
		}
	}
	if len(token) == 0 {
		c.mu.Unlock()
		writeError(w, http.StatusNotFound, fmt.Errorf("token %q is not in the partner catalogue — sync tokens from Evolt first", id))
		return
	}
	favorite := &FavoriteToken{
		ID: id, Label: strings.TrimSpace(input.Label), Token: cdrTokenFromToken(token),
		LocationID: input.LocationID, EVSEUID: input.EVSEUID, ConnectorID: input.ConnectorID,
		CreatedAt: now(),
	}
	if existing := c.favorites[id]; existing != nil {
		favorite.CreatedAt = existing.CreatedAt
		if favorite.LocationID == "" {
			favorite.LocationID, favorite.EVSEUID, favorite.ConnectorID = existing.LocationID, existing.EVSEUID, existing.ConnectorID
		}
	}
	if favorite.Label == "" {
		favorite.Label = fieldText(favorite.Token, "contract_id")
	}
	c.favorites[id] = favorite
	c.mu.Unlock()
	if err := c.persistFavorite(r.Context(), favorite); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	c.changed()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "favorite": favorite})
}

func (c *Controller) apiDeleteFavorite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c.mu.Lock()
	delete(c.favorites, id)
	c.mu.Unlock()
	if c.repository != nil {
		if err := c.repository.DeleteFavoriteToken(r.Context(), c.favoriteStorageID(id)); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	c.changed()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// rememberFavoriteTarget records where a favourite driver was last plugged in, so picking the same
// driver again prefills the same Location/EVSE/Connector. Caller holds c.mu; the returned value is
// what has to be written to PostgreSQL once the lock is released.
func (c *Controller) rememberFavoriteTarget(id, locationID, evseUID, connectorID string) *FavoriteToken {
	favorite := c.favorites[id]
	if favorite == nil {
		return nil
	}
	favorite.LocationID, favorite.EVSEUID, favorite.ConnectorID = locationID, evseUID, connectorID
	return favorite
}

func (c *Controller) persistFavorite(ctx context.Context, favorite *FavoriteToken) error {
	if c.repository == nil || favorite == nil {
		return nil
	}
	stored := storeFavorite(favorite)
	stored.ID = c.favoriteStorageID(stored.ID)
	return c.repository.SaveFavoriteToken(ctx, stored)
}

func (c *Controller) favoriteStorageID(id string) string {
	if c.profile == "dual" {
		return "dual:" + id
	}
	return id
}

func storeFavorite(favorite *FavoriteToken) store.FavoriteToken {
	return store.FavoriteToken{
		ID: favorite.ID, Label: favorite.Label, Token: favorite.Token,
		LocationID: favorite.LocationID, EVSEUID: favorite.EVSEUID, ConnectorID: favorite.ConnectorID,
		CreatedAt: favorite.CreatedAt,
	}
}

func favoriteList(favorites map[string]*FavoriteToken) []*FavoriteToken {
	result := make([]*FavoriteToken, 0, len(favorites))
	for _, favorite := range favorites {
		result = append(result, favorite)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
