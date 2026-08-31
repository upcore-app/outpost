// Command outpost is a remote check probe for upcore. upcore dispatches batches
// of checks to it and aggregates the results; the outpost keeps no history and
// never calls back.
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

	key, err := apikey.Resolve(cfg.APIKey, cfg.DataDir, log)
	if err != nil {
		log.Error("cannot establish an API key", "error", err)
		os.Exit(1)
	}

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

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
