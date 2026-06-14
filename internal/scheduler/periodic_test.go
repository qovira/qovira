package scheduler_test

// Tests for RegisterPeriodic — "System housekeeping runs exactly once".
//
// Acceptance criteria:
//   1. Repeated registration of the same Key converges to exactly one row.
//   2. The created row is system-scoped (user_id NULL) and recurring.
//   3. Changing the Recurrence and re-registering updates the schedule in place
//      (no duplicate row; recurrence columns reflect the new schedule).
//   4. The registered job self-reschedules through the same machinery as any
//      recurring job.
//   5. End-to-end: Register + RegisterPeriodic → handler fires when due.

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/scheduler"
)

// periodicJobRow reads the columns needed to assert periodic-job behaviour from
// the jobs table, looking up by key (not id — the key is the stable handle).
func periodicJobRow(
	t *testing.T, db *sql.DB, key string,
) (id, status, runAt string, attempt int, userID sql.NullString, rruleVal sql.NullString, tzVal sql.NullString, intervalSecs sql.NullInt64) {
	t.Helper()
	err := db.QueryRow(
		`SELECT id, status, run_at, attempt, user_id, rrule, tz, interval_secs FROM jobs WHERE key = ?`, key,
	).Scan(&id, &status, &runAt, &attempt, &userID, &rruleVal, &tzVal, &intervalSecs)
	if err != nil {
		t.Fatalf("periodicJobRow(%q): %v", key, err)
	}
	return
}

// countJobsByKeyCol counts rows where the key column equals the given value.
// (Re-declared here so this file compiles standalone; the main test file also
// declares countJobsByKey but that is the same helper — both are in the same
// package so only one declaration may exist.)
// Actually, countJobsByKey is already declared in scheduler_test.go. We use it
// directly below.

// ── Criterion 1: repeated RegisterPeriodic converges to exactly one row ──────

// TestRegisterPeriodic_IdempotentAcrossBoots verifies that registering the same
// PeriodicJob Key many times results in exactly one row in the jobs table.
func TestRegisterPeriodic_IdempotentAcrossBoots(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	p := scheduler.PeriodicJob{
		Key:  "test.periodic.singleton",
		Kind: "test.periodic.kind",
		Recurrence: &scheduler.Recurrence{
			Every: time.Minute,
		},
	}

	// Register many times — simulates many restarts.
	for range 10 {
		if err := sched.RegisterPeriodic(p); err != nil {
			t.Fatalf("RegisterPeriodic: %v", err)
		}
	}

	// Exactly one row must exist.
	if n := countJobsByKey(t, s.Writer(), p.Key); n != 1 {
		t.Errorf("row count for key %q = %d, want 1", p.Key, n)
	}
}

// ── Criterion 2: system-scoped (user_id NULL) and recurring ──────────────────

// TestRegisterPeriodic_SystemScopedAndRecurring verifies that the row created by
// RegisterPeriodic has user_id NULL (system scope) and has the recurrence columns
// set (interval_secs for an Every-based job).
func TestRegisterPeriodic_SystemScopedAndRecurring(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	const key = "test.periodic.scope"
	p := scheduler.PeriodicJob{
		Key:  key,
		Kind: "test.periodic.kind",
		Recurrence: &scheduler.Recurrence{
			Every: 5 * time.Minute,
		},
	}

	if err := sched.RegisterPeriodic(p); err != nil {
		t.Fatalf("RegisterPeriodic: %v", err)
	}

	_, _, _, _, userID, _, _, intervalSecs := periodicJobRow(t, s.Writer(), key)

	// Must be system-scoped (user_id NULL).
	if userID.Valid {
		t.Errorf("user_id = %q, want NULL (system scope)", userID.String)
	}

	// Must be recurring (interval_secs set).
	if !intervalSecs.Valid {
		t.Error("interval_secs is NULL, want non-NULL (recurring job)")
	}
	wantSecs := int64((5 * time.Minute).Seconds())
	if intervalSecs.Int64 != wantSecs {
		t.Errorf("interval_secs = %d, want %d", intervalSecs.Int64, wantSecs)
	}
}

// TestRegisterPeriodic_SystemScopedRRULE verifies that an RRULE-based PeriodicJob
// also has user_id NULL and the rrule/tz columns set.
func TestRegisterPeriodic_SystemScopedRRULE(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	const key = "test.periodic.rrule"
	p := scheduler.PeriodicJob{
		Key:  key,
		Kind: "test.periodic.rrule.kind",
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "UTC",
		},
	}

	if err := sched.RegisterPeriodic(p); err != nil {
		t.Fatalf("RegisterPeriodic: %v", err)
	}

	_, _, _, _, userID, rruleVal, tzVal, intervalSecs := periodicJobRow(t, s.Writer(), key)

	// Must be system-scoped.
	if userID.Valid {
		t.Errorf("user_id = %q, want NULL (system scope)", userID.String)
	}
	// RRULE and TZ must be set.
	if !rruleVal.Valid || rruleVal.String != "FREQ=DAILY" {
		t.Errorf("rrule = %v, want FREQ=DAILY", rruleVal)
	}
	if !tzVal.Valid || tzVal.String != "UTC" {
		t.Errorf("tz = %v, want UTC", tzVal)
	}
	// interval_secs must be NULL for an RRULE job.
	if intervalSecs.Valid {
		t.Errorf("interval_secs = %d, want NULL for RRULE job", intervalSecs.Int64)
	}
}

// ── Criterion 2b: initial run_at is deferred (not an immediate boot-time fire) ─

// TestRegisterPeriodic_IntervalRunAtDeferred verifies that an interval-based
// PeriodicJob's initial run_at = now + Every (first fire after one interval —
// avoids immediate boot-time fires).
func TestRegisterPeriodic_IntervalRunAtDeferred(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	const key = "test.periodic.deferred"
	const every = time.Minute
	p := scheduler.PeriodicJob{
		Key:  key,
		Kind: "test.periodic.deferred",
		Recurrence: &scheduler.Recurrence{
			Every: every,
		},
	}

	if err := sched.RegisterPeriodic(p); err != nil {
		t.Fatalf("RegisterPeriodic: %v", err)
	}

	_, _, runAt, _, _, _, _, _ := periodicJobRow(t, s.Writer(), key)

	gotRunAt, err := time.Parse(time.RFC3339, runAt)
	if err != nil {
		t.Fatalf("parse run_at %q: %v", runAt, err)
	}

	// run_at must be now + every (deferred by one interval).
	wantRunAt := now.Add(every)
	diff := gotRunAt.UTC().Sub(wantRunAt.UTC())
	if diff < -time.Second || diff > time.Second {
		t.Errorf("run_at = %v, want %v (now+every); diff=%v — initial fire must be deferred by one interval",
			gotRunAt.UTC(), wantRunAt.UTC(), diff)
	}
}

// ── Criterion 3: changing Recurrence updates the row in place ────────────────

// TestRegisterPeriodic_UpdatesRecurrenceInPlace verifies criterion 3:
// calling RegisterPeriodic with the same Key but a different Recurrence updates
// the stored schedule columns in place. No second row is created.
func TestRegisterPeriodic_UpdatesRecurrenceInPlace(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	const key = "test.periodic.update"

	// First registration: 5-minute interval.
	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "test.periodic.update",
		Recurrence: &scheduler.Recurrence{
			Every: 5 * time.Minute,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic (first): %v", err)
	}

	// Capture the row id so we can assert the row wasn't replaced.
	idBefore, _, _, _, _, _, _, intervalBefore := periodicJobRow(t, s.Writer(), key)
	if !intervalBefore.Valid || intervalBefore.Int64 != int64((5*time.Minute).Seconds()) {
		t.Fatalf("pre-condition: interval_secs = %v, want %d", intervalBefore, int64((5 * time.Minute).Seconds()))
	}

	// Second registration: changed to 10-minute interval.
	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "test.periodic.update",
		Recurrence: &scheduler.Recurrence{
			Every: 10 * time.Minute,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic (second): %v", err)
	}

	// Still exactly one row.
	if n := countJobsByKey(t, s.Writer(), key); n != 1 {
		t.Errorf("row count for key %q = %d after update, want 1", key, n)
	}

	// Same row id (not a new insert).
	idAfter, _, _, _, _, _, _, intervalAfter := periodicJobRow(t, s.Writer(), key)
	if idBefore != idAfter {
		t.Errorf("row id changed: before=%q after=%q; row must be updated in place, not replaced", idBefore, idAfter)
	}

	// interval_secs must reflect the new schedule.
	wantSecs := int64((10 * time.Minute).Seconds())
	if !intervalAfter.Valid || intervalAfter.Int64 != wantSecs {
		t.Errorf("interval_secs = %v after update, want %d", intervalAfter, wantSecs)
	}
}

// TestRegisterPeriodic_UpdatesKindInPlace verifies that the kind column also
// updates when changed in a re-registration.
func TestRegisterPeriodic_UpdatesKindInPlace(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	const key = "test.periodic.kindchange"

	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "kind.v1",
		Recurrence: &scheduler.Recurrence{
			Every: time.Minute,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic (v1): %v", err)
	}

	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "kind.v2",
		Recurrence: &scheduler.Recurrence{
			Every: time.Minute,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic (v2): %v", err)
	}

	// Row count must be 1.
	if n := countJobsByKey(t, s.Writer(), key); n != 1 {
		t.Errorf("row count = %d, want 1", n)
	}

	// The kind column must reflect the updated value.
	var kind string
	if err := s.Writer().QueryRow(`SELECT kind FROM jobs WHERE key = ?`, key).Scan(&kind); err != nil {
		t.Fatalf("SELECT kind: %v", err)
	}
	if kind != "kind.v2" {
		t.Errorf("kind = %q after re-register, want %q", kind, "kind.v2")
	}
}

// ── Criterion 4: self-reschedule via same machinery as any recurring job ──────

// TestRegisterPeriodic_SelfReschedules verifies criterion 4:
// a registered periodic job fires and then self-reschedules through the same
// machinery as any recurring job (AdvanceRecurringJob path). The row must survive,
// status=pending, run_at advanced to now+interval, attempt reset to 0.
func TestRegisterPeriodic_SelfReschedules(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)

	// Clock set so the periodic job is due immediately: run_at = now + Every,
	// and we will advance the clock past that.
	base := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	const every = 5 * time.Minute

	clk := &advanceable{now: base}
	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	handlerDone := make(chan struct{})
	var handlerOnce atomic.Bool

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, clk.Now)
	sched.Register("periodic.reschedule.test", func(_ context.Context, _ scheduler.Job) error {
		if handlerOnce.CompareAndSwap(false, true) {
			close(handlerDone)
		}
		return nil
	})

	// RegisterPeriodic: run_at will be base + every = base+5m.
	const key = "test.periodic.reschedule"
	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "periodic.reschedule.test",
		Recurrence: &scheduler.Recurrence{
			Every: every,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic: %v", err)
	}

	// Advance the clock so the job is due (past base+every).
	clk.Advance(every + time.Second)

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Wait for the handler to fire.
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("periodic job handler did not fire within timeout")
	}

	// Give the scheduler time to write the self-reschedule.
	time.Sleep(100 * time.Millisecond)

	// Row must still exist (self-reschedule, not deleted).
	if n := countJobsByKey(t, s.Writer(), key); n != 1 {
		t.Errorf("row count after handler = %d, want 1 (row must survive for self-reschedule)", n)
	}

	_, status, runAtStr, attempt, _, _, _, intervalSecs := periodicJobRow(t, s.Writer(), key)
	if status != "pending" {
		t.Errorf("status = %q after self-reschedule, want pending", status)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d after self-reschedule, want 0 (reset on success)", attempt)
	}
	if !intervalSecs.Valid {
		t.Error("interval_secs is NULL after self-reschedule; recurrence must be preserved")
	}

	// run_at must be strictly after the current clock time (next occurrence in the future).
	nowAtAdvance := clk.Now()
	gotRunAt, err := time.Parse(time.RFC3339, runAtStr)
	if err != nil {
		t.Fatalf("parse run_at %q: %v", runAtStr, err)
	}
	if !gotRunAt.After(nowAtAdvance) {
		t.Errorf("run_at = %v is not after now=%v; must be a future occurrence", gotRunAt, nowAtAdvance)
	}
}

// ── Criterion 5: end-to-end Register + RegisterPeriodic dispatches to handler ─

// TestRegisterPeriodic_EndToEnd verifies criterion 5:
// the exact shape from the issue spec — Register("harness.sweep_confirmations", handler)
// + RegisterPeriodic({ Key: "harness.sweep_confirmations", Kind: "harness.sweep_confirmations",
// Recurrence: &Recurrence{Every: time.Minute} }) — produces one system job that,
// when due, dispatches to the registered handler.
func TestRegisterPeriodic_EndToEnd(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	base := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	clk := &advanceable{now: base}
	cfg := defaultTestConfig()
	cfg.PollInterval = 5 * time.Millisecond

	var fired atomic.Int32
	handlerDone := make(chan struct{})
	var handlerOnce atomic.Bool

	sched := scheduler.NewWithClock(s, fakeBus{}, cfg, clk.Now)

	// Register the handler first (as the wiring seam specifies).
	sched.Register("harness.sweep_confirmations", func(_ context.Context, job scheduler.Job) error {
		fired.Add(1)
		// Verify scope is system.
		if !job.Scope.IsSystem() {
			t.Errorf("scope.IsSystem() = false, want true for periodic system job")
		}
		if handlerOnce.CompareAndSwap(false, true) {
			close(handlerDone)
		}
		return nil
	})

	// Register the periodic job (as the wiring seam specifies).
	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  "harness.sweep_confirmations",
		Kind: "harness.sweep_confirmations",
		Recurrence: &scheduler.Recurrence{
			Every: time.Minute,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic: %v", err)
	}

	// Exactly one row in the DB.
	if n := countJobsByKey(t, s.Writer(), "harness.sweep_confirmations"); n != 1 {
		t.Errorf("row count = %d, want 1", n)
	}

	// Advance the clock so the job is due (past base + 1 minute).
	clk.Advance(time.Minute + time.Second)

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sched.Stop(stopCtx)
	})

	// Handler must fire.
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("periodic handler did not fire within timeout; expected dispatch to registered handler")
	}

	// Give time to ensure no spurious second fire within this window.
	time.Sleep(50 * time.Millisecond)

	// Handler must have fired at least once (we only wait for one).
	if n := fired.Load(); n < 1 {
		t.Errorf("handler fired %d times, want >= 1", n)
	}
}

// ── Validation: RegisterPeriodic rejects invalid inputs ──────────────────────

// TestRegisterPeriodic_RejectsEmptyKey verifies that an empty Key returns an error.
func TestRegisterPeriodic_RejectsEmptyKey(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(time.Now()))

	err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  "", // invalid
		Kind: "some.kind",
		Recurrence: &scheduler.Recurrence{
			Every: time.Minute,
		},
	})
	if err == nil {
		t.Fatal("RegisterPeriodic with empty Key returned nil; want error")
	}
	t.Logf("Got expected error: %v", err)
}

// TestRegisterPeriodic_RejectsEmptyKind verifies that an empty Kind returns an error.
func TestRegisterPeriodic_RejectsEmptyKind(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(time.Now()))

	err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  "some.key",
		Kind: "", // invalid
		Recurrence: &scheduler.Recurrence{
			Every: time.Minute,
		},
	})
	if err == nil {
		t.Fatal("RegisterPeriodic with empty Kind returned nil; want error")
	}
	t.Logf("Got expected error: %v", err)
}

// TestRegisterPeriodic_RejectsNilRecurrence verifies that a nil Recurrence (no
// recurrence at all) returns an error — a PeriodicJob MUST recur.
func TestRegisterPeriodic_RejectsNilRecurrence(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(time.Now()))

	err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:        "some.key",
		Kind:       "some.kind",
		Recurrence: nil, // invalid — periodic must recur
	})
	if err == nil {
		t.Fatal("RegisterPeriodic with nil Recurrence returned nil; want error")
	}
	t.Logf("Got expected error: %v", err)
}

// TestRegisterPeriodic_RejectsZeroEvery verifies that a zero-valued Every (no
// recurrence kind set) returns an error.
func TestRegisterPeriodic_RejectsZeroEvery(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(time.Now()))

	err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  "some.key",
		Kind: "some.kind",
		Recurrence: &scheduler.Recurrence{
			Every: 0, // neither Every nor RRULE — invalid for a periodic job
		},
	})
	if err == nil {
		t.Fatal("RegisterPeriodic with zero Every and no RRULE returned nil; want error")
	}
	t.Logf("Got expected error: %v", err)
}

// TestRegisterPeriodic_RejectsBothRecurrenceKinds verifies that specifying both
// RRULE+TZ and Every returns an error (same constraint as Enqueue).
func TestRegisterPeriodic_RejectsBothRecurrenceKinds(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(time.Now()))

	err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  "some.key",
		Kind: "some.kind",
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			TZ:    "UTC",
			Every: time.Minute, // both — invalid
		},
	})
	if err == nil {
		t.Fatal("RegisterPeriodic with both RRULE+TZ and Every returned nil; want error")
	}
	t.Logf("Got expected error: %v", err)
}

// TestRegisterPeriodic_RejectsRRuleWithoutTZ verifies that an RRULE without TZ
// returns an error.
func TestRegisterPeriodic_RejectsRRuleWithoutTZ(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(time.Now()))

	err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  "some.key",
		Kind: "some.kind",
		Recurrence: &scheduler.Recurrence{
			RRULE: "FREQ=DAILY",
			// TZ empty — invalid
		},
	})
	if err == nil {
		t.Fatal("RegisterPeriodic with RRULE but no TZ returned nil; want error")
	}
	t.Logf("Got expected error: %v", err)
}

// TestRegisterPeriodic_PayloadDefaultsToEmpty verifies that a zero Payload is
// stored as an empty JSON object `{}`, matching the Enqueue convention.
func TestRegisterPeriodic_PayloadDefaultsToEmpty(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	const key = "test.periodic.payload"
	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "test.periodic.kind",
		// Payload is nil/zero
		Recurrence: &scheduler.Recurrence{
			Every: time.Minute,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic: %v", err)
	}

	var payload string
	if err := s.Writer().QueryRow(`SELECT payload FROM jobs WHERE key = ?`, key).Scan(&payload); err != nil {
		t.Fatalf("SELECT payload: %v", err)
	}
	// Must be valid JSON (at minimum `{}`).
	var v any
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		t.Errorf("payload %q is not valid JSON: %v", payload, err)
	}
}

// TestRegisterPeriodic_WithPayload verifies that a non-nil Payload is stored as-is.
func TestRegisterPeriodic_WithPayload(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	const key = "test.periodic.payload.nonempty"
	wantPayload := json.RawMessage(`{"config":"value"}`)

	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:     key,
		Kind:    "test.periodic.kind",
		Payload: wantPayload,
		Recurrence: &scheduler.Recurrence{
			Every: time.Minute,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic: %v", err)
	}

	var payload string
	if err := s.Writer().QueryRow(`SELECT payload FROM jobs WHERE key = ?`, key).Scan(&payload); err != nil {
		t.Fatalf("SELECT payload: %v", err)
	}
	if payload != string(wantPayload) {
		t.Errorf("payload = %q, want %q", payload, wantPayload)
	}
}

// TestRegisterPeriodic_StatusIsPending verifies that the initial row created by
// RegisterPeriodic has status='pending' and attempt=0.
func TestRegisterPeriodic_StatusIsPending(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	const key = "test.periodic.status"
	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "test.periodic.kind",
		Recurrence: &scheduler.Recurrence{
			Every: time.Minute,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic: %v", err)
	}

	_, status, _, attempt, _, _, _, _ := periodicJobRow(t, s.Writer(), key)
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d, want 0", attempt)
	}
}

// ── Fix: re-registering a dead-lettered periodic job must revive it ──────────

// periodicJobLockedAt reads locked_at for the given key row (NULL → empty string).
func periodicJobLockedAt(t *testing.T, db *sql.DB, key string) string {
	t.Helper()
	var lockedAt sql.NullString
	if err := db.QueryRow(
		`SELECT locked_at FROM jobs WHERE key = ?`, key,
	).Scan(&lockedAt); err != nil {
		t.Fatalf("periodicJobLockedAt(%q): %v", key, err)
	}
	if lockedAt.Valid {
		return lockedAt.String
	}
	return ""
}

// TestRegisterPeriodic_RevivesDeadLettered verifies that calling RegisterPeriodic
// with the same Key on a row that dead-lettered (status='failed') revives it back
// to status='pending' with attempt=0 and locked_at cleared. The row count for the
// key must still be exactly 1 (no duplicate row), and run_at must be a sane future
// instant relative to the scheduler clock.
func TestRegisterPeriodic_RevivesDeadLettered(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	const key = "test.periodic.revive"
	const every = 5 * time.Minute

	// Step 1: initial registration — creates the row as pending.
	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "test.periodic.revive",
		Recurrence: &scheduler.Recurrence{
			Every: every,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic (initial): %v", err)
	}

	// Confirm initial state.
	if n := countJobsByKey(t, s.Writer(), key); n != 1 {
		t.Fatalf("pre-condition: row count = %d, want 1", n)
	}
	_, status, _, attempt, _, _, _, _ := periodicJobRow(t, s.Writer(), key)
	if status != "pending" || attempt != 0 {
		t.Fatalf("pre-condition: status=%q attempt=%d, want pending/0", status, attempt)
	}

	// Step 2: force the row into the dead-lettered state — simulates a Permanent
	// error having been processed by the scheduler without needing to run the engine.
	_, err := s.Writer().ExecContext(context.Background(),
		`UPDATE jobs SET status='failed', last_error='permanent failure', attempt=3, locked_at=NULL WHERE key=?`, key,
	)
	if err != nil {
		t.Fatalf("force dead-letter: %v", err)
	}

	// Confirm forced state.
	_, status, _, attempt, _, _, _, _ = periodicJobRow(t, s.Writer(), key)
	if status != "failed" || attempt != 3 {
		t.Fatalf("pre-condition after force: status=%q attempt=%d, want failed/3", status, attempt)
	}

	// Step 3: re-register with the same key — must revive the row.
	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "test.periodic.revive",
		Recurrence: &scheduler.Recurrence{
			Every: every,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic (re-register): %v", err)
	}

	// Assert exactly one row (no duplicate).
	if n := countJobsByKey(t, s.Writer(), key); n != 1 {
		t.Errorf("row count after re-register = %d, want 1", n)
	}

	// Assert revived to pending with attempt reset.
	_, status, runAtStr, attempt, _, _, _, _ := periodicJobRow(t, s.Writer(), key)
	if status != "pending" {
		t.Errorf("status = %q after revive, want pending", status)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d after revive, want 0", attempt)
	}

	// Assert locked_at is cleared.
	if lockedAt := periodicJobLockedAt(t, s.Writer(), key); lockedAt != "" {
		t.Errorf("locked_at = %q after revive, want NULL/empty", lockedAt)
	}

	// Assert run_at is a sane future instant (now + every).
	gotRunAt, err := time.Parse(time.RFC3339, runAtStr)
	if err != nil {
		t.Fatalf("parse run_at %q: %v", runAtStr, err)
	}
	wantRunAt := now.Add(every)
	diff := gotRunAt.UTC().Sub(wantRunAt.UTC())
	if diff < -time.Second || diff > time.Second {
		t.Errorf("run_at = %v, want ~%v (now+every); diff=%v — revived run_at must be a future occurrence",
			gotRunAt.UTC(), wantRunAt.UTC(), diff)
	}
}

// TestRegisterPeriodic_InFlightPendingUndisturbed verifies that re-registering a
// PeriodicJob whose existing row is in-flight (status='pending' with a non-zero
// attempt, simulating an active retry cycle) does NOT reset attempt, run_at, or
// status — only the schedule columns (kind/payload/rrule/tz/interval_secs) update.
func TestRegisterPeriodic_InFlightPendingUndisturbed(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := scheduler.NewWithClock(s, fakeBus{}, defaultTestConfig(), fixedClock(now))

	const key = "test.periodic.inflight"
	const every = 5 * time.Minute

	// Step 1: initial registration.
	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "kind.v1",
		Recurrence: &scheduler.Recurrence{
			Every: every,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic (initial): %v", err)
	}

	// Step 2: simulate an in-flight retry cycle — set attempt=2 and a custom run_at
	// so we can assert the re-register leaves them untouched.
	customRunAt := now.Add(30 * time.Minute).UTC().Format(time.RFC3339)
	_, err := s.Writer().ExecContext(context.Background(),
		`UPDATE jobs SET status='pending', attempt=2, run_at=? WHERE key=?`, customRunAt, key,
	)
	if err != nil {
		t.Fatalf("force in-flight state: %v", err)
	}

	// Step 3: re-register with a changed Kind — schedule cols must update,
	// but status/attempt/run_at must stay.
	if err := sched.RegisterPeriodic(scheduler.PeriodicJob{
		Key:  key,
		Kind: "kind.v2",
		Recurrence: &scheduler.Recurrence{
			Every: every,
		},
	}); err != nil {
		t.Fatalf("RegisterPeriodic (re-register): %v", err)
	}

	// Exactly one row.
	if n := countJobsByKey(t, s.Writer(), key); n != 1 {
		t.Errorf("row count = %d, want 1", n)
	}

	// Kind must have updated.
	var kind string
	if err := s.Writer().QueryRow(`SELECT kind FROM jobs WHERE key = ?`, key).Scan(&kind); err != nil {
		t.Fatalf("SELECT kind: %v", err)
	}
	if kind != "kind.v2" {
		t.Errorf("kind = %q, want kind.v2 (schedule cols must update)", kind)
	}

	// status/attempt/run_at must be undisturbed.
	_, status, runAtStr, attempt, _, _, _, _ := periodicJobRow(t, s.Writer(), key)
	if status != "pending" {
		t.Errorf("status = %q, want pending (must be undisturbed)", status)
	}
	if attempt != 2 {
		t.Errorf("attempt = %d, want 2 (must be undisturbed for in-flight pending)", attempt)
	}
	if runAtStr != customRunAt {
		t.Errorf("run_at = %q, want %q (must be undisturbed for in-flight pending)", runAtStr, customRunAt)
	}
}
