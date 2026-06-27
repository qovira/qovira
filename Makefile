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

# WEBDIST is the target directory the embed directive reads.
WEBDIST := internal/httpx/webdist

# E2E binary name and ephemeral data dir.
E2E_BINARY   := qovira-e2e
E2E_DATA_DIR := /tmp/qovira-e2e-data

.PHONY: build build-go web sync-web generate test race lint fuzz clean docker-build docker-run e2e-server

# build: full pipeline — build the SvelteKit SPA, sync its output into webdist/,
# then compile the Go binary with -tags embed_spa so the real SPA is embedded.
# Requires Node 24 + pnpm.
build: web sync-web
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -tags embed_spa -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/qovira

# build-go: Go-only build — skips the SPA entirely and uses the in-code stub
# (spa_noembed.go). The binary's web UI shows a "not embedded" page. Safe on a
# fresh checkout with no Node/pnpm installed and no webdist/ directory.
build-go:
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/qovira

# web: build the SvelteKit SPA from web/.
web:
	cd web && pnpm install --frozen-lockfile && pnpm build

# sync-web: wipe and repopulate webdist/ from the SvelteKit build output.
# webdist/ is fully gitignored — nothing there is ever committed — so a clean
# wipe-and-copy is the simplest way to avoid orphaned stale chunks from a
# prior build without needing git clean.
sync-web:
	rm -rf $(WEBDIST)
	mkdir -p $(WEBDIST)
	cp -R web/build/. $(WEBDIST)/

generate:
	go tool sqlc generate

test:
	CGO_ENABLED=$(CGO_ENABLED) go test ./...

race:
	CGO_ENABLED=$(CGO_ENABLED) go test -race ./...

# fuzz: run each native fuzz target for a bounded time (default 30s each). `go test -fuzz` mutates one target per
# invocation, so we iterate explicitly. Override the per-target budget with `make fuzz FUZZTIME=2m`. The seed-corpus
# regression (every Fuzz* executed once, without mutation) already runs as part of `make test`; this target is for the
# active, mutation-driven pass — wire it into a nightly CI job, not the per-PR gate.
FUZZTIME ?= 30s
FUZZ_TARGETS := \
	internal/harness:FuzzDecodeConvCursor \
	internal/harness:FuzzConvCursorRoundTrip \
	internal/reminders:FuzzDecodeCursor \
	internal/reminders:FuzzCursorRoundTrip \
	internal/auth:FuzzParsePHC \
	internal/auth:FuzzParsePHCRoundTrip \
	internal/store:FuzzScanQueryViolations \
	internal/store:FuzzScopeGuardNoUserIDMustFlag

fuzz:
	@for target in $(FUZZ_TARGETS); do \
		pkg=$${target%%:*}; fn=$${target##*:}; \
		echo "== fuzzing $$fn in ./$$pkg ($(FUZZTIME)) =="; \
		CGO_ENABLED=$(CGO_ENABLED) go test ./$$pkg -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZTIME) || exit 1; \
	done

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
		-p 8000:8000 \
		-e QOVIRA_MASTER_KEY \
		-v qovira-data:/data \
		qovira:$(VERSION)

# e2e-server: build the SvelteKit SPA, embed it in the e2e binary, wipe the
# ephemeral data dir (so first-run admin seeding fires fresh), then exec the
# server. Playwright's webServer calls this target and polls /healthz until 200.
# The server env (master key, admin credentials, ports, fixture path) is
# injected by playwright.config.ts via webServer.env so it never appears here.
# Reuses the web + sync-web prerequisites so the SPA is always current.
e2e-server: web sync-web
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -tags e2e,embed_spa -ldflags "$(LDFLAGS)" \
		-o $(E2E_BINARY) ./cmd/qovira
	rm -rf $(E2E_DATA_DIR)
	mkdir -p $(E2E_DATA_DIR)
	exec ./$(E2E_BINARY) serve
