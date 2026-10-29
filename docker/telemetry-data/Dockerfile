# syntax=docker/dockerfile:1

FROM rust:1.90-slim-bookworm AS build
WORKDIR /src
COPY Cargo.toml Cargo.lock* ./
COPY src ./src
RUN cargo test --locked --release && \
    cargo build --locked --release && \
    strip target/release/telemetry-data

FROM gcr.io/distroless/base-nossl-debian12:nonroot
COPY --from=build /src/target/release/telemetry-data /telemetry-data
# Rust uses libgcc_s for stack unwinding. Copy that one runtime library instead
# of inheriting the larger cc image (and its unused OpenSSL installation).
COPY --from=build /usr/lib/*-linux-gnu/libgcc_s.so.1 /usr/lib/libgcc_s.so.1
USER 65532:65532
ENTRYPOINT ["/telemetry-data"]
CMD ["compact"]
