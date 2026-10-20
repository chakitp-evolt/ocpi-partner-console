//go:build integration

package dbschema

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func testDatabase(t *testing.T) *sql.DB {
	t.Helper()
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || u.Path != "/partner_console_test" {
		t.Fatal("TEST_DATABASE_URL must name the isolated partner_console_test database")
	}
	admin, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("console_migration_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	return db
}

func execSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureFreshDatabaseAndRestart(t *testing.T) {
	db := testDatabase(t)
	applied, err := Ensure(context.Background(), db)
	if err != nil || len(applied) != 2 {
		t.Fatalf("fresh database: applied %v, error %v", applied, err)
	}
	execSQL(t, db, `INSERT INTO partner_console_connections (id, leg, profile, name) VALUES ('kept', 'emsp', 'emsp', 'saved connection')`)
	applied, err = Ensure(context.Background(), db)
	if err != nil || len(applied) != 0 {
		t.Fatalf("restart: applied %v, error %v", applied, err)
	}
	var name, env string
	if err := db.QueryRow(`SELECT name, env FROM partner_console_connections WHERE id = 'kept'`).Scan(&name, &env); err != nil || name != "saved connection" || env != "dev" {
		t.Fatalf("existing row changed: name %q env %q error %v", name, env, err)
	}
}

func TestEnsureAdoptsStandaloneScripts(t *testing.T) {
	for _, upgraded := range []bool{false, true} {
		t.Run(fmt.Sprint("environment-schema=", upgraded), func(t *testing.T) {
			db := testDatabase(t)
			execSQL(t, db, initialSchema)
			execSQL(t, db, `INSERT INTO partner_console_connections (id, leg, profile) VALUES ('legacy', 'emsp', 'emsp')`)
			execSQL(t, db, `INSERT INTO partner_console_saved_requests (id, lane, module, name, content) VALUES ('request', 'COMMON', 'versions', 'saved request', '{}')`)
			execSQL(t, db, `INSERT INTO partner_console_exchanges (saved_request_id, method, url, result) VALUES ('request', 'GET', 'https://example.com', 'PASS')`)
			if upgraded {
				execSQL(t, db, environmentSchema)
			}
			applied, err := Ensure(context.Background(), db)
			want := 1
			if upgraded {
				want = 0
			}
			if err != nil || len(applied) != want {
				t.Fatalf("applied %v, error %v", applied, err)
			}
			var env string
			if err := db.QueryRow(`SELECT env FROM partner_console_connections WHERE id = 'legacy'`).Scan(&env); err != nil || env != "dev" {
				t.Fatalf("legacy connection: env %q error %v", env, err)
			}
			if err := db.QueryRow(`SELECT env FROM partner_console_exchanges WHERE saved_request_id = 'request'`).Scan(&env); err != nil || env != "dev" {
				t.Fatalf("saved history: env %q error %v", env, err)
			}
		})
	}
}

func TestEnsureConcurrentStartup(t *testing.T) {
	db := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := Ensure(ctx, db); err != nil {
				t.Errorf("concurrent startup: %v", err)
			}
		})
	}
	wg.Wait()
}

func TestEnsureRejectsPartialSchema(t *testing.T) {
	db := testDatabase(t)
	execSQL(t, db, `CREATE TABLE partner_console_connections (id text)`)
	if _, err := Ensure(context.Background(), db); err == nil {
		t.Fatal("partial schema was accepted")
	}
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("partial schema was changed: tables %d error %v", tables, err)
	}
}

func TestEnsureRollsBackBothStepsOnFailure(t *testing.T) {
	db := testDatabase(t)
	// This constraint name collides only when the second script runs.
	execSQL(t, db, `CREATE TABLE unrelated (id text); CREATE UNIQUE INDEX partner_console_connections_env_leg_profile_key ON unrelated(id)`)
	if _, err := Ensure(context.Background(), db); err == nil {
		t.Fatal("expected the environment migration to fail")
	}
	var exists bool
	if err := db.QueryRow(`SELECT to_regclass('partner_console_connections') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("failed migration was not rolled back: table exists %v error %v", exists, err)
	}
	var unrelated bool
	if err := db.QueryRow(`SELECT to_regclass('unrelated') IS NOT NULL`).Scan(&unrelated); err != nil || !unrelated {
		t.Fatalf("unrelated table changed: exists %v error %v", unrelated, err)
	}
}
