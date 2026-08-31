// Command outpost is a remote check probe for upcore. upcore dispatches batches
// of checks to it and aggregates the results; the outpost keeps no history.
//
// It calls upcore exactly once, and only when deployed with a setup key: the
// auto-enrollment in internal/enroll. Everything after that is inbound.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/upcore-app/outpost/internal/apikey"
	"github.com/upcore-app/outpost/internal/config"
	"github.com/upcore-app/outpost/internal/enroll"
	"github.com/upcore-app/outpost/internal/server"
)

// version is a var, not a const, so the build can stamp it:
// -ldflags "-X main.version=1.2.3".
var version = "0.1.0"

// shutdownTimeout gives an in-flight batch time to finish. It is shorter than
// the server's WriteTimeout so a drain always ends, worst case by dropping the
// slowest batch.
const shutdownTimeout = 30 * time.Second

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg := config.Load(log)

	enrollment := enroll.Options{
		UpcoreURL: cfg.UpcoreURL,
		SetupKey:  cfg.SetupKey,
		PublicURL: cfg.PublicURL,
		Addr:      cfg.Addr,
		DataDir:   cfg.DataDir,
		Version:   version,
	}
	autoSetup := enroll.Configured(enrollment, log)

	// An auto-enrolling outpost sends its key to upcore itself, so the banner
	// that asks the operator to copy it would only be a second place the key
	// exists.
	key, err := apikey.Resolve(cfg.APIKey, cfg.DataDir, !autoSetup, log)
	if err != nil {
		log.Error("cannot establish an API key", "error", err)
		os.Exit(1)
	}
	enrollment.APIKey = key

	srv := server.New(cfg, key, version, log)

	log.Info("outpost starting",
		"version", version,
		"addr", cfg.Addr,
		"location", cfg.Location,
		"provider", cfg.Provider,
		"country", cfg.Country,
		"maxConcurrency", cfg.MaxConcurrency,
		"maxChecks", cfg.MaxChecks,
		"apiKey", apikey.Masked(key),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	// Auto-setup, when this outpost was deployed with a setup key. It runs
	// beside the server rather than before it: upcore calls back on /v1/info as
	// part of the enrollment, so something has to be listening by then.
	if autoSetup {
		go enroll.Run(ctx, enrollment, log)
	}

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server stopped", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		// Stop listening for further signals: a second Ctrl-C should kill the
		// process outright rather than be swallowed by the drain.
		stop()
		log.Info("shutdown signal received, draining", "timeout", shutdownTimeout.String())

		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(drainCtx); err != nil {
			log.Error("graceful shutdown failed", "error", err)
			os.Exit(1)
		}
		log.Info("outpost stopped")
	}
}
