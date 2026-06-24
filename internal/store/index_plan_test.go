package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/store"
)

// eqpRows executes EXPLAIN QUERY PLAN for the given SQL against the provided
// *sql.DB and returns the detail strings from every plan row.
func eqpRows(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN\n"+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			t.Errorf("eqpRows rows.Close: %v", cerr)
		}
	}()
	var plans []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan EQP row: %v", err)
		}
		plans = append(plans, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EQP rows.Err: %v", err)
	}
	return plans
}

// assertNoFullScan checks that no plan row is an unindexed full SCAN of the
// named table (i.e., a SCAN without a USING clause).
func assertNoFullScan(t *testing.T, label, tableName string, plans []string) {
	t.Helper()
	upper := strings.ToUpper(tableName)
	for _, p := range plans {
		up := strings.ToUpper(p)
		if strings.Contains(up, "SCAN "+upper) && !strings.Contains(up, "USING") {
			t.Errorf("[%s] query plan contains unindexed SCAN %s; plan line: %q", label, tableName, p)
		}
	}
}

// assertNoTempBTree checks that no plan row contains a USE TEMP B-TREE FOR
// ORDER BY, which would indicate a sort that the index should be eliminating.
func assertNoTempBTree(t *testing.T, label string, plans []string) {
	t.Helper()
	for _, p := range plans {
		if strings.Contains(strings.ToUpper(p), "USE TEMP B-TREE FOR ORDER BY") {
			t.Errorf("[%s] query plan uses USE TEMP B-TREE FOR ORDER BY; plan line: %q", label, p)
		}
	}
}

// assertUsesIndex checks that at least one plan row references the named index.
func assertUsesIndex(t *testing.T, label, indexName string, plans []string) {
	t.Helper()
	for _, p := range plans {
		if strings.Contains(p, indexName) {
			return
		}
	}
	t.Errorf("[%s] query plan does not reference index %q; plans: %v", label, indexName, plans)
}

// seedIndexPlanUser inserts a minimal user row for index-plan tests that need
// real data to influence the query planner.
func seedIndexPlanUser(t *testing.T, s *store.Store, userID string) {
	t.Helper()
	_, err := s.Writer().ExecContext(context.Background(),
		`INSERT INTO users (id, email, display_name, password_hash, role, timezone, locale, language, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'), strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		userID, userID+"@eqp.test", "EQP User", "hash", "member", "UTC", "en-US", "en",
	)
	if err != nil {
		t.Fatalf("seedIndexPlanUser %q: %v", userID, err)
	}
}

// seedIndexPlanConversation inserts a minimal conversation row.
func seedIndexPlanConversation(t *testing.T, s *store.Store, convID, userID string) {
	t.Helper()
	_, err := s.Writer().ExecContext(context.Background(),
		`INSERT INTO conversations (id, user_id, created_at, updated_at)
         VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'), strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		convID, userID,
	)
	if err != nil {
		t.Fatalf("seedIndexPlanConversation %q: %v", convID, err)
	}
}

// TestListMessages_IndexPlan verifies via EXPLAIN QUERY PLAN that the
// ListMessages query uses the messages_conv_user_created index on
// (conversation_id, user_id, created_at, id) and produces no full unindexed
// SCAN of messages and no USE TEMP B-TREE FOR ORDER BY.
//
// Real rows are seeded so the SQLite query planner sees a non-empty table —
// an empty table can produce degenerate plans that differ from production.
func TestListMessages_IndexPlan(t *testing.T) {
	t.Parallel()
	s := openMigratedStore(t)

	const userID = "eqp-msg-user-01"
	const convID = "eqp-msg-conv-01"

	seedIndexPlanUser(t, s, userID)
	seedIndexPlanConversation(t, s, convID, userID)

	// Seed several messages so the planner sees real data.
	for i := range 5 {
		msgID := strings.Repeat("A", 25) + string(rune('1'+i))
		// Trim to 26 chars (valid ULID length).
		if len(msgID) > 26 {
			msgID = msgID[len(msgID)-26:]
		}
		if _, err := s.Writer().ExecContext(context.Background(),
			`INSERT INTO messages (id, conversation_id, user_id, role, content, created_at)
             VALUES (?, ?, ?, 'user', 'hello', strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
			msgID, convID, userID,
		); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}

	// The SQL emitted by sqlc for ListMessages (positional ?N params).
	// Keep in sync with the generated listMessages constant in db/messages.sql.go.
	const listSQL = `SELECT id, conversation_id, user_id, role, content, tool_calls, tool_call_id, finish_reason, abandoned, created_at
FROM messages
WHERE conversation_id = ?1
  AND user_id = ?2
ORDER BY created_at, id`

	plans := eqpRows(t, s.Reader(), listSQL, convID, userID)

	t.Logf("EXPLAIN QUERY PLAN [ListMessages]:")
	for _, p := range plans {
		t.Logf("  %s", p)
	}
	if len(plans) == 0 {
		t.Fatal("[ListMessages] EXPLAIN QUERY PLAN returned no rows")
	}

	assertUsesIndex(t, "ListMessages", "messages_conv_user_created", plans)
	assertNoTempBTree(t, "ListMessages", plans)
	assertNoFullScan(t, "ListMessages", "messages", plans)
}

// TestReclaimStaleJobs_IndexPlan verifies via EXPLAIN QUERY PLAN that the
// ReclaimStaleJobs UPDATE uses the jobs_running_locked_at partial index
// (finding 5) and does not fall back to an unindexed scan of the jobs table.
//
// A realistic mix of running rows (with varied locked_at) and pending rows is
// seeded so the query planner has enough statistics to prefer the targeted
// partial index over the broader jobs_status_run_at index.  PRAGMA optimize is
// run after seeding to refresh the planner's row estimates before EQP.
func TestReclaimStaleJobs_IndexPlan(t *testing.T) {
	t.Parallel()
	s := openMigratedStore(t)
	ctx := context.Background()

	// Seed 50 running rows with a locked_at in the past.  The planner needs a
	// realistic dataset to prefer jobs_running_locked_at (locked_at range scan)
	// over jobs_status_run_at (status point lookup + full running-rows scan).
	for i := range 50 {
		id := fmt.Sprintf("01JRECLAIM%016d", i)
		if _, err := s.Writer().ExecContext(ctx,
			`INSERT INTO jobs (id, kind, status, run_at, locked_at, created_at, updated_at)
             VALUES (?, 'test', 'running',
                     strftime('%Y-%m-%dT%H:%M:%fZ','now','-1 hour'),
                     strftime('%Y-%m-%dT%H:%M:%fZ','now','-10 minutes'),
                     strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                     strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
			id,
		); err != nil {
			t.Fatalf("seed running job %d: %v", i, err)
		}
	}
	// Also seed pending rows so the planner can distinguish the status
	// distribution: a large pending set makes the running subset more selective.
	for i := range 100 {
		id := fmt.Sprintf("01JPENDING0%015d", i)
		if _, err := s.Writer().ExecContext(ctx,
			`INSERT INTO jobs (id, kind, status, run_at, created_at, updated_at)
             VALUES (?, 'test', 'pending',
                     strftime('%Y-%m-%dT%H:%M:%fZ','now','+1 hour'),
                     strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                     strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
			id,
		); err != nil {
			t.Fatalf("seed pending job %d: %v", i, err)
		}
	}

	// Refresh planner statistics so EQP reflects the seeded distribution.
	if _, err := s.Writer().ExecContext(ctx, "PRAGMA optimize;"); err != nil {
		t.Logf("optimize warn: %v", err)
	}

	// The SQL emitted by sqlc for ReclaimStaleJobs (named params → positional).
	// Keep in sync with the generated reclaimStaleJobs constant in db/jobs.sql.go.
	const reclaimSQL = `UPDATE jobs SET status = 'pending', locked_at = NULL, updated_at = ?1
WHERE status = 'running' AND locked_at IS NOT NULL AND locked_at < ?2`

	plans := eqpRows(t, s.Writer(), reclaimSQL,
		"2026-01-01T00:00:00Z", "2030-01-01T00:00:00Z")

	t.Logf("EXPLAIN QUERY PLAN [ReclaimStaleJobs]:")
	for _, p := range plans {
		t.Logf("  %s", p)
	}
	if len(plans) == 0 {
		t.Fatal("[ReclaimStaleJobs] EXPLAIN QUERY PLAN returned no rows")
	}

	assertUsesIndex(t, "ReclaimStaleJobs", "jobs_running_locked_at", plans)
	assertNoFullScan(t, "ReclaimStaleJobs", "jobs", plans)
}

// TestListConversations_MessagesIndexPlan verifies via EXPLAIN QUERY PLAN that
// the messages-side aggregation inside ListConversations uses the
// messages_user_role_conv index on (user_id, role, conversation_id, created_at)
// and produces no full unindexed SCAN of messages.
//
// Real rows are seeded so the SQLite query planner sees a non-empty table.
func TestListConversations_MessagesIndexPlan(t *testing.T) {
	t.Parallel()
	s := openMigratedStore(t)

	const userID = "eqp-conv-user-01"
	const convID = "eqp-conv-conv-01"

	seedIndexPlanUser(t, s, userID)
	seedIndexPlanConversation(t, s, convID, userID)

	// Seed several user-role messages — these are what the aggregation targets.
	for i := range 5 {
		msgID := strings.Repeat("B", 25) + string(rune('1'+i))
		if len(msgID) > 26 {
			msgID = msgID[len(msgID)-26:]
		}
		if _, err := s.Writer().ExecContext(context.Background(),
			`INSERT INTO messages (id, conversation_id, user_id, role, content, created_at)
             VALUES (?, ?, ?, 'user', 'hi', strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
			msgID, convID, userID,
		); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}

	// The SQL emitted by sqlc for ListConversations (positional ?N params).
	// Keep in sync with the generated listConversations constant in db/conversations.sql.go.
	const listSQL = `SELECT
    c.id,
    c.created_at,
    c.updated_at,
    COALESCE(fm.content, '') AS preview
FROM conversations c
LEFT JOIN (
    SELECT
        m.conversation_id AS conversation_id,
        m.content AS content,
        MIN(m.created_at || m.id) AS first_key
    FROM messages m
    WHERE m.user_id = ?1
      AND m.role = 'user'
    GROUP BY m.conversation_id
) fm ON fm.conversation_id = c.id
WHERE c.user_id = ?1
  AND (
      ?2 IS NULL
      OR c.updated_at < ?2
      OR (c.updated_at = ?2 AND c.id < ?3)
  )
ORDER BY c.updated_at DESC, c.id DESC
LIMIT ?4`

	plans := eqpRows(t, s.Reader(), listSQL, userID, nil, nil, int64(25))

	t.Logf("EXPLAIN QUERY PLAN [ListConversations]:")
	for _, p := range plans {
		t.Logf("  %s", p)
	}
	if len(plans) == 0 {
		t.Fatal("[ListConversations] EXPLAIN QUERY PLAN returned no rows")
	}

	assertUsesIndex(t, "ListConversations", "messages_user_role_conv", plans)
	assertNoFullScan(t, "ListConversations", "messages", plans)
}
