# Qovira

Qovira is a private, self-hostable AI personal assistant: a single Go binary that serves a JSON/SSE API and an embedded web client, organizing your reminders, notes, and day through chat backed by real structured records. It runs on your own server against the model endpoint you choose — nothing phones home.

> **Status:** early development. The repository has been reset to a clean slate and is being rebuilt; build tooling, the API, and the web client are not yet in place.

## Install

Not yet available. Qovira will ship as a single binary built from this repository and as a distroless Docker image. Build and run instructions will land here once the server foundation is in place.

## Usage

Forthcoming — point the server at a model endpoint, expose it on a domain, and interact through the bundled web client.

## Development

This repository is the Qovira application (Go backend + embedded SvelteKit SPA). It consumes the `@qovira/theme` and `@qovira/ui` libraries for its client and the `go-sqlcipher` driver for its encrypted store, all from sibling repositories. See [CLAUDE.md](./CLAUDE.md) for the build/test commands and layout, and [CONTRIBUTING.md](./CONTRIBUTING.md) for the workflow.

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) and the [Code of Conduct](./CODE_OF_CONDUCT.md).

## License

[AGPL-3.0-only](./LICENSE) © OMNILIUM ADVANCED CYBERNETICS SRL.
