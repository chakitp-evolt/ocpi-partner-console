package server

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

// compressDashboard gzips dashboard pages and /api/* responses to keep hosted egress down.
// The OCPI surface and probes stay byte-for-byte as before, and SSE and range requests pass through.
func compressDashboard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOCPIPath(r.URL.Path) || isProbe(r.URL.Path) || r.Header.Get("Range") != "" ||
			strings.Contains(r.Header.Get("Accept"), "text/event-stream") ||
			!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// gzipResponseWriter decides on compression at the first body write, once headers are final.
type gzipResponseWriter struct {
	http.ResponseWriter
	status  int
	started bool
	gz      *gzip.Writer
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if !w.started && w.status == 0 {
		w.status = status
	}
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.started {
		w.start(p)
	}
	if w.gz != nil {
		return w.gz.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *gzipResponseWriter) start(first []byte) {
	w.started = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	header := w.Header()
	// Sniff on the plain bytes, otherwise net/http would sniff the gzip stream.
	if header.Get("Content-Type") == "" && len(first) > 0 {
		header.Set("Content-Type", http.DetectContentType(first))
	}
	if len(first) > 0 && header.Get("Content-Encoding") == "" &&
		!strings.HasPrefix(header.Get("Content-Type"), "text/event-stream") &&
		w.status != http.StatusNoContent && w.status != http.StatusNotModified {
		header.Set("Content-Encoding", "gzip")
		header.Add("Vary", "Accept-Encoding")
		header.Del("Content-Length")
		w.gz = gzipWriters.Get().(*gzip.Writer)
		w.gz.Reset(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(w.status)
}

func (w *gzipResponseWriter) Flush() {
	if !w.started {
		w.start(nil)
	}
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *gzipResponseWriter) close() {
	if !w.started {
		if w.status == 0 {
			return
		}
		w.start(nil)
	}
	if w.gz != nil {
		_ = w.gz.Close()
		gzipWriters.Put(w.gz)
		w.gz = nil
	}
}
