#!/usr/bin/env bash
# =============================================================================
# integration/test.sh — run the suite against the real services
# =============================================================================
#
# There is no Go toolchain on the host, so tests run in a container. That
# container needs the host's network namespace, not a bridge:
#
#   * Zitadel resolves its instance from the request's host name. It is
#     configured with ExternalDomain=localhost, so a request to
#     http://zitadel:8080 over the compose network answers "Instance not
#     found". Only http://localhost:8081 works.
#   * The Kubernetes API server is rancher-desktop's, published on
#     127.0.0.1:6443, and its certificate is issued for 127.0.0.1.
#
# Both are reachable only from the host namespace, hence --network host.
# On Docker Desktop this requires "Enable host networking" (Settings →
# Resources → Network); the script says so rather than failing obscurely.
#
#   integration/up.sh > .integ.env    # stand the services up
#   integration/test.sh               # run everything
#   integration/down.sh
# =============================================================================

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/.." && pwd)"
GO_IMAGE="golang:1.26-alpine"
ENV_FILE="${INTEG_ENV_FILE:-${HERE}/.integ.env}"

[ -s "${ENV_FILE}" ] || { echo "no ${ENV_FILE}: run 'integration/up.sh > ${ENV_FILE}' first" >&2; exit 1; }

# The kubeconfig is copied rather than mounted: rancher-desktop writes it with
# a client certificate path relative to the host home directory, and the copy
# lets it be rewritten to the in-container mount point.
WORK="$(mktemp -d "${HERE}/.run.XXXXXX")"
# No EXIT trap: this script ends in `exec docker run`, which replaces the
# shell and discards traps. down.sh removes these directories.
# Deliberately NOT the ambient $KUBECONFIG: sourcing scripts/env-from-keychain.sh
# exports one for the production cluster, and picking that up would run the
# suite against production tenants. up.sh records the cluster it actually
# seeded; use exactly that.
KUBECONFIG_SRC="$(sed -n "s/^export INTEG_KUBECONFIG='\(.*\)'$/\1/p" "${ENV_FILE}")"
KUBECONFIG_SRC="${KUBECONFIG_SRC:-${HOME}/.kube/config}"
if [ -r "${KUBECONFIG_SRC}" ]; then
  cp "${KUBECONFIG_SRC}" "${WORK}/kubeconfig"
  chmod 600 "${WORK}/kubeconfig"
fi

# Env for the container: the harness output, minus quoting, plus KUBECONFIG.
grep -E '^export INTEG_' "${ENV_FILE}" | sed "s/^export //; s/'//g" > "${WORK}/env"
# GitHub and Cloudflare are hosted services with no self-hostable server, so
# their tests run against the real API when a credential is present and skip
# otherwise. Forwarded from the caller's environment rather than the harness
# file, so a real token never lands on disk.
for passthrough in INTEG_GITHUB_TOKEN INTEG_GITHUB_OWNER INTEG_GITHUB_REPO INTEG_CLOUDFLARE_TOKEN INTEG_CLOUDFLARE_ACCOUNT; do
  if [ -n "${!passthrough:-}" ]; then
    echo "${passthrough}=${!passthrough}" >> "${WORK}/env"
  fi
done

echo "KUBECONFIG=/work/kubeconfig" >> "${WORK}/env"
echo "GOFLAGS=-mod=mod" >> "${WORK}/env"
echo "GOTOOLCHAIN=local" >> "${WORK}/env"
echo "CGO_ENABLED=1" >> "${WORK}/env"

if ! docker run --rm --network host "${GO_IMAGE}" true 2>/dev/null; then
  echo "docker --network host is unavailable." >&2
  echo "Enable it in Docker Desktop: Settings -> Resources -> Network -> Enable host networking." >&2
  exit 1
fi

# A named volume for the module cache. Without it every run re-downloads the
# whole dependency set, which dominates the wall time of a 6-second suite.
exec docker run --rm \
  --network host \
  -v infra-console-gomod:/go/pkg/mod \
  --env-file "${WORK}/env" \
  -v "${ROOT}":/src \
  -v "${WORK}":/work \
  -v "${HOME}/.rd":"${HOME}/.rd":ro \
  -w /src \
  "${GO_IMAGE}" \
  sh -c 'apk add --no-cache gcc musl-dev >/dev/null 2>&1; go test ./... "$@"' -- "$@"
