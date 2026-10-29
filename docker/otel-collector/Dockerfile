# syntax=docker/dockerfile:1

FROM otel/opentelemetry-collector-builder:0.159.0 AS build
WORKDIR /src
COPY manifest.yaml ./manifest.yaml
COPY transfermanager ./transfermanager
RUN cd transfermanager && go test ./... && cd .. && ocb --config manifest.yaml

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /src/_build/otelcol-infra /otelcol-infra
COPY config.yaml /etc/otelcol-infra/config.yaml
USER 65532:65532
EXPOSE 4317 4318 13133
ENTRYPOINT ["/otelcol-infra"]
CMD ["--config=/etc/otelcol-infra/config.yaml"]
