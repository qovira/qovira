# Qovira

Qovira is a private, self-hostable AI personal assistant: a single Go binary that serves a JSON/SSE API and an embedded web client, organizing your reminders, notes, and day through chat backed by real structured records. It runs on your own server against the model endpoint you choose — nothing phones home.

> **Status:** early development. The build chassis is in place — the `qovira` binary builds with the web client embedded and serves a blank page, packaged as a distroless image and exercised by CI on amd64 and arm64. The HTTP API and the real web client are still to come.

## Install

Not yet available for end users — there is no functionality to use yet. To build the current chassis from source you need a Go toolchain plus Node and pnpm for the web client: `make build` produces the `./qovira` binary (with the SPA embedded) and `make docker` builds the distroless image. End-user install instructions will land here as the server gains real capabilities.

## Usage

Forthcoming — point the server at a model endpoint, expose it on a domain, and interact through the bundled web client.

## Development

This repository is the Qovira application (Go backend + embedded SvelteKit SPA). It consumes the `@qovira/theme` and `@qovira/ui` libraries for its client and the `go-sqlcipher` driver for its encrypted store, all from sibling repositories. See [CLAUDE.md](./CLAUDE.md) for the build/test commands and layout, and [CONTRIBUTING.md](./CONTRIBUTING.md) for the workflow.

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) and the [Code of Conduct](./CODE_OF_CONDUCT.md).

## License

[AGPL-3.0-only](./LICENSE) © OMNILIUM ADVANCED CYBERNETICS SRL.
