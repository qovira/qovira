// Package scheduler implements the durable SQLite job queue for Qovira. It provides
// a poll-then-claim loop that atomically leases due jobs, dispatches them to registered
// handlers, and deletes rows on success. This package privately owns the jobs table;
// no other package may read or write it directly.
//
// Usage:
//
//	cfg := scheduler.DefaultConfig()
//	if err := cfg.Validate(); err != nil {
//	    return err // fails fast at boot
//	}
//	sched := scheduler.New(store, bus, cfg)
//	sched.Register("my.job", myHandler)
//	if err := sched.Start(ctx); err != nil {
//	    return err
//	}
//	defer sched.Stop(shutdownCtx)
package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/id"
	"github.com/qovira/qovira/internal/store"
	"github.com/qovira/qovira/internal/store/db"
)

// ── Public types ──────────────────────────────────────────────────────────────

// Handler is a function that executes a single job. The context carries the
// JobTimeout deadline; returning nil causes the row to be deleted (one-shot).
// Any error leaves the row in the running state (retry/dead-letter is a later slice).
type Handler func(ctx context.Context, job Job) error

// Job carries the resolved, handler-facing representation of a queued job. The
// scheduler resolves scope from the stored user_id and passes it here; handlers
// never see a raw user_id.
type Job struct {
	// ID is the ULID string identifier of the job.
	ID string
	// Kind is the dispatch selector used to find the registered Handler.
	Kind string
	// Payload is the opaque JSON text stored at enqueue time. The scheduler does
	// not interpret it; the handler's contract defines its shape.
	Payload json.RawMessage
	// Attempt is 1-based at dispatch (it was 0 before the first claim).
	Attempt int
	// Scope is resolved from the row's user_id column: system scope when NULL,
	// user scope otherwise. Handlers use this for data access, not authz.
	Scope store.Scope
}

// EnqueueRequest is the caller-facing specification for a new job.
type EnqueueRequest struct {
	// Kind is the dispatch selector. A registered handler for this kind must exist
	// before the job is due; no handler at execution time is a loud logged error.
	Kind string
	// Payload is the opaque JSON text passed through to the handler.
	Payload json.RawMessage
	// Scope determines user_id in the stored row: system scope → NULL, user scope
	// → the user's ID string.
	Scope store.Scope
	// RunAt is the earliest instant the job may run. Zero means now.
	RunAt time.Time
	// Key is an optional idempotency handle. When non-empty the insert is
	// ON CONFLICT(key) DO NOTHING, and the second Enqueue returns the existing id
	// without creating a new row. Leave empty for fire-and-forget jobs.
	Key string
	// Recurrence is accepted but one-shot here; recurrence behavior is a later slice.
	Recurrence *Recurrence
}

// Recurrence is a placeholder for future recurring job support. Fields are
// accepted but not persisted or acted on in this slice.
type Recurrence struct {
	// RRULE is an iCalendar RRULE string (e.g. "FREQ=DAILY;COUNT=10").
	RRULE string
	// TZ is the timezone name for the recurrence (e.g. "America/New_York").
	TZ string
	// Every is the interval in seconds between recurrences (alternative to RRULE).
	Every time.Duration
}

// ── Config ────────────────────────────────────────────────────────────────────

// Config holds the boot-time knobs for the Scheduler. Obtain a validated instance
// via DefaultConfig() then Validate(); the composition root calls Validate() so a
// bad config fails fast at boot.
type Config struct {
	// PollInterval is the time between claim ticks. Default: 1s.
	PollInterval time.Duration
	// Workers is the maximum number of concurrently running handlers. Default: 4.
	Workers int
	// JobTimeout is the per-job context deadline (from dispatch). Default: 30s.
	JobTimeout time.Duration
	// LeaseTimeout is the maximum time a row may stay in running state before it
	// is considered stale (reclaim on boot is a later slice). Default: 5m.
	// Invariant: LeaseTimeout > JobTimeout.
	LeaseTimeout time.Duration
	// BackoffBase is the base for the exponential retry backoff. Default: 10s.
	BackoffBase time.Duration
	// BackoffCap is the maximum backoff duration. Default: 1h.
	BackoffCap time.Duration
	// MaxAttempts is the number of attempts before a job is dead-lettered. Default: 5.
	MaxAttempts int
}

// DefaultConfig returns a Config populated with the spec-mandated defaults.
func DefaultConfig() Config {
	return Config{
		PollInterval: 1 * time.Second,
		Workers:      4,
		JobTimeout:   30 * time.Second,
		LeaseTimeout: 5 * time.Minute,
		BackoffBase:  10 * time.Second,
		BackoffCap:   1 * time.Hour,
		MaxAttempts:  5,
	}
}

// Validate checks the one cross-field invariant: LeaseTimeout must be strictly
// greater than JobTimeout. The composition root calls this and returns the error
// from app.New if invalid, so a bad config fails boot immediately.
func (c Config) Validate() error {
	if c.LeaseTimeout <= c.JobTimeout {
		return fmt.Errorf(
			"scheduler: LeaseTimeout (%v) must be greater than JobTimeout (%v)",
			c.LeaseTimeout, c.JobTimeout,
		)
	}
	return nil
}

// ── Scheduler ─────────────────────────────────────────────────────────────────

// Scheduler is the durable job queue engine. Obtain one via New or NewWithClock;
// the zero value is not valid.
type Scheduler struct {
	st       *store.Store
	bus      events.Bus // reserved for job lifecycle events in a later slice
	cfg      Config
	now      func() time.Time
	logger   *slog.Logger
	handlers map[string]Handler
	mu       sync.RWMutex // guards handlers

	// lifecycle
	cancel   context.CancelFunc
	wg       sync.WaitGroup // tracks running handlers
	sem      chan struct{}  // bounded to cfg.Workers
	stopOnce sync.Once
	started  bool
	startMu  sync.Mutex
}

// New constructs a Scheduler wired with the given store, bus, and config. The
// nil-clock variant defaults to time.Now. Call Start to begin polling.
func New(st *store.Store, bus events.Bus, cfg Config) *Scheduler {
	return NewWithClock(st, bus, cfg, nil)
}

// NewWithClock constructs a Scheduler with an injected clock. When nowFn is nil,
// time.Now is used. All other parameters are identical to New.
func NewWithClock(st *store.Store, bus events.Bus, cfg Config, nowFn func() time.Time) *Scheduler {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Scheduler{
		st:       st,
		bus:      bus,
		cfg:      cfg,
		now:      nowFn,
		logger:   slog.Default(),
		handlers: make(map[string]Handler),
		sem:      make(chan struct{}, cfg.Workers),
	}
}

// Register associates kind with h in the handler registry. Must be called before
// Start; concurrent calls after Start are safe but the handler is only guaranteed
// to be visible on the next tick.
func (s *Scheduler) Register(kind string, h Handler) {
	s.mu.Lock()
	s.handlers[kind] = h
	s.mu.Unlock()
}

// Enqueue inserts a job row and returns the job id. If req.RunAt is zero, the job
// is due immediately (run_at = now). When req.Key is non-empty the insert is
// idempotent: a second Enqueue with the same Key returns the existing id without
// creating a new row.
//
// The Recurrence field is accepted but one-shot here; recurrence columns are
// stored as NULL until the recurrence slice is implemented.
func (s *Scheduler) Enqueue(ctx context.Context, req EnqueueRequest) (string, error) {
	runAt := req.RunAt
	if runAt.IsZero() {
		runAt = s.now()
	}
	runAtStr := runAt.UTC().Format(time.RFC3339)
	nowStr := s.now().UTC().Format(time.RFC3339)

	payload := req.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	// Derive user_id from scope.
	var userID sql.NullString
	if !req.Scope.IsSystem() && req.Scope.UserID() != "" {
		userID = sql.NullString{String: req.Scope.UserID(), Valid: true}
	}

	// Derive key.
	var key sql.NullString
	if req.Key != "" {
		key = sql.NullString{String: req.Key, Valid: true}
	}

	jobID := id.New()

	// INSERT OR IGNORE handles the idempotency-by-key constraint. When key is non-NULL
	// the partial unique index on (key) WHERE key IS NOT NULL fires on conflict and the
	// INSERT is silently ignored (returning RowsAffected=0). When key is NULL, no
	// constraint can match (NULL != NULL in SQL), so every fire-and-forget job is inserted.
	//
	// sqlc cannot model the ON CONFLICT(col) DO NOTHING upsert clause for SQLite, so this
	// uses raw SQL on the single-connection write pool (inherently serialised).
	const insertSQL = `
INSERT OR IGNORE INTO jobs (id, key, kind, payload, user_id, status, run_at, attempt, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'pending', ?, 0, ?, ?)`

	res, err := s.st.Writer().ExecContext(ctx, insertSQL,
		jobID, key, req.Kind, string(payload), userID, runAtStr, nowStr, nowStr,
	)
	if err != nil {
		return "", fmt.Errorf("scheduler: enqueue insert: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("scheduler: enqueue rows affected: %w", err)
	}

	if rows == 0 {
		// ON CONFLICT(key) DO NOTHING fired: the key already exists. Look up the
		// existing id via the generated query.
		if !key.Valid {
			// Should never happen: rows==0 only when key conflicts.
			return "", errors.New("scheduler: enqueue: conflict with no key")
		}
		existingID, err := db.New(s.st.Reader()).GetJobIDByKey(ctx, key)
		if err != nil {
			return "", fmt.Errorf("scheduler: enqueue get existing id by key: %w", err)
		}
		return existingID, nil
	}

	return jobID, nil
}

// Start begins the poll→claim→execute loop. It is safe to call Start only once;
// subsequent calls are no-ops. The provided ctx is the application context; it is
// used as the parent for the poller goroutine and all handler contexts.
func (s *Scheduler) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return nil
	}
	s.started = true

	pollCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	s.wg.Add(1)
	go s.pollLoop(pollCtx)

	return nil
}

// Stop cancels in-flight handlers and waits for the worker pool to drain under
// the provided context's deadline. Any claimed-but-unstarted rows are left in the
// running state (reclaim on boot is a later slice).
//
// Stop is safe to call before, concurrently with, or after Start. It is
// idempotent: only the first call does work.
func (s *Scheduler) Stop(ctx context.Context) error {
	var stopErr error
	s.stopOnce.Do(func() {
		// Read s.cancel and s.started under startMu to synchronise with Start, which
		// writes both under the same lock. This prevents a data race on s.cancel when
		// Start and Stop race. Because Start calls s.wg.Add(1) while holding startMu,
		// reading started == true here also guarantees that the WaitGroup counter is
		// already at least 1, making the subsequent wg.Wait safe.
		s.startMu.Lock()
		cancel := s.cancel
		started := s.started
		s.startMu.Unlock()

		if cancel != nil {
			cancel() // cancels the poll loop and all child handler contexts
		}

		if !started {
			// Start was never called; no poller goroutine was added to the WaitGroup.
			// Calling wg.Wait() with a zero counter while Add might race is unsafe, so
			// skip the drain entirely.
			return
		}

		// Wait for the poller goroutine and all in-flight handlers to finish.
		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			stopErr = fmt.Errorf("scheduler: Stop deadline exceeded: %w", ctx.Err())
		}
	})
	return stopErr
}

// ── Internal poll loop ────────────────────────────────────────────────────────

// pollLoop runs on a dedicated goroutine until ctx is cancelled. On each tick it
// claims up to (Workers - running) due jobs and dispatches each to the worker pool.
func (s *Scheduler) pollLoop(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// claimedRow is the minimal projection returned by the atomic claim query.
type claimedRow struct {
	id      string
	kind    string
	payload string
	userID  sql.NullString
	attempt int64
}

// claimSQL is the atomic UPDATE...WHERE id IN (SELECT...LIMIT)...RETURNING statement.
// sqlc cannot model this compound shape (UPDATE...WHERE...IN(SELECT...LIMIT)...RETURNING),
// so it is executed as raw SQL on the single-connection write pool, which serialises all
// claims automatically.
const claimSQL = `
UPDATE jobs
SET status = 'running', locked_at = ?, attempt = attempt + 1, updated_at = ?
WHERE id IN (
    SELECT id FROM jobs
    WHERE status = 'pending' AND run_at <= ?
    ORDER BY run_at
    LIMIT ?
)
RETURNING id, kind, payload, user_id, attempt`

// tick executes one claim-and-dispatch cycle.
func (s *Scheduler) tick(ctx context.Context) {
	// batch = number of free worker slots. len(s.sem) is an intentionally
	// approximate count: the poller is the sole owner of the semaphore, so this
	// read is effectively single-threaded; at worst it under-claims by one slot on
	// a tick, which is harmless.
	free := s.cfg.Workers - len(s.sem)
	if free <= 0 {
		return // all slots occupied; skip this tick.
	}

	nowStr := s.now().UTC().Format(time.RFC3339)

	rows, err := s.st.Writer().QueryContext(ctx, claimSQL, nowStr, nowStr, nowStr, free)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, sql.ErrConnDone) || isClosedError(err) {
			return // expected during shutdown or when the store is closed
		}
		s.logger.Error("scheduler: claim query failed", "err", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var claimed []claimedRow
	for rows.Next() {
		var r claimedRow
		if err := rows.Scan(&r.id, &r.kind, &r.payload, &r.userID, &r.attempt); err != nil {
			s.logger.Error("scheduler: scan claimed row", "err", err)
			continue
		}
		claimed = append(claimed, r)
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("scheduler: iterate claimed rows", "err", err)
		return
	}

	for _, r := range claimed {
		// Acquire a worker slot. This send cannot block: the poller claimed at most
		// free = Workers - len(s.sem) rows, so there are at least that many open
		// slots in the buffered semaphore channel. The dead-code default branch has
		// been removed to eliminate the stuck-row footgun it created.
		s.sem <- struct{}{}

		row := r // capture for goroutine
		s.wg.Go(func() {
			defer func() { <-s.sem }()
			s.dispatch(ctx, row)
		})
	}
}

// dispatch runs the handler for one claimed job row. It resolves scope, builds
// the Job struct, calls the handler under a JobTimeout-bounded context, and on
// success deletes the row.
func (s *Scheduler) dispatch(ctx context.Context, r claimedRow) {
	// Resolve scope from stored user_id.
	var scope store.Scope
	if r.userID.Valid && r.userID.String != "" {
		scope = store.UserScope(store.Principal{UserID: r.userID.String})
	} else {
		scope = store.SystemScope()
	}

	job := Job{
		ID:      r.id,
		Kind:    r.kind,
		Payload: json.RawMessage(r.payload),
		Attempt: int(r.attempt),
		Scope:   scope,
	}

	// Look up the handler.
	s.mu.RLock()
	h, ok := s.handlers[r.kind]
	s.mu.RUnlock()

	if !ok {
		// No registered handler for this kind. Log loudly at error level; leave the
		// row in running state so it is visible. Do NOT delete it -- that would silently
		// drop a job with no handler. Retry/dead-letter handling is a later slice.
		s.logger.Error("scheduler: no handler registered for job kind; row left running",
			"id", r.id, "kind", r.kind, "attempt", r.attempt)
		return
	}

	// Build a JobTimeout-bounded context derived from the poller context (which is
	// cancelled on Stop). The handler context is cancelled on Stop via the parent
	// chain, so in-flight handlers are cancelled when Stop is called.
	jobCtx, cancel := context.WithTimeout(ctx, s.cfg.JobTimeout)
	defer cancel()

	// Invoke the handler under a deferred recover so that a panicking handler does
	// not propagate out of the worker goroutine and crash the process. On panic the
	// row is left in "running" state, matching the semantics of a handler returning
	// an error (retry/dead-letter is a later slice).
	var handlerErr error
	panicked := false
	func() {
		defer func() {
			if p := recover(); p != nil {
				panicked = true
				s.logger.Error("scheduler: handler panicked; job left running",
					"id", r.id, "kind", r.kind, "attempt", r.attempt,
					"panic", p,
					"stack", string(debug.Stack()),
				)
			}
		}()
		handlerErr = h(jobCtx, job)
	}()

	if panicked {
		// Panic was recovered; treat the same as a handler error -- leave the row in
		// running state. Do not fall through to the success delete path below.
		return
	}

	if handlerErr != nil {
		// Handler returned an error. Leave the row in running state.
		// Retry/backoff/dead-letter is a later slice.
		if !errors.Is(handlerErr, context.Canceled) && !errors.Is(handlerErr, context.DeadlineExceeded) {
			s.logger.Error("scheduler: handler failed",
				"id", r.id, "kind", r.kind, "attempt", r.attempt, "err", handlerErr)
		}
		return
	}

	// One-shot: handler succeeded -- delete the row.
	delCtx, delCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer delCancel()
	if err := db.New(s.st.Writer()).DeleteJob(delCtx, r.id); err != nil {
		s.logger.Error("scheduler: delete job row after success",
			"id", r.id, "kind", r.kind, "err", err)
	}
}

// isClosedError reports whether err indicates that the database pool was closed
// (e.g. during application shutdown). The standard library's errDBClosed is
// unexported, so we check the message string as a last resort.
func isClosedError(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == "sql: database is closed"
}
