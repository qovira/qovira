# syntax=docker/dockerfile:1

# ── web stage ────────────────────────────────────────────────────────────────
# Build the SvelteKit SPA using adapter-static, which writes its output to ../internal/httpx/webdist relative to web/
# (i.e. /app/internal/httpx/webdist).
# glibc (trixie-slim) is required — musl breaks native build scripts (esbuild).
# Tag + manifest-list digest; bump both together (Renovate/Dependabot handles this).
FROM node:24-trixie-slim@sha256:366fdef91728b1b7fa18c84fba63b6e79ed77b7e10cc206878e9705da4d7b169 AS web

# Enable pnpm via corepack (version pinned by packageManager field in package.json).
RUN corepack enable

WORKDIR /app/web

# Copy dependency manifests first so the install layer is cached independently of source changes.
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./

# Install dependencies with a cache mount for the pnpm content-addressable store.
RUN --mount=type=cache,target=/pnpm-store \
    pnpm config set store-dir /pnpm-store && \
    pnpm install --frozen-lockfile

COPY web/ ./
RUN pnpm build


# ── build stage ──────────────────────────────────────────────────────────────
# Compile the Go binary with CGO_ENABLED=1 (cgo-ready for the SQLCipher store that lands in a later unit) and embed
# the SPA produced by the web stage.
# Tag + manifest-list digest; bump both together (Renovate/Dependabot handles this).
FROM golang:1.26-trixie@sha256:76a29248dedcd75870e95cbd90cc8cb356db082404ac7d3a5803f276c3ba79c9 AS build

# Build-identity args — passed from CI (and `make docker`) to stamp the binary via ldflags. These must be
# declared in the build stage (not just the runtime stage) so the go build command can consume them. The
# runtime stage re-declares them to populate the OCI labels, reading from the same --build-arg values.
ARG QOVIRA_VERSION=""
ARG QOVIRA_REVISION=""
ARG QOVIRA_CREATED=""

WORKDIR /app

# Copy Go module manifests and download dependencies as a separate cached layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the SPA artifact from the web stage BEFORE the embed directive compiles — //go:embed all:webdist fails
# at compile time if webdist/ is absent.
COPY --from=web /app/internal/httpx/webdist ./internal/httpx/webdist

# Copy the Go source tree (most volatile; comes last).
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# -w -s strips the DWARF/symbol tables to shrink the binary; the cache mounts keep CGO rebuilds fast.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -tags embed_spa \
      -ldflags "-w -s \
        -X github.com/qovira/qovira/internal/buildinfo.Version=${QOVIRA_VERSION} \
        -X github.com/qovira/qovira/internal/buildinfo.Commit=${QOVIRA_REVISION} \
        -X github.com/qovira/qovira/internal/buildinfo.BuildTime=${QOVIRA_CREATED}" \
      -o /qovira ./cmd/qovira


# ── runtime stage ────────────────────────────────────────────────────────────
# Distroless base-debian13:nonroot — glibc present for CGO; no shell, no curl, no wget; runs as user 65532 (nonroot)
# out of the box. Tracks debian13 (trixie) to match the build/web stages' glibc — a cgo binary linked against trixie
# glibc can fail to load on an older (debian12) userland.
# Pinned by manifest-list digest so `docker build` resolves per-arch safely.
FROM gcr.io/distroless/base-debian13:nonroot@sha256:ab7554b6d07ad354fad31957f8a1a813e65dfb93a8ad160568c79c3f2be6884f

# OCI image labels — static values stamped now; CI overrides the dynamic ones via --build-arg.
ARG QOVIRA_VERSION=""
ARG QOVIRA_REVISION=""
ARG QOVIRA_CREATED=""

LABEL org.opencontainers.image.title="qovira" \
      org.opencontainers.image.description="Self-hostable personal AI assistant" \
      org.opencontainers.image.source="https://github.com/qovira/qovira" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.vendor="OMNILIUM ADVANCED CYBERNETICS SRL" \
      org.opencontainers.image.version="${QOVIRA_VERSION}" \
      org.opencontainers.image.revision="${QOVIRA_REVISION}" \
      org.opencontainers.image.created="${QOVIRA_CREATED}"

COPY --from=build /qovira /qovira

# Numeric UID:GID — verifiably non-root; matches the distroless nonroot user.
USER 65532:65532

EXPOSE 18888

# Self-probe via the built-in healthcheck subcommand.
# QOVIRA_ADDR defaults to :18888; the healthcheck command rewrites the empty host to 127.0.0.1, so this works with
# no extra env vars.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/qovira", "healthcheck"]

# Single-process server: it spawns no children and handles SIGTERM itself (see signal.NotifyContext in internal/cli).
# Exec-form so PID 1 is qovira and receives signals directly.
ENTRYPOINT ["/qovira"]
CMD ["serve"]
