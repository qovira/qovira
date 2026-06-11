package cli

import (
	"bytes"
	"strings"
	"testing"
)

// runCmd builds a fresh root command, sets args, captures stdout+stderr, and
// returns (stdout, stderr, error). It is a test helper shared by all table
// rows.
func runCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var outBuf, errBuf bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)

	err := cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

// TestRootHelp verifies that "qovira --help" lists all three top-level
// commands.
func TestRootHelp(t *testing.T) {
	t.Parallel()

	want := []string{"serve", "migrate", "version"}

	out, _, err := runCmd(t, "--help")
	if err != nil {
		t.Fatalf("--help returned error: %v", err)
	}

	for _, sub := range want {
		if !strings.Contains(out, sub) {
			t.Errorf("--help output missing %q\ngot:\n%s", sub, out)
		}
	}
}

// TestMigrateHelp verifies that "qovira migrate --help" lists up, status, and
// down subcommands.
func TestMigrateHelp(t *testing.T) {
	t.Parallel()

	want := []string{"up", "status", "down"}

	out, _, _ := runCmd(t, "migrate", "--help")

	for _, sub := range want {
		if !strings.Contains(out, sub) {
			t.Errorf("migrate --help output missing %q\ngot:\n%s", sub, out)
		}
	}
}

// TestVersionOutput verifies that "qovira version" prints each piece of
// injected build metadata.
//
// This test mutates package-level vars so it must NOT be marked t.Parallel()
// at the top level; the subtests only read the captured output string and are
// safe to run in parallel with each other.
func TestVersionOutput(t *testing.T) {
	// Restore the package-level vars when the test and all its subtests finish.
	origVersion, origCommit, origDate := version, commit, date
	t.Cleanup(func() {
		version, commit, date = origVersion, origCommit, origDate
	})

	version = "1.2.3"
	commit = "abc1234"
	date = "2026-01-01"

	out, _, err := runCmd(t, "version")
	if err != nil {
		t.Fatalf("version returned error: %v", err)
	}

	cases := []struct {
		field string
		want  string
	}{
		{"version", "1.2.3"},
		{"commit", "abc1234"},
		{"date", "2026-01-01"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			// out is captured before entering subtests; safe to read in parallel.
			t.Parallel()
			if !strings.Contains(out, tc.want) {
				t.Errorf("version output missing %s %q\ngot: %s", tc.field, tc.want, out)
			}
		})
	}
}

// TestVersionDefaults verifies that when build vars are not overridden the
// default sentinel values appear in the output.
//
// Mutates package-level vars — must NOT be marked t.Parallel().
func TestVersionDefaults(t *testing.T) {
	origVersion, origCommit, origDate := version, commit, date
	t.Cleanup(func() {
		version, commit, date = origVersion, origCommit, origDate
	})

	version, commit, date = "dev", "none", "unknown"

	out, _, err := runCmd(t, "version")
	if err != nil {
		t.Fatalf("version returned error: %v", err)
	}

	for _, want := range []string{"dev", "none", "unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("version defaults output missing %q\ngot: %s", want, out)
		}
	}
}

// TestCommandWiring is a table-driven test that checks each command path
// returns the expected exit behaviour (success or a known error string).
func TestCommandWiring(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "version succeeds",
			args:    []string{"version"},
			wantErr: false,
		},
		{
			name:    "serve stub returns error",
			args:    []string{"serve"},
			wantErr: true,
			errMsg:  "not yet implemented",
		},
		{
			name:    "migrate up stub returns error",
			args:    []string{"migrate", "up"},
			wantErr: true,
			errMsg:  "not yet implemented",
		},
		{
			name:    "migrate status stub returns error",
			args:    []string{"migrate", "status"},
			wantErr: true,
			errMsg:  "not yet implemented",
		},
		{
			name:    "migrate down stub returns error",
			args:    []string{"migrate", "down"},
			wantErr: true,
			errMsg:  "not yet implemented",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := runCmd(t, tc.args...)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errMsg)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestExecuteExitCode verifies that execute() returns 0 for success and 1 for
// a command error, AND that a failing command writes a diagnostic to stderr
// (the root sets SilenceErrors, so execute() must print it — otherwise every
// command, including the real bodies added later, would fail silently).
func TestExecuteExitCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr bool
	}{
		{"version exits 0 with no stderr", []string{"version"}, 0, false},
		{"unknown command exits 1 with diagnostic", []string{"nonexistent"}, 1, true},
		{"serve stub exits 1 with diagnostic", []string{"serve"}, 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var outBuf, errBuf bytes.Buffer
			cmd := newRootCmd()
			cmd.SetOut(&outBuf)
			cmd.SetErr(&errBuf)
			cmd.SetArgs(tc.args)

			code := execute(cmd)

			if code != tc.wantCode {
				t.Errorf("args %v: got exit code %d, want %d", tc.args, code, tc.wantCode)
			}
			if gotStderr := errBuf.Len() > 0; gotStderr != tc.wantStderr {
				t.Errorf("args %v: stderr written = %v (%q), want %v", tc.args, gotStderr, errBuf.String(), tc.wantStderr)
			}
			if tc.wantStderr && !strings.Contains(errBuf.String(), "qovira:") {
				t.Errorf("args %v: stderr %q missing diagnostic prefix %q", tc.args, errBuf.String(), "qovira:")
			}
		})
	}
}
