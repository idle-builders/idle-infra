#!/usr/bin/env bash
# =============================================================================
# scripts/gen-console.sh — regenerate infra-console artifacts from the contract
# =============================================================================
#
# docs/api/console-v1.yaml is the single source for the console's generated
# code. This script regenerates it. The output is committed, and CI fails if
# regenerating produces a diff — see docs/plans/infra-console.md (AC1).
#
# Usage:
#   scripts/gen-console.sh          # regenerate in place
#   scripts/gen-console.sh --check  # fail if the tree is out of date
#
# Runs in Docker so it needs no local Go toolchain. Pin OAPI_VERSION rather
# than tracking @latest, or the committed output drifts with upstream releases.
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

GO_IMAGE="${GO_IMAGE:-golang:1.26-alpine}"
OAPI_VERSION="${OAPI_VERSION:-v2.8.0}"

SVC_DIR="${REPO_ROOT}/docker/infra-console"
SPEC="${REPO_ROOT}/docs/api/console-v1.yaml"
GENERATED="${SVC_DIR}/internal/api/gen.go"
MCP_TOOLS="${SVC_DIR}/internal/mcp/tools_gen.go"

CHECK=0
[ "${1:-}" = "--check" ] && CHECK=1

# Fingerprint every generated file. --check regenerates and compares, so drift
# is caught whether or not the files are tracked yet.
#
# Not `git diff`: it ignores untracked files, and every generated file is
# untracked on a feature branch, so the gate passed unconditionally. Not
# `git status` either: on a branch where nothing is committed it reports the
# whole directory and the gate fails unconditionally. The content is the thing
# being checked, so hash the content.
fingerprint() {
  for f in "${GENERATED}" "${MCP_TOOLS}"; do
    [ -f "${f}" ] && shasum -a 256 "${f}"
  done
}

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
[ -f "${SPEC}" ] || { echo "spec not found: ${SPEC}" >&2; exit 1; }

# Docker on macOS cannot follow the /tmp -> /private/tmp symlink, so resolve
# every mount path physically before handing it to -v.
resolve() { (cd "$1" && pwd -P); }

BEFORE=""
[ "${CHECK}" -eq 1 ] && BEFORE="$(fingerprint)"

mkdir -p "${SVC_DIR}/internal/api"

echo "Generating ${GENERATED#"${REPO_ROOT}/"} from ${SPEC#"${REPO_ROOT}/"}"
docker run --rm \
  -v "$(resolve "${SVC_DIR}")":/svc \
  -v "$(resolve "$(dirname "${SPEC}")")":/spec:ro \
  -w /svc \
  -e GOFLAGS=-mod=mod \
  "${GO_IMAGE}" sh -c "
    set -e
    go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@${OAPI_VERSION} >/dev/null 2>&1
    /go/bin/oapi-codegen --config /svc/oapi-codegen.yaml /spec/$(basename "${SPEC}")
    go mod tidy
    gofmt -l . | tee /tmp/unformatted
    test ! -s /tmp/unformatted
  "

# Docker writes as root; hand the files back to the caller.
if [ "$(uname -s)" = "Linux" ]; then
  docker run --rm -v "$(resolve "${SVC_DIR}")":/svc "${GO_IMAGE}" \
    chown -R "$(id -u):$(id -g)" /svc >/dev/null 2>&1 || true
fi

echo "Generating ${MCP_TOOLS#"${REPO_ROOT}/"} from ${SPEC#"${REPO_ROOT}/"}"
mkdir -p "$(dirname "${MCP_TOOLS}")"
python3 "${REPO_ROOT}/scripts/gen-mcp-tools.py" "${SPEC}" "${MCP_TOOLS}"

if [ "${CHECK}" -eq 1 ]; then
  after="$(fingerprint)"
  if [ "${BEFORE}" != "${after}" ]; then
    echo "FAIL: generated code is out of date. Run scripts/gen-console.sh and commit the result." >&2
    diff <(echo "${BEFORE}") <(echo "${after}") >&2 || true
    exit 1
  fi
  echo "OK: generated code matches the contract."
fi

echo "Done."
