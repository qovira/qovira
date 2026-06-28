package cli_test

import (
	"io"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/cli"
)

func TestExecute_HelpExitsZero(t *testing.T) {
	t.Parallel()

	// Running with --help should exit 0.
	code := cli.ExecuteArgsWithOutput([]string{"--help"}, io.Discard, io.Discard)
	if code != 0 {
		t.Errorf("--help: expected exit 0, got %d", code)
	}
}

func TestExecute_UnknownCommandExitsNonZero(t *testing.T) {
	t.Parallel()

	code := cli.ExecuteArgsWithOutput([]string{"notacommand"}, io.Discard, io.Discard)
	if code == 0 {
		t.Error("unknown command: expected non-zero exit, got 0")
	}
}

// TestExecute_ErrorWritesToStderr verifies that when the root command fails, the error is surfaced on the
// error writer (not silently swallowed by SilenceErrors) and does not leak onto the normal output writer. A
// bogus flag is the cheapest trigger: it fails in cobra's flag parsing before any RunE runs.
func TestExecute_ErrorWritesToStderr(t *testing.T) {
	t.Parallel()

	var out, errOut strings.Builder
	code := cli.ExecuteArgsWithOutput([]string{"--bogus"}, &out, &errOut)

	if code == 0 {
		t.Fatalf("--bogus: expected non-zero exit, got %d", code)
	}

	stderr := errOut.String()
	if !strings.Contains(stderr, "Error:") {
		t.Errorf("--bogus: expected error output on errOut, got %q", stderr)
	}

	if !strings.Contains(stderr, "unknown flag") {
		t.Errorf("--bogus: expected errOut to name the unknown flag, got %q", stderr)
	}

	if out.Len() != 0 {
		t.Errorf("--bogus: expected nothing on the normal output writer, got %q", out.String())
	}
}

func TestExecute_HelpContainsLogFlags(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	code := cli.ExecuteArgsWithOutput([]string{"--help"}, &buf, io.Discard)
	if code != 0 {
		t.Fatalf("--help: expected exit 0, got %d", code)
	}

	help := buf.String()
	if !strings.Contains(help, "--log-level") {
		t.Errorf("--help output missing --log-level:\n%s", help)
	}

	if !strings.Contains(help, "--log-format") {
		t.Errorf("--help output missing --log-format:\n%s", help)
	}
}
