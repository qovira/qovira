package store

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// systemTables is the allowlist of tables that are system-owned and therefore exempt from the user_id predicate
// requirement. SELECT/UPDATE/DELETE queries against these tables are allowed without a WHERE user_id clause.
//
// Maintenance: as sibling specs add domain tables, add them to this package's scope guard test (scopeguard_test.go) —
// but do NOT add them here unless they are genuinely system-owned (i.e. they have no user_id column and are not
// per-user data). User-owned tables must carry a user_id predicate in every SELECT/UPDATE/DELETE query; the guard is
// the backstop, not the DB.
//
// Reviewed exemption mechanism: if a query is intentionally complex (e.g. a JOIN or subquery that the heuristic cannot
// verify) and has been reviewed for cross-user safety, add the annotation
//
//	-- scopeguard:allow-unscoped: <reason>
//
// to the query block. The guard will skip that block without flagging it. This is the only reviewed exception path — do
// not add complex queries to systemTables.
var systemTables = map[string]bool{
	"instance":         true,
	"goose_db_version": true,
	// settings is system-owned (no user_id column) — instance-global operational config that is readable and writable
	// by any authenticated subsystem, not bound to a specific user.
	"settings": true,
	// users is system-owned (no user_id column) — it is the identity table from which per-user scope is derived.
	// Every user IS a row in this table, so it cannot itself be scoped by user_id; the system layer (auth.Service)
	// owns reads and writes to this table directly.
	"users": true,
}

// Violation describes a query that is missing a user_id predicate.
type Violation struct {
	// File is the path of the SQL file relative to the queries FS root.
	File string
	// QueryName is the sqlc query name from the "-- name: ..." annotation.
	QueryName string
	// Statement is the SQL text of the offending query block.
	Statement string
	// Reason is the diagnostic message from the scope guard explaining why this query was flagged (e.g. "no WHERE
	// clause", "JOIN queries must be simplified or exempted"). It is always non-empty for a real violation.
	Reason string
}

// ScanQueryViolations scans all *.sql files in queriesFS for user-owned table queries that lack a verified user_id
// predicate on the target table. It returns one Violation per offending query block.
//
// The guard fails closed: any query shape it cannot confidently verify as scoping the target table (JOIN, subquery,
// UNION, WITH/CTE, missing WHERE) is flagged as a violation. Only simple single-source queries with a top-level WHERE
// user_id predicate on the target table pass.
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
	// blocks[0] is everything before the first "-- name: " (file-level comments, blank lines). We only process
	// blocks[1:].
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
			violations = append(violations, Violation{
				File:      filename,
				QueryName: name,
				Statement: strings.TrimSpace(block),
				Reason:    msg,
			})
		}
	}

	return violations
}

// extractQueryName returns the query name from the start of a block (the text following "-- name: " up to the first
// whitespace or colon).
func extractQueryName(block string) string {
	// block starts with e.g. "GetInstance :one\nSELECT ..."
	name := block
	if i := strings.IndexAny(name, " :\t\n\r"); i >= 0 {
		name = name[:i]
	}
	return name
}

// extractStatementType returns the SQL statement keyword (SELECT, UPDATE, DELETE, INSERT, WITH) from the first keyword
// of the statement body — the line(s) after the "-- name: ..." annotation. This avoids misclassification when e.g. a
// DELETE block contains a SELECT subquery.
func extractStatementType(block string) string {
	// Skip the annotation line ("GetFoo :one\n") and find the body.
	body := block
	if _, after, ok := strings.Cut(block, "\n"); ok {
		body = after
	}
	// Walk lines until we find a non-blank, non-comment line.
	for line := range strings.SplitSeq(body, "\n") {
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

// extractTargetTable extracts the primary table name from a query block based on the statement type. It uses a
// line-by-line scan that handles both same-line clauses (e.g. "SELECT id FROM items") and multi-line clauses (e.g.
// "FROM\n  items").
//
// String literals are stripped before keyword matching so that FROM/UPDATE/DELETE/INSERT inside a literal value (e.g.
// SELECT 'from here', id FROM items …) cannot cause a false table-name extraction.
func extractTargetTable(block, stmt string) string {
	// Normalize: strip comments then literals so structural keywords in literal
	// values cannot mislead the FROM/UPDATE/DELETE/INSERT keyword scan.
	normalized := stripStringLiterals(stripLineComments(block))
	lines := strings.SplitSeq(normalized, "\n")
	for line := range lines {
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

// stripLineComments removes SQL line comments (-- ...) from s. The regex [^\n]* deliberately excludes the newline
// character, so the newline that terminates each comment line is left in place untouched — line structure is preserved
// without any placeholder.
func stripLineComments(s string) string {
	return reLineComment.ReplaceAllString(s, "")
}

// stripStringLiterals replaces the content of every single-quoted SQL string literal in s with spaces, so that
// structural keywords (FROM, SELECT, OR, …), punctuation ('(', ','), and predicates (user_id =) inside a literal value
// cannot influence the guard's pattern-matching heuristics.
//
// The ” (two consecutive single-quotes) SQL escape for a literal single-quote is handled correctly — it is consumed as
// an in-literal escape rather than treated as the end of the literal.
//
// This must be applied after stripLineComments so that an unclosed literal started inside a -- comment (e.g.
// `-- don't`) does not consume real SQL that follows on subsequent lines.
//
// Length is not preserved; only the characters inside the quotes are replaced. The surrounding quote characters are
// kept so that quote-counting remains consistent for any caller that inspects them.
func stripStringLiterals(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inLit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inLit {
			b.WriteByte(c)
			if c == '\'' {
				inLit = true
			}
			continue
		}
		// Inside a string literal.
		if c == '\'' {
			// Check for '' escape (two consecutive single-quotes = one literal quote).
			if i+1 < len(s) && s[i+1] == '\'' {
				// Write two spaces in place of the '' escape; advance past both.
				b.WriteByte(' ')
				b.WriteByte(' ')
				i++
				continue
			}
			// Closing quote — end of literal; emit the closing quote.
			b.WriteByte(c)
			inLit = false
			continue
		}
		// Replace the literal character with a space so structural characters
		// (parentheses, commas, keywords) cannot leak out.
		b.WriteByte(' ')
	}
	return b.String()
}

// verifyUserIDScoping reports whether the query block has a verified user_id predicate on the target table. It returns
// (message, true) when the block passes and (message, false) when it should be flagged.
//
// Fail-closed rules (in order):
//  1. WITH/CTE at the statement level → flag (cannot verify target).
//  2. UNION in the stripped body → flag.
//  3. JOIN keyword in the stripped body → flag (user_id could be on the joined table).
//  4. Subquery in the WHERE clause (IN (SELECT …)) → flag.
//  5. No WHERE clause → flag.
//  6. Examine the WHERE clause (after stripping comments):
//     Accept only a bare "user_id" match or one qualified to the target table/alias. A "user_id" that is qualified to
//     any other identifier → flag.
func verifyUserIDScoping(block, target string) (string, bool) {
	// Rule 1: WITH/CTE.
	body := block
	if _, after, ok := strings.Cut(block, "\n"); ok {
		body = after
	}
	upperBody := strings.ToUpper(strings.TrimSpace(body))
	if strings.HasPrefix(upperBody, "WITH ") || upperBody == "WITH" {
		return "cannot verify user_id scoping of target " + target +
			"; WITH/CTE queries must be simplified or exempted", false
	}

	// Strip line comments then string literals before all remaining checks. Comment stripping first ensures that an
	// unclosed quote inside a -- comment (e.g. `-- don't`) does not consume real SQL on subsequent lines. Literal
	// stripping then ensures that structural keywords and punctuation inside a quoted value (parentheses, commas, OR,
	// SELECT, FROM, user_id =) cannot influence pattern detection.
	stripped := stripStringLiterals(stripLineComments(block))
	upperStripped := strings.ToUpper(stripped)

	// Rule 2: UNION.
	if strings.Contains(upperStripped, " UNION ") || strings.Contains(upperStripped, "\nUNION\n") ||
		strings.Contains(upperStripped, "\nUNION ") || strings.Contains(upperStripped, " UNION\n") {
		return "cannot verify user_id scoping of target " + target + "; UNION queries must be simplified or exempted", false
	}

	// Rule 3: JOIN — including old-style comma cross-join (a top-level comma between
	// FROM and WHERE indicates more than one source table in the FROM clause).
	if containsWholeWordUpper(upperStripped, "JOIN") {
		return "cannot verify user_id scoping of target " + target + "; JOIN queries must be simplified or exempted", false
	}
	if hasCommaJoin(upperStripped) {
		return "cannot verify user_id scoping of target " + target +
			"; comma cross-join (old-style JOIN) must be simplified or exempted", false
	}

	// Rule 4: subquery in the text (a SELECT inside the block other than at the top-level SELECT position).
	if hasSubquery(stripped) {
		return "cannot verify user_id scoping of target " + target +
			"; subquery/IN(SELECT) must be simplified or exempted", false
	}

	// Rule 5: no WHERE clause.
	whereIdx := findWhereClause(upperStripped)
	if whereIdx < 0 {
		return "no WHERE clause; user_id predicate required for target " + target, false
	}

	// Rule 6: examine the WHERE clause portion (literals already stripped).
	whereClause := stripped[whereIdx:]
	return checkUserIDInWhere(whereClause, target)
}

// containsWholeWordUpper reports whether upper (already uppercased) contains the keyword kw as a whole word (not a
// substring of another identifier).
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

// hasSubquery reports whether stripped contains a subquery — detected by the patterns IN(SELECT, IN (SELECT,
// EXISTS(SELECT, EXISTS (SELECT, =(SELECT, or = (SELECT (and their newline-after-paren variants), which are the ways a
// subquery appears in Qovira's query style.
//
// For SELECT statements a second SELECT also signals a subquery (e.g. "SELECT ... FROM (SELECT ...)"). For
// UPDATE/DELETE statements there is no leading SELECT, so any SELECT found anywhere in the body is by definition a
// nested subquery and is caught the same way.
func hasSubquery(stripped string) bool {
	upper := strings.ToUpper(stripped)
	// Common subquery introduction patterns, including newline-after-paren variants.
	for _, pat := range []string{
		"IN(SELECT", "IN (SELECT", "IN(\nSELECT", "IN (\nSELECT",
		"EXISTS(SELECT", "EXISTS (SELECT", "EXISTS(\nSELECT", "EXISTS (\nSELECT",
		"=(SELECT", "= (SELECT", "=(\nSELECT", "= (\nSELECT",
	} {
		if strings.Contains(upper, pat) {
			return true
		}
	}
	// Any SELECT in the body signals a subquery. For a top-level SELECT
	// statement the first SELECT is the statement itself, so we need a second
	// one. For UPDATE/DELETE there is no leading SELECT, so the first one found
	// is already a nested subquery.
	first, after, hasFirst := strings.Cut(upper, "SELECT")
	if hasFirst {
		// If the text before "SELECT" contains UPDATE or DELETE (whole-word),
		// this SELECT is nested — it is not the top-level statement keyword.
		firstUpper := strings.ToUpper(first)
		if containsWholeWordUpper(firstUpper, "UPDATE") || containsWholeWordUpper(firstUpper, "DELETE") {
			return true
		}
		// Otherwise this is the top-level SELECT; a second SELECT is a subquery.
		if strings.Contains(after, "SELECT") {
			return true
		}
	}
	return false
}

// findWhereClause locates the WHERE keyword in the uppercased block text as a whole-word match, tolerating newlines and
// tabs as surrounding whitespace. Returns the byte offset into the original (non-uppercased) stripped text, or -1 if
// not found.
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

// isWhitespaceOrPunct reports whether r is a whitespace character or a punctuation character that can legally precede
// or follow a keyword.
func isWhitespaceOrPunct(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '(' || r == ')'
}

// reUserIDEquality matches a user_id equality predicate in the WHERE clause text. The predicate must be of the form
// "user_id =" or "<qualifier>.user_id =", ensuring it is an equality constraint and not a bare token inside a function
// call or other expression.
//
// Groups:
//
//	[1] optional qualifier (table/alias name before the dot)
//	[2] "user_id" itself
var reUserIDEquality = regexp.MustCompile(`(?i)(?:([a-zA-Z_][a-zA-Z0-9_]*)\.)?(\buser_id\b)\s*=`)

// checkUserIDInWhere examines the WHERE clause text (still with original case, comments already stripped) for a user_id
// equality predicate on the target table. It fails closed on two additional structural properties:
//
//  1. Top-level OR — an OR at paren-depth 0 in the WHERE clause makes the predicate non-constraining (rows are
//     returned regardless of user_id), so we fail closed. OR inside parentheses, e.g. "AND (x IS NULL OR x = @y)", is
//     safe because the outer AND still requires user_id.
//
//  2. Equality form — the match must be "user_id =" not a bare "user_id" appearing inside COALESCE(...) or any other
//     non-constraining expression.
//
// Accepts:
//   - "user_id = ..." (bare equality, no qualifier) → target is single-source
//   - "<target>.user_id = ..." → explicit target qualification
//
// Rejects:
//   - top-level OR alongside the user_id predicate
//   - "user_id" in any non-equality position (e.g. COALESCE(user_id,”)=”)
//   - "<other>.user_id = ..." → qualified to a non-target table
//   - no user_id equality predicate at all
func checkUserIDInWhere(whereClause, target string) (string, bool) {
	// Rule: top-level OR makes user_id non-constraining — fail closed.
	if hasTopLevelOR(whereClause) {
		return "WHERE clause contains a top-level OR; user_id predicate is not guaranteed " +
			"to constrain every returned row for target " + target, false
	}

	matches := reUserIDEquality.FindAllStringSubmatch(whereClause, -1)
	if len(matches) == 0 {
		return "no user_id equality predicate (user_id = ...) found in WHERE clause for target " + target, false
	}

	targetLower := strings.ToLower(target)
	for _, m := range matches {
		qualifier := strings.ToLower(m[1]) // may be empty
		if qualifier == "" {
			// Bare user_id = ... — acceptable for a single-source query.
			return "", true
		}
		if qualifier == targetLower {
			// Explicitly qualified to the target table.
			return "", true
		}
		// Qualified to something else — another table or alias: reject.
	}

	// Every user_id equality was qualified to a non-target table.
	return "user_id predicate is scoped to a non-target table in WHERE clause; target " + target + " is unscoped", false
}

// hasTopLevelOR reports whether the WHERE clause text contains an OR keyword at paren-depth 0 (i.e. not nested inside
// parentheses). An OR at depth 0 means the predicate can return rows without the user_id constraint being satisfied, so
// the guard fails closed when one is present.
func hasTopLevelOR(whereClause string) bool {
	upper := strings.ToUpper(whereClause)
	depth := 0
	i := 0
	for i < len(upper) {
		switch upper[i] {
		case '(':
			depth++
			i++
		case ')':
			if depth > 0 {
				depth--
			}
			i++
		case 'O':
			if depth == 0 && strings.HasPrefix(upper[i:], "OR") {
				// Confirm whole-word: character after "OR" must not be an ident char.
				afterOR := i + 2
				before := i == 0 || !isIdentChar(rune(upper[i-1]))
				after := afterOR >= len(upper) || !isIdentChar(rune(upper[afterOR]))
				if before && after {
					return true
				}
			}
			i++
		default:
			i++
		}
	}
	return false
}

// firstWord returns the first whitespace-delimited token from s, with any trailing SQL punctuation (comma, semicolon,
// closing paren) stripped. This ensures that a token like "items," from "FROM items, audit" does not defeat a
// systemTables lookup.
func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t\n\r("); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, ",;)")
}

// hasCommaJoin reports whether the uppercased stripped query text contains an old-style comma cross-join — a top-level
// comma that appears between the FROM clause and the WHERE clause (or end of statement). A comma inside parentheses
// (e.g. a function call or subquery) is not a join comma, so we track paren depth and only flag commas at depth zero.
func hasCommaJoin(upper string) bool {
	// Locate the FROM keyword (whole-word).
	fromIdx := -1
	idx := 0
	for {
		pos := strings.Index(upper[idx:], "FROM")
		if pos < 0 {
			break
		}
		abs := idx + pos
		before := abs == 0 || !isIdentChar(rune(upper[abs-1]))
		after := abs+4 >= len(upper) || !isIdentChar(rune(upper[abs+4]))
		if before && after {
			fromIdx = abs + 4 // character after "FROM"
			break
		}
		idx = abs + 1
	}
	if fromIdx < 0 {
		return false
	}

	// Scan from after FROM to the first top-level WHERE (or end of string),
	// tracking paren depth. A comma at depth 0 signals a comma join.
	depth := 0
	for i := fromIdx; i < len(upper); i++ {
		c := upper[i]
		switch c {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return true
			}
		case 'W':
			// Check for whole-word WHERE at depth 0.
			if depth == 0 && strings.HasPrefix(upper[i:], "WHERE") {
				after := i+5 >= len(upper) || !isIdentChar(rune(upper[i+5]))
				before := i == 0 || !isIdentChar(rune(upper[i-1]))
				if before && after {
					return false
				}
			}
		}
	}
	return false
}
