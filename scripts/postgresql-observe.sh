#!/usr/bin/env bash
# Emit agent-readable PostgreSQL query observations as one JSON object per sample.
#
# Usage:
#   scripts/postgresql-observe.sh
#   scripts/postgresql-observe.sh --limit 25 --min-active-ms 250
#   scripts/postgresql-observe.sh --watch 5

set -euo pipefail

NAMESPACE="${PG_NAMESPACE:-infra}"
PRIMARY="${PG_PRIMARY_POD:-postgresql-primary-0}"
CONTAINER="${PG_CONTAINER:-postgresql}"
DATABASE="${PG_DATABASE:-postgres}"
USER_NAME="${PG_USER:-postgres}"
PASSWORD_FILE="${PG_PASSWORD_FILE:-/opt/bitnami/postgresql/secrets/postgres-password}"
DSN="${PG_DSN:-}"
ENRICH_KUBERNETES="${PG_ENRICH_KUBERNETES:-auto}"
LIMIT=20
MIN_ACTIVE_MS=250
WATCH_SECONDS=0
INCLUDE_QUERY_TEXT=false

usage() {
  cat >&2 <<EOF
Usage: $0 [--limit N] [--min-active-ms N] [--watch SECONDS] [--include-query-text]

Set PG_DSN to monitor any PostgreSQL endpoint with local psql. When PG_DSN is
unset, PG_NAMESPACE, PG_PRIMARY_POD, PG_CONTAINER, PG_USER, PG_DATABASE, and
PG_PASSWORD_FILE configure Kubernetes exec mode.

Query text is omitted by default because SQL can contain secrets or personal data.
EOF
}

require_uint() {
  [[ "$2" =~ ^[0-9]+$ ]] || {
    echo "ERROR: $1 must be a non-negative integer" >&2
    exit 2
  }
}

while (($#)); do
  case "$1" in
    --limit)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      LIMIT="$2"
      require_uint "--limit" "$LIMIT"
      (( LIMIT > 0 && LIMIT <= 100 )) || {
        echo "ERROR: --limit must be between 1 and 100" >&2
        exit 2
      }
      shift 2
      ;;
    --min-active-ms)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      MIN_ACTIVE_MS="$2"
      require_uint "--min-active-ms" "$MIN_ACTIVE_MS"
      shift 2
      ;;
    --watch)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      WATCH_SECONDS="$2"
      require_uint "--watch" "$WATCH_SECONDS"
      (( WATCH_SECONDS >= 1 )) || {
        echo "ERROR: --watch must be at least 1 second" >&2
        exit 2
      }
      shift 2
      ;;
    --include-query-text)
      INCLUDE_QUERY_TEXT=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

command -v jq >/dev/null || {
  echo "ERROR: jq is required" >&2
  exit 2
}

if [[ -n "$DSN" ]]; then
  command -v psql >/dev/null || {
    echo "ERROR: psql is required when PG_DSN is set" >&2
    exit 2
  }
else
  command -v kubectl >/dev/null || {
    echo "ERROR: kubectl is required when PG_DSN is unset" >&2
    exit 2
  }
fi

run_psql() {
  if [[ -n "$DSN" ]]; then
    psql "$DSN" -X -qAt -v ON_ERROR_STOP=1 "$@"
  else
    kubectl exec -i -n "$NAMESPACE" "$PRIMARY" -c "$CONTAINER" -- \
      sh -lc 'PGPASSWORD=$(cat "$1")
        export PGPASSWORD
        shift
        exec psql "$@"' sh "$PASSWORD_FILE" \
      -h 127.0.0.1 -U "$USER_NAME" -d "$DATABASE" -X -qAt -v ON_ERROR_STOP=1 "$@"
  fi
}

HAS_PG_STAT_STATEMENTS="$(
  run_psql -c "SELECT CASE WHEN to_regclass('pg_stat_statements') IS NULL THEN 'false' ELSE 'true' END"
)"

read -r -d '' SQL <<'SQL' || true
WITH
settings AS (
  SELECT jsonb_object_agg(name, setting) AS value
  FROM pg_settings
  WHERE name IN (
    'max_connections',
    'shared_buffers',
    'work_mem',
    'maintenance_work_mem',
    'statement_timeout'
  )
),
connections AS (
  SELECT COALESCE(
    jsonb_agg(to_jsonb(c) ORDER BY c.connections DESC, c.application_name),
    '[]'::jsonb
  ) AS value
  FROM (
    SELECT
      usename AS role,
      COALESCE(NULLIF(application_name, ''), 'unattributed') AS application_name,
      state,
      count(*)::integer AS connections,
      round(max(EXTRACT(epoch FROM (clock_timestamp() - backend_start)) * 1000))::bigint
        AS oldest_connection_ms
    FROM pg_stat_activity
    WHERE backend_type = 'client backend'
      AND pid <> pg_backend_pid()
    GROUP BY usename, COALESCE(NULLIF(application_name, ''), 'unattributed'), state
  ) c
),
active_queries AS (
  SELECT COALESCE(
    jsonb_agg(jsonb_strip_nulls(to_jsonb(a)) ORDER BY a.duration_ms DESC),
    '[]'::jsonb
  ) AS value
  FROM (
    SELECT
      pid,
      usename AS role,
      COALESCE(NULLIF(application_name, ''), 'unattributed') AS application_name,
      host(client_addr) AS client_addr,
      round(EXTRACT(epoch FROM (clock_timestamp() - query_start)) * 1000)::bigint
        AS duration_ms,
      wait_event_type,
      wait_event,
      query_id,
      CASE WHEN :'include_query_text'::boolean
        THEN left(regexp_replace(query, '[[:space:]]+', ' ', 'g'), 1000)
      END AS query
    FROM pg_stat_activity
    WHERE backend_type = 'client backend'
      AND state = 'active'
      AND pid <> pg_backend_pid()
      AND clock_timestamp() - query_start >=
        make_interval(secs => (:'min_active_ms')::double precision / 1000)
    ORDER BY query_start
  ) a
),
top_queries AS (
  SELECT COALESCE(
    jsonb_agg(jsonb_strip_nulls(to_jsonb(q)) ORDER BY q.total_exec_ms DESC),
    '[]'::jsonb
  ) AS value
  FROM (
    SELECT
      userid::regrole::text AS role,
      (SELECT datname FROM pg_database WHERE oid = dbid) AS database,
      queryid AS query_id,
      calls,
      round(total_exec_time::numeric, 2) AS total_exec_ms,
      round(mean_exec_time::numeric, 2) AS mean_exec_ms,
      round(max_exec_time::numeric, 2) AS max_exec_ms,
      rows,
      round((rows::numeric / NULLIF(calls, 0)), 2) AS rows_per_call,
      shared_blks_read,
      shared_blks_hit,
      temp_blks_read * current_setting('block_size')::bigint AS temp_read_bytes,
      temp_blks_written * current_setting('block_size')::bigint AS temp_written_bytes,
      wal_bytes::numeric AS wal_bytes,
      array_remove(ARRAY[
        CASE WHEN mean_exec_time >= 1000 THEN 'high_mean_latency' END,
        CASE WHEN max_exec_time >= 5000 THEN 'high_max_latency' END,
        CASE WHEN rows::numeric / NULLIF(calls, 0) >= 10000 THEN 'high_rows_per_call' END,
        CASE WHEN shared_blks_read::numeric / NULLIF(calls, 0) >= 10000
          THEN 'high_disk_reads_per_call' END,
        CASE WHEN temp_blks_written > 0 THEN 'temporary_disk_spill' END,
        CASE WHEN wal_bytes::numeric >= 1073741824 THEN 'high_wal_volume' END
      ], NULL) AS risk_flags,
      CASE WHEN :'include_query_text'::boolean
        THEN left(regexp_replace(query, '[[:space:]]+', ' ', 'g'), 1000)
      END AS query
    FROM pg_stat_statements
    WHERE toplevel
    ORDER BY total_exec_time DESC
    LIMIT :'result_limit'
  ) q
),
stats AS (
  SELECT stats_reset FROM pg_stat_statements_info
)
SELECT jsonb_build_object(
  'schema_version', 1,
  'observed_at', clock_timestamp(),
  'server', jsonb_build_object(
    'version', current_setting('server_version'),
    'in_recovery', pg_is_in_recovery(),
    'stats_reset_at', stats.stats_reset,
    'settings', settings.value
  ),
  'connections', connections.value,
  'active_queries', active_queries.value,
  'top_queries', top_queries.value
)
FROM settings, connections, active_queries, top_queries, stats;
SQL

if [[ "$HAS_PG_STAT_STATEMENTS" != "true" ]]; then
  read -r -d '' SQL <<'SQL' || true
WITH
settings AS (
  SELECT jsonb_object_agg(name, setting) AS value
  FROM pg_settings
  WHERE name IN (
    'max_connections',
    'shared_buffers',
    'work_mem',
    'maintenance_work_mem',
    'statement_timeout'
  )
),
connections AS (
  SELECT COALESCE(
    jsonb_agg(to_jsonb(c) ORDER BY c.connections DESC, c.application_name),
    '[]'::jsonb
  ) AS value
  FROM (
    SELECT
      usename AS role,
      COALESCE(NULLIF(application_name, ''), 'unattributed') AS application_name,
      state,
      count(*)::integer AS connections,
      round(max(EXTRACT(epoch FROM (clock_timestamp() - backend_start)) * 1000))::bigint
        AS oldest_connection_ms
    FROM pg_stat_activity
    WHERE backend_type = 'client backend'
      AND pid <> pg_backend_pid()
    GROUP BY usename, COALESCE(NULLIF(application_name, ''), 'unattributed'), state
  ) c
),
active_queries AS (
  SELECT COALESCE(
    jsonb_agg(jsonb_strip_nulls(to_jsonb(a)) ORDER BY a.duration_ms DESC),
    '[]'::jsonb
  ) AS value
  FROM (
    SELECT
      pid,
      usename AS role,
      COALESCE(NULLIF(application_name, ''), 'unattributed') AS application_name,
      host(client_addr) AS client_addr,
      round(EXTRACT(epoch FROM (clock_timestamp() - query_start)) * 1000)::bigint
        AS duration_ms,
      wait_event_type,
      wait_event,
      query_id,
      CASE WHEN :'include_query_text'::boolean
        THEN left(regexp_replace(query, '[[:space:]]+', ' ', 'g'), 1000)
      END AS query
    FROM pg_stat_activity
    WHERE backend_type = 'client backend'
      AND state = 'active'
      AND pid <> pg_backend_pid()
      AND clock_timestamp() - query_start >=
        make_interval(secs => (:'min_active_ms')::double precision / 1000)
    ORDER BY query_start
  ) a
)
SELECT jsonb_build_object(
  'schema_version', 1,
  'observed_at', clock_timestamp(),
  'server', jsonb_build_object(
    'version', current_setting('server_version'),
    'in_recovery', pg_is_in_recovery(),
    'stats_reset_at', NULL,
    'settings', settings.value
  ),
  'connections', connections.value,
  'active_queries', active_queries.value,
  'top_queries', '[]'::jsonb
)
FROM settings, connections, active_queries;
SQL
fi

sample() {
  local database_json pod_map
  database_json="$(
    run_psql \
      -v result_limit="$LIMIT" \
      -v min_active_ms="$MIN_ACTIVE_MS" \
      -v include_query_text="$INCLUDE_QUERY_TEXT" <<<"$SQL"
  )"
  pod_map="{}"
  if [[ "$ENRICH_KUBERNETES" != "false" ]] && command -v kubectl >/dev/null; then
    pod_map="$(kubectl get pods -A -o json 2>/dev/null | jq -c '
      [.items[]
        | select(.status.podIP != null)
        | {key: .status.podIP, value: (.metadata.namespace + "/" + .metadata.name)}]
      | from_entries
    ')" || pod_map="{}"
  fi
  jq -c \
    --argjson pod_map "$pod_map" \
    --argjson pg_stat_statements_available "$HAS_PG_STAT_STATEMENTS" '
    .server.pg_stat_statements_available = $pg_stat_statements_available
    |
    .active_queries |= map(. + {client_pod: ($pod_map[.client_addr] // null)})
  ' <<<"$database_json"
}

if (( WATCH_SECONDS == 0 )); then
  sample
  exit
fi

while true; do
  sample
  sleep "$WATCH_SECONDS"
done
