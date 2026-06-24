package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
)

// ── Public contract types ─────────────────────────────────────────────────────

// ChatRequest is the caller-facing input to [Gateway.Chat].
//
// Temperature and MaxTokens are pointers so that a nil value means "omit the field entirely" and let the upstream
// apply its own default, as opposed to explicitly sending zero.
type ChatRequest struct {
	Messages    []Message
	Tools       []ToolSchema
	ToolChoice  ToolChoice
	Temperature *float64
	MaxTokens   *int
}

// Message is one entry in the conversation history sent to the model.
//
// Role must be one of the OpenAI-compatible role strings: "system", "user", "assistant", or "tool". For assistant
// messages that include tool calls, set ToolCalls. For tool-result messages, set Role="tool" and ToolCallID to the ID
// of the call being answered.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolSchema describes one tool that the model may call.
type ToolSchema struct {
	Name        string
	Description string
	// Parameters is the JSON Schema of the tool's input, passed through verbatim to the upstream endpoint.
	Parameters json.RawMessage
}

// ToolChoice expresses the caller's preference for tool use. The zero value maps to "auto" (the upstream decides
// whether to use a tool). Use the [ToolChoiceNone], [ToolChoiceAuto], [ToolChoiceRequired], and [ToolChoiceNamed]
// constructors rather than building the struct directly.
//
// Wire mapping:
//   - mode == "auto"     → JSON string "auto"
//   - mode == "none"     → JSON string "none"
//   - mode == "required" → JSON string "required"
//   - mode == "function" → JSON object {"type":"function","function":{"name":"…"}}
//
// The zero value (mode == "") is treated identically to ToolChoiceAuto by [marshalToolChoice]; the field is omitted
// from the request body entirely when no tools are present.
type ToolChoice struct {
	mode string // "auto" | "none" | "required" | "function"
	name string // set when mode == "function"
}

// ToolChoiceAuto lets the upstream decide whether to call a tool. This is the
// default — a zero [ToolChoice] behaves identically.
func ToolChoiceAuto() ToolChoice { return ToolChoice{mode: "auto"} }

// ToolChoiceNone instructs the upstream not to call any tool.
func ToolChoiceNone() ToolChoice { return ToolChoice{mode: "none"} }

// ToolChoiceRequired instructs the upstream to call at least one tool.
func ToolChoiceRequired() ToolChoice { return ToolChoice{mode: "required"} }

// ToolChoiceNamed instructs the upstream to call the tool with the given name.
func ToolChoiceNamed(name string) ToolChoice { return ToolChoice{mode: "function", name: name} }

// ── Wire serialisation ────────────────────────────────────────────────────────

// chatRequestWire is the JSON shape sent to the upstream endpoint.
type chatRequestWire struct {
	Model       string          `json:"model"`
	Messages    []messageWire   `json:"messages"`
	Stream      bool            `json:"stream"`
	Tools       []toolWire      `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
}

type messageWire struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []toolCallWire `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type toolCallWire struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function toolFunctionWire `json:"function"`
}

type toolFunctionWire struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolWire struct {
	Type     string         `json:"type"`
	Function toolSchemaWire `json:"function"`
}

type toolSchemaWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// marshalToolChoice serialises a ToolChoice into the wire shape expected by OpenAI-compatible endpoints. The zero value
// (mode == "" or mode == "auto") produces the JSON string "auto".
func marshalToolChoice(tc ToolChoice) (json.RawMessage, error) {
	mode := tc.mode
	if mode == "" {
		mode = "auto"
	}

	switch mode {
	case "auto", "none", "required":
		b, err := json.Marshal(mode)
		if err != nil {
			return nil, err
		}
		return b, nil
	case "function":
		v := struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}{
			Type: "function",
		}
		v.Function.Name = tc.name
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return b, nil
	default:
		return nil, fmt.Errorf("gateway: unknown tool_choice mode %q", tc.mode)
	}
}

// buildWireRequest converts a [ChatRequest] and a resolved model name into the JSON bytes sent to the upstream
// chat/completions endpoint.
func buildWireRequest(req ChatRequest, model string) ([]byte, error) {
	wire := chatRequestWire{
		Model:       model,
		Stream:      true,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}

	// Map messages.
	wire.Messages = make([]messageWire, len(req.Messages))
	for i, m := range req.Messages {
		wm := messageWire{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			wm.ToolCalls = make([]toolCallWire, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				wm.ToolCalls[j] = toolCallWire{
					ID:   tc.ID,
					Type: "function",
					Function: toolFunctionWire{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				}
			}
		}
		wire.Messages[i] = wm
	}

	// Map tools.
	if len(req.Tools) > 0 {
		wire.Tools = make([]toolWire, len(req.Tools))
		for i, ts := range req.Tools {
			wire.Tools[i] = toolWire{
				Type:     "function",
				Function: toolSchemaWire(ts),
			}
		}

		// Map tool_choice only when tools are present.
		tc, err := marshalToolChoice(req.ToolChoice)
		if err != nil {
			return nil, err
		}
		wire.ToolChoice = tc
	}

	b, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("gateway: marshal chat request: %w", err)
	}
	return b, nil
}

// ── Chat ──────────────────────────────────────────────────────────────────────

// Chat sends req to the configured chat endpoint and returns a streaming iterator over the response chunks.
//
// Two-phase error model:
//   - A non-nil error returned directly (setup error) means the request was never accepted: the gateway is
//     unconfigured, the request could not be built, or the upstream rejected it (non-2xx after exhausting retries). In
//     this case the returned iterator is nil.
//   - A nil error with a non-nil iterator means the upstream accepted the request (2xx). Subsequent errors — transport
//     breaks, malformed SSE, idle or first-token timeouts — surface as the error value in each (Chunk, error) yield
//     from the iterator. The iterator always yields at most one non-nil error, after which ranging over it terminates.
//
// Chat applies resilience policies (pre-first-token retry with jittered backoff, first-token timeout, idle timeout) as
// configured by the Gateway's [ResilienceConfig].
//
// The caller must range over the iterator to completion (or break early) to ensure the response body is closed. The
// body is closed automatically when the iterator is exhausted or the consumer breaks.
func (g *Gateway) Chat(ctx context.Context, req ChatRequest) (iter.Seq2[Chunk, error], error) {
	return g.chatWithResilience(ctx, req, g.resilienceCfg)
}
