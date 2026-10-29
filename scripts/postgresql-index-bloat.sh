#!/usr/bin/env bash
# Measure one explicitly named B-tree index with pgstattuple's pgstatindex.
# This page-by-page scan is intentionally never run across every index.

set -euo pipefail

NAMESPACE="${PG_NAMESPACE:-infra}"
POD="${PG_READ_POD:-postgresql-read-0}"
CONTAINER="${PG_CONTAINER:-postgresql}"
PASSWORD_FILE="${PG_PASSWORD_FILE:-/opt/bitnami/postgresql/secrets/postgres-password}"
STATEMENT_TIMEOUT="${PGSTATINDEX_STATEMENT_TIMEOUT:-15min}"
LOCK_TIMEOUT="${PGSTATINDEX_LOCK_TIMEOUT:-5s}"

usage() {
  cat <<'USAGE'
Usage: postgresql-index-bloat.sh DATABASE SCHEMA INDEX

DATABASE must be postgres or zitadel. The command scans exactly one B-tree
index on the read replica and returns JSON with size, density, fragmentation,
and deleted-page evidence. It does not recommend or perform a rebuild.
USAGE
}

if [ "$#" -ne 3 ]; then
  usage >&2
  exit 2
fi

DATABASE="$1"
SCHEMA="$2"
INDEX="$3"

case "${DATABASE}" in
  postgres | zitadel) ;;
  *)
    echo "Unsupported database: ${DATABASE}. Expected postgres or zitadel." >&2
    exit 2
    ;;
esac

for identifier in "${SCHEMA}" "${INDEX}"; do
  if ! [[ "${identifier}" =~ ^[A-Za-z_][A-Za-z0-9_$]*$ ]]; then
    echo "Schema and index must be unquoted PostgreSQL identifiers." >&2
    exit 2
  fi
done

TARGET="${SCHEMA}.${INDEX}"
QUERY="
  SELECT row_to_json(measurement)
  FROM (
    SELECT
      current_database() AS database,
      '${TARGET}'::text AS index,
      clock_timestamp() AS measured_at,
      index_size AS index_bytes,
      tree_level,
      leaf_pages,
      deleted_pages,
      avg_leaf_density,
      leaf_fragmentation
    FROM pgstatindex('${TARGET}'::regclass)
  ) AS measurement;
"
kubectl exec -n "${NAMESPACE}" "${POD}" -c "${CONTAINER}" -- \
  sh -ceu '
    PGPASSWORD=$(cat "$1")
    export PGPASSWORD
    export PGOPTIONS="-c statement_timeout=$2 -c lock_timeout=$3"
    exec psql \
      --host=127.0.0.1 \
      --port=5432 \
      --username=postgres \
      --dbname="$4" \
      --no-password \
      --no-psqlrc \
      --set=ON_ERROR_STOP=1 \
      --tuples-only \
      --no-align \
      --command="$5"
  ' sh "${PASSWORD_FILE}" "${STATEMENT_TIMEOUT}" "${LOCK_TIMEOUT}" "${DATABASE}" "${QUERY}"
