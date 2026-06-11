# qovira

The Qovira application server.

## Requirements

- Go 1.26+
- GCC (CGO is required for the SQLCipher database driver)

## Build

```sh
make build
```

The binary is written to `./qovira`.

## Run

```sh
./qovira --help
```

## Development

```sh
make test     # run tests
make race     # run tests with the race detector
make lint     # run golangci-lint
```

## Running with Docker

The Dockerfile uses `--mount=type=cache` and requires BuildKit (Docker Engine
23.0+ enables it by default; set `DOCKER_BUILDKIT=1` on older versions). Use
`make docker-build` which sets this automatically, or build directly:

```sh
DOCKER_BUILDKIT=1 docker build -t qovira:dev .
docker run --rm -p 8080:8080 -e QOVIRA_MASTER_KEY=<passphrase> -v qovira-data:/data qovira:dev
```

The server listens on `:8080` and stores the encrypted SQLCipher database under
`/data` inside the container (backed by the named volume above).

### Supplying the master key

The master key **must never be baked into the image**. Two safe runtime options:

1. **Environment variable** (quick start):
   ```sh
   docker run ... -e QOVIRA_MASTER_KEY=<passphrase> qovira:dev
   ```

2. **`_FILE` indirection** (preferred in production — the key never appears in
   `docker inspect` or image history):
   ```sh
   # Write the key to a file (or use a Docker secret mount)
   echo -n "<passphrase>" > /run/secrets/master_key
   docker run ... \
     -e QOVIRA_MASTER_KEY_FILE=/run/secrets/master_key \
     -v /run/secrets/master_key:/run/secrets/master_key:ro \
     qovira:dev
   ```

### Health check

The image exposes `GET /healthz`. Docker queries it automatically via the
built-in `HEALTHCHECK` instruction. To probe manually:

```sh
curl http://localhost:8080/healthz
# or inside a running container (distroless — no curl/shell):
docker exec <container> /usr/local/bin/qovira healthcheck
```

## License

[AGPL-3.0-only](./LICENSE).
