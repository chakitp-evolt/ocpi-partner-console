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
	logger.Info("starting partner console", "port", cfg.Port, "database", cfg.RedactedDatabaseURL(), "public_base_url", cfg.PublicBaseURL)

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

	// The schema is applied by hand from scripts/db; boot fails here if it has not been.
	repository := store.New(db, cfg.EncryptionKey)
	if repository.WeakEncryptionKey() {
		logger.Warn("ENCRYPTION_KEY is still a placeholder, falling back to the built-in development key")
	}
	console, err := emsp.New(repository, logger, emsp.Options{DefaultPublicURL: cfg.PublicBaseURL})
	if err != nil {
		logger.Error("initialize console", "error", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           server.New(logger, repository, console.Handler()),
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

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown", "error", err)
	}
	if err := console.Close(shutdownCtx); err != nil {
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
