// Command gateway is the opencost-ai HTTP gateway entry point.
//
// Per CLAUDE.md, cmd/gateway is wire-up only: load config, build an
// HTTP server, install signal handling, run. Request handling, auth,
// audit, and bridge I/O live under internal/.
//
// This initial scaffold exposes a single liveness endpoint
// (GET /v1/health) and handles SIGTERM/SIGINT by shutting down the
// server with the configured request timeout as its drain budget.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/opencost/opencost-ai/internal/config"
	"github.com/opencost/opencost-ai/pkg/apiv1"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if err := run(context.Background(), logger); err != nil {
		logger.Error("gateway exited with error", "err", err)
		os.Exit(1)
	}
}

// run owns the process lifecycle. Split out from main so it can be
// exercised by future tests without os.Exit in the call path.
func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.Load(config.OSGetenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	logger.Info("gateway starting",
		"version", version,
		"listen_addr", cfg.ListenAddr,
		"bridge_url", cfg.BridgeURL,
		"default_model", cfg.DefaultModel,
	)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           newMux(logger),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.RequestTimeout,
		WriteTimeout:      cfg.RequestTimeout,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// Trap SIGTERM (k8s) and SIGINT (local ctrl-c). SIGTERM is the
	// important one: the kubelet sends it before killing the pod.
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	case <-sigCtx.Done():
		logger.Info("shutdown signal received, draining")
	}

	// Derive shutdown from the parent ctx, not context.Background, so a
	// test or embedded caller that cancels its ctx also aborts the
	// drain instead of waiting out RequestTimeout.
	shutdownCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serverErr; err != nil {
		return fmt.Errorf("listen after shutdown: %w", err)
	}
	logger.Info("gateway stopped cleanly")
	return nil
}

// newMux builds the HTTP routing tree. Only /v1/health is live in this
// scaffold; the remaining /v1 endpoints land in follow-up commits once
// the auth, bridge, and prompt packages exist.
func newMux(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", healthHandler(logger))
	return mux
}

// healthHandler returns a stable JSON liveness document. It does not
// probe upstream dependencies — that concern belongs to a future
// /v1/ready endpoint that will land with internal/bridge. Per
// apiv1.HealthResponse, this endpoint is liveness-only in v0.1;
// Kubernetes readiness probes must not target it yet.
func healthHandler(logger *slog.Logger) http.HandlerFunc {
	resp := apiv1.HealthResponse{Status: "ok", Version: version}
	body, err := json.Marshal(resp)
	if err != nil {
		// Marshalling a constant struct cannot fail; if it does, the
		// build is broken in a way we want to see immediately.
		panic(fmt.Errorf("marshal health response: %w", err))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			logger.Warn("write health response", "err", err)
		}
	}
}
