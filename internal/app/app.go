package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/qovira/qovira/internal/httpx"
)

// HTTP server and graceful-shutdown constants. Kept together so promotion to
// config is a single, localized change.
const (
	// serverReadHeaderTimeout limits the time allowed to read request headers.
	// Setting this prevents the Slowloris class of slow-client attacks (gosec G112).
	serverReadHeaderTimeout = 10 * time.Second

	// serverReadTimeout limits the time allowed to read the full request body.
	serverReadTimeout = 30 * time.Second

	// serverWriteTimeout limits the time allowed to write the full response.
	serverWriteTimeout = 30 * time.Second

	// serverIdleTimeout is the maximum time to wait for the next request when
	// keep-alives are enabled.
	serverIdleTimeout = 120 * time.Second

	// shutdownGrace is the maximum time given to in-flight requests to complete
	// after the shutdown signal is received before the server forcibly closes.
	shutdownGrace = 15 * time.Second
)

// Run is the application entry point. It builds the logger, resolves SPA
// assets, wires the HTTP server, and blocks until ctx is cancelled, at which
// point it drains in-flight requests via [http.Server.Shutdown].
func Run(ctx context.Context, cfg Config) error {
	logger, err := BuildLogger(cfg, os.Stdout)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}

	slog.SetDefault(logger)

	assets, err := httpx.Assets()
	if err != nil {
		return fmt.Errorf("resolve SPA assets: %w", err)
	}

	mux := http.NewServeMux()

	// Liveness probe — trivially returns 200.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// All other paths — SPA handler with index.html fallback.
	mux.Handle("/", httpx.NewSPAHandler(assets))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}

	// Serve in a background goroutine so we can block on context cancel.
	serveErr := make(chan error, 1)

	go func() {
		logger.Info("server starting", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}

		close(serveErr)
	}()

	// Block until context is cancelled (SIGINT / SIGTERM via signal.NotifyContext).
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("server: %w", err)
		}

		return nil
	}

	// Graceful shutdown: give in-flight requests up to shutdownGrace to finish.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	logger.Info("server stopped gracefully")

	return nil
}

// BuildLogger constructs a [*slog.Logger] that writes to w, using the level
// and format from cfg. It is exported so it can be tested and wired
// independently of [Run].
func BuildLogger(cfg Config, w io.Writer) (*slog.Logger, error) {
	var level slog.Level

	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return nil, fmt.Errorf("parse log level %q: %w", cfg.LogLevel, err)
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler

	switch cfg.LogFormat {
	case "json":
		handler = slog.NewJSONHandler(w, opts)
	case "text":
		handler = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q", cfg.LogFormat)
	}

	return slog.New(handler), nil
}
