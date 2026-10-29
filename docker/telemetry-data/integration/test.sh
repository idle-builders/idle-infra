#!/usr/bin/env bash
set -euo pipefail

COLLECTOR_IMAGE="${COLLECTOR_IMAGE:-ghcr.io/idle-builders/idle-infra/otel-collector:v0}"
TELEMETRY_IMAGE="${TELEMETRY_IMAGE:-ghcr.io/idle-builders/idle-infra/telemetry-data:v0}"
# minio/minio and minio/mc are no longer published on Docker Hub. This frozen
# Bitnami build carries both the server and the mc client on its PATH.
MINIO_IMAGE="bitnamilegacy/minio:2025.7.23-debian-12-r5@sha256:6dabb4a2088c9a79908de3bc05f4586c23ad2182c8908e7e3acbf61c1467fb20"
MC_IMAGE="${MINIO_IMAGE}"
METRICS_IMAGE="busybox:1.37.0-musl"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_KEY="${GITHUB_RUN_ID:-$$}"
NETWORK="o1-telemetry-${RUN_KEY}"
COLLECTOR="o1-collector-${RUN_KEY}"
MINIO="o1-minio-${RUN_KEY}"
METRICS="o1-metrics-${RUN_KEY}"

cleanup() {
  docker rm -f "${COLLECTOR}" "${MINIO}" "${METRICS}" >/dev/null 2>&1 || true
  docker network rm "${NETWORK}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker network create "${NETWORK}" >/dev/null
docker run -d --name "${METRICS}" --network "${NETWORK}" \
  -v "${SCRIPT_DIR}/prometheus.txt:/www/metrics:ro" \
  "${METRICS_IMAGE}" httpd -f -p 8080 -h /www >/dev/null
docker run -d --name "${COLLECTOR}" --network "${NETWORK}" --user 65532:65532 \
  --tmpfs /tmp/otel-state:rw,uid=65532,gid=65532,mode=0700 \
  -e S3_ENDPOINT="http://${MINIO}:9000" \
  -e PROMETHEUS_TARGET="${METRICS}:8080" \
  -e AWS_ACCESS_KEY_ID=minioadmin \
  -e AWS_SECRET_ACCESS_KEY=minioadmin \
  -v "${SCRIPT_DIR}/collector.yaml:/etc/otelcol-infra/config.yaml:ro" \
  "${COLLECTOR_IMAGE}" --config=/etc/otelcol-infra/config.yaml >/dev/null

READY=false
for _ in $(seq 1 30); do
  if docker run --rm --network "${NETWORK}" curlimages/curl:8.16.0 \
    -fsS "http://${COLLECTOR}:13133/" >/dev/null 2>&1; then
    READY=true
    break
  fi
  sleep 1
done
if [ "${READY}" != true ]; then
  docker logs "${COLLECTOR}" >&2
  echo "collector did not become ready" >&2
  exit 1
fi

for signal in logs traces; do
  docker run --rm --network "${NETWORK}" \
    -v "${SCRIPT_DIR}/${signal}.json:/fixture.json:ro" \
    curlimages/curl:8.16.0 -fsS \
    -H 'Content-Type: application/json' --data-binary @/fixture.json \
    "http://${COLLECTOR}:4318/v1/${signal}" >/dev/null
done

# The collector has accepted all three signals while S3 is unavailable. Bring
# storage up afterwards to prove the file-backed exporter queues retry.
docker run -d --name "${MINIO}" --network "${NETWORK}" --user 0 \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  --entrypoint minio "${MINIO_IMAGE}" server /data >/dev/null

for _ in $(seq 1 30); do
  if docker run --rm --network "${NETWORK}" curlimages/curl:8.16.0 \
    -fsS "http://${MINIO}:9000/minio/health/live" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

mc() {
  docker run --rm --network "${NETWORK}" -e HOME=/tmp --entrypoint /bin/sh "${MC_IMAGE}" -c \
    "mc alias set local http://${MINIO}:9000 minioadmin minioadmin >/dev/null && $*"
}

mc 'mc mb --ignore-existing local/telemetry >/dev/null'
for _ in $(seq 1 60); do
  RAW_COUNT="$(mc 'mc find local/telemetry/raw/v1 --name "*.gz" | wc -l' | tr -d ' ')"
  if [ "${RAW_COUNT}" -ge 3 ]; then
    break
  fi
  sleep 1
done
if [ "${RAW_COUNT:-0}" -lt 3 ]; then
  docker logs "${COLLECTOR}" >&2
  echo "expected raw logs, metrics, and traces after S3 recovery" >&2
  exit 1
fi

METRICS_KEY="$(mc 'mc find local/telemetry/raw/v1/metrics --name "*.gz" | head -n 1')"
METRICS_JSON="$(mc "mc cat ${METRICS_KEY}" | gzip -dc)"
for metric in pg_up pg_settings_max_connections pg_stat_database_numbackends \
  pg_stat_database_xact_commit pg_stat_database_xact_rollback \
  pg_stat_database_conflicts pg_stat_database_deadlocks \
  pg_stat_database_temp_bytes pg_database_size_bytes \
  pg_replication_is_replica pg_replication_lag_seconds \
  pg_replication_last_replay_seconds scrape_samples_scraped; do
  if ! grep -q "${metric}" <<<"${METRICS_JSON}"; then
    echo "approved metric ${metric} did not reach object storage" >&2
    exit 1
  fi
done
if grep -Eq 'pg_settings_work_mem_bytes|pg_stat_activity_count' <<<"${METRICS_JSON}"; then
  echo "unapproved metric family reached object storage" >&2
  exit 1
fi
if ! grep -q 'datname' <<<"${METRICS_JSON}" || grep -q 'datid' <<<"${METRICS_JSON}"; then
  echo "database metric labels are not reduced to the bounded allowlist" >&2
  exit 1
fi
if grep -q 'scrape_series_added' <<<"${METRICS_JSON}"; then
  echo "unneeded Prometheus scrape metadata reached object storage" >&2
  exit 1
fi

docker run --rm --network "${NETWORK}" \
  -e S3_ENDPOINT="http://${MINIO}:9000" -e S3_REGION=us-east-1 \
  -e S3_BUCKET=telemetry -e S3_ACCESS_KEY_ID=minioadmin \
  -e S3_SECRET_ACCESS_KEY=minioadmin -e COMPACTION_SEAL_DELAY_SECONDS=0 \
  -e RAW_RETENTION_SECONDS=7200 "${TELEMETRY_IMAGE}" compact >/dev/null

for signal in logs metrics traces; do
  MANIFEST_COUNT="$(mc "mc find local/telemetry/manifests/v1/${signal} --name '*.json' | wc -l" | tr -d ' ')"
  PARQUET_COUNT="$(mc "mc find local/telemetry/curated/v1/${signal} --name '*.parquet' | wc -l" | tr -d ' ')"
  if [ "${MANIFEST_COUNT}" -lt 1 ] || [ "${PARQUET_COUNT}" -lt 1 ]; then
    echo "expected ${signal} manifest and Parquet output; got manifests=${MANIFEST_COUNT}, parquet=${PARQUET_COUNT}" >&2
    exit 1
  fi
done

echo "PASS: filtered Prometheus + OTLP queue retry -> S3 JSON -> three-signal Parquet"
