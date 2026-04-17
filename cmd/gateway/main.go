// Command gateway is the opencost-ai HTTP gateway entry point.
//
// Per CLAUDE.md, cmd/gateway is wire-up only: load config, build an
// HTTP server, install signal handling, run. Request handling, auth,
// audit, and bridge I/O live under internal/.
//
// The process owns two HTTP listeners:
//
//   - the public API on cfg.ListenAddr, exposing /v1/health (unauth)
//     and the authenticated v1 endpoints.
//   - the metrics listener on cfg.MetricsListenAddr (loopback by default
//     per docs/architecture.md §7.6), exposing /metrics.
//
// Both listeners share the same audit/ratelimit/metrics wiring. Signal
// handling drains both.
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
	"sync"
	"syscall"
	"time"

	"github.com/opencost/opencost-ai/internal/audit"
	"github.com/opencost/opencost-ai/internal/auth"
	"github.com/opencost/opencost-ai/internal/bridge"
	"github.com/opencost/opencost-ai/internal/config"
	"github.com/opencost/opencost-ai/internal/metrics"
	"github.com/opencost/opencost-ai/internal/ratelimit"
	"github.com/opencost/opencost-ai/internal/server"
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
		"metrics_listen_addr", cfg.MetricsListenAddr,
		"bridge_url", cfg.BridgeURL,
		"default_model", cfg.DefaultModel,
		"rate_limit_per_min", cfg.RateLimitPerMin,
		"audit_log_query", cfg.AuditLogQuery,
	)

	bc, err := bridge.New(cfg.BridgeURL)
	if err != nil {
		return fmt.Errorf("bridge client: %w", err)
	}

	auditLogger := audit.NewLogger(os.Stdout, cfg.AuditLogQuery)
	limiter := ratelimit.New(cfg.RateLimitPerMin)
	reg := metrics.NewRegistry()
	authSource := auth.NewSource(cfg.AuthTokenFile)

	apiHandler, err := server.New(server.Options{
		Bridge:          bc,
		AuthValidator:   authSource,
		DefaultModel:    cfg.DefaultModel,
		MaxRequestBytes: cfg.MaxRequestBytes,
		Logger:          logger,
		Audit:           auditLogger,
		RateLimiter:     limiter,
		Metrics:         reg,
	})
	if err != nil {
		return fmt.Errorf("server.New: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           newMux(logger, apiHandler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.RequestTimeout,
		// WriteTimeout must be zero on the public listener: SSE
		// responses stream for arbitrarily long while the model emits
		// tokens, so a hard cap here would cut the stream mid-frame.
		// Per-request deadlines live inside the handler instead.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	metricsSrv := &http.Server{
		Addr:              cfg.MetricsListenAddr,
		Handler:           newMetricsMux(reg),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// Trap SIGTERM (k8s) and SIGINT (local ctrl-c). SIGTERM is the
	// important one: the kubelet sends it before killing the pod.
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	type serveResult struct {
		name string
		err  error
	}
	results := make(chan serveResult, 2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			results <- serveResult{name: "api", err: err}
			return
		}
		results <- serveResult{name: "api"}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			results <- serveResult{name: "metrics", err: err}
			return
		}
		results <- serveResult{name: "metrics"}
	}()

	select {
	case res := <-results:
		// An unexpected early return from either listener is fatal.
		if res.err != nil {
			// Best-effort shutdown of the other server before returning.
			shutdownCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
			_ = srv.Shutdown(shutdownCtx)
			_ = metricsSrv.Shutdown(shutdownCtx)
			cancel()
			wg.Wait()
			return fmt.Errorf("%s listener: %w", res.name, res.err)
		}
	case <-sigCtx.Done():
		logger.Info("shutdown signal received, draining")
	}

	// Derive shutdown from the parent ctx, not context.Background, so a
	// test or embedded caller that cancels its ctx also aborts the
	// drain instead of waiting out RequestTimeout.
	shutdownCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil && shutdownErr == nil {
		shutdownErr = err
	}
	wg.Wait()
	// Drain any remaining results so the goroutines that delivered them
	// did not race past the select above.
	for len(results) > 0 {
		<-results
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutdown: %w", shutdownErr)
	}
	logger.Info("gateway stopped cleanly")
	return nil
}

// newMux composes the public-listener handler tree: /v1/health remains
// unauthenticated so liveness probes succeed even if the bridge is
// unreachable, and every other /v1/* route is delegated to the
// authenticated handler returned by server.New.
func newMux(logger *slog.Logger, apiHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", healthHandler(logger))
	// Subtree match so /v1/ask, /v1/tools, /v1/models all reach the
	// authenticated handler. Method-specific patterns registered above
	// take precedence over the subtree, so /v1/health is not shadowed.
	mux.Handle("/v1/", apiHandler)
	return mux
}

// newMetricsMux binds /metrics to the registry handler. The listener
// is bound to loopback by default; see DefaultMetricsListenAddr.
func newMetricsMux(reg *metrics.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", reg.Handler())
	return mux
}

// healthHandler returns a stable JSON liveness document. It does not
// probe upstream dependencies — that concern belongs to a future
// /v1/ready endpoint. Per apiv1.HealthResponse, this endpoint is
// liveness-only in v0.1; Kubernetes readiness probes must not target
// it yet.
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
