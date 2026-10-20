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
	logger      *slog.Logger
	db          Pinger
	consoleAuth func(http.Handler) http.Handler
}

type Option func(*Server)

// Env is the console of one Evolt environment; dev has an empty Prefix and is served at the root.
type Env struct {
	ID      string
	Prefix  string
	Handler http.Handler
}

// New mounts the probes next to the console of every env and wraps them in the shared middleware.
func New(logger *slog.Logger, db Pinger, envs []Env, options ...Option) http.Handler {
	s := &Server{logger: logger, db: db, consoleAuth: func(h http.Handler) http.Handler { return h }}
	for _, option := range options {
		option(s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /readiness", s.readiness)
	mux.Handle("GET /console-envs.js", s.consoleAuth(envsScript(envs)))
	for _, env := range envs {
		handler := s.consoleAuth(env.Handler)
		if env.Prefix == "" {
			mux.Handle("/", handler)
			continue
		}
		// The console redirects to absolute paths, so the env roots are redirected here before the prefix is stripped.
		mux.Handle("GET "+env.Prefix, redirect(env.Prefix+"/emsp"))
		mux.Handle("GET "+env.Prefix+"/{$}", redirect(env.Prefix+"/emsp"))
		mux.Handle("GET "+env.Prefix+"/dual", redirect(env.Prefix+"/dual/"))
		mux.Handle(env.Prefix+"/", http.StripPrefix(env.Prefix, handler))
	}
	return s.recoverer(s.accessLog(compressDashboard(mux)))
}

func redirect(target string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	})
}

// envsScript tells the pages which env prefixes exist, so links and API calls stay inside the env they opened in.
func envsScript(envs []Env) http.HandlerFunc {
	type entry struct {
		ID     string `json:"id"`
		Prefix string `json:"prefix"`
	}
	list := make([]entry, 0, len(envs))
	for _, env := range envs {
		list = append(list, entry{ID: env.ID, Prefix: env.Prefix})
	}
	body, _ := json.Marshal(list)
	script := []byte("window.CONSOLE_ENVS=" + string(body) + ";\n")
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(script)
	}
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

// isOCPIPath covers the protocol surface Evolt calls, which must reach it byte-for-byte:
// /ocpi/, /dual/ocpi/ and the same under every env prefix.
func isOCPIPath(path string) bool {
	return strings.Contains(path, "/ocpi/")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
