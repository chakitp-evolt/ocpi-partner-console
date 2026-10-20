// Package dbschema initializes the console database before serving traffic.
package dbschema

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed 001_init.sql
var initialSchema string

//go:embed 002_connection_env.sql
var environmentSchema string

// Ensure also accepts databases initialized with the standalone SQL scripts.
// Both steps share one transaction and a transaction-scoped lock, including behind a pooler.
func Ensure(ctx context.Context, db *sql.DB) ([]string, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(221, 20261020)`); err != nil {
		return nil, fmt.Errorf("lock console schema: %w", err)
	}

	var tables int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND table_name IN (
		'partner_console_connections', 'partner_console_objects', 'partner_console_commands',
		'partner_console_saved_requests', 'partner_console_exchanges')`).Scan(&tables); err != nil {
		return nil, err
	}
	applied := []string{}
	switch tables {
	case 0:
		if err := execute(ctx, tx, initialSchema); err != nil {
			return nil, fmt.Errorf("apply 001_init.sql: %w", err)
		}
		applied = append(applied, "001_init.sql")
	case 5:
	default:
		return nil, errors.New("console schema is incomplete; refusing to change existing tables")
	}

	var columns int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND column_name = 'env'
		AND table_name IN ('partner_console_connections', 'partner_console_exchanges')`).Scan(&columns); err != nil {
		return nil, err
	}
	switch columns {
	case 0:
		if err := execute(ctx, tx, environmentSchema); err != nil {
			return nil, fmt.Errorf("apply 002_connection_env.sql: %w", err)
		}
		applied = append(applied, "002_connection_env.sql")
	case 2:
	default:
		return nil, errors.New("console environment schema is incomplete; refusing to change existing columns")
	}

	var constraints int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint
		WHERE (conrelid = 'partner_console_connections'::regclass AND conname = 'partner_console_connections_env_leg_profile_key')
		OR (conrelid = 'partner_console_exchanges'::regclass AND conname = 'partner_console_exchanges_env_check')`).Scan(&constraints); err != nil {
		return nil, err
	}
	if constraints != 2 {
		return nil, errors.New("console environment constraints are incomplete")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return applied, nil
}

func execute(ctx context.Context, tx *sql.Tx, script string) error {
	// Strip only the standalone wrapper; Ensure owns the transaction and its lock.
	_, body, begin := strings.Cut(script, "\nBEGIN;")
	body, commit := strings.CutSuffix(strings.TrimSpace(body), "COMMIT;")
	if !begin || !commit {
		return errors.New("schema script must have a BEGIN/COMMIT wrapper")
	}
	_, err := tx.ExecContext(ctx, body)
	return err
}
