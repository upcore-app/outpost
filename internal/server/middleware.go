package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/upcore-app/outpost/internal/apikey"
)

const bearerPrefix = "bearer "

// authenticated guards everything but /healthz. The presented key is never
// logged, not even masked: a near-miss in a log is a working key for whoever
// reads the log.
func (s *server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !apikey.Equal(presentedKey(r), s.apiKey) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// presentedKey accepts both spellings upcore may use: a bearer token, and the
// X-API-Key header that survives proxies which strip Authorization.
func presentedKey(r *http.Request) string {
	// RFC 7235 makes the scheme case-insensitive.
	if h := strings.TrimSpace(r.Header.Get("Authorization")); len(h) >= len(bearerPrefix) &&
		strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return strings.TrimSpace(h[len(bearerPrefix):])
	}
	// Fall through rather than fail: a proxy that injects its own Authorization
	// header (Basic, Negotiate) must not make the X-API-Key spelling unusable.
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// recoverPanic keeps one malformed request from taking down a probe that other
// monitors depend on. It is the outermost layer so it also covers the logger.
func (s *server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic while serving", "method", r.Method, "path", r.URL.Path, "panic", rec)
				// The status may already be written; WriteHeader then logs a
				// superfluous-call warning and changes nothing, which is the
				// best available outcome for a half-sent response.
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.LogRequests {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Round(time.Millisecond).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

// statusRecorder remembers the status code for the access log. It starts at 200
// because a handler that only calls Write never calls WriteHeader.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.MaxBytesReader and http.ResponseController reach the real
// ResponseWriter through this wrapper. Without it an oversized body is still
// rejected, but the connection is never marked as such and the client sees a
// reset instead of the 400.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
