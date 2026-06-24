package scheduler

// White-box tests for rearmRow. These live in package scheduler (not package
// scheduler_test) so they can call s.rearmRow directly — the only way to exercise
// that unexported method without a real rows.Scan failure.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/store"
)

// openMigratedStoreInternal opens a fresh SQLCipher database on a temp file and
// runs all migrations. Mirrors the helper in scheduler_test.go but lives in
// package scheduler so the white-box test file can use it without a circular
// dependency on the _test package.
func openMigratedStoreInternal(t *testing.T) *store.Store {
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

// insertRunningRowInternal inserts a row in status='running' with a specific
// locked_at, bypassing the scheduler claim path. Returns the generated job id.
func insertRunningRowInternal(t *testing.T, db *sql.DB, attempt int, lockedAt string) string {
	t.Helper()
	jobID := fmt.Sprintf("01TESTREARM%014d", attempt)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO jobs (id, kind, payload, user_id, status, run_at, attempt, locked_at, created_at, updated_at)
		VALUES (?, 'rearm.test', '{}', NULL, 'running', ?, ?, ?, ?, ?)`,
		jobID, now, attempt, lockedAt, now, now,
	)
	if err != nil {
		t.Fatalf("insertRunningRowInternal: %v", err)
	}
	return jobID
}

// nopBus is a no-op events.Bus for constructing a Scheduler in tests.
type nopBus struct{}

func (nopBus) Publish(_ string, _ events.Event) {}
func (nopBus) Subscribe(_ string) (<-chan events.Event, func()) {
	ch := make(chan events.Event)
	return ch, func() { close(ch) }
}

// TestRearmRow_RunningRowReturnsToPending verifies the core behaviour of rearmRow:
//   - A row in status='running' with a set locked_at is flipped to status='pending'
//     with locked_at cleared.
//   - attempt is unchanged (the claim already consumed one attempt; rearm must not
//     reset or double-count it).
func TestRearmRow_RunningRowReturnsToPending(t *testing.T) {
	t.Parallel()

	st := openMigratedStoreInternal(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := NewWithClock(st, nopBus{}, DefaultConfig(), func() time.Time { return now })

	const attempt = 3
	lockedAt := now.UTC().Format(time.RFC3339)
	jobID := insertRunningRowInternal(t, st.Writer(), attempt, lockedAt)

	// Pre-condition: row is 'running' with locked_at set.
	var status, gotLockedAt string
	var gotAttempt int
	if err := st.Writer().QueryRow(
		`SELECT status, COALESCE(locked_at, ''), attempt FROM jobs WHERE id = ?`, jobID,
	).Scan(&status, &gotLockedAt, &gotAttempt); err != nil {
		t.Fatalf("pre-condition SELECT: %v", err)
	}
	if status != "running" {
		t.Fatalf("pre-condition: status = %q, want running", status)
	}
	if gotLockedAt == "" {
		t.Fatal("pre-condition: locked_at is empty, want set")
	}
	if gotAttempt != attempt {
		t.Fatalf("pre-condition: attempt = %d, want %d", gotAttempt, attempt)
	}

	// Exercise rearmRow directly (white-box call — only possible in package scheduler).
	sched.rearmRow(jobID)

	// Post-condition: row must be 'pending' with locked_at cleared.
	if err := st.Writer().QueryRow(
		`SELECT status, COALESCE(locked_at, ''), attempt FROM jobs WHERE id = ?`, jobID,
	).Scan(&status, &gotLockedAt, &gotAttempt); err != nil {
		t.Fatalf("post-condition SELECT: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q after rearmRow; want pending", status)
	}
	if gotLockedAt != "" {
		t.Errorf("locked_at = %q after rearmRow; want empty (cleared)", gotLockedAt)
	}
	// attempt must be unchanged — the claim already incremented it; rearm must not touch it.
	if gotAttempt != attempt {
		t.Errorf("attempt = %d after rearmRow; want %d (must be unchanged)", gotAttempt, attempt)
	}
}

// TestRearmRow_NonRunningRowIsUntouched verifies the WHERE status='running' guard:
// calling rearmRow on a row that is already 'pending' (or 'failed') must be a no-op.
// This prevents a double-update if reclaim already fired between the scan failure
// and the proactive re-arm.
func TestRearmRow_NonRunningRowIsUntouched(t *testing.T) {
	t.Parallel()

	st := openMigratedStoreInternal(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	sched := NewWithClock(st, nopBus{}, DefaultConfig(), func() time.Time { return now })

	// Insert the row in 'running', then flip it to 'pending' to simulate reclaim
	// having already fired before our proactive re-arm arrives.
	lockedAt := now.UTC().Format(time.RFC3339)
	jobID := insertRunningRowInternal(t, st.Writer(), 2, lockedAt)

	// Flip to pending (simulates a concurrent reclaim having already fired).
	if _, err := st.Writer().ExecContext(context.Background(),
		`UPDATE jobs SET status='pending', locked_at=NULL WHERE id=?`, jobID,
	); err != nil {
		t.Fatalf("flip to pending: %v", err)
	}

	// Sanity: row is now 'pending'.
	var status string
	if err := st.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		t.Fatalf("sanity SELECT: %v", err)
	}
	if status != "pending" {
		t.Fatalf("sanity: status = %q, want pending", status)
	}

	// rearmRow on an already-pending row must be a no-op (WHERE status='running' guard).
	sched.rearmRow(jobID)

	// Status must still be 'pending' (untouched).
	if err := st.Writer().QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		t.Fatalf("post-rearm SELECT: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q after rearmRow on pending row; want pending (must be a no-op)", status)
	}
}
