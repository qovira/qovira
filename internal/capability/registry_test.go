package capability_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/qovira/qovira/internal/capability"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// makeTool builds a minimal Tool with the given name for use in registry tests.
func makeTool(name string) capability.Tool {
	return capability.Tool{
		Name:        name,
		Description: "test tool " + name,
		Schema:      json.RawMessage(`{}`),
		Risk:        capability.RiskRead,
	}
}

// fakeSource is a minimal Module test double that returns a fixed tool slice.
type fakeSource struct {
	tools []capability.Tool
}

func (f fakeSource) Tools() []capability.Tool { return f.tools }

// collectNames returns the names of the given tools in order.
func collectNames(tools []capability.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}

// ── NewRegistry ───────────────────────────────────────────────────────────────

// TestNewRegistry_IsEmpty verifies that a freshly created Registry has an
// empty catalog.
func TestNewRegistry_IsEmpty(t *testing.T) {
	t.Parallel()

	reg := capability.NewRegistry()
	got := reg.Catalog()
	if len(got) != 0 {
		t.Errorf("Catalog() on new registry = %d tools, want 0", len(got))
	}
}

// ── Add ───────────────────────────────────────────────────────────────────────

// TestAdd_SingleModule_AppearsInCatalog verifies that tools from a single module
// are all present in the catalog after Add.
func TestAdd_SingleModule_AppearsInCatalog(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		tools []capability.Tool
		want  int
	}{
		{"nil tools", nil, 0},
		{"empty slice", []capability.Tool{}, 0},
		{"one tool", []capability.Tool{makeTool("tool_a")}, 1},
		{"three tools", []capability.Tool{makeTool("tool_a"), makeTool("tool_b"), makeTool("tool_c")}, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := capability.NewRegistry()
			src := fakeSource{tools: tc.tools}
			if err := reg.Add(src); err != nil {
				t.Fatalf("Add returned unexpected error: %v", err)
			}

			got := reg.Catalog()
			if len(got) != tc.want {
				t.Errorf("Catalog() len = %d, want %d (tools: %v)", len(got), tc.want, collectNames(got))
			}
		})
	}
}

// TestAdd_MultipleModules_AllToolsInCatalog verifies that adding two distinct
// modules merges their tools into the catalog with no loss.
func TestAdd_MultipleModules_AllToolsInCatalog(t *testing.T) {
	t.Parallel()

	reg := capability.NewRegistry()

	srcA := fakeSource{tools: []capability.Tool{makeTool("tool_a"), makeTool("tool_b")}}
	srcB := fakeSource{tools: []capability.Tool{makeTool("tool_c")}}

	if err := reg.Add(srcA); err != nil {
		t.Fatalf("Add(srcA): %v", err)
	}
	if err := reg.Add(srcB); err != nil {
		t.Fatalf("Add(srcB): %v", err)
	}

	got := reg.Catalog()
	if len(got) != 3 {
		t.Errorf("Catalog() len = %d, want 3; names = %v", len(got), collectNames(got))
	}
}

// TestAdd_DuplicateToolName_ReturnsError verifies that adding two tools with
// the same name returns a non-nil error naming the offending tool.
func TestAdd_DuplicateToolName_ReturnsError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		firstSrc    fakeSource
		secondSrc   fakeSource
		dupToolName string
	}{
		{
			name:        "same module adds duplicate",
			firstSrc:    fakeSource{tools: []capability.Tool{makeTool("tool_a")}},
			secondSrc:   fakeSource{tools: []capability.Tool{makeTool("tool_a")}},
			dupToolName: "tool_a",
		},
		{
			name:        "different modules share a tool name",
			firstSrc:    fakeSource{tools: []capability.Tool{makeTool("ping"), makeTool("pong")}},
			secondSrc:   fakeSource{tools: []capability.Tool{makeTool("ping")}},
			dupToolName: "ping",
		},
		{
			// A single module that returns two tools with the same name must
			// abort on its own Add, not silently collapse them into one entry.
			name:        "single module repeats a name within one batch",
			firstSrc:    fakeSource{tools: nil},
			secondSrc:   fakeSource{tools: []capability.Tool{makeTool("dup"), makeTool("dup")}},
			dupToolName: "dup",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := capability.NewRegistry()
			if err := reg.Add(tc.firstSrc); err != nil {
				t.Fatalf("first Add: unexpected error: %v", err)
			}

			err := reg.Add(tc.secondSrc)
			if err == nil {
				t.Fatal("second Add returned nil error for duplicate tool name; want non-nil")
			}

			// The error must name the offending tool so the startup failure is diagnosable.
			errMsg := err.Error()
			if errMsg == "" {
				t.Error("error message is empty; want it to name the offending tool")
			}
			// Verify the duplicate tool name appears in the error message.
			if !strings.Contains(errMsg, tc.dupToolName) {
				t.Errorf("error %q does not mention the duplicate tool name %q", errMsg, tc.dupToolName)
			}
		})
	}
}

// TestAdd_NilTools_IsNoOp verifies that a module returning nil does not error
// and contributes nothing to the catalog.
func TestAdd_NilTools_IsNoOp(t *testing.T) {
	t.Parallel()

	reg := capability.NewRegistry()
	src := fakeSource{tools: nil}

	if err := reg.Add(src); err != nil {
		t.Fatalf("Add(nil tools): unexpected error: %v", err)
	}

	got := reg.Catalog()
	if len(got) != 0 {
		t.Errorf("Catalog() len = %d, want 0 after nil Tools()", len(got))
	}
}

// ── Catalog ───────────────────────────────────────────────────────────────────

// TestCatalog_ReturnsSnapshot_NotReference verifies that Catalog() returns a
// new slice on each call (snapshot semantics), so the registry is unaffected by
// any mutation of the returned slice.
func TestCatalog_ReturnsSnapshot_NotReference(t *testing.T) {
	t.Parallel()

	reg := capability.NewRegistry()
	src := fakeSource{tools: []capability.Tool{makeTool("tool_a")}}
	if err := reg.Add(src); err != nil {
		t.Fatalf("Add: %v", err)
	}

	snap1 := reg.Catalog()
	// Verify the first snapshot has the expected length.
	if len(snap1) != 1 {
		t.Fatalf("first Catalog() len = %d, want 1", len(snap1))
	}

	// Mutate the returned slice; the registry must be unaffected because Catalog
	// hands back a fresh copy each call.
	snap1[0] = makeTool("mutated")

	snap2 := reg.Catalog()
	if len(snap2) != 1 {
		t.Fatalf("second Catalog() len = %d, want 1 (snapshot must be independent)", len(snap2))
	}
	if snap2[0].Name != "tool_a" {
		t.Errorf("second Catalog()[0].Name = %q, want \"tool_a\" — mutating an earlier snapshot leaked into the registry", snap2[0].Name)
	}
}

// TestCatalog_UniqueNames verifies that Catalog() never returns duplicate names
// when multiple modules are registered (regression guard).
func TestCatalog_UniqueNames(t *testing.T) {
	t.Parallel()

	reg := capability.NewRegistry()

	srcs := []fakeSource{
		{tools: []capability.Tool{makeTool("alpha"), makeTool("beta")}},
		{tools: []capability.Tool{makeTool("gamma")}},
		{tools: []capability.Tool{makeTool("delta"), makeTool("epsilon")}},
	}
	for i, s := range srcs {
		if err := reg.Add(s); err != nil {
			t.Fatalf("Add[%d]: %v", i, err)
		}
	}

	got := reg.Catalog()
	seen := make(map[string]bool, len(got))
	for _, tool := range got {
		if seen[tool.Name] {
			t.Errorf("duplicate tool name %q in Catalog()", tool.Name)
		}
		seen[tool.Name] = true
	}

	if len(got) != 5 {
		t.Errorf("Catalog() len = %d, want 5; names = %v", len(got), collectNames(got))
	}
}

// ── Concurrency ───────────────────────────────────────────────────────────────

// TestRegistry_ConcurrentAccess exercises concurrent Add and Catalog calls so
// the race detector has something to catch if the mutex is missing. The test
// does not assert ordering — only that it does not panic or race.
func TestRegistry_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	reg := capability.NewRegistry()

	const writers = 4
	const toolsPerWriter = 5
	const readers = 8
	const readsPerReader = 20

	var wg sync.WaitGroup

	// Writers: each Add a distinct set of tools.
	for w := range writers {
		wg.Go(func() {
			tools := make([]capability.Tool, toolsPerWriter)
			for i := range toolsPerWriter {
				tools[i] = makeTool(fmt.Sprintf("w%d_tool%d", w, i))
			}
			// Ignore error — concurrent adds may collide in contrived scenarios;
			// we are testing for data races, not error semantics here.
			_ = reg.Add(fakeSource{tools: tools})
		})
	}

	// Readers: each call Catalog in a tight loop.
	for range readers {
		wg.Go(func() {
			for range readsPerReader {
				_ = reg.Catalog()
			}
		})
	}

	wg.Wait()
}

// TestRegistry_ConcurrentAdd_DuplicateDetection verifies that when two goroutines
// concurrently try to Add sources with the same tool name, exactly one succeeds
// and the other returns an error (no lost-update, no panic).
func TestRegistry_ConcurrentAdd_DuplicateDetection(t *testing.T) {
	t.Parallel()

	const iterations = 50

	for range iterations {
		reg := capability.NewRegistry()

		var (
			mu       sync.Mutex
			errCount int
			okCount  int
		)

		var wg sync.WaitGroup

		// Both goroutines add the same tool name.
		for range 2 {
			wg.Go(func() {
				src := fakeSource{tools: []capability.Tool{makeTool("shared_tool")}}
				err := reg.Add(src)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errCount++
				} else {
					okCount++
				}
			})
		}

		wg.Wait()

		// Exactly one should succeed and one should fail.
		if okCount != 1 || errCount != 1 {
			t.Errorf("concurrent Add of duplicate: okCount=%d errCount=%d, want 1/1", okCount, errCount)
		}
	}
}
