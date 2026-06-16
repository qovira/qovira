# CLAUDE.md

Guidance for Claude Code working in the `qovira` repo. This is one of several sibling repos under the Qovira workspace: the parent directory's `CLAUDE.md` governs cross-repo rules and is authoritative there; **this file is authoritative for `qovira`'s internals**. `qovira` is the **product application** — a downstream leaf of the chain (alongside `website`) — consuming the published `@qovira/theme` + `@qovira/ui` from npm for its frontend and the Omnilium `go-sqlcipher` driver via Go modules for its encrypted store.

## What this is

The **Qovira application server**: a private, self-hostable personal assistant shipped as a single Go binary (`qovira serve`) that serves the JSON API, a realtime event stream, and the bundled web UI, backed by an **encrypted SQLite (SQLCipher)** database. It runs on the user's own server against the model endpoint they configure — nothing phones home.

It is an **unpublished application, not a package**: no npm release, no Changesets, no semver. It ships as the binary built from this repo and the container image built from the `Dockerfile`.

## Commands

```sh
make build         # full pipeline: web SPA + sync + go build -tags embed_spa → ./qovira
                   # (CGO_ENABLED=1; real SPA embedded; requires Node 24 + pnpm)
make build-go      # Go-only build — no SPA step, no Node/pnpm required; the binary's
                   # web UI serves the in-code stub ("not embedded" page). Safe on a
                   # fresh checkout with no webdist/ directory.
make web           # build the SvelteKit SPA only (cd web && pnpm install + pnpm build)
make sync-web      # wipe and repopulate internal/httpx/webdist/ from web/build/
                   # (runs as part of make build; webdist/ is gitignored build output)
make generate      # regenerate sqlc query code (go tool sqlc generate)
make test          # go test ./...
make race          # go test -race ./...
make lint          # golangci-lint run ./...
make docker-build  # build the image locally (BuildKit; forwards VERSION/COMMIT/DATE)
make docker-run    # run the image with a volatile /data volume
```

- **CGO is required** (`CGO_ENABLED=1`, the `Makefile` default). The SQLCipher driver builds via CGO — a C toolchain (GCC/Clang) and OpenSSL headers must be present.
- **Never hand-edit generated code.** Query code under `internal/store/db` is produced by sqlc from the SQL in `internal/store/queries` and the schema in `internal/store/migrations`. Edit the SQL, then run `make generate`.

## E2E server mode (scripted assistant)

The server can be compiled and run in a scripted-assistant mode for Playwright E2E testing. This mode replaces the real model gateway with a deterministic `ScriptedChatter` that emits a canned sequence of streaming deltas, tool calls, and completions per turn — exercising the real auth/REST/SSE/tool-execution stack without a live LLM endpoint.

**The scripted provider is physically absent from the default binary** — it is compiled only when the `e2e` build tag is passed. A default `go build ./...` will never include it; `go build -tags e2e ./...` compiles it in and tests tagged `//go:build e2e` are exercised by `go test -tags e2e ./...`.

**Building and running in e2e mode:**

```sh
CGO_ENABLED=1 go build -tags e2e -o qovira-e2e ./cmd/qovira
QOVIRA_MASTER_KEY=<key-min-16-bytes> \
QOVIRA_ADMIN_EMAIL=<admin@example.com> \
QOVIRA_ADMIN_PASSWORD=<password> \
QOVIRA_E2E_SCRIPT_PATH=<path-to-fixture.json> \
./qovira-e2e serve
```

When `QOVIRA_E2E_SCRIPT_PATH` is set, the server uses the scripted provider; when it is absent (but the binary was still built with `-tags e2e`), the server falls back to the real gateway.

**Fixture schema** — a JSON file with a `rules` array; each rule has a `match` (case-insensitive `contains` / `prefix` on the latest user message) and an ordered `rounds` array; each round is an ordered `chunks` array with `textDelta`, `toolCall` (`name` + optional `id` + `arguments`), `done`, and `delayMs` fields. The round index is selected statelessly from the ChatRequest history: round 0 is the first model response; round 1 is the response after one assistant-message-plus-tool-results exchange; and so on. A minimal two-round example:

```json
{
  "rules": [
    {
      "match": { "contains": "delete reminder" },
      "rounds": [
        { "chunks": [{ "toolCall": { "name": "delete_reminder", "arguments": { "id": "r1" } } }, { "done": true }] },
        { "chunks": [{ "textDelta": "Done, I deleted that reminder." }, { "done": true }] }
      ]
    }
  ]
}
```

**Result templating (`$fromResult`)** — a tool call's `arguments` may reference a value from an earlier tool's result so that fixtures can, for example, create a reminder in one turn and delete it by its real server-generated id in a later turn. Reference form (anywhere inside `arguments`):

```json
{ "$fromResult": { "callId": "<earlier call id>", "path": "<dot path>" } }
```

At emit time the provider scans `req.Messages` for a message with `Role=="tool"` and `ToolCallID==callId` (last match wins), JSON-parses its `Content`, traverses the dot-separated `path` (numeric segments index into arrays, e.g. `"items.0.id"`), and substitutes the resolved value in place. The reference may appear at any depth in the arguments tree. `path` is required and the marker object must carry **no sibling keys** beside `$fromResult`. Resolution errors are fatal — an unknown `callId`, an empty/misspelled `path`, a path segment that misses or indexes out of range, or a sibling key all make the iterator yield an error and stop, never a silent empty or wrong value. Only tool calls with an **explicit `id`** field in the fixture are referenceable; auto-generated ids (when `id` is omitted) are random and cannot be predicted by a reference.

The `create_reminder` result shape is the full `Reminder` object (same as the REST response). The path to the generated id is `"id"`. Example fixture using a cross-turn reference:

```json
{
  "rules": [
    {
      "match": { "contains": "create then delete" },
      "rounds": [
        {
          "chunks": [
            { "toolCall": { "id": "c-create-dentist", "name": "create_reminder",
                "arguments": { "title": "Dentist", "dueAt": "2026-07-01T09:00:00Z" } } },
            { "done": true }
          ]
        },
        {
          "chunks": [
            { "toolCall": { "name": "delete_reminder",
                "arguments": { "id": { "$fromResult": { "callId": "c-create-dentist", "path": "id" } } } } },
            { "done": true }
          ]
        },
        { "chunks": [{ "textDelta": "Done, deleted the dentist reminder." }, { "done": true }] }
      ]
    }
  ]
}
```

No journey-specific logic lives in the Go provider — all scripted behaviour is in the fixture file. The Playwright suite (a separate issue) supplies the concrete fixtures for each journey.

## Architecture

Single binary (`cmd/qovira/main.go`) delegating to a Cobra command tree, wired from `internal/` packages. Package map (each entry: what it owns + the invariants that constrain edits):

- **`internal/cli`** — the command tree. Subcommands: `serve` (container entrypoint — starts API, event stream, web UI), `migrate` (schema), `healthcheck` (probe a local server), `version` (build info), `admin` (currently `admin reset-password <email>` — resets the password and revokes all sessions). `version`/`commit`/`date` are injected at link time via `-ldflags`. `serve` loads config → builds logger → opens store → composes via `app.New`, then runs under a `SIGINT`/`SIGTERM`-cancelled context for graceful shutdown. On the first boot with `QOVIRA_ADMIN_EMAIL` + `QOVIRA_ADMIN_PASSWORD` set, `app.New` creates the initial admin account automatically (see `app.New` below).
- **`internal/config`** — env-first boot config: only settings needed *before* the encrypted DB opens (everything else lives in the DB). Precedence: **env > optional TOML file > defaults** (`DataDir=./data`, `HTTPAddr=:8080`, `LogFormat=json`, `LogLevel=info`, `AutoMigrate=true`). **Secrets are env-only, never read from TOML**: `QOVIRA_MASTER_KEY` (SQLCipher passphrase, min 16 bytes, required) and `QOVIRA_ADMIN_PASSWORD`. Both support `_FILE` indirection; setting both a var and its `_FILE` counterpart is an error. Secret values use `config.Secret`, which redacts itself across `fmt` verbs and `slog`. `QOVIRA_ADMIN_EMAIL` + `QOVIRA_ADMIN_PASSWORD` activate first-run seeding (see `app.New` below); both must be set together or not at all.
- **`internal/gateway`** — the model gateway. `Gateway` resolves capability roles (chat, embeddings, vision, …) to AI endpoint coordinates and forwards to the configured upstream; config lives in the `"model_gateway"` settings namespace, read-through on every call. `Chat(ctx, req)` returns an `iter.Seq2[Chunk, error]` with a two-phase error model (setup error vs. per-yield error). **Resilience layer** (`resilience.go`) wraps every `Chat`: first-token timeout (45s), idle timeout (30s), ctx propagation, plus a pre-first-token retry loop (3 attempts, jittered backoff) retrying on 5xx, network failures, and 429 (honouring `Retry-After`). Never retried: generic 4xx, `ErrContextLength`, `ErrAuth`, `ErrModelNotFound`; 451 is retried only when `retryLegalUnavailable` is set. All params live in `Gateway.resilienceCfg` (`ResilienceConfig`); `sleepFn` is injectable for zero-sleep deterministic tests. The internal HTTP client carries **no wall-clock timeout** — timeouts come entirely from the resilience layer's derived contexts. `Probe(ctx, role)` checks an endpoint: step 1 GET `/v1/models` (reachability + model presence); step 2 (chat only) a minimal streamed request with a forced dummy tool call to verify streaming + native tool calling. Returns `ProbeResult` with independent `Reachable`/`ModelServed`/`Streaming`/`ToolCalling` booleans + an `Err` for the first failure.
- **`internal/store`** — the encrypted data layer (SQLCipher via `github.com/omnilium/go-sqlcipher`). Migrations are embedded, applied on startup when `AutoMigrate` is set. **The scope model is the security backbone**: a `store.Scope` is the *sole* source of user identity, built only via `UserScope(Principal)` or `SystemScope()` — its fields are unexported so callers can't forge one. `ScopedQueries` enforce a `user_id` predicate on every user-owned query; the **scope guard** (`scopeguard.go`) is the backstop — it allowlists genuinely system-owned tables, else requires the predicate. **When adding a domain table, the maintenance rule lives in `scopeguard_test.go`, not the allowlist.** When a query uses a correlated subquery that carries its own `user_id` predicate (e.g. `ListConversations`, which has a preview subquery on `messages` scoped by `m.user_id = @user_id`), add a `-- scopeguard:allow-unscoped: <reason>` annotation to the query block so the guard skips it — the guard fails closed on any SELECT-inside-SELECT even when both tables are correctly scoped.
- **`internal/httpx`** — the HTTP layer: server, router, middleware, the realtime event stream (`events.go`, backed by `internal/events`), and the embedded SPA. `AuthMiddleware` validates bearer tokens through a `TokenValidator` (`serve` wires the real `auth.Authenticator`, backed by `auth.Sessions.Resolve`). Token extraction is centralised in the exported `SessionTokenFromRequest` helper (cookie-first, Bearer fallback), shared by the middleware and the logout handlers. The CSRF double-submit cookie (`qovira_csrf`) is set on login, cleared on logout; the middleware enforces it on unsafe cookie-authenticated requests. `SecurityHeadersMiddleware` sets the CSP (`spa_csp.go`): it computes the SHA-256 hashes of the embedded SPA's first-party inline scripts (the SvelteKit bootstrap + the pre-paint theme-boot) from the served `index.html` bytes at startup and emits `default-src 'self'; script-src 'self' <hashes>; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'` — script stays strict (`'self'` + exact hashes, never `'unsafe-inline'`); `style-src` allows `'unsafe-inline'` only because Svelte's transition runtime injects un-hashable per-instance `<style>` keyframes (see ADR-010 in the Architecture note). Never relax the script axis to make a browser test pass.
- **`internal/auth`** — the identity domain. `Service` owns user CRUD and `Authenticate(ctx, email, password)` (argon2id verify + opportunistic rehash on login; returns the uniform `ErrInvalidCredentials` sentinel for both unknown-email and wrong-password to prevent enumeration). `Sessions` owns session lifecycle: `Mint`/`Lookup`/`Bump`/`DeleteByToken`/`DeleteAllForUser`. `Authenticator` wraps `Sessions.Resolve` as an `httpx.TokenValidator`.
- **`internal/authhttp`** — the auth HTTP module, implementing `app.Module` (name `"auth"`). Endpoints: `POST /api/v1/auth/login` (public; mints a session; sets `__Host-qovira_session` HttpOnly + `qovira_csrf` readable cookies; returns `{expiresAt, user}` — **token never in the body**), `DELETE /api/v1/auth/session` (logout one), `DELETE /api/v1/auth/sessions` (logout everywhere); both logouts clear both cookies. Wired via `app.AuthModuleCtor`, which returns a `func(*store.Store) app.Module` constructor.
- **`internal/harness`** — the AI engine: turns an inbound message into a bounded sequence of validated tool calls. Constructed once in `app.New` with `(reg, gw, st, bus, harness.Config)`. The turn is **decoupled from the request**: `POST /api/v1/conversations/{id}/messages` persists the user message and returns **`202`** with the persisted body; all output (text deltas, tool events, the final message) streams over the existing per-user `/events` SSE, tagged by `conversationId`, so the turn outlives the request. Read-only conversation endpoints: `GET /api/v1/conversations` returns a cursor-paginated list (most-recently-active first, keyset on `(updated_at DESC, id DESC)`), each item including an `id`, `preview` (first user message truncated to ~80 chars), `createdAt`, and `updatedAt`; `GET /api/v1/conversations/{id}` returns the full chronological message history (404 when the conversation does not exist or belongs to another user). `run` is a **re-entrant loop over persisted conversation state only** (no in-memory turn state): assemble → `gateway.Chat` → execute validated tool calls → feed results back, bounded by a step cap. Tool execution is gated by a pure `policy(RiskTier, TrustLevel)` matrix (Auto/Confirm/Block); a `Confirm` **suspends** the turn (persists a `pending_confirmations` row, emits `confirmation.required`, returns) and `POST .../confirmations/{callId}` → `Harness.Resolve` re-enters `run` — approve executes, deny feeds a declined result. `run` is **serialized per conversation** (a keyed lock) and resolution transitions are atomic status CAS, so concurrent resolves can't double-fire a tool. Confirmations expire (lazy check at resolve + a registrable `SweepExpiredConfirmations` job awaiting the Scheduler). Context is grounded per turn (system prompt with the user's time/tz/locale/language + a reserved memory slot) and history slides within a soft token budget (`chars/4` heuristic, never orphaning a tool-call group), with `ErrContextLength` as the hard backstop. Owns the domain chat-event vocabulary (`message.delta`/`message.completed`/`tool.started`/`tool.completed`/`tool.failed`/`confirmation.required`/`confirmation.expired`/`turn.failed`); **every event payload now carries `conversationId`** so clients can correlate SSE events to the correct conversation without parsing `message.delta` payloads. The SSE framing is the Foundation's. Tunables (`StepCap`, `HistoryTokenBudget`, `MaxContextRetries`, `ConfirmationTTL`) live in `harness.Config` with sane defaults, threaded from `serve`. Persists into the `conversations`/`messages` tables plus its own `pending_confirmations`.
- **`internal/reminders`** — the reminders capability module (the first end-to-end capability, proving one-service-two-surfaces). A single `Service` owns all logic — create/get/list/update/complete/delete, fire-job sync, validation, and event emission — with REST handlers and AI tools as thin adapters over it; nothing else reads or writes the module-owned `reminders` table. REST (`/api/v1/reminders`): `POST` (201 + Location), `GET /{id}`, `GET` (cursor-paginated keyset list over `(due_at, id)`, status + due-window filters), `PATCH /{id}` (merge update; `status` routes to complete/reopen), `DELETE /{id}` (204). AI tools via `Tools()`: `create_reminder`/`update_reminder`/`complete_reminder` (`RiskWrite`), `delete_reminder` (`RiskDestructive`), and the context-safe `list_reminders` (`RiskRead` — hard-caps at 20, compact `id`/`title`/`dueAt`/`status` projection, appends a truncation line when more match), thin adapters over the same Service that take **structured** args (the model does the NL→RFC 3339/5545 conversion using the harness-injected now+tz — the module never NL-parses); the one `ValidationError` maps to a `422` on REST and a `*capability.ToolError` on the tool surface. The `"reminder.fire"` scheduler handler fires a reminder live over SSE (the thin `reminder.fired`), stamping `last_fired_at` (the at-least-once idempotency guard) and never moving `status` — firing is a delivery event, status is user intent — and then emits the matching **fat** domain event for the stamp (`reminder.completed` on auto-complete or an exhausted finite series; `reminder.updated` on a recurring advance or a keep-active restamp), the same full-`Reminder` payload `Update`/`Complete` publish, so connected clients reflect the fired state live without a refetch. One-shot `auto_complete=true` (default) completes on fire, `false` stays active; a recurring reminder (rrule, validated/advanced with the same RFC 5545 evaluator the scheduler uses, in its snapshotted IANA `tz`) advances `due_at` to the next occurrence and stays active; a non-active reminder short-circuits. Timezone is snapshotted at create (explicit `tz` → user profile zone via `GetProfile` → `"UTC"`); past `due_at` is accepted; the Service is the single writer of `fire_job_id`. Wired in `app.New` (step 10b) — added to the capability registry and its `"reminder.fire"` handler registered before `sched.Start`.
- **`app.New`** — accepts module *constructors* (`moduleCtors ...func(*store.Store) app.Module`), not pre-built modules, mirroring the `newValidator func(*store.Store) httpx.TokenValidator` pattern so all store-dependent objects are built after the store opens. The harness is constructed here too (with the gateway, registry, bus, and store), and its routes registered. The reminders module is wired inline in step 10b (after bus/scheduler are built, before sched.Start). **First-run admin seeding** runs at step 4b (after migrations): when `AutoMigrate=true`, no users exist, and both `cfg.AdminEmail` + `cfg.AdminPassword` are non-empty, a dedicated `*auth.Service` (production params) calls `bootstrap.MaybeSeedAdmin` to create the admin account. On success an INFO log line records the email (never the password). The seeding is a no-op on every subsequent start; a seeding error fails boot loudly.
- **`internal/httpx/spa.go`** — the SPA handler and the package-level `var spaFS fs.FS` it reads. The embed is gated behind the `embed_spa` build tag (mirroring the `e2e` tag pattern) and split across three files: `spa.go` (untagged — handler logic only), `spa_embed.go` (`//go:build embed_spa` — carries `//go:embed all:webdist` and wires `spaFS`; the `all:` prefix is required so SvelteKit's `_app/` subtree is included), and `spa_noembed.go` (`//go:build !embed_spa` — wires `spaFS` to a tiny in-code `testing/fstest.MapFS` stub so `go build ./...` and `go test ./...` work on a fresh checkout with no Node/pnpm step and no `webdist/` directory). `internal/httpx/webdist/` is a **gitignored** build artifact — nothing there is ever committed. **`make build`** runs the real SvelteKit build (`web/`), syncs the output into `webdist/` via `make sync-web`, then compiles with `-tags embed_spa` so the real SPA is embedded. The **Docker image** does the same. **`make build-go`** skips the SPA step; the binary's web UI shows an in-code "not embedded" stub page. Assets under `/_app/immutable/` get a 1-year immutable `Cache-Control`; everything else falls back to `index.html`.

## Frontend SPA (`web/`)

The SvelteKit 2 / Svelte 5 SPA lives in `web/` (pnpm, TypeScript, Vite, adapter-static). It is a separate sub-project with its own `package.json`, `pnpm-lock.yaml`, and `tsconfig.json`. Run all `pnpm` commands from inside `web/`.

```sh
cd web
pnpm install          # install deps (run after checkout)
pnpm generate:api     # regenerate TypeScript types from openapi.yaml (at repo root)
pnpm check            # svelte-check (TypeScript + Svelte type-check)
pnpm lint             # ESLint
pnpm format:check     # Prettier check
pnpm format           # Prettier write
pnpm test             # vitest run — all five projects including the storybook browser project (needs Chromium; run pnpm exec playwright install --with-deps chromium first in CI)
pnpm test:unit        # vitest run — node/runes/jsdom/browser only; browser-free path, no Chromium required
pnpm build            # SvelteKit static build → web/build/
```

From the repo root, `make web` is equivalent to `cd web && pnpm install --frozen-lockfile && pnpm build`. `make build` runs `make web` + `make sync-web` + `go build -tags embed_spa` in sequence, producing a binary that embeds the real SPA. `make build-go` skips the SPA entirely and uses the in-code stub — useful for fast backend iteration that does not require Node/pnpm.

**openapi.yaml** lives at the **repo root** (`/openapi.yaml`) and is the hand-authored OpenAPI 3.1 spec for the `/api/v1` surface. It is the canonical API contract until a server-side emitter exists. When Go handlers change, update `openapi.yaml` and re-run `pnpm generate:api` (from `web/`) — both files must move together.

**`web/src/lib/api/`** — the typed API module:
- `schema.d.ts` — generated by `openapi-typescript` from `openapi.yaml`; **do not hand-edit**. The CI drift gate regenerates it and fails if the committed file differs from the spec.
- `index.ts` — the `Api` wrapper: exports `Api` (openapi-fetch client with CSRF/credentials/problem+json middleware), `ProblemError` (typed RFC 9457 error subclass), and `onUnauthorized(cb)` (the 401 hook seam). Callers always use `Api`; they never import the internal `_client`.

**CSRF protocol**: `Api` automatically reads `qovira_csrf` from `document.cookie` and sends it as `CSRF-Token` on POST/PATCH/DELETE. GET/HEAD are exempt. The session cookie `__Host-qovira_session` is HttpOnly and rides automatically via `credentials: "include"`.

**vitest projects**: the `web/vitest.config.ts` defines five projects — `node` (for `src/tests/`), `runes` (node + the Svelte compiler, for `*.svelte.test.ts` rune-logic suites), `jsdom` (jsdom env, for `*.jsdom.test.ts` DOM-faithful sanitizer/XSS tests — jsdom passes DOMPurify's `isSupported` check and correctly handles block-context href sanitization), `browser` (happy-dom, for `src/lib/**/*.test.ts` excluding the rune and jsdom suites), and `storybook` (Vitest Browser Mode with Playwright/Chromium via `@storybook/addon-vitest`, runs every `*.stories.svelte` and `*.stories.ts` as a test including axe a11y checks). Tests that need `document.cookie` or `globalThis.fetch` go under `src/lib/`; `$state`/`$derived` logic goes in a `*.svelte.test.ts`; XSS/sanitizer tests go in a `*.jsdom.test.ts`. `pnpm test` runs all five projects; `pnpm test:unit` runs only the four browser-free projects (node/runes/jsdom/browser) and does not require Chromium. CI must run `pnpm exec playwright install --with-deps chromium` before `pnpm test`.

### i18n (Paraglide)

The SPA uses **Paraglide JS v2** (`@inlang/paraglide-js`) for i18n. Messages are authored in `web/messages/{locale}.json` (currently only `en.json` for v0.1) and compiled to a generated module at `web/src/lib/paraglide/`. The project is configured at `web/project.inlang/settings.json` (locale list, message plugin path pattern).

**Adding strings:** add a key→value pair to `messages/en.json`. When a second locale is needed, add a parallel `messages/{locale}.json` — no code changes at string-call sites.

**Using strings in `.svelte` and `.ts` files:** import the named message functions from `$lib/paraglide/messages.js` and call them inline (`nav_chat()`, `login_error_invalid_credentials()`, etc.).

**Generated output:** `src/lib/paraglide/` is **gitignored** (Paraglide emits its own `.gitignore` in the dir, and the outer `web/.gitignore` excludes it too). Do not hand-edit generated files.

**Compile step:** run `pnpm compile:i18n` from `web/` to regenerate the output. The `check`, `lint`, and `test` scripts chain this automatically (`pnpm compile:i18n && …`), so a clean checkout runs correctly locally. The Vite plugin (`paraglideVitePlugin` in `vite.config.ts`) recompiles on every `pnpm dev`/`pnpm build` invocation. CI runs an explicit `pnpm compile:i18n` step before the gate commands.

**Locale strategy:** v0.1 uses `globalVariable` + `baseLocale` only (no URL/cookie negotiation, no locale switcher). The strategy is set in `vite.config.ts`; widening it later does not touch string call sites.

## Docker & runtime

Multi-stage `Dockerfile`: `golang:1.26-bookworm` builds the CGO binary; the runtime stage is **`gcr.io/distroless/base-debian12:nonroot`** — *not* `static`/`scratch`, because the SQLCipher CGO binary needs glibc and the OpenSSL shared library. Runs as numeric nonroot `65532:65532`, exposes `:8080`, stores the encrypted DB under `/data`, `HEALTHCHECK` shells out to `qovira healthcheck` (no curl/shell in the image). **The master key is never baked in** — supply it at runtime via `QOVIRA_MASTER_KEY`, or `QOVIRA_MASTER_KEY_FILE` (Docker secret) in production so it never appears in `docker inspect` or image history. BuildKit is required (`make docker-build` sets `DOCKER_BUILDKIT=1`).

## CI

`.github/workflows/ci.yml` runs on every PR and push to `main`. The **`web` job** (Linux x64, Blacksmith) runs first and gates all Go jobs — it: regenerates API types and fails on schema drift (`pnpm generate:api && git diff --exit-code src/lib/api/schema.d.ts`), then compiles i18n messages (`pnpm compile:i18n`) before running svelte-check, lint, format:check, vitest, and the static build. **Go jobs** (**build**, **race**) run across Linux x86-64, Linux ARM64, and macOS ARM64 (SQLCipher CGO is platform-specific); **lint** (golangci-lint) and **vuln** (govulncheck) run once on Linux x64. Every job runs on a Blacksmith runner and shares `./.github/actions/setup` (OpenSSL + Go). A **docker** job builds the container image for both Linux arches — `push: false`. No release/publish workflow.

## Conventions

- **Keep `CLAUDE.md` and `README.md` current — in the same change.** When a change alters anything either file describes (commands, architecture, config/security model, Docker/CI, conventions), update the affected doc in the **same** change, never as a follow-up. Stale docs mislead every future session. (The parent workspace `CLAUDE.md` carries this rule across repos.)
- **Go house style** is enforced by `make lint` (golangci-lint, config in `.golangci.yaml`); run it before pushing, and keep tests green under `-race`.
- **Commits:** Conventional Commits (`feat:`, `fix:`, `ci:`, `chore:`, `test:`).
- **Branches:** feature branches off `main`; PRs target `main`, squash-merged.
- **No tracker identifiers in shipped content** — keep them out of source, comments, and docs; the code stands on its own.
- **Secrets never touch the repo or the image.** The master key and admin password are runtime-only; don't log, embed, or commit them.
