// Package server exposes the outpost's HTTP surface: a health probe, an
// identity endpoint and the batch check runner. upcore calls in; the outpost
// never calls out.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/upcore-app/outpost/internal/check"
	"github.com/upcore-app/outpost/internal/config"
	"github.com/upcore-app/outpost/internal/update"
)

const (
	// maxBodyBytes: a batch of 200 checks with headers is a few dozen KiB, so a
	// MiB is generous while still bounding what an unauthenticated caller can
	// make the process buffer.
	maxBodyBytes = 1 << 20

	// maxUpdateBodyBytes: the update request carries one release tag. A KiB is
	// already three orders of magnitude more than that.
	maxUpdateBodyBytes = 1 << 10

	// maxIDLength is what upcore needs for a monitor id in any encoding it may
	// pick later; longer means the caller is sending something else entirely.
	maxIDLength = 64

	// maxBatchDuration keeps a batch inside the server's WriteTimeout, so a
	// stuck probe ends as a down result rather than a dropped connection.
	maxBatchDuration = 140 * time.Second

	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	// WriteTimeout has to cover the slowest legal batch: checks may specify up
	// to 120 s, and they run concurrently, so the batch bound plus headroom.
	writeTimeout = 150 * time.Second
	idleTimeout  = 60 * time.Second
)

type server struct {
	cfg     config.Config
	apiKey  string
	version string
	log     *slog.Logger
	runner  *check.Runner
	updater *update.Manager

	// started keeps its monotonic reading, so uptime measures elapsed time and
	// not the effect of an NTP step; startedAt is the wall-clock instant, which
	// only makes sense to read once.
	started   time.Time
	startedAt string
}

// New wires the handlers and returns a server ready to listen. Lifecycle
// (listening, signals, draining) stays with the caller.
// The updater is passed in rather than built here: main constructs it before
// the server so its startup reconcile (see internal/update) is part of the boot
// log, and so a deployment that wires one up differently has somewhere to do it.
func New(cfg config.Config, apiKey, version string, updater *update.Manager, log *slog.Logger) *http.Server {
	now := time.Now()
	s := &server{
		cfg:       cfg,
		apiKey:    apiKey,
		version:   version,
		log:       log,
		runner:    check.NewRunner(cfg.MaxConcurrency),
		updater:   updater,
		started:   now,
		startedAt: now.UTC().Format(time.RFC3339),
	}

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	// Every route is registered twice: once for the method it supports, once
	// without a method to answer 405 in JSON. The catch-all "/" would otherwise
	// swallow a method mismatch and report it as 404, because a pattern that
	// matches the request wins over the mux's built-in 405.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("/healthz", methodNotAllowed(http.MethodGet))

	mux.Handle("GET /v1/info", s.authenticated(http.HandlerFunc(s.handleInfo)))
	mux.HandleFunc("/v1/info", methodNotAllowed(http.MethodGet))

	// The only endpoint that changes the host rather than reporting on it. It is
	// authenticated like the others and does nothing this deployment was not
	// already set up for: see internal/update on why the probe asks instead of
	// replacing its own binary.
	mux.Handle("POST /v1/update", s.authenticated(http.HandlerFunc(s.handleUpdate)))
	mux.HandleFunc("/v1/update", methodNotAllowed(http.MethodPost))

	mux.Handle("POST /v1/checks", s.authenticated(http.HandlerFunc(s.handleChecks)))
	mux.HandleFunc("/v1/checks", methodNotAllowed(http.MethodPost))

	mux.HandleFunc("/", handleNotFound)

	return s.recoverPanic(s.logRequests(mux))
}
