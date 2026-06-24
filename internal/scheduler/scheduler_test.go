package scheduler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/scheduler"
	"github.com/qovira/qovira/internal/store"
)

// ── capturingBus records published events for test assertions ────────────────

// publishedEvent holds a single Publish call's arguments.
type publishedEvent struct {
	userID string
	event  events.Event
}

// capturingBus is a fake events.Bus that records all Publish calls for later assertion.
// It is safe for concurrent use.
type capturingBus struct {
	mu     sync.Mutex
	events []publishedEvent
}

func (b *capturingBus) Publish(userID string, e events.Event) {
	b.mu.Lock()
	b.events = append(b.events, publishedEvent{userID: userID, event: e})
	b.mu.Unlock()
}

func (b *capturingBus) Subscribe(_ string) (<-chan events.Event, func()) {
	ch := make(chan events.Event)
	return ch, func() { close(ch) }
}

// published returns a snapshot of all recorded events.
func (b *capturingBus) published() []publishedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]publishedEvent, len(b.events))
	copy(out, b.events)
	return out
}

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

// jobLastError returns the last_error column value for the given job id.
func jobLastError(t *testing.T, db *sql.DB, jobID string) sql.NullString {
	t.Helper()
	var lastError sql.NullString
	if err := db.QueryRow(
		`SELECT last_error FROM jobs WHERE id = ?`, jobID,
	).Scan(&lastError); err != nil {
		t.Fatalf("jobLastError(%q): %v", jobID, err)
	}
	return lastError
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

	// The fast job should complete even though 2 slow jobs are running (Workers=4).
	// This verifies the poller doesn't block on slow handlers.
	select {
	case <-fastDone:
	case <-time.After(3 * time.Second):
		t.Fatal("fast job was not executed despite slow jobs occupying workers; poller must be non-blocking")
	}
}

// TestPoller_WorkerCapIsEnforced verifies that the peak concurrency of simultaneously
// running handlers never exceeds cfg.Workers even when more jobs are due simultaneously.
// It enqueues Workers+2 jobs, each of which:
//  1. Atomically increments a live-concurrency counter.
//  2. Blocks on a barrier until all workers have been observed at peak.
//  3. Atomically decrements the counter on exit.
//
// The observed peak counter must be <= Workers AND >= Workers (the barrier guarantees
// all first-batch handlers block before any decrement, so peak must equal Workers).
// The poller implements the cap via "free := Workers - len(s.sem)" then claims exactly
// that many rows per tick, so the buffered semaphore physically bounds concurrent
// handlers. Under -race, the atomic counter catches any unsynchronised access.
func TestPoller_WorkerCapIsEnforced(t *testing.T) {
	t.Parallel()

	const workers = 3
	const totalJobs = workers + 2 // 5 jobs, only 3 can run simultaneously

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.Workers = workers
	cfg.PollInterval = 5 * time.Millisecond
	// Large backoff so a failed claim never results in a retry during this test.
	cfg.BackoffBase = 1 * time.Hour
	cfg.BackoffCap = 1 * time.Hour

	var (
		liveConcurrency atomic.Int32
		peakObserved    atomic.Int32
	)

	// barrier is used to keep all workers running simultaneously so the peak is observable.
	// It is released once we have confirmed the cap holds.
	barrier := make(chan struct{})

	var doneCount atomic.Int32
	allDone := make(chan struct{})

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("cap.test", func(_ context.Context, _ scheduler.Job) error {
		// Increment live counter and record peak.
		cur := liveConcurrency.Add(1)
		for {
			old := peakObserved.Load()
			if cur <= old || peakObserved.CompareAndSwap(old, cur) {
				break
			}
		}
		// Wait at the barrier so multiple goroutines are live simultaneously.
		<-barrier
		liveConcurrency.Add(-1)
		if doneCount.Add(1) == totalJobs {
			close(allDone)
		}
		return nil
	})

	for i := range totalJobs {
		_, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
			Kind:    "cap.test",
			Payload: json.RawMessage(`{}`),
			Scope:   store.SystemScope(),
			Key:     fmt.Sprintf("cap-job-%d", i),
		})
		if err != nil {
			t.Fatalf("Enqueue job %d: %v", i, err)
		}
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Give workers time to fill up to the cap and stabilise before we release.
	// With Workers=3 and PollInterval=5ms, within 100ms at least the first batch of
	// Workers handlers should be blocked at the barrier.
	time.Sleep(200 * time.Millisecond)

	// Release the barrier and wait for all jobs to complete.
	close(barrier)
	select {
	case <-allDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("not all jobs completed within timeout (done=%d want=%d)", doneCount.Load(), totalJobs)
	}

	peak := peakObserved.Load()
	// The barrier guarantees all Workers first-batch handlers are simultaneously running
	// before any of them exit, so the observed peak must equal Workers exactly — not just
	// be <= Workers (which would pass a serialising regression) or > Workers (cap breach).
	if int(peak) > workers {
		t.Errorf("peak concurrent handlers = %d; want <= Workers (%d): cap exceeded", peak, workers)
	}
	if int(peak) < workers {
		t.Errorf("peak concurrent handlers = %d; want >= Workers (%d): barrier must hold all first-batch handlers simultaneously — a serialising regression or premature barrier release", peak, workers)
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
		{
			name: "invalid: MaxAttempts < 1",
			cfg: scheduler.Config{
				PollInterval: 1 * time.Second,
				Workers:      4,
				JobTimeout:   30 * time.Second,
				LeaseTimeout: 5 * time.Minute,
				BackoffBase:  10 * time.Second,
				BackoffCap:   1 * time.Hour,
				MaxAttempts:  0, // would dead-letter every job before it ever runs
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
// (b) the panicking job's row is re-armed as pending (not left running), because
// panics now flow through the retry/dead-letter failure path as transient failures,
// (c) a second, well-behaved job dispatched concurrently/after still runs to
// completion and its row is deleted.
func TestHandler_PanicDoesNotCrash(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.Workers = 4
	// Set MaxAttempts high enough that the panicking job isn't dead-lettered during
	// the short observation window — we want to assert it gets re-armed as pending.
	cfg.MaxAttempts = 100
	// Large backoff so the re-armed run_at is strictly in the future under the frozen
	// clock: with a frozen clock at 12:00:00.000 and BackoffBase=1s, the full-jitter
	// backoff samples in [0,1s) and RFC3339 truncates to the same second — run_at <= now
	// remains true so the poller re-claims on the very next tick and can catch the row
	// transiently 'running' during our status check. 1h backoff guarantees the re-armed
	// run_at is now+jitter > now for any jitter > 0, and the frozen clock never advances
	// past it, making the post-panic status deterministically 'pending'.
	cfg.BackoffBase = 1 * time.Hour
	cfg.BackoffCap = 1 * time.Hour

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

	// (b) The panicking job's row must still be present. After a panic the failure path
	// re-arms it as 'pending' (transient failure with backoff) rather than leaving it
	// in 'running'. Give the scheduler a moment to process the post-panic update.
	// MaxAttempts=100 is chosen specifically so the job is nowhere near dead-lettering
	// during this short window; the only expected outcome is 'pending' (re-armed).
	// A regression that mis-routes the panic to dead-letter (status='failed') would also
	// be caught here, distinguishing the two valid alternatives from this expected path.
	time.Sleep(100 * time.Millisecond)
	if !jobExists(t, s.Writer(), panicJobID) {
		t.Error("panicking job row was deleted; expected it to remain (re-armed as pending)")
	}
	status, _, _, _ := jobRow(t, s.Writer(), panicJobID)
	if status != "pending" {
		t.Errorf("panicking job status = %q; want 'pending' (panic flows through transient-failure path with MaxAttempts=100, must be re-armed not dead-lettered)", status)
	}

	// (c) The well-behaved job must complete and have its row deleted.
	select {
	case <-goodDone:
	case <-time.After(3 * time.Second):
		t.Fatal("well-behaved job did not complete within timeout")
	}
	time.Sleep(50 * time.Millisecond)
	// The good job row was deleted; the panic job row remains (re-armed as pending).
	if !jobExists(t, s.Writer(), panicJobID) {
		t.Error("panic job row deleted; expected it to remain in pending state")
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

// ── Cancel / Reschedule tests (issue: Producers can cancel or reschedule a job) ──

// TestCancel_PendingJob verifies criterion 1: Cancel removes a pending job so it is never claimed.
func TestCancel_PendingJob(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	// Use a far-future RunAt so the poller cannot claim the job before we cancel it.
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "cancel.test",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		RunAt:   now.Add(24 * time.Hour), // far future — will never be due during this test
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Row must exist before cancel.
	if !jobExists(t, s.Writer(), jobID) {
		t.Fatal("job row not found after Enqueue")
	}

	if err := sched.Cancel(context.Background(), jobID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Row must be gone.
	if jobExists(t, s.Writer(), jobID) {
		t.Error("job row still exists after Cancel; expected deletion")
	}
}

// TestCancel_PendingNeverClaimed verifies that a cancelled job is never subsequently claimed
// even when the scheduler is running (criterion 1 — integration).
func TestCancel_PendingNeverClaimed(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	var runCount atomic.Int32
	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("cancel.never", func(_ context.Context, _ scheduler.Job) error {
		runCount.Add(1)
		return nil
	})

	// Enqueue due immediately.
	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "cancel.never",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Cancel before starting the poller.
	if err := sched.Cancel(context.Background(), jobID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Let the poller run a few ticks.
	time.Sleep(100 * time.Millisecond)

	if n := runCount.Load(); n != 0 {
		t.Errorf("handler ran %d times after Cancel; expected 0", n)
	}
}

// TestCancel_NotFound verifies criterion 3: Cancel of a missing id returns ErrJobNotFound.
func TestCancel_NotFound(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(time.Now()))

	err := sched.Cancel(context.Background(), "01JNONEXISTENTJOB000000001")
	if err == nil {
		t.Fatal("Cancel returned nil for non-existent job; want ErrJobNotFound")
	}
	if !errors.Is(err, scheduler.ErrJobNotFound) {
		t.Errorf("Cancel error = %v; want errors.Is(err, ErrJobNotFound)", err)
	}
}

// TestCancel_RunningJob verifies criterion 4: Cancel of a running job deletes the row
// without interrupting the handler and without causing a double-run.
func TestCancel_RunningJob(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	handlerStarted := make(chan struct{})
	handlerBlock := make(chan struct{})
	// release closes handlerBlock exactly once, whether the test body or the
	// cleanup gets there first (an early t.Fatal can skip the body's release).
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(handlerBlock) }) }
	var runCount atomic.Int32

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("cancel.running", func(_ context.Context, _ scheduler.Job) error {
		runCount.Add(1)
		close(handlerStarted)
		<-handlerBlock // block until the test releases it
		return nil
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "cancel.running",
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
		release() // unblock the handler so Stop can drain
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Wait for handler to start (row is now running).
	select {
	case <-handlerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start within timeout")
	}

	// Cancel the running job — must succeed (no ErrJobRunning for Cancel).
	if err := sched.Cancel(context.Background(), jobID); err != nil {
		t.Fatalf("Cancel of running job returned error: %v; want nil", err)
	}

	// Row must be gone.
	if jobExists(t, s.Writer(), jobID) {
		t.Error("job row still exists after Cancel of running job; expected deletion")
	}

	// Release handler and let it finish (post-success delete is a harmless no-op).
	release()

	// Give the scheduler time to process the handler completion.
	time.Sleep(100 * time.Millisecond)

	// Handler must have run exactly once (no double-run).
	if n := runCount.Load(); n != 1 {
		t.Errorf("handler ran %d times; want exactly 1 (no double-run)", n)
	}
}

// TestReschedule_PendingJob verifies criterion 2: Reschedule updates run_at and the
// job fires at the new time, not the old.
func TestReschedule_PendingJob(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	handlerRan := make(chan struct{})
	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("reschedule.test", func(_ context.Context, _ scheduler.Job) error {
		close(handlerRan)
		return nil
	})

	// Enqueue far in the future.
	future := now.Add(24 * time.Hour)
	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "reschedule.test",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		RunAt:   future,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Verify run_at is set to future.
	var runAt string
	if err := s.Writer().QueryRow("SELECT run_at FROM jobs WHERE id = ?", jobID).Scan(&runAt); err != nil {
		t.Fatalf("SELECT run_at: %v", err)
	}
	if runAt != future.UTC().Format(time.RFC3339) {
		t.Errorf("run_at before reschedule = %q, want %q", runAt, future.UTC().Format(time.RFC3339))
	}

	// Reschedule to now (immediately due).
	if err := sched.Reschedule(context.Background(), jobID, now); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}

	// Verify run_at is updated.
	if err := s.Writer().QueryRow("SELECT run_at FROM jobs WHERE id = ?", jobID).Scan(&runAt); err != nil {
		t.Fatalf("SELECT run_at after reschedule: %v", err)
	}
	if runAt != now.UTC().Format(time.RFC3339) {
		t.Errorf("run_at after reschedule = %q, want %q", runAt, now.UTC().Format(time.RFC3339))
	}

	// Start the scheduler — the rescheduled job should fire at the new time.
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	select {
	case <-handlerRan:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not run after Reschedule to now; want it to fire at new time")
	}
}

// TestReschedule_NotFound verifies criterion 3: Reschedule of a missing id returns ErrJobNotFound.
func TestReschedule_NotFound(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(time.Now()))

	err := sched.Reschedule(context.Background(), "01JNONEXISTENTJOB000000001", time.Now())
	if err == nil {
		t.Fatal("Reschedule returned nil for non-existent job; want ErrJobNotFound")
	}
	if !errors.Is(err, scheduler.ErrJobNotFound) {
		t.Errorf("Reschedule error = %v; want errors.Is(err, ErrJobNotFound)", err)
	}
}

// TestReschedule_RunningJob verifies criterion 4: Reschedule of a running job returns
// ErrJobRunning and does NOT touch the row (no double-run risk).
func TestReschedule_RunningJob(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	handlerStarted := make(chan struct{})
	handlerBlock := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(handlerBlock) }) }

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("reschedule.running", func(_ context.Context, _ scheduler.Job) error {
		close(handlerStarted)
		<-handlerBlock
		return nil
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "reschedule.running",
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
		release()
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Wait for handler to be running.
	select {
	case <-handlerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start within timeout")
	}

	// Reschedule must return ErrJobRunning.
	newTime := now.Add(time.Hour)
	err = sched.Reschedule(context.Background(), jobID, newTime)
	if err == nil {
		t.Fatal("Reschedule of running job returned nil; want ErrJobRunning")
	}
	if !errors.Is(err, scheduler.ErrJobRunning) {
		t.Errorf("Reschedule error = %v; want errors.Is(err, ErrJobRunning)", err)
	}

	// Row must still be present in running state (no double-run risk).
	if !jobExists(t, s.Writer(), jobID) {
		t.Error("running job row disappeared during Reschedule; expected it to remain")
	}
	status, _, _, _ := jobRow(t, s.Writer(), jobID)
	if status != "running" {
		t.Errorf("job status after Reschedule = %q, want %q", status, "running")
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// Retry / backoff / dead-letter tests (issue: failing jobs retry with backoff)
// ══════════════════════════════════════════════════════════════════════════════

// TestDeadLetter_AtMaxAttempts verifies criterion 2: at MaxAttempts the job is dead-lettered
// with status=failed, last_error set, row kept, and a job.failed event emitted.
func TestDeadLetter_AtMaxAttempts(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 2 // small so the test converges quickly
	cfg.BackoffBase = 1 * time.Millisecond
	cfg.BackoffCap = 1 * time.Millisecond

	bus := &capturingBus{}
	sched := scheduler.NewWithClock(s, bus, cfg, fixedClock(now))

	var callCount atomic.Int32
	sched.Register("deadletter.test", func(_ context.Context, _ scheduler.Job) error {
		callCount.Add(1)
		return errors.New("always fails")
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "deadletter.test",
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

	// Wait for the job to dead-letter: poll for status=failed.
	var finalStatus string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&finalStatus); err != nil {
			t.Fatalf("SELECT status: %v", err)
		}
		if finalStatus == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if finalStatus != "failed" {
		t.Errorf("job status = %q after MaxAttempts=%d; want %q", finalStatus, cfg.MaxAttempts, "failed")
	}

	// The handler must have been called exactly MaxAttempts times: fail attempt 1 →
	// retry with backoff → fail attempt 2 → dead-letter. A regression that dead-letters
	// on the first failure (skipping the retry loop) would call it only once and pass all
	// the DB assertions above; this assertion catches that regression.
	if n := callCount.Load(); int(n) != cfg.MaxAttempts {
		t.Errorf("handler call count = %d; want %d (== MaxAttempts: fail each attempt then dead-letter)", n, cfg.MaxAttempts)
	}

	// Row must be kept (not deleted).
	if !jobExists(t, s.Writer(), jobID) {
		t.Error("dead-lettered job row was deleted; expected it to remain")
	}

	// last_error must be set.
	lastError := jobLastError(t, s.Writer(), jobID)
	if !lastError.Valid || lastError.String == "" {
		t.Error("last_error is NULL/empty after dead-letter; expected the error text")
	}

	// System-scoped job: job.failed must be logged only, NOT published to the bus.
	// (The bus has no system channel; system jobs are logged only.)
	evts := bus.published()
	for _, ev := range evts {
		if ev.event.Type == "job.failed" {
			t.Errorf("job.failed event published for system-scoped job (userID=%q); want no publish", ev.userID)
		}
	}
}

// TestDeadLetter_PermanentErrorSkipsRetry verifies criterion 3: a Permanent error causes
// immediate dead-letter on the first failure (attempt=1), with no backoff, status=failed.
func TestDeadLetter_PermanentErrorSkipsRetry(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 5 // would normally get 5 attempts; Permanent should skip straight to dead-letter

	bus := &capturingBus{}
	sched := scheduler.NewWithClock(s, bus, cfg, fixedClock(now))

	var callCount atomic.Int32
	sched.Register("permanent.test", func(_ context.Context, _ scheduler.Job) error {
		callCount.Add(1)
		return scheduler.Permanent(errors.New("permanent: data deleted"))
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "permanent.test",
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

	// Wait for dead-letter.
	var finalStatus string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&finalStatus); err != nil {
			t.Fatalf("SELECT status: %v", err)
		}
		if finalStatus == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if finalStatus != "failed" {
		t.Errorf("job status = %q after Permanent error; want %q immediately", finalStatus, "failed")
	}

	// Must have been called exactly once (no retries before dead-letter).
	if n := callCount.Load(); n != 1 {
		t.Errorf("handler called %d times; want exactly 1 (Permanent skips retry)", n)
	}

	// last_error must be set.
	lastError := jobLastError(t, s.Writer(), jobID)
	if !lastError.Valid || lastError.String == "" {
		t.Error("last_error is NULL/empty after Permanent dead-letter; expected the error text")
	}
}

// TestJobFailed_UserScopedPublishesEvent verifies criterion 4 (user side): a user-scoped job
// that dead-letters publishes a job.failed event on the owning user's bus.
func TestJobFailed_UserScopedPublishesEvent(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	const testUserID = "01JXYZ000USER0000000000000"

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 1 // dead-letter on first failure
	cfg.BackoffBase = 1 * time.Millisecond
	cfg.BackoffCap = 1 * time.Millisecond

	bus := &capturingBus{}
	sched := scheduler.NewWithClock(s, bus, cfg, fixedClock(now))

	sched.Register("user.deadletter.test", func(_ context.Context, _ scheduler.Job) error {
		return errors.New("user job failed")
	})

	_, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "user.deadletter.test",
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

	// Wait for a job.failed event to be published.
	var gotEvent *publishedEvent
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for i, ev := range bus.published() {
			if ev.event.Type == "job.failed" && ev.userID == testUserID {
				evts := bus.published()
				gotEvent = &evts[i]
				break
			}
		}
		if gotEvent != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if gotEvent == nil {
		t.Fatal("no job.failed event published on the user's bus within timeout")
	}

	// Verify the event payload is the expected fat event type.
	payload, ok := gotEvent.event.Data.(scheduler.JobFailedEvent)
	if !ok {
		t.Errorf("event.Data type = %T, want scheduler.JobFailedEvent", gotEvent.event.Data)
	} else {
		if payload.Kind != "user.deadletter.test" {
			t.Errorf("JobFailedEvent.Kind = %q, want %q", payload.Kind, "user.deadletter.test")
		}
		if payload.LastError == "" {
			t.Error("JobFailedEvent.LastError is empty; want the error text")
		}
		if payload.Attempt < 1 {
			t.Errorf("JobFailedEvent.Attempt = %d; want >= 1", payload.Attempt)
		}
	}
}

// TestJobFailed_SystemScopedLogsOnly verifies criterion 4 (system side): a system-scoped
// dead-lettered job does NOT publish to the bus (logged only).
func TestJobFailed_SystemScopedLogsOnly(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 1
	cfg.BackoffBase = 1 * time.Millisecond
	cfg.BackoffCap = 1 * time.Millisecond

	bus := &capturingBus{}
	sched := scheduler.NewWithClock(s, bus, cfg, fixedClock(now))

	sched.Register("system.deadletter.test", func(_ context.Context, _ scheduler.Job) error {
		return errors.New("system job failed")
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "system.deadletter.test",
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

	// Wait for the job to reach failed status.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := s.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
			t.Fatalf("SELECT status: %v", err)
		}
		if status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Give a little extra time to ensure no bus publish happens.
	time.Sleep(50 * time.Millisecond)

	// No job.failed event should have been published (system has no bus channel).
	for _, ev := range bus.published() {
		if ev.event.Type == "job.failed" {
			t.Errorf("job.failed event published for system-scoped job (userID=%q); want logged only", ev.userID)
		}
	}
}

// TestRetry_JobTimeoutIsTransient verifies criterion 5: when a handler's JobTimeout fires,
// the job is treated as a transient failure and re-armed as pending (not dead-lettered).
func TestRetry_JobTimeoutIsTransient(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.JobTimeout = 20 * time.Millisecond // very short so the test is fast
	cfg.LeaseTimeout = 1 * time.Minute     // maintain invariant: LeaseTimeout > JobTimeout
	cfg.MaxAttempts = 5                    // don't dead-letter on first attempt
	// Large backoff so the retried run_at lands far in the future of the frozen
	// clock: the job stays 'pending' and is never re-claimed, so it cannot churn
	// through all MaxAttempts and dead-letter before the assertion samples the row.
	// Without this, a slow/loaded runner can observe 'failed' instead of 'pending'
	// (same fragility fixed in TestRecurring_FailureRetriesSameOccurrence).
	cfg.BackoffBase = 1 * time.Hour
	cfg.BackoffCap = 1 * time.Hour

	handlerStarted := make(chan struct{}, 1)

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("timeout.test", func(ctx context.Context, _ scheduler.Job) error {
		select {
		case handlerStarted <- struct{}{}:
		default:
		}
		// Block until context is cancelled by JobTimeout.
		<-ctx.Done()
		return ctx.Err() // returns context.DeadlineExceeded
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "timeout.test",
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

	// Wait for the handler to start (so we know the timeout will fire).
	select {
	case <-handlerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("handler not invoked within timeout")
	}

	// Wait for the job to be re-armed (status=pending after the timeout). Use jobRow to
	// read status and locked_at atomically so the test does not observe a re-claimed row
	// between two separate SELECT calls on high-load parallel runs (when backoff is 0ms
	// and the frozen clock makes run_at immediately due, the poller can re-claim the row
	// between a status read and a separate locked_at read).
	var finalStatus, finalLockedAt string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		finalStatus, finalLockedAt, _, _ = jobRow(t, s.Writer(), jobID)
		if finalStatus == "pending" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if finalStatus != "pending" {
		t.Errorf("status after JobTimeout = %q; want %q (deadline fires = transient, retried)", finalStatus, "pending")
	}

	// locked_at must be cleared (read atomically with status above).
	if finalLockedAt != "" {
		t.Errorf("locked_at = %q after retry; want empty (cleared)", finalLockedAt)
	}
}

// TestRetry_UnknownKindFlowsThroughFailurePath verifies that a job with no registered handler
// flows through the retry/dead-letter failure path (transient), rather than being left in
// 'running' state (the old behavior).
func TestRetry_UnknownKindFlowsThroughFailurePath(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 1 // dead-letter on first attempt so we can assert quickly
	cfg.BackoffBase = 1 * time.Millisecond
	cfg.BackoffCap = 1 * time.Millisecond

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	// No handler registered for "unknown.kind".

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "unknown.kind",
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

	// Wait for the job to dead-letter or retry (must NOT stay in 'running').
	deadline := time.Now().Add(5 * time.Second)
	var finalStatus string
	for time.Now().Before(deadline) {
		if err := s.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&finalStatus); err != nil {
			t.Fatalf("SELECT status: %v", err)
		}
		if finalStatus != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if finalStatus == "running" {
		t.Errorf("unknown-kind job left in 'running' state; want 'pending' or 'failed' (flows through failure path)")
	}
}

// TestPermanent_ErrorWrapsAndUnwraps verifies that scheduler.Permanent wraps the inner error
// and that errors.Is/As can see through it via Unwrap.
func TestPermanent_ErrorWrapsAndUnwraps(t *testing.T) {
	t.Parallel()

	inner := errors.New("inner cause")
	wrapped := scheduler.Permanent(inner)

	if wrapped == nil {
		t.Fatal("Permanent(err) returned nil")
	}

	// errors.Is must see the inner error via Unwrap.
	if !errors.Is(wrapped, inner) {
		t.Errorf("errors.Is(Permanent(inner), inner) = false; want true (Unwrap must chain)")
	}

	// The error message should be non-empty and meaningful.
	if wrapped.Error() == "" {
		t.Error("Permanent(err).Error() is empty")
	}
}

// TestCancel_FailedJob verifies that Cancel of a dead-lettered (status='failed') row deletes
// it cleanly and returns no error. An operator cancelling a dead-lettered job removes it so
// the failed row does not accumulate indefinitely.
func TestCancel_FailedJob(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 1 // dead-letter on the very first failure
	cfg.BackoffBase = 1 * time.Millisecond
	cfg.BackoffCap = 1 * time.Millisecond

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("cancel.failed.test", func(_ context.Context, _ scheduler.Job) error {
		return errors.New("always fails")
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "cancel.failed.test",
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

	// Wait for the job to reach status='failed'.
	deadline := time.Now().Add(5 * time.Second)
	var finalStatus string
	for time.Now().Before(deadline) {
		if err := s.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&finalStatus); err != nil {
			t.Fatalf("SELECT status: %v", err)
		}
		if finalStatus == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if finalStatus != "failed" {
		t.Fatalf("job did not reach status='failed' within timeout (got %q)", finalStatus)
	}

	// Cancel the dead-lettered row — must succeed and remove the row.
	if err := sched.Cancel(context.Background(), jobID); err != nil {
		t.Fatalf("Cancel of failed job returned error: %v; want nil", err)
	}

	// Row must be gone.
	if jobExists(t, s.Writer(), jobID) {
		t.Error("failed job row still exists after Cancel; expected deletion (operator cleanup)")
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// Reclaim tests (issue: crashed or wedged jobs are reclaimed)
// ══════════════════════════════════════════════════════════════════════════════

// insertRunningJob directly inserts a row into the jobs table with status='running'
// and the given locked_at timestamp, bypassing the scheduler claim path. This
// simulates a row orphaned by a process crash.
func insertRunningJob(t *testing.T, db *sql.DB, kind, lockedAt string, attempt int) string {
	t.Helper()
	jobID := "01TEST" + kind[:min(len(kind), 10)] + fmt.Sprintf("%014d", attempt)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO jobs (id, kind, payload, user_id, status, run_at, attempt, locked_at, created_at, updated_at)
		VALUES (?, ?, '{}', NULL, 'running', ?, ?, ?, ?, ?)`,
		jobID, kind, now, attempt, lockedAt, now, now,
	)
	if err != nil {
		t.Fatalf("insertRunningJob: %v", err)
	}
	return jobID
}

// TestReclaim_BootSweepReclaimsStaleLeasedRow verifies acceptance criterion 1:
// a row left running with a locked_at older than LeaseTimeout is reclaimed to pending
// by the boot sweep (before the poll loop ticks).
func TestReclaim_BootSweepReclaimsStaleLeasedRow(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	// Freeze the clock so "now" is well after the stale locked_at.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	cfg := defaultTestConfig()
	cfg.PollInterval = 10 * time.Second // very long — we want only the boot sweep to fire

	// Insert a running row with locked_at = now - 2*LeaseTimeout (definitely stale).
	staleLockedAt := now.Add(-2 * cfg.LeaseTimeout).UTC().Format(time.RFC3339)
	jobID := insertRunningJob(t, s.Writer(), "reclaim.boot", staleLockedAt, 1)

	// Confirm the row is running before Start.
	status, _, attempt, _ := jobRow(t, s.Writer(), jobID)
	if status != "running" {
		t.Fatalf("pre-condition: status = %q, want %q", status, "running")
	}
	if attempt != 1 {
		t.Fatalf("pre-condition: attempt = %d, want 1", attempt)
	}

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	// No handler registered — we just want to observe the reclaim, not dispatch.

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// The boot sweep is synchronous before the poll loop, so after Start returns the
	// reclaim has already happened. Give it a brief moment to be safe.
	time.Sleep(50 * time.Millisecond)

	// Row must be pending now, locked_at cleared, attempt preserved.
	status, lockedAt, attemptAfter, _ := jobRow(t, s.Writer(), jobID)
	if status != "pending" {
		t.Errorf("after boot sweep: status = %q, want %q", status, "pending")
	}
	if lockedAt != "" {
		t.Errorf("after boot sweep: locked_at = %q, want empty (cleared)", lockedAt)
	}
	if attemptAfter != 1 {
		t.Errorf("after boot sweep: attempt = %d, want 1 (reclaim must not reset attempt)", attemptAfter)
	}
}

// TestReclaim_PeriodicTickReclaimsStaleLeasedRow verifies acceptance criterion 2:
// a row inserted AFTER Start (so the boot sweep has already run) is reclaimed to
// pending by the periodic in-loop check when its locked_at goes stale.
func TestReclaim_PeriodicTickReclaimsStaleLeasedRow(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	clk := &advanceable{now: time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)}

	cfg := defaultTestConfig()
	cfg.PollInterval = 10 * time.Millisecond

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, clk.Now)
	// No handler registered — we just want to observe the reclaim.

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Wait for the boot sweep tick to have fired (at least one poll cycle).
	time.Sleep(50 * time.Millisecond)

	// NOW insert the stale running row — after Start, so the boot sweep already ran.
	// locked_at is at the current clock time (not yet stale).
	lockedAtNow := clk.Now().UTC().Format(time.RFC3339)
	jobID := insertRunningJob(t, s.Writer(), "reclaim.tick", lockedAtNow, 2)

	// Confirm status=running.
	status, _, _, _ := jobRow(t, s.Writer(), jobID)
	if status != "running" {
		t.Fatalf("pre-condition: status = %q, want %q", status, "running")
	}

	// Advance the clock past LeaseTimeout so locked_at is now stale.
	clk.Advance(cfg.LeaseTimeout + time.Second)

	// Wait for the periodic reclaim to fire (a few poll intervals).
	var finalStatus, finalLockedAt string
	var finalAttempt int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		finalStatus, finalLockedAt, finalAttempt, _ = jobRow(t, s.Writer(), jobID)
		if finalStatus == "pending" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if finalStatus != "pending" {
		t.Errorf("after periodic reclaim: status = %q, want %q", finalStatus, "pending")
	}
	if finalLockedAt != "" {
		t.Errorf("after periodic reclaim: locked_at = %q, want empty (cleared)", finalLockedAt)
	}
	if finalAttempt != 2 {
		t.Errorf("after periodic reclaim: attempt = %d, want 2 (reclaim must not reset attempt)", finalAttempt)
	}
}

// TestReclaim_NotStaleRowIsNotReclaimed verifies acceptance criterion 4:
// a still-running job with locked_at within LeaseTimeout is NOT reclaimed.
func TestReclaim_NotStaleRowIsNotReclaimed(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	cfg := defaultTestConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.LeaseTimeout = 5 * time.Minute

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Wait for the boot sweep to complete.
	time.Sleep(50 * time.Millisecond)

	// Insert a running row with locked_at = now - (LeaseTimeout / 2): NOT yet stale.
	freshLockedAt := now.Add(-cfg.LeaseTimeout / 2).UTC().Format(time.RFC3339)
	jobID := insertRunningJob(t, s.Writer(), "reclaim.fresh", freshLockedAt, 1)

	// Let several poll ticks run.
	time.Sleep(100 * time.Millisecond)

	// Row must still be running — not reclaimed.
	status, _, _, _ := jobRow(t, s.Writer(), jobID)
	if status != "running" {
		t.Errorf("fresh job: status = %q, want %q (must not be reclaimed when within LeaseTimeout)", status, "running")
	}
}

// TestReclaim_PoisonJobDeadLettersViaAttemptCeiling verifies acceptance criterion 3:
// a process-crashing poison job (simulated by inserting running row with
// attempt=MaxAttempts) is reclaimed → pending (attempt preserved), then re-leased
// (attempt becomes MaxAttempts+1), and the dispatch-time guard dead-letters it
// WITHOUT invoking the handler. The job.failed event is published (user-scoped).
func TestReclaim_PoisonJobDeadLettersViaAttemptCeiling(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.MaxAttempts = 3
	cfg.BackoffBase = 1 * time.Millisecond
	cfg.BackoffCap = 1 * time.Millisecond

	const testUserID = "01JXYZ000USER0000000000000"
	bus := &capturingBus{}

	sched := scheduler.NewWithClock(s, bus, cfg, fixedClock(now))

	// Register a handler that records if it was ever invoked.
	var handlerInvoked atomic.Bool
	sched.Register("poison.job", func(_ context.Context, _ scheduler.Job) error {
		handlerInvoked.Store(true)
		return nil
	})

	// Insert a running row with attempt=MaxAttempts and a stale locked_at.
	// This simulates a job that already used all its attempts and is now orphaned.
	staleLockedAt := now.Add(-2 * cfg.LeaseTimeout).UTC().Format(time.RFC3339)

	// Insert directly with user_id so we can assert job.failed event.
	jobID := "01TESTPOISONJOB0000000000001"
	insertNow := now.UTC().Format(time.RFC3339)
	_, err := s.Writer().ExecContext(context.Background(), `
		INSERT INTO jobs (id, kind, payload, user_id, status, run_at, attempt, locked_at, created_at, updated_at)
		VALUES (?, 'poison.job', '{}', ?, 'running', ?, ?, ?, ?, ?)`,
		jobID, testUserID, insertNow, cfg.MaxAttempts, staleLockedAt, insertNow, insertNow,
	)
	if err != nil {
		t.Fatalf("insert poison job: %v", err)
	}

	// Verify pre-condition: attempt = MaxAttempts, status = running.
	status, _, attempt, _ := jobRow(t, s.Writer(), jobID)
	if status != "running" {
		t.Fatalf("pre-condition: status = %q, want %q", status, "running")
	}
	if attempt != cfg.MaxAttempts {
		t.Fatalf("pre-condition: attempt = %d, want %d", attempt, cfg.MaxAttempts)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Wait for the job to reach status=failed.
	var finalStatus string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&finalStatus); err != nil {
			t.Fatalf("SELECT status: %v", err)
		}
		if finalStatus == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if finalStatus != "failed" {
		t.Errorf("poison job status = %q; want %q (dispatch guard must dead-letter)", finalStatus, "failed")
	}

	// Handler must NEVER have been invoked — the dispatch guard fires before the handler.
	if handlerInvoked.Load() {
		t.Error("handler was invoked for poison job; want it dead-lettered before handler runs")
	}

	// last_error must be set.
	lastErr := jobLastError(t, s.Writer(), jobID)
	if !lastErr.Valid || lastErr.String == "" {
		t.Error("last_error is NULL/empty after dispatch-guard dead-letter; expected error text")
	}

	// job.failed event must be published on the user's bus.
	var gotEvent *publishedEvent
	for i, ev := range bus.published() {
		if ev.event.Type == "job.failed" && ev.userID == testUserID {
			evts := bus.published()
			gotEvent = &evts[i]
			break
		}
	}
	if gotEvent == nil {
		t.Fatal("no job.failed event published for poison job; want job.failed on user's bus")
	}

	payload, ok := gotEvent.event.Data.(scheduler.JobFailedEvent)
	if !ok {
		t.Errorf("event.Data type = %T, want scheduler.JobFailedEvent", gotEvent.event.Data)
	} else {
		if payload.Kind != "poison.job" {
			t.Errorf("JobFailedEvent.Kind = %q, want %q", payload.Kind, "poison.job")
		}
		if payload.LastError == "" {
			t.Error("JobFailedEvent.LastError is empty; want the error text")
		}
	}
}

// TestRetry_BackoffWindowOnTransientFailure verifies that after a transient failure the
// job is re-armed with run_at inside the full-jitter exponential window for attempt=1.
//
// Clock strategy: a frozen clock is injected so that run_at - now equals exactly the
// backoff duration with no real-time drift. BackoffBase=30s, BackoffCap=1h, so the
// attempt=1 ceiling is 30s — well above the 1s RFC3339 storage granularity. The
// assertion allows a ≤1s downward tolerance for the truncation, but no upward slack.
//
// Exponential growth and cap-clamp are covered deterministically by the pure-function
// tests in backoff_internal_test.go (package scheduler); this test focuses solely on
// the end-to-end re-arm path: claim → fail → DB write → status/run_at check.
func TestRetry_BackoffWindowOnTransientFailure(t *testing.T) {
	t.Parallel()

	// BackoffBase=30s, BackoffCap=1h: attempt=1 ceiling = min(1h, 30s*2^0) = 30s.
	// Using second-scale values ensures RFC3339 storage preserves the bound; the
	// 1s truncation is the only allowed slack on the lower bound.
	const backoffBase = 30 * time.Second
	const backoffCap = 1 * time.Hour

	// Freeze the clock at a round second so RFC3339 round-trips without truncation.
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	clk := fixedClock(now)

	s := openMigratedStore(t)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 10 // never dead-letter during this test
	cfg.BackoffBase = backoffBase
	cfg.BackoffCap = backoffCap

	// handlerCalled is closed on the first (and only) handler invocation.
	handlerCalled := make(chan struct{})
	var handlerOnce sync.Once

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, clk)
	sched.Register("backoff.window.test", func(_ context.Context, _ scheduler.Job) error {
		handlerOnce.Do(func() { close(handlerCalled) })
		return errors.New("transient failure")
	})

	// Enqueue with zero RunAt so the scheduler sets run_at = clk() = now, which
	// satisfies the claim predicate (run_at <= now) on the very first tick.
	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "backoff.window.test",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the handler to be invoked exactly once.
	select {
	case <-handlerCalled:
	case <-time.After(10 * time.Second):
		t.Fatal("handler was not called within timeout")
	}

	// Stop the scheduler before it can attempt a second claim (with the frozen clock
	// the re-armed row is not yet due, so this is a safety measure, not a race).
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := sched.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Read the persisted run_at from the DB.
	var runAtStr string
	if err := s.Writer().QueryRow(`SELECT run_at FROM jobs WHERE id = ?`, jobID).Scan(&runAtStr); err != nil {
		t.Fatalf("SELECT run_at: %v", err)
	}
	runAt, err := time.Parse(time.RFC3339, runAtStr)
	if err != nil {
		t.Fatalf("parse run_at %q: %v", runAtStr, err)
	}

	// With a frozen clock, run_at = now + backoff where backoff ∈ [0, BackoffBase].
	// Therefore run_at - now ∈ [0, BackoffBase].
	//
	// RFC3339 stores at second precision; backoffDuration uses Int64N so it can produce
	// values anywhere in [0, ceiling]. A sample of exactly 0 round-trips without loss.
	// A sample of e.g. 17,400,999,999ns stores as 17s (truncation ≤1s downward).
	// We therefore allow ≤1s below 'now' on the lower bound, but no upward slack.
	lowerBound := now.Add(-1 * time.Second) // 1s tolerance for RFC3339 truncation
	upperBound := now.Add(backoffBase)      // attempt=1 ceiling = BackoffBase
	if runAt.Before(lowerBound) {
		t.Errorf("run_at %v < lower bound %v (now=%v minus 1s RFC3339 tolerance); backoff is negative",
			runAt, lowerBound, now)
	}
	if runAt.After(upperBound) {
		t.Errorf("run_at %v > upper bound %v (now=%v + BackoffBase=%v); ceiling exceeded",
			runAt, upperBound, now, backoffBase)
	}

	// The row must be pending (re-armed) with locked_at cleared.
	status, lockedAt, _, _ := jobRow(t, s.Writer(), jobID)
	if status != "pending" {
		t.Errorf("status after transient failure = %q, want %q", status, "pending")
	}
	if lockedAt != "" {
		t.Errorf("locked_at after retry = %q, want empty (cleared)", lockedAt)
	}
}
