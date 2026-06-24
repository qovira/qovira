// Package capability defines the construction seam for the capability registry. A Module registers its tools here at
// boot time; the Capability Registry Spec (a later slice) fleshes out the full schema and runtime behaviour.
package capability

// Tool is defined in tool.go. The type declaration lives there alongside Result, RiskTier, ToolError, and NewTool so
// the full tool contract is co-located.

import (
	"fmt"
	"sync"
)

// Module is the built-in tool source: a unit that contributes tools to the registry. app.Module satisfies it
// structurally — no import of package app is needed here.
type Module interface {
	Tools() []Tool
}

// Registry holds the tools contributed by all registered modules. It is safe for concurrent use: built-in modules are
// registered at boot; future dynamic sources (MCP servers, skills) connect at runtime and require concurrent mutation.
//
// The extensibility seam for external sources is:
//
//	AddSource(name string, p ToolProvider) error
//
// where ToolProvider is a not-yet-specified interface whose tools are name-prefixed to prevent collisions with
// first-party built-in tools. That seam is deliberately unimplemented in this slice — designing the external-provider
// contract now would be speculative.
//
// The zero value is not valid; use [NewRegistry].
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool // keyed by tool name for O(1) dedup
}

// NewRegistry constructs and returns an empty, ready-to-use *Registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Add registers a built-in module's tools in the catalog. If the module returns nil or an empty slice, Add is a no-op
// and returns nil. If any tool name from the module is already registered, Add returns an error naming the offending
// tool; the composition root must treat this as a fatal startup error.
//
// The error message names the duplicate tool so the startup failure is diagnosable without inspecting each module's
// source.
func (r *Registry) Add(m Module) error {
	tools := m.Tools()
	if len(tools) == 0 {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Pre-check the whole batch — against the existing catalog AND against itself — before committing any of it, so a
	// collision aborts loudly without partially registering the module. Checking the batch against itself catches a
	// module that hands back two tools with the same name, which would otherwise silently collapse into one entry.
	seen := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		if _, exists := r.tools[t.Name]; exists {
			return fmt.Errorf("capability: duplicate tool name %q", t.Name)
		}
		if _, dup := seen[t.Name]; dup {
			return fmt.Errorf("capability: duplicate tool name %q", t.Name)
		}
		seen[t.Name] = struct{}{}
	}

	// All names are unique; commit the full batch.
	for _, t := range tools {
		r.tools[t.Name] = t
	}

	return nil
}

// Catalog returns a per-call snapshot of the current union of all registered tools. The snapshot is a fresh slice on
// every call so future dynamic sources can change the catalog without affecting snapshots already in flight.
func (r *Registry) Catalog() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.tools) == 0 {
		return nil
	}

	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}

	return out
}
