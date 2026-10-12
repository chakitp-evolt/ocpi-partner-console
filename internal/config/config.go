// Package config reads the runtime settings from the environment and the Vault secret file.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/joho/godotenv"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/defaults"
)

// envFiles are loaded highest priority first; a key keeps the first value it gets and real env vars beat every file.
// Vault Agent renders into /vault/secrets/* or ./config/secret.env (= /app/config/secret.env, as in the other Evolt services).
var envFiles = []string{"/vault/secrets/secret.env", "/vault/secrets/config", "./config/secret.env", ".env"}

type Config struct {
	Port          string
	DatabaseURL   string
	EncryptionKey string
	PublicBaseURL string
}

// Load merges every env file that exists, then reads the settings from the process environment.
func Load(logger *slog.Logger) (Config, error) {
	paths := envFiles
	if custom := os.Getenv("SECRET_ENV_FILE"); custom != "" {
		paths = append([]string{custom}, paths...)
	}
	for _, path := range paths {
		loadEnvFile(logger, path)
	}

	databaseURL, err := databaseURL()
	if err != nil {
		return Config{}, err
	}
	key := os.Getenv("ENCRYPTION_KEY")
	if key == "" {
		return Config{}, errors.New("ENCRYPTION_KEY is required")
	}
	return Config{
		Port:          env("PORT", "8080"),
		DatabaseURL:   databaseURL,
		EncryptionKey: key,
		PublicBaseURL: defaults.PartnerPublicURL(),
	}, nil
}

// RedactedDatabaseURL keeps host and database name for logs and drops the password.
func (c Config) RedactedDatabaseURL() string {
	parsed, err := url.Parse(c.DatabaseURL)
	if err != nil {
		return "unparseable"
	}
	return parsed.Redacted()
}

// databaseURL takes DATABASE_URL as is, otherwise builds it from the DATABASE_* parts.
func databaseURL() (string, error) {
	if value := os.Getenv("DATABASE_URL"); value != "" {
		return value, nil
	}
	host, user, name := os.Getenv("DATABASE_HOST"), os.Getenv("DATABASE_USER"), os.Getenv("DATABASE_NAME")
	if host == "" || user == "" || name == "" {
		return "", errors.New("set DATABASE_URL or DATABASE_HOST, DATABASE_USER and DATABASE_NAME")
	}
	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, os.Getenv("DATABASE_PASSWORD")),
		Host:     fmt.Sprintf("%s:%s", host, env("DATABASE_PORT", "5432")),
		Path:     "/" + name,
		RawQuery: url.Values{"sslmode": {env("DATABASE_SSLMODE", "disable")}}.Encode(),
	}
	return dsn.String(), nil
}

// loadEnvFile never overrides a key that is already set.
func loadEnvFile(logger *slog.Logger, path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := godotenv.Load(path); err != nil {
		logger.Error("load env file", "path", path, "error", err)
		return
	}
	logger.Info("loaded env file", "path", path)
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
