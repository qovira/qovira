package harness

// prompt_window_test.go — white-box unit tests for the system-prompt composition and sliding-window trim logic.
// These test unexported functions directly and are therefore in package harness (not harness_test).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/store/db"
)

// ── estimateTokens ────────────────────────────────────────────────────────────

func TestEstimateTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantAtLst int // estimated tokens must be >= this
	}{
		{name: "empty", input: "", wantAtLst: 0},
		{name: "four_chars", input: "abcd", wantAtLst: 1},
		{name: "hundred_chars", input: strings.Repeat("a", 100), wantAtLst: 25},
		{name: "thousand_chars", input: strings.Repeat("a", 1000), wantAtLst: 250},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := estimateTokens(tt.input)
			if got < tt.wantAtLst {
				t.Errorf("estimateTokens(%d chars) = %d, want >= %d", len(tt.input), got, tt.wantAtLst)
			}
		})
	}
}

// ── segmentGroups ─────────────────────────────────────────────────────────────

func TestSegmentGroups_Basic(t *testing.T) {
	t.Parallel()

	msgs := []gateway.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
		{Role: "user", Content: "how are you?"},
		{Role: "assistant", Content: "fine"},
	}

	groups := segmentGroups(msgs)

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0][0].Content != "hello" {
		t.Errorf("group[0][0] = %q, want hello", groups[0][0].Content)
	}
	if groups[1][0].Content != "how are you?" {
		t.Errorf("group[1][0] = %q, want 'how are you?'", groups[1][0].Content)
	}
}

func TestSegmentGroups_SingleGroup(t *testing.T) {
	t.Parallel()

	msgs := []gateway.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a"},
	}

	groups := segmentGroups(msgs)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
}

func TestSegmentGroups_WithToolCalls(t *testing.T) {
	t.Parallel()

	// user → assistant(tool_calls) → tool → assistant is ONE group (everything from the first user until the next
	// user).
	msgs := []gateway.Message{
		{Role: "user", Content: "use the tool"},
		{Role: "assistant", Content: "", ToolCalls: []gateway.ToolCall{{ID: "c1", Name: "t", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "c1", Content: "result"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "next question"},
		{Role: "assistant", Content: "answer"},
	}

	groups := segmentGroups(msgs)

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d: %v", len(groups), groupSummary(groups))
	}

	// Group 0 must contain all 4 messages (user, assistant+tool_calls, tool, assistant).
	if len(groups[0]) != 4 {
		t.Errorf("group[0] len = %d, want 4; messages: %v", len(groups[0]), groupSummary(groups[:1]))
	}

	// Group 1 is the "next question" exchange.
	if len(groups[1]) != 2 {
		t.Errorf("group[1] len = %d, want 2", len(groups[1]))
	}

	// BOUNDARY RULE: no tool message in group 0 must be orphaned from its tool_calls.
	for i, msg := range groups[0] {
		if msg.Role == "tool" {
			// Ensure the preceding assistant message has matching tool_calls.
			found := false
			for j := i - 1; j >= 0; j-- {
				for _, tc := range groups[0][j].ToolCalls {
					if tc.ID == msg.ToolCallID {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found {
				t.Errorf("tool message at group[0][%d] is orphaned: tool_call_id=%q has no matching tool_calls message in the group", i, msg.ToolCallID)
			}
		}
	}
}

func TestSegmentGroups_Empty(t *testing.T) {
	t.Parallel()

	groups := segmentGroups(nil)
	if len(groups) != 0 {
		t.Errorf("expected empty groups for nil input, got %d", len(groups))
	}
}

// groupSummary returns a compact description of groups for test error messages.
func groupSummary(groups [][]gateway.Message) string {
	var sb strings.Builder
	for i, g := range groups {
		sb.WriteString("group[")
		sb.WriteString(string(rune('0' + i)))
		sb.WriteString("]:[")
		for j, m := range g {
			if j > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(m.Role)
		}
		sb.WriteString("] ")
	}
	return sb.String()
}

// ── trimToWindowBudget ────────────────────────────────────────────────────────

func TestTrimToWindowBudget_DropOldestGroups(t *testing.T) {
	t.Parallel()

	// Construct a history that clearly exceeds a 20-token budget.
	// Each message has 80 chars of content → ~20 estimated tokens each.
	// Group 0 (oldest): user + assistant = ~40 tokens → over any 20-tok budget.
	// Group 1 (newest): user + assistant = ~40 tokens.
	bigContent := strings.Repeat("x", 80) // ~20 est tokens
	msgs := []gateway.Message{
		{Role: "user", Content: bigContent},
		{Role: "assistant", Content: bigContent},
		{Role: "user", Content: bigContent},
		{Role: "assistant", Content: bigContent},
	}

	// Budget = 20 tokens — not enough for both groups, so oldest must drop.
	result := trimToWindowBudget(msgs, 20, 0)

	// Must keep at least the newest group.
	if len(result) == 0 {
		t.Fatal("trimToWindowBudget returned empty history; must always keep at least the newest group")
	}

	// Newest user message must be present.
	lastUser := ""
	for _, m := range result {
		if m.Role == "user" {
			lastUser = m.Content
		}
	}
	if lastUser == "" {
		t.Error("no user message in trimmed result")
	}

	// Result must have fewer messages than the original (oldest group dropped).
	if len(result) >= len(msgs) {
		t.Errorf("expected trimming to reduce message count; got %d (same as input %d)", len(result), len(msgs))
	}
}

func TestTrimToWindowBudget_AllFit(t *testing.T) {
	t.Parallel()

	// Small messages that easily fit a large budget — nothing should be dropped.
	msgs := []gateway.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "bye"},
		{Role: "assistant", Content: "goodbye"},
	}

	result := trimToWindowBudget(msgs, 50_000, 0)

	if len(result) != len(msgs) {
		t.Errorf("expected all %d messages to fit; got %d", len(msgs), len(result))
	}
}

func TestTrimToWindowBudget_NeverOrphansToolGroup(t *testing.T) {
	t.Parallel()

	// Build a history where:
	//   Group 0 (oldest, big): user, assistant(tool_calls), tool, assistant — must be dropped whole.
	//   Group 1 (newest, small): user, assistant.
	bigContent := strings.Repeat("z", 200) // big enough to push group over budget.
	tcs := []gateway.ToolCall{{ID: "tc1", Name: "t", Arguments: json.RawMessage(`{}`)}}

	msgs := []gateway.Message{
		{Role: "user", Content: bigContent},
		{Role: "assistant", Content: "", ToolCalls: tcs},
		{Role: "tool", ToolCallID: "tc1", Content: bigContent},
		{Role: "assistant", Content: bigContent},
		{Role: "user", Content: "small"},
		{Role: "assistant", Content: "small"},
	}

	// Budget = 5 tokens — much less than group 0 uses, so group 0 must be dropped.
	result := trimToWindowBudget(msgs, 5, 0)

	// BOUNDARY RULE: no tool message should be in the result without its matching assistant tool_calls message.
	for i, m := range result {
		if m.Role != "tool" {
			continue
		}
		found := false
		for j := i - 1; j >= 0; j-- {
			for _, tc := range result[j].ToolCalls {
				if tc.ID == m.ToolCallID {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			t.Errorf("tool message at result[%d] (tool_call_id=%q) is orphaned — its assistant tool_calls message was trimmed", i, m.ToolCallID)
		}
	}

	// The newest group (user:"small", assistant:"small") must survive.
	var hasSmallUser bool
	for _, m := range result {
		if m.Role == "user" && m.Content == "small" {
			hasSmallUser = true
		}
	}
	if !hasSmallUser {
		t.Error("newest group user message 'small' was trimmed — must always keep newest group")
	}
}

func TestTrimToWindowBudget_AlwaysKeepsNewestGroup(t *testing.T) {
	t.Parallel()

	// Even with a budget of 0, the newest group must be kept.
	msgs := []gateway.Message{
		{Role: "user", Content: "keep me"},
		{Role: "assistant", Content: "ok"},
	}

	result := trimToWindowBudget(msgs, 0, 0)

	if len(result) == 0 {
		t.Error("trimToWindowBudget with 0-budget returned empty — must always keep at least the newest group")
	}
}

// ── buildSystemPrompt ─────────────────────────────────────────────────────────

func TestBuildSystemPrompt_ContainsRequiredFields(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 6, 14, 10, 30, 0, 0, time.UTC)
	profile := db.User{
		Timezone:    "America/New_York",
		Locale:      "en-US",
		Language:    "en",
		DisplayName: "Alice Tester",
	}

	prompt := buildSystemPrompt(fixedNow, profile)

	// AC-1: must contain current time in the user's IANA timezone. New York is UTC-4 in June, so 10:30 UTC = 06:30 EDT.
	if !strings.Contains(prompt, "2026") {
		t.Error("system prompt does not contain current year")
	}
	if !strings.Contains(prompt, "America/New_York") {
		t.Errorf("system prompt does not contain IANA timezone %q; prompt:\n%s", "America/New_York", prompt)
	}
	if !strings.Contains(prompt, "en-US") {
		t.Errorf("system prompt does not contain locale %q; prompt:\n%s", "en-US", prompt)
	}
	if !strings.Contains(prompt, "en") {
		t.Errorf("system prompt does not contain language %q", "en")
	}
	if !strings.Contains(prompt, "Alice Tester") {
		t.Errorf("system prompt does not contain display name %q; prompt:\n%s", "Alice Tester", prompt)
	}

	// AC-2: reserved memory slot must be present but empty.
	if !strings.Contains(prompt, memorySlotMarker) {
		t.Errorf("system prompt missing reserved memory slot marker %q; prompt:\n%s", memorySlotMarker, prompt)
	}
	// The slot must be empty (no content between the marker and the next section).
	_, after, ok := strings.Cut(prompt, memorySlotMarker)
	if !ok {
		t.Fatal("memory slot marker not found")
	}
	afterMarker := after
	// The section after the marker should start immediately with whitespace/newline — no actual content injected in
	// v0.1.
	trimmed := strings.TrimSpace(afterMarker)
	// trimmed may be empty or start a new section header; it must NOT start with content that looks like memory
	// entries (we consider anything not a section header to be injected content).
	if len(trimmed) > 0 && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "---") {
		// Accept that there might be a closing section indicator — just ensure there's no substantial injected content
		// on the line directly after the marker.
		firstLine := strings.SplitN(trimmed, "\n", 2)[0]
		if len(strings.TrimSpace(firstLine)) > 0 &&
			!strings.HasPrefix(firstLine, "#") &&
			!strings.HasPrefix(firstLine, "---") {
			t.Errorf("memory slot appears non-empty in v0.1: content after marker = %q", firstLine)
		}
	}
}

// TestTrimToWindowBudget_DropsLeadingNonUserGroup verifies that when the trimmed window starts with a non-user
// group (an orphaned assistant/tool run with no user message anywhere in history), trimToWindowBudget drops those
// leading messages so the returned window always starts with a user-role message (or is empty).
//
// This covers the case the doc comment calls "structurally impossible" but which can occur when the ENTIRE
// persisted history is a leading assistant/tool sequence (e.g. a legacy conversation that begins with an assistant
// message).
func TestTrimToWindowBudget_DropsLeadingNonUserGroup(t *testing.T) {
	t.Parallel()

	// Case 1: history is ONLY [assistant(tool_calls), tool] — no user at all. segmentGroups puts everything in ONE
	// group (no user to start a new one). trimToWindowBudget keeps that single group (the newest) as-is today — the
	// fix must then drop it because it doesn't start with "user".
	tcs := []gateway.ToolCall{{ID: "tc1", Name: "t", Arguments: json.RawMessage(`{}`)}}
	onlyAssistant := []gateway.Message{
		{Role: "assistant", Content: "tool round", ToolCalls: tcs},
		{Role: "tool", ToolCallID: "tc1", Content: "result"},
	}

	got := trimToWindowBudget(onlyAssistant, 50_000, 0)
	if len(got) > 0 && got[0].Role != "user" {
		t.Errorf("case1: result starts with role=%q, want user or empty; got %v roles",
			got[0].Role, roleNames(got))
	}

	// Case 2: [assistant, tool, user, assistant] — the first group is the orphaned assistant/tool prefix; the
	// second group starts with user. After trimming to budget that fits both groups the result is returned
	// verbatim; the fix must drop the leading non-user group.
	mixed := []gateway.Message{
		{Role: "assistant", Content: "preamble"},
		{Role: "tool", ToolCallID: "tc2", Content: "preamble result"},
		{Role: "user", Content: "actual question"},
		{Role: "assistant", Content: "answer"},
	}

	got2 := trimToWindowBudget(mixed, 50_000, 0)
	if len(got2) == 0 {
		t.Fatal("case2: got empty result; want the user+assistant group kept")
	}
	if got2[0].Role != "user" {
		t.Errorf("case2: result[0].role = %q, want user; roles: %v", got2[0].Role, roleNames(got2))
	}
}

// roleNames extracts the roles from a message slice for test diagnostics.
func roleNames(msgs []gateway.Message) []string {
	names := make([]string, len(msgs))
	for i, m := range msgs {
		names[i] = m.Role
	}
	return names
}

func TestBuildSystemPrompt_BadTimezoneUsesUTC(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	profile := db.User{
		Timezone:    "Not/A/Timezone",
		Locale:      "en-US",
		Language:    "en",
		DisplayName: "Bob",
	}

	// Must not panic; must fall back to UTC.
	prompt := buildSystemPrompt(fixedNow, profile)

	if !strings.Contains(prompt, "2026") {
		t.Error("system prompt with bad timezone must still contain current year (UTC fallback)")
	}
	// Should contain the original tz string even if we fell back (so the user knows what was configured).
	if !strings.Contains(prompt, "Not/A/Timezone") {
		t.Errorf("system prompt should include configured (bad) tz string so operator can debug; prompt:\n%s", prompt)
	}
}
