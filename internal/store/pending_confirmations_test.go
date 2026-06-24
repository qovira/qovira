package store_test

// Tests for pending_confirmations behaviour: CAS branch discrimination and
// cross-user isolation of the scope-bypass methods.
//
// These are integration tests against a real migrated SQLCipher store.  They
// characterise existing contract (B.1) and prove isolation semantics (B.2) for
// the system-housekeeping bypass methods.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// seedUser inserts a minimal users row so foreign-key constraints on
// conversations and messages are satisfied.
func seedUser(t *testing.T, s *store.Store, userID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.Writer().ExecContext(context.Background(),
		`INSERT OR IGNORE INTO users
		   (id, email, display_name, password_hash, role, timezone, locale, language, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, userID+"@test.example", "Test User",
		"$argon2id$v=19$m=65536,t=3,p=2$fakesalt$fakehash",
		"member", "UTC", "en", "en", now, now,
	)
	if err != nil {
		t.Fatalf("seedUser %q: %v", userID, err)
	}
}

// seedConversation inserts a minimal conversations row.
func seedConversation(t *testing.T, s *store.Store, convID, userID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.Writer().ExecContext(context.Background(),
		`INSERT OR IGNORE INTO conversations (id, user_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?)`,
		convID, userID, now, now,
	)
	if err != nil {
		t.Fatalf("seedConversation %q: %v", convID, err)
	}
}

// seedMessage inserts a minimal messages row. Returns the inserted ID.
func seedMessage(t *testing.T, s *store.Store, msgID, convID, userID, role string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.Writer().ExecContext(context.Background(),
		`INSERT OR IGNORE INTO messages (id, conversation_id, user_id, role, content, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		msgID, convID, userID, role, "test content", now,
	)
	if err != nil {
		t.Fatalf("seedMessage %q: %v", msgID, err)
	}
}

// seedConfirmation inserts a pending_confirmations row with the given status
// and expires_at value.  Returns the inserted row.
func seedConfirmation(
	t *testing.T,
	s *store.Store,
	callID, convID, msgID, userID, status, expiresAt string,
) {
	t.Helper()
	_, err := s.Writer().ExecContext(context.Background(),
		`INSERT INTO pending_confirmations
		   (id, conversation_id, message_id, user_id, tool_name, args, risk, status, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		callID, convID, msgID, userID, "test_tool", "{}", "low", status, expiresAt,
	)
	if err != nil {
		t.Fatalf("seedConfirmation %q: %v", callID, err)
	}
}

// futureTime returns an RFC 3339 UTC timestamp that is definitely in the future.
func futureTime() string {
	return time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
}

// pastTime returns an RFC 3339 UTC timestamp that is definitely in the past.
func pastTime() string {
	return time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
}

// now returns the current time as RFC 3339 UTC.
func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// ── B.1: CAS branch discrimination ───────────────────────────────────────────

// TestUpdatePendingConfirmationStatusIfCurrent_Branches characterises the four
// outcomes of UpdatePendingConfirmationStatusIfCurrent against distinct row states.
//
// The method must return, via errors.Is:
//   - nil:                           row is pending and not yet expired
//   - ErrConfirmationNotFound:       no row exists for this callID
//   - ErrConfirmationAlreadyResolved: row exists but status != "pending"
//   - ErrConfirmationExpired:        row is pending but expires_at < now
func TestUpdatePendingConfirmationStatusIfCurrent_Branches(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()
	const userID = "user-cas-test"
	const convID = "conv-cas-test"
	const msgID = "msg-cas-test"

	seedUser(t, s, userID)
	seedConversation(t, s, convID, userID)
	seedMessage(t, s, msgID, convID, userID, "assistant")

	sq := s.ForUser(store.UserScope(store.Principal{UserID: userID, Role: "member"}))

	t.Run("pending_and_current_returns_nil", func(t *testing.T) {
		callID := "call-cas-pending-01"
		seedConfirmation(t, s, callID, convID, msgID, userID, "pending", futureTime())

		err := sq.UpdatePendingConfirmationStatusIfCurrent(ctx, callID, "approved", now())
		if err != nil {
			t.Errorf("expected nil for pending+current row; got: %v", err)
		}
	})

	t.Run("no_row_returns_ErrConfirmationNotFound", func(t *testing.T) {
		err := sq.UpdatePendingConfirmationStatusIfCurrent(ctx, "call-nonexistent-xx", "approved", now())
		if !errors.Is(err, store.ErrConfirmationNotFound) {
			t.Errorf("expected ErrConfirmationNotFound; got: %v", err)
		}
	})

	t.Run("already_resolved_returns_ErrConfirmationAlreadyResolved", func(t *testing.T) {
		callID := "call-cas-resolved-01"
		seedConfirmation(t, s, callID, convID, msgID, userID, "approved", futureTime())

		err := sq.UpdatePendingConfirmationStatusIfCurrent(ctx, callID, "approved", now())
		if !errors.Is(err, store.ErrConfirmationAlreadyResolved) {
			t.Errorf("expected ErrConfirmationAlreadyResolved; got: %v", err)
		}
	})

	t.Run("pending_but_expired_returns_ErrConfirmationExpired", func(t *testing.T) {
		callID := "call-cas-expired-01"
		seedConfirmation(t, s, callID, convID, msgID, userID, "pending", pastTime())

		err := sq.UpdatePendingConfirmationStatusIfCurrent(ctx, callID, "approved", now())
		if !errors.Is(err, store.ErrConfirmationExpired) {
			t.Errorf("expected ErrConfirmationExpired; got: %v", err)
		}
	})
}

// TestMarkConfirmationExpired_ZeroRows verifies that MarkConfirmationExpired on a
// row that no longer exists (or was already transitioned) returns ErrConfirmationExpired.
func TestMarkConfirmationExpired_ZeroRows(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()
	const userID = "user-expire-test"
	const convID = "conv-expire-test"
	const msgID = "msg-expire-test"

	seedUser(t, s, userID)
	seedConversation(t, s, convID, userID)
	seedMessage(t, s, msgID, convID, userID, "assistant")

	sq := s.ForUser(store.UserScope(store.Principal{UserID: userID, Role: "member"}))

	t.Run("no_row_returns_ErrConfirmationExpired", func(t *testing.T) {
		err := sq.MarkConfirmationExpired(ctx, "call-mark-nonexistent-xx")
		if !errors.Is(err, store.ErrConfirmationExpired) {
			t.Errorf("expected ErrConfirmationExpired; got: %v", err)
		}
	})

	t.Run("already_expired_row_returns_ErrConfirmationExpired", func(t *testing.T) {
		// Seed a row that is already in "expired" status — the CAS (WHERE status='pending')
		// finds zero rows for this case too.
		callID := "call-mark-already-expired-01"
		seedConfirmation(t, s, callID, convID, msgID, userID, "expired", pastTime())

		err := sq.MarkConfirmationExpired(ctx, callID)
		if !errors.Is(err, store.ErrConfirmationExpired) {
			t.Errorf("expected ErrConfirmationExpired for already-expired row; got: %v", err)
		}
	})
}

// ── B.2: cross-user isolation of the bypass methods ──────────────────────────

// TestBypassMethods_CrossUserIsolation seeds two users' confirmations that share
// message/call-ID patterns, then asserts:
//
//  1. MarkConfirmationExpiredByUserID(callID, userA) expires only userA's row.
//  2. MarkMessageAbandonedByUserID(msgID, userA) abandons only userA's message.
//  3. ListLapsedConfirmations returns BOTH users' lapsed rows and excludes non-lapsed.
func TestBypassMethods_CrossUserIsolation(t *testing.T) {
	t.Parallel()

	s := openMigratedStore(t)
	ctx := context.Background()

	const userA = "user-bypass-a"
	const userB = "user-bypass-b"
	const conv = "conv-bypass-shared"
	const msgA = "msg-bypass-a"
	const msgB = "msg-bypass-b"
	const callA = "call-bypass-a"
	const callB = "call-bypass-b"
	// A non-lapsed call (future expiry) that must NOT appear in ListLapsedConfirmations.
	const callActive = "call-bypass-active"

	seedUser(t, s, userA)
	seedUser(t, s, userB)
	seedConversation(t, s, conv, userA)
	// userB needs their own conversation (FK requires user_id match on conversations).
	const convB = "conv-bypass-b"
	seedConversation(t, s, convB, userB)
	seedMessage(t, s, msgA, conv, userA, "assistant")
	seedMessage(t, s, msgB, convB, userB, "assistant")

	// Both users have a lapsed ("pending" + past expires_at) confirmation.
	past := pastTime()
	future := futureTime()
	seedConfirmation(t, s, callA, conv, msgA, userA, "pending", past)
	seedConfirmation(t, s, callB, convB, msgB, userB, "pending", past)
	// userA also has an active (non-lapsed) confirmation — must not appear in lapsed list.
	seedConfirmation(t, s, callActive, conv, msgA, userA, "pending", future)

	sysSQ := s.ForUser(store.SystemScope())

	// ── 3. ListLapsedConfirmations returns BOTH lapsed rows and excludes active ──
	lapsed, err := sysSQ.ListLapsedConfirmations(ctx, now())
	if err != nil {
		t.Fatalf("ListLapsedConfirmations: %v", err)
	}

	// Collect lapsed rows by callID.
	lapsedByID := make(map[string]string) // callID → userID
	for _, row := range lapsed {
		lapsedByID[row.ID] = row.UserID
	}

	if uid, ok := lapsedByID[callA]; !ok {
		t.Errorf("ListLapsedConfirmations: expected callA %q in results", callA)
	} else if uid != userA {
		t.Errorf("callA userID = %q, want %q", uid, userA)
	}
	if uid, ok := lapsedByID[callB]; !ok {
		t.Errorf("ListLapsedConfirmations: expected callB %q in results", callB)
	} else if uid != userB {
		t.Errorf("callB userID = %q, want %q", uid, userB)
	}
	if _, ok := lapsedByID[callActive]; ok {
		t.Errorf("ListLapsedConfirmations: active call %q must not appear in lapsed results", callActive)
	}

	// ── 1. MarkConfirmationExpiredByUserID only affects userA's row ──────────────
	n, err := sysSQ.MarkConfirmationExpiredByUserID(ctx, callA, userA)
	if err != nil {
		t.Fatalf("MarkConfirmationExpiredByUserID: %v", err)
	}
	if n != 1 {
		t.Errorf("MarkConfirmationExpiredByUserID rowsAffected = %d, want 1", n)
	}

	// Verify userA's row is now "expired".
	sqA := s.ForUser(store.UserScope(store.Principal{UserID: userA, Role: "member"}))
	rowA, err := sqA.GetPendingConfirmation(ctx, callA)
	if err != nil {
		t.Fatalf("GetPendingConfirmation callA after expire: %v", err)
	}
	if rowA.Status != "expired" {
		t.Errorf("callA status after MarkConfirmationExpiredByUserID = %q, want %q", rowA.Status, "expired")
	}

	// Verify userB's row is still "pending" — not touched.
	sqB := s.ForUser(store.UserScope(store.Principal{UserID: userB, Role: "member"}))
	rowB, err := sqB.GetPendingConfirmation(ctx, callB)
	if err != nil {
		t.Fatalf("GetPendingConfirmation callB (should be untouched): %v", err)
	}
	if rowB.Status != "pending" {
		t.Errorf("callB status after userA's expire = %q, want %q (must be untouched)", rowB.Status, "pending")
	}

	// ── 2. MarkMessageAbandonedByUserID only affects userA's message ─────────────
	// First assert userA's message is not abandoned.
	msgsA, err := sqA.ListMessages(ctx, conv)
	if err != nil {
		t.Fatalf("ListMessages userA before abandon: %v", err)
	}
	if len(msgsA) != 1 {
		t.Fatalf("expected 1 message for userA; got %d", len(msgsA))
	}
	if msgsA[0].Abandoned != 0 {
		t.Error("userA's message must not be abandoned before MarkMessageAbandonedByUserID")
	}

	if err := sysSQ.MarkMessageAbandonedByUserID(ctx, msgA, userA); err != nil {
		t.Fatalf("MarkMessageAbandonedByUserID: %v", err)
	}

	// userA's message is now abandoned.
	msgsA2, err := sqA.ListMessages(ctx, conv)
	if err != nil {
		t.Fatalf("ListMessages userA after abandon: %v", err)
	}
	if len(msgsA2) != 1 {
		t.Fatalf("expected 1 message for userA after abandon; got %d", len(msgsA2))
	}
	if msgsA2[0].Abandoned == 0 {
		t.Error("userA's message must be abandoned after MarkMessageAbandonedByUserID")
	}

	// userB's message is unaffected.
	msgsB, err := sqB.ListMessages(ctx, convB)
	if err != nil {
		t.Fatalf("ListMessages userB: %v", err)
	}
	if len(msgsB) != 1 {
		t.Fatalf("expected 1 message for userB; got %d", len(msgsB))
	}
	if msgsB[0].Abandoned != 0 {
		t.Errorf("userB's message abandoned = %d after userA's MarkMessageAbandonedByUserID; want 0", msgsB[0].Abandoned)
	}
}
