//go:build integration

package emsp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

func TestCommandConnectionIsolation(t *testing.T) {
	db := integrationDatabase(t)
	ctx := context.Background()
	repository := store.New(db, "integration-test-encryption-key")
	application, err := New(repository, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{DefaultPublicURL: "https://partner.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := application.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	if err := application.Close(ctx); err != nil {
		t.Fatal(err)
	}

	connections := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT leg || '/' || profile, id FROM partner_console_connections`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key, id string
		if err := rows.Scan(&key, &id); err != nil {
			t.Fatal(err)
		}
		connections[key] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if len(connections) != 4 {
		t.Fatalf("connections = %d, want 4", len(connections))
	}
	for _, profile := range []string{"emsp", "dual"} {
		if err := repository.PutSecret(ctx, connections["emsp/"+profile], "inbound_token_b", "callback-test-"+profile); err != nil {
			t.Fatal(err)
		}
	}

	const commandID = "shared-command-id"
	seed := func(t *testing.T) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `DELETE FROM partner_console_commands`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO partner_console_commands(connection_id,leg,id,type,status,request,response_url,result)
			SELECT id,leg,$1,'START_SESSION','SENT','{}'::jsonb,'https://partner.example/callback',
			CASE WHEN leg='cpo' THEN '{"result":"ACCEPTED","session_id":"session-preserved"}'::jsonb END
			FROM partner_console_connections`, commandID); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(t *testing.T) map[string]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT connection_id, to_jsonb(c)::text FROM partner_console_commands c`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		result := map[string]string{}
		for rows.Next() {
			var id, body string
			if err := rows.Scan(&id, &body); err != nil {
				t.Fatal(err)
			}
			result[id] = body
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertChanges := func(t *testing.T, before map[string]string, targetID, field string) {
		t.Helper()
		after := snapshot(t)
		if len(after) != len(before) {
			t.Fatal("command row count changed")
		}
		for id, body := range before {
			if id != targetID {
				if after[id] != body {
					t.Fatal("another connection's command changed")
				}
				continue
			}
			var command struct {
				Status         string          `json:"status"`
				DirectResponse json.RawMessage `json:"direct_response"`
				AsyncResult    json.RawMessage `json:"async_result"`
				TimeoutSeconds int             `json:"timeout_seconds"`
			}
			if err := json.Unmarshal([]byte(after[id]), &command); err != nil {
				t.Fatal(err)
			}
			response := command.AsyncResult
			if field == "direct_response" {
				response = command.DirectResponse
				if command.TimeoutSeconds != 30 {
					t.Fatalf("timeout = %d, want 30", command.TimeoutSeconds)
				}
			}
			var result map[string]any
			if err := json.Unmarshal(response, &result); err != nil {
				t.Fatal(err)
			}
			if command.Status != "ACCEPTED" || result["result"] != "ACCEPTED" {
				t.Fatal("the owning connection's command was not updated")
			}
		}
	}
	handler := application.Handler()
	callback := func(t *testing.T, profile, tokenProfile, id string, wantHTTP, wantOCPI int) {
		t.Helper()
		path := "/ocpi/emsp/2.2.1/commands/callback/" + id
		if profile == "dual" {
			path = "/dual" + path
		}
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"result":"ACCEPTED"}`))
		r.Header.Set("Authorization", ocpi.EncodeCredential("callback-test-"+tokenProfile))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Request-ID", "callback-request")
		r.Header.Set("X-Correlation-ID", "callback-correlation")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var response ocpi.Envelope
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if w.Code != wantHTTP || response.StatusCode != wantOCPI {
			t.Fatalf("callback HTTP %d OCPI %d, want HTTP %d OCPI %d", w.Code, response.StatusCode, wantHTTP, wantOCPI)
		}
	}
	for _, profile := range []string{"emsp", "dual"} {
		t.Run(profile+" direct response", func(t *testing.T) {
			seed(t)
			before := snapshot(t)
			id := connections["emsp/"+profile]
			if err := repository.UpdateCommandDirect(ctx, id, commandID, "ACCEPTED", 30, map[string]any{"result": "ACCEPTED", "timeout": 30}); err != nil {
				t.Fatal(err)
			}
			assertChanges(t, before, id, "direct_response")
		})
		t.Run(profile+" authenticated callback", func(t *testing.T) {
			seed(t)
			before := snapshot(t)
			callback(t, profile, profile, commandID, http.StatusOK, 1000)
			assertChanges(t, before, connections["emsp/"+profile], "async_result")
		})
		t.Run(profile+" callback for another connection", func(t *testing.T) {
			seed(t)
			if _, err := db.ExecContext(ctx, `DELETE FROM partner_console_commands WHERE connection_id=$1`, connections["emsp/"+profile]); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t)
			callback(t, profile, profile, commandID, http.StatusNotFound, 2000)
			assertChanges(t, before, "", "")
		})
		t.Run(profile+" callback with another connection's token", func(t *testing.T) {
			seed(t)
			before := snapshot(t)
			otherProfile := "dual"
			if profile == "dual" {
				otherProfile = "emsp"
			}
			callback(t, profile, otherProfile, commandID, http.StatusUnauthorized, 2000)
			assertChanges(t, before, "", "")
		})
	}
	for _, target := range []string{"cpo/cpo", "cpo/dual", "missing-connection"} {
		t.Run(target+" rejected by eMSP updates", func(t *testing.T) {
			seed(t)
			before := snapshot(t)
			id := connections[target]
			if id == "" {
				id = target
			}
			if err := repository.UpdateCommandDirect(ctx, id, commandID, "ACCEPTED", 30, map[string]any{"result": "ACCEPTED"}); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("direct update error = %v, want sql.ErrNoRows", err)
			}
			if err := repository.UpdateCommandAsync(ctx, id, commandID, "ACCEPTED", map[string]any{"result": "ACCEPTED"}); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("async update error = %v, want sql.ErrNoRows", err)
			}
			assertChanges(t, before, "", "")
		})
	}
}
