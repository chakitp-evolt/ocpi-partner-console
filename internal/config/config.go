// Package config reads the runtime settings from the environment and the Vault secret file.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/joho/godotenv"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/defaults"
)

// envFiles are loaded highest priority first; a key keeps the first value it gets and real env vars beat every file.
// Vault Agent renders into /vault/secrets/* or ./config/secret.env (= /app/config/secret.env, as in the other Evolt services).
var envFiles = []string{"/vault/secrets/secret.env", "/vault/secrets/config", "./config/secret.env", ".env"}

type Config struct {
	Port            string
	DatabaseURL     string
	EncryptionKey   string
	PublicBaseURL   string
	ConsoleUsername string
	ConsolePassword string
	// Envs starts with dev at the root URL, then every CONSOLE_ENVS entry under /<id>.
	Envs []Env
}

// Env is one Evolt environment the console holds its own connections for.
type Env struct {
	ID               string
	Prefix           string
	EvoltVersionsURL string
	PublicBaseURL    string
}

var envID = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// reservedEnvIDs are first path segments the console already serves.
var reservedEnvIDs = map[string]bool{"dev": true, "emsp": true, "cpo": true, "dual": true, "api": true, "ocpi": true, "health": true, "readiness": true}

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
	username, password := os.Getenv("CONSOLE_USERNAME"), os.Getenv("CONSOLE_PASSWORD")
	if (username == "") != (password == "") {
		return Config{}, errors.New("CONSOLE_USERNAME and CONSOLE_PASSWORD must be set together")
	}
	if os.Getenv("RENDER") == "true" {
		if username == "" {
			return Config{}, errors.New("CONSOLE_USERNAME and CONSOLE_PASSWORD are required on Render")
		}
		if len(key) < 32 || strings.Contains(key, "change-me") || strings.Contains(key, "replace-with") {
			return Config{}, errors.New("ENCRYPTION_KEY must be a random secret of at least 32 characters on Render")
		}
	}
	publicBaseURL := strings.TrimRight(defaults.PartnerPublicURL(), "/")
	envs, err := parseEnvs(publicBaseURL)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Port:            env("PORT", "8080"),
		DatabaseURL:     databaseURL,
		EncryptionKey:   key,
		PublicBaseURL:   publicBaseURL,
		ConsoleUsername: username,
		ConsolePassword: password,
		Envs:            envs,
	}, nil
}

// parseEnvs reads CONSOLE_ENVS; each entry needs its own EVOLT_VERSIONS_URL_<ID> so it never falls back to dev.
func parseEnvs(publicBaseURL string) ([]Env, error) {
	envs := []Env{{ID: "dev", EvoltVersionsURL: defaults.EvoltVersionsURL(), PublicBaseURL: publicBaseURL}}
	seen := map[string]bool{}
	for _, raw := range strings.Split(os.Getenv("CONSOLE_ENVS"), ",") {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if !envID.MatchString(id) {
			return nil, fmt.Errorf("CONSOLE_ENVS: %q must be lowercase letters, digits or '-' and start with a letter", id)
		}
		if reservedEnvIDs[id] || seen[id] {
			return nil, fmt.Errorf("CONSOLE_ENVS: %q is reserved or listed twice", id)
		}
		seen[id] = true
		key := "EVOLT_VERSIONS_URL_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
		versionsURL := strings.TrimSpace(os.Getenv(key))
		if versionsURL == "" {
			return nil, fmt.Errorf("%s is required for CONSOLE_ENVS entry %q", key, id)
		}
		envs = append(envs, Env{ID: id, Prefix: "/" + id, EvoltVersionsURL: versionsURL, PublicBaseURL: publicBaseURL + "/" + id})
	}
	return envs, nil
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
