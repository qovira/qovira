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

// GracefulShutdown drains the hub and then the HTTP server in two sequential steps, each with its own
// independent timeout budget (derived from ctx via context.WithoutCancel so that a cancelled root ctx does
// not poison the budgets). The steps are intentionally ordered hub-first, server-second — see the inline
// comment inside Run for the rationale.
//
// Hub timeout: if hub.Shutdown exhausts its budget the error is logged at Warn and we proceed — the server
// shutdown step will force-close any SSE connections that did not drain in time.
//
// Server timeout: if srv.Shutdown exhausts its budget, srv.Close() is called to force-close all remaining
// open connections, and GracefulShutdown returns nil. A deadline/timeout on the server side means the
// connections are now force-closed — that is the documented fallback, not a Run-level failure. Only a
// non-timeout error from srv.Shutdown is surfaced to the caller.
func GracefulShutdown(ctx context.Context, hub HubShutdowner, srv ServerShutdowner, grace time.Duration) error {
	// Each step gets its own fresh budget. context.WithoutCancel ensures a cancelled root ctx (i.e. the
	// shutdown signal itself) does not immediately expire these derived contexts — only the per-step timeout
	// governs them.
	hubCtx, hubCancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer hubCancel()

	if err := hub.Shutdown(hubCtx); err != nil {
		// A partial drain is acceptable: srv.Close() below will force-close any connections that did not drain.
		// Log at Warn so operators can see wedged clients, but do not treat it as a Run-level failure.
		slog.Warn("hub shutdown did not fully drain within budget — will force-close remainder via srv.Close()",
			"err", err,
		)
	}

	// Give the HTTP server its own independent budget. srv.Shutdown asks active connections to finish their
	// current requests and then returns; for SSE connections that the hub already drained this returns
	// immediately. For any that did not drain (e.g. non-SSE long-polling), srv.Shutdown will wait up to
	// grace; if it still times out, srv.Close() actually force-closes the connections.
	srvCtx, srvCancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer srvCancel()

	if err := srv.Shutdown(srvCtx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// The server-shutdown budget expired — force-close all remaining connections now. This is
			// the correct fallback: net/http.Server.Shutdown does NOT interrupt active connections;
			// only srv.Close() does. Log at Warn (not Error — this is an expected wedged-client scenario)
			// and return nil so the caller (Run) does not treat a stuck client as a fatal error.
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

	// Mount the Huma API under /api/v1. Go 1.22 most-specific-pattern routing ensures /api/v1/... requests
	// reach Huma before the SPA catch-all, so no manual prefix-stripping is needed.
	api.New(mux, bi, logger)

	// Construct the event hub. hub.Shutdown is called in the graceful-shutdown block below (before
	// srv.Shutdown) to drain all open SSE connections before the process exits.
	hub := events.New(events.DefaultBufferSize)

	// Mount the SSE endpoint. Go 1.22 most-specific-pattern routing sends GET /events to this handler
	// and all other paths (including non-GET /events) to the SPA catch-all below.
	mux.Handle("GET /events", events.NewHandler(hub, logger, events.DefaultTimings))

	// All other paths — SPA handler with index.html fallback.
	mux.Handle("/", httpx.NewSPAHandler(assets))

	// TODO(security): add CSP and X-Frame-Options / frame-ancestors before a real web client ships.
	// X-Content-Type-Options: nosniff is now handled by NewSecurityHeadersMiddleware (outermost layer below).

	// Compose the server-edge middleware chain. Layer order matters — each line wraps everything below it:
	//
	//   SecurityHeaders — outermost: sets X-Content-Type-Options: nosniff (and, once wired, CSP/frame-ancestors)
	//                     on every response — SPA assets, API JSON, SSE streams, recovery 500s, and CORS
	//                     preflights — so no response ever escapes without the baseline security headers.
	//   MaxBytesHandler — body-size gate; pairs the 4 MiB Huma per-op cap with a server-edge backstop so
	//                     bodies are rejected before any handler reads them.
	//   RequestID       — injects the req_… correlation token into context and sets the Request-Id header;
	//                     must sit outside everything that reads the ID (access-log, recovery, error edge).
	//   AccessLog       — emits one structured slog line per request; must be outside recovery so it
	//                     observes the 500 that recovery writes after a panic (AC3).
	//   Recovery        — catches panics from the non-API surface and returns a generic 500; must be inside
	//                     the access-log so the logged status is the recovered one.
	//   CORS            — answers OPTIONS preflights before routing; must be inside recovery so a bug in
	//                     the CORS logic is caught rather than dropping the connection.

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

	// Graceful shutdown: the order here is intentionally inverted — hub.Shutdown runs BEFORE srv.Shutdown.
	//
	// Why: an SSE connection is never "idle" from net/http's perspective (it has an open, streaming response
	// body that never naturally completes). Calling srv.Shutdown first would block on every open SSE stream
	// until the shutdownGrace budget expired, then hard-close them — a silent mid-stream drop for every user.
	//
	// By draining the hub first, every connection goroutine receives the hub.Done() signal, writes a
	// system.shutdown frame (so the client knows to reconnect), and returns — releasing its http.ResponseWriter
	// and allowing net/http to mark the connection as idle. srv.Shutdown then finds no live connections and
	// returns promptly.
	//
	// Each step gets its own independent timeout budget via GracefulShutdown — a hub drain timeout does NOT
	// poison the server-shutdown budget, and a server-shutdown timeout triggers srv.Close() (which actually
	// force-closes connections) rather than becoming a fatal Run error.
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
