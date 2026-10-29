#!/usr/bin/env bash
# =============================================================================
# integration/run.sh — run the console against the integration environment
# =============================================================================
#
# Builds the binary and serves it on http://localhost:8099, wired to every
# service integration/up.sh brought up. No OIDC_ISSUER is set, so the console
# runs in the local development mode the contract describes (no sign-in);
# deployments always set it.
#
# GitHub and Cloudflare have no local equivalent. Export a real credential to
# populate those panels; without one they render as unavailable, which is the
# behaviour worth looking at anyway.
#
#   integration/up.sh > integration/.integ.env
#   integration/run.sh
# =============================================================================

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/.." && pwd)"
ENV_FILE="${INTEG_ENV_FILE:-${HERE}/.integ.env}"
PORT="${CONSOLE_PORT:-8099}"

[ -s "${ENV_FILE}" ] || { echo "no ${ENV_FILE}: run 'integration/up.sh > ${ENV_FILE}' first" >&2; exit 1; }

# shellcheck disable=SC1090
. "${ENV_FILE}"

WORK="$(mktemp -d "${HERE}/.run.XXXXXX")"
# No EXIT trap: this script ends in `exec docker run`, which replaces the
# shell and discards traps. down.sh removes these directories.
cp "${INTEG_KUBECONFIG:-${HOME}/.kube/config}" "${WORK}/kubeconfig"
chmod 600 "${WORK}/kubeconfig"

cat > "${WORK}/env" <<ENV
LISTEN_ADDR=:${PORT}
# No OIDC_ISSUER here, so authentication is off — and the binary now refuses to
# start that way unless the intent is explicit. This runner is local only.
AUTH_DISABLED=1
KUBECONFIG=/work/kubeconfig
POSTGRES_HOST=127.0.0.1
POSTGRES_PORT=55432
POSTGRES_DATABASE=postgres
# platform_reader, not the superuser: masking is column privileges, so
# connecting as postgres would show everything and hide the very behaviour
# this server exists to demonstrate.
POSTGRES_USER=platform_reader
POSTGRES_PASSWORD=reader-integration
POSTGRES_SSLMODE=disable
ZITADEL_URL=${INTEG_ZITADEL_URL}
ZITADEL_TOKEN=${INTEG_ZITADEL_TOKEN}
S3_ENDPOINT=${INTEG_S3_ENDPOINT}
S3_REGION=us-east-1
S3_ACCESS_KEY_ID=${INTEG_S3_ACCESS_KEY}
S3_SECRET_ACCESS_KEY=${INTEG_S3_SECRET_KEY}
GITHUB_TOKEN=${INTEG_GITHUB_TOKEN:-}
CLOUDFLARE_API_TOKEN=${INTEG_CLOUDFLARE_TOKEN:-}
CLOUDFLARE_ACCOUNT_ID=${INTEG_CLOUDFLARE_ACCOUNT:-}
LOG_LEVEL=debug
GOTOOLCHAIN=local
ENV

echo "console on http://localhost:${PORT}" >&2
exec docker run --rm --network host \
  -v infra-console-gomod:/go/pkg/mod \
  --name infra-console-dev \
  --env-file "${WORK}/env" \
  -v "${ROOT}":/src \
  -v "${WORK}":/work \
  -w /src \
  golang:1.26-alpine \
  go run .
