package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func writeEnvFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func useEnvFiles(t *testing.T, paths ...string) {
	t.Helper()
	previous := envFiles
	envFiles = paths
	t.Cleanup(func() { envFiles = previous })
}

func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var configKeys = []string{"PORT", "DATABASE_URL", "DATABASE_HOST", "DATABASE_PORT", "DATABASE_USER", "DATABASE_PASSWORD", "DATABASE_NAME", "DATABASE_SSLMODE", "ENCRYPTION_KEY", "PUBLIC_BASE_URL", "SECRET_ENV_FILE"}

func TestLoadReadsVaultFileNextToConfigSecretFile(t *testing.T) {
	clearEnv(t, configKeys...)
	dir := t.TempDir()
	vault := writeEnvFile(t, dir, "vault.env", "ENCRYPTION_KEY=from-vault\nDATABASE_PASSWORD=vault-password\n")
	secret := writeEnvFile(t, dir, "secret.env", "ENCRYPTION_KEY=from-config\nDATABASE_HOST=db.internal\nDATABASE_USER=console\nDATABASE_NAME=partner_console\n")
	useEnvFiles(t, vault, secret)

	cfg, err := Load(quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EncryptionKey != "from-vault" {
		t.Fatalf("ENCRYPTION_KEY = %q, want the Vault value", cfg.EncryptionKey)
	}
	want := "postgres://console:vault-password@db.internal:5432/partner_console?sslmode=disable"
	if cfg.DatabaseURL != want {
		t.Fatalf("DatabaseURL = %q, want %q", cfg.DatabaseURL, want)
	}
}

func TestLoadKeepsRealEnvironmentOverFiles(t *testing.T) {
	clearEnv(t, configKeys...)
	t.Setenv("ENCRYPTION_KEY", "from-env")
	t.Setenv("DATABASE_URL", "postgres://env@host/db")
	vault := writeEnvFile(t, t.TempDir(), "vault.env", "ENCRYPTION_KEY=from-vault\nDATABASE_URL=postgres://vault@host/db\n")
	useEnvFiles(t, vault)

	cfg, err := Load(quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EncryptionKey != "from-env" || cfg.DatabaseURL != "postgres://env@host/db" {
		t.Fatalf("env vars lost to the file: %+v", cfg)
	}
}

func TestLoadFailsWithoutRequiredSettings(t *testing.T) {
	clearEnv(t, configKeys...)
	useEnvFiles(t)
	if _, err := Load(quietLogger()); err == nil {
		t.Fatal("Load succeeded without a database or encryption key")
	}
}
