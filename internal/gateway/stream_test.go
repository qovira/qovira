package gateway_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/gateway"
)

// openFixture opens a testdata file relative to the test source directory and
// registers a cleanup to close it. It fails the test immediately if the file
// cannot be opened.
func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open fixture %q: %v", name, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// ── AC1: Text-only stream ─────────────────────────────────────────────────────

// TestParseSSE_TextOnly verifies that a clean text-only stream produces the
// right ordered TextDeltas followed by a Done chunk with correct Usage.
func TestParseSSE_TextOnly(t *testing.T) {
	t.Parallel()

	chunks, err := gateway.ParseSSE(openFixture(t, "text_only.sse"))
	if err != nil {
		t.Fatalf("ParseSSE: unexpected error: %v", err)
	}

	// Expect three text deltas ("Hello", ", ", "world") + one Done chunk.
	wantDeltas := []string{"Hello", ", ", "world"}
	var gotDeltas []string
	var doneChunks []gateway.Chunk
	for _, c := range chunks {
		if c.Done {
			doneChunks = append(doneChunks, c)
			continue
		}
		if c.TextDelta != "" {
			gotDeltas = append(gotDeltas, c.TextDelta)
		}
	}

	if len(gotDeltas) != len(wantDeltas) {
		t.Fatalf("text deltas: got %v, want %v", gotDeltas, wantDeltas)
	}
	for i, want := range wantDeltas {
		if gotDeltas[i] != want {
			t.Errorf("delta[%d] = %q, want %q", i, gotDeltas[i], want)
		}
	}

	if len(doneChunks) != 1 {
		t.Fatalf("done chunks: got %d, want 1", len(doneChunks))
	}
	done := doneChunks[0]
	if done.Usage == nil {
		t.Fatal("Done chunk missing Usage")
	}
	if done.Usage.PromptTokens != 10 {
		t.Errorf("PromptTokens = %d, want 10", done.Usage.PromptTokens)
	}
	if done.Usage.CompletionTokens != 3 {
		t.Errorf("CompletionTokens = %d, want 3", done.Usage.CompletionTokens)
	}
	if done.Usage.TotalTokens != 13 {
		t.Errorf("TotalTokens = %d, want 13", done.Usage.TotalTokens)
	}
}

// ── AC2: Single tool call fragmented across multiple chunks ───────────────────

// TestParseSSE_SingleToolCall verifies that a tool call fragmented across many
// delta.tool_calls[] chunks is emitted as exactly one complete ToolCall with
// the arguments properly concatenated.
func TestParseSSE_SingleToolCall(t *testing.T) {
	t.Parallel()

	chunks, err := gateway.ParseSSE(openFixture(t, "single_tool_call.sse"))
	if err != nil {
		t.Fatalf("ParseSSE: unexpected error: %v", err)
	}

	var toolChunks []gateway.Chunk
	for _, c := range chunks {
		if c.ToolCall != nil {
			toolChunks = append(toolChunks, c)
		}
	}

	if len(toolChunks) != 1 {
		t.Fatalf("tool-call chunks: got %d, want exactly 1", len(toolChunks))
	}

	tc := toolChunks[0].ToolCall
	if tc.ID != "call_abc123" {
		t.Errorf("ToolCall.ID = %q, want %q", tc.ID, "call_abc123")
	}
	if tc.Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q, want %q", tc.Name, "get_weather")
	}
	wantArgs := `{"location":"Paris"}`
	if string(tc.Arguments) != wantArgs {
		t.Errorf("ToolCall.Arguments = %q, want %q", string(tc.Arguments), wantArgs)
	}

	// Verify exactly one Done chunk.
	var doneCount int
	for _, c := range chunks {
		if c.Done {
			doneCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("done chunks: got %d, want 1", doneCount)
	}
}

// ── AC3: Multiple parallel tool calls ────────────────────────────────────────

// TestParseSSE_ParallelToolCalls verifies that multiple parallel tool calls
// (distinct indexes) are each assembled and emitted whole exactly once.
func TestParseSSE_ParallelToolCalls(t *testing.T) {
	t.Parallel()

	chunks, err := gateway.ParseSSE(openFixture(t, "parallel_tool_calls.sse"))
	if err != nil {
		t.Fatalf("ParseSSE: unexpected error: %v", err)
	}

	var toolChunks []gateway.Chunk
	for _, c := range chunks {
		if c.ToolCall != nil {
			toolChunks = append(toolChunks, c)
		}
	}

	if len(toolChunks) != 2 {
		t.Fatalf("tool-call chunks: got %d, want 2", len(toolChunks))
	}

	// Order of emission must match first-seen index order: index 0, then index 1.
	cases := []struct {
		id       string
		name     string
		wantArgs string
	}{
		{"call_t1", "get_weather", `{"city":"London"}`},
		{"call_t2", "get_time", `{"tz":"UTC"}`},
	}
	for i, want := range cases {
		tc := toolChunks[i].ToolCall
		if tc.ID != want.id {
			t.Errorf("tool[%d].ID = %q, want %q", i, tc.ID, want.id)
		}
		if tc.Name != want.name {
			t.Errorf("tool[%d].Name = %q, want %q", i, tc.Name, want.name)
		}
		if string(tc.Arguments) != want.wantArgs {
			t.Errorf("tool[%d].Arguments = %q, want %q", i, string(tc.Arguments), want.wantArgs)
		}
	}
}

// ── AC3 (cont.): Sparse / out-of-order tool-call indices ──────────────────────

// TestParseSSE_SparseToolCallIndices verifies that tool calls whose indices are
// non-contiguous and first appear out of numeric order (index 2 before index 0)
// are each assembled whole and emitted in first-seen order — exercising the
// map-keyed accumulation rather than a naive contiguous-slice assumption.
func TestParseSSE_SparseToolCallIndices(t *testing.T) {
	t.Parallel()

	chunks, err := gateway.ParseSSE(openFixture(t, "sparse_indices.sse"))
	if err != nil {
		t.Fatalf("ParseSSE: unexpected error: %v", err)
	}

	var toolChunks []gateway.Chunk
	for _, c := range chunks {
		if c.ToolCall != nil {
			toolChunks = append(toolChunks, c)
		}
	}

	if len(toolChunks) != 2 {
		t.Fatalf("tool-call chunks: got %d, want 2", len(toolChunks))
	}

	// First-seen order is index 2 (call_x2) then index 0 (call_x0), not numeric.
	cases := []struct {
		id       string
		name     string
		wantArgs string
	}{
		{"call_x2", "foo", `{"a":1}`},
		{"call_x0", "bar", `{"b":2}`},
	}
	for i, want := range cases {
		tc := toolChunks[i].ToolCall
		if tc.ID != want.id {
			t.Errorf("tool[%d].ID = %q, want %q", i, tc.ID, want.id)
		}
		if tc.Name != want.name {
			t.Errorf("tool[%d].Name = %q, want %q", i, tc.Name, want.name)
		}
		if string(tc.Arguments) != want.wantArgs {
			t.Errorf("tool[%d].Arguments = %q, want %q", i, string(tc.Arguments), want.wantArgs)
		}
	}
}

// TestParseSSE_LargeArgumentLine verifies a single data line carrying a tool-call
// arguments payload larger than bufio.Scanner's 64 KiB default is parsed whole
// rather than misclassified as malformed (the line-buffer ceiling fix). The
// payload is built in-memory so the test needn't ship a multi-hundred-KiB
// fixture.
func TestParseSSE_LargeArgumentLine(t *testing.T) {
	t.Parallel()

	// A ~200 KiB JSON-string value, comfortably past the 64 KiB default.
	big := strings.Repeat("x", 200*1024)
	args := `{"blob":"` + big + `"}`

	argsJSON, err := json.Marshal(args) // JSON-encode the args string so the data line stays valid JSON
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	var b strings.Builder
	b.WriteString(`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_big","type":"function","function":{"name":"store","arguments":`)
	b.Write(argsJSON)
	b.WriteString("}}]},\"finish_reason\":null}]}\n\n")
	b.WriteString(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n")
	b.WriteString("data: [DONE]\n")

	chunks, err := gateway.ParseSSE(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("ParseSSE: unexpected error on large line: %v", err)
	}

	var tool *gateway.ToolCall
	for _, c := range chunks {
		if c.ToolCall != nil {
			tool = c.ToolCall
		}
	}
	if tool == nil {
		t.Fatal("no ToolCall emitted for large-argument stream")
	}
	if tool.ID != "call_big" || tool.Name != "store" {
		t.Errorf("ToolCall id/name = %q/%q, want call_big/store", tool.ID, tool.Name)
	}
	if string(tool.Arguments) != args {
		t.Errorf("ToolCall.Arguments length = %d, want %d (payload truncated or altered)", len(tool.Arguments), len(args))
	}
}

// ── AC4: Missing trailing usage ───────────────────────────────────────────────

// TestParseSSE_NoUsage verifies that a stream without trailing usage terminates
// cleanly and the Done chunk has nil Usage.
func TestParseSSE_NoUsage(t *testing.T) {
	t.Parallel()

	chunks, err := gateway.ParseSSE(openFixture(t, "no_usage.sse"))
	if err != nil {
		t.Fatalf("ParseSSE: unexpected error: %v", err)
	}

	var done *gateway.Chunk
	for i := range chunks {
		if chunks[i].Done {
			done = &chunks[i]
			break
		}
	}
	if done == nil {
		t.Fatal("no Done chunk in output")
	}
	if done.Usage != nil {
		t.Errorf("Done.Usage = %+v, want nil", done.Usage)
	}
}

// ── AC5: Missing [DONE] sentinel ──────────────────────────────────────────────

// TestParseSSE_NoDoneSentinel verifies that a stream that ends without the
// [DONE] sentinel (end-of-body) still terminates cleanly with a Done chunk.
func TestParseSSE_NoDoneSentinel(t *testing.T) {
	t.Parallel()

	chunks, err := gateway.ParseSSE(openFixture(t, "no_done_sentinel.sse"))
	if err != nil {
		t.Fatalf("ParseSSE: unexpected error: %v", err)
	}

	var doneCount int
	for _, c := range chunks {
		if c.Done {
			doneCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("done chunks: got %d, want 1", doneCount)
	}

	// The text delta "OK" must have been captured.
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(c.TextDelta)
	}
	gotText := sb.String()
	if gotText != "OK" {
		t.Errorf("accumulated text = %q, want %q", gotText, "OK")
	}
}

// ── AC6: Keep-alive comment lines ────────────────────────────────────────────

// TestParseSSE_KeepaliveComments verifies that SSE comment lines (lines
// beginning with ':') are silently ignored and do not affect the output.
func TestParseSSE_KeepaliveComments(t *testing.T) {
	t.Parallel()

	chunks, err := gateway.ParseSSE(openFixture(t, "keepalive_comments.sse"))
	if err != nil {
		t.Fatalf("ParseSSE: unexpected error: %v", err)
	}

	var textSB strings.Builder
	var doneCount int
	for _, c := range chunks {
		textSB.WriteString(c.TextDelta)
		if c.Done {
			doneCount++
		}
	}

	if gotText := textSB.String(); gotText != "Hey" {
		t.Errorf("accumulated text = %q, want %q", gotText, "Hey")
	}
	if doneCount != 1 {
		t.Errorf("done chunks: got %d, want 1", doneCount)
	}
}

// ── AC7: Malformed SSE returns distinguishable error ─────────────────────────

// TestParseSSE_MalformedJSON verifies that a data line with invalid JSON
// returns an error wrapping ErrMalformedStream and does not panic.
func TestParseSSE_MalformedJSON(t *testing.T) {
	t.Parallel()

	chunks, err := gateway.ParseSSE(openFixture(t, "malformed_json.sse"))
	if err == nil {
		t.Fatalf("ParseSSE: expected error, got nil; chunks = %+v", chunks)
	}
	if !errors.Is(err, gateway.ErrMalformedStream) {
		t.Errorf("error %v does not wrap ErrMalformedStream", err)
	}
}

// TestParseSSE_MalformedJSONInline exercises the same code path via an inline
// reader so the table-driven approach covers both fixture and inline forms.
func TestParseSSE_MalformedJSONInline(t *testing.T) {
	t.Parallel()

	input := "data: {bad json}\n"
	_, err := gateway.ParseSSE(strings.NewReader(input))
	if err == nil {
		t.Fatal("ParseSSE: expected error, got nil")
	}
	if !errors.Is(err, gateway.ErrMalformedStream) {
		t.Errorf("error %v does not wrap ErrMalformedStream", err)
	}
}

// ── GW1 #1: Zero-arg tool call yields valid JSON arguments ──────────────────

// TestParseSSE_ZeroArgToolCall verifies that a tool call whose argument
// fragments are all empty strings (zero-param tool) emits Arguments that
// round-trips through json.Marshal rather than the invalid json.RawMessage("").
func TestParseSSE_ZeroArgToolCall(t *testing.T) {
	t.Parallel()

	// A minimal stream: the function has no arguments (empty string fragment).
	const ssePayload = "" +
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_z","type":"function","function":{"name":"noop","arguments":""}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n"

	chunks, err := gateway.ParseSSE(strings.NewReader(ssePayload))
	if err != nil {
		t.Fatalf("ParseSSE: unexpected error: %v", err)
	}

	var tool *gateway.ToolCall
	for _, c := range chunks {
		if c.ToolCall != nil {
			tool = c.ToolCall
		}
	}
	if tool == nil {
		t.Fatal("no ToolCall emitted")
	}

	// Arguments must be valid JSON (at minimum "{}") so json.Marshal round-trips.
	if _, jsonErr := json.Marshal(tool); jsonErr != nil {
		t.Errorf("json.Marshal of ToolCall containing Arguments %q failed: %v", string(tool.Arguments), jsonErr)
	}
	// Specifically, the Arguments must be a valid JSON value — not an empty string.
	var dummy any
	if jsonErr := json.Unmarshal(tool.Arguments, &dummy); jsonErr != nil {
		t.Errorf("json.Unmarshal(Arguments=%q) failed: %v", string(tool.Arguments), jsonErr)
	}
	// The canonical value for a zero-arg tool is the empty object.
	if string(tool.Arguments) != `{}` {
		t.Errorf("Arguments = %q, want %q", string(tool.Arguments), `{}`)
	}
}

// ── GW1 #3: Aggregate args-bytes cap and index-count cap ─────────────────────

// TestParseSSE_ArgsCapExceeded verifies that streaming a total argument payload
// that exceeds the aggregate args-bytes cap returns an error wrapping
// ErrMalformedStream instead of buffering unboundedly.
//
// Each individual SSE line stays small (under the per-line 4 MiB cap) but
// many lines accumulate a total that exceeds any sane per-stream aggregate cap.
func TestParseSSE_ArgsCapExceeded(t *testing.T) {
	t.Parallel()

	// Emit 10 MiB total in 1 KiB fragments across 10 240 SSE lines.
	// Any reasonable aggregate cap (e.g. 8 MiB) will be exceeded well
	// before all lines are consumed.
	const fragSize = 1024
	const numFrags = 10 * 1024 // 10 MiB total
	frag := strings.Repeat("x", fragSize)

	var b strings.Builder
	// First fragment establishes the tool call (id + name).
	fmt.Fprintf(&b,
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c0\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"%s\"}}]},\"finish_reason\":null}]}\n\n",
		frag,
	)
	// Subsequent fragments accumulate argument bytes.
	for range numFrags - 1 {
		fmt.Fprintf(&b,
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"%s\"}}]},\"finish_reason\":null}]}\n\n",
			frag,
		)
	}
	b.WriteString(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n")
	b.WriteString("data: [DONE]\n")

	_, err := gateway.ParseSSE(strings.NewReader(b.String()))
	if err == nil {
		t.Fatal("ParseSSE: expected error for oversized args, got nil")
	}
	if !errors.Is(err, gateway.ErrMalformedStream) {
		t.Errorf("error = %v, want wrapping ErrMalformedStream", err)
	}
}

// TestParseSSE_IndexCountCapExceeded verifies that a stream with more distinct
// tool-call indices than maxSSEToolCallIndices returns an error wrapping
// ErrMalformedStream.
func TestParseSSE_IndexCountCapExceeded(t *testing.T) {
	t.Parallel()

	// Generate a stream with far more distinct tool-call indices than any
	// reasonable cap. Use 200 indices — well above any expected cap of 64 or 128.
	const numIndices = 200
	var b strings.Builder
	for i := range numIndices {
		fmt.Fprintf(&b,
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":\"c%d\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n",
			i, i,
		)
	}
	b.WriteString(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n")
	b.WriteString("data: [DONE]\n")

	_, err := gateway.ParseSSE(strings.NewReader(b.String()))
	if err == nil {
		t.Fatal("ParseSSE: expected error for too many tool-call indices, got nil")
	}
	if !errors.Is(err, gateway.ErrMalformedStream) {
		t.Errorf("error = %v, want wrapping ErrMalformedStream", err)
	}
}

// ── AC8: Table-driven fixture tests ──────────────────────────────────────────

// TestParseSSE_Fixtures is the omnibus table-driven test that runs all eight
// acceptance criteria through the fixture files. Each row exercises one
// scenario and verifies the salient contract, supplementing the individual
// tests above.
func TestParseSSE_Fixtures(t *testing.T) {
	t.Parallel()

	type row struct {
		name    string
		fixture string
		check   func(t *testing.T, chunks []gateway.Chunk, err error)
	}

	rows := []row{
		{
			name:    "text only - ordered deltas then done",
			fixture: "text_only.sse",
			check: func(t *testing.T, chunks []gateway.Chunk, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var textSB2 strings.Builder
				for _, c := range chunks {
					textSB2.WriteString(c.TextDelta)
				}
				if text := textSB2.String(); text != "Hello, world" {
					t.Errorf("accumulated text = %q, want %q", text, "Hello, world")
				}
				last := chunks[len(chunks)-1]
				if !last.Done {
					t.Error("last chunk is not Done")
				}
			},
		},
		{
			name:    "single tool call - one complete ToolCall chunk",
			fixture: "single_tool_call.sse",
			check: func(t *testing.T, chunks []gateway.Chunk, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var tcs []*gateway.ToolCall
				for _, c := range chunks {
					if c.ToolCall != nil {
						tcs = append(tcs, c.ToolCall)
					}
				}
				if len(tcs) != 1 {
					t.Fatalf("tool calls: got %d, want 1", len(tcs))
				}
				if tcs[0].Name != "get_weather" {
					t.Errorf("Name = %q, want get_weather", tcs[0].Name)
				}
			},
		},
		{
			name:    "parallel tool calls - two complete ToolCall chunks",
			fixture: "parallel_tool_calls.sse",
			check: func(t *testing.T, chunks []gateway.Chunk, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var tcs []*gateway.ToolCall
				for _, c := range chunks {
					if c.ToolCall != nil {
						tcs = append(tcs, c.ToolCall)
					}
				}
				if len(tcs) != 2 {
					t.Fatalf("tool calls: got %d, want 2", len(tcs))
				}
			},
		},
		{
			name:    "no usage - Done chunk has nil Usage",
			fixture: "no_usage.sse",
			check: func(t *testing.T, chunks []gateway.Chunk, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				for _, c := range chunks {
					if c.Done && c.Usage != nil {
						t.Errorf("Done chunk has non-nil Usage: %+v", c.Usage)
					}
				}
			},
		},
		{
			name:    "no DONE sentinel - terminates with Done chunk",
			fixture: "no_done_sentinel.sse",
			check: func(t *testing.T, chunks []gateway.Chunk, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(chunks) == 0 {
					t.Fatal("no chunks returned")
				}
				last := chunks[len(chunks)-1]
				if !last.Done {
					t.Error("last chunk is not Done")
				}
			},
		},
		{
			name:    "keep-alive comments - ignored",
			fixture: "keepalive_comments.sse",
			check: func(t *testing.T, chunks []gateway.Chunk, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var textSB3 strings.Builder
				for _, c := range chunks {
					textSB3.WriteString(c.TextDelta)
				}
				if text := textSB3.String(); text != "Hey" {
					t.Errorf("text = %q, want Hey", text)
				}
			},
		},
		{
			name:    "malformed JSON - ErrMalformedStream, no panic",
			fixture: "malformed_json.sse",
			check: func(t *testing.T, chunks []gateway.Chunk, err error) {
				t.Helper()
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, gateway.ErrMalformedStream) {
					t.Errorf("error %v does not wrap ErrMalformedStream", err)
				}
				if chunks != nil {
					t.Errorf("chunks should be nil on error, got %v", chunks)
				}
			},
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			f := openFixture(t, r.fixture)
			chunks, err := gateway.ParseSSE(f)
			r.check(t, chunks, err)
		})
	}
}
