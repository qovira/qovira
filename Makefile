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

.PHONY: build generate test race lint clean docker-build docker-run

build:
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/qovira

generate:
	go tool sqlc generate

test:
	CGO_ENABLED=$(CGO_ENABLED) go test ./...

race:
	CGO_ENABLED=$(CGO_ENABLED) go test -race ./...

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY)

# Build the Docker image locally (amd64 only; arm64 requires buildx + cross-gcc).
# VERSION/COMMIT/DATE are forwarded from the same shell variables used by `make build`.
# The Dockerfile uses --mount=type=cache and requires BuildKit; DOCKER_BUILDKIT=1
# ensures it is active even on older Docker Engine versions (23.0+ enables it by default).
docker-build:
	DOCKER_BUILDKIT=1 docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t qovira:$(VERSION) \
		.

# Run the Docker image with a volatile /data volume.
# Set QOVIRA_MASTER_KEY in your environment before calling this target.
# For production use QOVIRA_MASTER_KEY_FILE pointing at a Docker secret instead.
docker-run:
	docker run --rm \
		-p 8080:8080 \
		-e QOVIRA_MASTER_KEY \
		-v qovira-data:/data \
		qovira:$(VERSION)
