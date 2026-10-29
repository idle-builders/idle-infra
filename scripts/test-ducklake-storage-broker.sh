#!/usr/bin/env bash
# Real DuckDB/DuckLake acceptance probe for the governed broker S3 subset.
# Uses temporary in-memory DuckDB credentials only; the SESSION_TOKEN is a
# projected Kubernetes token and the configured signing secret has no authority.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BROKER_NS="${BROKER_NS:-infra}"
TENANT_NS="${TENANT_NS:?Set TENANT_NS}"
PUBLISHER_SA="${PUBLISHER_SA:?Set PUBLISHER_SA}"
LOCAL_PORT="${LOCAL_PORT:-18081}"
CONTROL_LOCAL_PORT="${CONTROL_LOCAL_PORT:-18080}"
DUCKLAKE_BUCKET="${DUCKLAKE_BUCKET:?Set DUCKLAKE_BUCKET}"
DUCKLAKE_AUTHORITY="${DUCKLAKE_AUTHORITY:-workspace-11111111-1111-4111-8111-111111111111}"
DUCKLAKE_OTHER_AUTHORITY="${DUCKLAKE_OTHER_AUTHORITY:-workspace-22222222-2222-4222-8222-222222222222}"
PROBE_ID="$(date +%s)-$$"
DUCKLAKE_DATA_PREFIX="${DUCKLAKE_ROOT:-ducklake/}acceptance-${PROBE_ID}/"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing dependency: $1" >&2; exit 2; }; }
need kubectl
need node
need npm
need jq
need psql
need nc
need curl

case "$DUCKLAKE_AUTHORITY" in
  tenant)
    EXPECTED_KIND=tenant
    EXPECTED_WORKSPACE=""
    ;;
  workspace-????????-????-????-????-????????????)
    EXPECTED_KIND=workspace
    EXPECTED_WORKSPACE="${DUCKLAKE_AUTHORITY#workspace-}"
    ;;
  *)
    echo "DUCKLAKE_AUTHORITY must be tenant or workspace-<uuid>" >&2
    exit 2
    ;;
esac

if [ -z "${STORAGE_BROKER_TEST_DATABASE_URL:-}" ] && [ -z "${PGHOST:-}" ]; then
  echo "set STORAGE_BROKER_TEST_DATABASE_URL or standard libpq PG* variables to verify governed rows" >&2
  exit 2
fi

run_psql() {
  local attempt status query_file output_file
  query_file="$(mktemp "$probe_tmp/psql.XXXXXX")"
  output_file="$(mktemp "$probe_tmp/psql-output.XXXXXX")"
  cat >"$query_file"
  for attempt in 1 2 3 4; do
    : >"$output_file"
    if [ -n "${STORAGE_BROKER_TEST_DATABASE_URL:-}" ]; then
      if psql "$STORAGE_BROKER_TEST_DATABASE_URL" "$@" -v tenant="$TENANT_NS" -v physical_bucket="$DUCKLAKE_BUCKET" -f "$query_file" >"$output_file"; then
        cat "$output_file"
        return 0
      else
        status=$?
      fi
    else
      if psql "$@" -v tenant="$TENANT_NS" -v physical_bucket="$DUCKLAKE_BUCKET" -f "$query_file" >"$output_file"; then
        cat "$output_file"
        return 0
      else
        status=$?
      fi
    fi
    [ "$status" -eq 2 ] || return "$status"
    [ "$attempt" -eq 4 ] || sleep 2
  done
  return 2
}

probe_tmp="$(mktemp -d /tmp/ducklake-storage-broker.XXXXXX)"

cleanup_active_objects() {
  if [ -z "${DUCKLAKE_SESSION_TOKEN:-}" ] || [ -z "${broker_pf_pid:-}" ]; then
    return 0
  fi
  local authority authorization key encoded_key
  while IFS=$'\t' read -r key authority; do
    [ -n "$key" ] || continue
    authorization="AWS4-HMAC-SHA256 Credential=${authority}/20260908/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-security-token, Signature=acceptance"
    encoded_key="$(jq -rn --arg key "$key" '$key | @uri')"
    curl --fail --silent --show-error --request DELETE \
      --header "Authorization: $authorization" \
      --header "x-amz-security-token: $DUCKLAKE_SESSION_TOKEN" \
      "http://127.0.0.1:${LOCAL_PORT}/${DUCKLAKE_BUCKET}/${encoded_key}" >/dev/null
  done < <(
    run_psql -At -F $'\t' -v ON_ERROR_STOP=1 -v prefix="$DUCKLAKE_DATA_PREFIX" <<'SQL'
SELECT physical_key,
       CASE authority_kind
         WHEN 'tenant' THEN 'tenant'
         ELSE 'workspace-' || workspace_id::text
       END
FROM storage_broker.storage_objects
WHERE tenant_id = :'tenant'
  AND artifact_class = 'ducklake_data'
  AND physical_bucket = :'physical_bucket'
  AND physical_key LIKE :'prefix' || '%'
  AND state = 'active';
SQL
  )
}

finish() {
  local status=$?
  cleanup_active_objects >/dev/null 2>&1 || true
  kill "${broker_pf_pid:-}" 2>/dev/null || true
  rm -rf "$probe_tmp"
  exit "$status"
}
trap finish EXIT

if [ -z "${DUCKLAKE_SESSION_TOKEN:-}" ]; then
  DUCKLAKE_SESSION_TOKEN="$(kubectl create token "$PUBLISHER_SA" -n "$TENANT_NS" --audience storage-broker --duration 10m)"
fi

kubectl -n "$BROKER_NS" port-forward svc/storage-broker "${CONTROL_LOCAL_PORT}:80" "${LOCAL_PORT}:81" >/dev/null 2>&1 &
broker_pf_pid=$!
for _ in $(seq 1 40); do
  if nc -z 127.0.0.1 "$LOCAL_PORT" >/dev/null 2>&1; then break; fi
  sleep 0.25
done

lake_inventory() {
  local cursor="${1:-}" body
  body="$(jq -cn --arg workspace "$EXPECTED_WORKSPACE" --arg cursor "$cursor" \
    '{schemaVersion:1,authority:{kind:"workspace",workspaceId:$workspace},limit:1000} +
      (if $cursor == "" then {} else {cursor:$cursor} end)')"
  curl --fail --silent --show-error -X POST "http://127.0.0.1:${CONTROL_LOCAL_PORT}/v2/lake-objects/inventory" \
    -H "Authorization: Bearer $DUCKLAKE_SESSION_TOKEN" -H 'Content-Type: application/json' -d "$body"
}

npm install --silent --prefix "$probe_tmp" @duckdb/node-api@1.5.4-r.1
export DUCKDB_NODE_API_ROOT="$probe_tmp/node_modules/@duckdb/node-api"
export DUCKLAKE_BROKER_ENDPOINT="127.0.0.1:${LOCAL_PORT}"
export DUCKLAKE_BUCKET DUCKLAKE_SESSION_TOKEN DUCKLAKE_AUTHORITY DUCKLAKE_OTHER_AUTHORITY DUCKLAKE_DATA_PREFIX
export DUCKLAKE_METADATA_PATH="${DUCKLAKE_METADATA_PATH:-$probe_tmp/catalog.ducklake}"

result="$(node "$(dirname "$0")/ducklake-storage-broker-probe.mjs")"
jq -e '.inserted == 25000 and .committed == 25001 and .snapshots >= 5 and .files >= 1 and
  .crossWorkspaceDenied == true and (.scheduled | length) > 0 and
  (.cleaned | length) == (.scheduled | length) and .obsoleteMissing == true and
  .currentReadable == true and .publicUntouched == true and .peerUntouched == true and
  .listingDenied == true and .multipartDenied == true' <<<"$result" >/dev/null

read -r total active deleted wrong_scope <<<"$(
  run_psql -At -F ' ' -v ON_ERROR_STOP=1 \
    -v prefix="$DUCKLAKE_DATA_PREFIX" -v expected_kind="$EXPECTED_KIND" -v expected_workspace="$EXPECTED_WORKSPACE" <<'SQL'
SELECT
  count(*),
  count(*) FILTER (WHERE state = 'active'),
  count(*) FILTER (WHERE state = 'deleted'),
  count(*) FILTER (
    WHERE physical_key NOT IN (
            :'prefix' || 'public-sentinel.bin',
            :'prefix' || 'peer-sentinel.bin'
          )
      AND (authority_kind <> :'expected_kind'
        OR (:'expected_kind' = 'tenant' AND workspace_id IS NOT NULL)
        OR (:'expected_kind' = 'workspace' AND workspace_id::text <> :'expected_workspace'))
  )
FROM storage_broker.storage_objects
WHERE tenant_id = :'tenant'
  AND artifact_class = 'ducklake_data'
  AND physical_bucket = :'physical_bucket'
  AND physical_key LIKE :'prefix' || '%';
SQL
)"

for count in "$total" "$active" "$deleted" "$wrong_scope"; do
  case "$count" in
    "" | *[!0-9]*)
      echo "governed row verification returned non-numeric counts" >&2
      exit 1
      ;;
  esac
done

if [ "$total" -lt 1 ] || [ "$deleted" -lt 1 ] || [ "$wrong_scope" -ne 0 ]; then
  echo "governed row verification failed: total=$total active=$active deleted=$deleted wrong_scope=$wrong_scope" >&2
  exit 1
fi

if [ "$EXPECTED_KIND" = workspace ]; then
  inventory="$(lake_inventory)"
  inventory_matches="$(jq --arg prefix "$DUCKLAKE_DATA_PREFIX" '[.objects[] | select(.physicalKey | startswith($prefix))] | length' <<<"$inventory")"
  if [ "$inventory_matches" -lt 1 ]; then
    echo "exact-workspace lake inventory omitted active acceptance paths" >&2
    exit 1
  fi
fi

cleanup_active_objects

remaining_active="$(
  run_psql -At -v ON_ERROR_STOP=1 -v prefix="$DUCKLAKE_DATA_PREFIX" <<'SQL'
SELECT count(*)
FROM storage_broker.storage_objects
WHERE tenant_id = :'tenant'
  AND artifact_class = 'ducklake_data'
  AND physical_bucket = :'physical_bucket'
  AND physical_key LIKE :'prefix' || '%'
  AND state = 'active';
SQL
)"
if [ "$remaining_active" -ne 0 ]; then
  echo "acceptance cleanup left $remaining_active active governed rows" >&2
  exit 1
fi

if [ "$EXPECTED_KIND" = workspace ]; then
  inventory="$(lake_inventory)"
  inventory_matches="$(jq --arg prefix "$DUCKLAKE_DATA_PREFIX" '[.objects[] | select(.physicalKey | startswith($prefix))] | length' <<<"$inventory")"
  if [ "$inventory_matches" -ne 0 ]; then
    echo "confirmed acceptance deletions remain in lake inventory: $inventory_matches" >&2
    exit 1
  fi
fi

echo "DuckLake broker probe passed: snapshots=$(jq -r .snapshots <<<"$result") files=$(jq -r .files <<<"$result") scheduled=$(jq -r '.scheduled | length' <<<"$result") cleaned=$(jq -r '.cleaned | length' <<<"$result") governed_rows=$total deleted_rows=$deleted cleaned_active_rows=$active listing=denied multipart=denied"
