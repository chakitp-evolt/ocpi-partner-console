package store

import (
	"context"
	"fmt"

	dbscripts "bitbucket.org/evoltth/evolt-ocpi-partner-console/scripts/db"
)

// migrateLock keeps two instances booting together from applying the same script twice.
const migrateLock = 7303580211

// migrations run in order; applied answers whether the schema already has what the script adds,
// so a database migrated by hand with psql is recognised without any bookkeeping table.
var migrations = []struct {
	file    string
	applied string
}{
	{"001_init.sql", `SELECT to_regclass('partner_console_connections') IS NOT NULL`},
	{"002_connection_env.sql", `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'partner_console_connections' AND column_name = 'env')`},
}

// Migrate applies every script in scripts/db the database does not have yet and returns their names.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrateLock); err != nil {
		return nil, fmt.Errorf("lock schema migration: %w", err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLock)

	var ran []string
	for _, migration := range migrations {
		var applied bool
		if err := conn.QueryRowContext(ctx, migration.applied).Scan(&applied); err != nil {
			return ran, fmt.Errorf("check %s: %w", migration.file, err)
		}
		if applied {
			continue
		}
		script, err := dbscripts.FS.ReadFile(migration.file)
		if err != nil {
			return ran, err
		}
		if _, err := conn.ExecContext(ctx, string(script)); err != nil {
			return ran, fmt.Errorf("apply %s: %w", migration.file, err)
		}
		ran = append(ran, migration.file)
	}
	return ran, nil
}
