# Contributing to Qovira

Thanks for your interest. This guide covers how to work in this repo; the [Code of Conduct](./CODE_OF_CONDUCT.md) applies to all participation.

## Ground rules

Be respectful, open an issue before starting a large or structural change so the approach can be agreed first, and keep each pull request focused on one logical change.

## What belongs here

This repository is the Qovira application — the Go server and its embedded web client, shipped as one self-hostable binary. The client's visual primitives live in the sibling `@qovira/theme` and `@qovira/ui` libraries, and the marketing site lives in the `website` repo; changes to those belong there, not here.

## Getting set up

Clone the repository and install a recent Go toolchain (a C toolchain and OpenSSL headers are also needed, since the encrypted store builds via CGO). Build tooling is still being established; until the `Makefile` lands, the standard `go build ./...` and `go test ./...` are the baseline. The repo [CLAUDE.md](./CLAUDE.md) is the authority for the current build/test/run commands — keep it as your reference.

## How the codebase is organized

The planned shape is a `cmd/qovira` entrypoint wired from `internal/*` packages through one composition root, with the SvelteKit SPA built and embedded into the binary. See [CLAUDE.md](./CLAUDE.md) for the layout and the load-bearing concepts.

## Testing

Run `go test ./...`. New behavior is written test-first, and a change is expected to land with its tests; the build, lint, and full test suite must be green before it is merged.

## Opening a pull request

Keep each PR to one logical change, make sure the build, lint, and tests pass, and write a clear description of what changed and why.

### Commits

Use [Conventional Commits](https://www.conventionalcommits.org/), per the house `conventions:writing-commits` guide — the repo squash-merges a conventional subject onto the default branch.

## Releases

This repository is an unpublished application: it has no npm release, Changesets, or semver flow. It ships as the binary and container image built from this repo.
