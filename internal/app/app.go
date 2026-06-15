// Package app is the composition root for the serve command. It wires the full dependency graph in one explicit,
// top-to-bottom function (New), manages the application lifecycle (Run), and defines the Module seam that sibling
// feature slices use to register their routes and tools.
//
// No init() self-registration, DI container, or service locator is used.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/qovira/qovira/internal/auth"
	"github.com/qovira/qovira/internal/authhttp"
	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/harness"
	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/reminders"
	"github.com/qovira/qovira/internal/scheduler"
	"github.com/qovira/qovira/internal/store"
)

// shutdownTimeout is the maximum time allowed for the ordered shutdown sequence (HTTP drain → scheduler stop → store
// close). It is intentionally generous: SSE connections are cancelled first via BaseContext so Shutdown drains quickly.
const shutdownTimeout = 15 * time.Second

// Module is the seam that feature slices implement to register their HTTP routes and capability tools at boot time. New
// iterates over all registered modules and calls Routes then Tools for each.
//
// Name returns a stable, unique module identifier used as the registry key and in logs. It must be distinct across all
// modules registered with a single App.
//
// Routes receives the shared *httpx.Router; modules mount their own fully-qualified patterns under /api/v1/... by
// convention. Modules must NOT register the server's reserved patterns (/healthz, /events, /, /api/v1/{path...}) —
// doing so panics on duplicate ServeMux registration.
//
// Tools returns the capability.Tool descriptors contributed by this module. The Capability Registry Spec fleshes out
// the full schema; returning nil or an empty slice is valid for modules that have no tools yet.
type Module interface {
	Name() string
	Routes(r *httpx.Router)
	Tools() []capability.Tool
}

// isPublicRoute reports whether the request targets a route that must be reachable without authentication.
// The auth middleware uses this predicate; all other cross-cutting middleware (recover, request-id, log,
// security-headers) run for every request including public ones.
//
// Explicitly public routes (reviewed list — add new public routes here with a comment):
//   - /healthz — liveness probe; must be reachable by the container runtime.
//   - POST /api/v1/auth/login — credential exchange; must be reachable before a session exists.
//     (Onboarding / bootstrap routes that require unauthenticated access will join this list
//     once their spec is finalised; do NOT invent their paths here ahead of that work.)
//   - SPA paths — anything that is not /api/v1/… or /events is served by the embedded SPA
//     and is therefore public (asset delivery; auth lives inside the SPA).
//
// Protected: /api/v1/… (all paths, except the explicit per-method exemptions above), /events.
func isPublicRoute(r *http.Request) bool {
	path := r.URL.Path

	// Liveness probe — always public.
	if path == "/healthz" {
		return true
	}

	// SSE stream — always protected.
	if path == "/events" {
		return false
	}

	// Per-method API exemptions: login is public only for POST.
	if path == "/api/v1/auth/login" && r.Method == http.MethodPost {
		return true
	}

	// All other /api/v1/… paths are protected.
	if path == "/api/v1" || (len(path) > len("/api/v1") && path[:len("/api/v1/")] == "/api/v1/") {
		return false
	}

	// Everything else (SPA paths, static assets) is public.
	return true
}

// App holds the wired dependency graph and the handles needed for an ordered shutdown. Obtain one via New; drive it
// with Run.
type App struct {
	store       *store.Store
	srv         *http.Server
	cancelConns context.CancelFunc
	logger      *slog.Logger
	sched       *scheduler.Scheduler
}

// AuthModuleCtor returns a module constructor that builds the auth HTTP module
// from the store that app.New opens.  Pass it as one of the moduleCtors
// arguments to [New] in production.
//
// params, policy, and cfg are the argon2id, password policy, and session config
// respectively.  For production, pass [auth.DefaultParams],
// [auth.DefaultPolicy], and [auth.DefaultSessionConfig].
//
// logger is forwarded to the module so internal errors are diagnosable via
// server-side logs without leaking details to the client.
func AuthModuleCtor(
	params auth.Params,
	policy auth.Policy,
	cfg auth.SessionConfig,
	logger *slog.Logger,
) func(*store.Store) Module {
	return func(s *store.Store) Module {
		hasher := auth.NewHasher(params)
		svc := auth.NewService(s, hasher, policy)
		sessions := auth.NewSessions(s, cfg)
		return authhttp.New(svc, sessions, cfg, nil, logger) // nil clock → time.Now
	}
}

// New wires the full dependency graph in explicit, top-to-bottom order and returns a ready-to-run *App. It fails fast
// on any construction error; if the store opened successfully but a later step fails, New closes the store before
// returning so no resource leaks.
//
// version is the release version string reported by /healthz. Pass the value injected by the build system (the cli
// package's version var) so that /healthz always reflects the real release version rather than a hard-coded "dev"
// sentinel.
//
// newValidator is a constructor that receives the opened [*store.Store] and
// returns the [httpx.TokenValidator] used by the auth middleware. This
// two-phase design lets callers (e.g. serve.go) build an Authenticator that
// holds a reference to the store without exposing the store before it is fully
// initialised. Pass a func that wraps auth.NewAuthenticator for production; for
// tests any fake that satisfies the interface works:
//
//	func(s *store.Store) httpx.TokenValidator { return myFakeValidator{} }
//
// harnessCfg is the configuration for the AI harness. For production, pass the
// value populated in serve.go; for tests pass harness.Config{} (the zero value
// is valid for this slice).
//
// moduleCtors is a slice of module constructors — each receives the opened store
// and returns a [Module].  This two-phase design mirrors newValidator: modules
// can hold store references without being constructed before the store opens.
// Pass [AuthModuleCtor] and any other feature-slice ctors for production; tests
// may pass none (or wrap a [fakeModule]) to exercise wiring in isolation.
//
// Order:
//  1. Open the encrypted store (store.Open).
//  2. Call newValidator(s) to build the token validator.
//  3. Build each module by calling moduleCtors[i](s).
//  4. If cfg.AutoMigrate, run all pending migrations against the write pool.
//  5. Construct the in-memory event bus.
//  6. Construct the scheduler (validates config; does NOT start yet — all handlers
//     and periodic jobs must be registered before Start is called).
//  7. Construct the capability registry.
//  8. Construct the HTTP router.
//  9. For each module: mount routes onto the router, register tools in the registry.
//  10. Construct the AI harness and mount its routes.
//  11. Register the harness.sweep_confirmations handler on the scheduler.
//  12. Upsert the harness.sweep_confirmations periodic job (idempotent on restart;
//     a failure on an unmigrated DB is logged and tolerated, not fatal).
//  13. Start the scheduler (all handlers and periodic jobs are now registered).
//  14. Build the HTTP server with the StandardChain middleware.
func New(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	newValidator func(*store.Store) httpx.TokenValidator,
	version string,
	harnessCfg harness.Config,
	moduleCtors ...func(*store.Store) Module,
) (_ *App, err error) {
	// Step 1: open the encrypted store.
	s, err := store.Open(store.Config{
		Path: store.DBPath(cfg.DataDir),
		Key:  string(cfg.MasterKey),
	})
	if err != nil {
		return nil, fmt.Errorf("app: open store: %w", err)
	}
	// From this point forward, any error must close the store.
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()

	// Step 2: build the token validator now that the store is open.
	validator := newValidator(s)

	// Step 3: build each module now that the store is open.
	modules := make([]Module, 0, len(moduleCtors))
	for _, ctor := range moduleCtors {
		modules = append(modules, ctor(s))
	}

	// Step 4: run migrations on boot when requested.
	if cfg.AutoMigrate {
		runner := store.NewRunner()
		if err = runner.Up(ctx, s.Writer()); err != nil {
			return nil, fmt.Errorf("app: auto-migrate: %w", err)
		}
	}

	// Step 5: in-memory event bus.
	bus := events.NewBus()

	// Step 6: build the scheduler (validated here so a bad config fails boot immediately)
	// but do NOT start it yet — it must start after the harness is constructed and all
	// periodic jobs are registered (step 13 below).
	schedCfg := scheduler.DefaultConfig()
	if err = schedCfg.Validate(); err != nil {
		return nil, fmt.Errorf("app: scheduler config: %w", err)
	}
	sched := scheduler.New(s, bus, schedCfg)

	// Step 7: capability registry.
	reg := capability.NewRegistry()

	// Step 8: HTTP router.
	router := httpx.NewRouter()

	// Step 9: module registration loop.
	for _, m := range modules {
		m.Routes(router)
		if err := reg.Add(m); err != nil {
			return nil, fmt.Errorf("app: register %s tools: %w", m.Name(), err)
		}
	}

	// Step 10: construct the AI harness and mount its routes. The harness is wired
	// with reg, gw, s, bus, and harnessCfg. It does not contribute capability tools
	// (Tools() returns nil), so it is not passed through reg.Add.
	// newChatter is provided by chatter_default.go (production) or chatter_e2e.go
	// (e2e build tag); the production file always returns a *gateway.Gateway.
	gw := newChatter(s.Settings())
	h := harness.New(reg, gw, s, bus, harnessCfg, logger)
	h.Routes(router)

	// Step 10b: construct the reminders module and mount its routes. reminders.New
	// registers the "reminder.fire" handler on sched before Start is called (step 13),
	// satisfying the invariant that all handlers are registered before the first tick.
	remMod := reminders.New(s, sched, bus, sched)
	remMod.Routes(router)
	if err := reg.Add(remMod); err != nil {
		return nil, fmt.Errorf("app: register %s tools: %w", remMod.Name(), err)
	}

	// Step 11: register scheduler handlers that depend on the harness being constructed.
	// The handler must be registered before RegisterPeriodic upserts the row, and both
	// must run before sched.Start so the first tick can dispatch the job.
	sched.Register("harness.sweep_confirmations", func(ctx context.Context, _ scheduler.Job) error {
		_, sweepErr := h.SweepExpiredConfirmations(ctx)
		return sweepErr
	})

	// Step 12: upsert the harness sweep as a system periodic job (idempotent across
	// restarts). run_at = now+1m on INSERT so it never fires during fast boot-time tests
	// and does not spam on restart. On CONFLICT the schedule is updated in place.
	//
	// Registration is unconditional: in every correctly-provisioned deployment the schema
	// is present — whether applied by AutoMigrate or out-of-band via `qovira migrate` — so
	// the sweep is always registered (gating on AutoMigrate would wrongly skip it on a
	// migrate-then-serve deployment). A failure (e.g. the jobs table is absent on an
	// intentionally-unmigrated DB) is logged and tolerated rather than failing boot,
	// mirroring the scheduler's boot-sweep, which also soft-fails on a missing table —
	// app.New does not require the schema to be present.
	if regErr := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  "harness.sweep_confirmations",
		Kind: "harness.sweep_confirmations",
		Recurrence: &scheduler.Recurrence{
			Every: time.Minute,
		},
	}); regErr != nil {
		logger.Warn("app: could not register harness sweep periodic job; continuing without it", "err", regErr)
	}

	// Step 13: start the scheduler now that all handlers and periodic jobs are registered.
	if err = sched.Start(ctx); err != nil {
		return nil, fmt.Errorf("app: scheduler start: %w", err)
	}

	// Step 14: build the HTTP server with the standard middleware chain.
	// The connection context (connCtx) is a cancelable parent given to every request via srv.BaseContext. Cancelling
	// it before srv.Shutdown is called causes long-lived SSE handlers to return (they select on r.Context().Done()),
	// so Shutdown drains quickly rather than waiting for the full timeout.
	connCtx, cancelConns := context.WithCancel(context.Background())
	mws := httpx.StandardChain(logger, validator, isPublicRoute)
	srv := httpx.NewServer(cfg.HTTPAddr, version, router, bus, mws...)
	srv.BaseContext = func(net.Listener) context.Context { return connCtx }

	return &App{
		store:       s,
		srv:         srv,
		cancelConns: cancelConns,
		logger:      logger,
		sched:       sched,
	}, nil
}

// Run starts the HTTP server and blocks until ctx is cancelled (signal) or the server encounters a fatal error (e.g.
// address already in use).
//
// On ctx cancellation Run performs an ordered, bounded shutdown:
//  1. Cancel the connection context to unblock live SSE handlers, then call
//     srv.Shutdown to drain in-flight HTTP requests.
//  2. Stop the scheduler: cancels in-flight handlers and drains the worker pool.
//  3. Close both database pools.
//
// Shutdown errors are logged and returned so the caller can exit non-zero if any teardown step fails. All three steps
// always run regardless of an earlier failure so teardown is never skipped.
func (a *App) Run(ctx context.Context) error {
	// Start the listener in the background; forward errors on a channel.
	listenErr := make(chan error, 1)
	go func() {
		if err := a.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
		}
		close(listenErr)
	}()

	// Block until the server fails or the caller cancels the context.
	select {
	case err := <-listenErr:
		// Fatal startup error (e.g. EADDRINUSE). Run the shutdown steps so the store is still closed cleanly, then
		// return the original error.
		// Shutdown errors are logged inside shutdown(); we discard the joined error here to surface the root cause
		// (the listen failure) to the caller.
		_ = a.shutdown()
		return err
	case <-ctx.Done():
		// Normal signal-driven shutdown path.
	}

	return a.shutdown()
}

// shutdown runs the ordered teardown steps and returns the first non-nil error encountered. All steps always run so
// nothing is skipped if an earlier step fails.
func (a *App) shutdown() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var errs []error

	// Step 1: cancel live connections (SSE), then drain HTTP.
	a.cancelConns()
	if err := a.srv.Shutdown(shutdownCtx); err != nil {
		a.logger.Error("app: http shutdown error", "err", err)
		errs = append(errs, fmt.Errorf("app: http shutdown: %w", err))
	}

	// Step 2: stop the scheduler -- cancels in-flight handlers and drains the worker pool.
	if a.sched != nil {
		if schedErr := a.sched.Stop(shutdownCtx); schedErr != nil {
			a.logger.Error("app: scheduler stop error", "err", schedErr)
			errs = append(errs, fmt.Errorf("app: scheduler stop: %w", schedErr))
		}
	}

	// Step 3: close the store. A non-nil joined error may surface a failed write-pool close (indicating
	// uncheckpointed WAL data).
	if closeErr := a.store.Close(); closeErr != nil {
		a.logger.Error("app: store close error", "err", closeErr,
			slog.Bool("writePoolError", errors.Is(closeErr, store.ErrWritePoolClose)),
		)
		errs = append(errs, fmt.Errorf("app: store close: %w", closeErr))
	}

	return errors.Join(errs...)
}

// Store returns the underlying *store.Store. It is exported for use in tests that need to verify the store is closed
// after shutdown.
func (a *App) Store() *store.Store { return a.store }

// Server returns the underlying *http.Server. Exposed for tests that probe the handler directly without starting a real
// listener.
func (a *App) Server() *http.Server { return a.srv }

// Logger returns the logger in use. Useful in tests to access the configured slog.Logger.
func (a *App) Logger() *slog.Logger { return a.logger }
