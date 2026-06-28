package cli_test

import (
	"io"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/cli"
)

func TestExecute_ServeHelpExitsZero(t *testing.T) {
	t.Parallel()

	code := cli.ExecuteArgsWithOutput([]string{"serve", "--help"}, io.Discard, io.Discard)
	if code != 0 {
		t.Errorf("serve --help: expected exit 0, got %d", code)
	}
}

func TestExecute_ServeHelpContainsLogFlags(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	code := cli.ExecuteArgsWithOutput([]string{"serve", "--help"}, &buf, io.Discard)
	if code != 0 {
		t.Fatalf("serve --help: expected exit 0, got %d", code)
	}

	help := buf.String()
	if !strings.Contains(help, "--log-level") {
		t.Errorf("serve --help output missing --log-level:\n%s", help)
	}

	if !strings.Contains(help, "--log-format") {
		t.Errorf("serve --help output missing --log-format:\n%s", help)
	}
}

// TestExecute_ServeInvalidLogLevelExitsNonZero exercises serve's RunE flag wiring: a bogus --log-level must flow
// through the Changed()-gating into config validation and make the command exit non-zero. This covers the
// cobra→FlagOverrides seam that `serve --help` short-circuits, and fails before the server binds a port.
func TestExecute_ServeInvalidLogLevelExitsNonZero(t *testing.T) {
	t.Parallel()

	code := cli.ExecuteArgsWithOutput([]string{"serve", "--log-level", "bogus"}, io.Discard, io.Discard)
	if code == 0 {
		t.Error("serve --log-level bogus: expected non-zero exit, got 0")
	}
}
