package server

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

// unreachableStore points at a port nothing listens on. sql.Open does not dial, so the failure
// surfaces on the first Ping — which is exactly what the probe is for.
func unreachableStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return store.New(db, "readiness-test-key")
}

// The probe exists to take an instance out of rotation when PostgreSQL is gone. Answering 200 there
// is worse than answering nothing: every page and every OCPI write needs the database, so the
// platform would keep sending traffic to an instance that can only fail request by request.
func TestReadinessReportsTheDatabaseIsUnreachable(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), db: unreachableStore(t)}

	recorder := httptest.NewRecorder()
	s.readiness(recorder, httptest.NewRequest(http.MethodGet, "/readiness", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness answered HTTP %d with a dead database, want 503: %s", recorder.Code, recorder.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable body: %v", err)
	}
	if body["status"] != "unavailable" {
		t.Fatalf("body = %#v", body)
	}
	if reason, _ := body["database"].(string); reason == "" {
		t.Fatal("the answer must say what went wrong, otherwise the probe is a dead end to debug")
	}
}

// health stays a liveness answer: it must not depend on the database, or a database outage would
// have the platform restart every instance instead of routing around them.
func TestHealthAnswersWithoutTouchingTheDatabase(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), db: unreachableStore(t)}

	recorder := httptest.NewRecorder()
	s.health(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("health answered HTTP %d, want 200: %s", recorder.Code, recorder.Body)
	}
}
