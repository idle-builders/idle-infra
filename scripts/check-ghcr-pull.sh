#!/usr/bin/env bash
# Verify that the configured deployment credential can pull a GHCR image.
#
# This intentionally performs a real `docker pull`, not just a registry manifest
# lookup: Kubernetes failed on layer/blob authorization even when token exchange
# succeeded, so the preflight must exercise the same path kubelet uses.
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: check-ghcr-pull.sh <image-ref> [image-ref ...]

Environment:
  GHCR_USERNAME or TF_VAR_ghcr_username
  GHCR_TOKEN    or TF_VAR_ghcr_token

Example:
  GHCR_USERNAME=tobias GHCR_TOKEN=... \
    scripts/check-ghcr-pull.sh ghcr.io/ambrovia/infra/storage-broker@sha256:...
USAGE
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  usage
  exit 0
fi

if [ "$#" -eq 0 ]; then
  usage
  exit 2
fi

username="${GHCR_USERNAME:-${TF_VAR_ghcr_username:-}}"
token="${GHCR_TOKEN:-${TF_VAR_ghcr_token:-}}"

if [ -z "$username" ] || [ -z "$token" ]; then
  echo "error: GHCR_USERNAME/GHCR_TOKEN or TF_VAR_ghcr_username/TF_VAR_ghcr_token must be set" >&2
  exit 2
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "error: docker is required to verify a real pull" >&2
  exit 2
fi

tmp_docker_config="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp_docker_config"
}
trap cleanup EXIT

export DOCKER_CONFIG="$tmp_docker_config"

printf '%s' "$token" | docker login ghcr.io --username "$username" --password-stdin >/dev/null

for image_ref in "$@"; do
  case "$image_ref" in
    ghcr.io/*) ;;
    *)
      echo "error: image must be a ghcr.io reference: $image_ref" >&2
      exit 2
      ;;
  esac

  docker pull "$image_ref" >/dev/null
  echo "verified GHCR pull access for $image_ref"
done

docker logout ghcr.io >/dev/null 2>&1 || true
