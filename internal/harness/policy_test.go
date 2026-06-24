package harness

// policy_test.go — white-box unit tests for the pure policy function and catalog filter. These tests live in
// package harness (not harness_test) to exercise the unexported policy function and filterCatalogForTrust
// directly.

import (
	"testing"

	"github.com/qovira/qovira/internal/capability"
)

// TestPolicy_AllCells verifies the full 4×2 policy matrix:
//
//	Risk          | Trusted | Untrusted
//	──────────────────────────────────
//	RiskRead      | Auto    | Auto
//	RiskWrite     | Auto    | Confirm
//	RiskExternal  | Confirm | Confirm
//	RiskDestructive | Confirm | Block
func TestPolicy_AllCells(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		risk  capability.RiskTier
		trust TrustLevel
		want  Decision
	}{
		{name: "Read/Trusted", risk: capability.RiskRead, trust: Trusted, want: Auto},
		{name: "Read/Untrusted", risk: capability.RiskRead, trust: Untrusted, want: Auto},
		{name: "Write/Trusted", risk: capability.RiskWrite, trust: Trusted, want: Auto},
		{name: "Write/Untrusted", risk: capability.RiskWrite, trust: Untrusted, want: Confirm},
		{name: "External/Trusted", risk: capability.RiskExternal, trust: Trusted, want: Confirm},
		{name: "External/Untrusted", risk: capability.RiskExternal, trust: Untrusted, want: Confirm},
		{name: "Destructive/Trusted", risk: capability.RiskDestructive, trust: Trusted, want: Confirm},
		{name: "Destructive/Untrusted", risk: capability.RiskDestructive, trust: Untrusted, want: Block},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := policy(tt.risk, tt.trust)
			if got != tt.want {
				t.Errorf("policy(%v, %v) = %v, want %v", tt.risk, tt.trust, got, tt.want)
			}
		})
	}
}

// TestDecision_StringValues sanity-checks the iota ordering so refactoring doesn't silently reorder the constants
// (Auto=0, Confirm=1, Block=2).
func TestDecision_StringValues(t *testing.T) {
	t.Parallel()

	if Auto != 0 {
		t.Errorf("Auto = %d, want 0", Auto)
	}
	if Confirm != 1 {
		t.Errorf("Confirm = %d, want 1", Confirm)
	}
	if Block != 2 {
		t.Errorf("Block = %d, want 2", Block)
	}
}

// TestFilterCatalogForTrust_TrustedOrigin verifies that for a Trusted origin the catalog is unchanged (no tools
// are Block-tier for a Trusted caller).
func TestFilterCatalogForTrust_TrustedOrigin(t *testing.T) {
	t.Parallel()

	tools := []capability.Tool{
		{Name: "read_tool", Risk: capability.RiskRead},
		{Name: "write_tool", Risk: capability.RiskWrite},
		{Name: "ext_tool", Risk: capability.RiskExternal},
		{Name: "dest_tool", Risk: capability.RiskDestructive},
	}

	filtered := filterCatalogForTrust(tools, Trusted)
	if len(filtered) != len(tools) {
		t.Errorf("Trusted: filtered catalog len = %d, want %d (no tools should be blocked)", len(filtered), len(tools))
	}
}

// TestFilterCatalogForTrust_UntrustedOrigin verifies that for an Untrusted origin Destructive tools are omitted
// and all other tiers remain.
func TestFilterCatalogForTrust_UntrustedOrigin(t *testing.T) {
	t.Parallel()

	tools := []capability.Tool{
		{Name: "read_tool", Risk: capability.RiskRead},
		{Name: "write_tool", Risk: capability.RiskWrite},
		{Name: "ext_tool", Risk: capability.RiskExternal},
		{Name: "dest_tool_1", Risk: capability.RiskDestructive},
		{Name: "dest_tool_2", Risk: capability.RiskDestructive},
	}

	filtered := filterCatalogForTrust(tools, Untrusted)

	// Destructive tools must be absent.
	for _, t2 := range filtered {
		if t2.Risk == capability.RiskDestructive {
			t.Errorf("Untrusted: destructive tool %q should be filtered out", t2.Name)
		}
	}

	// Read, Write, External must remain.
	wantCount := 3 // read + write + ext
	if len(filtered) != wantCount {
		t.Errorf("Untrusted: filtered catalog len = %d, want %d", len(filtered), wantCount)
	}
}
