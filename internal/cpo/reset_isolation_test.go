package cpo

import (
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
	"context"
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"
)

type isolatedResetStore struct {
	*store.Store
	deletedID string
	saved     bool
	deleteErr error
}

func (s *isolatedResetStore) DeleteCPO(_ context.Context, id string) error {
	s.deletedID = id
	return s.deleteErr
}
func (s *isolatedResetStore) SaveCPO(_ context.Context, _ store.CPOSnapshot) error {
	s.saved = true
	return nil
}
func TestQAResetOnlyDeletesPartnerRun(t *testing.T) {
	c := New("http://mock.example")
	repository := &isolatedResetStore{}
	c.repository = repository
	old := c.state.RunID
	rec := httptest.NewRecorder()
	c.apiReset(rec, httptest.NewRequest("POST", "/api/cpo/reset", strings.NewReader(`{"confirmation":"RESET"}`)))
	if rec.Code != 200 || repository.deletedID != old || !repository.saved {
		t.Fatal("QA reset did not replace partner run", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"evolt_console_credentials_cleared":false`) {
		t.Fatal("reset claims to clear API workspace")
	}
}

func TestResetBeforeFirstSaveSucceeds(t *testing.T) {
	c := New("http://mock.example")
	c.repository = &isolatedResetStore{deleteErr: sql.ErrNoRows}
	rec := httptest.NewRecorder()
	c.apiReset(rec, httptest.NewRequest("POST", "/api/cpo/reset", strings.NewReader(`{"confirmation":"RESET"}`)))
	if rec.Code != 200 {
		t.Fatalf("reset of an unsaved run answered HTTP %d: %s", rec.Code, rec.Body)
	}
}
