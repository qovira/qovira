package harness

// helpers_unit_test.go — focused table-driven unit tests for the pure in-memory
// helpers that were made cheaply testable by the H3 load-once refactor.
//
// Each helper is exercised with constructed []db.ListMessagesRow slices so no
// store I/O is needed. Each table includes at least one case that would FAIL if
// the boundary rule were broken, proving the assertion is not tautological.

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/store/db"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// row builds a db.ListMessagesRow for test setup. Zero values are omitted.
func row(role, msgID, toolCalls, toolCallID string, abandoned int64) db.ListMessagesRow {
	return db.ListMessagesRow{
		ID:         msgID,
		Role:       role,
		ToolCalls:  sql.NullString{String: toolCalls, Valid: toolCalls != ""},
		ToolCallID: sql.NullString{String: toolCallID, Valid: toolCallID != ""},
		Abandoned:  abandoned,
	}
}

// toolCallsJSON encodes a slice of ToolCalls to a JSON string for use in rows.
func toolCallsJSON(calls []gateway.ToolCall) string {
	b, err := json.Marshal(calls)
	if err != nil {
		panic("toolCallsJSON: " + err.Error())
	}
	return string(b)
}

// ── buildToolResultSet ────────────────────────────────────────────────────────

func TestBuildToolResultSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		msgs    []db.ListMessagesRow
		wantIDs []string // IDs that must be present in the result set
		notIDs  []string // IDs that must NOT be present
	}{
		{
			name:    "empty slice",
			msgs:    nil,
			wantIDs: nil,
			notIDs:  []string{"any"},
		},
		{
			name: "single tool row with valid non-empty ID",
			msgs: []db.ListMessagesRow{
				row("tool", "msg1", "", "call_abc", 0),
			},
			wantIDs: []string{"call_abc"},
		},
		{
			// An empty ToolCallID string (Valid=true, String="") must NOT be
			// included — the `!= ""` guard is load-bearing. A regression that drops
			// it would set[""] = true, treating every un-IDed result as "done" and
			// hiding repeated execution bugs.
			// Use explicit sql.NullString{Valid:true, String:""} rather than the
			// row() helper, which sets Valid=false when the string is empty (a
			// distinct case). Both must be excluded, but this case tests the
			// non-empty-string guard specifically.
			name: "empty tool_call_id string (Valid=true, String=empty) is excluded",
			msgs: []db.ListMessagesRow{
				{Role: "tool", ToolCallID: sql.NullString{Valid: true, String: ""}},
				row("tool", "msg2", "", "call_real", 0),
			},
			wantIDs: []string{"call_real"},
			notIDs:  []string{""},
		},
		{
			// A null ToolCallID (Valid=false) must also be excluded.
			name: "null tool_call_id (Valid=false) is excluded",
			msgs: []db.ListMessagesRow{
				{Role: "tool", ToolCallID: sql.NullString{Valid: false}},
				row("tool", "msg2", "", "call_real", 0),
			},
			wantIDs: []string{"call_real"},
		},
		{
			// Non-tool rows (user, assistant) must never populate the set even if
			// their ToolCallID field happens to be set.
			name: "non-tool roles are not included",
			msgs: []db.ListMessagesRow{
				row("assistant", "msg1", "", "spurious_id", 0),
				row("user", "msg2", "", "other_id", 0),
				row("tool", "msg3", "", "real_id", 0),
			},
			wantIDs: []string{"real_id"},
			notIDs:  []string{"spurious_id", "other_id"},
		},
		{
			name: "multiple tool rows all included",
			msgs: []db.ListMessagesRow{
				row("tool", "msg1", "", "c1", 0),
				row("tool", "msg2", "", "c2", 0),
				row("tool", "msg3", "", "c3", 0),
			},
			wantIDs: []string{"c1", "c2", "c3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildToolResultSet(tt.msgs)
			for _, id := range tt.wantIDs {
				if !got[id] {
					t.Errorf("ID %q missing from result set; set=%v", id, got)
				}
			}
			for _, id := range tt.notIDs {
				if got[id] {
					t.Errorf("ID %q must NOT be in result set but is; set=%v", id, got)
				}
			}
		})
	}
}

// ── isTurnComplete ────────────────────────────────────────────────────────────

func TestIsTurnComplete(t *testing.T) {
	t.Parallel()

	anyToolCalls := toolCallsJSON([]gateway.ToolCall{{ID: "c1", Name: "t"}})

	tests := []struct {
		name string
		msgs []db.ListMessagesRow
		want bool
	}{
		{
			name: "empty slice → not complete",
			msgs: nil,
			want: false,
		},
		{
			name: "last message is user → not complete",
			msgs: []db.ListMessagesRow{
				row("assistant", "a", "", "", 0),
				row("user", "u", "", "", 0),
			},
			want: false,
		},
		{
			name: "last message is tool → not complete",
			msgs: []db.ListMessagesRow{
				row("assistant", "a", anyToolCalls, "", 0),
				row("tool", "t", "", "c1", 0),
			},
			want: false,
		},
		{
			name: "last message is assistant with tool_calls → not complete",
			msgs: []db.ListMessagesRow{
				row("assistant", "a", anyToolCalls, "", 0),
			},
			want: false,
		},
		{
			// The key boundary: an assistant message WITHOUT tool_calls is the only
			// terminal. If isTurnComplete treated a tool-call assistant message as
			// complete, run() would exit early and never process the outstanding calls.
			name: "last message is assistant with no tool_calls → complete",
			msgs: []db.ListMessagesRow{
				row("user", "u", "", "", 0),
				row("assistant", "a", "", "", 0), // no ToolCalls
			},
			want: true,
		},
		{
			// Abandoned assistant messages are treated as terminal — an abandoned
			// message means all sibling confirmations expired without user action.
			// isTurnComplete must return true so run() does not re-enter.
			name: "abandoned assistant message → complete",
			msgs: []db.ListMessagesRow{
				row("assistant", "a", anyToolCalls, "", 1), // Abandoned=1
			},
			want: true,
		},
		{
			// Assistant with empty ToolCalls.String (Valid=true, String="") must be
			// treated as a final reply (no outstanding calls). This is the same
			// boundary as outstandingToolCalls — the "non-empty" check is load-bearing.
			name: "assistant with valid but empty tool_calls string → complete",
			msgs: []db.ListMessagesRow{
				{Role: "assistant", ToolCalls: sql.NullString{String: "", Valid: true}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isTurnComplete(tt.msgs)
			if got != tt.want {
				t.Errorf("isTurnComplete() = %v, want %v (msgs: %v)", got, tt.want, tt.msgs)
			}
		})
	}
}

// ── outstandingToolCalls ──────────────────────────────────────────────────────

func TestOutstandingToolCalls(t *testing.T) {
	t.Parallel()

	callA := gateway.ToolCall{ID: "c_a", Name: "tool_a"}
	callB := gateway.ToolCall{ID: "c_b", Name: "tool_b"}

	tests := []struct {
		name       string
		msgs       []db.ListMessagesRow
		wantIDs    []string // call IDs that must appear in outstanding
		wantNilOut bool     // entire result must be nil
		wantErr    bool
	}{
		{
			name:       "empty slice → nil",
			msgs:       nil,
			wantNilOut: true,
		},
		{
			name: "last assistant has no tool_calls → nil",
			msgs: []db.ListMessagesRow{
				row("user", "u", "", "", 0),
				row("assistant", "a", "", "", 0),
			},
			wantNilOut: true,
		},
		{
			// Both calls are outstanding (no tool-result rows at all).
			name: "both calls outstanding",
			msgs: []db.ListMessagesRow{
				row("assistant", "a", toolCallsJSON([]gateway.ToolCall{callA, callB}), "", 0),
			},
			wantIDs: []string{"c_a", "c_b"},
		},
		{
			// One call has a result; the other is still outstanding.
			name: "one call done → other still outstanding",
			msgs: []db.ListMessagesRow{
				row("assistant", "a", toolCallsJSON([]gateway.ToolCall{callA, callB}), "", 0),
				row("tool", "t", "", "c_a", 0), // c_a done
			},
			wantIDs: []string{"c_b"},
		},
		{
			// All calls have results → nil (not a suspended state).
			name: "all calls done → nil",
			msgs: []db.ListMessagesRow{
				row("assistant", "a", toolCallsJSON([]gateway.ToolCall{callA, callB}), "", 0),
				row("tool", "t1", "", "c_a", 0),
				row("tool", "t2", "", "c_b", 0),
			},
			wantNilOut: true,
		},
		{
			// The backward-scan rule: only the LAST assistant-with-tool_calls is checked.
			// An older assistant message with outstanding calls must NOT be returned
			// when a newer one with all results present comes after it.
			name: "backward scan: last assistant checked, not older ones",
			msgs: []db.ListMessagesRow{
				// Older round: tool call c_a with its result.
				row("assistant", "a1", toolCallsJSON([]gateway.ToolCall{callA}), "", 0),
				row("tool", "t1", "", "c_a", 0),
				// Newer round: both calls resolved.
				row("assistant", "a2", toolCallsJSON([]gateway.ToolCall{callA, callB}), "", 0),
				row("tool", "t2", "", "c_a", 0), // reused ID in new round; already in set
				row("tool", "t3", "", "c_b", 0),
			},
			wantNilOut: true,
		},
		{
			// An empty-ID tool-result (Valid=true, String="") must NOT mark the
			// empty-ID call done. buildToolResultSet excludes empty IDs; a regression
			// that dropped the `!= ""` guard would set set[""] = true, falsely
			// satisfying an outstanding call whose ID is also "".
			// Use an explicit NullString{Valid:true,String:""} — the row() helper
			// sets Valid=false for empty strings, which is the different (null) case.
			name: "empty-ID tool result does NOT mark empty-ID call done",
			msgs: []db.ListMessagesRow{
				// A call with ID="" is outstanding (edge case; harness synthesizes IDs
				// in practice, but the guard must hold unconditionally).
				row("assistant", "a", toolCallsJSON([]gateway.ToolCall{{ID: "", Name: "tool_a"}}), "", 0),
				{Role: "tool", ToolCallID: sql.NullString{Valid: true, String: ""}}, // Valid=true, String="" — must NOT satisfy ID=""
			},
			// With the guard: set[""] is absent → call "" remains outstanding.
			// Without the guard: set[""] = true → call "" falsely marked done → nil result.
			wantIDs:    []string{""},
			wantNilOut: false,
		},
		{
			// Abandoned assistant message → nil immediately (the fully-stale short-circuit).
			// If this guard were removed, outstandingToolCalls would return the calls and
			// run() would try to re-process them — but they all have status="expired" and
			// synthetic results, making the re-entry spurious.
			name: "abandoned assistant → nil (fully-stale short-circuit)",
			msgs: []db.ListMessagesRow{
				row("assistant", "a", toolCallsJSON([]gateway.ToolCall{callA}), "", 1), // Abandoned=1
			},
			wantNilOut: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := outstandingToolCalls(tt.msgs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("outstandingToolCalls() err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantNilOut {
				if len(got) != 0 {
					ids := make([]string, len(got))
					for i, c := range got {
						ids[i] = c.ID
					}
					t.Errorf("expected nil/empty result, got calls: %v", ids)
				}
				return
			}
			gotIDs := make(map[string]bool, len(got))
			for _, c := range got {
				gotIDs[c.ID] = true
			}
			for _, id := range tt.wantIDs {
				if !gotIDs[id] {
					t.Errorf("call %q missing from outstanding result; got IDs: %v", id, gotIDs)
				}
			}
		})
	}
}

// ── findAssistantMessageForCall ───────────────────────────────────────────────

func TestFindAssistantMessageForCall(t *testing.T) {
	t.Parallel()

	callA := gateway.ToolCall{ID: "c_a", Name: "tool_a"}
	callB := gateway.ToolCall{ID: "c_b", Name: "tool_b"}

	tests := []struct {
		name    string
		msgs    []db.ListMessagesRow
		callID  string
		wantMID string // expected message ID; "" means error expected
		wantErr bool
	}{
		{
			name:    "empty slice → error",
			msgs:    nil,
			callID:  "c_a",
			wantErr: true,
		},
		{
			// The call is in the most recent assistant message.
			name: "call found in last assistant message",
			msgs: []db.ListMessagesRow{
				row("user", "u", "", "", 0),
				row("assistant", "msg_a1", toolCallsJSON([]gateway.ToolCall{callA}), "", 0),
			},
			callID:  "c_a",
			wantMID: "msg_a1",
		},
		{
			// The backward scan returns the most recent assistant message containing the call.
			name: "backward scan: most recent assistant that has the call",
			msgs: []db.ListMessagesRow{
				row("assistant", "msg_old", toolCallsJSON([]gateway.ToolCall{callA}), "", 0),
				row("tool", "t1", "", "c_a", 0),
				row("assistant", "msg_new", toolCallsJSON([]gateway.ToolCall{callA, callB}), "", 0),
			},
			callID:  "c_a",
			wantMID: "msg_new",
		},
		{
			// The call ID is not present in any assistant message → error.
			name: "call not found → error",
			msgs: []db.ListMessagesRow{
				row("assistant", "msg_a1", toolCallsJSON([]gateway.ToolCall{callB}), "", 0),
			},
			callID:  "c_a",
			wantErr: true,
		},
		{
			// An assistant message with no tool_calls is skipped.
			name: "assistant with no tool_calls is skipped",
			msgs: []db.ListMessagesRow{
				row("assistant", "no_calls", "", "", 0),
				row("assistant", "has_calls", toolCallsJSON([]gateway.ToolCall{callA}), "", 0),
			},
			callID:  "c_a",
			wantMID: "has_calls",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := findAssistantMessageForCall(tt.msgs, tt.callID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("findAssistantMessageForCall() err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.wantMID {
				t.Errorf("findAssistantMessageForCall() = %q, want %q", got, tt.wantMID)
			}
		})
	}
}
