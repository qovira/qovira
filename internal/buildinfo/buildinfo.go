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

// Info carries resolved build identity. All fields are guaranteed non-empty after Resolve returns.
type Info struct {
	// Version is the semantic release tag (e.g. "v0.1.0"), or "(devel)" for unstamped builds.
	Version string

	// Commit is the short VCS revision, optionally suffixed with "-dirty" when the working tree was modified.
	// Falls back to "unknown" when the Go toolchain did not embed VCS information.
	Commit string

	// BuildTime is an RFC 3339 timestamp of when the binary was built. For an unstamped `go build` it falls
	// back to the embedded commit timestamp (vcs.time); it is empty when neither exists (e.g. under `go run`),
	// which Huma emits as "" — valid for the optional field.
	BuildTime string

	// GoVersion is the Go toolchain version used to build the binary, sourced from runtime/debug.ReadBuildInfo.
	GoVersion string
}

// Resolve returns a fully-populated Info. When the ldflags vars are set they win (the Commit is normalized to
// its short form). When they are absent (empty strings), the function reads runtime/debug.ReadBuildInfo to
// fill Commit, BuildTime, and GoVersion from the embedded VCS data, and marks Version as "(devel)".
//
// Pass the package-level vars as the seed; this separation makes it testable without touching package globals:
//
//	bi := buildinfo.Resolve(buildinfo.Info{
//	    Version:   buildinfo.Version,
//	    Commit:    buildinfo.Commit,
//	    BuildTime: buildinfo.BuildTime,
//	})
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

	// Version: ldflags value wins; fall back to "(devel)".
	if out.Version == "" {
		out.Version = "(devel)"
	}

	// Commit: an ldflags value wins, otherwise the embedded VCS revision; either way normalized to the short
	// form the field documents. "unknown" when neither is available.
	switch {
	case out.Commit != "":
		out.Commit = shortRevision(out.Commit, false)
	case vcsRevision != "":
		out.Commit = shortRevision(vcsRevision, vcsModified == "true")
	default:
		out.Commit = "unknown"
	}

	// BuildTime: an ldflags value wins; fall back to the commit timestamp the toolchain embedded (vcs.time),
	// so an unstamped `go build` still reports an honest, if approximate, build time. Empty when neither
	// exists (e.g. under `go run`), which serializes as "" — valid for the optional field.
	if out.BuildTime == "" && vcsTime != "" {
		out.BuildTime = vcsTime
	}

	return out
}

// shortCommitLen is the number of leading hex characters kept from a full VCS revision in the development
// fallback, matching git's conventional 7-character abbreviation (and the Commit field's documented example).
// Stamped builds already pass a short revision via `git rev-parse --short`; this only abbreviates the full
// 40-character hash that runtime/debug.ReadBuildInfo embeds.
const shortCommitLen = 7

// shortRevision abbreviates a full VCS revision to shortCommitLen characters and appends "-dirty" when the
// working tree was modified. A revision already at or below shortCommitLen is returned unchanged; an empty
// revision yields an empty string (the caller substitutes "unknown").
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
