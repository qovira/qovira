package store_test

// TestListMessages_OrderingTiebreaker verifies that ListMessages returns messages in strict insertion (id/ULID) order
// even when two rows share the same created_at timestamp. This guards against the ORDER BY created_at tiebreaker bug:
// without a secondary ORDER BY id, SQLite's sort order among ties is undefined, so an assistant-with-tool_calls message
// and its following tool-result message could be returned in swapped order, corrupting the OpenAI-shaped message
// sequence fed back to the model.
//
// The test inserts rows with identical created_at values and out-of-id-order timing to make the tiebreaker observable.
// Because ULID is monotonically strictly increasing by id.New(), we insert the "later" message first in terms of
// wall-clock time but with a lexicographically-smaller synthetic ID, then verify the query returns them sorted by id,
// not by insertion time.
//
// To make the same-millisecond scenario deterministic we insert both rows via direct SQL with a fixed created_at string
// so the tie is guaranteed.
import (
	"context"
	"testing"

	"github.com/qovira/qovira/internal/store"
)

func TestListMessages_OrderingTiebreaker(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	// Use a fixed user and conversation so the foreign-key constraints are met. We bypass ScopedQueries and write
	// directly so we can inject an identical created_at for both rows, forcing the tie.
	const userID = "user-ordering-test"
	const convID = "conv-ordering-test"

	// Insert user row (required by FK on messages.user_id if enforced at the Go layer; the schema has no explicit FK
	// here but user_id is denormalised). We still need conversations.user_id to match — insert the conversation too.
	_, err := s.Writer().ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, password_hash, role, timezone, locale, language, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'), strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		userID, userID+"@test.example", "Test", "hash", "member", "UTC", "en-US", "en",
	)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	_, err = s.Writer().ExecContext(ctx,
		`INSERT INTO conversations (id, user_id, created_at, updated_at)
         VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'), strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		convID, userID,
	)
	if err != nil {
		t.Fatalf("insert test conversation: %v", err)
	}

	// We use a fixed created_at so both rows are guaranteed to share a timestamp. IDs are chosen so that idFirst <
	// idSecond lexicographically (ULIDs sort by prefix so we use synthetic values that are obviously ordered).
	const fixedCreatedAt = "2025-01-01T00:00:00.000Z"
	// idFirst must come first in the result (lexicographically smaller id). idSecond must come second
	// (lexicographically larger id).
	//
	// We deliberately insert idSecond (the LARGER id) first, then idFirst (the SMALLER id) second. This means:
	//   - SQLite's internal rowid order (physical insertion) → idSecond, idFirst (WRONG)
	//   - ORDER BY created_at alone (both equal) → undefined, typically rowid → WRONG
	//   - ORDER BY created_at, id → idFirst, idSecond (CORRECT)
	//
	// This models the assistant-then-tool scenario: the assistant message has the smaller ULID (was inserted first),
	// the tool-result has the larger ULID (was inserted after). We stress-test by physically inserting the tool-result
	// row first (as if a race reversed insertion timing), so only the id tiebreaker can produce the correct order.
	const idFirst = "01AAAAAAAAAAAAAAAAAAAAAA01"  // lexicographically smaller — must come first
	const idSecond = "01AAAAAAAAAAAAAAAAAAAAAA02" // lexicographically larger — must come second

	// Insert idSecond (larger id, "tool result") FIRST physically — reversed order. Without ORDER BY id tiebreaker,
	// SQLite may return this one first.
	_, err = s.Writer().ExecContext(ctx,
		`INSERT INTO messages (id, conversation_id, user_id, role, content, created_at)
         VALUES (?, ?, ?, 'tool', 'tool result', ?)`,
		idSecond, convID, userID, fixedCreatedAt,
	)
	if err != nil {
		t.Fatalf("insert idSecond: %v", err)
	}
	// Insert idFirst (smaller id, "assistant msg") SECOND physically. Without ORDER BY id tiebreaker, SQLite rowid
	// order would put idSecond first.
	_, err = s.Writer().ExecContext(ctx,
		`INSERT INTO messages (id, conversation_id, user_id, role, content, created_at)
         VALUES (?, ?, ?, 'assistant', 'assistant msg', ?)`,
		idFirst, convID, userID, fixedCreatedAt,
	)
	if err != nil {
		t.Fatalf("insert idFirst: %v", err)
	}

	// Now query via the ScopedQueries path (the real production code path).
	p := store.Principal{UserID: userID, Role: "member"}
	sq := s.ForUser(store.UserScope(p))
	msgs, err := sq.ListMessages(ctx, convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("ListMessages returned %d messages, want 2", len(msgs))
	}

	// The row with the lexicographically-smaller id must be first.
	if msgs[0].ID != idFirst {
		t.Errorf("msgs[0].ID = %q, want %q — ORDER BY tiebreaker is wrong", msgs[0].ID, idFirst)
	}
	if msgs[1].ID != idSecond {
		t.Errorf("msgs[1].ID = %q, want %q — ORDER BY tiebreaker is wrong", msgs[1].ID, idSecond)
	}
}
