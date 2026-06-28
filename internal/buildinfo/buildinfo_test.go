package buildinfo_test

import (
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/buildinfo"
)

// TestResolve_LdflagsWin verifies that when all three ldflags-set variables are non-empty, Resolve returns
// them verbatim without consulting runtime/debug.ReadBuildInfo. This is the stamped-build path (Makefile /
// Dockerfile / CI).
func TestResolve_LdflagsWin(t *testing.T) {
	t.Parallel()

	bi := buildinfo.Resolve(buildinfo.Info{
		Version:   "v1.2.3",
		Commit:    "abc1234",
		BuildTime: "2024-01-15T10:00:00Z",
	})

	if bi.Version != "v1.2.3" {
		t.Errorf("Version: want %q, got %q", "v1.2.3", bi.Version)
	}

	if bi.Commit != "abc1234" {
		t.Errorf("Commit: want %q, got %q", "abc1234", bi.Commit)
	}

	if bi.BuildTime != "2024-01-15T10:00:00Z" {
		t.Errorf("BuildTime: want %q, got %q", "2024-01-15T10:00:00Z", bi.BuildTime)
	}
}

// TestResolve_UnstampedYieldsDevel verifies that when Version is unset (empty string, as when no ldflags are
// passed), Resolve falls back to "(devel)" for the version.
func TestResolve_UnstampedYieldsDevel(t *testing.T) {
	t.Parallel()

	bi := buildinfo.Resolve(buildinfo.Info{})

	if bi.Version != "(devel)" {
		t.Errorf("Version: want %q (devel), got %q", "(devel)", bi.Version)
	}
}

// TestResolve_UnstampedFillsCommitFromVCS verifies that when Commit is unset, Resolve consults
// runtime/debug.ReadBuildInfo and fills Commit with the VCS revision (or "unknown" when not embedded). The
// test binary is built with go test so there may or may not be a VCS revision, but Commit must never be empty
// — it must be either a real revision or "unknown".
func TestResolve_UnstampedFillsCommitFromVCS(t *testing.T) {
	t.Parallel()

	bi := buildinfo.Resolve(buildinfo.Info{})

	if bi.Commit == "" {
		t.Error("Commit: want non-empty (VCS revision or 'unknown'), got empty string")
	}
}

// TestResolve_GoVersionPresent verifies that Resolve fills GoVersion from runtime/debug.ReadBuildInfo even in
// the unstamped path.
func TestResolve_GoVersionPresent(t *testing.T) {
	t.Parallel()

	bi := buildinfo.Resolve(buildinfo.Info{})

	if !strings.HasPrefix(bi.GoVersion, "go") {
		t.Errorf("GoVersion: want 'go...' prefix, got %q", bi.GoVersion)
	}
}

// TestResolve_CommitNeverEmpty verifies that Resolve yields a non-empty Commit for both the stamped
// (ldflags-provided) and unstamped (development) paths. The "-dirty" suffix behaviour is covered by the
// white-box tests in resolve_internal_test.go and shortrev_internal_test.go, which can force the VCS state.
func TestResolve_CommitNeverEmpty(t *testing.T) {
	t.Parallel()

	// Both stamped and unstamped paths must yield a non-empty Commit.
	stamped := buildinfo.Resolve(buildinfo.Info{Commit: "deadbeef"})
	unstamped := buildinfo.Resolve(buildinfo.Info{})

	if stamped.Commit == "" {
		t.Error("stamped Commit: want non-empty, got empty")
	}

	if unstamped.Commit == "" {
		t.Error("unstamped Commit: want non-empty, got empty")
	}
}
