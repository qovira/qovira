# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> This repo is one of several sibling repos under the Qovira workspace. The parent directory's `CLAUDE.md` governs cross-repo rules; this file is the authority for the `qovira` repo's internals. This is the **product application** — one of the two downstream leaves of the chain (alongside `website`), consuming the published `@qovira/theme` and `@qovira/ui` from npm for its frontend, and the Omnilium `go-sqlcipher` driver via Go modules for its encrypted store.

## What this is

The **Qovira application server**: a private, self-hostable personal assistant. A single Go binary (`qovira serve`) serves the JSON API, a realtime event stream, and the bundled web UI, backed by an **encrypted SQLite (SQLCipher)** database. It runs on the user's own server and points at the model endpoint they configure — nothing phones home. It is the piece you deploy.

This is an **unpublished application**, not a package: no npm release, no Changesets, no semver cadence. It ships as the binary built from this repo and the container image built from the `Dockerfile`.

## Commands

```sh
make build         # build ./qovira (CGO_ENABLED=1; injects version/commit/date via -ldflags)
make generate      # regenerate sqlc query code (go tool sqlc generate)
make test          # go test ./...
make race          # go test -race ./...
make lint          # golangci-lint run ./...
make docker-build  # build the image locally (BuildKit; forwards VERSION/COMMIT/DATE)
make docker-run    # run the image with a volatile /data volume
```

- **CGO is required.** The SQLCipher driver is built via CGO, so a C toolchain (GCC/Clang) and OpenSSL headers must be present. `CGO_ENABLED=1` is the default in the `Makefile`.
- **Generated code is not hand-edited.** Query code under `internal/store/db` is produced by sqlc from the SQL in `internal/store/queries` and the schema in `internal/store/migrations`. Edit the SQL and run `make generate`; never edit the generated Go directly.

## Architecture

A single binary (`cmd/qovira/main.go`) that delegates to a Cobra command tree, wired up from `internal/` packages.

**`internal/cli` is the command tree.** Subcommands: `serve` (the container entrypoint — starts the API, event stream, and web UI), `migrate` (database schema), `healthcheck` (probe a local server), `version` (build info), and `admin` (administrative operations — currently `admin reset-password <email>`, which resets an account's password and revokes all its sessions). The `version`/`commit`/`date` vars are injected at link time via `-ldflags` from the `Makefile`. `serve` loads config, builds the logger, opens the store, and composes the app via `app.New`, then runs it under a `SIGINT`/`SIGTERM`-cancelled context for graceful shutdown.

**`internal/config` is env-first boot configuration** — only the settings needed *before* the encrypted database opens (everything else lives in the DB). Precedence is **env > optional TOML file > built-in defaults**. Defaults: `DataDir=./data`, `HTTPAddr=:8080`, `LogFormat=json`, `LogLevel=info`, `AutoMigrate=true`. **Secrets are env-only and never read from the TOML file** — `QOVIRA_MASTER_KEY` (the SQLCipher passphrase, minimum 16 bytes, required) and `QOVIRA_ADMIN_PASSWORD`. Both support `_FILE` indirection (`QOVIRA_MASTER_KEY_FILE`); setting both the direct var and its `_FILE` counterpart is an error. Secret values use the `config.Secret` type, which redacts itself across `fmt` verbs and `slog` so the value can't leak into logs.

**`internal/store` is the encrypted data layer** (SQLCipher via `github.com/omnilium/go-sqlcipher`). Migrations are embedded and applied on startup when `AutoMigrate` is set. The **scope model is the security backbone of data access**: a `store.Scope` is the *sole* source of user identity, constructed only via `UserScope(Principal)` or `SystemScope()` (its fields are unexported so callers can't forge one). `ScopedQueries` enforce a `user_id` predicate on every user-owned query; the **scope guard** (`scopeguard.go`) is the backstop — it allowlists genuinely system-owned tables and otherwise requires the predicate. When a new domain table is added, the maintenance rule lives in `scopeguard_test.go`, not in the allowlist.

**`internal/httpx` is the HTTP layer** — server, router, middleware, the realtime event stream (`events.go`, backed by `internal/events`), and the embedded SPA. `AuthMiddleware` validates bearer tokens through a `TokenValidator`; `serve` wires the real `auth.Authenticator` (backed by `auth.Sessions.Resolve`). Token extraction is centralised in the exported `SessionTokenFromRequest` helper (cookie-first, Bearer fallback) so both the middleware and the logout handlers share the same logic. The CSRF double-submit cookie (`qovira_csrf`) is set on login and cleared on logout; the middleware enforces it on unsafe cookie-authenticated requests.

**`internal/auth` is the identity domain** — `Service` owns user CRUD and the `Authenticate(ctx, email, password)` method (argon2id verify + opportunistic rehash on login; returns the uniform `ErrInvalidCredentials` sentinel for both unknown-email and wrong-password paths to prevent user enumeration). `Sessions` manages session lifecycle: `Mint`/`Lookup`/`Bump`/`DeleteByToken`/`DeleteAllForUser`. `Authenticator` wraps `Sessions.Resolve` as an `httpx.TokenValidator`.

**`internal/authhttp` is the auth HTTP module** — implements `app.Module` (name `"auth"`) and mounts three endpoints: `POST /api/v1/auth/login` (public; mints a session; sets `__Host-qovira_session` HttpOnly + `qovira_csrf` readable cookies; returns `{expiresAt, user}` — token never in the body), `DELETE /api/v1/auth/session` (logout one; clears both cookies), `DELETE /api/v1/auth/sessions` (logout everywhere; clears both cookies). The module is wired via `app.AuthModuleCtor` which returns a `func(*store.Store) app.Module` constructor.

**`app.New` accepts module constructors** (`moduleCtors ...func(*store.Store) app.Module`) rather than pre-built modules, mirroring the `newValidator func(*store.Store) httpx.TokenValidator` pattern so all store-dependent objects are built after the store opens.

**The web UI is embedded** (`internal/httpx/spa.go`) via `go:embed all:webdist` (the `all:` prefix is required so SvelteKit's `_app/` subtree is included). `internal/httpx/webdist/` currently holds a **placeholder** build whose contents are swapped for the real SvelteKit `adapter-static` output at release time. That frontend is built from `@qovira/theme` + `@qovira/ui` — the cross-repo dependency edge (see the parent `CLAUDE.md`). Assets under `/_app/immutable/` are served with a 1-year immutable `Cache-Control`; everything else falls back to `index.html`.

## Docker & runtime

The `Dockerfile` is a multi-stage build: `golang:1.26-bookworm` builds the CGO binary, and the runtime stage is **`gcr.io/distroless/base-debian12:nonroot`** — *not* `static`/`scratch`, because the SQLCipher CGO binary needs glibc and the OpenSSL shared library. It runs as the numeric nonroot user `65532:65532`, exposes `:8080`, stores the encrypted DB under `/data`, and its `HEALTHCHECK` shells out to `qovira healthcheck` (the image has no curl/shell). **The master key is never baked into the image** — supply it at runtime via `QOVIRA_MASTER_KEY`, or `QOVIRA_MASTER_KEY_FILE` (Docker secret) in production so it never appears in `docker inspect` or image history. BuildKit is required (`make docker-build` sets `DOCKER_BUILDKIT=1`).

## CI

`.github/workflows/ci.yml` runs on every PR and push to `main`: **build**, **lint** (golangci-lint), **race** (`go test -race`), and **vuln** (govulncheck) — each on a Blacksmith runner (house rule: every job runs on Blacksmith). There is no release/publish workflow: this app is deployed, not published.

## Conventions

- **Keep `CLAUDE.md` and `README.md` current.** Both are documentation that must track reality: when a change alters something either file describes (commands, architecture, the config/security model, the Docker/CI setup, conventions), update the affected doc automatically in the **same** change — never leave it as a follow-up. Stale docs silently mislead every future reader and session. (The parent workspace `CLAUDE.md` carries the same rule across repos.)
- **Go house style** is enforced by `make lint` (golangci-lint, config in `.golangci.yaml`); run it before pushing. Keep tests green under `-race`.
- **Commits:** Conventional Commits (`feat:`, `fix:`, `ci:`, `chore:`, `test:`).
- **Branches:** feature branches off `main`; PRs target `main` and are squash-merged.
- **No tracker identifiers in shipped content.** Internal Linear references belong in commit messages only — never in source, comments, or docs.
- **Secrets never touch the repo or the image.** The master key and admin password are runtime-only; don't log, embed, or commit them.
