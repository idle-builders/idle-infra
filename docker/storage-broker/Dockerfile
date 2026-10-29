# syntax=docker/dockerfile:1
# -----------------------------------------------------------------------------
# storage-broker — credential-mediating S3 presign broker.
# See docs/plans/presign-storage-broker.md.
# -----------------------------------------------------------------------------

FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first (cached layer).
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
# Static, stripped binary; reproducible paths.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /storage-broker .

# distroless static: no shell/package manager, includes CA roots (needed for TLS
# to the S3 gateway, Zitadel, and the Kubernetes API). :nonroot runs as 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /storage-broker /storage-broker
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/storage-broker"]
