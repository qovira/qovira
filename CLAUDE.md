# CLAUDE.md

Guidance for Claude Code when working in the `qovira` repository. This is one of several sibling repos under the Qovira workspace: the parent directory's `CLAUDE.md` governs cross-repo rules and is authoritative there; **this file is authoritative for `qovira`'s internals**.

## What this is

The **Qovira application server**: a private, self-hostable personal assistant shipped as a single Go binary that serves a JSON/SSE API and the bundled web UI, backed by an **encrypted SQLite (SQLCipher)** store. It runs on the user's own server against the model endpoint they configure — nothing phones home. It is an **unpublished application, not a package**: no npm release, no Changesets, no semver — it ships as the built binary and the container image.

It consumes the published `@qovira/theme` + `@qovira/ui` libraries for its frontend and the Omnilium `go-sqlcipher` driver via Go modules for its encrypted store.

> **Repository status:** the build chassis is in place — Go module, cobra CLI, embedded SvelteKit SPA, distroless image, and CI. The product itself is still early: the HTTP API, persistence, and the real web client are later units. Keep this file honest as they land.

## Commands

The `Makefile` is the build authority other flows read:

- `make build` — build the web SPA, then compile the embedded binary (`-tags embed_spa`) to `./qovira`.
- `make build-web` — install web deps (frozen lockfile) and build the SvelteKit SPA into `internal/httpx/webdist/`.
- `make build-go` — fast `go build ./...` stub compile (no SPA needed; uses the no-embed placeholder).
- `make run` — `make build` then serve the embedded binary locally with dev env (`QOVIRA_ADDR=:18888`, debug-level text logs); blocks until Ctrl-C.
- `make lint` — `golangci-lint run` plus `pnpm -C web lint`.
- `make test` — `go test -race ./...` plus `pnpm -C web test`.
- `make docker` — build the multi-stage distroless image.
- `make clean` — remove `./qovira` and `internal/httpx/webdist/`.

For backend work without the web toolchain, the plain Go commands work against the no-embed stub: `go build ./...`, `go test ./...`, and `go run ./cmd/qovira serve` (which serves a placeholder page on `/`). The real SvelteKit SPA is embedded only under `-tags embed_spa` — i.e. `make build` and the Docker image.

## Layout & build

- `cmd/qovira/` — entrypoint; one line handing off to `internal/cli`.
- `internal/cli/` — the cobra command tree (`root`, `serve`, `healthcheck`); thin adapters, no business logic.
- `internal/app/` — the composition root: env config, `slog` setup, HTTP server wiring, run + graceful shutdown.
- `internal/httpx/` — HTTP serving and the SPA embed seam: `spa.go` (the `fs.FS` handler with `index.html` fallback), `spa_embed.go` (`//go:embed all:webdist`, built under `-tags embed_spa`), `spa_noembed.go` (the placeholder stub), and the gitignored `webdist/` build output.
- `web/` — the SvelteKit SPA (Vite + adapter-static), built straight into `internal/httpx/webdist/`.

The SPA is embedded into the binary via a build tag: a bare `go build`/`go test` compiles against the no-embed placeholder, while `-tags embed_spa` embeds the real `webdist/` tree. The `all:` prefix on the embed directive is mandatory — it captures SvelteKit's leading-underscore `_app/` bundle.

## Conventions

Follow the `conventions:writing-go` house guide (and the matching `writing-*` guides for SQLite, Docker, and the embedded SPA's frontend stack). Cross-repo rules live in the workspace `CLAUDE.md`; don't restate them here. Record only repo-specific deviations and load-bearing invariants in this file as they emerge.

## Testing

`make test` runs the full suite (`go test -race ./...` plus `pnpm -C web test`); `go test ./...` covers the Go side alone. Write tests first (house TDD discipline): a failing test that fails for the right reason, then the minimal code to pass. "Green" — build, lint, and the full test suite passing — is the bar before pushing to `main` or opening a PR.
