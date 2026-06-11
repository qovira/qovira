CGO_ENABLED ?= 1
BINARY      := qovira
PKG         := github.com/qovira/qovira/internal/cli

# Build-info injection.
# git describe falls back to the short commit hash when no tag exists.
# Each variable has a safe fallback so the build works in a clean checkout
# with no tags (e.g. CI without fetch-depth=0).
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     := $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)

# -ldflags injects version metadata into the cli package vars at link time.
# Format: -X '<import/path>.<VarName>=<value>'
LDFLAGS := -X '$(PKG).version=$(VERSION)' \
           -X '$(PKG).commit=$(COMMIT)' \
           -X '$(PKG).date=$(DATE)'

GOFLAGS := -trimpath

.PHONY: build test race lint clean

build:
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/qovira

test:
	CGO_ENABLED=$(CGO_ENABLED) go test ./...

race:
	CGO_ENABLED=$(CGO_ENABLED) go test -race ./...

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY)
