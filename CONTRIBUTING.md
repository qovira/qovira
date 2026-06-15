# Contributing to Qovira

Thanks for your interest in **Qovira** — a private, self-hostable personal assistant. This repository holds the **application server**: a single Go binary that serves the JSON API, a realtime event stream, and the bundled web UI, backed by an encrypted SQLite (SQLCipher) database. It's the piece you deploy, and we're glad to have help making it sharper.

## Ground rules

- **Open an issue first.** Before sending a pull request — especially anything that changes the API surface, the storage schema, the configuration contract, or the security model — open an issue so we can agree on the approach. Typo fixes and obviously correct documentation tweaks can skip straight to a PR.
- **Be kind.** This project follows the [Contributor Covenant](./CODE_OF_CONDUCT.md). By participating you're expected to uphold it; please report unacceptable behavior to the address listed there.
- **Licensing.** The project is [AGPL-3.0-only](./LICENSE). Any contribution you submit for inclusion is licensed under those same terms (inbound = outbound) — opening a pull request is all the agreement we need. There's no CLA and no per-commit sign-off to remember.

## What belongs here

This is the **product application server** — one Go binary that is the unit of deployment. A few invariants keep it coherent; please keep your change inside them.

**In scope** — contributions we welcome:

- Backend features and bug fixes — the API, the realtime event stream, the store layer, configuration, the CLI subcommands.
- Tests — unit and integration coverage, especially around the store and the encryption boundary.
- Security hardening that respects the model below (at-rest encryption, no phone-home, key supplied at runtime).
- Documentation, build, Docker, and tooling improvements.

**Out of scope** — please don't open a PR for these:

- **Baking secrets into the image or the binary.** The master key is supplied at runtime and **never** stored in the image or committed to the repo. A change that embeds, logs, or persists it in plaintext won't be accepted; see *Supplying the master key* in the [README](./README.md).
- **A phone-home or telemetry beacon.** Qovira runs on the user's server and points at the model endpoint they configure. Nothing leaves the room — outbound calls to Qovira-operated services don't belong here.
- **Vendoring a patched SQLCipher driver.** The encrypted-store driver lives upstream in [`github.com/omnilium/go-sqlcipher`](https://github.com/omnilium/go-sqlcipher); a fix there belongs there, not copied into this tree (see [Cross-repo changes](#cross-repo-changes)).
- **Tracker identifiers in shipped content.** Don't put internal issue references in source, comments, or docs — the codebase stands on its own.

## Getting set up

Building the encrypted store requires **CGO**, so you need a working C toolchain in addition to Go:

- **Go 1.26+**
- A **C toolchain** (GCC or Clang) and **OpenSSL headers** — CGO compiles the SQLCipher driver against them.

The `Makefile` covers the workflow:

```sh
make build       # build the binary to ./qovira (CGO_ENABLED=1, with version info injected)
make generate    # regenerate the sqlc query code (go tool sqlc generate)
make test        # run the test suite
make race        # run the tests under the race detector
make lint        # run golangci-lint
make docker-build  # build the container image locally (BuildKit)
```

Before you open a PR, the full gate below should pass — that's what CI runs.

## How the codebase is organized

A single binary (`cmd/qovira`) wired up from `internal/` packages:

| Path                 | What it is                                                                            |
| -------------------- | ------------------------------------------------------------------------------------- |
| `cmd/qovira`         | The binary entrypoint; delegates to the CLI.                                          |
| `internal/cli`       | Subcommand wiring (`serve`, `migrate`, `healthcheck`, `version`) and build-info vars. |
| `internal/app`       | The application wiring — composes the server from its dependencies.                   |
| `internal/bootstrap` | Process startup: config load, store open, graceful lifecycle.                         |
| `internal/store`     | The encrypted SQLite (SQLCipher) data layer; consumes `go-sqlcipher`.                 |
| `internal/httpx`     | HTTP server, routing, and middleware.                                                 |
| `internal/events`    | The realtime event stream.                                                            |
| `internal/config`    | Environment- and file-driven configuration.                                           |
| `internal/capability`, `internal/id`, `internal/logging` | Cross-cutting building blocks.                     |
| `Makefile`, `sqlc.yaml`, `.golangci.yaml`, `Dockerfile` | Build, codegen, lint, and image definitions.        |

More architecture and operational detail lives in [`CLAUDE.md`](./CLAUDE.md) and the [README](./README.md).

## A few load-bearing conventions

- **The master key never touches the repo or the image.** It's read from the environment (`QOVIRA_MASTER_KEY`, or `QOVIRA_MASTER_KEY_FILE` for the file-indirection path) at runtime.
- **Database access goes through `internal/store`.** Queries are generated by sqlc — edit the SQL and run `make generate`, don't hand-write the generated Go.
- **Errors are wrapped with context, not swallowed.** Follow the surrounding Go style; `make lint` (golangci-lint) is the arbiter.
- **Keep the tests green under `-race`.** Concurrency around the event stream and store matters; `make race` is part of the gate.

## Cross-repo changes

The encrypted store driver — [`github.com/omnilium/go-sqlcipher`](https://github.com/omnilium/go-sqlcipher) — is a **separate repository**, consumed here via Go modules (not a local symlink). If your change needs a fix or a new capability in that driver, **don't vendor or patch it in this tree** — the fix belongs upstream:

1. Open an issue (here and/or on `go-sqlcipher`) describing the gap.
2. The fix lands and is tagged in `go-sqlcipher`.
3. This repo bumps its `go-sqlcipher` dependency and consumes the new release.

A contributor PR can't span repos, so note the dependency in your issue and a maintainer will help sequence the upstream release.

## Opening a pull request

Once there's an issue and your change is ready:

1. **Branch** off `main` and make your change there.
2. **Keep it scoped.** One logical change per PR. A focused diff is reviewed faster than a sweeping one.
3. **Run the full gate locally** — `make lint && make test && make race` — before you push. CI runs exactly these (plus a vulnerability scan), and they must be green to merge.
4. **Write a clear PR description.** Say what changed and why, and link the issue it resolves.

### Commits

We use [Conventional Commits](https://www.conventionalcommits.org/) on `main`, but **you don't have to**. PRs are squash-merged, and a maintainer writes the final Conventional Commit message on merge. So:

- Give the **PR** a clear, descriptive title and a useful description — that's what we work from.
- Your individual commits can be whatever helps you; they won't survive the squash.

### Review

A maintainer will review for correctness, scope, test coverage, and fit with the security model (at-rest encryption, no phone-home, runtime-supplied key). Expect a conversation — it's how we keep the server coherent. Once it's approved and green, we squash and merge.

## Releasing

Releasing is **maintainers only** — there's nothing for a contributor to do here, but it's documented so the process isn't a mystery. Qovira is an application you deploy, not a published package: it ships as the single binary and the container image built from this repo, so there are **no npm releases and no public package to track**. CI runs the full gate (build, lint, race, vulnerability scan) on Blacksmith for every PR and push to `main`.

---

Thanks again for contributing. Questions that aren't a bug report are welcome as issues too — if something here is unclear, that's worth an issue of its own.
