package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFindModuleRoot_FindsGoMod verifies that findModuleRoot walks up from a subdirectory of a module and
// returns the directory containing go.mod — not the subdirectory itself.
func TestFindModuleRoot_FindsGoMod(t *testing.T) {
	t.Parallel()

	// Build a temp directory tree: <root>/go.mod  +  <root>/sub/nested/
	// Start findModuleRoot from <root>/sub/nested and expect <root>.
	root := t.TempDir()
	gomod := filepath.Join(root, "go.mod")

	if err := os.WriteFile(gomod, []byte("module example.com/test\n\ngo 1.26.4\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	nested := filepath.Join(root, "sub", "nested")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	got, err := findModuleRoot(nested)
	if err != nil {
		t.Fatalf("findModuleRoot: unexpected error: %v", err)
	}

	if got != root {
		t.Errorf("findModuleRoot: want %q, got %q", root, got)
	}
}

// TestFindModuleRoot_NoGoMod verifies that findModuleRoot returns a descriptive error when no go.mod
// exists in the start directory or any of its ancestors up to the filesystem root.
func TestFindModuleRoot_NoGoMod(t *testing.T) {
	t.Parallel()

	// Create a temp tree with no go.mod anywhere inside it.
	dir := filepath.Join(t.TempDir(), "no-module")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := findModuleRoot(dir)
	if err == nil {
		t.Fatal("findModuleRoot: want error for missing go.mod, got nil")
	}

	if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("error should mention go.mod, got: %v", err)
	}
}

// TestRunWithOutFlag exercises the -out flag path end-to-end: it runs the generator's run() function
// redirecting the output to a temp file and confirms that a valid YAML file containing "openapi: 3.1"
// was written.
func TestRunWithOutFlag(t *testing.T) {
	t.Parallel()

	outFile := filepath.Join(t.TempDir(), "openapi.yaml")

	// Simulate: go run cmd/gen-openapi -out <outFile>
	os.Args = []string{"gen-openapi", "-out", outFile}

	if err := run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("output file is empty")
	}

	if !strings.Contains(string(data), "openapi: 3.1") {
		t.Errorf("output does not contain 'openapi: 3.1'; content prefix: %q", string(data[:min(200, len(data))]))
	}
}
