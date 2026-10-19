package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// Tariff restrictions use the Location time zone (OCPI 2.2.1 §11.4.6), so the zone database ships in the binary.
	_ "time/tzdata"

	_ "github.com/jackc/pgx/v5/stdlib"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/config"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/emsp"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/server"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

// maxOpenConns stays small: one instance, a handful of testers.
const maxOpenConns = 8

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(logger)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}
	envIDs := make([]string, 0, len(cfg.Envs))
	for _, env := range cfg.Envs {
		envIDs = append(envIDs, env.ID)
	}
	logger.Info("starting partner console", "port", cfg.Port, "database", cfg.RedactedDatabaseURL(), "public_base_url", cfg.PublicBaseURL, "envs", envIDs)

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 60*time.Second)
	err = waitForDB(waitCtx, db)
	waitCancel()
	if err != nil {
		logger.Error("database unavailable", "error", err, "database", cfg.RedactedDatabaseURL())
		os.Exit(1)
	}

	repository := store.New(db, cfg.EncryptionKey)
	if repository.WeakEncryptionKey() {
		logger.Warn("ENCRYPTION_KEY is still a placeholder, falling back to the built-in development key")
	}
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 60*time.Second)
	applied, err := repository.Migrate(migrateCtx)
	migrateCancel()
	for _, script := range applied {
		logger.Info("applied schema script", "script", script)
	}
	if err != nil {
		logger.Error("migrate database", "error", err)
		os.Exit(1)
	}
	// Every env gets its own console over the same database, scoped to its own connections.
	var consoles consoleGroup
	envs := make([]server.Env, 0, len(cfg.Envs))
	for _, env := range cfg.Envs {
		console, err := emsp.New(repository.ForEnv(env.ID, env.EvoltVersionsURL), logger, emsp.Options{DefaultPublicURL: env.PublicBaseURL, MountPath: env.Prefix})
		if err != nil {
			logger.Error("initialize console", "env", env.ID, "error", err)
			os.Exit(1)
		}
		consoles = append(consoles, console)
		envs = append(envs, server.Env{ID: env.ID, Prefix: env.Prefix, Handler: console.Handler()})
	}

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           server.New(logger, repository, envs),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		logger.Info("partner console listening", "address", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdown(httpServer, consoles, logger)
}

type httpShutdown interface {
	Shutdown(context.Context) error
	Close() error
}

type consoleFlusher interface {
	Close(context.Context) error
}

// consoleGroup flushes the console of every env and reports every failure.
type consoleGroup []consoleFlusher

func (g consoleGroup) Close(ctx context.Context) error {
	var err error
	for _, console := range g {
		err = errors.Join(err, console.Close(ctx))
	}
	return err
}

func shutdown(httpServer httpShutdown, console consoleFlusher, logger *slog.Logger) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown", "error", err)
		if err := httpServer.Close(); err != nil {
			logger.Error("close http connections", "error", err)
		}
	}
	shutdownCancel()

	// SSE can exhaust the HTTP shutdown deadline; the final database save needs its own time budget.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer flushCancel()
	if err := console.Close(flushCtx); err != nil {
		logger.Error("flush partner state", "error", err)
	}
}

func waitForDB(ctx context.Context, db *sql.DB) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := db.PingContext(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
