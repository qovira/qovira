# syntax=docker/dockerfile:1

# ──────────────────────────────────────────────────────────────────────────────
# Build stage
# golang:1.26-bookworm — glibc matches the distroless base-debian12 (bookworm)
# runtime, so dynamic-linked libcrypto loads cleanly in the runtime image.
# ──────────────────────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm@sha256:13e7249b4618c115a175ea2627213131855233ecf465328cac30a0f754beb985 AS build

# Build-info ARGs — default to safe sentinels so a bare "docker build" works.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# BuildKit platform args (used to gate arch-specific cross-compile toolchains).
ARG TARGETARCH
ARG TARGETPLATFORM

# Install the extra toolchain packages the golang image doesn't include.
# gcc and libc6-dev are already present; only libssl-dev and pkg-config
# are needed to compile and link the bundled SQLCipher amalgamation against
# OpenSSL's libcrypto.
#
# For linux/arm64 cross-compilation (CI only — requires buildx and a host with
# the cross toolchain): add the arm64 architecture and install
# crossbuild-essential-arm64 + libssl-dev:arm64 so the CGO build can target
# aarch64. The cross-compile path is architecture-gated and not verified in the
# amd64-only local environment; it is CI-verified via the GitHub Actions matrix.
# dpkg --add-architecture is gated so amd64 builds don't pull arm64 package
# lists from apt, keeping the update step fast on the common path.
RUN if [ "$TARGETARCH" = "arm64" ]; then dpkg --add-architecture arm64; fi && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
        libssl-dev \
        pkg-config \
        $([ "$TARGETARCH" = "arm64" ] && echo "crossbuild-essential-arm64 libssl-dev:arm64" || true) && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Layer ordering: least-volatile (dependency manifests) before most-volatile
# (application source). A source-only change skips the expensive download layer.
COPY go.mod go.sum ./

# Download Go module dependencies. The cache mount persists the module cache
# across builds so repeated builds only fetch changed dependencies.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Build the binary with CGO enabled. go-sqlcipher compiles the SQLCipher
# amalgamation directly into the binary — no system libsqlcipher is needed or
# installed. OpenSSL libcrypto is linked dynamically (OpenSSL headers come from
# libssl-dev above; the shared library ships with the distroless runtime base).
#
# For arm64 cross-compilation, set CC to the cross-compiler and point
# PKG_CONFIG_PATH at the arm64 sysroot so pkg-config finds libssl.
#
# Both cache mounts are kept warm across builds: the module cache avoids
# re-downloading dependencies; the build cache avoids recompiling unchanged
# packages (especially the large SQLCipher amalgamation).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ "$TARGETARCH" = "arm64" ]; then \
      export CC=aarch64-linux-gnu-gcc; \
      export PKG_CONFIG_PATH=/usr/lib/aarch64-linux-gnu/pkgconfig; \
      export GOARCH=arm64; \
    fi && \
    CGO_ENABLED=1 go build \
      -trimpath \
      -ldflags "-X 'github.com/qovira/qovira/internal/cli.version=${VERSION}' \
                -X 'github.com/qovira/qovira/internal/cli.commit=${COMMIT}' \
                -X 'github.com/qovira/qovira/internal/cli.date=${DATE}'" \
      -o /out/qovira \
      ./cmd/qovira

# Pre-create /data with the correct ownership so uid 65532 can write the
# SQLCipher database when the container mounts a volume there. The distroless
# runtime image does not include mkdir or chown, so we must create the
# directory in the build stage and COPY it across.
RUN mkdir -p /data && chown 65532:65532 /data

# ──────────────────────────────────────────────────────────────────────────────
# Runtime stage — distroless base-debian12 (bookworm).
#
# Ships glibc and OpenSSL libcrypto. Required: the qovira binary links
# libcrypto dynamically (go-sqlcipher's SQLCipher amalgamation uses OpenSSL as
# its crypto backend). Do NOT swap to scratch or distroless/static — those lack
# libcrypto and the binary will fail to load.
# ──────────────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/base-debian12:nonroot@sha256:4ae8d0163a6f04d96f36e41324d76f00744f0db7545b6d04039c9e6fa1df77f3

# OCI image labels — wired from build ARGs for traceability.
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="qovira" \
      org.opencontainers.image.description="A private, self-hostable personal assistant — reminders, notes, calendar, and quick answers, organized by AI on a server you own and a model you choose." \
      org.opencontainers.image.vendor="OMNILIUM ADVANCED CYBERNETICS SRL" \
      org.opencontainers.image.source="https://github.com/qovira/qovira" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.version="${VERSION}"

# Copy the compiled binary from the build stage.
COPY --from=build /out/qovira /usr/local/bin/qovira

# Copy the pre-created /data directory (owned by 65532:65532) so the
# SQLCipher database can be opened at /data without a runtime chown.
COPY --from=build --chown=65532:65532 /data /data

# Declare /data as a volume — the runtime mount must be writable by uid 65532.
VOLUME ["/data"]

# ──────────────────────────────────────────────────────────────────────────────
# Secrets — NEVER bake master key material into the image.
#
# Supply the master key at runtime via one of:
#   -e QOVIRA_MASTER_KEY=<passphrase>
#   -e QOVIRA_MASTER_KEY_FILE=/run/secrets/master_key  (+ --mount a secret)
#
# The _FILE form is preferred in production (Coolify/Docker Swarm secrets):
# the key never appears in `docker inspect` or image history.
# ──────────────────────────────────────────────────────────────────────────────

# Runtime environment defaults — match the exact env var names config.Load reads.
ENV QOVIRA_HTTP_ADDR=":8080"
ENV QOVIRA_DATA_DIR="/data"

EXPOSE 8080

# Run as the distroless nonroot user (numeric — verifiable as non-root by
# orchestrators that compare against UID 0).
USER 65532:65532

# Default invocation: qovira serve.
# Alternative subcommands are reachable by overriding CMD, e.g.:
#   docker run qovira migrate up
#   docker run qovira healthcheck
ENTRYPOINT ["/usr/local/bin/qovira"]
CMD ["serve"]

# Distroless has no shell or curl, so the healthcheck calls the app's own
# healthcheck subcommand — the canonical house pattern for exec-form probes.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/qovira", "healthcheck"]
