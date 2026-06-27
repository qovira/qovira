# CLAUDE.md

Guidance for Claude Code when working in the `qovira` repository. This is one of several sibling repos under the Qovira workspace: the parent directory's `CLAUDE.md` governs cross-repo rules and is authoritative there; **this file is authoritative for `qovira`'s internals**.

## What this is

The **Qovira application server**: a private, self-hostable personal assistant shipped as a single Go binary that serves a JSON/SSE API and the bundled web UI, backed by an **encrypted SQLite (SQLCipher)** store. It runs on the user's own server against the model endpoint they configure — nothing phones home. It is an **unpublished application, not a package**: no npm release, no Changesets, no semver — it ships as the built binary and the container image.

It consumes the published `@qovira/theme` + `@qovira/ui` libraries for its frontend and the Omnilium `go-sqlcipher` driver via Go modules for its encrypted store.

> **Repository status:** freshly reset to a clean slate. The codebase, build tooling, and the sections below are being rebuilt — keep this file honest as the real layout and commands land.

## Commands

No build tooling exists yet. Once code lands, the standard Go commands apply (`go build ./...`, `go test ./...`); the intended `Makefile`-driven pipeline (SPA build + embed, sqlc generation, lint, Docker image) will be recorded here as it is added. This section is the authority other flows read.

## Layout & build

Not yet scaffolded. The planned shape is a `cmd/qovira` entrypoint delegating to a command tree, wired from `internal/*` packages through one composition root, with the SvelteKit SPA built and embedded into the binary. Fill in the directory map as packages are created.

## Conventions

Follow the `conventions:writing-go` house guide (and the matching `writing-*` guides for SQLite, Docker, and the embedded SPA's frontend stack). Cross-repo rules live in the workspace `CLAUDE.md`; don't restate them here. Record only repo-specific deviations and load-bearing invariants in this file as they emerge.

## Testing

`go test ./...` once the module exists. Write tests first (house TDD discipline): a failing test that fails for the right reason, then the minimal code to pass. "Green" — build, lint, and the full test suite passing — is the bar before pushing to `main` or opening a PR.
