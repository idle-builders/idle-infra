#!/usr/bin/env bash
# Tear down the integration environment: containers, volumes, and the
# Kubernetes namespace created on the real cluster.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NS="${INTEG_NAMESPACE:-console-integration}"

# This deletes a namespace, so it must never run against production. The
# ambient KUBECONFIG is deliberately ignored: sourcing
# scripts/env-from-keychain.sh exports one for the Infomaniak cluster, and a
# `kubectl delete namespace` inheriting that would delete a real tenant
# namespace. Only an explicitly local context is accepted, and only via
# INTEG_KUBECONFIG.
KUBECONFIG_FILE="${INTEG_KUBECONFIG:-${HOME}/.kube/config}"
KUBECTX="$(KUBECONFIG="${KUBECONFIG_FILE}" kubectl config current-context 2>/dev/null || true)"
case "${KUBECTX}" in
  rancher-desktop|docker-desktop|kind-*|minikube|k3d-*)
    export KUBECONFIG="${KUBECONFIG_FILE}"
    # A context NAME is user-chosen metadata and can point anywhere. This runs
    # `kubectl delete namespace`, so confirm the API server itself is local.
    KUBESERVER="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || true)"
    case "${KUBESERVER}" in
      https://127.0.0.1:*|https://localhost:*|https://0.0.0.0:*|https://192.168.*|https://kubernetes.docker.internal:*)
        kubectl delete namespace "${NS}" --wait=false >/dev/null 2>&1
        ;;
      *)
        echo "refusing to delete: context '${KUBECTX}' points at ${KUBESERVER}, not a local API server" >&2
        ;;
    esac
    ;;
  "")
    echo "no kubernetes context in ${KUBECONFIG_FILE}; leaving the namespace alone" >&2
    ;;
  *)
    echo "refusing to delete namespace '${NS}' on context '${KUBECTX}':" >&2
    echo "not a recognised local cluster. Containers are still being removed." >&2
    ;;
esac

docker compose -f "${HERE}/compose.yaml" down -v >/dev/null 2>&1
rm -rf "${HERE}/.pat" "${HERE}"/.run.* "${HERE}/.integ.env" "${HERE}/.kubeconfig"
echo "integration environment torn down"
