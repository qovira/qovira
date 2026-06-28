// Package app is the composition root for the Qovira application server. It reads env config, builds the slog
// logger, wires the HTTP server, and owns the run / graceful-shutdown lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/qovira/qovira/internal/api"
	"github.com/qovira/qovira/internal/buildinfo"
	"github.com/qovira/qovira/internal/httpx"
)

// HTTP server and graceful-shutdown constants. Kept together so promotion to config is a single, localized change.
const (
	// serverReadHeaderTimeout limits the time allowed to read request headers. Setting this prevents the
	// Slowloris class of slow-client attacks (gosec G112).
	serverReadHeaderTimeout = 10 * time.Second

	// serverReadTimeout limits the time allowed to read the full request body.
	serverReadTimeout = 30 * time.Second

	// serverWriteTimeout limits the time allowed to write the full response.
	serverWriteTimeout = 30 * time.Second

	// serverIdleTimeout is the maximum time to wait for the next request when keep-alives are enabled.
	serverIdleTimeout = 120 * time.Second

	// shutdownGrace is the maximum time given to in-flight requests to complete after the shutdown signal is
	// received before the server forcibly closes.
	shutdownGrace = 15 * time.Second
)

// Run is the application entry point. It builds the logger, resolves SPA assets, wires the HTTP server, and
// blocks until ctx is cancelled, at which point it drains in-flight requests via [http.Server.Shutdown].
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

	// Resolve build identity once at startup. The package-level ldflags vars are set by the Makefile /
	// Dockerfile / CI; Resolve fills gaps from runtime/debug.ReadBuildInfo for development builds.
	bi := buildinfo.Resolve(buildinfo.Info{
		Version:   buildinfo.Version,
		Commit:    buildinfo.Commit,
		BuildTime: buildinfo.BuildTime,
	})

	mux := http.NewServeMux()

	// Mount the Huma API under /api/v1. Go 1.22 most-specific-pattern routing ensures /api/v1/... requests
	// reach Huma before the SPA catch-all, so no manual prefix-stripping is needed.
	api.New(mux, bi, logger)

	// All other paths — SPA handler with index.html fallback.
	mux.Handle("/", httpx.NewSPAHandler(assets))

	// TODO(security): wrap mux in a security-headers middleware (X-Content-Type-Options: nosniff at minimum;
	// add CSP + frame-ancestors) before a real web client ships. While only static placeholder assets are
	// served this is defense-in-depth, not yet exploitable.
	// TODO(security): add http.MaxBytesHandler (a request body size limit) once any endpoint reads a request
	// body — ReadTimeout bounds time, not bytes.
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}

	// Bind synchronously so a bind failure (invalid or already-bound address) is returned directly, and the
	// "listening" line is logged only once the socket is actually open.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", cfg.Addr, err)
	}

	logger.Info("server listening", "addr", ln.Addr().String())

	// Serve in a background goroutine so we can block on context cancellation.
	serveErr := make(chan error, 1)

	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}

		close(serveErr)
	}()

	// Block until the context is cancelled (SIGINT / SIGTERM via signal.NotifyContext) or the server fails.
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}

		return nil
	}

	// Graceful shutdown: give in-flight requests up to shutdownGrace to finish.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	// Surface a serve error that raced the shutdown signal: if Serve failed at the same instant ctx was
	// cancelled, the select above may have taken the ctx.Done() branch and skipped it. The goroutine always
	// closes serveErr, so this receives nil on a clean shutdown.
	if err := <-serveErr; err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	logger.Info("server stopped gracefully")

	return nil
}

// BuildLogger constructs a [*slog.Logger] that writes to w, using the level and format from cfg. It is
// exported so it can be tested and wired independently of [Run].
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
