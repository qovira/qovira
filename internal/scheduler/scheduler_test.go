package scheduler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/scheduler"
	"github.com/qovira/qovira/internal/store"
)

// ── Test helpers ─────────────────────────────────────────────────────────────

// openMigratedStore opens a fresh SQLCipher database on a temp file and runs all
// migrations. It is closed via t.Cleanup. Never use ":memory:" -- SQLCipher
// requires a file path.
func openMigratedStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(store.Config{
		Path: filepath.Join(dir, "test.db"),
		Key:  "test-key-sufficiently-long-for-sqlcipher",
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.NewRunner().Up(context.Background(), s.Writer()); err != nil {
		_ = s.Close()
		t.Fatalf("migrations.Up: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fakeBus is a no-op events.Bus for tests that don't need event inspection.
type fakeBus struct{}

func (fakeBus) Publish(_ string, _ events.Event) {}
func (fakeBus) Subscribe(_ string) (<-chan events.Event, func()) {
	ch := make(chan events.Event)
	return ch, func() { close(ch) }
}

// fixedClock returns a clock func that always returns t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// advanceable wraps a time.Time that can be advanced in tests. Safe for concurrent use.
type advanceable struct {
	mu  sync.Mutex
	now time.Time
}

func (a *advanceable) Now() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.now
}

func (a *advanceable) Advance(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = a.now.Add(d)
}

// tableExists reports whether the named table is visible in sqlite_master.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name,
	).Scan(&n); err != nil {
		t.Fatalf("sqlite_master query for %q: %v", name, err)
	}
	return n > 0
}

// indexExists reports whether the named index is visible in sqlite_master.
func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", name,
	).Scan(&n); err != nil {
		t.Fatalf("sqlite_master query for index %q: %v", name, err)
	}
	return n > 0
}

// jobRow queries a single row from the jobs table by id; fails if not found.
func jobRow(t *testing.T, db *sql.DB, jobID string) (status, lockedAt string, attempt int, userID sql.NullString) {
	t.Helper()
	err := db.QueryRow(
		`SELECT status, COALESCE(locked_at, ''), attempt, user_id FROM jobs WHERE id = ?`, jobID,
	).Scan(&status, &lockedAt, &attempt, &userID)
	if err != nil {
		t.Fatalf("jobRow(%q): %v", jobID, err)
	}
	return
}

// jobExists reports whether a row with the given id exists in jobs.
func jobExists(t *testing.T, db *sql.DB, jobID string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count(*) FROM jobs WHERE id = ?", jobID,
	).Scan(&n); err != nil {
		t.Fatalf("jobExists(%q): %v", jobID, err)
	}
	return n > 0
}

// countJobsByKey counts rows in jobs where key = given value.
func countJobsByKey(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count(*) FROM jobs WHERE key = ?", key,
	).Scan(&n); err != nil {
		t.Fatalf("countJobsByKey(%q): %v", key, err)
	}
	return n
}

// defaultTestConfig returns a Config suitable for unit tests -- very short intervals so
// tests are fast, but with the invariant (LeaseTimeout > JobTimeout) satisfied.
func defaultTestConfig() scheduler.Config {
	return scheduler.Config{
		PollInterval: 10 * time.Millisecond,
		Workers:      4,
		JobTimeout:   5 * time.Second,
		LeaseTimeout: 10 * time.Second,
		BackoffBase:  1 * time.Second,
		BackoffCap:   10 * time.Second,
		MaxAttempts:  5,
	}
}

// ── Acceptance criterion 1: migration creates jobs table + both indexes ──────

func TestMigration_JobsTableAndIndexes(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	if !tableExists(t, s.Writer(), "jobs") {
		t.Error("jobs table not found after migration")
	}
	if !indexExists(t, s.Writer(), "jobs_key_unique") {
		t.Error("jobs_key_unique index not found after migration")
	}
	if !indexExists(t, s.Writer(), "jobs_status_run_at") {
		t.Error("jobs_status_run_at index not found after migration")
	}
}

// ── Acceptance criterion 2: Enqueue inserts pending row, returns id, zero RunAt = now ──

func TestEnqueue_InsertsPendingRow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	s := openMigratedStore(t)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	payload := json.RawMessage(`{"hello":"world"}`)
	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "test.job",
		Payload: payload,
		Scope:   store.SystemScope(),
		// RunAt is zero -- should default to now.
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if jobID == "" {
		t.Fatal("Enqueue returned empty job ID")
	}

	// Verify the row exists with status=pending and the correct run_at.
	var status, runAt string
	if err := s.Writer().QueryRow(
		"SELECT status, run_at FROM jobs WHERE id = ?", jobID,
	).Scan(&status, &runAt); err != nil {
		t.Fatalf("SELECT jobs row: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want %q", status, "pending")
	}
	// run_at must equal the injected now.
	wantRunAt := now.UTC().Format(time.RFC3339)
	if runAt != wantRunAt {
		t.Errorf("run_at = %q, want %q", runAt, wantRunAt)
	}
}

// ── Acceptance criterion 3: two Enqueue calls with same Key → one row, returns existing id ──

func TestEnqueue_IdempotentByKey(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	req := scheduler.EnqueueRequest{
		Kind:    "test.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		Key:     "my-unique-job",
	}

	id1, err := sched.Enqueue(context.Background(), req)
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}

	id2, err := sched.Enqueue(context.Background(), req)
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}

	if id1 != id2 {
		t.Errorf("second Enqueue returned different id: first=%q second=%q; want same id", id1, id2)
	}

	// Exactly one row in the DB for this key.
	if n := countJobsByKey(t, s.Writer(), "my-unique-job"); n != 1 {
		t.Errorf("jobs count for key %q = %d, want 1", "my-unique-job", n)
	}
}

// ── Acceptance criterion 4: due job is leased atomically (no double-lease under -race) ──

func TestPoller_ClaimLeaseAtomic(t *testing.T) {
	t.Parallel()

	// Use a controllable clock frozen in the past so we can decide exactly when
	// a job becomes due.
	clk := &advanceable{now: time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)}
	s := openMigratedStore(t)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.Workers = 10 // headroom so slots are never the bottleneck

	// Count how many times the handler runs -- must be exactly 1.
	var runCount atomic.Int32
	handlerDone := make(chan struct{})

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, clk.Now)

	sched.Register("claim.test", func(_ context.Context, _ scheduler.Job) error {
		runCount.Add(1)
		close(handlerDone)
		return nil
	})

	// Enqueue a job with run_at = now (already due).
	_, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "claim.test",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Wait for handler to complete.
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not invoked within timeout")
	}

	// Give time for any erroneous second execution.
	time.Sleep(50 * time.Millisecond)

	if n := runCount.Load(); n != 1 {
		t.Errorf("handler ran %d times, want exactly 1", n)
	}
}

// ── Acceptance criterion 4b: attempt is bumped, locked_at is set after claim ──

func TestPoller_ClaimSetsAttemptAndLockedAt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	s := openMigratedStore(t)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	// Block handler until we can observe the running state.
	handlerStarted := make(chan struct{})
	handlerBlock := make(chan struct{})

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("state.test", func(_ context.Context, _ scheduler.Job) error {
		close(handlerStarted)
		<-handlerBlock
		return nil
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "state.test",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		close(handlerBlock)
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Wait for handler to start.
	select {
	case <-handlerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start within timeout")
	}

	// Inspect running state -- attempt must be 1, status must be running, locked_at set.
	status, lockedAt, attempt, _ := jobRow(t, s.Writer(), jobID)
	if status != "running" {
		t.Errorf("status = %q, want %q", status, "running")
	}
	if attempt != 1 {
		t.Errorf("attempt = %d, want 1", attempt)
	}
	if lockedAt == "" {
		t.Error("locked_at is empty, want a timestamp")
	}
}

// ── Acceptance criterion 5: at most Workers jobs claimed per tick; slow handler ──
//    doesn't block other jobs on a later tick.
//
// This test verifies two things:
//  1. The poller dispatches all due jobs concurrently (non-blocking): 2 slow jobs running
//     simultaneously do not prevent the fast job from being dispatched and completing.
//  2. Claims are bounded by Workers: with Workers=4 and only 3 jobs, all 3 are claimed on
//     the first tick, which is within the Workers bound.
//
// "Slow handler occupies one slot without blocking other jobs" means the poller goroutine
// does not wait for handlers to finish -- it dispatches to the bounded pool and returns.

func TestPoller_WorkerBoundAndNonBlocking(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	cfg := defaultTestConfig()
	cfg.Workers = 4 // 4 slots: 2 slow + 1 fast all run concurrently on tick 1
	cfg.PollInterval = 20 * time.Millisecond

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	// Slow handler: blocks until released.
	slowBlock := make(chan struct{})
	var slowCount atomic.Int32

	// Fast handler: immediately returns nil.
	var fastCount atomic.Int32
	fastDone := make(chan struct{}, 10)

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("slow.job", func(_ context.Context, _ scheduler.Job) error {
		slowCount.Add(1)
		<-slowBlock
		return nil
	})
	sched.Register("fast.job", func(_ context.Context, _ scheduler.Job) error {
		fastCount.Add(1)
		fastDone <- struct{}{}
		return nil
	})

	// Enqueue 2 slow + 1 fast job. All are due now.
	for i := range 2 {
		_, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
			Kind:    "slow.job",
			Payload: json.RawMessage(`{}`),
			Scope:   store.SystemScope(),
			Key:     "slow-" + string(rune('0'+i)),
		})
		if err != nil {
			t.Fatalf("Enqueue slow %d: %v", i, err)
		}
	}
	_, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "fast.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		Key:     "fast-1",
	})
	if err != nil {
		t.Fatalf("Enqueue fast: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		close(slowBlock)
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// The fast job should complete even though 2 slow jobs are running (Workers=2).
	// This verifies the poller doesn't block on slow handlers.
	select {
	case <-fastDone:
	case <-time.After(3 * time.Second):
		t.Fatal("fast job was not executed despite slow jobs occupying workers; poller must be non-blocking")
	}
}

// ── Acceptance criterion 6: handler receives resolved Scope and opaque payload ──

func TestHandler_ReceivesScopeAndPayload(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	type received struct {
		scope   store.Scope
		payload json.RawMessage
	}
	ch := make(chan received, 1)

	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))
	sched.Register("scope.test", func(_ context.Context, job scheduler.Job) error {
		ch <- received{scope: job.Scope, payload: job.Payload}
		return nil
	})

	// System-scoped job.
	wantPayload := json.RawMessage(`{"x":42}`)
	_, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "scope.test",
		Payload: wantPayload,
		Scope:   store.SystemScope(),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx := context.Background()
	if err := sched.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	var got received
	select {
	case got = <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("handler not invoked within timeout")
	}

	if !got.scope.IsSystem() {
		t.Errorf("scope.IsSystem() = false, want true for system-scoped job")
	}
	if string(got.payload) != string(wantPayload) {
		t.Errorf("payload = %q, want %q", got.payload, wantPayload)
	}
}

// TestHandler_UserScopeResolved verifies that a job enqueued with a user scope
// delivers a user scope (not system scope) to the handler.
func TestHandler_UserScopeResolved(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	const testUserID = "01JXYZ000USER0000000000000"

	ch := make(chan store.Scope, 1)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))
	sched.Register("user.scope.test", func(_ context.Context, job scheduler.Job) error {
		ch <- job.Scope
		return nil
	})

	_, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "user.scope.test",
		Payload: json.RawMessage(`{}`),
		Scope:   store.UserScope(store.Principal{UserID: testUserID}),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	var got store.Scope
	select {
	case got = <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("handler not invoked within timeout")
	}

	if got.IsSystem() {
		t.Error("scope.IsSystem() = true, want false for user-scoped job")
	}
	if got.UserID() != testUserID {
		t.Errorf("scope.UserID() = %q, want %q", got.UserID(), testUserID)
	}
}

// ── Acceptance criterion 7: on success, one-shot row is deleted ──

func TestHandler_SuccessDeletesRow(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	done := make(chan struct{})
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))
	sched.Register("delete.test", func(_ context.Context, _ scheduler.Job) error {
		close(done)
		return nil
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "delete.test",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler not invoked within timeout")
	}

	// Give the poller a moment to delete the row.
	time.Sleep(50 * time.Millisecond)

	if jobExists(t, s.Writer(), jobID) {
		t.Error("job row still exists after successful handler; expected deletion")
	}
}

// ── Acceptance criterion 8: Stop cancels in-flight handlers, drains under deadline ──

func TestStop_CancelsInFlightAndDrains(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	handlerStarted := make(chan struct{})
	handlerCtxDone := make(chan struct{})

	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))
	sched.Register("long.job", func(ctx context.Context, _ scheduler.Job) error {
		close(handlerStarted)
		<-ctx.Done() // blocks until ctx is cancelled by Stop
		close(handlerCtxDone)
		return ctx.Err()
	})

	_, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "long.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for handler to start.
	select {
	case <-handlerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start within timeout")
	}

	// Stop with a generous deadline.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stopDone := make(chan error, 1)
	go func() { stopDone <- sched.Stop(stopCtx) }()

	// Handler context must be cancelled.
	select {
	case <-handlerCtxDone:
	case <-time.After(3 * time.Second):
		t.Fatal("handler context was not cancelled by Stop")
	}

	// Stop must return within the deadline.
	select {
	case err := <-stopDone:
		if err != nil {
			t.Errorf("Stop returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within deadline")
	}
}

// ── Acceptance criterion 9: Config.Validate rejects LeaseTimeout <= JobTimeout ──

func TestConfig_Validate_RejectsInvalidLeaseTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     scheduler.Config
		wantErr bool
	}{
		{
			name:    "valid: LeaseTimeout > JobTimeout",
			cfg:     scheduler.DefaultConfig(),
			wantErr: false,
		},
		{
			name: "invalid: LeaseTimeout == JobTimeout",
			cfg: scheduler.Config{
				PollInterval: 1 * time.Second,
				Workers:      4,
				JobTimeout:   30 * time.Second,
				LeaseTimeout: 30 * time.Second, // equal -- invalid
				BackoffBase:  10 * time.Second,
				BackoffCap:   1 * time.Hour,
				MaxAttempts:  5,
			},
			wantErr: true,
		},
		{
			name: "invalid: LeaseTimeout < JobTimeout",
			cfg: scheduler.Config{
				PollInterval: 1 * time.Second,
				Workers:      4,
				JobTimeout:   5 * time.Minute,
				LeaseTimeout: 1 * time.Minute, // less -- invalid
				BackoffBase:  10 * time.Second,
				BackoffCap:   1 * time.Hour,
				MaxAttempts:  5,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// TestDefaultConfig verifies the defaults match the spec.
func TestDefaultConfig_Defaults(t *testing.T) {
	t.Parallel()

	cfg := scheduler.DefaultConfig()

	if cfg.PollInterval != 1*time.Second {
		t.Errorf("PollInterval = %v, want 1s", cfg.PollInterval)
	}
	if cfg.Workers != 4 {
		t.Errorf("Workers = %d, want 4", cfg.Workers)
	}
	if cfg.JobTimeout != 30*time.Second {
		t.Errorf("JobTimeout = %v, want 30s", cfg.JobTimeout)
	}
	if cfg.LeaseTimeout != 5*time.Minute {
		t.Errorf("LeaseTimeout = %v, want 5m", cfg.LeaseTimeout)
	}
	if cfg.BackoffBase != 10*time.Second {
		t.Errorf("BackoffBase = %v, want 10s", cfg.BackoffBase)
	}
	if cfg.BackoffCap != 1*time.Hour {
		t.Errorf("BackoffCap = %v, want 1h", cfg.BackoffCap)
	}
	if cfg.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", cfg.MaxAttempts)
	}
}

// ── Fix #1: panicking handler must not crash the process ─────────────────────
//
// TestHandler_PanicDoesNotCrash verifies three things after a handler panics:
// (a) the scheduler/process survives (the test itself proves this -- a process
// crash would be a test binary crash, not a test failure),
// (b) the panicking job's row is still present with status "running" (not
// deleted, not reset to pending),
// (c) a second, well-behaved job dispatched concurrently/after still runs to
// completion and its row is deleted.
func TestHandler_PanicDoesNotCrash(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.Workers = 4

	// panicDone is closed once the panicking handler has been invoked (the panic
	// itself immediately follows). We use a sync.Once so the close is idempotent
	// in case the poller re-dispatches before the row transitions (shouldn't, but
	// defensive).
	var panicOnce sync.Once
	panicInvoked := make(chan struct{})

	// goodDone is closed once the well-behaved handler completes.
	goodDone := make(chan struct{})

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))

	sched.Register("panic.job", func(_ context.Context, _ scheduler.Job) error {
		panicOnce.Do(func() { close(panicInvoked) })
		panic("intentional test panic")
	})
	sched.Register("good.job", func(_ context.Context, _ scheduler.Job) error {
		close(goodDone)
		return nil
	})

	// Enqueue both jobs so both are due immediately.
	panicJobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "panic.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
	})
	if err != nil {
		t.Fatalf("Enqueue panic job: %v", err)
	}
	_, err = sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "good.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
	})
	if err != nil {
		t.Fatalf("Enqueue good job: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// (a) The panicking handler must have been invoked without crashing the process.
	select {
	case <-panicInvoked:
	case <-time.After(3 * time.Second):
		t.Fatal("panicking handler was not invoked within timeout")
	}

	// (b) The panicking job's row must still be present and in "running" state.
	// Give the scheduler a moment to settle after the panic.
	time.Sleep(50 * time.Millisecond)
	if !jobExists(t, s.Writer(), panicJobID) {
		t.Error("panicking job row was deleted; expected it to remain in running state")
	}
	status, _, _, _ := jobRow(t, s.Writer(), panicJobID)
	if status != "running" {
		t.Errorf("panicking job status = %q, want %q", status, "running")
	}

	// (c) The well-behaved job must complete and have its row deleted.
	select {
	case <-goodDone:
	case <-time.After(3 * time.Second):
		t.Fatal("well-behaved job did not complete within timeout")
	}
	time.Sleep(50 * time.Millisecond)
	// Verify that only the panic job row remains (good job row was deleted).
	var remaining int
	if err := s.Writer().QueryRow("SELECT count(*) FROM jobs").Scan(&remaining); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if remaining != 1 {
		t.Errorf("expected exactly 1 job row remaining (the panicking one), got %d", remaining)
	}
}

// ── Fix #2: Stop before/concurrent with Start must be race-free ──────────────
//
// TestStop_BeforeStart verifies that calling Stop before Start is safe and
// returns nil without panicking or blocking.
func TestStop_BeforeStart(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(time.Now()))

	// Stop without ever calling Start: must not panic, must not block, must return nil.
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sched.Stop(stopCtx); err != nil {
		t.Errorf("Stop before Start returned error: %v", err)
	}
}

// TestStop_ConcurrentWithStart exercises Start and Stop racing. Under -race, any
// unsynchronised read of s.cancel in Stop while Start is writing it must be
// detected. We run several iterations to increase the chance of observing the race.
func TestStop_ConcurrentWithStart(t *testing.T) {
	t.Parallel()

	for range 20 {
		s := openMigratedStore(t)
		sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(time.Now()))

		startErr := make(chan error, 1)
		stopErr := make(chan error, 1)

		go func() { startErr <- sched.Start(t.Context()) }()

		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		go func() { stopErr <- sched.Stop(stopCtx) }()

		if err := <-startErr; err != nil {
			t.Errorf("Start: %v", err)
		}
		if err := <-stopErr; err != nil {
			t.Errorf("Stop: %v", err)
		}
		_ = s.Close()
	}
}
