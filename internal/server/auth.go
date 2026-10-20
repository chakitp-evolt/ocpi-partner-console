package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
)

// WithConsoleAuth protects the dashboard after the environment prefix is stripped.
// OCPI handlers keep their own token authentication for partner callbacks.
func WithConsoleAuth(username, password string) Option {
	return func(s *Server) {
		if username == "" && password == "" {
			return
		}
		wantUser, wantPassword := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(password))
		s.consoleAuth = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/ocpi/") || strings.HasPrefix(r.URL.Path, "/dual/ocpi/") {
					next.ServeHTTP(w, r)
					return
				}
				user, pass, ok := r.BasicAuth()
				gotUser, gotPassword := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
				userMatch := subtle.ConstantTimeCompare(gotUser[:], wantUser[:])
				passwordMatch := subtle.ConstantTimeCompare(gotPassword[:], wantPassword[:])
				w.Header().Set("Cache-Control", "no-store")
				if !ok || userMatch&passwordMatch != 1 {
					w.Header().Set("WWW-Authenticate", `Basic realm="Partner Console", charset="UTF-8"`)
					http.Error(w, "authentication required", http.StatusUnauthorized)
					return
				}
				if origin := r.Header.Get("Origin"); origin != "" && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
					parsed, err := url.Parse(origin)
					if err != nil || parsed.Host != r.Host || (parsed.Scheme != "https" && parsed.Scheme != "http") {
						http.Error(w, "cross-origin request denied", http.StatusForbidden)
						return
					}
				}
				next.ServeHTTP(w, r)
			})
		}
	}
}
