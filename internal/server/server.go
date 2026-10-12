// Package server wraps the console with the probes and the HTTP middleware every route shares.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Pinger is the database check behind the readiness probe.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Server struct {
	logger *slog.Logger
	db     Pinger
}

// New mounts the probes next to the console and wraps both in the shared middleware.
func New(logger *slog.Logger, db Pinger, console http.Handler) http.Handler {
	s := &Server{logger: logger, db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /readiness", s.readiness)
	mux.Handle("/", console)
	return s.recoverer(s.accessLog(compressDashboard(mux)))
}

// health is the liveness probe and never touches the database.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}

// readiness answers 503 while PostgreSQL is unreachable, since every page and OCPI call needs it.
func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "database": err.Error(), "time": time.Now().UTC()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "database": "ok", "time": time.Now().UTC()})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("panic", "value", recovered, "path", r.URL.Path)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "internal error", "detail": fmt.Sprint(recovered)})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		if !isProbe(r.URL.Path) && !strings.HasSuffix(r.URL.Path, "/events") {
			s.logger.Info("http request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds())
		}
	})
}

func isProbe(path string) bool {
	return path == "/health" || path == "/readiness"
}

// isOCPIPath covers the protocol surface Evolt calls, which must reach it byte-for-byte.
func isOCPIPath(path string) bool {
	return strings.HasPrefix(path, "/ocpi/") || strings.HasPrefix(path, "/dual/ocpi/")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
