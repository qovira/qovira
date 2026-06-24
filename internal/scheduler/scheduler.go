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

	rrule "github.com/teambition/rrule-go"

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
	// Recurrence controls how the job repeats after a successful execution. When
	// nil or zero-valued the job is one-shot (deleted on success). See the
	// Recurrence type for field semantics and the "exactly one of" constraint.
	Recurrence *Recurrence
}

// Recurrence controls how a job repeats after a successful execution (or after
// exhausting MaxAttempts without a Permanent error). Exactly one of (RRULE+TZ) or
// Every must be specified; both and neither are both accepted as "one-shot".
//
//   - RRULE+TZ: wall-clock recurrence (iCalendar RFC 5545 RRULE string + IANA
//     timezone). "Every day at 8am America/New_York" stays at 8am local time
//     across DST boundaries. The next instant is computed in the stored TZ and
//     persisted back as UTC.
//   - Every: interval-based recurrence (e.g. 6*time.Hour for "every 6h"). Phase
//     drift is acceptable for housekeeping jobs; next = now + Every.
//
// Both RRULE+TZ and Every set simultaneously is invalid and Enqueue returns an
// error. RRULE without TZ is also invalid (a wall-clock rule needs a timezone to
// resolve DST). If the RRULE is finite (COUNT/UNTIL exhausted and no further
// occurrence), the series is completed: the row is deleted and the completion is
// logged.
type Recurrence struct {
	// RRULE is an iCalendar RRULE string (e.g. "FREQ=DAILY").
	RRULE string
	// TZ is the IANA timezone for the recurrence (e.g. "America/New_York").
	TZ string
	// Every is the interval between recurrences (alternative to RRULE).
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

// Validate checks the config invariants. The cross-field invariant is that
// LeaseTimeout must be strictly greater than JobTimeout. MaxAttempts must also be
// at least 1, otherwise the dispatch-time attempt-ceiling guard would dead-letter
// every job before its handler ever runs. The composition root calls this and
// returns the error from app.New if invalid, so a bad config fails boot immediately.
func (c Config) Validate() error {
	if c.LeaseTimeout <= c.JobTimeout {
		return fmt.Errorf(
			"scheduler: LeaseTimeout (%v) must be greater than JobTimeout (%v)",
			c.LeaseTimeout, c.JobTimeout,
		)
	}
	if c.MaxAttempts < 1 {
		return fmt.Errorf("scheduler: MaxAttempts (%d) must be at least 1", c.MaxAttempts)
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
// When req.Recurrence is non-nil, the recurrence columns (rrule, tz,
// interval_secs) are persisted. Enqueue validates the "exactly one of"
// constraint: specifying both (RRULE+TZ) and Every is an error, as is RRULE
// without TZ. Both empty (or nil Recurrence) is one-shot.
func (s *Scheduler) Enqueue(ctx context.Context, req EnqueueRequest) (string, error) {
	// Validate and derive recurrence columns before any DB work.
	rruleCol, tzCol, intervalSecsCol, err := buildRecurrenceColumns(req.Recurrence)
	if err != nil {
		return "", fmt.Errorf("scheduler: enqueue: %w", err)
	}

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
	// uses raw SQL on the single-connection write pool (inherently serialised). Recurrence
	// columns (rrule, tz, interval_secs) are included; NULL for one-shot jobs.
	const insertSQL = `
INSERT OR IGNORE INTO jobs (id, key, kind, payload, user_id, status, run_at, attempt, rrule, tz, interval_secs, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'pending', ?, 0, ?, ?, ?, ?, ?)`

	res, err := s.st.Writer().ExecContext(ctx, insertSQL,
		jobID, key, req.Kind, string(payload), userID, runAtStr,
		rruleCol, tzCol, intervalSecsCol,
		nowStr, nowStr,
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

// buildRecurrenceColumns validates the Recurrence and returns the (rrule, tz,
// interval_secs) column values to persist. Returns NULL NullStrings/NullInt64
// for a one-shot job (nil or zero Recurrence). Returns an error if both kinds
// are specified, or if RRULE is set without TZ.
func buildRecurrenceColumns(r *Recurrence) (rruleCol, tzCol sql.NullString, intervalCol sql.NullInt64, err error) {
	if r == nil {
		return // all NULL — one-shot
	}

	hasRRule := r.RRULE != ""
	hasEvery := r.Every > 0

	if hasRRule && hasEvery {
		err = errors.New("recurrence: cannot specify both RRULE+TZ and Every; choose one")
		return
	}
	if hasRRule && r.TZ == "" {
		err = errors.New("recurrence: RRULE requires TZ (an IANA timezone name)")
		return
	}

	if hasRRule {
		rruleCol = sql.NullString{String: r.RRULE, Valid: true}
		tzCol = sql.NullString{String: r.TZ, Valid: true}
		return
	}

	if hasEvery {
		intervalCol = sql.NullInt64{Int64: int64(r.Every.Seconds()), Valid: true}
		return
	}

	// Neither set — one-shot (all NULL).
	return
}

// Start begins the poll→claim→execute loop. It is safe to call Start only once;
// subsequent calls are no-ops. The provided ctx is the application context; it is
// used as the parent for the poller goroutine and all handler contexts.
//
// A one-shot reclaim sweep runs before the poll loop begins, recovering any rows
// that were left in running state by a prior process crash.
func (s *Scheduler) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return nil
	}
	s.started = true

	// Boot sweep: reclaim running rows older than LeaseTimeout before the loop starts.
	// Uses context.Background() so it is not affected by a concurrent early Stop.
	s.reclaim(context.Background())

	pollCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	s.wg.Go(func() { s.pollLoop(pollCtx) })

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
		// Start and Stop race. Because Start calls s.wg.Go(...) while holding startMu,
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
// The goroutine is launched via s.wg.Go so WaitGroup bookkeeping is handled there.
func (s *Scheduler) pollLoop(ctx context.Context) {
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

// claimedRow is the minimal projection returned by the atomic claim query. It
// includes the recurrence columns (rrule, tz, interval_secs) and the current
// run_at so the success and exhaustion paths can compute the next occurrence.
type claimedRow struct {
	id           string
	kind         string
	payload      string
	userID       sql.NullString
	attempt      int64
	runAt        string         // RFC3339 UTC — the occurrence that just fired
	rrule        sql.NullString // RRULE string for wall-clock recurrence
	tz           sql.NullString // IANA timezone for the RRULE
	intervalSecs sql.NullInt64  // seconds between occurrences for interval-based recurrence
}

// scope resolves the store.Scope for the row: system scope when user_id is NULL/empty,
// user scope otherwise. This mapping is used in both dispatch and advanceRecurring.
func (r claimedRow) scope() store.Scope {
	if r.userID.Valid && r.userID.String != "" {
		return store.UserScope(store.Principal{UserID: r.userID.String})
	}
	return store.SystemScope()
}

// isRecurring reports whether the row carries a recurrence specification.
func (r claimedRow) isRecurring() bool {
	return r.rrule.Valid || r.intervalSecs.Valid
}

// nextRunAt computes the next occurrence for a recurring job. The returned bool
// is false when the RRULE is finite and has no further occurrence (series complete
// — caller should delete the row). Interval recurrence always returns true.
//
// For RRULE recurrence: the rule is re-anchored at the current (just-fired)
// run_at converted into the stored TZ. This preserves the wall-clock phase (e.g.
// always 8am local) and avoids drift. The next occurrence is the first instant
// strictly after now (not after run_at), which makes downtime catch-up correct:
// missed occurrences are skipped and the series jumps to the next future instant.
//
// For interval recurrence: next = now + interval. Phase drift is fine for
// housekeeping jobs.
func (r claimedRow) nextRunAt(now time.Time) (time.Time, bool, error) {
	if r.intervalSecs.Valid {
		interval := time.Duration(r.intervalSecs.Int64) * time.Second
		return now.Add(interval), true, nil
	}

	// RRULE path: parse the current (on-phase) run_at and convert to the stored TZ
	// so that the Dtstart anchor preserves the wall-clock time-of-day (e.g. always 8am).
	current, err := time.Parse(time.RFC3339, r.runAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse run_at %q: %w", r.runAt, err)
	}
	return nextRRuleOccurrence(r.rrule.String, r.tz.String, current, now)
}

// nextRRuleOccurrence computes the next RRULE occurrence strictly after after,
// anchoring the rule at anchor (in the named tz). It returns (next, true, nil) on
// success, (zero, false, nil) when the rule is finite and exhausted, and
// (zero, false, err) on parse/build errors. Caller wraps the error with its own
// context message.
func nextRRuleOccurrence(rruleStr, tz string, anchor, after time.Time) (time.Time, bool, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("load tz %q: %w", tz, err)
	}

	ropt, err := rrule.StrToROptionInLocation(rruleStr, loc)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse rrule %q: %w", rruleStr, err)
	}
	ropt.Dtstart = anchor.In(loc)

	rule, err := rrule.NewRRule(*ropt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("build rrule %q: %w", rruleStr, err)
	}

	next := rule.After(after, false) // strictly after after — skips missed backlog
	if next.IsZero() {
		return time.Time{}, false, nil
	}
	return next, true, nil
}

// claimSQL is the atomic UPDATE...WHERE id IN (SELECT...LIMIT)...RETURNING statement.
// sqlc cannot model this compound shape (UPDATE...WHERE...IN(SELECT...LIMIT)...RETURNING),
// so it is executed as raw SQL on the single-connection write pool, which serialises all
// claims automatically. The RETURNING clause now includes recurrence columns and run_at
// so the success/exhaustion paths can compute the next occurrence without a second query.
const claimSQL = `
UPDATE jobs
SET status = 'running', locked_at = ?, attempt = attempt + 1, updated_at = ?
WHERE id IN (
    SELECT id FROM jobs
    WHERE status = 'pending' AND run_at <= ?
    ORDER BY run_at
    LIMIT ?
)
RETURNING id, kind, payload, user_id, attempt, run_at, rrule, tz, interval_secs`

// tick executes one reclaim-then-claim-and-dispatch cycle.
func (s *Scheduler) tick(ctx context.Context) {
	// Periodic reclaim: return stale running rows to pending before claiming new ones.
	// This handles wedged-but-alive workers that ignored cancellation, as well as any
	// rows missed by the boot sweep (e.g. inserted between boot and this tick).
	s.reclaim(ctx)

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
		if err := rows.Scan(
			&r.id, &r.kind, &r.payload, &r.userID, &r.attempt,
			&r.runAt, &r.rrule, &r.tz, &r.intervalSecs,
		); err != nil {
			s.logger.Error("scheduler: scan claimed row; re-arming row to pending", "id", r.id, "err", err)
			// The claim UPDATE already moved this row to status='running'. Re-arm it
			// back to 'pending' now so it can be re-claimed on the next tick rather
			// than sitting in 'running' until the LeaseTimeout sweep catches it.
			// We can only re-arm by id; if the id column itself failed to scan
			// (r.id is empty), the periodic reclaim will recover the row in due course.
			if r.id != "" {
				s.rearmRow(r.id)
			}
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

// reclaim sweeps running rows whose locked_at is older than LeaseTimeout and flips
// them back to pending so they can be re-leased. It does NOT reset attempt: the retry
// ceiling must still apply to reclaimed rows. It is called both on boot (boot sweep)
// and on each poll tick (periodic sweep).
func (s *Scheduler) reclaim(ctx context.Context) {
	threshold := s.now().Add(-s.cfg.LeaseTimeout).UTC().Format(time.RFC3339)
	nowStr := s.now().UTC().Format(time.RFC3339)

	n, err := db.New(s.st.Writer()).ReclaimStaleJobs(ctx, db.ReclaimStaleJobsParams{
		UpdatedAt: nowStr,
		Threshold: sql.NullString{String: threshold, Valid: true},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, sql.ErrConnDone) || isClosedError(err) {
			return // expected during shutdown
		}
		s.logger.Error("scheduler: reclaim stale jobs failed", "err", err)
		return
	}
	if n > 0 {
		s.logger.Info("scheduler: reclaimed stale running jobs", "count", n)
	}
}

// rearmSQL flips a single row from status='running' back to status='pending' and
// clears locked_at. This is used in the scan-failure recovery path of tick() to
// proactively return a stranded row to the claim queue. attempt is intentionally
// NOT decremented: the row consumed a claim slot, so the attempt count is correct.
// The WHERE status='running' guard prevents a double-update if reclaim already fired.
const rearmSQL = `
UPDATE jobs SET status = 'pending', locked_at = NULL, updated_at = ?
WHERE id = ? AND status = 'running'`

// rearmRow proactively returns a row to 'pending' after a scan error in tick().
// It exists to avoid leaving the row stranded in 'running' until the next
// LeaseTimeout-triggered reclaim sweep. Uses context.Background() (not the poll
// context) so the write survives Stop cancellation — same rationale as the failure
// writes in deadLetter and handleFailure. A best-effort write: errors are logged
// but not propagated (the periodic reclaim is the fallback).
func (s *Scheduler) rearmRow(id string) {
	nowStr := s.now().UTC().Format(time.RFC3339)
	rearmCtx, rearmCancel := context.WithTimeout(context.Background(), failureWriteTimeout)
	defer rearmCancel()
	if _, err := s.st.Writer().ExecContext(rearmCtx, rearmSQL, nowStr, id); err != nil {
		s.logger.Error("scheduler: rearm row after scan error failed", "id", id, "err", err)
	}
}

// dispatch runs the handler for one claimed job row. It resolves scope, builds
// the Job struct, calls the handler under a JobTimeout-bounded context, and on
// success deletes the row (one-shot) or advances/ends the series (recurring job:
// self-reschedules to the next occurrence, or deletes if the RRULE is exhausted,
// or dead-letters on a compute error). On failure it either re-arms the row with
// jittered backoff (transient) or dead-letters it (MaxAttempts reached or Permanent error).
func (s *Scheduler) dispatch(ctx context.Context, r claimedRow) {
	scope := r.scope()

	job := Job{
		ID:      r.id,
		Kind:    r.kind,
		Payload: json.RawMessage(r.payload),
		Attempt: int(r.attempt),
		Scope:   scope,
	}

	// Dispatch-time attempt-ceiling guard: if attempt > MaxAttempts, the job was
	// reclaimed from a prior crash-loop (attempt was already at MaxAttempts, reclaim
	// preserved it, and the claim incremented it to MaxAttempts+1). Dead-letter
	// immediately without running the handler — this prevents reclaim-loop poisoning.
	// Normal dead-lettering via handleFailure sets attempt == MaxAttempts at failure;
	// this guard only fires at MaxAttempts+1 or beyond, so it does not interfere with
	// normal retries (which never reach MaxAttempts+1).
	//
	// Deliberate: this terminates even recurring jobs (no series advance). A job that
	// crash-loops the process past MaxAttempts+1 is catastrophic; ending it is safer
	// than advancing it forever. This is intentionally distinct from the normal
	// exhaustion-advances-series rule in handleFailure.
	if int(r.attempt) > s.cfg.MaxAttempts {
		s.deadLetter(r, scope, int(r.attempt),
			errors.New("exceeded max attempts after repeated reclaim"))
		return
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

	// Handler succeeded.
	if r.isRecurring() {
		s.advanceRecurring(r)
		return
	}

	// One-shot: delete the row.
	delCtx, delCancel := context.WithTimeout(context.Background(), failureWriteTimeout)
	defer delCancel()
	if err := db.New(s.st.Writer()).DeleteJob(delCtx, r.id); err != nil {
		s.logger.Error("scheduler: delete job row after success",
			"id", r.id, "kind", r.kind, "err", err)
	}
}

// advanceRecurring computes the next occurrence for a recurring job that just
// succeeded and updates the row in place (status=pending, run_at=next, attempt=0,
// locked_at=NULL). Two distinct outcomes are handled:
//   - Compute error (bad TZ / unparseable RRULE / unparseable run_at): the row is
//     dead-lettered (status=failed, last_error set, job.failed emitted). Deleting the
//     row on a compute error would silently destroy it; leaving it running would cause
//     the stale-lease reclaim sweep to re-run the handler side-effect every LeaseTimeout.
//     Dead-lettering stops the series terminally while preserving the row for inspection.
//   - Finite RRULE genuinely exhausted (err == nil && !hasNext): the row is deleted
//     (series complete) and the completion is logged.
func (s *Scheduler) advanceRecurring(r claimedRow) {
	scope := r.scope()

	now := s.now()
	next, hasNext, err := r.nextRunAt(now)
	if err != nil {
		// Compute error: unrecoverable misconfiguration (bad TZ / unparseable RRULE).
		// Dead-letter the row so the series stops cleanly and the error is recorded.
		s.logger.Error("scheduler: recurring job: compute error advancing series; dead-lettering row",
			"id", r.id, "kind", r.kind, "err", err)
		s.deadLetter(r, scope, int(r.attempt), fmt.Errorf("recurring advance compute error: %w", err))
		return
	}

	writeCtx, writeCancel := context.WithTimeout(context.Background(), failureWriteTimeout)
	defer writeCancel()

	if !hasNext {
		// Finite RRULE exhausted (COUNT/UNTIL) — delete the row (series complete).
		s.logger.Info("scheduler: recurring job series complete (RRULE COUNT/UNTIL exhausted); deleting row",
			"id", r.id, "kind", r.kind)
		if err := db.New(s.st.Writer()).DeleteJob(writeCtx, r.id); err != nil {
			s.logger.Error("scheduler: delete series-complete recurring job row",
				"id", r.id, "kind", r.kind, "err", err)
		}
		return
	}

	nowStr := now.UTC().Format(time.RFC3339)
	nextStr := next.UTC().Format(time.RFC3339)

	if _, err := db.New(s.st.Writer()).AdvanceRecurringJob(writeCtx, db.AdvanceRecurringJobParams{
		RunAt:     nextStr,
		UpdatedAt: nowStr,
		ID:        r.id,
	}); err != nil {
		s.logger.Error("scheduler: advance recurring job failed",
			"id", r.id, "kind", r.kind, "nextRunAt", nextStr, "err", err)
	}
}

// handleFailure implements the retry/dead-letter decision for a failed job.
//
// Decision logic:
//   - Permanent error → dead-letter unconditionally (status=failed, row kept for
//     one-shot; series ends for recurring — no advance).
//   - attempt >= MaxAttempts AND recurring (non-Permanent) → emit job.failed, then
//     advance the series to the next occurrence (attempt reset to 0, status=pending).
//     The series survives exhaustion; only Permanent ends it.
//   - attempt >= MaxAttempts AND one-shot (non-Permanent) → dead-letter (status=failed).
//   - context.Canceled or context.DeadlineExceeded (JobTimeout) → transient retry.
//   - Any other error below MaxAttempts → transient retry with backoff (same occurrence).
func (s *Scheduler) handleFailure(r claimedRow, scope store.Scope, handlerErr error) {
	attempt := int(r.attempt)

	// Determine whether this is a permanent (non-retryable) failure.
	var permErr *permanentError
	isPermanent := errors.As(handlerErr, &permErr)

	if isPermanent {
		// Permanent errors always dead-letter, both one-shot and recurring.
		s.deadLetter(r, scope, attempt, handlerErr)
		return
	}

	if attempt >= s.cfg.MaxAttempts {
		if r.isRecurring() {
			// Recurring exhaustion: emit job.failed but advance the series instead of
			// dead-lettering. The series survives; only Permanent ends it terminally.
			s.exhaustRecurring(r, scope, attempt, handlerErr)
		} else {
			// One-shot exhaustion: dead-letter terminally.
			s.deadLetter(r, scope, attempt, handlerErr)
		}
		return
	}

	// Transient failure below MaxAttempts: log (unless it's a context cancellation) and re-arm.
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

// publishJobFailed publishes the job.failed event on the owning user's bus when
// the job is user-scoped. System-scoped jobs have no per-user bus channel and are
// logged only; this function is a no-op for them.
func (s *Scheduler) publishJobFailed(scope store.Scope, r claimedRow, attempt int, errStr string) {
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

// exhaustRecurring handles the case where a recurring job exhausts MaxAttempts
// without a Permanent error. It advances the series to the next occurrence so the
// series survives, then (if the advance write confirmed a row was updated) emits
// job.failed to surface the failure. This is distinct from Permanent errors, which
// end the series. Publishing after advance ensures we do not fire job.failed when a
// concurrent reclaim already flipped the row to 'pending' before the advance write.
func (s *Scheduler) exhaustRecurring(r claimedRow, scope store.Scope, attempt int, handlerErr error) {
	errStr := handlerErr.Error()

	s.logger.Error("scheduler: recurring job exhausted MaxAttempts; advancing series and emitting job.failed if advanced",
		"id", r.id, "kind", r.kind, "attempt", attempt, "err", handlerErr)

	// Advance the series first. advanceRecurring internally calls AdvanceRecurringJob
	// whose WHERE status='running' guard confirms the write succeeded. We need the
	// row count to decide whether to publish, so we replicate the advance logic here
	// to capture it, rather than calling advanceRecurring (which doesn't surface the count).
	now := s.now()
	next, hasNext, computeErr := r.nextRunAt(now)
	if computeErr != nil {
		// Compute error path: dead-letter the row (same as advanceRecurring).
		s.logger.Error("scheduler: recurring job: compute error advancing series; dead-lettering row",
			"id", r.id, "kind", r.kind, "err", computeErr)
		s.deadLetter(r, scope, attempt, fmt.Errorf("recurring advance compute error: %w", computeErr))
		return
	}

	writeCtx, writeCancel := context.WithTimeout(context.Background(), failureWriteTimeout)
	defer writeCancel()

	if !hasNext {
		// Finite RRULE exhausted — delete the row and skip job.failed (series is done).
		s.logger.Info("scheduler: recurring job series complete (RRULE COUNT/UNTIL exhausted); deleting row",
			"id", r.id, "kind", r.kind)
		if err := db.New(s.st.Writer()).DeleteJob(writeCtx, r.id); err != nil {
			s.logger.Error("scheduler: delete series-complete recurring job row",
				"id", r.id, "kind", r.kind, "err", err)
		}
		return
	}

	nowStr := now.UTC().Format(time.RFC3339)
	nextStr := next.UTC().Format(time.RFC3339)

	n, err := db.New(s.st.Writer()).AdvanceRecurringJob(writeCtx, db.AdvanceRecurringJobParams{
		RunAt:     nextStr,
		UpdatedAt: nowStr,
		ID:        r.id,
	})
	if err != nil {
		s.logger.Error("scheduler: advance recurring job failed",
			"id", r.id, "kind", r.kind, "nextRunAt", nextStr, "err", err)
		return
	}

	// Publish job.failed only when the advance write confirmed a state transition.
	// If n == 0, a concurrent reclaim already moved the row; do not fire a spurious event.
	if n > 0 {
		s.publishJobFailed(scope, r, attempt, errStr)
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
	s.publishJobFailed(scope, r, attempt, errStr)
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

// IsPermanent reports whether err is, or wraps, a permanent error (i.e. was
// created by [Permanent]). It uses errors.As so it correctly unwraps arbitrarily
// deep error chains. Callers outside this package can use IsPermanent to detect the
// permanent/dead-letter signal without depending on the message string.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

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

// ── PeriodicJob / RegisterPeriodic ────────────────────────────────────────────

// PeriodicJob declares a system-scoped, idempotent recurring job. The Key is a
// stable singleton handle: exactly one row exists in the jobs table for a given
// Key at any time, regardless of how many times RegisterPeriodic is called (i.e.
// across restarts). The Kind field selects the registered Handler; the Recurrence
// field must be non-nil and specify exactly one of (RRULE+TZ) or Every.
type PeriodicJob struct {
	// Key is the stable, unique singleton handle. Must be non-empty.
	Key string
	// Kind is the dispatch selector that maps to a registered Handler. Must be non-empty.
	Kind string
	// Payload is the opaque JSON text passed to the handler. Nil is stored as `{}`.
	Payload json.RawMessage
	// Recurrence controls how the job repeats. Must be non-nil and specify exactly
	// one of (RRULE+TZ) or Every. A PeriodicJob with no recurrence is invalid.
	Recurrence *Recurrence
}

// RegisterPeriodic upserts a system-scoped recurring job identified by p.Key.
// Across any number of calls (restarts) with the same Key, exactly one row
// exists in the jobs table.
//
// On INSERT (new row): status='pending', attempt=0, user_id NULL, run_at deferred
// by one interval (now+Every for interval jobs) or the next RRULE occurrence after
// now (for RRULE jobs) to avoid an immediate boot-time fire.
//
// On CONFLICT (existing row): the schedule columns (kind, payload, rrule, tz,
// interval_secs) are always updated in place. Additionally:
//   - If the existing row is dead-lettered (status='failed'), it is revived: status
//     is reset to 'pending', attempt to 0, locked_at cleared, and run_at recomputed
//     to the next future occurrence. This lets a re-registration (e.g. a restart that
//     deploys a fix) bring a permanently-failed periodic job back to life.
//   - If the existing row is in-flight (status='pending' or 'running'), the execution
//     state (status, attempt, run_at, locked_at) is left undisturbed — the live cycle
//     continues and picks up any changed schedule on its next self-reschedule.
//
// Validation: returns an error for an empty Key, empty Kind, nil or zero-valued
// Recurrence (a periodic MUST recur), both RRULE+TZ and Every simultaneously, or
// RRULE without TZ. These checks let the composition root fail fast on boot.
//
// The upsert uses raw SQL on the single-connection write pool, targeting the
// partial unique index (key IS NOT NULL).
func (s *Scheduler) RegisterPeriodic(p PeriodicJob) error {
	if p.Key == "" {
		return errors.New("scheduler: RegisterPeriodic: Key must not be empty")
	}
	if p.Kind == "" {
		return errors.New("scheduler: RegisterPeriodic: Kind must not be empty")
	}

	// Validate recurrence. A PeriodicJob MUST recur — nil or zero-valued Recurrence is invalid.
	rruleCol, tzCol, intervalCol, err := buildRecurrenceColumns(p.Recurrence)
	if err != nil {
		return fmt.Errorf("scheduler: RegisterPeriodic: %w", err)
	}
	if !rruleCol.Valid && !intervalCol.Valid {
		// Neither kind of recurrence was specified (nil or zero Recurrence).
		return errors.New("scheduler: RegisterPeriodic: Recurrence must specify RRULE+TZ or Every; a PeriodicJob must recur")
	}

	// Compute the initial run_at for a brand-new row (used only on INSERT, not on
	// conflict). We defer the first fire by one interval or the next RRULE occurrence
	// to avoid an immediate boot-time fire.
	now := s.now()
	var runAt time.Time
	if intervalCol.Valid {
		// Interval: first fire after one interval from now.
		interval := time.Duration(intervalCol.Int64) * time.Second
		runAt = now.Add(interval)
	} else {
		// RRULE: first fire at the next occurrence strictly after now, in the stored TZ.
		// Anchor at now-in-tz so the Dtstart preserves wall-clock time-of-day.
		next, hasNext, ruleErr := nextRRuleOccurrence(rruleCol.String, tzCol.String, now, now)
		if ruleErr != nil {
			return fmt.Errorf("scheduler: RegisterPeriodic: %w", ruleErr)
		}
		if !hasNext {
			return fmt.Errorf("scheduler: RegisterPeriodic: RRULE %q has no future occurrence from now", rruleCol.String)
		}
		runAt = next
	}

	payload := p.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	nowStr := now.UTC().Format(time.RFC3339)
	runAtStr := runAt.UTC().Format(time.RFC3339)
	jobID := id.New()

	// Upsert: target the partial unique index (key IS NOT NULL) with the conflict predicate.
	//
	// ON INSERT: new row with status='pending', attempt=0, user_id NULL (system scope),
	// and run_at deferred by one interval/RRULE occurrence.
	//
	// ON CONFLICT (key already exists): always update the schedule columns (kind,
	// payload, rrule, tz, interval_secs, updated_at). Additionally, use CASE
	// expressions keyed on the pre-update status to conditionally revive a
	// dead-lettered row (status='failed') back to 'pending' while leaving an
	// in-flight row (status='pending' or 'running') completely undisturbed.
	// In a SQLite DO UPDATE SET clause, bare column references (e.g. "status")
	// refer to the existing row's value; "excluded.col" refers to the proposed
	// insert value. All CASE expressions read the same pre-update status
	// consistently within one atomic SET.
	//
	// sqlc cannot parse this upsert shape for SQLite, so we use raw SQL on the
	// single-connection write pool (inherently serialised), matching the Enqueue pattern.
	const upsertSQL = `
INSERT INTO jobs (id, key, kind, payload, user_id, status, run_at, attempt, locked_at, rrule, tz, interval_secs, created_at, updated_at)
VALUES (?, ?, ?, ?, NULL, 'pending', ?, 0, NULL, ?, ?, ?, ?, ?)
ON CONFLICT(key) WHERE key IS NOT NULL
DO UPDATE SET
    kind          = excluded.kind,
    payload       = excluded.payload,
    rrule         = excluded.rrule,
    tz            = excluded.tz,
    interval_secs = excluded.interval_secs,
    updated_at    = excluded.updated_at,
    status        = CASE WHEN status = 'failed' THEN 'pending'      ELSE status    END,
    attempt       = CASE WHEN status = 'failed' THEN 0              ELSE attempt   END,
    locked_at     = CASE WHEN status = 'failed' THEN NULL           ELSE locked_at END,
    run_at        = CASE WHEN status = 'failed' THEN excluded.run_at ELSE run_at   END`

	_, err = s.st.Writer().ExecContext(context.Background(), upsertSQL,
		jobID, p.Key, p.Kind, string(payload), runAtStr,
		rruleCol, tzCol, intervalCol,
		nowStr, nowStr,
	)
	if err != nil {
		return fmt.Errorf("scheduler: RegisterPeriodic upsert: %w", err)
	}

	return nil
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

// ErrJobNotReschedulable is returned by Reschedule when the target job exists but
// is in a status that cannot be rescheduled — specifically, when the job is
// dead-lettered (status='failed'). A dead-lettered job must first be cancelled
// (cleared) and re-enqueued, or (for periodic jobs) the series revives on
// re-registration. Use errors.Is to detect it.
var ErrJobNotReschedulable = errors.New("scheduler: job is not reschedulable (dead-lettered)")

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

	switch status {
	case "running":
		// Do not touch the row — flipping run_at on a running row risks a double-run
		// when the handler finishes and the claim loop re-sees it.
		return fmt.Errorf("scheduler: reschedule %q: %w", jobID, ErrJobRunning)
	case "failed":
		// Dead-lettered job: the row exists but rescheduling it would silently no-op
		// (RescheduleJob's WHERE is status='pending'). Return a clear sentinel instead
		// of inferring absence from 0 rows affected. Cancel the job then re-enqueue,
		// or for periodic jobs let re-registration revive it.
		return fmt.Errorf("scheduler: reschedule %q: %w", jobID, ErrJobNotReschedulable)
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
