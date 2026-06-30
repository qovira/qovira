package buildinfo

import (
	"runtime/debug"
	"testing"
)

// TestResolveCore_StampedWins verifies that ldflags-set fields win over any embedded VCS settings, and that an
// already-short stamped commit is left unchanged.
func TestResolveCore_StampedWins(t *testing.T) {
	t.Parallel()

	out := resolve(
		Info{Version: "v1.2.3", Commit: "abc1234", BuildTime: "2024-01-15T10:00:00Z"},
		[]debug.BuildSetting{
			{Key: "vcs.revision", Value: "ffffffffffffffffffffffffffffffffffffffff"},
			{Key: "vcs.time", Value: "2030-01-01T00:00:00Z"},
		},
		"go1.26.4",
	)

	if out.Version != "v1.2.3" {
		t.Errorf("Version: want %q, got %q", "v1.2.3", out.Version)
	}

	if out.Commit != "abc1234" {
		t.Errorf("Commit: want %q, got %q", "abc1234", out.Commit)
	}

	if out.BuildTime != "2024-01-15T10:00:00Z" {
		t.Errorf("BuildTime: want %q, got %q", "2024-01-15T10:00:00Z", out.BuildTime)
	}

	if out.GoVersion != "go1.26.4" {
		t.Errorf("GoVersion: want %q, got %q", "go1.26.4", out.GoVersion)
	}
}

// TestResolveCore_LongStampedCommitShortened verifies that a full 40-char stamped commit (as CI passes via
// github.sha) is abbreviated to the documented short form, so every build mode agrees on a short commit.
func TestResolveCore_LongStampedCommitShortened(t *testing.T) {
	t.Parallel()

	out := resolve(Info{Version: "v1", Commit: "0123456789abcdef0123456789abcdef01234567", BuildTime: "t"}, nil, "go1.26.4")

	if out.Commit != "0123456" {
		t.Errorf("Commit: want %q (short), got %q", "0123456", out.Commit)
	}
}

// TestResolveCore_FallbackFromVCS verifies the unstamped development path: Version becomes "(devel)", Commit is
// the short VCS revision with a "-dirty" suffix when modified, and BuildTime is filled from vcs.time.
func TestResolveCore_FallbackFromVCS(t *testing.T) {
	t.Parallel()

	out := resolve(
		Info{},
		[]debug.BuildSetting{
			{Key: "vcs.revision", Value: "96fd4ef5f1bc2139af33e8b56f35fde372e0be3b"},
			{Key: "vcs.modified", Value: "true"},
			{Key: "vcs.time", Value: "2026-06-28T12:00:00Z"},
		},
		"go1.26.4",
	)

	if out.Version != "(devel)" {
		t.Errorf("Version: want %q, got %q", "(devel)", out.Version)
	}

	if out.Commit != "96fd4ef-dirty" {
		t.Errorf("Commit: want %q, got %q", "96fd4ef-dirty", out.Commit)
	}

	if out.BuildTime != "2026-06-28T12:00:00Z" {
		t.Errorf("BuildTime: want %q (from vcs.time), got %q", "2026-06-28T12:00:00Z", out.BuildTime)
	}

	if out.GoVersion != "go1.26.4" {
		t.Errorf("GoVersion: want %q, got %q", "go1.26.4", out.GoVersion)
	}
}

// TestResolveCore_FallbackNoSettings verifies the bare path with no build info at all (as under `go run`):
// "(devel)", "unknown" commit, empty BuildTime, "unknown" Go version.
func TestResolveCore_FallbackNoSettings(t *testing.T) {
	t.Parallel()

	out := resolve(Info{}, nil, "")

	if out.Version != "(devel)" {
		t.Errorf("Version: want %q, got %q", "(devel)", out.Version)
	}

	if out.Commit != "unknown" {
		t.Errorf("Commit: want %q, got %q", "unknown", out.Commit)
	}

	if out.BuildTime != "" {
		t.Errorf("BuildTime: want empty, got %q", out.BuildTime)
	}

	if out.GoVersion != "unknown" {
		t.Errorf("GoVersion: want %q, got %q", "unknown", out.GoVersion)
	}
}

// TestResolveCore_CleanVCSNoDirtySuffix verifies a clean (unmodified) tree yields no "-dirty" suffix.
func TestResolveCore_CleanVCSNoDirtySuffix(t *testing.T) {
	t.Parallel()

	out := resolve(
		Info{},
		[]debug.BuildSetting{
			{Key: "vcs.revision", Value: "96fd4ef5f1bc2139af33e8b56f35fde372e0be3b"},
			{Key: "vcs.modified", Value: "false"},
		},
		"go1.26.4",
	)

	if out.Commit != "96fd4ef" {
		t.Errorf("Commit: want %q, got %q", "96fd4ef", out.Commit)
	}
}
