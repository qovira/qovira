package harness

// policy.go — trust-gating policy for the AI turn executor.
//
// The policy function is a pure, table-driven function mapping (RiskTier, TrustLevel) to a Decision. It is the
// single source of truth for what the harness does with a tool call; the execute step and catalog filter both
// consult it.
//
// Policy matrix:
//
//	Risk            | Trusted | Untrusted
//	────────────────────────────────────
//	RiskRead        | Auto    | Auto
//	RiskWrite       | Auto    | Confirm
//	RiskExternal    | Confirm | Confirm
//	RiskDestructive | Confirm | Block
//
// In v0.1 only the Trusted column is reachable via ResolveOrigin; the Untrusted column is built, tested, and
// enforced, but no live origin resolves to Untrusted. v0.2 changes only the resolver — policy and the execute
// switch are stable.

import "github.com/qovira/qovira/internal/capability"

// Decision is the outcome of the policy function for a single tool call.
type Decision int

const (
	// Auto means the tool call should be executed immediately, without confirmation.
	Auto Decision = iota
	// Confirm means the tool call requires explicit confirmation before execution. In this slice the seam suspends
	// the turn; the next slice (confirmation-suspend-resume) will persist pending_confirmations, emit
	// confirmation.required, and enable Resolve.
	Confirm
	// Block means the tool call must not be executed and should be refused with a model-visible "not permitted from
	// this source" result so the model can adapt.
	Block
)

// policy returns the execution Decision for a tool call based on the tool's RiskTier and the caller's TrustLevel.
// It is a pure function: no I/O, no side effects.
//
// The full 4×2 matrix is encoded directly so the exhaustive linter stays happy and the logic is readable at a
// glance. The inner TrustLevel switch must be exhaustive over all TrustLevel values; the outer RiskTier default
// catches future unknown tiers.
func policy(risk capability.RiskTier, trust TrustLevel) Decision {
	switch risk {
	case capability.RiskRead:
		// Read is always Auto regardless of trust level.
		return Auto

	case capability.RiskWrite:
		switch trust {
		case Trusted:
			return Auto
		case Untrusted:
			return Confirm
		}

	case capability.RiskExternal:
		// External always requires confirmation regardless of trust level.
		return Confirm

	case capability.RiskDestructive:
		switch trust {
		case Trusted:
			return Confirm
		case Untrusted:
			return Block
		}
	}

	// Unknown risk tier or unknown trust level — treat conservatively as Confirm.
	return Confirm
}

// filterCatalogForTrust returns the subset of tools that should be offered to the model given the caller's trust
// level. This is the advisory catalog filter (Layer 1): it omits tools whose policy(tool.Risk, trust) == Block so
// the model is not tempted to call them. The execution gate (Layer 2) enforces Block even if the model ignores the
// filtered catalog.
func filterCatalogForTrust(tools []capability.Tool, trust TrustLevel) []capability.Tool {
	out := make([]capability.Tool, 0, len(tools))
	for _, t := range tools {
		if policy(t.Risk, trust) != Block {
			out = append(out, t)
		}
	}
	return out
}
