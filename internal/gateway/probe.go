package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ProbeResult carries the outcome of a [Gateway.Probe] call.
//
// Each boolean field is independent and records what the probe actually observed — they are never inferred
// from model names or heuristic tables.
type ProbeResult struct {
	// Reachable is true when the endpoint responded to the /v1/models GET request (even with a non-2xx status
	// code other than a dial/transport error).
	Reachable bool
	// ModelServed is true when the configured model name appears in the /v1/models response list.
	ModelServed bool
	// ToolCalling is true (chat role only) when a minimal streamed chat/completions request with a forced dummy
	// tool call produced a tool_call chunk in the response stream.
	ToolCalling bool
	// Streaming is true (chat role only) when the chat/completions request was accepted over text/event-stream
	// and the SSE stream could be parsed, regardless of whether a tool call was present.
	Streaming bool
	// Err is the first error encountered during the probe, or nil on full success. Auth failures, unreachable
	// endpoints, and configuration errors are all reported here.
	Err error
}

// modelsResponseWire is the minimal OpenAI-compatible shape of the GET /v1/models response — only the model IDs
// are needed for the probe.
type modelsResponseWire struct {
	Data []modelEntryWire `json:"data"`
}

type modelEntryWire struct {
	ID string `json:"id"`
}

// dummyTool is the single tool schema used in the step-2 probe request. Its parameters schema is an empty object
// so the model can produce a valid (though semantically empty) tool call with no arguments.
var dummyTool = ToolSchema{
	Name:        "_probe",
	Description: "Probe tool — not for real use.",
	Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
}

// Probe empirically checks the configured endpoint for the given role and returns a [ProbeResult] describing what
// the probe observed.
//
// For [RoleChat] the probe is two steps:
//  1. GET {baseURL}/v1/models — confirms reachability, authentication, and
//     that the configured model is served.
//  2. A minimal streamed POST to /v1/chat/completions with a single dummy tool
//     and tool_choice "required" — confirms native tool calling and streaming.
//
// For any other role only step 1 is executed; ToolCalling and Streaming remain false.
//
// The probe stops early (without running step 2) when:
//   - the endpoint is unreachable (dial/transport error),
//   - authentication fails (401/403) or any non-2xx response is received, or
//   - the configured model is absent from the /v1/models list.
func (g *Gateway) Probe(ctx context.Context, role Role) ProbeResult {
	resolved, err := g.resolve(ctx, role)
	if err != nil {
		return ProbeResult{Err: err}
	}

	// ── Step 1: GET /v1/models ───────────────────────────────────────────────

	mURL, err := modelsEndpointURL(resolved.BaseURL)
	if err != nil {
		return ProbeResult{Err: fmt.Errorf("gateway: probe build models URL: %w", err)}
	}

	resp, err := g.getJSON(ctx, mURL, resolved.APIKey) //nolint:bodyclose // Body is closed via defer drainClose below on the 2xx path; non-2xx path reads and closes inline.
	if err != nil {
		// Dial/transport failure: endpoint is not reachable.
		return ProbeResult{Err: err}
	}
	defer drainClose(resp.Body)

	// The endpoint responded — it is reachable even if it returns an error status.
	result := ProbeResult{Reachable: true}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Non-2xx: classify and record the error; do not proceed to step 2.
		result.Err = ClassifyResponse(resp.StatusCode, resp.Header, readErrBody(resp.Body))
		return result
	}

	// Parse the model list and check whether the configured model is served.
	var modelsResp modelsResponseWire
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		result.Err = fmt.Errorf("gateway: probe decode models response: %w", err)
		return result
	}

	for _, m := range modelsResp.Data {
		if m.ID == resolved.Model {
			result.ModelServed = true
			break
		}
	}

	if !result.ModelServed {
		// Model absent: we cannot meaningfully probe further, so stop here.
		return result
	}

	// ── Step 2 (chat role only): minimal streamed tool-call probe ────────────

	if role != RoleChat {
		return result
	}

	cURL, err := chatEndpointURL(resolved.BaseURL)
	if err != nil {
		result.Err = fmt.Errorf("gateway: probe build chat URL: %w", err)
		return result
	}

	probeReq := ChatRequest{
		Messages:   []Message{{Role: "user", Content: "Call the probe tool."}},
		Tools:      []ToolSchema{dummyTool},
		ToolChoice: ToolChoiceRequired(),
	}

	wireBody, err := buildWireRequest(probeReq, resolved.Model)
	if err != nil {
		result.Err = fmt.Errorf("gateway: probe build wire request: %w", err)
		return result
	}

	chatResp, err := g.postJSON(ctx, cURL, resolved.APIKey, wireBody) //nolint:bodyclose // Body is closed via defer drainClose below on the 2xx path; non-2xx path reads and closes inline.
	if err != nil {
		// Transport error on the chat step.
		result.Err = err
		return result
	}
	defer drainClose(chatResp.Body)

	if chatResp.StatusCode < 200 || chatResp.StatusCode >= 300 {
		result.Err = ClassifyResponse(chatResp.StatusCode, chatResp.Header, readErrBody(chatResp.Body))
		return result
	}

	// Require text/event-stream to consider the response a streaming endpoint.
	if !strings.HasPrefix(chatResp.Header.Get("Content-Type"), "text/event-stream") {
		// Non-streaming response: Streaming and ToolCalling remain false.
		return result
	}

	chunks, err := ParseSSE(chatResp.Body)
	if err != nil {
		// Malformed stream: streaming partially worked but the content is bad.
		result.Err = fmt.Errorf("%w: probe: %w", ErrUpstreamProtocol, err)
		return result
	}

	// A parseable SSE stream counts as streaming.
	result.Streaming = true

	for _, chunk := range chunks {
		if chunk.ToolCall != nil {
			result.ToolCalling = true
			break
		}
	}

	return result
}
