package store_test

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/qovira/qovira/internal/store"
)

// TestScopeGuard_RealQueries runs the scope guard against the shipped query
// files and asserts zero violations. A future query that omits a user_id
// predicate on a user-owned target table will cause this test to fail, making
// the violation a build failure rather than a silent cross-user data leak.
func TestScopeGuard_RealQueries(t *testing.T) {
	t.Parallel()

	// Resolve the queries directory relative to this test file's location.
	// os.DirFS is the simplest fs.FS implementation for a real directory.
	queriesDir := filepath.Join(repoRoot(t), "internal", "store", "queries")
	fsys := os.DirFS(queriesDir)

	violations, err := store.ScanQueryViolations(fsys)
	if err != nil {
		t.Fatalf("ScanQueryViolations: %v", err)
	}

	if len(violations) > 0 {
		t.Errorf("scope guard found %d violation(s) in shipped queries — every SELECT/UPDATE/DELETE on a user-owned table must include a user_id predicate:", len(violations))
		for _, v := range violations {
			t.Errorf("  file=%s query=%s reason=%s", v.File, v.QueryName, v.Reason)
		}
	}
}

// repoRoot returns the repository root by walking up from the test binary's
// working directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root (go.mod) from", wd)
		}
		dir = parent
	}
}

// TestScopeGuard_Fixtures tests the guard logic with synthetic query content.
// It covers simple happy-path cases, allowlisted system tables, and adversarial
// shapes that must fail closed (JOIN-scoped targets, subqueries, UNION, WITH/CTE,
// missing WHERE, user_id only in SELECT list, user_id only in a comment).
func TestScopeGuard_Fixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		sql            string
		wantViol       bool   // whether a violation is expected
		wantQuery      string // query name expected in violation (only checked when wantViol=true)
		wantReasonFill bool   // when true, the matched violation must have a non-empty Reason
	}{
		// ----------------------------------------------------------------
		// Happy-path / allowlisted cases — must NOT produce violations.
		// ----------------------------------------------------------------
		{
			name: "select_user_table_with_bare_user_id",
			sql: `-- name: ListItems :many
SELECT id, user_id, value FROM items
WHERE user_id = ?
ORDER BY created_at;
`,
			wantViol: false,
		},
		{
			name: "select_user_table_with_qualified_user_id",
			// items.user_id = ? is an explicit target-table qualification.
			sql: `-- name: ListItems :many
SELECT id, value FROM items
WHERE items.user_id = ?;
`,
			wantViol: false,
		},
		{
			name: "select_system_table_no_user_id",
			sql: `-- name: GetInstance :one
SELECT id, created_at FROM instance WHERE id = 1;
`,
			wantViol: false,
		},
		{
			name: "delete_user_table_with_user_id",
			sql: `-- name: PurgeItem :exec
DELETE FROM items
WHERE id = ?
  AND user_id = ?;
`,
			wantViol: false,
		},
		{
			name: "update_user_table_with_user_id",
			sql: `-- name: SetValue :exec
UPDATE items
SET value = ?
WHERE id = ?
  AND user_id = ?;
`,
			wantViol: false,
		},
		{
			name: "insert_user_table_no_user_id_in_where",
			// INSERTs are always exempt — user_id appears as a column value, not a predicate.
			sql: `-- name: CreateItem :exec
INSERT INTO items (id, user_id, value) VALUES (?, ?, ?);
`,
			wantViol: false,
		},
		{
			name: "goose_db_version_system_table",
			sql: `-- name: GetGooseVersion :one
SELECT version_id FROM goose_db_version ORDER BY id DESC LIMIT 1;
`,
			wantViol: false,
		},
		{
			// WHERE flush against a newline — robustness: must still detect the valid predicate.
			name: "where_newline_adjacent_valid_predicate",
			sql: `-- name: GetItem :one
SELECT id, value FROM items
WHERE
user_id = ?
AND id = ?;
`,
			wantViol: false,
		},
		{
			// sqlc named param style (@user_id) — used in the shipped queries.
			name: "select_named_param_user_id",
			sql: `-- name: GetUserData :one
SELECT id, user_id, value, created_at
FROM user_data
WHERE id = @id
  AND user_id = @user_id;
`,
			wantViol: false,
		},
		{
			// DELETE with named param — mirrors the shipped DeleteUserData query.
			name: "delete_named_param_user_id",
			sql: `-- name: DeleteUserData :exec
DELETE FROM user_data
WHERE id = @id
  AND user_id = @user_id;
`,
			wantViol: false,
		},

		// ----------------------------------------------------------------
		// Simple violations — obvious missing predicates.
		// ----------------------------------------------------------------
		{
			name: "select_user_table_missing_user_id",
			sql: `-- name: ListItems :many
SELECT id, user_id, value FROM items
WHERE status = ?
ORDER BY created_at;
`,
			wantViol:  true,
			wantQuery: "ListItems",
		},
		{
			name: "delete_user_table_missing_user_id",
			sql: `-- name: PurgeItem :exec
DELETE FROM items
WHERE id = ?;
`,
			wantViol:  true,
			wantQuery: "PurgeItem",
		},
		{
			name: "update_user_table_missing_user_id",
			sql: `-- name: SetValue :exec
UPDATE items
SET value = ?
WHERE id = ?;
`,
			wantViol:  true,
			wantQuery: "SetValue",
		},

		// ----------------------------------------------------------------
		// MUST-FIX 1 adversarial: JOIN-scoped — user_id only on joined table.
		// The target (items) is unscoped; guard must flag this.
		// ----------------------------------------------------------------
		{
			name: "select_join_scoped_target_unscoped",
			sql: `-- name: ListAuditedItems :many
SELECT items.id FROM items JOIN audit ON items.id = audit.item_id WHERE audit.user_id = ?;
`,
			wantViol:  true,
			wantQuery: "ListAuditedItems",
		},

		// ----------------------------------------------------------------
		// MUST-FIX 1 adversarial: subquery-scoped SELECT.
		// ----------------------------------------------------------------
		{
			name: "select_subquery_scoped_target_unscoped",
			sql: `-- name: ListPermittedItems :many
SELECT id FROM items WHERE id IN (SELECT id FROM other WHERE user_id = ?);
`,
			wantViol:  true,
			wantQuery: "ListPermittedItems",
		},

		// ----------------------------------------------------------------
		// MUST-FIX 1 adversarial: subquery-scoped DELETE.
		// ----------------------------------------------------------------
		{
			name: "delete_subquery_scoped_target_unscoped",
			sql: `-- name: PurgePermitted :exec
DELETE FROM items WHERE id IN (SELECT id FROM perms WHERE user_id = ?);
`,
			wantViol:  true,
			wantQuery: "PurgePermitted",
		},

		// ----------------------------------------------------------------
		// MUST-FIX 1 adversarial: subquery-scoped UPDATE.
		// ----------------------------------------------------------------
		{
			name: "update_subquery_scoped_target_unscoped",
			sql: `-- name: UpdatePermitted :exec
UPDATE items SET value = ? WHERE id IN (SELECT id FROM perms WHERE perms.user_id = ?);
`,
			wantViol:  true,
			wantQuery: "UpdatePermitted",
		},

		// ----------------------------------------------------------------
		// MUST-FIX 1 adversarial: UNION — guard cannot verify scope; fail closed.
		// ----------------------------------------------------------------
		{
			name: "select_union",
			sql: `-- name: UnionItems :many
SELECT id FROM items WHERE user_id = ?
UNION
SELECT id FROM other_items WHERE user_id = ?;
`,
			wantViol:  true,
			wantQuery: "UnionItems",
		},

		// ----------------------------------------------------------------
		// MUST-FIX 1 adversarial: WITH / CTE — fail closed.
		// ----------------------------------------------------------------
		{
			name: "select_with_cte",
			sql: `-- name: CTEItems :many
WITH scoped AS (SELECT id FROM items WHERE user_id = ?)
SELECT id FROM scoped;
`,
			wantViol:  true,
			wantQuery: "CTEItems",
		},

		// ----------------------------------------------------------------
		// MUST-FIX 2 adversarial: DELETE that contains SELECT in subquery —
		// statement type must be DELETE, not SELECT.
		// The outer DELETE target (items) is unscoped → violation.
		// ----------------------------------------------------------------
		{
			name: "delete_misclassified_as_select",
			sql: `-- name: DeleteViaSubquery :exec
DELETE FROM items WHERE id IN (SELECT id FROM perms WHERE user_id = ?);
`,
			wantViol:  true,
			wantQuery: "DeleteViaSubquery",
		},

		// ----------------------------------------------------------------
		// Adversarial: UPDATE with no WHERE at all — violation.
		// ----------------------------------------------------------------
		{
			name: "update_no_where",
			sql: `-- name: BulkUpdate :exec
UPDATE items SET value = ?;
`,
			wantViol:  true,
			wantQuery: "BulkUpdate",
		},

		// ----------------------------------------------------------------
		// Adversarial: DELETE with no WHERE at all — violation.
		// ----------------------------------------------------------------
		{
			name: "delete_no_where",
			sql: `-- name: BulkDelete :exec
DELETE FROM items;
`,
			wantViol:  true,
			wantQuery: "BulkDelete",
		},

		// ----------------------------------------------------------------
		// Adversarial: user_id only in the SELECT column list, not in WHERE.
		// ----------------------------------------------------------------
		{
			name: "user_id_only_in_select_list",
			sql: `-- name: ListItemsWithUID :many
SELECT id, user_id, value FROM items
WHERE status = 'active';
`,
			wantViol:  true,
			wantQuery: "ListItemsWithUID",
		},

		// ----------------------------------------------------------------
		// Adversarial: user_id only inside a SQL comment, not a real predicate.
		// ----------------------------------------------------------------
		{
			name: "user_id_only_in_comment",
			sql: `-- name: GetItem :one
SELECT id, value FROM items
-- filter by user_id here eventually
WHERE id = ?;
`,
			wantViol:  true,
			wantQuery: "GetItem",
		},

		// ----------------------------------------------------------------
		// Adversarial: user_id qualified to a non-target table (alias case).
		// ----------------------------------------------------------------
		{
			name: "user_id_qualified_to_joined_table",
			sql: `-- name: JoinedItems :many
SELECT i.id FROM items i JOIN audit a ON i.id = a.item_id WHERE a.user_id = ?;
`,
			wantViol:  true,
			wantQuery: "JoinedItems",
		},

		// ----------------------------------------------------------------
		// Finding 1: comma cross-join — must fail closed (target unscoped).
		// ----------------------------------------------------------------
		{
			name: "select_comma_join_unscoped_target",
			// Old-style comma cross-join: items,audit. Target (items) has no
			// user_id predicate; the bare user_id resolves to audit.user_id.
			// Guard must reject this just as it rejects a JOIN keyword.
			sql: `-- name: CommaJoin :many
SELECT id FROM items, audit WHERE user_id = ?;
`,
			wantViol:  true,
			wantQuery: "CommaJoin",
		},
		{
			name: "delete_comma_join_unscoped_target",
			sql: `-- name: DeleteCommaJoin :exec
DELETE FROM items WHERE id IN (SELECT id FROM items, perms WHERE user_id = ?);
`,
			wantViol:  true,
			wantQuery: "DeleteCommaJoin",
		},

		// ----------------------------------------------------------------
		// Finding 2: scalar subquery with newline after paren in UPDATE/DELETE.
		// ----------------------------------------------------------------
		{
			name: "update_scalar_subquery_newline",
			// UPDATE with "= (\n  SELECT …)" — hasSubquery must catch this.
			// items is unscoped; user_id only appears in the subquery.
			sql: `-- name: UpdateScalarSub :exec
UPDATE items SET x = (
  SELECT y FROM other WHERE user_id = ?) WHERE id = @id;
`,
			wantViol:  true,
			wantQuery: "UpdateScalarSub",
		},
		{
			name: "delete_scalar_subquery_newline",
			sql: `-- name: DeleteScalarSub :exec
DELETE FROM items WHERE id = (
  SELECT id FROM perms WHERE user_id = ?);
`,
			wantViol:  true,
			wantQuery: "DeleteScalarSub",
		},

		// ----------------------------------------------------------------
		// Finding 3: top-level OR disjunction makes user_id non-constraining.
		// ----------------------------------------------------------------
		{
			name: "select_top_level_or_with_user_id",
			// WHERE id = @id OR user_id = @x — the OR means the predicate
			// returns rows regardless of user ownership; must fail closed.
			sql: `-- name: OrDisjunction :many
SELECT id FROM items WHERE id = @id OR user_id = @user_id;
`,
			wantViol:  true,
			wantQuery: "OrDisjunction",
		},
		{
			name: "select_user_id_bare_token_in_expression",
			// user_id inside COALESCE — not an equality predicate; must flag.
			sql: `-- name: CoalesceUID :many
SELECT id FROM items WHERE COALESCE(user_id, '') = '' AND id = @id;
`,
			wantViol:  true,
			wantQuery: "CoalesceUID",
		},
		{
			// Safe: OR is inside parentheses AND'd with the user_id predicate,
			// so user_id is still required for every returned row.
			name: "select_or_inside_parens_and_with_user_id",
			sql: `-- name: ParenOr :many
SELECT id FROM items
WHERE user_id = @user_id
  AND (status IS NULL OR status = @status);
`,
			wantViol: false,
		},

		// ----------------------------------------------------------------
		// Finding 4: Reason field populated — violation must carry a non-empty
		// Reason so the CI diagnostic is load-bearing, not dropped.
		// ----------------------------------------------------------------
		{
			name: "violation_reason_populated",
			// Any simple violation suffices; we check v.Reason is non-empty.
			sql: `-- name: MissingUID :many
SELECT id FROM items WHERE status = 'active';
`,
			wantViol:       true,
			wantQuery:      "MissingUID",
			wantReasonFill: true,
		},

		// ----------------------------------------------------------------
		// Finding 6: allow-unscoped bypass — fully tested for correctness,
		// off-by-one typo, and proof that the annotation suppressed the flag.
		// ----------------------------------------------------------------
		{
			// (a) With valid annotation: must NOT produce a violation.
			name: "allow_unscoped_annotation_suppresses_join",
			sql: `-- name: ReviewedJoin :many
-- scopeguard:allow-unscoped: reviewed — this JOIN is safe because both tables
-- are independently user-scoped in the application layer.
SELECT items.id FROM items JOIN audit ON items.id = audit.item_id WHERE audit.user_id = ?;
`,
			wantViol: false,
		},
		{
			// (b) Same query WITHOUT annotation: must produce a violation,
			// proving the annotation is what suppressed it.
			name: "allow_unscoped_absent_still_flags_join",
			sql: `-- name: UnannotatedJoin :many
SELECT items.id FROM items JOIN audit ON items.id = audit.item_id WHERE audit.user_id = ?;
`,
			wantViol:  true,
			wantQuery: "UnannotatedJoin",
		},
		{
			// (c) Near-miss/typo annotation: must still flag the violation,
			// locking match precision of the annotation string.
			name: "allow_unscoped_typo_still_flags",
			sql: `-- name: TypoAnnotation :many
-- scopeguard:allow-unscope: typo is missing the trailing 'd'
SELECT items.id FROM items JOIN audit ON items.id = audit.item_id WHERE audit.user_id = ?;
`,
			wantViol:  true,
			wantQuery: "TypoAnnotation",
		},

		// ----------------------------------------------------------------
		// Finding 7: INSERT … RETURNING must not trip the guard.
		// ----------------------------------------------------------------
		{
			name: "insert_returning_no_violation",
			sql: `-- name: CreateItemReturning :one
INSERT INTO items (id, user_id) VALUES (?, ?) RETURNING id, user_id;
`,
			wantViol: false,
		},

		// ----------------------------------------------------------------
		// Literal-masking exploits — string literals must not influence
		// structural detection in hasTopLevelOR, hasCommaJoin, hasSubquery,
		// extractTargetTable, or the user_id equality matcher.
		// ----------------------------------------------------------------
		{
			// MUST-FIX: unbalanced '(' inside a string literal pushes paren
			// depth to 1 so the genuine top-level OR is treated as nested.
			// The query leaks every row with id>0 regardless of user_id.
			name: "literal_paren_masks_top_level_or",
			sql: `-- name: LiteralParenOR :many
SELECT id FROM items WHERE label = '(' AND user_id = @u OR id > 0;
`,
			wantViol:  true,
			wantQuery: "LiteralParenOR",
		},
		{
			// MUST-FIX: 'user_id = 5' inside a string literal must not be
			// accepted as the real user_id equality predicate.
			name: "literal_user_id_accepted_as_predicate",
			sql: `-- name: LiteralUID :many
SELECT id FROM items WHERE note = 'x user_id = 5 y';
`,
			wantViol:  true,
			wantQuery: "LiteralUID",
		},
		{
			// SHOULD-FIX: 'from here' in the SELECT list — the word FROM
			// inside a literal causes extractTargetTable to return 'here''
			// instead of 'items', so the query passes without a user_id check.
			name: "literal_from_in_projection_false_positive",
			sql: `-- name: LiteralFrom :many
SELECT 'from here', id FROM items WHERE user_id = @user_id;
`,
			wantViol: false,
		},
		{
			// SHOULD-FIX: SELECT inside a string literal causes hasSubquery to
			// see a "second SELECT" and wrongly flag a valid single-source UPDATE.
			name: "literal_select_in_update_false_positive",
			sql: `-- name: LiteralSelect :exec
UPDATE items SET note = 'please SELECT one' WHERE user_id = @user_id AND id = @id;
`,
			wantViol: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := fstest.MapFS{
				"fixture.sql": &fstest.MapFile{Data: []byte(tt.sql)},
			}

			violations, err := store.ScanQueryViolations(fsys)
			if err != nil {
				t.Fatalf("ScanQueryViolations: %v", err)
			}

			if tt.wantViol {
				if len(violations) == 0 {
					t.Errorf("expected a violation for query %q but got none", tt.wantQuery)
					return
				}
				var matched *store.Violation
				for i := range violations {
					if violations[i].QueryName == tt.wantQuery {
						matched = &violations[i]
						break
					}
				}
				if matched == nil {
					t.Errorf("expected violation for query %q; got violations: %v", tt.wantQuery, violations)
				} else if tt.wantReasonFill && matched.Reason == "" {
					t.Errorf("violation for query %q has empty Reason; want a non-empty diagnostic string", tt.wantQuery)
				}
			} else if len(violations) > 0 {
				t.Errorf("expected no violations but got %d: %v", len(violations), violations)
			}
		})
	}
}
