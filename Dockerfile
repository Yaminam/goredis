# syntax=docker/dockerfile:1

# goredis container image.
#
#   docker build --build-arg VERSION=v0.1.0 -t goredis:dev .
#
# Builder: golang:1.27.4-alpine3.23 (tag verified on Docker Hub).
# Runtime: gcr.io/distroless/static-debian12:nonroot, pinned by the digest of
# its multi-arch index (amd64, arm64, ...). Dependabot proposes digest bumps.
#
# The builder stage always runs on the build host's platform and cross-compiles
# with GOOS/GOARCH, so multi-arch builds do not need QEMU emulation.

ARG GO_IMAGE=golang:1.27.4-alpine3.23
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
WORKDIR /src

# Download modules first so that this layer is reused when only source changes.
# go.sum* tolerates a module with no dependencies (no go.sum yet).
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    export CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}"; \
    for cmd in goredis goredis-check-aof goredis-bench; do \
        go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
            -o "/out/bin/${cmd}" "./cmd/${cmd}"; \
    done; \
    mkdir -p /out/data

FROM ${RUNTIME_IMAGE}

ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=unknown
LABEL org.opencontainers.image.title="goredis" \
      org.opencontainers.image.description="Redis-inspired in-memory key-value server with AOF persistence" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot"

COPY --from=build /out/bin/ /usr/local/bin/
# Empty data directory owned by the runtime user. A named Docker volume mounted
# here is initialised from this directory, so it inherits the 65532 ownership.
COPY --from=build --chown=65532:65532 /out/data /data

ENV GOREDIS_ADDR=0.0.0.0:6379 \
    GOREDIS_ADMIN_ADDR=0.0.0.0:9379 \
    GOREDIS_AOF_PATH=/data/appendonly.aof \
    GOREDIS_LOG_FORMAT=json

VOLUME ["/data"]
EXPOSE 6379 9379
USER 65532:65532

# The image has no shell or curl, so the binary probes its own /readyz.
# --start-interval needs Docker Engine 25+; older engines ignore it.
HEALTHCHECK --interval=10s --timeout=3s --start-period=60s --start-interval=2s --retries=3 \
    CMD ["/usr/local/bin/goredis", "-healthcheck"]

STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/goredis"]
