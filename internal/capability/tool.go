package capability

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/qovira/qovira/internal/store"
)

// Result is the return value of a tool call. It must be JSON-marshalable.
// It is the affected entity itself — the same value a REST handler would return
// — so the model references real IDs without translation.
type Result = any

// RiskTier classifies a tool's operational risk. Every tool declares exactly
// one tier, statically, at construction. The harness uses the tier to decide
// whether a tool call requires confirmation before execution.
type RiskTier int

const (
	// RiskRead covers get/list operations. Executed automatically.
	RiskRead RiskTier = iota
	// RiskWrite covers reversible create/update operations. Executed
	// automatically for trusted origins.
	RiskWrite
	// RiskExternal covers operations that send to external systems (e.g. email).
	// Always requires confirmation.
	RiskExternal
	// RiskDestructive covers hard deletes. Requires confirmation and is blocked
	// for untrusted inbound origins.
	RiskDestructive
)

// ToolError is the model-visible error class. It signals a mistake the model
// can correct (bad arguments, a domain-invariant violation) rather than an
// infrastructure failure. The harness feeds it back to the model so the turn
// can continue; infrastructure errors should be returned as plain errors and
// will abort the turn.
//
// Code is a stable, machine-readable slug (e.g. "validation_failed"). Message
// is safe to surface to the model — it must not expose Go internals or
// implementation details.
type ToolError struct {
	Code    string
	Message string
}

// Error implements the error interface.
func (e *ToolError) Error() string {
	return fmt.Sprintf("tool error %s: %s", e.Code, e.Message)
}

// Tool is the type-erased unit the registry stores and the harness consumes.
// Always construct via [NewTool] — never build a Tool literal directly.
//
// Name is a flat snake_case verb-noun identifier (e.g. "create_reminder").
// Description is model-facing: what the tool does and when to use it.
// Schema is a hand-authored JSON Schema for the arguments object.
// Risk is the static risk tier declared at construction.
// Execute runs the tool with the given context, scope, and raw JSON arguments.
// The Execute func owned by NewTool unmarshals args before delegating to the
// typed handler; on a decode failure it returns a *ToolError, never a raw
// decode error.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Risk        RiskTier
	Execute     func(ctx context.Context, scope store.Scope, args json.RawMessage) (Result, error)
}

// NewTool constructs a Tool from a typed handler. The returned Tool.Execute
// owns the decode boundary: it unmarshals args into Args; on failure it returns
// a *ToolError (model-visible, turn continues). On success it calls fn with
// the decoded value.
//
// name is the flat snake_case verb-noun identifier.
// description is the model-facing summary.
// schema is a hand-authored json.RawMessage JSON Schema for the Args object.
// risk is the static risk tier for this tool.
// fn is the typed business-logic handler; its error is passed through as-is.
func NewTool[Args any](
	name, description string,
	schema json.RawMessage,
	risk RiskTier,
	fn func(context.Context, store.Scope, Args) (Result, error),
) Tool {
	return Tool{
		Name:        name,
		Description: description,
		Schema:      schema,
		Risk:        risk,
		Execute: func(ctx context.Context, scope store.Scope, args json.RawMessage) (Result, error) {
			// This boundary catches only syntactic decode failures (malformed JSON,
			// type mismatches). Semantic validation — required fields, value ranges,
			// a bare "null" payload decoding to a zero Args — is the handler's
			// concern and is fleshed out by a later slice.
			var decoded Args
			if err := json.Unmarshal(args, &decoded); err != nil {
				return nil, &ToolError{
					Code:    "validation_failed",
					Message: "the arguments provided could not be parsed; check the schema and try again",
				}
			}
			return fn(ctx, scope, decoded)
		},
	}
}
