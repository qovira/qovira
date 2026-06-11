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

## License

[AGPL-3.0-only](./LICENSE).
