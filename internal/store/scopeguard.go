package store

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// systemTables is the allowlist of tables that are system-owned and therefore exempt from the user_id predicate requirement. SELECT/UPDATE/DELETE queries against these tables are allowed without a WHERE user_id clause.
//
// Maintenance: as sibling specs add domain tables, add them to this package's scope guard test (scopeguard_test.go) — but do NOT add them here unless they are genuinely system-owned (i.e. they have no user_id column and are not per-user data). User-owned tables must carry a user_id predicate in every SELECT/UPDATE/DELETE query; the guard is the backstop, not the DB.
//
// Reviewed exemption mechanism: if a query is intentionally complex (e.g.
// a JOIN or subquery that the heuristic cannot verify) and has been reviewed
// for cross-user safety, add the annotation
//
//	-- scopeguard:allow-unscoped: <reason>
//
// to the query block. The guard will skip that block without flagging it. This
// is the only reviewed exception path — do not add complex queries to
// systemTables.
var systemTables = map[string]bool{
	"instance":         true,
	"goose_db_version": true,
	// settings is system-owned (no user_id column) — instance-global operational config that is readable and writable by any authenticated subsystem, not bound to a specific user.
	"settings": true,
}

// Violation describes a query that is missing a user_id predicate.
type Violation struct {
	// File is the path of the SQL file relative to the queries FS root.
	File string
	// QueryName is the sqlc query name from the "-- name: ..." annotation.
	QueryName string
	// Statement is the SQL text of the offending query block.
	Statement string
}

// ScanQueryViolations scans all *.sql files in queriesFS for user-owned table queries that lack a verified user_id predicate on the target table. It returns one Violation per offending query block.
//
// The guard fails closed: any query shape it cannot confidently verify as scoping the target table (JOIN, subquery, UNION, WITH/CTE, missing WHERE) is flagged as a violation. Only simple single-source queries with a top-level WHERE user_id predicate on the target table pass.
//
// Exemptions:
//   - Blocks against tables in systemTables (system-owned, no user_id column).
//   - INSERT blocks (user_id is a column value, not a predicate).
//   - Blocks carrying a "-- scopeguard:allow-unscoped: <reason>" annotation
//     (reviewed exceptions only — see systemTables comment above).
//
// Algorithm (text-scan heuristic, not a full SQL parser):
//  1. Split each file on "-- name: " to obtain individual query blocks.
//  2. Determine the statement type from the leading keyword of the body.
//  3. Extract the target table from the correct clause for that type.
//  4. Skip allowlisted system tables, INSERTs, and allow-unscoped blocks.
//  5. Strip SQL line comments from the block before predicate analysis.
//  6. Fail closed on JOIN, subquery (IN (SELECT …)), UNION, or WITH/CTE.
//  7. Require a top-level WHERE user_id predicate on the target table.
func ScanQueryViolations(queriesFS fs.FS) ([]Violation, error) {
	var violations []Violation

	entries, err := fs.ReadDir(queriesFS, ".")
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}

		data, err := fs.ReadFile(queriesFS, e.Name())
		if err != nil {
			return nil, err
		}

		vs := scanFile(e.Name(), string(data))
		violations = append(violations, vs...)
	}

	return violations, nil
}

// scanFile splits a SQL file into query blocks (on "-- name: ") and checks each block.
func scanFile(filename, content string) []Violation {
	const marker = "-- name: "
	var violations []Violation

	blocks := strings.Split(content, marker)
	// blocks[0] is everything before the first "-- name: " (file-level comments, blank lines). We only process blocks[1:].
	for _, block := range blocks[1:] {
		name := extractQueryName(block)
		stmt := extractStatementType(block)
		table := extractTargetTable(block, stmt)

		if systemTables[strings.ToLower(table)] {
			continue
		}
		if stmt == "INSERT" {
			continue
		}
		// Reviewed exemption: block carries an explicit allow-unscoped annotation.
		if strings.Contains(block, "-- scopeguard:allow-unscoped:") {
			continue
		}
		if stmt == "" {
			// Unknown statement type — skip to avoid false positives on non-DML blocks.
			continue
		}
		// SELECT, UPDATE, DELETE on a user-owned table: verify user_id scoping.
		if msg, ok := verifyUserIDScoping(block, table); !ok {
			_ = msg // msg is included in the Statement for context; violation carries the block
			violations = append(violations, Violation{
				File:      filename,
				QueryName: name,
				Statement: strings.TrimSpace(block),
			})
		}
	}

	return violations
}

// extractQueryName returns the query name from the start of a block (the text following "-- name: " up to the first whitespace or colon).
func extractQueryName(block string) string {
	// block starts with e.g. "GetInstance :one\nSELECT ..."
	name := block
	if i := strings.IndexAny(name, " :\t\n\r"); i >= 0 {
		name = name[:i]
	}
	return name
}

// extractStatementType returns the SQL statement keyword (SELECT, UPDATE, DELETE, INSERT, WITH) from the first keyword of the statement body — the line(s) after the "-- name: ..." annotation. This avoids misclassification when e.g. a DELETE block contains a SELECT subquery.
func extractStatementType(block string) string {
	// Skip the annotation line ("GetFoo :one\n") and find the body.
	body := block
	if nl := strings.IndexByte(block, '\n'); nl >= 0 {
		body = block[nl+1:]
	}
	// Walk lines until we find a non-blank, non-comment line.
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		upper := strings.ToUpper(t)
		for _, kw := range []string{"SELECT", "UPDATE", "DELETE", "INSERT", "WITH"} {
			if strings.HasPrefix(upper, kw) {
				return kw
			}
		}
		// First non-blank non-comment line doesn't start with a known keyword.
		break
	}
	return ""
}

// extractTargetTable extracts the primary table name from a query block based on the statement type. It uses a line-by-line scan that handles both same-line clauses (e.g. "SELECT id FROM items") and multi-line clauses (e.g. "FROM\n  items").
func extractTargetTable(block, stmt string) string {
	lines := strings.Split(block, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)

		switch stmt {
		case "SELECT":
			// Match "FROM <table>" whether at start of line or inline.
			if idx := strings.Index(upper, "FROM "); idx >= 0 {
				return firstWord(trimmed[idx+5:])
			}
		case "UPDATE":
			if strings.HasPrefix(upper, "UPDATE ") {
				return firstWord(trimmed[7:])
			}
		case "DELETE":
			if strings.HasPrefix(upper, "DELETE FROM ") {
				return firstWord(trimmed[12:])
			}
		case "INSERT":
			if strings.HasPrefix(upper, "INSERT INTO ") {
				return firstWord(trimmed[12:])
			}
		case "WITH":
			// CTEs are flagged at the predicate-check stage; no target to extract.
			return ""
		}
	}
	return ""
}

// reLineComment matches a SQL line comment and everything after it on the line.
var reLineComment = regexp.MustCompile(`--[^\n]*`)

// stripLineComments removes SQL line comments (-- ...) from s, preserving newlines so that line-based WHERE detection still works correctly.
func stripLineComments(s string) string {
	return reLineComment.ReplaceAllStringFunc(s, func(_ string) string {
		// Keep a newline placeholder so surrounding line structure is intact.
		return ""
	})
}

// verifyUserIDScoping reports whether the query block has a verified user_id predicate on the target table. It returns (message, true) when the block passes and (message, false) when it should be flagged.
//
// Fail-closed rules (in order):
//  1. WITH/CTE at the statement level → flag (cannot verify target).
//  2. UNION in the stripped body → flag.
//  3. JOIN keyword in the stripped body → flag (user_id could be on the joined table).
//  4. Subquery in the WHERE clause (IN (SELECT …)) → flag.
//  5. No WHERE clause → flag.
//  6. Examine the WHERE clause (after stripping comments):
//     Accept only a bare "user_id" match or one qualified to the target
//     table/alias. A "user_id" that is qualified to any other identifier → flag.
func verifyUserIDScoping(block, target string) (string, bool) {
	// Rule 1: WITH/CTE.
	body := block
	if nl := strings.IndexByte(block, '\n'); nl >= 0 {
		body = block[nl+1:]
	}
	upperBody := strings.ToUpper(strings.TrimSpace(body))
	if strings.HasPrefix(upperBody, "WITH ") || upperBody == "WITH" {
		return "cannot verify user_id scoping of target " + target + "; WITH/CTE queries must be simplified or exempted", false
	}

	// Strip line comments before all remaining checks so that
	//   -- filter by user_id here
	// does not count as a predicate.
	stripped := stripLineComments(block)
	upperStripped := strings.ToUpper(stripped)

	// Rule 2: UNION.
	if strings.Contains(upperStripped, " UNION ") || strings.Contains(upperStripped, "\nUNION\n") ||
		strings.Contains(upperStripped, "\nUNION ") || strings.Contains(upperStripped, " UNION\n") {
		return "cannot verify user_id scoping of target " + target + "; UNION queries must be simplified or exempted", false
	}

	// Rule 3: JOIN.
	if containsWholeWordUpper(upperStripped, "JOIN") {
		return "cannot verify user_id scoping of target " + target + "; JOIN queries must be simplified or exempted", false
	}

	// Rule 4: subquery in the text (a SELECT inside the block other than at the top-level SELECT position).
	if hasSubquery(stripped) {
		return "cannot verify user_id scoping of target " + target + "; subquery/IN(SELECT) must be simplified or exempted", false
	}

	// Rule 5: no WHERE clause.
	whereIdx := findWhereClause(upperStripped)
	if whereIdx < 0 {
		return "no WHERE clause; user_id predicate required for target " + target, false
	}

	// Rule 6: examine the WHERE clause portion.
	whereClause := stripped[whereIdx:]
	return checkUserIDInWhere(whereClause, target)
}

// containsWholeWordUpper reports whether upper (already uppercased) contains the keyword kw as a whole word (not a substring of another identifier).
func containsWholeWordUpper(upper, kw string) bool {
	idx := 0
	for {
		pos := strings.Index(upper[idx:], kw)
		if pos < 0 {
			return false
		}
		abs := idx + pos
		before := abs == 0 || !isIdentChar(rune(upper[abs-1]))
		after := abs+len(kw) >= len(upper) || !isIdentChar(rune(upper[abs+len(kw)]))
		if before && after {
			return true
		}
		idx = abs + 1
	}
}

// isIdentChar reports whether r is a valid SQL identifier character.
func isIdentChar(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
		(r >= '0' && r <= '9') || r == '_'
}

// hasSubquery reports whether stripped contains a subquery — detected by the patterns IN(SELECT, IN (SELECT, EXISTS(SELECT, EXISTS (SELECT, or =(SELECT, which are the only ways a correlated subquery appears in a WHERE clause in Qovira's query style.
//
// For SELECT statements a second SELECT also signals a subquery (e.g. "SELECT ... FROM (SELECT ...)"), but the WHERE-clause patterns above cover all the cases we care about for INSERT/UPDATE/DELETE as well.
func hasSubquery(stripped string) bool {
	upper := strings.ToUpper(stripped)
	// Common subquery introduction patterns.
	for _, pat := range []string{
		"IN(SELECT", "IN (SELECT", "IN(\nSELECT", "IN (\nSELECT",
		"EXISTS(SELECT", "EXISTS (SELECT", "EXISTS(\nSELECT", "EXISTS (\nSELECT",
		"=(SELECT", "= (SELECT",
	} {
		if strings.Contains(upper, pat) {
			return true
		}
	}
	// For SELECT statements, a second SELECT means a derived-table subquery.
	first := strings.Index(upper, "SELECT")
	if first >= 0 {
		rest := upper[first+6:]
		if strings.Contains(rest, "SELECT") {
			return true
		}
	}
	return false
}

// findWhereClause locates the WHERE keyword in the uppercased block text as a whole-word match, tolerating newlines and tabs as surrounding whitespace. Returns the byte offset into the original (non-uppercased) stripped text, or -1 if not found.
func findWhereClause(upper string) int {
	const kw = "WHERE"
	idx := 0
	for {
		pos := strings.Index(upper[idx:], kw)
		if pos < 0 {
			return -1
		}
		abs := idx + pos
		// Check that WHERE is not an embedded substring.
		before := abs == 0 || isWhitespaceOrPunct(rune(upper[abs-1]))
		after := abs+len(kw) >= len(upper) || isWhitespaceOrPunct(rune(upper[abs+len(kw)]))
		if before && after {
			return abs
		}
		idx = abs + 1
	}
}

// isWhitespaceOrPunct reports whether r is a whitespace character or a punctuation character that can legally precede or follow a keyword.
func isWhitespaceOrPunct(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '(' || r == ')'
}

// reUserIDToken matches a user_id occurrence in the WHERE clause text.
// Groups:
//
//	[1] optional qualifier (table/alias name before the dot)
//	[2] "user_id" itself
var reUserIDToken = regexp.MustCompile(`(?i)(?:([a-zA-Z_][a-zA-Z0-9_]*)\.)?(\buser_id\b)`)

// checkUserIDInWhere examines the WHERE clause text (still with original case, comments already stripped) for a user_id predicate on the target table.
//
// Accepts:
//   - bare "user_id = ..." (no qualifier) → target must be the only table
//   - "<target>.user_id = ..." → explicit target qualification
//
// Rejects:
//   - "<other>.user_id = ..." → qualified to a non-target table
//   - no "user_id" at all → missing predicate
func checkUserIDInWhere(whereClause, target string) (string, bool) {
	matches := reUserIDToken.FindAllStringSubmatch(whereClause, -1)
	if len(matches) == 0 {
		return "no user_id predicate found in WHERE clause for target " + target, false
	}

	targetLower := strings.ToLower(target)
	for _, m := range matches {
		qualifier := strings.ToLower(m[1]) // may be empty
		if qualifier == "" {
			// Bare user_id — acceptable for a single-source query.
			return "", true
		}
		if qualifier == targetLower {
			// Explicitly qualified to the target table.
			return "", true
		}
		// Qualified to something else — another table or alias: reject.
	}

	// Every user_id occurrence was qualified to a non-target table.
	return "user_id predicate is scoped to a non-target table in WHERE clause; target " + target + " is unscoped", false
}

// firstWord returns the first whitespace-delimited token from s.
func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t\n\r("); i >= 0 {
		return s[:i]
	}
	return s
}
