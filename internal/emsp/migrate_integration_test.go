//go:build integration

package emsp

import (
	"context"
	"os"
	"reflect"
	"testing"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

func applyByHand(t *testing.T, ctx context.Context, exec func(context.Context, string, ...any) error, files ...string) {
	t.Helper()
	for _, file := range files {
		script, err := os.ReadFile("../../scripts/db/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if err := exec(ctx, string(script)); err != nil {
			t.Fatalf("apply %s by hand: %v", file, err)
		}
	}
}

func TestMigrateBuildsAnEmptyDatabaseOnce(t *testing.T) {
	ctx := context.Background()
	db := emptyIntegrationDatabase(t)
	repository := store.New(db, "integration-test-encryption-key")

	ran, err := repository.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"001_init.sql", "002_connection_env.sql"}; !reflect.DeepEqual(ran, want) {
		t.Fatalf("first boot ran %v, want %v", ran, want)
	}
	ran, err = repository.Migrate(ctx)
	if err != nil || len(ran) != 0 {
		t.Fatalf("second boot ran %v, %v; want nothing", ran, err)
	}
}

func TestMigrateUpgradesADatabaseBuiltByHand(t *testing.T) {
	ctx := context.Background()
	db := emptyIntegrationDatabase(t)
	exec := func(ctx context.Context, query string, args ...any) error {
		_, err := db.ExecContext(ctx, query, args...)
		return err
	}
	applyByHand(t, ctx, exec, "001_init.sql")
	if err := exec(ctx, `INSERT INTO partner_console_connections(id,leg,profile,name) VALUES('dev-emsp','emsp','emsp','kept'),('dev-dual','emsp','dual','')`); err != nil {
		t.Fatal(err)
	}

	ran, err := store.New(db, "integration-test-encryption-key").Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"002_connection_env.sql"}; !reflect.DeepEqual(ran, want) {
		t.Fatalf("upgrade ran %v, want %v", ran, want)
	}
	var devRows, otherRows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE env='dev'), count(*) FILTER (WHERE env<>'dev') FROM partner_console_connections`).Scan(&devRows, &otherRows); err != nil {
		t.Fatal(err)
	}
	if devRows != 2 || otherRows != 0 {
		t.Fatalf("existing connections: %d dev, %d other; want both kept as dev", devRows, otherRows)
	}
	run, err := store.New(db, "integration-test-encryption-key").CurrentRunProfile(ctx, "emsp")
	if err != nil || run.ID != "dev-emsp" || run.Name != "kept" {
		t.Fatalf("dev eMSP connection after upgrade = %+v, %v", run, err)
	}
}

func TestMigrateAcceptsADatabaseMigratedByHand(t *testing.T) {
	ctx := context.Background()
	db := emptyIntegrationDatabase(t)
	exec := func(ctx context.Context, query string, args ...any) error {
		_, err := db.ExecContext(ctx, query, args...)
		return err
	}
	applyByHand(t, ctx, exec, "001_init.sql", "002_connection_env.sql")

	ran, err := store.New(db, "integration-test-encryption-key").Migrate(ctx)
	if err != nil || len(ran) != 0 {
		t.Fatalf("boot on a database migrated by hand ran %v, %v; want nothing", ran, err)
	}
}
