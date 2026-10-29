#!/usr/bin/env bash
# Read-only PostgreSQL replication health check.

set -euo pipefail

NAMESPACE="${PG_NAMESPACE:-infra}"
PRIMARY="${PG_PRIMARY_POD:-postgresql-primary-0}"
REPLICA="${PG_REPLICA_POD:-postgresql-read-0}"
CONTAINER="${PG_CONTAINER:-postgresql}"
DATABASE="${PG_DATABASE:-postgres}"
USER_NAME="${PG_USER:-postgres}"
PASSWORD_FILE="${PG_PASSWORD_FILE:-/opt/bitnami/postgresql/secrets/postgres-password}"
PRIMARY_DSN="${PG_DSN:-}"
REPLICA_DSN="${PG_REPLICA_DSN:-}"
EXPECTED_REPLICAS="${PG_EXPECTED_REPLICAS:-1}"
REPLICA_APPLICATION_NAME="${PG_REPLICA_APPLICATION_NAME:-}"
MAX_LAG_BYTES="${PG_MAX_REPLICATION_LAG_BYTES:-16777216}"

usage() {
  cat >&2 <<EOF
Usage: $0

Set PG_DSN for any PostgreSQL primary and optionally PG_REPLICA_DSN to verify
the standby directly. When PG_DSN is unset, PG_NAMESPACE, PG_PRIMARY_POD,
PG_REPLICA_POD, PG_CONTAINER, PG_USER, PG_DATABASE, and PG_PASSWORD_FILE
configure Kubernetes exec mode.
EOF
}

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[[ "$EXPECTED_REPLICAS" =~ ^[0-9]+$ ]] || fail "PG_EXPECTED_REPLICAS must be an integer"
[[ "$MAX_LAG_BYTES" =~ ^[0-9]+$ ]] || fail "PG_MAX_REPLICATION_LAG_BYTES must be an integer"

case "${1:-}" in
  -h|--help)
    usage
    exit 0
    ;;
  "")
    ;;
  *)
    usage
    exit 2
    ;;
esac

run_dsn_psql() {
  local dsn="$1"
  shift
  psql "$dsn" -X -Atq -v ON_ERROR_STOP=1 "$@"
}

run_pod_psql() {
  local pod="$1"
  shift
  kubectl exec -n "$NAMESPACE" "$pod" -c "$CONTAINER" -- \
    sh -lc 'PGPASSWORD=$(cat "$1")
      export PGPASSWORD
      shift
      exec psql "$@"' sh "$PASSWORD_FILE" \
    -h 127.0.0.1 -U "$USER_NAME" -d "$DATABASE" -X -Atq -v ON_ERROR_STOP=1 "$@"
}

if [[ -n "$PRIMARY_DSN" ]]; then
  command -v psql >/dev/null || fail "psql is required when PG_DSN is set"
  primary_sql() { run_dsn_psql "$PRIMARY_DSN" "$@"; }
  if [[ -n "$REPLICA_DSN" ]]; then
    replica_sql() { run_dsn_psql "$REPLICA_DSN" "$@"; }
  fi
else
  command -v kubectl >/dev/null || fail "kubectl is required when PG_DSN is unset"
  for pod in "$PRIMARY" "$REPLICA"; do
    ready="$(kubectl get pod -n "$NAMESPACE" "$pod" \
      -o jsonpath='{.status.containerStatuses[?(@.name=="'"$CONTAINER"'")].ready}')"
    [[ "$ready" == "true" ]] || fail "$pod is not ready"

    reason="$(kubectl get pod -n "$NAMESPACE" "$pod" \
      -o jsonpath='{.status.containerStatuses[?(@.name=="'"$CONTAINER"'")].lastState.terminated.reason}')"
    if [[ "$reason" == "OOMKilled" ]]; then
      echo "WARN: $pod has a historical OOM termination; inspect restart time and trend" >&2
    fi
  done
  primary_sql() { run_pod_psql "$PRIMARY" "$@"; }
  replica_sql() { run_pod_psql "$REPLICA" "$@"; }
fi

application_filter=""
if [[ -n "$REPLICA_APPLICATION_NAME" ]]; then
  application_filter="AND application_name = :'replica_application_name'"
fi

replication="$(
  primary_sql \
    -v replica_application_name="$REPLICA_APPLICATION_NAME" \
    -c "SELECT count(*) || '|' ||
      COALESCE(max(pg_wal_lsn_diff(sent_lsn, replay_lsn)), -1)::bigint
      FROM pg_stat_replication
      WHERE state = 'streaming' $application_filter"
)"
streaming_replicas="${replication%%|*}"
lag_bytes="${replication##*|}"

(( streaming_replicas >= EXPECTED_REPLICAS )) ||
  fail "streaming replicas=${streaming_replicas} (expected at least ${EXPECTED_REPLICAS})"
(( lag_bytes <= MAX_LAG_BYTES )) ||
  fail "replication lag is ${lag_bytes} bytes (limit ${MAX_LAG_BYTES})"

if declare -F replica_sql >/dev/null; then
  in_recovery="$(replica_sql -c "SELECT pg_is_in_recovery()")"
  [[ "$in_recovery" == "t" ]] || fail "configured replica is not in recovery"
fi

echo "OK: streaming_replicas=${streaming_replicas}; max_lag=${lag_bytes} bytes"
