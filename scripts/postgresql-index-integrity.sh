#!/usr/bin/env bash
# Run the lightweight amcheck B-tree integrity check on PostgreSQL.
# This is evidence only: it never repairs or rebuilds an index.

set -euo pipefail

NAMESPACE="${PG_NAMESPACE:-infra}"
# @lore Routine checks default to the primary. The first production proof on
# 2026-08-21 ran on the replica, where WAL replay cancelled checks for the large
# tenant indexes. Those recovery conflicts are availability protection, not
# corruption, and would make a replica-only job produce false alarms.
POD="${PG_PRIMARY_POD:-postgresql-primary-0}"
CONTAINER="${PG_CONTAINER:-postgresql}"
PASSWORD_FILE="${PG_PASSWORD_FILE:-/opt/bitnami/postgresql/secrets/postgres-password}"
JOBS="${PG_AMCHECK_JOBS:-1}"
STATEMENT_TIMEOUT="${PG_AMCHECK_STATEMENT_TIMEOUT:-15min}"
LOCK_TIMEOUT="${PG_AMCHECK_LOCK_TIMEOUT:-5s}"

usage() {
  cat <<'USAGE'
Usage: postgresql-index-integrity.sh [postgres|zitadel ...]

Checks every valid B-tree index in postgres and zitadel by default. The routine
mode uses bt_index_check on the primary with one connection. It does not
enable parent, root-descent, heap-all-indexed, or heap/TOAST verification.
USAGE
}

if [ "${1:-}" = "--help" ] || [ "${1:-}" = "-h" ]; then
  usage
  exit 0
fi

if ! [[ "${JOBS}" =~ ^[1-4]$ ]]; then
  echo "PG_AMCHECK_JOBS must be an integer from 1 to 4." >&2
  exit 2
fi

if [ "$#" -eq 0 ]; then
  DATABASES=(postgres zitadel)
else
  DATABASES=("$@")
fi

for database in "${DATABASES[@]}"; do
  case "${database}" in
    postgres | zitadel) ;;
    *)
      echo "Unsupported database: ${database}. Expected postgres or zitadel." >&2
      exit 2
      ;;
  esac

  echo "Checking B-tree indexes on ${database} via ${POD}..."
  kubectl exec -n "${NAMESPACE}" "${POD}" -c "${CONTAINER}" -- \
    sh -ceu '
      PGPASSWORD=$(cat "$1")
      export PGPASSWORD
      export PGOPTIONS="-c statement_timeout=$2 -c lock_timeout=$3"
      exec pg_amcheck \
        --host=127.0.0.1 \
        --port=5432 \
        --username=postgres \
        --no-password \
        --database="$4" \
        --index="$4.*.*" \
        --no-dependent-indexes \
        --no-dependent-toast \
        --jobs="$5" \
        --progress
    ' sh "${PASSWORD_FILE}" "${STATEMENT_TIMEOUT}" "${LOCK_TIMEOUT}" "${database}" "${JOBS}"
done

echo "Index integrity checks passed."
