.DEFAULT_GOAL := build

.PHONY: build build-web build-go run docker lint test generate clean

# Build-identity ldflags — stamped from git at `make` time. The same variable names (QOVIRA_VERSION,
# QOVIRA_REVISION, QOVIRA_CREATED) are used as build-args in the Dockerfile and passed as --build-arg in CI,
# so the binary, image labels, and OCI metadata all share the same source of truth.
QOVIRA_VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "(devel)")
QOVIRA_REVISION  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
QOVIRA_CREATED   ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# -w -s strips the DWARF debug info and symbol table, reducing binary size. The three -X flags stamp the
# build identity into the package-level vars in internal/buildinfo.
GO_LDFLAGS := -w -s \
	-X github.com/qovira/qovira/internal/buildinfo.Version=$(QOVIRA_VERSION) \
	-X github.com/qovira/qovira/internal/buildinfo.Commit=$(QOVIRA_REVISION) \
	-X github.com/qovira/qovira/internal/buildinfo.BuildTime=$(QOVIRA_CREATED)

## build-web: install web dependencies and compile the SvelteKit SPA into internal/httpx/webdist/ via adapter-static.
build-web:
	pnpm -C web install --frozen-lockfile
	pnpm -C web build

## build-go: compile ./qovira from the no-embed stub (no SPA required) for fast manual CLI checks without the
##           web build, and type-check all packages via `go build ./...`. The binary serves the placeholder
##           page; use `make build` for one with the real SPA embedded.
build-go:
	go build ./...
	go build -ldflags "$(GO_LDFLAGS)" -o ./qovira ./cmd/qovira

## build: run the web build then compile the binary with the real SPA embedded.
##        Depends on build-web so webdist/ is always populated before the //go:embed directive compiles.
build: build-web
	go build -tags embed_spa -ldflags "$(GO_LDFLAGS)" -o ./qovira ./cmd/qovira

## run: build the embedded binary then serve it locally with dev-friendly env: port :18888, debug-level human-readable
##      (text) logs. Blocks until Ctrl-C.
run: build
	QOVIRA_ADDR=:18888 QOVIRA_LOG_LEVEL=debug QOVIRA_LOG_FORMAT=text ./qovira serve

## docker: build the multi-stage Docker image (tags qovira:dev).
docker:
	docker build -t qovira:dev .

## lint: run golangci-lint over Go sources, the web linter over the SPA, and actionlint over the workflows.
lint:
	golangci-lint run ./...
	pnpm -C web lint
	actionlint

## test: run the Go test suite (with race detector) and the web test suite.
test:
	go test -race ./...
	pnpm -C web test

## generate: regenerate derived artifacts from source (runs go generate ./...). Writes openapi.yaml at the
##           repo root from the live api.New registration — re-run after any handler or schema change.
generate:
	go generate ./...

## clean: remove the compiled binary and the generated webdist/ tree.
clean:
	rm -f ./qovira
	rm -rf internal/httpx/webdist/
