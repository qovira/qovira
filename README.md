# Qovira

A private, self-hostable personal assistant — your reminders, notes, calendar, and quick answers, organized by AI on a server you own and a model you choose. Nothing leaves the room.

This repository holds the Qovira **application server**: a single Go binary that serves the JSON API, a realtime event stream, and the bundled web UI, backed by an encrypted SQLite (SQLCipher) database. It is the piece you deploy.

## How it works

- **One binary, one container.** `qovira serve` runs the whole backend — API, realtime events, and the static web UI — and is the container entrypoint.
- **Encrypted at rest.** State lives in a single SQLCipher database file under the data directory; the master key is supplied at runtime and never stored in the image.
- **Yours by architecture.** It runs on your server and points at the model endpoint you configure. There is no phone-home.

## Requirements

- Go 1.26+
- A C toolchain (GCC/Clang) and OpenSSL headers — CGO is required to build the SQLCipher driver.

## Build

```sh
make build
```

The binary is written to `./qovira`.

## Run

```sh
./qovira --help          # see all subcommands
./qovira serve           # start the server (needs QOVIRA_MASTER_KEY)
./qovira migrate up      # apply pending database migrations
./qovira healthcheck     # probe a locally running server
./qovira version         # print build information
```

The server reads its configuration from environment variables (and an optional `--config` file). At minimum it needs `QOVIRA_MASTER_KEY` to open the encrypted database; it listens on `:8080` and stores data under `./data` by default.

## Development

```sh
make test     # run tests
make race     # run tests with the race detector
make lint     # run golangci-lint
```

## Running with Docker

The Dockerfile uses `--mount=type=cache` and requires BuildKit (Docker Engine 23.0+ enables it by default; set `DOCKER_BUILDKIT=1` on older versions). Use `make docker-build`, which sets this automatically, or build directly:

```sh
DOCKER_BUILDKIT=1 docker build -t qovira:dev .
docker run --rm -p 8080:8080 -e QOVIRA_MASTER_KEY=<passphrase> -v qovira-data:/data qovira:dev
```

The server listens on `:8080` and stores the encrypted database under `/data` inside the container (backed by the named volume above).

### Supplying the master key

The master key **must never be baked into the image**. Two safe runtime options:

1. **Environment variable** (quick start):
   ```sh
   docker run ... -e QOVIRA_MASTER_KEY=<passphrase> qovira:dev
   ```

2. **`_FILE` indirection** (preferred in production — the key never appears in `docker inspect` or image history):
   ```sh
   # Write the key to a file (or use a Docker secret mount)
   echo -n "<passphrase>" > /run/secrets/master_key
   docker run ... \
     -e QOVIRA_MASTER_KEY_FILE=/run/secrets/master_key \
     -v /run/secrets/master_key:/run/secrets/master_key:ro \
     qovira:dev
   ```

### Health check

The image exposes `GET /healthz`. Docker queries it automatically via the built-in `HEALTHCHECK` instruction. To probe manually:

```sh
curl http://localhost:8080/healthz
# or inside a running container (distroless — no curl/shell):
docker exec <container> /usr/local/bin/qovira healthcheck
```

## License

[AGPL-3.0-only](./LICENSE) © OMNILIUM ADVANCED CYBERNETICS SRL
