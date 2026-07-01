// Command gen-openapi generates the committed OpenAPI 3.1 spec (openapi.yaml) at the repository root by
// reusing the same api.New registration that the production server runs. Running it via go generate (see
// internal/api/generate.go) keeps the artifact and the live server in perfect sync — there is no separate
// schema-definition step. The spec version is pinned to specVersion (not the build's ldflags identity) so the
// committed YAML is byte-identical across machines; see that constant for the rationale.
//
// Output path: the generator walks up from its working directory to find go.mod and writes openapi.yaml
// beside it. An optional -out flag overrides the path (used by the tests to redirect to a temp file).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/qovira/qovira/internal/api"
	"github.com/qovira/qovira/internal/buildinfo"
)

// specVersion is the committed-contract version of the OpenAPI artifact. It is decoupled from the live
// build identity (bi.Version from ldflags) so the committed openapi.yaml is byte-identical on every
// regeneration regardless of the machine, branch, or build stamp. Bump this constant per API-version
// milestone — e.g. when the v0.1 set of endpoints is finalized, or when /v2 ships.
const specVersion = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "gen-openapi: %v\n", err)
		os.Exit(1)
	}
}

// run parses args (the program arguments, excluding the binary name) and generates the spec. Taking args as
// a parameter rather than reading the global os.Args keeps run() pure and lets tests drive it without
// mutating process-global state. A local FlagSet (not flag.CommandLine, which panics on flag redefinition)
// makes run safe to call multiple times in the same process.
func run(args []string) error {
	fs := flag.NewFlagSet("gen-openapi", flag.ContinueOnError)

	var outPath string
	fs.StringVar(&outPath, "out", "", "path to write openapi.yaml (default: module-root/openapi.yaml)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if outPath == "" {
		root, err := findModuleRoot(".")
		if err != nil {
			return fmt.Errorf("locate go.mod: %w", err)
		}

		outPath = filepath.Join(root, "openapi.yaml")
	}

	yaml, err := generateYAML()
	if err != nil {
		return fmt.Errorf("generate spec: %w", err)
	}

	if err := writeFile(outPath, yaml); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}

	fmt.Printf("gen-openapi: wrote %s (%d bytes)\n", outPath, len(yaml))

	return nil
}

// generateYAML builds a throwaway registration using the same api.New code the server runs, marshals the
// resulting OpenAPI 3.1 spec as YAML, and returns the bytes. The mux and logger are discards — only the
// Huma operation registration matters; no HTTP server is started.
func generateYAML() ([]byte, error) {
	mux := http.NewServeMux()
	// Discard logger — the generator does not produce request logs; only the fmt.Printf progress line above.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Only Version reaches the spec (as its title/version); Commit and BuildTime are left empty.
	bi := buildinfo.Info{Version: specVersion}

	ha := api.New(mux, bi, logger)

	yaml, err := ha.OpenAPI().YAML()
	if err != nil {
		return nil, fmt.Errorf("marshal YAML: %w", err)
	}

	return yaml, nil
}

// writeFile writes data to path, creating or truncating the file. A deferred close error is captured and
// promoted when the write itself succeeded, guarding against lost-write on flush.
func writeFile(path string, data []byte) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	_, err = f.Write(data)

	return err
}

// findModuleRoot walks up the directory tree from start, looking for a directory that contains go.mod, and
// returns its absolute path. It returns an error when go.mod is not found anywhere above start (i.e. the
// generator is run outside a Go module — should never happen in normal use).
func findModuleRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}

	dir := abs
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding go.mod.
			return "", errors.New("go.mod not found in any parent directory of " + abs)
		}

		dir = parent
	}
}
