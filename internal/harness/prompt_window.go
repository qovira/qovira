package harness

// prompt_window.go — system-prompt composition, token estimation, and sliding
// history-window trim for the AI turn assembler.
//
// Design decisions (documented here for consistency):
//   - The system prompt does NOT count toward the HistoryTokenBudget; it is
//     always included in the assembled request regardless of budget. The budget
//     covers history messages only.
//   - Token estimation uses the chars/4 heuristic with a small (10%) upward
//     margin; no bundled tokenizer.
//   - Grouping: a "group" is a user message and all subsequent messages up to
//     (not including) the next user message. This makes the boundary rule
//     structurally impossible to violate: tool-calls messages and their tool
//     results always live in the same group.
//   - At least the newest group is always kept, even if it alone exceeds the
//     budget. "Never zero history" is the guarantee.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/qovira/qovira/internal/gateway"
	"github.com/qovira/qovira/internal/store/db"
)

// memorySlotMarker is the section header for the reserved memory injection slot.
// Tier-1 soul/profile will populate this section in v0.2; it is present but
// empty in v0.1. Tests assert that this exact string appears in the system prompt.
//
// The value is unexported as a constant so that prompt_window_test.go (which is
// in package harness) can reference it directly, and the black-box tests reference
// it through the exported MemorySlotMarker alias below.
const memorySlotMarker = "## Memory"

// MemorySlotMarker is the exported alias used by harness_test-package tests.
const MemorySlotMarker = memorySlotMarker

// ── estimateTokens ────────────────────────────────────────────────────────────

// tokenMargin is the fractional upward margin applied on top of the chars/4
// estimate to account for tokenizer differences (punctuation, Unicode, etc.).
const tokenMargin = 0.10

// estimateTokens returns a rough upper-bound token count for s, using the
// chars/4 heuristic plus a 10% margin. It is a pure function with no I/O.
// The margin errs on the side of over-counting to avoid exceeding the budget.
func estimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	base := len(s) / 4
	// Apply margin, rounding up.
	margin := int(float64(base)*tokenMargin) + 1
	return base + margin
}

// estimateMessageTokens returns the estimated token count for a single
// gateway.Message. It includes the content and, for assistant messages with
// tool calls, the JSON-serialised tool_calls.
func estimateMessageTokens(m gateway.Message) int {
	total := estimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		total += estimateTokens(tc.Name)
		total += estimateTokens(string(tc.Arguments))
		total += estimateTokens(tc.ID)
	}
	if m.ToolCallID != "" {
		total += estimateTokens(m.ToolCallID)
	}
	return total
}

// ── segmentGroups ─────────────────────────────────────────────────────────────

// segmentGroups partitions msgs into exchange groups. Each group begins with a
// "user" message and contains all following non-user messages until the next
// "user" message (exclusive). The groups are returned in the same order as the
// input (oldest → newest).
//
// This grouping makes the boundary rule structurally impossible to violate:
// an assistant tool_calls message and its corresponding tool-result messages
// always share a group because only a "user" message can start a new group.
func segmentGroups(msgs []gateway.Message) [][]gateway.Message {
	if len(msgs) == 0 {
		return nil
	}

	var groups [][]gateway.Message
	var current []gateway.Message

	for _, m := range msgs {
		if m.Role == "user" && len(current) > 0 {
			// Start a new group.
			groups = append(groups, current)
			current = nil
		}
		current = append(current, m)
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}

	return groups
}

// ── trimToWindowBudget ────────────────────────────────────────────────────────

// trimToWindowBudget returns the subset of msgs that fits within tokenBudget
// estimated tokens, always keeping at least the newest group (the last exchange).
// It drops oldest groups first. The returned slice is a sub-slice of msgs;
// no allocation of message values occurs.
//
// extraDrop removes that many additional oldest groups beyond what the budget
// alone would drop — used by the context-length retry to apply a "harder trim"
// on successive attempts.
//
// The model contract requires that the first history message has role "user".
// trimToWindowBudget enforces this by dropping any leading groups whose first
// message is not "user" (head orphans) after all budget and extraDrop trimming.
// If that leaves nothing, it returns nil — the system prompt and current user
// turn are still included by the caller.
func trimToWindowBudget(msgs []gateway.Message, tokenBudget int, extraDrop int) []gateway.Message {
	groups := segmentGroups(msgs)
	if len(groups) == 0 {
		return nil
	}

	var kept [][]gateway.Message

	if len(groups) == 1 {
		// Single group: always keep it (budget overridden), then apply the
		// head-orphan guard below.
		kept = groups
	} else {
		// Count token cost of all groups except the first (which we may drop).
		// Work newest → oldest, accumulating the groups that fit.
		keep := len(groups) // how many groups (from the tail) to keep
		tokens := 0
		for i, v := range slices.Backward(groups) {
			groupTokens := 0
			for _, m := range v {
				groupTokens += estimateMessageTokens(m)
			}
			if i == len(groups)-1 {
				// Newest group is always kept regardless of budget.
				tokens += groupTokens
				continue
			}
			if tokens+groupTokens > tokenBudget {
				// This group doesn't fit — drop it and everything older.
				keep = i + 1 // keep groups[i+1 ... len-1]
				break
			}
			tokens += groupTokens
		}

		// Apply extra drops (for harder trim on context-length retries).
		// Never drop below 1 group (the newest).
		keep = max(1, keep-extraDrop)

		startGroup := len(groups) - keep
		if startGroup >= len(groups) {
			// Safety: always return at least the newest group.
			startGroup = len(groups) - 1
		}
		kept = groups[startGroup:]
	}

	// Drop any leading head-orphan group — one whose first message is not
	// "user". Such groups violate the model contract (every history window must
	// start with a "user" message) and arise when persisted history begins with
	// an assistant/tool run or when aggressive trimming leaves only a non-user
	// group at the head. If all groups are orphans (e.g. pure assistant/tool
	// history), we return nil so the turn still proceeds with the system prompt
	// and the current user message.
	for len(kept) > 0 && kept[0][0].Role != "user" {
		kept = kept[1:]
	}

	var out []gateway.Message
	for _, g := range kept {
		out = append(out, g...)
	}
	return out
}

// ── buildSystemPrompt ─────────────────────────────────────────────────────────

// buildSystemPrompt composes the per-turn system prompt from the fixed identity
// section, dynamic user context (time, timezone, locale, language, display name),
// and the empty reserved memory slot.
//
// The prompt is composed fresh each turn so that the time, user profile changes,
// and future memory content are always current.
//
// If the IANA timezone in profile.Timezone cannot be loaded, the time is
// formatted in UTC. The configured timezone string is still included in the
// prompt so operators can see what was set even if it is invalid.
func buildSystemPrompt(now time.Time, profile db.User) string {
	// Resolve the user's timezone; fall back to UTC on parse failure.
	loc, err := time.LoadLocation(profile.Timezone)
	if err != nil {
		loc = time.UTC
	}

	localTime := now.In(loc)
	formattedTime := localTime.Format("2006-01-02 15:04:05 MST")

	var sb strings.Builder

	// ── Identity & behaviour ──────────────────────────────────────────────────
	sb.WriteString("# System\n\n")
	sb.WriteString("You are Qovira, a helpful personal assistant. ")
	sb.WriteString("Be concise and direct. ")
	sb.WriteString("Prefer using tools when they can provide accurate information. ")
	sb.WriteString("Before taking any action that is write, external, or destructive, ")
	sb.WriteString("confirm with the user unless they have already given explicit approval.\n\n")

	// ── User context (dynamic, injected per turn) ─────────────────────────────
	sb.WriteString("## User Context\n\n")
	fmt.Fprintf(&sb, "- Display name: %s\n", profile.DisplayName)
	fmt.Fprintf(&sb, "- Current time: %s\n", formattedTime)
	fmt.Fprintf(&sb, "- Timezone: %s\n", profile.Timezone)
	fmt.Fprintf(&sb, "- Locale: %s\n", profile.Locale)
	fmt.Fprintf(&sb, "- Language: %s\n", profile.Language)
	sb.WriteString("\n")

	// ── Reserved memory slot (empty in v0.1; Tier-1 injects here in v0.2) ────
	sb.WriteString(memorySlotMarker)
	sb.WriteString("\n\n")
	// [v0.2: soul/profile memory will be injected here]

	return sb.String()
}
