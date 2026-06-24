package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrMalformedStream is returned by ParseSSE when a data line contains text that is not valid JSON, or when a
// single line exceeds maxSSELineBytes. The caller (a later call-slice) is responsible for mapping this onto a
// broader gateway error type.
var ErrMalformedStream = errors.New("gateway: malformed SSE stream")

// maxSSELineBytes caps the size of a single SSE data line. One line carries an entire chunk's JSON, and a tool
// call streaming a large arguments payload can produce a line well past bufio.Scanner's 64 KiB default — so the
// default would misclassify a valid stream as malformed and silently drop data. The cap is raised generously
// while staying bounded, so a runaway or hostile stream still can't force unbounded buffering.
const maxSSELineBytes = 4 << 20 // 4 MiB

// maxSSETotalArgsBytes is the aggregate cap on the total bytes written across ALL in-flight tool-call argument
// builders in a single stream. Each individual line is already capped at maxSSELineBytes, but a hostile or
// malfunctioning upstream that keeps making progress within IdleTimeout can drive unbounded memory via many
// small lines. This cap limits the total to 8 MiB — generous for any real model output, but bounded against
// adversarial streams.
const maxSSETotalArgsBytes = 8 << 20 // 8 MiB

// maxSSEToolCallIndices is the maximum number of distinct tool-call indices allowed in a single stream. The inFlight
// map and order slice are bounded by this constant so a hostile stream cannot grow them without limit.
const maxSSEToolCallIndices = 128

// Chunk is one unit of streamed output from the model. Exactly one of TextDelta, ToolCall, or Done is meaningful
// per chunk — they are never set simultaneously.
//
//   - TextDelta is non-empty on text-content delta chunks.
//   - ToolCall is non-nil when a fully assembled tool call is ready to deliver.
//   - Done is true on the terminal chunk; Usage may be set on that chunk if the
//     upstream endpoint included trailing usage data.
type Chunk struct {
	TextDelta string
	ToolCall  *ToolCall
	Done      bool
	Usage     *Usage
}

// ToolCall carries one complete, assembled tool invocation. Arguments is the raw, concatenated JSON fragment
// string exactly as sent by the model — it is never validated or parsed.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Usage carries token-count metadata emitted at the end of the stream.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ── SSE wire types ────────────────────────────────────────────────────────────

// sseChunk is the deserialized shape of one SSE data line from an OpenAI-compatible streaming endpoint.
type sseChunk struct {
	Choices []sseChoice `json:"choices"`
	Usage   *sseUsage   `json:"usage"`
}

type sseChoice struct {
	Delta        sseDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type sseDelta struct {
	Content   *string       `json:"content"`
	ToolCalls []sseToolCall `json:"tool_calls"`
}

type sseToolCall struct {
	Index    int             `json:"index"`
	ID       string          `json:"id"`
	Function sseToolFunction `json:"function"`
}

type sseToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type sseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ── inFlight tracks an in-progress tool call being assembled ─────────────────

type inFlightToolCall struct {
	id   string
	name string
	args strings.Builder
}

// streamSSE reads an OpenAI-compatible SSE byte stream from r and calls emit for each assembled Chunk. It returns
// as soon as emit returns false (consumer stopped), or after all chunks including the terminal Done chunk have
// been emitted, or on the first parse/scan error.
//
// On a scan or JSON error the function returns an error wrapping [ErrMalformedStream]. A normal end-of-stream
// (with or without a "[DONE]" sentinel) returns nil after emitting the Done chunk — even when emit returns true
// for every chunk.
//
// The Done chunk is always the last thing emitted; emit is never called after it returns false or after Done
// has been emitted.
func streamSSE(r io.Reader, emit func(Chunk) bool) error {
	scanner := bufio.NewScanner(r)
	// Raise the line ceiling above bufio's 64 KiB default so a large but valid tool-call arguments line isn't misread
	// as malformed (see maxSSELineBytes).
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)

	// inFlight maps tool-call index → assembler.
	inFlight := make(map[int]*inFlightToolCall)
	// order preserves the first-seen order of tool-call indices for deterministic output.
	var order []int
	// totalArgsBytes tracks the aggregate bytes written across all in-flight argument builders. It is checked against
	// maxSSETotalArgsBytes on every fragment write so a hostile stream cannot force unbounded allocation.
	var totalArgsBytes int

	var finalUsage *sseUsage

	for scanner.Scan() {
		line := scanner.Text()

		// SSE comment lines (keep-alive, heartbeat) — skip silently.
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Empty lines are the SSE event separator — nothing to do.
		if line == "" {
			continue
		}

		// Only "data:" lines carry payload. Per the SSE spec the single space after the colon is optional, so accept
		// both "data: {…}" (space) and "data:{…}" (no space) by stripping "data:" then removing at most one leading
		// space from the remainder.
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			// Non-data, non-comment, non-empty — not part of the OpenAI SSE contract; skip rather than error so
			// unknown fields don't break.
			continue
		}
		payload := strings.TrimPrefix(rest, " ")

		// A bare "data:" line (empty payload after optional space strip) is legal SSE and must be skipped, not passed
		// to json.Unmarshal which would error.
		if payload == "" {
			continue
		}

		// Terminal sentinel — stream is done.
		if payload == "[DONE]" {
			break
		}

		var wire sseChunk
		if err := json.Unmarshal([]byte(payload), &wire); err != nil {
			return fmt.Errorf("%w: %w", ErrMalformedStream, err)
		}

		// Accumulate trailing usage when provided.
		if wire.Usage != nil {
			finalUsage = wire.Usage
		}

		// v0.1 chat streaming is single-choice (n=1). All tool-call fragments are keyed by tc.Index alone, so an n>1
		// stream sharing indices across choices is out of scope and not supported here.
		for _, choice := range wire.Choices {
			delta := choice.Delta

			// Text content delta. Empty-string content (e.g. the role-priming first chunk) is intentionally elided so
			// it doesn't surface as a spurious empty TextDelta chunk.
			if delta.Content != nil && *delta.Content != "" {
				if !emit(Chunk{TextDelta: *delta.Content}) {
					return nil
				}
			}

			// Tool-call delta fragments — accumulate by index.
			for _, tc := range delta.ToolCalls {
				ifl, exists := inFlight[tc.Index]
				if !exists {
					// Cap the number of distinct tool-call indices before allocating a new assembler.
					if len(inFlight) >= maxSSEToolCallIndices {
						return fmt.Errorf("%w: too many tool-call indices (limit %d)", ErrMalformedStream, maxSSEToolCallIndices)
					}
					ifl = &inFlightToolCall{}
					inFlight[tc.Index] = ifl
					order = append(order, tc.Index)
				}
				// id and name arrive in the first fragment; subsequent fragments have empty strings.
				if tc.ID != "" {
					ifl.id = tc.ID
				}
				if tc.Function.Name != "" {
					ifl.name = tc.Function.Name
				}
				// Enforce the aggregate args-bytes cap before writing.
				if frag := tc.Function.Arguments; frag != "" {
					totalArgsBytes += len(frag)
					if totalArgsBytes > maxSSETotalArgsBytes {
						return fmt.Errorf("%w: tool-call arguments exceed aggregate limit of %d bytes", ErrMalformedStream, maxSSETotalArgsBytes)
					}
					ifl.args.WriteString(frag)
				}
			}

			// finish_reason signals the end of the choice sequence. Flush all in-flight tool calls in first-seen
			// index order, then set up the terminal chunk.
			if choice.FinishReason != nil {
				for _, idx := range order {
					ifl := inFlight[idx]
					// Normalise empty argument strings to the valid JSON empty object "{}". An empty
					// json.RawMessage("") is not valid JSON and would cause json.Marshal of any containing value
					// to fail with "unexpected end of JSON input".
					args := ifl.args.String()
					var rawArgs json.RawMessage
					if args == "" {
						rawArgs = json.RawMessage("{}")
					} else {
						rawArgs = json.RawMessage(args)
					}
					tc := &ToolCall{
						ID:        ifl.id,
						Name:      ifl.name,
						Arguments: rawArgs,
					}
					if !emit(Chunk{ToolCall: tc}) {
						return nil
					}
				}
				// Clear the in-flight map so a second finish_reason (unlikely but tolerated) doesn't double-emit.
				// Reset the byte counter so the cap reflects currently-buffered allocation, not a cumulative total
				// across multiple finish_reason rounds.
				clear(inFlight)
				order = order[:0]
				totalArgsBytes = 0
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: scanner: %w", ErrMalformedStream, err)
	}

	// Build the terminal Done chunk. Usage is attached if the stream provided it.
	done := Chunk{Done: true}
	if finalUsage != nil {
		done.Usage = &Usage{
			PromptTokens:     finalUsage.PromptTokens,
			CompletionTokens: finalUsage.CompletionTokens,
			TotalTokens:      finalUsage.TotalTokens,
		}
	}
	emit(done)

	return nil
}

// ParseSSE reads an OpenAI-compatible SSE byte stream from r and returns a slice of Chunk values representing the
// parsed output.
//
// The function is a pure transformation of the byte stream — it holds no HTTP state and performs no I/O beyond
// reading r.
//
// Tolerances:
//   - Lines beginning with ':' (SSE comment / keep-alive) are silently ignored.
//   - End-of-body (io.EOF without a preceding "[DONE]") is treated as normal
//     termination; the Done chunk is still emitted.
//   - Missing trailing usage is accepted; the Done chunk's Usage field is nil.
//   - Sparse or non-contiguous tool-call indices are supported via a map.
//   - Multiple parallel tool calls (distinct index values) are assembled
//     independently and each emitted whole exactly once when the stream ends.
//
// A data line whose JSON payload is unparseable causes an immediate return of a nil slice and an error wrapping
// [ErrMalformedStream].
func ParseSSE(r io.Reader) ([]Chunk, error) {
	var chunks []Chunk
	err := streamSSE(r, func(c Chunk) bool {
		chunks = append(chunks, c)
		return true
	})
	if err != nil {
		return nil, err
	}
	return chunks, nil
}
