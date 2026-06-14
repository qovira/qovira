package harness

// classify.go — error classification for the AI turn loop.
//
// Every error that surfaces inside run() is routed through classify, which maps
// it to one of three fault classes. The loop then acts on the class rather than
// on the raw error value, keeping the classification logic pure and testable.

import (
	"errors"

	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/gateway"
)

// turnFault is the classification of an error encountered during a turn.
// The loop branches on the class; it never switches on raw error values.
type turnFault int

const (
	// faultToolError indicates the error is a *capability.ToolError — a model-visible
	// error the model can self-correct. The harness persists it as the tool result,
	// emits tool.failed, and continues the loop.
	faultToolError turnFault = iota

	// faultContextLength indicates the upstream rejected the request because the
	// input exceeds the model's context window (gateway.ErrContextLength). It is
	// kept distinct so the context-assembly slice can implement prompt trimming
	// and retry. For now the seam routes to handleContextLength.
	faultContextLength

	// faultInfrastructure covers all other errors: bad auth, rate limits, upstream
	// failures, network errors, recovered panics, DB failures, etc. The harness
	// aborts the turn, logs the detail server-side, and emits turn.failed.
	faultInfrastructure
)

// classify maps err to a turnFault. It is a pure function: no I/O, no side effects.
// Callers must pass non-nil errors.
//
// Classification order:
//  1. *capability.ToolError (errors.As) → faultToolError.
//  2. gateway.ErrContextLength (errors.Is) → faultContextLength.
//  3. Everything else (any gateway sentinel, DB error, network failure, recovered
//     panic) → faultInfrastructure. All gateway sentinels other than ErrContextLength
//     map to infrastructure, so no explicit per-sentinel check is needed.
func classify(err error) turnFault {
	// 1. Model-visible tool errors — the model can self-correct.
	var toolErr *capability.ToolError
	if errors.As(err, &toolErr) {
		return faultToolError
	}

	// 2. Context-length exceeded — separate seam for trimming retry.
	if errors.Is(err, gateway.ErrContextLength) {
		return faultContextLength
	}

	// 3. Everything else: gateway sentinels, DB errors, network failures, recovered panics.
	return faultInfrastructure
}
