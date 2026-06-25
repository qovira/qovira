package store_test

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qovira/qovira/internal/store"
)

// FuzzScanQueryViolations checks that the scope guard never panics on arbitrary SQL file content. The guard is a
// hand-rolled text scanner built on raw byte-index slicing (clause offsets, keyword-boundary lookarounds, paren-depth
// walks); malformed or adversarial input must be analyzed without crashing, because a panic here would take down
// whatever loads the query set.
func FuzzScanQueryViolations(f *testing.F) {
	f.Add("-- name: ListItems :many\nSELECT id FROM items WHERE user_id = ?;\n")
	f.Add("-- name: Bad :many\nSELECT id FROM items;\n")
	f.Add("")
	f.Add("-- name: ")
	f.Add("WITH")
	f.Add("-- name: X :one\nUPDATE items SET v = (\nSELECT 1) WHERE id = ?;\n")
	f.Add("-- name: L :many\nSELECT 'from here', id FROM items WHERE user_id = @user_id;\n")

	f.Fuzz(func(t *testing.T, content string) {
		fsys := fstest.MapFS{"fuzz.sql": &fstest.MapFile{Data: []byte(content)}}
		// Contract: the guard analyzes any content without panicking. (MapFS never returns an I/O error here, so a
		// non-nil error would itself be a surprise worth surfacing.)
		if _, err := store.ScanQueryViolations(fsys); err != nil {
			t.Fatalf("ScanQueryViolations returned an error on in-memory content: %v", err)
		}
	})
}

// reUserIDToken matches the user_id token in any case; used to strip it from fuzz input so the constructed query is
// provably free of any user_id predicate.
var reUserIDToken = regexp.MustCompile(`(?i)user_id`)

// FuzzScopeGuardNoUserIDMustFlag is a sound false-negative oracle for the tenant-isolation guard. It assembles a SELECT
// against the user-owned `items` table whose WHERE predicate is fuzzer-controlled but provably contains no `user_id`
// token — every case-insensitive occurrence is stripped to a fixpoint (a single non-overlapping pass can re-form the
// token, so we iterate). Such a query can never be legitimately user-scoped, so the guard MUST report at least one
// violation. A pass would be a genuine cross-user data-leak escape — exactly the failure the guard exists to prevent.
func FuzzScopeGuardNoUserIDMustFlag(f *testing.F) {
	f.Add("id = ?")
	f.Add("status = 'active' OR id > 0")
	f.Add("label = '(' AND x = @u")
	f.Add("id IN (SELECT id FROM other WHERE x = ?)")
	f.Add("")

	f.Fuzz(func(t *testing.T, predicate string) {
		// Strip every user_id token to a fixpoint so the predicate provably lacks the one token the guard's equality
		// matcher requires.
		pred := predicate
		for {
			stripped := reUserIDToken.ReplaceAllString(pred, "")
			if stripped == pred {
				break
			}
			pred = stripped
		}
		// Defensive guards that would invalidate the ground truth: a surviving token (shouldn't happen after the
		// fixpoint loop) or an injected allow-unscoped annotation that would legitimately suppress the FuzzQuery block.
		if reUserIDToken.MatchString(pred) || strings.Contains(pred, "scopeguard:allow-unscoped:") {
			t.Skip()
		}

		sql := "-- name: FuzzQuery :many\nSELECT id FROM items WHERE " + pred + ";\n"
		fsys := fstest.MapFS{"fuzz.sql": &fstest.MapFile{Data: []byte(sql)}}

		violations, err := store.ScanQueryViolations(fsys)
		if err != nil {
			t.Fatalf("ScanQueryViolations: %v", err)
		}
		if len(violations) == 0 {
			t.Errorf("scope guard PASSED a SELECT on user-owned `items` with no user_id predicate (false negative):\n%s", sql)
		}
	})
}
