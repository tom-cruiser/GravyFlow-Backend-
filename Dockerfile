# syntax=docker/dockerfile:1

# ─────────────────────────────────────────────────────────────────────────────
# Stage 1 — build the Go control-plane binary
# ─────────────────────────────────────────────────────────────────────────────
FROM golang:1.25-bookworm AS builder

WORKDIR /src

# Cache modules first — only re-downloads when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Pure-Go build (pgx, gin, moby client are all CGO-free), statically linked so it
# runs on the slim runtime image regardless of glibc version.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/gravyflow-api ./cmd/api

# ─────────────────────────────────────────────────────────────────────────────
# Stage 2 — runtime
#
# This is a Docker control plane: at runtime it shells out to `nixpacks` and
# drives the host Docker daemon (mounted socket) to build & run user apps.
# So the runtime image must carry the docker CLI + nixpacks, not just the binary.
# ─────────────────────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

ARG DOCKER_CLI_VERSION=27.3.1
ARG TARGETARCH

# Use bash with pipefail so a failed curl inside a pipe (e.g. `curl | bash`,
# `curl | tar`) fails the build instead of silently producing an image with no
# nixpacks/docker CLI — the failure mode behind "nixpacks not found" at runtime.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl git tar xz-utils; \
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
    rm -rf /var/lib/apt/lists/*; \
    # Assert both tools are present & on PATH now, so a broken install fails the
    # build here rather than every deployment job later.
    docker --version; \
    nixpacks --version

COPY --from=builder /out/gravyflow-api /usr/local/bin/gravyflow-api

ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["gravyflow-api"]
