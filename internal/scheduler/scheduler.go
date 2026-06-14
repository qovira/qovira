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
	"math/rand/v2"
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
// JobTimeout deadline. Returning nil deletes the row (one-shot success). Returning
// a non-nil error triggers retry-with-backoff: the row is re-armed as pending with
// a jittered exponential delay. At MaxAttempts the job is dead-lettered (status set
// to 'failed', row kept, job.failed event published for user-scoped jobs). Wrapping
// the error with [Permanent] skips any remaining retries and dead-letters immediately.
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
	bus      events.Bus // publishes job.failed to the owning user's bus when a user-scoped job dead-letters; system-scoped failures are logged only
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
// success deletes the row. On failure it either re-arms the row with jittered
// backoff (transient) or dead-letters it (MaxAttempts reached or Permanent error).
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
		// No registered handler for this kind. Log loudly and flow through the
		// failure path as a transient error: retry with backoff, then dead-letter
		// at MaxAttempts. This prevents the old "row left running" footgun.
		s.logger.Error("scheduler: no handler registered for job kind; treating as transient failure",
			"id", r.id, "kind", r.kind, "attempt", r.attempt)
		s.handleFailure(r, scope, fmt.Errorf("no handler registered for kind %q", r.kind))
		return
	}

	// Build a JobTimeout-bounded context derived from the poller context (which is
	// cancelled on Stop). The handler context is cancelled on Stop via the parent
	// chain, so in-flight handlers are cancelled when Stop is called.
	jobCtx, cancel := context.WithTimeout(ctx, s.cfg.JobTimeout)
	defer cancel()

	// Invoke the handler under a deferred recover so that a panicking handler does
	// not propagate out of the worker goroutine and crash the process. A panic is
	// converted into an error and flows through the same retry/dead-letter path as
	// any transient handler error.
	var handlerErr error
	func() {
		defer func() {
			if p := recover(); p != nil {
				handlerErr = fmt.Errorf("scheduler: handler panicked: %v", p)
				s.logger.Error("scheduler: handler panicked; flowing through retry/dead-letter path",
					"id", r.id, "kind", r.kind, "attempt", r.attempt,
					"panic", p,
					"stack", string(debug.Stack()),
				)
			}
		}()
		handlerErr = h(jobCtx, job)
	}()

	if handlerErr != nil {
		s.handleFailure(r, scope, handlerErr)
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

// handleFailure implements the retry/dead-letter decision for a failed one-shot job.
//
// Decision logic:
//   - Permanent error or attempt >= MaxAttempts → dead-letter (status=failed, row kept).
//   - context.Canceled or context.DeadlineExceeded (JobTimeout) → transient retry.
//   - Any other error → transient retry if attempt < MaxAttempts, else dead-letter.
func (s *Scheduler) handleFailure(r claimedRow, scope store.Scope, handlerErr error) {
	attempt := int(r.attempt)

	// Determine whether this is a permanent (non-retryable) failure.
	var permErr *permanentError
	isPermanent := errors.As(handlerErr, &permErr)

	// Determine whether to dead-letter.
	deadLetter := isPermanent || attempt >= s.cfg.MaxAttempts

	if deadLetter {
		s.deadLetter(r, scope, attempt, handlerErr)
		return
	}

	// Transient failure: log (unless it's a context cancellation) and re-arm.
	if !errors.Is(handlerErr, context.Canceled) && !errors.Is(handlerErr, context.DeadlineExceeded) {
		s.logger.Error("scheduler: handler failed; will retry",
			"id", r.id, "kind", r.kind, "attempt", attempt,
			"maxAttempts", s.cfg.MaxAttempts, "err", handlerErr)
	} else {
		s.logger.Info("scheduler: handler context cancelled/timed out; will retry",
			"id", r.id, "kind", r.kind, "attempt", attempt)
	}

	backoff := backoffDuration(s.cfg, attempt)
	runAt := s.now().Add(backoff)
	nowStr := s.now().UTC().Format(time.RFC3339)
	runAtStr := runAt.UTC().Format(time.RFC3339)

	writeCtx, writeCancel := context.WithTimeout(context.Background(), failureWriteTimeout)
	defer writeCancel()

	if _, err := db.New(s.st.Writer()).RetryJob(writeCtx, db.RetryJobParams{
		RunAt:     runAtStr,
		UpdatedAt: nowStr,
		ID:        r.id,
	}); err != nil {
		s.logger.Error("scheduler: retry job update failed",
			"id", r.id, "kind", r.kind, "attempt", attempt, "err", err)
	}
}

// deadLetter marks the job as permanently failed (status=failed, last_error set, row kept)
// and publishes a job.failed event on the owning user's bus (or logs-only for system jobs).
func (s *Scheduler) deadLetter(r claimedRow, scope store.Scope, attempt int, handlerErr error) {
	errStr := handlerErr.Error()

	s.logger.Error("scheduler: job dead-lettered",
		"id", r.id, "kind", r.kind, "attempt", attempt, "err", handlerErr)

	writeCtx, writeCancel := context.WithTimeout(context.Background(), failureWriteTimeout)
	defer writeCancel()

	nowStr := s.now().UTC().Format(time.RFC3339)
	if _, err := db.New(s.st.Writer()).DeadLetterJob(writeCtx, db.DeadLetterJobParams{
		LastError: sql.NullString{String: errStr, Valid: true},
		UpdatedAt: nowStr,
		ID:        r.id,
	}); err != nil {
		s.logger.Error("scheduler: dead-letter job update failed",
			"id", r.id, "kind", r.kind, "attempt", attempt, "err", err)
		return
	}

	// Publish job.failed on the owning user's bus. System-scoped jobs have no bus
	// channel, so they are logged only (the bus is strictly per-user).
	if !scope.IsSystem() && scope.UserID() != "" {
		s.bus.Publish(scope.UserID(), events.Event{
			Type: jobFailedEventType,
			Data: JobFailedEvent{
				JobID:     r.id,
				Kind:      r.kind,
				Attempt:   attempt,
				LastError: errStr,
			},
		})
	}
}

// ── Permanent error ───────────────────────────────────────────────────────────

// permanentError wraps an inner error and signals to the scheduler that the job
// must be dead-lettered immediately without any retry attempt. The inner error is
// preserved via Unwrap so errors.Is and errors.As can inspect the chain.
type permanentError struct {
	inner error
}

func (e *permanentError) Error() string { return "scheduler: permanent: " + e.inner.Error() }
func (e *permanentError) Unwrap() error { return e.inner }

// Permanent wraps err to signal that the job must be dead-lettered immediately
// on the first failure without any retry, regardless of how many attempts remain.
// Use it in a handler when the error is definitively unrecoverable:
//
//	return scheduler.Permanent(fmt.Errorf("reminder %s gone: %w", id, ErrNotFound))
//
// The wrapped error is accessible via errors.Unwrap / errors.Is / errors.As.
func Permanent(err error) error { return &permanentError{inner: err} }

// ── job.failed event ──────────────────────────────────────────────────────────

// JobFailedEvent is the payload published on the owning user's bus when a one-shot
// job is dead-lettered. It is a fat event: it carries enough information to render
// a failure surface without a re-fetch.
type JobFailedEvent struct {
	// JobID is the ULID string identifier of the dead-lettered job.
	JobID string `json:"jobId"`
	// Kind is the dispatch selector of the dead-lettered job.
	Kind string `json:"kind"`
	// Attempt is the 1-based attempt number that triggered dead-lettering.
	Attempt int `json:"attempt"`
	// LastError is the string representation of the error that caused dead-lettering.
	LastError string `json:"lastError"`
}

// jobFailedEventType is the event type string for the job.failed bus event.
const jobFailedEventType = "job.failed"

// failureWriteTimeout is the deadline applied to failure-path DB writes (RetryJob,
// DeadLetterJob). These writes use context.Background() — not the handler context —
// because they must survive shutdown cancellation: a failing job's final state must
// be persisted even when Stop cancels the poller context.
const failureWriteTimeout = 5 * time.Second

// ── Backoff ───────────────────────────────────────────────────────────────────

// backoffDuration returns a jittered backoff duration for the given attempt using
// full-jitter capped exponential backoff:
//
//	random(0, min(BackoffCap, BackoffBase * 2^(attempt-1)))
//
// attempt is 1-based at dispatch. math/rand/v2 is used deliberately: this is
// scheduling spread, not a security secret, so crypto/rand would be unnecessary.
func backoffDuration(cfg Config, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Compute cap: BackoffBase * 2^(attempt-1), guarded against overflow.
	var ceiling time.Duration
	shift := attempt - 1
	if shift >= 62 || cfg.BackoffBase > cfg.BackoffCap>>(shift) {
		// Would overflow or exceed cap — use BackoffCap directly.
		ceiling = cfg.BackoffCap
	} else {
		ceiling = min(cfg.BackoffBase<<shift, cfg.BackoffCap) // BackoffBase * 2^(attempt-1), capped
	}
	if ceiling <= 0 {
		return 0
	}
	// Full jitter: uniform random in [0, ceiling]. math/rand is intentional here:
	// this is scheduling spread, not a secret. See issue spec §Backoff.
	return time.Duration(rand.Int64N(int64(ceiling) + 1)) //nolint:gosec // scheduling jitter, not a secret
}

// ── Sentinel errors ───────────────────────────────────────────────────────────

// ErrJobNotFound is returned by Cancel and Reschedule when the given job id does
// not exist in the jobs table. Use errors.Is to detect it.
var ErrJobNotFound = errors.New("scheduler: job not found")

// ErrJobRunning is returned by Reschedule when the target job is currently being
// executed by a handler goroutine. Cancel does NOT return this error — it deletes
// the row even for a running job (the in-flight handler completes once, the
// post-success delete becomes a harmless no-op, and the job is never re-leased).
// Use errors.Is to detect it.
var ErrJobRunning = errors.New("scheduler: job is currently running")

// ── Cancel / Reschedule ───────────────────────────────────────────────────────

// Cancel removes the job with the given id so it never fires again.
//
// Behaviour by row state:
//   - pending: the row is deleted immediately. The claim loop never sees it again
//     because claim only selects status='pending' rows.
//   - running: the row is deleted, but the in-flight handler is NOT interrupted.
//     The handler will complete exactly once; its post-success DELETE becomes a
//     harmless no-op (0 rows affected). The job can never be re-leased because the
//     row is gone. This is safe and race-clean under -race.
//   - failed: the dead-lettered row is deleted (operator cleanup). Cancel treats a
//     dead-lettered job the same as any other status — it deletes by id regardless.
//     This lets operators clear dead-lettered jobs rather than leaving them indefinitely.
//   - absent: returns ErrJobNotFound.
//
// Cancel uses a transaction on the single-connection write pool so that the
// status read and the delete are one atomic unit, serialised against the claim loop.
func (s *Scheduler) Cancel(ctx context.Context, jobID string) error {
	tx, err := s.st.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("scheduler: cancel begin tx: %w", err)
	}
	// Always rollback on early return; a committed tx makes Rollback a no-op.
	defer func() { _ = tx.Rollback() }()

	q := db.New(tx)

	// Read current status to determine whether the row exists at all.
	_, err = q.GetJobStatus(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("scheduler: cancel %q: %w", jobID, ErrJobNotFound)
		}
		return fmt.Errorf("scheduler: cancel get status %q: %w", jobID, err)
	}

	// Delete regardless of status (pending or running today). For a running row:
	// the in-flight goroutine's post-success delete becomes a no-op; no double-run
	// is possible because the row is gone and claim only selects pending rows.
	if err := q.DeleteJob(ctx, jobID); err != nil {
		return fmt.Errorf("scheduler: cancel delete %q: %w", jobID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("scheduler: cancel commit %q: %w", jobID, err)
	}
	return nil
}

// Reschedule moves a pending job's earliest fire time to runAt.
//
// Behaviour by row state:
//   - pending: run_at and updated_at are updated. The job will fire at the new
//     time, not the old one.
//   - running: returns ErrJobRunning. The row is NOT modified. Changing run_at or
//     flipping a running row back to pending while a handler goroutine holds the
//     lease could cause the job to be re-claimed and executed a second time — a
//     double-run that the scheduler invariant forbids. The caller should retry after
//     the job settles (row deleted on success, or left running on error).
//   - absent: returns ErrJobNotFound.
//
// Reschedule uses a transaction on the single-connection write pool so that the
// status read and the update are one atomic unit, serialised against the claim loop.
func (s *Scheduler) Reschedule(ctx context.Context, jobID string, runAt time.Time) error {
	tx, err := s.st.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("scheduler: reschedule begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := db.New(tx)

	// Read current status first so we can distinguish not-found from running.
	status, err := q.GetJobStatus(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("scheduler: reschedule %q: %w", jobID, ErrJobNotFound)
		}
		return fmt.Errorf("scheduler: reschedule get status %q: %w", jobID, err)
	}

	if status == "running" {
		// Do not touch the row — flipping run_at on a running row risks a double-run
		// when the handler finishes and the claim loop re-sees it.
		return fmt.Errorf("scheduler: reschedule %q: %w", jobID, ErrJobRunning)
	}

	nowStr := s.now().UTC().Format(time.RFC3339)
	runAtStr := runAt.UTC().Format(time.RFC3339)

	rows, err := q.RescheduleJob(ctx, db.RescheduleJobParams{
		RunAt:     runAtStr,
		UpdatedAt: nowStr,
		ID:        jobID,
	})
	if err != nil {
		return fmt.Errorf("scheduler: reschedule update %q: %w", jobID, err)
	}
	if rows == 0 {
		// Status was not 'pending' (e.g. it transitioned between our SELECT and UPDATE,
		// which cannot happen on a MaxOpenConns=1 write pool inside a tx, but be
		// defensive). Treat as not-found to avoid a silent no-op.
		return fmt.Errorf("scheduler: reschedule %q: %w", jobID, ErrJobNotFound)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("scheduler: reschedule commit %q: %w", jobID, err)
	}
	return nil
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
