.DEFAULT_GOAL := build

.PHONY: build build-web build-go run docker lint test clean

## build-web: install web dependencies and compile the SvelteKit SPA into internal/httpx/webdist/ via adapter-static.
build-web:
	pnpm -C web install --frozen-lockfile
	pnpm -C web build

## build-go: compile ./qovira from the no-embed stub (no SPA required) for fast manual CLI checks without the
##           web build, and type-check all packages via `go build ./...`. The binary serves the placeholder
##           page; use `make build` for one with the real SPA embedded.
build-go:
	go build ./...
	go build -o ./qovira ./cmd/qovira

## build: run the web build then compile the binary with the real SPA embedded.
##        Depends on build-web so webdist/ is always populated before the //go:embed directive compiles.
build: build-web
	go build -tags embed_spa -o ./qovira ./cmd/qovira

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

## clean: remove the compiled binary and the generated webdist/ tree.
clean:
	rm -f ./qovira
	rm -rf internal/httpx/webdist/
