// Package buildinfo carries build-identity variables stamped by ldflags at compile time, with a fallback to
// runtime/debug.ReadBuildInfo so that a bare `go run` or `go test` still reports something honest. The
// canonical version string comes from the Makefile / Dockerfile / CI via:
//
//	-X github.com/qovira/qovira/internal/buildinfo.Version=<version>
//	-X github.com/qovira/qovira/internal/buildinfo.Commit=<sha>
//	-X github.com/qovira/qovira/internal/buildinfo.BuildTime=<rfc3339>
//
// When those flags are absent (development builds), Resolve fills the gaps from the VCS settings embedded by
// the Go toolchain.
package buildinfo

import (
	"runtime/debug"
)

// Version, Commit, and BuildTime are set by -X ldflags at build time. They are empty strings in unstamped
// (development) builds; Resolve fills in sensible fallbacks in that case.
//
//nolint:gochecknoglobals // ldflags targets must be package-level vars.
var (
	Version   string
	Commit    string
	BuildTime string
)

// Info carries resolved build identity.
type Info struct {
	// Version is the semantic release tag (e.g. "v0.1.0"), or "(devel)" for unstamped builds.
	Version string

	// Commit is the short VCS revision, optionally suffixed with "-dirty" when the working tree was modified.
	// Falls back to "unknown" when the Go toolchain did not embed VCS information.
	Commit string

	// BuildTime is an RFC 3339 timestamp of when the binary was built. For an unstamped `go build` it falls
	// back to the embedded commit timestamp (vcs.time); it is empty when neither exists (e.g. under `go run`).
	BuildTime string

	// GoVersion is the Go toolchain version used to build the binary, sourced from runtime/debug.ReadBuildInfo.
	GoVersion string
}

// Resolve returns a fully-populated Info, reading runtime/debug.ReadBuildInfo for whatever ldflags did not
// stamp. Callers seed it with the package-level vars (passed in rather than read directly, so the fallback
// rules in resolve stay testable without touching globals).
func Resolve(seed Info) Info {
	var (
		settings  []debug.BuildSetting
		goVersion string
	)

	if bi, ok := debug.ReadBuildInfo(); ok {
		settings = bi.Settings
		goVersion = bi.GoVersion
	}

	return resolve(seed, settings, goVersion)
}

// resolve is the pure core of Resolve: it fills seed from the given VCS build settings and Go version. It is
// split out from Resolve — which reads the process's real runtime/debug.ReadBuildInfo — so the fallback logic
// is unit-testable with synthetic settings. The rules: an ldflags-set field wins (the Commit is normalized to
// the short form the field documents); an unset field falls back to the embedded VCS data, or to a sentinel
// ("(devel)", "unknown", "") when nothing is available.
func resolve(seed Info, settings []debug.BuildSetting, goVersion string) Info {
	out := seed

	var vcsRevision, vcsModified, vcsTime string

	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			vcsRevision = s.Value
		case "vcs.modified":
			vcsModified = s.Value
		case "vcs.time":
			vcsTime = s.Value
		}
	}

	out.GoVersion = goVersion
	if out.GoVersion == "" {
		out.GoVersion = "unknown"
	}

	if out.Version == "" {
		out.Version = "(devel)"
	}

	switch {
	case out.Commit != "":
		out.Commit = shortRevision(out.Commit, false)
	case vcsRevision != "":
		out.Commit = shortRevision(vcsRevision, vcsModified == "true")
	default:
		out.Commit = "unknown"
	}

	if out.BuildTime == "" && vcsTime != "" {
		out.BuildTime = vcsTime
	}

	return out
}

// shortCommitLen matches git's conventional 7-char abbreviation. It only abbreviates the full 40-char hash
// runtime/debug.ReadBuildInfo embeds in dev builds; stamped builds already pass a short `git rev-parse --short`.
const shortCommitLen = 7

// shortRevision abbreviates rev to shortCommitLen characters and appends "-dirty" when the tree was modified.
func shortRevision(rev string, dirty bool) string {
	if rev == "" {
		return ""
	}

	short := rev
	if len(short) > shortCommitLen {
		short = short[:shortCommitLen]
	}

	if dirty {
		short += "-dirty"
	}

	return short
}
