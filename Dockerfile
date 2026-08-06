# syntax=docker/dockerfile:1

# ============================================================================
# ARGUMENTS
# ============================================================================

ARG GO_VERSION=1.25
ARG BASE_IMAGE=debian:bookworm-slim
ARG DOCKER_CLI_VERSION=27.3.1
ARG BUILD_VERSION=latest
ARG BUILD_TIME

# ============================================================================
# Stage 1 — build the Go control-plane binary
# ============================================================================

FROM golang:${GO_VERSION}-bookworm AS builder

ARG BUILD_VERSION
ARG BUILD_TIME

WORKDIR /src

# Cache modules first — only re-downloads when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Pure-Go build (pgx, gin, moby client are all CGO-free), statically linked so it
# runs on the slim runtime image regardless of glibc version.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${BUILD_VERSION} -X main.buildTime=${BUILD_TIME}" \
    -o /out/gravyflow-api ./cmd/api

# ============================================================================
# Stage 2 — runtime
# ============================================================================
# This is a Docker control plane: at runtime it shells out to `nixpacks` and
# drives the host Docker daemon (mounted socket) to build & run user apps.
# So the runtime image must carry the docker CLI + nixpacks, not just the binary.
# ============================================================================

FROM ${BASE_IMAGE} AS runtime

ARG DOCKER_CLI_VERSION
ARG TARGETARCH
ARG BUILD_VERSION

# Environment variables (set at runtime via .env file)
ENV PORT=8080 \
    TZ=UTC \
    DEBIAN_FRONTEND=noninteractive

# Use bash with pipefail so a failed curl inside a pipe (e.g. `curl | bash`,
# `curl | tar`) fails the build instead of silently producing an image with no
# nixpacks/docker CLI — the failure mode behind "nixpacks not found" at runtime.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Install runtime dependencies
RUN --mount=type=cache,target=/var/cache/apt \
    --mount=type=cache,target=/var/lib/apt/lists \
    set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        tar \
        xz-utils \
        tzdata \
        bash \
        procps \
        net-tools \
        dnsutils \
        jq; \
    # Update CA certificates
    update-ca-certificates; \
    # Set timezone
    ln -snf "/usr/share/zoneinfo/$TZ" /etc/localtime && echo "$TZ" > /etc/timezone; \
    # Static docker CLI — used by nixpacks to talk to the mounted host daemon.
    case "${TARGETARCH:-amd64}" in \
      amd64) DOCKER_ARCH=x86_64 ;; \
      arm64) DOCKER_ARCH=aarch64 ;; \
      *)     DOCKER_ARCH=x86_64 ;; \
    esac; \
    curl -fsSL "https://download.docker.com/linux/static/stable/${DOCKER_ARCH}/docker-${DOCKER_CLI_VERSION}.tgz" \
      | tar -xz -C /tmp; \
    mv /tmp/docker/docker /usr/local/bin/docker; \
    rm -rf /tmp/docker; \
    # nixpacks — the builder the control plane invokes for user source.
    curl -fsSL https://nixpacks.com/install.sh | bash; \
    # Cleanup
    apt-get clean; \
    rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*; \
    # Assert both tools are present & on PATH now, so a broken install fails the
    # build here rather than every deployment job later.
    docker --version; \
    nixpacks --version

# ============================================================================
# Create non-root user for security
# ============================================================================

RUN set -eux; \
    groupadd -r -g 1000 gravyflow; \
    useradd -r -m -u 1000 -g gravyflow -s /bin/bash gravyflow; \
    # Create necessary directories
    mkdir -p /var/lib/gravyflow/apps /var/cache/nixpacks; \
    chown -R gravyflow:gravyflow /var/lib/gravyflow /var/cache/nixpacks

# ============================================================================
# Copy application binary
# ============================================================================

COPY --from=builder /out/gravyflow-api /usr/local/bin/gravyflow-api

# Make binary executable
RUN chmod +x /usr/local/bin/gravyflow-api

# ============================================================================
# Health check (optional but recommended)
# ============================================================================

HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=5 \
    CMD curl -fsS http://localhost:8080/api/health || exit 1

# ============================================================================
# Entrypoint script for pre-start validation
# ============================================================================

RUN printf '#!/bin/bash\n\
set -e\n\
\n\
# Validate required environment variables\n\
if [ -z "${AUTH_JWT_SECRET:-}" ]; then\n\
    echo "WARNING: AUTH_JWT_SECRET is not set. Using default (insecure)!"\n\
fi\n\
\n\
# Validate database connection\n\
if [ -n "${DATABASE_URL:-}" ]; then\n\
    echo "Database URL configured"\n\
else\n\
    echo "WARNING: DATABASE_URL is not set!"\n\
fi\n\
\n\
# Validate Redis connection\n\
if [ -n "${REDIS_ADDR:-}" ]; then\n\
    echo "Redis address configured: ${REDIS_ADDR}"\n\
else\n\
    echo "WARNING: REDIS_ADDR is not set!"\n\
fi\n\
\n\
# Check Docker socket mount\n\
if [ -S /var/run/docker.sock ]; then\n\
    echo "Docker socket mounted successfully"\n\
else\n\
    echo "WARNING: Docker socket is not mounted!"\n\
fi\n\
\n\
echo "Starting GravyFlow API..."\n\
exec /usr/local/bin/gravyflow-api\n\
' > /entrypoint.sh && chmod +x /entrypoint.sh

# ============================================================================
# Switch to non-root user
# ============================================================================

USER gravyflow

# ============================================================================
# Expose ports
# ============================================================================

EXPOSE 8080 9090

# ============================================================================
# Metadata labels
# ============================================================================

LABEL maintainer="GravyFlow Team <team@gravyflow.com>" \
      org.opencontainers.image.title="GravyFlow API" \
      org.opencontainers.image.description="GravyFlow Deployment Platform API" \
      org.opencontainers.image.version="${BUILD_VERSION}" \
      org.opencontainers.image.vendor="GravyFlow" \
      com.gravyflow.service="api"

# ============================================================================
# Entrypoint
# ============================================================================

ENTRYPOINT ["/entrypoint.sh"]