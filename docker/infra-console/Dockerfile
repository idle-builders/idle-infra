# syntax=docker/dockerfile:1
# -----------------------------------------------------------------------------
# infra-console — read-only console and MCP surface over platform state.
# See docs/plans/infra-console.md.
#
# Mirrors docker/storage-broker/Dockerfile: same toolchain, same runtime base,
# same non-root UID, so the two services have one deployment posture.
# -----------------------------------------------------------------------------

FROM golang:1.26-alpine AS build
WORKDIR /src

# Re-declared inside the stage on purpose: an ARG before the first FROM is
# global and is NOT visible within a build stage, so the -X below would have
# stamped an empty string.
ARG VERSION=dev

# Dependencies first (cached layer).
COPY docker/infra-console/go.mod docker/infra-console/go.sum ./
RUN go mod download

COPY docker/infra-console/*.go ./
COPY docker/infra-console/internal/ ./internal/
COPY docker/infra-console/web/ ./web/
# Static, stripped binary; reproducible paths.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /infra-console .

# The query helper shares the compactor's Arrow/Parquet code, but is copied
# into this image so an occasional dashboard read needs no resident query pod.
FROM rust:1.90-slim-bookworm AS telemetry-build
WORKDIR /src
COPY docker/telemetry-data/Cargo.toml docker/telemetry-data/Cargo.lock* ./
COPY docker/telemetry-data/src ./src
RUN cargo test --locked --release && cargo build --locked --release && strip target/release/telemetry-data

# distroless static: no shell/package manager, includes CA roots (needed for
# TLS to Zitadel, Swift, Cloudflare, GitHub and the Kubernetes API).
# :nonroot runs as 65532.
FROM gcr.io/distroless/base-nossl-debian12:nonroot
COPY --from=build /infra-console /infra-console
COPY --from=telemetry-build /src/target/release/telemetry-data /telemetry-data
COPY --from=telemetry-build /usr/lib/*-linux-gnu/libgcc_s.so.1 /usr/lib/libgcc_s.so.1
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/infra-console"]
