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
	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/httpx"
)

// HTTP server and graceful-shutdown constants. Kept together so promotion to config is a single, localized change.
const (
	serverReadHeaderTimeout = 10 * time.Second
	serverReadTimeout       = 30 * time.Second
	serverWriteTimeout      = 30 * time.Second
	serverIdleTimeout       = 120 * time.Second
	shutdownGrace           = 15 * time.Second
)

// HubShutdowner is the interface consumed by GracefulShutdown for the event hub drain step. It is satisfied
// by *events.Hub; extracted here so graceful-shutdown logic can be unit-tested with a fake.
type HubShutdowner interface {
	Shutdown(ctx context.Context) error
}

// ServerShutdowner is the interface consumed by GracefulShutdown for the HTTP server drain and force-close
// steps. It is satisfied by *http.Server; extracted here so graceful-shutdown logic can be unit-tested with
// a fake.
type ServerShutdowner interface {
	Shutdown(ctx context.Context) error
	Close() error
}

// GracefulShutdown drains the hub, then the HTTP server, each within its own grace budget (derived via
// context.WithoutCancel so a cancelled root ctx does not pre-expire them). The hub-first order is required
// by Run's shutdown block. A hub-drain timeout is logged and tolerated; a server-shutdown timeout triggers
// srv.Close() and returns nil — force-closing stragglers is the documented fallback, not an error. Only a
// non-timeout srv.Shutdown error is returned to the caller.
func GracefulShutdown(ctx context.Context, hub HubShutdowner, srv ServerShutdowner, grace time.Duration) error {
	hubCtx, hubCancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer hubCancel()

	if err := hub.Shutdown(hubCtx); err != nil {
		// A partial drain is fine — the server step below force-closes any stragglers. Warn, don't fail.
		slog.Warn("hub shutdown did not fully drain within budget — will force-close remainder via srv.Close()",
			"err", err,
		)
	}

	// SSE connections the hub already drained return immediately; any straggler waits up to grace.
	srvCtx, srvCancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer srvCancel()

	if err := srv.Shutdown(srvCtx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// Shutdown does not interrupt active connections — only Close() does. Expected wedged-client
			// case, so Warn (not Error) and return nil rather than failing Run.
			slog.Warn("srv.Shutdown timed out — force-closing remaining connections via srv.Close()")

			if cerr := srv.Close(); cerr != nil {
				slog.Warn("srv.Close() after shutdown timeout returned error", "err", cerr)
			}

			return nil
		}

		return fmt.Errorf("graceful shutdown: %w", err)
	}

	return nil
}

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

	api.New(mux, bi, logger)

	hub := events.New(events.DefaultBufferSize)

	mux.Handle("GET /events", events.NewHandler(hub, logger, events.DefaultTimings))
	mux.Handle("/", httpx.NewSPAHandler(assets))

	// TODO(security): add CSP and X-Frame-Options / frame-ancestors before a real web client ships.
	// X-Content-Type-Options: nosniff is now handled by NewSecurityHeadersMiddleware (outermost layer below).

	// Server-edge middleware chain, applied inner→outer: the last wrap is the outermost layer and runs
	// first on each request. The order is load-bearing — each constructor's doc states its own placement
	// constraint.

	// TODO(config): wire CORSConfig.AllowedOrigins from the instance config model (unit 9) so operators
	// can configure allowed origins for their deployment without recompiling.
	corsPolicy := httpx.CORSConfig{
		AllowedOrigins: nil, // same-origin-default: no cross-origin access until unit 9 wires origins
	}

	handler := http.Handler(mux)
	handler = httpx.NewCORSMiddleware(corsPolicy, handler)
	handler = httpx.NewRecoveryMiddleware(logger, handler)
	handler = httpx.NewAccessLogMiddleware(logger, handler)
	handler = httpx.NewRequestIDMiddleware(handler)
	handler = http.MaxBytesHandler(handler, httpx.MaxBodyBytes)
	handler = httpx.NewSecurityHeadersMiddleware(handler)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
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

	serveErr := make(chan error, 1)

	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}

		close(serveErr)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}

		return nil
	}

	// Drain the hub BEFORE srv.Shutdown. An SSE connection never looks "idle" to net/http (its body streams
	// indefinitely), so a server-first shutdown would block on every open stream until the grace budget
	// expired, then hard-close them mid-stream. Draining the hub first makes each connection emit a
	// system.shutdown frame and return, so srv.Shutdown then finds no live connections and exits promptly.
	if err := GracefulShutdown(ctx, hub, srv, shutdownGrace); err != nil {
		return err
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
