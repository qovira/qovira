//go:build e2e

package gateway

// scripted.go — ScriptedChatter for deterministic E2E testing (e2e build tag only).
//
// ScriptedChatter implements harness.Chatter and emits a deterministic sequence of streaming deltas, tool calls,
// and completion events per turn, driven by a JSON script fixture.  It is physically absent from the default
// binary.
//
// # Script fixture schema
//
// A fixture is a JSON object with a top-level "rules" array.  Each rule has a "match" and a "rounds" array:
//
//	{
//	  "rules": [
//	    {
//	      "match": {
//	        "contains": "create reminder",   // case-insensitive substring match
//	        "prefix":   "delete"             // case-insensitive prefix match
//	      },
//	      "rounds": [
//	        {
//	          "chunks": [
//	            { "textDelta": "Planning…", "delayMs": 5 },
//	            { "toolCall": { "name": "create_reminder", "arguments": {"title":"Buy milk","dueAt":"2026-06-15T09:00:00Z"} } },
//	            { "done": true }
//	          ]
//	        },
//	        {
//	          "chunks": [
//	            { "textDelta": "Done! I created the reminder.", "delayMs": 2 },
//	            { "done": true }
//	          ]
//	        }
//	      ]
//	    }
//	  ]
//	}
//
// Match semantics (evaluated case-insensitively against the latest user message):
//   - "contains": the user message contains the given substring.
//   - "prefix":   the user message starts with the given prefix.
//   - When both fields are non-empty the rule matches when EITHER condition holds.
//   - The first matching rule wins (rules are evaluated in order).
//
// Round selection is stateless: the round index equals the number of non-user (assistant or tool) messages that
// follow the latest user message in the ChatRequest history.  Round 0 is the first response; round 1 is the
// response after a tool result has been added to history; and so on.  This lets a multi-round tool loop
// terminate correctly without any mutable per-conversation state in the scripted provider.
//
// When no rule matches, or the round index exceeds the rule's rounds array, the provider emits a safe default
// reply (a short text delta + Done) rather than hanging, and logs a warning.
//
// # Result templating ($fromResult)
//
// A tool call's arguments may reference values from earlier tool results so that E2E fixtures can, for example,
// create a reminder in one turn and then delete it by its real server-generated id in a later turn.
//
// Reference form — anywhere inside a tool call's arguments JSON, a value may be:
//
//	{"$fromResult": {"callId": "<earlier call id>", "path": "<dot path>"}}
//
// At emit time the scripted provider scans req.Messages for a message with Role=="tool" && ToolCallID==callId
// (last match wins), JSON-parses its Content, traverses the dot-separated path (numeric segments index into
// arrays, e.g. "items.0.id"), and substitutes the resolved JSON value in place.
//
// Example fixture fragment:
//
//	{ "toolCall": { "name": "delete_reminder",
//	  "arguments": { "id": { "$fromResult": { "callId": "c-create-dentist", "path": "id" } } } } }
//
// For the create_reminder tool, the result JSON shape is the full Reminder object returned by the service (same
// shape as the REST response):
//
//	{
//	  "id":           "01JXXXXXXXXXXXXXXXXXXXXX",
//	  "userId":       "...",
//	  "title":        "Dentist",
//	  "dueAt":        "2026-07-01T09:00:00Z",
//	  "tz":           "UTC",
//	  "autoComplete": true,
//	  "status":       "active",
//	  "createdAt":    "...",
//	  "updatedAt":    "..."
//	}
//
// To reference the generated id in a later tool call, use path "id".
//
// IMPORTANT: only tool calls whose id is explicitly set in the fixture (via the "id" field on the toolCall object)
// are referenceable — auto-generated ids (when "id" is omitted) receive a random value that cannot be predicted
// in the fixture. Always set an explicit id on any tool call you intend to reference.
//
// Fail-loud behaviour: if a reference cannot resolve — no matching tool message, malformed result JSON, or a
// path segment that doesn't exist or indexes out of range — Chat yields an error from the iterator and stops.
// Resolution errors are never silent.

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qovira/qovira/internal/id"
)

// ── JSON fixture types ────────────────────────────────────────────────────────

// scriptFixture is the top-level schema for a script fixture JSON file.
type scriptFixture struct {
	Rules []scriptRule `json:"rules"`
}

// scriptRule is one entry in a fixture's rules array.
type scriptRule struct {
	// Match describes how to select this rule from the latest user message.
	Match scriptMatch `json:"match"`
	// Rounds is the ordered sequence of model responses; each round corresponds to one Chat call on the same
	// conversation turn.
	Rounds []scriptRound `json:"rounds"`
}

// scriptMatch describes the match condition for a rule. Contains and Prefix are OR-ed; an empty string means
// "not checked".
type scriptMatch struct {
	// Contains is a case-insensitive substring that must appear in the user message.
	Contains string `json:"contains,omitempty"`
	// Prefix is a case-insensitive prefix that the user message must start with.
	Prefix string `json:"prefix,omitempty"`
}

// scriptRound is one round of model response (one Chat call).
type scriptRound struct {
	// Chunks is the ordered list of Chunk descriptors to emit for this round.
	Chunks []scriptChunk `json:"chunks"`
}

// scriptChunk describes one Chunk to emit.  Exactly one of TextDelta, ToolCall, or Done should be meaningful per
// chunk (mirroring the Chunk type). DelayMs is the time in milliseconds to wait before yielding this chunk, so a
// turn can be observed mid-stream.
type scriptChunk struct {
	// TextDelta is non-empty for text-content chunks.
	TextDelta string `json:"textDelta,omitempty"`
	// ToolCall, when non-nil, describes a tool call to emit.  The ID is auto-generated by ScriptedChatter if not
	// explicitly provided.
	ToolCall *scriptToolCall `json:"toolCall,omitempty"`
	// Done marks the terminal chunk for this round.
	Done bool `json:"done,omitempty"`
	// DelayMs is the number of milliseconds to sleep before yielding this chunk. Keep values small in fixtures (e.g.
	// 1–5 ms) to keep tests fast.
	DelayMs int `json:"delayMs,omitempty"`
}

// scriptToolCall describes the tool call fields within a scriptChunk.
type scriptToolCall struct {
	// ID is the tool call ID.  When empty, one is auto-generated.
	ID string `json:"id,omitempty"`
	// Name is the tool name (e.g. "create_reminder").
	Name string `json:"name"`
	// Arguments is the raw JSON arguments object.  Provided as json.RawMessage so any valid JSON object is accepted
	// without an intermediate type.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ── ScriptedChatter ───────────────────────────────────────────────────────────

// ScriptedChatter implements harness.Chatter with a deterministic scripted response sequence loaded from a JSON
// fixture.  It is safe for concurrent use.
type ScriptedChatter struct {
	rules  []scriptRule
	logger *slog.Logger
}

// NewScriptedChatterFromJSON constructs a ScriptedChatter from raw JSON fixture bytes. Returns an error if the
// JSON is malformed.
func NewScriptedChatterFromJSON(data []byte) (*ScriptedChatter, error) {
	var fix scriptFixture
	if err := json.Unmarshal(data, &fix); err != nil {
		return nil, fmt.Errorf("scripted: parse fixture: %w", err)
	}
	return &ScriptedChatter{
		rules:  fix.Rules,
		logger: slog.Default(),
	}, nil
}

// NewScriptedChatterFromFile constructs a ScriptedChatter by reading a fixture file from the given path.
func NewScriptedChatterFromFile(path string) (*ScriptedChatter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("scripted: read fixture %q: %w", path, err)
	}
	return NewScriptedChatterFromJSON(data)
}

// Chat implements harness.Chatter.  It selects the matching rule and round index from req's message history,
// then returns an iterator that yields each chunk (honouring per-chunk DelayMs and ctx cancellation).
//
// Rule and round selection is entirely stateless — no per-conversation state is kept in ScriptedChatter.  See
// the package-level comment for the algorithm.
//
// $fromResult references in tool call arguments are resolved against req.Messages at emit time.  If a reference
// cannot be resolved the iterator yields an error and stops.
func (sc *ScriptedChatter) Chat(ctx context.Context, req ChatRequest) (iter.Seq2[Chunk, error], error) {
	chunks := sc.selectChunks(req)
	seq := func(yield func(Chunk, error) bool) {
		for _, sc := range chunks {
			// Honour DelayMs before yielding — this lets a consumer observe a turn mid-stream (e.g. Playwright checking
			// SSE events mid-turn).
			if sc.DelayMs > 0 {
				delay := time.Duration(sc.DelayMs) * time.Millisecond
				select {
				case <-ctx.Done():
					yield(Chunk{}, ctx.Err())
					return
				case <-time.After(delay):
				}
			}
			// Check cancellation even when DelayMs == 0.
			select {
			case <-ctx.Done():
				yield(Chunk{}, ctx.Err())
				return
			default:
			}

			out, err := toChunk(sc, req.Messages)
			if err != nil {
				yield(Chunk{}, err)
				return
			}
			if !yield(out, nil) {
				return
			}
		}
	}
	return seq, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// selectChunks picks the scriptChunks to emit for this Chat call. It is the core of the stateless
// round-selection logic.
func (sc *ScriptedChatter) selectChunks(req ChatRequest) []scriptChunk {
	// 1. Find the latest user message index.
	latestUserIdx := -1
	for i, msg := range slices.Backward(req.Messages) {
		if msg.Role == "user" {
			latestUserIdx = i
			break
		}
	}
	if latestUserIdx < 0 {
		sc.logger.Warn("scripted: no user message in request; emitting safe default")
		return defaultChunks()
	}

	latestUserContent := req.Messages[latestUserIdx].Content

	// 2. Compute round index = number of assistant messages that appear AFTER the
	//    latest user message in the history.  Each assistant message (whether or
	//    not it carries tool calls) represents one completed model round; the tool
	//    result messages that follow are part of the same round exchange and do not
	//    advance the counter.  This gives:
	//      - 0 assistant msgs after user → round 0 (first model response)
	//      - 1 assistant msg  after user → round 1 (after a tool-result loop)
	//      - etc.
	roundIdx := 0
	for i := latestUserIdx + 1; i < len(req.Messages); i++ {
		if req.Messages[i].Role == "assistant" {
			roundIdx++
		}
	}

	// 3. Find the first matching rule.
	for _, rule := range sc.rules {
		if matchesRule(rule.Match, latestUserContent) {
			if roundIdx >= len(rule.Rounds) {
				sc.logger.Warn("scripted: round index out of range; emitting safe default",
					"latestUserContent", latestUserContent,
					"roundIdx", roundIdx,
					"rulesLen", len(rule.Rounds),
				)
				return defaultChunks()
			}
			return rule.Rounds[roundIdx].Chunks
		}
	}

	sc.logger.Warn("scripted: no matching rule; emitting safe default",
		"latestUserContent", latestUserContent,
	)
	return defaultChunks()
}

// matchesRule reports whether the rule's match condition is satisfied by the given user message.  Matching is
// case-insensitive.  When both Contains and Prefix are non-empty, either condition suffices.
func matchesRule(m scriptMatch, userMsg string) bool {
	lower := strings.ToLower(userMsg)
	if m.Contains != "" && strings.Contains(lower, strings.ToLower(m.Contains)) {
		return true
	}
	if m.Prefix != "" && strings.HasPrefix(lower, strings.ToLower(m.Prefix)) {
		return true
	}
	return false
}

// toChunk converts a scriptChunk into a gateway Chunk for emission. msgs is the full ChatRequest.Messages slice
// used to resolve $fromResult references in tool call arguments.  A resolution failure is returned as an error;
// the caller must stop iteration and surface it.
func toChunk(chunk scriptChunk, msgs []Message) (Chunk, error) {
	if chunk.ToolCall != nil {
		callID := chunk.ToolCall.ID
		if callID == "" {
			callID = id.New()
		}
		args := chunk.ToolCall.Arguments
		if args == nil {
			args = json.RawMessage(`{}`)
		}

		resolved, err := resolveArguments(args, msgs)
		if err != nil {
			return Chunk{}, err
		}

		return Chunk{
			ToolCall: &ToolCall{
				ID:        callID,
				Name:      chunk.ToolCall.Name,
				Arguments: resolved,
			},
		}, nil
	}
	return Chunk{
		TextDelta: chunk.TextDelta,
		Done:      chunk.Done,
	}, nil
}

// resolveArguments walks the raw JSON arguments, replaces any $fromResult marker objects with the resolved value
// from msgs, and re-marshals the result. Arguments with no $fromResult markers are semantically equivalent to
// the input — object keys may be reordered and whitespace normalised by the decode-and-re-marshal round-trip,
// but all values are preserved faithfully.
func resolveArguments(args json.RawMessage, msgs []Message) (json.RawMessage, error) {
	// Unmarshal into any so we can walk and mutate the tree. UseNumber keeps numeric literals as json.Number so they
	// re-marshal without float64 rounding (an integer id or count survives the round-trip intact).
	var tree any
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("scripted: resolve $fromResult: unmarshal arguments: %w", err)
	}

	resolved, err := resolveNode(tree, msgs)
	if err != nil {
		return nil, err
	}

	out, err := json.Marshal(resolved)
	if err != nil {
		return nil, fmt.Errorf("scripted: resolve $fromResult: re-marshal arguments: %w", err)
	}
	return out, nil
}

// resolveNode recursively walks a decoded JSON value and replaces any map[string]any that carries the "$fromResult"
// marker key with the looked-up value from msgs.
func resolveNode(node any, msgs []Message) (any, error) {
	switch v := node.(type) {
	case map[string]any:
		// Check for the $fromResult marker first — if present, resolve and return the looked-up value directly (do
		// not recurse into it). The marker must be the sole key: a sibling key signals a fixture mistake (a typo,
		// or a misplaced reference) whose siblings would otherwise be silently dropped, so fail loud rather than
		// guess the author's intent.
		if ref, ok := v["$fromResult"]; ok {
			if len(v) != 1 {
				return nil, fmt.Errorf(
					"scripted: resolve $fromResult: marker object must have no sibling keys, found %d",
					len(v),
				)
			}
			return resolveFromResult(ref, msgs)
		}
		// Otherwise recurse into every value.
		out := make(map[string]any, len(v))
		for k, val := range v {
			resolved, err := resolveNode(val, msgs)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil

	case []any:
		out := make([]any, len(v))
		for i, elem := range v {
			resolved, err := resolveNode(elem, msgs)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil

	default:
		// Scalar (string, number, bool, null) — return as-is.
		return v, nil
	}
}

// fromResultRef holds the decoded fields of a $fromResult marker.
type fromResultRef struct {
	CallID string `json:"callId"`
	Path   string `json:"path"`
}

// resolveFromResult decodes the value of the "$fromResult" key and performs the lookup against msgs.  It returns
// the resolved value (any — string/number/bool/object/array) or an error.
func resolveFromResult(ref any, msgs []Message) (any, error) {
	// Re-marshal and re-unmarshal via fromResultRef for type-safe field access.
	refBytes, err := json.Marshal(ref)
	if err != nil {
		return nil, fmt.Errorf("scripted: resolve $fromResult: marshal ref: %w", err)
	}
	var r fromResultRef
	if err := json.Unmarshal(refBytes, &r); err != nil {
		return nil, fmt.Errorf("scripted: resolve $fromResult: decode ref: %w", err)
	}
	if r.CallID == "" {
		return nil, fmt.Errorf("scripted: resolve $fromResult: callId is required")
	}

	// Find the last tool message with ToolCallID == r.CallID.
	content := ""
	found := false
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == r.CallID {
			content = m.Content
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("scripted: resolve $fromResult: no tool result message found for callId %q", r.CallID)
	}

	// Parse the result JSON. UseNumber preserves numeric fidelity through the later re-marshal (see resolveArguments).
	var result any
	rdec := json.NewDecoder(strings.NewReader(content))
	rdec.UseNumber()
	if err := rdec.Decode(&result); err != nil {
		return nil, fmt.Errorf("scripted: resolve $fromResult: parse result content for callId %q: %w", r.CallID, err)
	}

	// A path is required: extracting a specific value is the whole point, and an empty/misspelled path key (decoding
	// to "") must not silently substitute the entire result object.
	if r.Path == "" {
		return nil, fmt.Errorf("scripted: resolve $fromResult: path is required (callId %q)", r.CallID)
	}
	segments := strings.Split(r.Path, ".")
	return traversePath(result, segments, r.CallID, r.Path)
}

// traversePath walks the decoded JSON value following the dot-separated path segments.  Numeric segments index
// into arrays.
func traversePath(node any, segments []string, callID, fullPath string) (any, error) {
	if len(segments) == 0 {
		return node, nil
	}
	seg := segments[0]
	rest := segments[1:]

	switch v := node.(type) {
	case map[string]any:
		child, ok := v[seg]
		if !ok {
			return nil, fmt.Errorf(
				"scripted: resolve $fromResult: path %q: key %q not found in object (callId %q)",
				fullPath, seg, callID,
			)
		}
		return traversePath(child, rest, callID, fullPath)

	case []any:
		idx, err := strconv.Atoi(seg)
		if err != nil {
			return nil, fmt.Errorf(
				"scripted: resolve $fromResult: path %q: segment %q is not a valid array index (callId %q)",
				fullPath, seg, callID,
			)
		}
		if idx < 0 || idx >= len(v) {
			return nil, fmt.Errorf(
				"scripted: resolve $fromResult: path %q: array index %d out of range [0, %d) (callId %q)",
				fullPath, idx, len(v), callID,
			)
		}
		return traversePath(v[idx], rest, callID, fullPath)

	default:
		return nil, fmt.Errorf(
			"scripted: resolve $fromResult: path %q: cannot index into %T with segment %q (callId %q)",
			fullPath, node, seg, callID,
		)
	}
}

// defaultChunks returns the safe-default chunk sequence emitted when no rule matches or the round index is out
// of range.
func defaultChunks() []scriptChunk {
	return []scriptChunk{
		{TextDelta: "[scripted provider: no matching script for this turn]"},
		{Done: true},
	}
}
