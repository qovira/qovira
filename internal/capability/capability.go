// Package capability defines the construction seam for the capability registry. A Module registers its tools here at
// boot time; the Capability Registry Spec (a later slice) fleshes out the full schema and runtime behaviour.
package capability

// Tool is defined in tool.go. The type declaration lives there alongside Result,
// RiskTier, ToolError, and NewTool so the full tool contract is co-located.

// Registry holds the tools contributed by all registered modules.
// The zero value is not valid; use NewRegistry.
type Registry struct {
	// tools maps a module name to its contributed Tool slice.
	tools map[string][]Tool
}

// NewRegistry constructs and returns an empty, ready-to-use *Registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string][]Tool)}
}

// Add registers tools under the given module name. Calling Add multiple times with the same name appends rather than
// replaces, so modules may call it incrementally. A nil or empty tools slice is a no-op.
func (r *Registry) Add(module string, tools []Tool) {
	if len(tools) == 0 {
		return
	}
	r.tools[module] = append(r.tools[module], tools...)
}

// All returns a flat slice of every registered Tool across all modules. The order follows insertion order per module,
// but the module traversal order is not defined — callers must not rely on it.
func (r *Registry) All() []Tool {
	var all []Tool
	for _, ts := range r.tools {
		all = append(all, ts...)
	}
	return all
}
