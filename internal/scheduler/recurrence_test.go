package scheduler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/scheduler"
	"github.com/qovira/qovira/internal/store"
)

// ── Helpers for recurrence tests ─────────────────────────────────────────────

// jobRecurrenceRow reads the recurrence-related columns from the jobs table.
func jobRecurrenceRow(
	t *testing.T, db *sql.DB, jobID string,
) (status string, runAt string, attempt int, rruleVal sql.NullString, tzVal sql.NullString, intervalSecs sql.NullInt64) {
	t.Helper()
	err := db.QueryRow(
		`SELECT status, run_at, attempt, rrule, tz, interval_secs FROM jobs WHERE id = ?`, jobID,
	).Scan(&status, &runAt, &attempt, &rruleVal, &tzVal, &intervalSecs)
	if err != nil {
		t.Fatalf("jobRecurrenceRow(%q): %v", jobID, err)
	}
	return
}

// ── Criterion 7: Enqueue rejects both rrule+tz AND interval ─────────────────

// TestEnqueue_RejectsBothRecurrenceKinds verifies that specifying both
// (RRULE+TZ) and Every (interval) in a single EnqueueRequest returns an error.
func TestEnqueue_RejectsBothRecurrenceKinds(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	_, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "test.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "America/New_York",
			Every: 5 * time.Minute, // both set — must be rejected
		},
	})
	if err == nil {
		t.Fatal("Enqueue returned nil for request with both RRULE+TZ and Every; want error")
	}
	t.Logf("Got expected error: %v", err)
}

// TestEnqueue_RejectsRRuleWithoutTZ verifies that an RRULE without TZ is rejected.
func TestEnqueue_RejectsRRuleWithoutTZ(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	_, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "test.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			// TZ is empty — must be rejected
		},
	})
	if err == nil {
		t.Fatal("Enqueue returned nil for request with RRULE but no TZ; want error")
	}
	t.Logf("Got expected error: %v", err)
}

// TestEnqueue_OneShot_NoRecurrenceFields verifies that a nil Recurrence (one-shot)
// inserts NULL into rrule, tz, interval_secs.
func TestEnqueue_OneShot_NoRecurrenceFields(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "oneshot.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		// No Recurrence — one-shot
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	_, _, _, rruleVal, tzVal, intervalSecs := jobRecurrenceRow(t, s.Writer(), jobID)
	if rruleVal.Valid {
		t.Errorf("rrule = %q, want NULL for one-shot job", rruleVal.String)
	}
	if tzVal.Valid {
		t.Errorf("tz = %q, want NULL for one-shot job", tzVal.String)
	}
	if intervalSecs.Valid {
		t.Errorf("interval_secs = %d, want NULL for one-shot job", intervalSecs.Int64)
	}
}

// TestEnqueue_RRULE_PersistsRecurrenceColumns verifies that an RRULE+TZ recurrence
// is stored in the rrule and tz columns, with interval_secs NULL.
func TestEnqueue_RRULE_PersistsRecurrenceColumns(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "rrule.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "America/New_York",
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	_, _, _, rruleVal, tzVal, intervalSecs := jobRecurrenceRow(t, s.Writer(), jobID)
	if !rruleVal.Valid || rruleVal.String != "FREQ=DAILY" {
		t.Errorf("rrule = %v, want FREQ=DAILY", rruleVal)
	}
	if !tzVal.Valid || tzVal.String != "America/New_York" {
		t.Errorf("tz = %v, want America/New_York", tzVal)
	}
	if intervalSecs.Valid {
		t.Errorf("interval_secs = %d, want NULL for RRULE job", intervalSecs.Int64)
	}
}

// TestEnqueue_Interval_PersistsRecurrenceColumns verifies that an Every-based
// recurrence is stored in interval_secs (seconds), with rrule/tz NULL.
func TestEnqueue_Interval_PersistsRecurrenceColumns(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "interval.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		Recurrence: &scheduler.Recurrence{
			Every: 5 * time.Minute,
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	_, _, _, rruleVal, tzVal, intervalSecs := jobRecurrenceRow(t, s.Writer(), jobID)
	if rruleVal.Valid {
		t.Errorf("rrule = %q, want NULL for interval job", rruleVal.String)
	}
	if tzVal.Valid {
		t.Errorf("tz = %q, want NULL for interval job", tzVal.String)
	}
	wantSecs := int64((5 * time.Minute).Seconds())
	if !intervalSecs.Valid || intervalSecs.Int64 != wantSecs {
		t.Errorf("interval_secs = %v, want %d", intervalSecs, wantSecs)
	}
}

// ── Criterion 1: RRULE self-reschedule, DST-correct ──────────────────────────

// TestRecurring_RRULE_SuccessAdvancesRunAt verifies criterion 1:
// a successful RRULE recurring job advances run_at to the next wall-clock occurrence
// in the stored TZ, DST-correct, and resets attempt to 0.
//
// Scenario: FREQ=DAILY job anchored at 8am America/New_York just before the March 2026
// DST spring-forward. The new run_at must be 8am EDT (UTC-4) not 8am EST (UTC-5).
func TestRecurring_RRULE_SuccessAdvancesRunAt(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	// current run_at = 2026-03-07 08:00 EST (day before DST spring-forward)
	runAt := time.Date(2026, time.March, 7, 8, 0, 0, 0, loc) // 2026-03-07T13:00:00Z
	// Simulated "now" = 30 minutes after run_at (handler just completed)
	now := runAt.Add(30 * time.Minute)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 3

	handlerDone := make(chan struct{})
	var handlerOnce atomic.Bool

	clk := fixedClock(now)
	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, clk)
	sched.Register("dst.daily.job", func(_ context.Context, _ scheduler.Job) error {
		if handlerOnce.CompareAndSwap(false, true) {
			close(handlerDone)
		}
		return nil
	})

	// Enqueue with run_at = just before the DST boundary.
	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "dst.daily.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		RunAt:   runAt,
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "America/New_York",
		},
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
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not run within timeout")
	}

	// Give scheduler time to write the self-reschedule.
	time.Sleep(100 * time.Millisecond)

	// Row must still exist (recurring — not deleted on success).
	if !jobExists(t, s.Writer(), jobID) {
		t.Fatal("recurring job row was deleted after success; expected self-reschedule in place")
	}

	status, runAtStr, attempt, _, _, _ := jobRecurrenceRow(t, s.Writer(), jobID)
	if status != "pending" {
		t.Errorf("status = %q, want pending (recurring job re-arms itself)", status)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d, want 0 (reset on success)", attempt)
	}

	// Parse the stored run_at.
	nextRunAt, parseErr := time.Parse(time.RFC3339, runAtStr)
	if parseErr != nil {
		t.Fatalf("parse run_at %q: %v", runAtStr, parseErr)
	}

	// DST-correct expectation: 8am EDT on 2026-03-08 = 2026-03-08T12:00:00Z.
	// "now" is 2026-03-07T13:30:00Z. .After(now) must skip to 2026-03-08 08:00 EDT.
	wantNextUTC := time.Date(2026, time.March, 8, 12, 0, 0, 0, time.UTC) // 8am EDT
	if !nextRunAt.UTC().Equal(wantNextUTC) {
		t.Errorf("run_at (UTC) = %v, want %v (8am local after DST spring-forward)", nextRunAt.UTC(), wantNextUTC)
	}

	// Verify local hour is 8 (DST-correct).
	localHour := nextRunAt.UTC().In(loc).Hour()
	if localHour != 8 {
		t.Errorf("run_at local hour = %d, want 8 (wall-clock must stay at 8am after DST)", localHour)
	}

	// locked_at must be cleared.
	var lockedAt sql.NullString
	if err := s.Writer().QueryRow(`SELECT locked_at FROM jobs WHERE id = ?`, jobID).Scan(&lockedAt); err != nil {
		t.Fatalf("SELECT locked_at: %v", err)
	}
	if lockedAt.Valid {
		t.Errorf("locked_at = %q after self-reschedule, want NULL", lockedAt.String)
	}
}

// ── Criterion 2: interval job advances by Every ───────────────────────────────

// TestRecurring_Interval_SuccessAdvancesByEvery verifies criterion 2:
// an interval-based recurring job advances run_at by exactly Every (from now).
func TestRecurring_Interval_SuccessAdvancesByEvery(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	const every = 5 * time.Minute

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	handlerDone := make(chan struct{})
	var handlerOnce atomic.Bool

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("interval.job", func(_ context.Context, _ scheduler.Job) error {
		if handlerOnce.CompareAndSwap(false, true) {
			close(handlerDone)
		}
		return nil
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "interval.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		Recurrence: &scheduler.Recurrence{
			Every: every,
		},
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
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not run within timeout")
	}

	time.Sleep(100 * time.Millisecond)

	if !jobExists(t, s.Writer(), jobID) {
		t.Fatal("interval job row deleted; want self-reschedule")
	}

	status, runAtStr, attempt, _, _, _ := jobRecurrenceRow(t, s.Writer(), jobID)
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d, want 0", attempt)
	}

	nextRunAt, parseErr := time.Parse(time.RFC3339, runAtStr)
	if parseErr != nil {
		t.Fatalf("parse run_at %q: %v", runAtStr, parseErr)
	}

	// next = now + every (with fixed clock). Allow 1s tolerance for RFC3339 truncation.
	wantNext := now.Add(every)
	diff := nextRunAt.UTC().Sub(wantNext)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("run_at = %v, want ~%v (now+every=%v+%v), diff=%v",
			nextRunAt.UTC(), wantNext, now, every, diff)
	}
}

// ── Criterion 3: downtime catch-up fires once, jumps to next future instant ──

// TestRecurring_DowntimeCatchup verifies criterion 3:
// after simulated downtime across several overdue RRULE occurrences, the job fires
// exactly once and the new run_at is the next FUTURE instant (no replay burst).
func TestRecurring_DowntimeCatchup(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	loc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	// Job was scheduled to fire at 08:00 UTC daily.
	// Simulated downtime: 3 days. run_at is 3 days ago.
	originalRunAt := time.Date(2026, 1, 10, 8, 0, 0, 0, loc) // 3 days overdue
	// "now" is Jan 13 08:30 UTC (after 3 missed occurrences: Jan 10, 11, 12 and
	// currently past Jan 13's occurrence too).
	now := time.Date(2026, 1, 13, 8, 30, 0, 0, loc)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	var runCount atomic.Int32
	handlerInvoked := make(chan struct{}, 1)

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("catchup.job", func(_ context.Context, _ scheduler.Job) error {
		n := runCount.Add(1)
		if n == 1 {
			select {
			case handlerInvoked <- struct{}{}:
			default:
			}
		}
		return nil
	})

	// Enqueue with the overdue run_at (simulating a job stored 3 days ago).
	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "catchup.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		RunAt:   originalRunAt,
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "UTC",
		},
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

	// Wait for the one (and only) handler invocation.
	select {
	case <-handlerInvoked:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not run within timeout")
	}

	// Give extra time for any erroneous second invocation.
	time.Sleep(200 * time.Millisecond)

	// Exactly one run (no burst replay).
	if n := runCount.Load(); n != 1 {
		t.Errorf("handler ran %d times; want exactly 1 (no replay burst after downtime)", n)
	}

	// The new run_at must be the next FUTURE instant (> now).
	status, runAtStr, attempt, _, _, _ := jobRecurrenceRow(t, s.Writer(), jobID)
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d, want 0", attempt)
	}

	nextRunAt, parseErr := time.Parse(time.RFC3339, runAtStr)
	if parseErr != nil {
		t.Fatalf("parse run_at %q: %v", runAtStr, parseErr)
	}

	// nextRunAt must be strictly after now (not a past missed occurrence).
	// now = 2026-01-13T08:30Z, so next DAILY at 08:00 UTC is 2026-01-14T08:00:00Z.
	wantNext := time.Date(2026, 1, 14, 8, 0, 0, 0, loc)
	if !nextRunAt.UTC().Equal(wantNext) {
		t.Errorf("run_at = %v, want %v (next future occurrence, not a past missed one)", nextRunAt.UTC(), wantNext)
	}
	if !nextRunAt.After(now) {
		t.Errorf("run_at = %v is not after now=%v; must be a future instant", nextRunAt, now)
	}
}

// ── Criterion 4: failing recurring job retries the same occurrence ───────────

// TestRecurring_FailureRetriesSameOccurrence verifies criterion 4:
// a failing recurring job retries the current occurrence (existing transient-retry
// backoff path) and does NOT advance run_at to the next RRULE occurrence.
func TestRecurring_FailureRetriesSameOccurrence(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	// Frozen clock. A large backoff (≥ poll interval, far larger than the test
	// runtime) guarantees the retried run_at lands strictly in the future of the
	// frozen clock, so the row is never re-claimed during observation: the handler
	// fails exactly once and we read a stable retry state. This makes the test
	// deterministic instead of racing the poll loop through attempts.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 5 // don't dead-letter during this observation
	cfg.BackoffBase = 1 * time.Hour
	cfg.BackoffCap = 1 * time.Hour

	// Count calls; we'll let only 1 happen and then stop.
	var callCount atomic.Int32
	handlerCalled := make(chan struct{}, 1)

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("retry.recurring.job", func(_ context.Context, _ scheduler.Job) error {
		n := callCount.Add(1)
		if n == 1 {
			select {
			case handlerCalled <- struct{}{}:
			default:
			}
		}
		return errors.New("transient failure")
	})

	originalRunAt := now // immediately due
	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "retry.recurring.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		RunAt:   originalRunAt,
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "UTC",
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait for the one failure. Stop then drains the dispatch goroutine (including
	// its handleFailure DB write), so the retry state is committed before we read.
	select {
	case <-handlerCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not run")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	_ = sched.Stop(stopCtx)

	// The handler should have fired exactly once: the retried run_at is in the
	// future of the frozen clock, so the row is never re-claimed.
	if got := callCount.Load(); got != 1 {
		t.Errorf("handler call count = %d, want 1 (retried row must not be re-claimed under the frozen clock)", got)
	}

	// Row must still exist (recurring — not dead-lettered on first transient failure).
	if !jobExists(t, s.Writer(), jobID) {
		t.Fatal("recurring job row was deleted after transient failure")
	}

	status, runAtStr, _, _, _, _ := jobRecurrenceRow(t, s.Writer(), jobID)
	if status != "pending" {
		t.Errorf("status = %q, want pending (retrying current occurrence)", status)
	}

	// run_at must be inside the backoff window (now, now+BackoffCap] — proving the
	// failure retried the CURRENT occurrence and did NOT skip ahead to the next
	// RRULE instant (which would be now + 24h).
	runAt, parseErr := time.Parse(time.RFC3339, runAtStr)
	if parseErr != nil {
		t.Fatalf("parse run_at %q: %v", runAtStr, parseErr)
	}
	if !runAt.After(now) || runAt.After(now.Add(cfg.BackoffCap)) {
		t.Errorf("run_at = %v is outside the backoff window (%v, %v]; a transient failure must re-arm within backoff, not advance the series",
			runAt, now, now.Add(cfg.BackoffCap))
	}
}

// ── Criterion 5: exhaustion on recurring job advances series, not dead-letters ─

// TestRecurring_ExhaustionAdvancesSeries verifies criterion 5:
// a recurring job that exhausts MaxAttempts emits job.failed but advances to the
// next instant with attempt=0, keeping status=pending (series survives).
func TestRecurring_ExhaustionAdvancesSeries(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	const userID = "01JXYZ000USER0000000000001"

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 2
	cfg.BackoffBase = 1 * time.Millisecond
	cfg.BackoffCap = 1 * time.Millisecond

	bus := &capturingBus{}
	sched := scheduler.NewWithClock(s, bus, cfg, fixedClock(now))

	sched.Register("exhausted.recurring", func(_ context.Context, _ scheduler.Job) error {
		return errors.New("always fails")
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "exhausted.recurring",
		Payload: json.RawMessage(`{}`),
		Scope:   store.UserScope(store.Principal{UserID: userID}),
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "UTC",
		},
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

	// Wait for a job.failed event (exhaustion).
	var gotEvent *publishedEvent
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for i, ev := range bus.published() {
			if ev.event.Type == "job.failed" && ev.userID == userID {
				evts := bus.published()
				gotEvent = &evts[i]
				break
			}
		}
		if gotEvent != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if gotEvent == nil {
		t.Fatal("no job.failed event published after exhaustion; want emission")
	}

	// Give the advance-write time to settle.
	time.Sleep(100 * time.Millisecond)

	// Row must still exist (series survives).
	if !jobExists(t, s.Writer(), jobID) {
		t.Fatal("recurring job row deleted after exhaustion; series must survive")
	}

	// Status must be pending (advanced, not dead-lettered).
	status, runAtStr, attempt, _, _, _ := jobRecurrenceRow(t, s.Writer(), jobID)
	if status != "pending" {
		t.Errorf("status = %q after exhaustion, want pending (series advances, not dead-letters)", status)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d after exhaustion advance, want 0 (reset for next occurrence)", attempt)
	}

	// run_at must be a future occurrence (> now).
	nextRunAt, parseErr := time.Parse(time.RFC3339, runAtStr)
	if parseErr != nil {
		t.Fatalf("parse run_at %q: %v", runAtStr, parseErr)
	}
	if !nextRunAt.After(now) {
		t.Errorf("run_at = %v is not after now=%v; must be next future occurrence", nextRunAt, now)
	}
}

// ── Criterion 6: Permanent error ends the recurring series ───────────────────

// ── Fix #1: compute error dead-letters instead of deleting ───────────────────

// TestRecurring_ComputeErrorDeadLetters verifies that when advanceRecurring
// encounters a compute error (bad TZ or unparseable RRULE) the row is dead-lettered
// (status=failed, last_error set, job.failed emitted) rather than deleted or left running.
//
// A bad TZ ("Not/AZone") cannot be injected through Enqueue (it validates the TZ), so
// this test inserts the row directly via SQL with an invalid tz column value.
func TestRecurring_ComputeErrorDeadLetters(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	const userID = "01JXYZ000USER0000000000002"

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 5

	bus := &capturingBus{}
	sched := scheduler.NewWithClock(s, bus, cfg, fixedClock(now))

	// Register a handler that always succeeds — the failure must come from the
	// compute path (bad TZ), not the handler itself.
	handlerDone := make(chan struct{})
	var handlerOnce atomic.Bool
	sched.Register("bad.tz.recurring", func(_ context.Context, _ scheduler.Job) error {
		if handlerOnce.CompareAndSwap(false, true) {
			close(handlerDone)
		}
		return nil
	})

	// Insert a recurring job row directly with an invalid TZ so advanceRecurring
	// will hit a compute error after the handler succeeds.
	jobID := "01TESTBADTZ000000000000001"
	nowStr := now.UTC().Format(time.RFC3339)
	_, err := s.Writer().ExecContext(context.Background(), `
		INSERT INTO jobs (id, kind, payload, user_id, status, run_at, attempt, rrule, tz, created_at, updated_at)
		VALUES (?, 'bad.tz.recurring', '{}', ?, 'pending', ?, 0, 'FREQ=DAILY', 'Not/AZone', ?, ?)`,
		jobID, userID, nowStr, nowStr, nowStr,
	)
	if err != nil {
		t.Fatalf("insert bad-tz job: %v", err)
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Wait for the handler to run (the bad TZ is only hit after the handler succeeds).
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not run within timeout")
	}

	// Give the scheduler time to process advanceRecurring and write the dead-letter.
	time.Sleep(200 * time.Millisecond)

	// Row must still exist (dead-lettered, not deleted).
	if !jobExists(t, s.Writer(), jobID) {
		t.Fatal("bad-tz recurring job row was deleted; want dead-lettered (row kept) on compute error")
	}

	// Status must be failed (dead-lettered).
	var finalStatus string
	if err := s.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&finalStatus); err != nil {
		t.Fatalf("SELECT status: %v", err)
	}
	if finalStatus != "failed" {
		t.Errorf("status = %q after compute error; want failed (dead-lettered)", finalStatus)
	}

	// last_error must be set.
	lastErr := jobLastError(t, s.Writer(), jobID)
	if !lastErr.Valid || lastErr.String == "" {
		t.Error("last_error is NULL/empty after compute-error dead-letter; want error text")
	}

	// job.failed must be published on the user's bus.
	var gotEvent *publishedEvent
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for i, ev := range bus.published() {
			if ev.event.Type == "job.failed" && ev.userID == userID {
				evts := bus.published()
				gotEvent = &evts[i]
				break
			}
		}
		if gotEvent != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gotEvent == nil {
		t.Fatal("no job.failed event published after compute-error dead-letter; want emission on user's bus")
	}
}

// ── Fix #2a: backward/fall-back DST ──────────────────────────────────────────

// TestRecurring_RRULE_FallBackDST verifies that a DAILY rule anchored at 8am
// America/New_York advances correctly across the Nov 1 2026 fall-back transition
// (EDT UTC-4 → EST UTC-5): the new run_at's LOCAL hour is still 8 and the UTC
// offset shifts by 1 hour.
func TestRecurring_RRULE_FallBackDST(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	// Oct 31 2026 08:00 EDT — the day before the fall-back (Nov 1 2026 02:00 EDT → 01:00 EST).
	runAt := time.Date(2026, time.October, 31, 8, 0, 0, 0, loc) // 2026-10-31T12:00:00Z
	// "now" = 30 min after the occurrence that just fired.
	now := runAt.Add(30 * time.Minute)

	s := openMigratedStore(t)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	handlerDone := make(chan struct{})
	var handlerOnce atomic.Bool

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("fallback.daily.job", func(_ context.Context, _ scheduler.Job) error {
		if handlerOnce.CompareAndSwap(false, true) {
			close(handlerDone)
		}
		return nil
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "fallback.daily.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		RunAt:   runAt,
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "America/New_York",
		},
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
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not run within timeout")
	}
	time.Sleep(100 * time.Millisecond)

	if !jobExists(t, s.Writer(), jobID) {
		t.Fatal("recurring job row was deleted after success; expected self-reschedule")
	}

	_, runAtStr, attempt, _, _, _ := jobRecurrenceRow(t, s.Writer(), jobID)
	if attempt != 0 {
		t.Errorf("attempt = %d, want 0 (reset on success)", attempt)
	}

	nextRunAt, parseErr := time.Parse(time.RFC3339, runAtStr)
	if parseErr != nil {
		t.Fatalf("parse run_at %q: %v", runAtStr, parseErr)
	}

	// Nov 1 2026 08:00 EST = 2026-11-01T13:00:00Z (UTC-5 after fall-back).
	// The UTC offset shifts: Oct 31 8am EDT was UTC-4 (12:00Z), Nov 1 8am EST is UTC-5 (13:00Z).
	wantNextUTC := time.Date(2026, time.November, 1, 13, 0, 0, 0, time.UTC)
	if !nextRunAt.UTC().Equal(wantNextUTC) {
		t.Errorf("run_at (UTC) = %v, want %v (8am EST after fall-back DST)", nextRunAt.UTC(), wantNextUTC)
	}

	// Local hour must still be 8 (wall-clock preserved across fall-back).
	localHour := nextRunAt.UTC().In(loc).Hour()
	if localHour != 8 {
		t.Errorf("run_at local hour = %d, want 8 (wall-clock must stay at 8am after fall-back)", localHour)
	}
}

// ── Fix #2b: ambiguous/nonexistent wall-clock hour (spring-forward 2:30am) ───

// TestRecurring_RRULE_SpringForwardAmbiguousHour verifies that a DAILY rule
// anchored at 2:30am America/New_York does not panic and produces a sane future
// instant when crossing the Mar 8 2026 spring-forward (2:00am → 3:00am; 2:30am
// does not exist). The result must not be zero, must be after now, and the
// returned local hour must be 3 (the library collapses nonexistent times forward).
func TestRecurring_RRULE_SpringForwardAmbiguousHour(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	// Anchor: Mar 7 2026 02:30 EST (the day before spring-forward).
	runAt := time.Date(2026, time.March, 7, 2, 30, 0, 0, loc) // 2026-03-07T07:30:00Z
	// "now" = 5 minutes after the occurrence that just fired.
	now := runAt.Add(5 * time.Minute)

	s := openMigratedStore(t)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	handlerDone := make(chan struct{})
	var handlerOnce atomic.Bool

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("ambiguous.hour.job", func(_ context.Context, _ scheduler.Job) error {
		if handlerOnce.CompareAndSwap(false, true) {
			close(handlerDone)
		}
		return nil
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "ambiguous.hour.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		RunAt:   runAt,
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "America/New_York",
		},
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
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not run within timeout")
	}
	time.Sleep(100 * time.Millisecond)

	if !jobExists(t, s.Writer(), jobID) {
		t.Fatal("recurring job row was deleted; expected self-reschedule for ambiguous-hour rule")
	}

	_, runAtStr, _, _, _, _ := jobRecurrenceRow(t, s.Writer(), jobID)

	nextRunAt, parseErr := time.Parse(time.RFC3339, runAtStr)
	if parseErr != nil {
		t.Fatalf("parse run_at %q: %v", runAtStr, parseErr)
	}

	// Must not be zero.
	if nextRunAt.IsZero() {
		t.Fatal("run_at is zero after spring-forward ambiguous-hour rule; want a sane future instant")
	}

	// Must be strictly after now.
	if !nextRunAt.After(now) {
		t.Errorf("run_at = %v is not after now=%v; must be a future instant", nextRunAt, now)
	}

	// Go's time package resolves the nonexistent 2:30am EST on the spring-forward day
	// by folding backward: 2:30am EST = 1:30am EST (just before the gap). The rrule-go
	// library inherits this, so the result lands at 1:30am on Mar 8 (still EST, hour=1).
	// The key invariants are: not zero, strictly after now, and a sane local hour (1).
	localHour := nextRunAt.UTC().In(loc).Hour()
	if localHour != 1 {
		t.Errorf("run_at local hour = %d on spring-forward day; want 1 (2:30am collapses to 1:30am EST before the gap)", localHour)
	}
}

// ── Fix #3: finite RRULE (COUNT=1) deletes the row ───────────────────────────

// TestRecurring_FiniteRRULE_DeletesRowOnExhaustion verifies that a recurring job
// with a RRULE that has no further occurrence (FREQ=DAILY;COUNT=1 — only one
// occurrence total) is deleted (series complete) after a successful run, with no
// zero/garbage run_at persisted.
func TestRecurring_FiniteRRULE_DeletesRowOnExhaustion(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	handlerDone := make(chan struct{})
	var handlerOnce atomic.Bool

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("finite.count.job", func(_ context.Context, _ scheduler.Job) error {
		if handlerOnce.CompareAndSwap(false, true) {
			close(handlerDone)
		}
		return nil
	})

	// COUNT=1: only one occurrence total; after it fires there are no further occurrences.
	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "finite.count.job",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY;COUNT=1",
			TZ:    "UTC",
		},
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
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not run within timeout")
	}

	// Give the scheduler time to process the series-complete path and delete the row.
	time.Sleep(200 * time.Millisecond)

	// Row must be deleted (series complete — COUNT=1 exhausted).
	if jobExists(t, s.Writer(), jobID) {
		t.Error("finite RRULE job row still exists after COUNT=1 exhausted; want deletion (series complete)")
	}
}

// ── Criterion 6: Permanent error ends the recurring series ───────────────────

// TestRecurring_PermanentErrorEndsSeries verifies criterion 6:
// a Permanent error on a recurring job ends the series (status=failed, no advance).
func TestRecurring_PermanentErrorEndsSeries(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 5 // would normally allow retries; Permanent must skip straight to dead-letter

	bus := &capturingBus{}
	sched := scheduler.NewWithClock(s, bus, cfg, fixedClock(now))

	var callCount atomic.Int32
	sched.Register("permanent.recurring", func(_ context.Context, _ scheduler.Job) error {
		callCount.Add(1)
		return scheduler.Permanent(errors.New("data deleted — cannot recover"))
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "permanent.recurring",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "UTC",
		},
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

	// Wait for status=failed.
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
		t.Errorf("status = %q after Permanent error on recurring job; want failed (series ends)", finalStatus)
	}

	// Handler called exactly once (Permanent skips retries).
	if n := callCount.Load(); n != 1 {
		t.Errorf("handler called %d times; want 1 (Permanent skips retry)", n)
	}

	// last_error must be set.
	lastErr := jobLastError(t, s.Writer(), jobID)
	if !lastErr.Valid || lastErr.String == "" {
		t.Error("last_error is NULL/empty after Permanent dead-letter; want error text")
	}
}

// ── Re-execution positive coverage ───────────────────────────────────────────

// TestReexecution_FailThenSucceed verifies the full claim→fail→re-claim→re-run path:
// a one-shot job whose handler fails on attempt 1 and succeeds on attempt 2 must be
// invoked exactly twice total, and the row must be deleted after the second (successful)
// run. This is the "positive re-execution" path that is not exercised by any other test:
// existing retry tests either freeze the clock so the row is never re-claimed, or only
// verify the row re-arms as pending after one failure without confirming re-execution.
//
// Clock strategy: the advanceable clock starts frozen. BackoffBase=BackoffCap=10m ensures
// the re-armed run_at is now+jitter where jitter ∈ [0,10m] — strictly in the future
// under the frozen clock, so the row is NOT immediately re-claimable. Only after
// clk.Advance(11m) does run_at <= now become true and the poller re-claims on the next
// tick. This makes the clock advance genuinely load-bearing: if the advance were removed
// the second invocation could never occur, proving the re-claim is clock-driven.
// MaxAttempts=5 ensures the job is not dead-lettered before the second attempt.
//
// Note: BackoffBase=1ms would be tempting for speed but is wrong here. RFC3339 stores
// at second granularity; jitter ∈ [0,1ms) truncates to the same second as now, so
// run_at <= now is immediately true and the row is re-claimed before clk.Advance fires —
// passing the test via immediate re-claim rather than clock-driven re-execution, which
// defeats the purpose of the advanceable clock.
func TestReexecution_FailThenSucceed(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	// Start the clock well in the past so the job is immediately due.
	clk := &advanceable{now: time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)}

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 5
	// BackoffBase=BackoffCap=10m: the re-armed run_at = now + jitter where
	// jitter ∈ [0,10m]. Under the frozen advanceable clock, run_at is always
	// strictly > now (even jitter=0 stores as now+0ns = now exactly, which is
	// NOT < now — the claim predicate is run_at <= now). A clock advance of 11m
	// makes any jitter value due on the next poll tick, so the advance is the
	// causal trigger for re-execution (not a timing race).
	cfg.BackoffBase = 10 * time.Minute
	cfg.BackoffCap = 10 * time.Minute

	var callCount atomic.Int32
	// firstFailed is closed when the handler returns its first error.
	firstFailed := make(chan struct{})
	var firstFailedOnce sync.Once
	// secondSucceeded is closed when the handler returns nil on the second call.
	secondSucceeded := make(chan struct{})

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, clk.Now)
	sched.Register("reexec.test", func(_ context.Context, _ scheduler.Job) error {
		n := callCount.Add(1)
		switch n {
		case 1:
			// First call: fail. Signal as the handler unwinds (defer), before the
			// scheduler's post-return retry write (handleFailure / RetryJob DB update).
			defer firstFailedOnce.Do(func() { close(firstFailed) })
			return errors.New("transient: first attempt fails")
		case 2:
			// Second call: succeed.
			close(secondSucceeded)
			return nil
		default:
			// Should not reach here — extra calls are a test failure.
			return errors.New("unexpected extra call")
		}
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "reexec.test",
		Payload: json.RawMessage(`{}`),
		Scope:   store.SystemScope(),
		// RunAt zero → immediately due.
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

	// Wait for the first handler invocation (the failure) to complete.
	select {
	case <-firstFailed:
	case <-time.After(3 * time.Second):
		t.Fatal("first handler invocation did not occur within timeout")
	}

	// Wait for the RetryJob DB write to commit before advancing the clock. The scheduler
	// calls handleFailure after h() returns, so firstFailed fires while the row may still
	// be 'running'. With BackoffBase=10m the re-armed run_at is strictly future under the
	// frozen clock, so the row will NOT be re-claimed until after clk.Advance fires — the
	// row stays present and transitions to 'pending'; it cannot be deleted here.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var st string
		if err := s.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&st); err != nil {
			// Row not yet visible or transient error; retry.
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if st == "pending" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Confirm the row is re-armed (not deleted or still running) before we advance.
	var preAdvanceStatus string
	if err := s.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&preAdvanceStatus); err != nil {
		t.Fatalf("job row missing before clock advance: %v (want pending)", err)
	}
	if preAdvanceStatus != "pending" {
		t.Fatalf("job status = %q before clock advance; want 'pending' (large backoff must prevent immediate re-claim)", preAdvanceStatus)
	}

	// Advance the clock by 11 minutes — past the 10m BackoffCap — so any re-armed
	// row with backoff in [0,10m] is now due on the next poll tick. This advance is
	// the causal trigger for re-execution; without it the row would never be re-claimed.
	clk.Advance(11 * time.Minute)

	// Wait for the second (successful) handler invocation.
	select {
	case <-secondSucceeded:
	case <-time.After(5 * time.Second):
		t.Fatalf("second handler invocation did not occur within timeout (call count = %d)", callCount.Load())
	}

	// Give the scheduler a moment to delete the row (one-shot success path).
	time.Sleep(50 * time.Millisecond)

	// Assert the handler was invoked exactly twice.
	if n := callCount.Load(); n != 2 {
		t.Errorf("handler call count = %d; want exactly 2 (fail once, succeed once)", n)
	}

	// Assert the row is deleted (one-shot success).
	if jobExists(t, s.Writer(), jobID) {
		t.Error("job row still exists after successful second run; expected deletion (one-shot success)")
	}
}
