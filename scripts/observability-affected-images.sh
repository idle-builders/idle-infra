#!/usr/bin/env bash
set -euo pipefail

collector=false
telemetry=false

while IFS= read -r path; do
  case "${path}" in
    docker/otel-collector/*)
      collector=true
      ;;
    docker/telemetry-data/*)
      telemetry=true
      ;;
    .github/workflows/build-observability.yml | \
      scripts/check-ghcr-pull.sh | \
      scripts/observability-affected-images.sh)
      # Control-plane changes can affect either build or its publication proof.
      collector=true
      telemetry=true
      ;;
  esac
done

printf 'collector=%s\n' "${collector}"
printf 'telemetry=%s\n' "${telemetry}"
