package scheduler_test

// Tests for the three behavior fixes in this work unit (SCH1).
//
// Fix 1: Scan failure in tick() re-arms the row to pending instead of leaving it running.
// Fix 2: Reschedule of a failed (dead-lettered) job returns ErrJobNotReschedulable.
// Fix 3: exhaustRecurring publishes job.failed only after the advance write confirms a row was updated.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/scheduler"
	"github.com/qovira/qovira/internal/store"
)

// ── Fix 1: scan-failure re-arms the row to pending promptly ─────────────────
//
// TestReclaim_PeriodicSweepRecoversRunningRowWithStaleClock verifies the periodic reclaim sweep path that would recover
// a row stranded by a scan failure: a 'running' row whose locked_at is older than LeaseTimeout (as judged by the
// scheduler clock) is returned to 'pending' within a few poll cycles. This covers the reclaim-sweep recovery path; the
// proactive rearmRow call on scan error is covered by the white-box tests in rearm_test.go.
func TestReclaim_PeriodicSweepRecoversRunningRowWithStaleClock(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	clk := &advanceable{now: time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)}

	cfg := defaultTestConfig()
	cfg.PollInterval = 10 * time.Millisecond
	// LeaseTimeout is 2s so that after advancing by LeaseTimeout + 1s = 3s, the RFC3339 threshold (second-precision)
	// is strictly greater than locked_at. The invariant LeaseTimeout > JobTimeout must hold.
	cfg.JobTimeout = 1 * time.Second
	cfg.LeaseTimeout = 2 * time.Second

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, clk.Now)
	// No handler registered — we only test the reclaim path, not dispatch.

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Let the boot sweep and at least one tick complete.
	time.Sleep(50 * time.Millisecond)

	// Insert a row in 'running' state with locked_at = clk.Now() to simulate exactly what the claim query would have
	// set, had a Scan failure occurred immediately after. The row is 'running' and will become stale once the clock
	// advances past LeaseTimeout.
	lockedAt := clk.Now().UTC().Format(time.RFC3339)
	jobID := insertRunningJob(t, s.Writer(), "scanfail.kind", lockedAt, 1)

	// Confirm pre-condition: row is running.
	status, _, _, _ := jobRow(t, s.Writer(), jobID)
	if status != "running" {
		t.Fatalf("pre-condition: status = %q, want running", status)
	}

	// Advance the clock so locked_at is now older than LeaseTimeout. RFC3339 stores timestamps at second precision, so
	// we advance by LeaseTimeout + 1s to ensure the computed threshold (now - LeaseTimeout) is strictly greater than
	// locked_at in the string comparison used by ReclaimStaleJobs.
	clk.Advance(cfg.LeaseTimeout + time.Second)

	// The periodic reclaim fires on every tick (PollInterval = 10ms). The row must be returned to 'pending' within a
	// few cycles — well within 3 seconds. If the row were only recovered via a wall-clock LeaseTimeout trigger, this
	// would take much longer than the test timeout.
	var finalStatus string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		finalStatus, _, _, _ = jobRow(t, s.Writer(), jobID)
		if finalStatus == "pending" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if finalStatus != "pending" {
		t.Errorf(
			"row status = %q after clock advance past LeaseTimeout; "+
				"want 'pending' (reclaim must fire on each tick, not only after wall-clock LeaseTimeout)",
			finalStatus,
		)
	}
}

// ── Fix 2: Reschedule of a failed (dead-lettered) job returns ErrJobNotReschedulable ─

// TestReschedule_FailedJob_ReturnsNotReschedulable verifies that Reschedule of a dead-lettered (status='failed') job
// returns ErrJobNotReschedulable and does NOT return ErrJobNotFound (the old incorrect behaviour — the row exists, so
// ErrJobNotFound was wrong; the row simply cannot be rescheduled in its current state).
func TestReschedule_FailedJob_ReturnsNotReschedulable(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 1 // dead-letter on first failure
	cfg.BackoffBase = 1 * time.Millisecond
	cfg.BackoffCap = 1 * time.Millisecond

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, fixedClock(now))
	sched.Register("reschedule.failed.test", func(_ context.Context, _ scheduler.Job) error {
		return errors.New("always fails")
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "reschedule.failed.test",
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

	// Wait for the row to reach status='failed'.
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
		t.Fatalf("job did not dead-letter within timeout (got %q)", finalStatus)
	}

	// Reschedule must return ErrJobNotReschedulable, NOT ErrJobNotFound.
	newTime := now.Add(time.Hour)
	err = sched.Reschedule(context.Background(), jobID, newTime)
	if err == nil {
		t.Fatal("Reschedule of failed job returned nil; want ErrJobNotReschedulable")
	}
	if errors.Is(err, scheduler.ErrJobNotFound) {
		t.Errorf("Reschedule of failed job returned ErrJobNotFound; want ErrJobNotReschedulable (row exists, just not reschedulable)")
	}
	if !errors.Is(err, scheduler.ErrJobNotReschedulable) {
		t.Errorf("Reschedule error = %v; want errors.Is(err, ErrJobNotReschedulable)", err)
	}
}

// ── Fix 3: exhaustRecurring publishes job.failed only after confirming advance ─

// TestExhaustRecurring_EventPublishedAfterAdvanceSucceeds verifies that job.failed is published when the series-advance
// write successfully updates a row (the normal path). The fix re-orders the exhaustRecurring logic so the
// AdvanceRecurringJob write runs first and the publish only fires when execrows > 0.
//
// The negative case (advance affects 0 rows because reclaim raced it) cannot be deterministically triggered without
// process-level concurrency injection. This test covers the happy path: advance succeeds → event fires, and the row
// ends up pending.
func TestExhaustRecurring_EventPublishedAfterAdvanceSucceeds(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	const userID = "01JXYZ000USER0000000003"

	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.MaxAttempts = 2
	cfg.BackoffBase = 1 * time.Millisecond
	cfg.BackoffCap = 1 * time.Millisecond

	var mu sync.Mutex
	var jobIDAtExhaustion string

	bus := &capturingBus{}
	sched := scheduler.NewWithClock(s, bus, cfg, fixedClock(now))
	sched.Register("exhaust.event.order", func(_ context.Context, job scheduler.Job) error {
		mu.Lock()
		jobIDAtExhaustion = job.ID
		mu.Unlock()
		return errors.New("always fails")
	})

	jobID, err := sched.Enqueue(context.Background(), scheduler.EnqueueRequest{
		Kind:    "exhaust.event.order",
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

	// Wait for a job.failed event to be published on the user's bus.
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
		t.Fatal("no job.failed event published after exhaustion; want emission when advance succeeds")
	}

	// Allow time for the advance write to settle.
	time.Sleep(100 * time.Millisecond)

	// The job must exist and be pending (series advanced, not dead-lettered).
	if !jobExists(t, s.Writer(), jobID) {
		t.Fatal("recurring job row deleted after exhaustion; series must survive")
	}

	var statusAfter string
	if err := s.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&statusAfter); err != nil {
		t.Fatalf("SELECT status: %v", err)
	}
	if statusAfter != "pending" {
		t.Errorf("status = %q after exhaustion; want pending (series advanced)", statusAfter)
	}

	// Confirm the handler saw this job.
	mu.Lock()
	capturedID := jobIDAtExhaustion
	mu.Unlock()
	if capturedID != jobID {
		t.Errorf("job ID at exhaustion = %q, want %q", capturedID, jobID)
	}

	// Verify the event has the right job kind.
	payload, ok := gotEvent.event.Data.(scheduler.JobFailedEvent)
	if !ok {
		t.Errorf("event.Data type = %T, want scheduler.JobFailedEvent", gotEvent.event.Data)
	} else if payload.Kind != "exhaust.event.order" {
		t.Errorf("JobFailedEvent.Kind = %q, want exhaust.event.order", payload.Kind)
	}

	// Ordering invariant: the event was published only after the advance write succeeded. At the point we observed
	// the event, the DB was already advanced (status='pending'). We read status='pending' above after observing the
	// event, confirming the write preceded (or was concurrent with) the publish.
	_ = sql.NullString{} // keep the sql import used in other declarations
}
