#!/usr/bin/env bash
# =============================================================================
# integration/up.sh — stand up the real services the console reads
# =============================================================================
#
# There are no stubs in the console's tests. This brings up the actual software
# at the versions the platform runs, seeds it with real data, and prints the
# environment the Go integration tests read.
#
#   eval "$(integration/up.sh)"      # export the env into your shell
#   go test ./... -count=1
#   integration/down.sh
#
# Kubernetes comes from the current KUBECONFIG context. A namespace named
# INTEG_NAMESPACE is created and torn down by down.sh.
# =============================================================================

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE="docker compose -f ${HERE}/compose.yaml"

NS="${INTEG_NAMESPACE:-console-integration}"
# Zitadel binds its instance to ExternalDomain; calling 127.0.0.1 returns
# "Instance not found". The host name matters, not just the port.
ZITADEL_BASE="http://localhost:8081"
S3_ENDPOINT="http://127.0.0.1:59000"

# A REAL commit in this repository, with a real GitHub Actions run. The tenant
# CI sets image.tag=${github.sha} (docs/tenant-deployment-guide.md), so the
# image tag IS the commit SHA — that is the whole join the deployment timeline
# depends on: Helm revision -> image tag -> commit -> run -> actor, duration.
# Seeding a made-up tag would leave that join permanently untested.
# NOTE: GitHub Actions retains runs for 90 days. When this commit's run ages
# out, RunForCommit returns nothing and the actor/duration assertions fail with
# a symptom that does not name its cause. Refresh to a recent commit on main
# that has a completed run.
SEED_SHA="${INTEG_SEED_SHA:?Set INTEG_SEED_SHA to a real commit with a completed workflow in your fixture repository}"
SEED_IMAGE="${INTEG_SEED_IMAGE:?Set INTEG_SEED_IMAGE to the fixture workload image at that commit}"
if [ -n "${INTEG_GITHUB_TOKEN:-}" ]; then
  : "${INTEG_GITHUB_OWNER:?Set the GitHub fixture repository owner}"
  : "${INTEG_GITHUB_REPO:?Set the GitHub fixture repository name}"
fi
PG_PORT=55432

log() { echo "  $*" >&2; }

# --- cluster safety ----------------------------------------------------------
# The harness creates and deletes a namespace, so it must never point at the
# production cluster. Sourcing scripts/env-from-keychain.sh exports a KUBECONFIG
# for Infomaniak, and inheriting that here would seed production. Pin an
# explicitly local context instead, and record it so test.sh uses the same one.
KUBECONFIG_FILE="${INTEG_KUBECONFIG:-${HOME}/.kube/config}"
KUBECTX="$(KUBECONFIG="${KUBECONFIG_FILE}" kubectl config current-context 2>/dev/null || true)"
case "${KUBECTX}" in
  rancher-desktop|docker-desktop|kind-*|minikube|k3d-*) ;;
  "") echo "no kubernetes context in ${KUBECONFIG_FILE}" >&2; exit 1 ;;
  *)
    echo "refusing to seed context '${KUBECTX}': the harness creates and deletes a namespace" >&2
    echo "and this is not a recognised local cluster. Set INTEG_KUBECONFIG to a local one." >&2
    exit 1 ;;
esac
export KUBECONFIG="${KUBECONFIG_FILE}"

# A context NAME is user-chosen metadata: a context called "rancher-desktop"
# can point anywhere. Assert the API server is actually local before creating
# or deleting anything.
KUBESERVER="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || true)"
case "${KUBESERVER}" in
  https://127.0.0.1:*|https://localhost:*|https://0.0.0.0:*|https://192.168.*|https://kubernetes.docker.internal:*) ;;
  *)
    echo "refusing to seed: context '${KUBECTX}' points at ${KUBESERVER}, which is not a local API server" >&2
    exit 1 ;;
esac

# --- containers --------------------------------------------------------------
log "starting containers"
# 0700, not 0777: this directory receives a Zitadel PAT. World-writable
# would let any local account read it, or replace it and steer what the
# suite authenticates against.
mkdir -p "${HERE}/.pat"; chmod 700 "${HERE}/.pat"
${COMPOSE} up -d >/dev/null 2>&1

log "waiting for zitadel"
for _ in $(seq 1 120); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "${ZITADEL_BASE}/debug/healthz" 2>/dev/null)" = "200" ] && break
  sleep 2
done
[ -s "${HERE}/.pat/console.pat" ] || { echo "zitadel did not write a PAT" >&2; exit 1; }
PAT="$(cat "${HERE}/.pat/console.pat")"


# --- Zitadel: an organization matching the tenant, with a user and an app ----
#
# The console looks up a tenant's identity by org NAME == namespace. The
# bootstrap org is called "beacon", so without this the auth panel would
# only ever be seen in its degraded state.
#
# Resolve-or-create: Zitadel refuses to delete an org (405), so re-running
# up.sh without down.sh must not create a second app in the same org. An org
# that already exists was already seeded by a previous run.
log "seeding zitadel organization"

zorg_id() {
  curl -s -X POST "${ZITADEL_BASE}/admin/v1/orgs/_search" \
    -H "Authorization: Bearer ${PAT}" -H 'Content-Type: application/json' -d '{}' \
  | ORG_NAME="$1" python3 -c 'import json,os,sys
d = json.load(sys.stdin)
want = os.environ["ORG_NAME"]
print(next((o["id"] for o in d.get("result", []) if o["name"] == want), ""))'
}

# Zitadel's org search reads a projection, which is eventually consistent: a
# lookup issued immediately after a create returns nothing. Poll rather than
# assume.
zorg_await() {
  # 90s: a cold Zitadel builds its projections after first start, and the org
  # search reads a projection. 30s was not enough on a fresh container.
  for _ in $(seq 1 90); do
    id="$(zorg_id "$1")"
    if [ -n "${id}" ]; then echo "${id}"; return 0; fi
    sleep 1
  done
  return 1
}

# Take the id from the CREATE response, not from a follow-up search: the org
# search reads a projection that is eventually consistent, and on a cold
# instance it can lag long enough to fail the run outright. Search is only the
# fallback for an org that already exists from a previous run.
# /debug/healthz goes green before the management API accepts writes, so the
# create is retried rather than assumed. An id in the response is the only
# reliable signal that Zitadel is actually ready.
zorg_create() {
  curl -s -X POST "${ZITADEL_BASE}/management/v1/orgs" \
    -H "Authorization: Bearer ${PAT}" -H 'Content-Type: application/json' \
    -d "{\"name\":\"$1\"}" \
  | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("id",""))
except Exception: print("")'
}

ZORG=""
for _ in $(seq 1 60); do
  ZORG="$(zorg_create "${NS}")"
  [ -n "${ZORG}" ] && break
  # Already exists from an earlier run? Then the search is authoritative.
  ZORG="$(zorg_id "${NS}")"
  [ -n "${ZORG}" ] && break
  sleep 2
done
[ -n "${ZORG}" ] || { echo "zitadel org ${NS} could not be created or found" >&2; exit 1; }

# Seed the user and app if they are missing, rather than assuming an existing
# org was fully seeded. Zitadel refuses to delete an org (405), so an
# interrupted run would otherwise leave a permanently half-seeded instance.
zapp_count() {
  curl -s -X POST "${ZITADEL_BASE}/management/v1/projects/_search" \
    -H "Authorization: Bearer ${PAT}" -H "x-zitadel-orgid: ${ZORG}" \
    -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("result", [])))'
}

if [ "$(zapp_count)" = "0" ]; then
  curl -s -X POST "${ZITADEL_BASE}/management/v1/users/human/_import" \
    -H "Authorization: Bearer ${PAT}" -H "x-zitadel-orgid: ${ZORG}" \
    -H 'Content-Type: application/json' \
    -d '{"userName":"seed@example.com","profile":{"firstName":"Seed","lastName":"User"},"email":{"email":"seed@example.com","isEmailVerified":true},"password":"SeedPassword1!"}' >/dev/null

  ZPROJ="$(curl -s -X POST "${ZITADEL_BASE}/management/v1/projects" \
    -H "Authorization: Bearer ${PAT}" -H "x-zitadel-orgid: ${ZORG}" \
    -H 'Content-Type: application/json' -d "{\"name\":\"${NS}-app\"}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("id",""))')"

  # A PKCE public client, the same shape the console itself signs in with.
  curl -s -X POST "${ZITADEL_BASE}/management/v1/projects/${ZPROJ}/apps/oidc" \
    -H "Authorization: Bearer ${PAT}" -H "x-zitadel-orgid: ${ZORG}" \
    -H 'Content-Type: application/json' \
    -d '{"name":"web","redirectUris":["https://beta.example.com/callback"],"responseTypes":["OIDC_RESPONSE_TYPE_CODE"],"grantTypes":["OIDC_GRANT_TYPE_AUTHORIZATION_CODE"],"appType":"OIDC_APP_TYPE_WEB","authMethodType":"OIDC_AUTH_METHOD_TYPE_NONE"}' >/dev/null
fi

# A real, valid credential that is NOT a platform operator: a machine user in
# the tenant org with no memberships at all. Zitadel here is one shared
# instance serving every tenant's end users, so "authenticates but must not be
# admitted" is the case that actually protects tenant isolation — and it can
# only be proven with a genuine token.
PLAIN_ID="$(curl -s -X POST "${ZITADEL_BASE}/management/v1/users/_search" \
  -H "Authorization: Bearer ${PAT}" -H "x-zitadel-orgid: ${ZORG}" \
  -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import json,sys
print(next((u["id"] for u in json.load(sys.stdin).get("result", []) if u.get("userName") == "plainuser"), ""))')"
if [ -z "${PLAIN_ID}" ]; then
  PLAIN_ID="$(curl -s -X POST "${ZITADEL_BASE}/management/v1/users/machine" \
    -H "Authorization: Bearer ${PAT}" -H "x-zitadel-orgid: ${ZORG}" \
    -H 'Content-Type: application/json' \
    -d '{"userName":"plainuser","name":"plain tenant user","description":"no memberships"}' \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("userId",""))')"
fi
PLAIN_TOKEN="$(curl -s -X POST "${ZITADEL_BASE}/management/v1/users/${PLAIN_ID}/pats" \
  -H "Authorization: Bearer ${PAT}" -H "x-zitadel-orgid: ${ZORG}" \
  -H 'Content-Type: application/json' -d '{"expirationDate":"2100-01-01T00:00:00Z"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token",""))')"

# An org with NO users. proto3 JSON omits zero values, so a successful search
# against an empty org returns no totalResult at all — which once made the
# console report "unknown" instead of 0. This keeps that fixed.
zorg_create "${NS}-empty" >/dev/null

# --- PostgreSQL: a schema-per-app layout, like the tenant module produces ----
log "seeding postgresql"
PGC="$(${COMPOSE} ps -q postgres)"
docker exec -i "${PGC}" psql -U postgres -q -v ON_ERROR_STOP=1 >/dev/null <<SQL
DROP SCHEMA IF EXISTS "${NS}" CASCADE;
CREATE SCHEMA "${NS}";
CREATE TABLE "${NS}".players (
  id text PRIMARY KEY, display_name text NOT NULL,
  email text, api_key text, tier text, created_at timestamptz DEFAULT now());
INSERT INTO "${NS}".players (id,display_name,email,api_key,tier) VALUES
  ('a1','Nord Vagrant','nord@example.com','sk-live-1','pro'),
  ('a2','Quiet Harbour','quiet@example.com','sk-live-2','free'),
  ('a3','Slate Runner','slate@example.com','sk-live-3','pro');
CREATE TABLE "${NS}".sessions (id bigserial PRIMARY KEY, player_id text);
INSERT INTO "${NS}".sessions (player_id) SELECT 'a1' FROM generate_series(1,120);
-- Keyed on a column whose name matches a masking token. Masking the primary
-- key nulls the pagination cursor, so the page came back truncated with no
-- nextCursor and the browser dead-ended with no error. Content-addressed
-- tables really are keyed on "hash".
CREATE TABLE "${NS}".artifacts (hash text PRIMARY KEY, body text);
INSERT INTO "${NS}".artifacts SELECT 'h' || g, 'payload' FROM generate_series(1,120) g;
-- "reservation_id" contains "vat"; unanchored masking matched it.
CREATE TABLE "${NS}".reservations (reservation_id text PRIMARY KEY, note text);
INSERT INTO "${NS}".reservations SELECT 'r' || g, 'note' FROM generate_series(1,5) g;
-- A composite key: genuinely unpageable, so the 409 path is exercised for real.
CREATE TABLE "${NS}".memberships (user_id text, group_id text, PRIMARY KEY (user_id, group_id));
INSERT INTO "${NS}".memberships VALUES ('a1','g1'),('a2','g1');
-- platform_reader, reproduced exactly as terraform/main/main.tf declares it.
-- The suite used to connect as superuser, which hid two real defects: masking
-- behaves differently under a restricted role, and pg_stat_activity hides
-- state/query for sessions this role does not own.
-- A tenant role that owns the schema, as modules/tenant creates it: schema
-- name == role name == namespace. Default privileges are owner-scoped, so
-- without this the FOR ROLE clause below has nothing to attach to.
-- \$\$ is escaped: this heredoc interpolates, and bare $$ would expand to the
-- shell's PID before psql ever saw it.
DO \$\$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${NS}') THEN
    EXECUTE format('DROP OWNED BY %I CASCADE', '${NS}');
    EXECUTE format('DROP ROLE %I', '${NS}');
  END IF;
END \$\$;
CREATE ROLE "${NS}" LOGIN PASSWORD 'tenant-integration';
ALTER SCHEMA "${NS}" OWNER TO "${NS}";

DROP ROLE IF EXISTS platform_reader;
CREATE ROLE platform_reader LOGIN PASSWORD 'reader-integration';
ALTER ROLE platform_reader SET statement_timeout = 5000;
GRANT USAGE ON SCHEMA "${NS}" TO platform_reader;
-- The one-time bootstrap grant the runbook documents. Terraform deliberately
-- does NOT reconcile this, or it would re-assert itself over any column the
-- owner withheld.
GRANT SELECT ON ALL TABLES IN SCHEMA "${NS}" TO platform_reader;

-- FOR ROLE "${NS}", matching production: Terraform declares
-- postgresql_default_privileges with owner = each.value, which renders
-- ALTER DEFAULT PRIVILEGES FOR ROLE <tenant>. Without FOR ROLE this grants for
-- the CURRENT role instead, so the harness was testing a shape production
-- never runs — and a table created by the tenant role would not have been
-- covered.
ALTER DEFAULT PRIVILEGES FOR ROLE "${NS}" IN SCHEMA "${NS}"
  GRANT SELECT ON TABLES TO platform_reader;

-- A table shaped like a real tenant's: the secret is in a free-form column
-- whose NAME says nothing, which is why name-based masking could not work.
CREATE TABLE "${NS}".accounts (
  id bigserial PRIMARY KEY,
  metadata jsonb,
  notes text,
  login_count integer,
  created_at timestamptz DEFAULT now());
INSERT INTO "${NS}".accounts (metadata, notes, login_count) VALUES
  ('{"api_token":"sk_live_DEADBEEF"}'::jsonb, 'SSN 123-45-6789', 4);
-- The owner withholds columns the way PostgreSQL expresses it. Column grants
-- are ADDITIVE, so the table-level grant (which ALTER DEFAULT PRIVILEGES hands
-- to every new table) must be revoked first — otherwise the column list is
-- redundant and everything stays readable. This is the exact workflow the
-- runbook documents.
REVOKE SELECT ON "${NS}".accounts FROM platform_reader;
GRANT SELECT (id, login_count, created_at) ON "${NS}".accounts TO platform_reader;
ANALYZE "${NS}".accounts;

ANALYZE "${NS}".players; ANALYZE "${NS}".sessions; ANALYZE "${NS}".memberships;
ANALYZE "${NS}".artifacts; ANALYZE "${NS}".reservations;
SQL



# --- MinIO: a real bucket with real objects ---------------------------------
log "seeding object storage"
MINIOC="$(${COMPOSE} ps -q minio)"
for _ in $(seq 1 60); do
  docker exec "${MINIOC}" mc alias set local http://127.0.0.1:9000 integration integration-secret >/dev/null 2>&1 && break
  sleep 1
done
BUCKET="${NS}-files"
docker exec "${MINIOC}" mc rb --force "local/${BUCKET}" >/dev/null 2>&1 || true
docker exec "${MINIOC}" mc mb "local/${BUCKET}" >/dev/null 2>&1
# Three objects of known size, so the console's totals can be checked exactly.
docker exec "${MINIOC}" sh -c '
  head -c 1024 /dev/zero > /tmp/a; head -c 2048 /dev/zero > /tmp/b; head -c 4096 /dev/zero > /tmp/c
' >/dev/null 2>&1
for f in a b c; do
  docker exec "${MINIOC}" mc cp "/tmp/${f}" "local/${BUCKET}/${f}.bin" >/dev/null 2>&1
done

# --- Kubernetes: a real namespace on the real cluster ------------------------
log "seeding kubernetes namespace ${NS}"
kubectl create namespace "${NS}" --dry-run=client -o yaml 2>/dev/null | kubectl apply -f - >/dev/null
kubectl label namespace "${NS}" platform.infra/tenant=true --overwrite >/dev/null
kubectl apply -n "${NS}" -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, labels: {app: app}}
spec:
  replicas: 1
  selector: {matchLabels: {app: app}}
  template:
    metadata: {labels: {app: app}}
    spec:
      containers:
        - name: app
          image: ${SEED_IMAGE}
          resources: {requests: {cpu: 50m, memory: 64Mi}}
---
apiVersion: v1
kind: ResourceQuota
metadata: {name: tenant}
spec: {hard: {requests.cpu: "2", requests.memory: 4Gi, pods: "10"}}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: app}
spec:
  ingressClassName: nginx
  rules:
    - host: beta.example.com
      http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: app, port: {number: 80}}}}]}
YAML

# The storage broker's policy, in the infra namespace where Terraform puts it.
# This is what maps a namespace to its bucket; the console must never derive
# that name, since the platform's buckets are "<app>-files".
kubectl create namespace infra --dry-run=client -o yaml 2>/dev/null | kubectl apply -f - >/dev/null
POLICY=$(cat <<JSON
{"defaultTTLSeconds":{"get":300,"put":900,"delete":60},
 "namespaces":{"${NS}":{"bucket":"${BUCKET}","ops":["get","put","delete"],
   "requireUserToken":true,"prefixes":["u/\${sub}/"]}}}
JSON
)
kubectl create configmap storage-broker-policy -n infra \
  --from-literal=policy.json="${POLICY}" --dry-run=client -o yaml 2>/dev/null | kubectl apply -f - >/dev/null

# A Helm release ConfigMap, encoded exactly as Helm stores it.
RELEASE="$(SEED_SHA="${SEED_SHA}" python3 - <<'PY'
import base64, gzip, json, os
doc = {"name":"app","version":41,
  "info":{"status":"deployed","last_deployed":"2026-08-17T09:12:00Z"},
  "chart":{"metadata":{"version":"0.4.0","appVersion":"1.3.0"}},
  "config":{"image":{"tag":os.environ["SEED_SHA"]}}}
print(base64.b64encode(gzip.compress(json.dumps(doc).encode())).decode())
PY
)"
kubectl create configmap sh.helm.release.v1.app.v41 -n "${NS}" \
  --from-literal=release="${RELEASE}" --dry-run=client -o yaml 2>/dev/null | kubectl apply -f - >/dev/null
kubectl label configmap sh.helm.release.v1.app.v41 -n "${NS}" \
  owner=helm name=app version=41 status=deployed --overwrite >/dev/null

# --- environment -------------------------------------------------------------
cat <<ENV
export INTEG_NAMESPACE='${NS}'
export INTEG_KUBE='1'
export INTEG_KUBECONFIG='${KUBECONFIG_FILE}'
export INTEG_KUBECONTEXT='${KUBECTX}'
export INTEG_POSTGRES_DSN='postgres://platform_reader:reader-integration@127.0.0.1:${PG_PORT}/postgres?sslmode=disable'
export INTEG_POSTGRES_SUPERUSER_DSN='postgres://postgres:integration@127.0.0.1:${PG_PORT}/postgres?sslmode=disable'
export INTEG_POSTGRES_SCHEMA='${NS}'
export INTEG_ZITADEL_URL='${ZITADEL_BASE}'
export INTEG_ZITADEL_TOKEN='${PAT}'
export INTEG_ZITADEL_ORG='${NS}'
export INTEG_ZITADEL_ORG_ID='${ZORG}'
export INTEG_ZITADEL_EMPTY_ORG='${NS}-empty'
export INTEG_ZITADEL_PLAIN_TOKEN='${PLAIN_TOKEN}'
export INTEG_S3_ENDPOINT='${S3_ENDPOINT}'
export INTEG_S3_ACCESS_KEY='integration'
export INTEG_S3_SECRET_KEY='integration-secret'
export INTEG_S3_BUCKET='${BUCKET}'
# 1024 + 2048 + 4096
export INTEG_S3_BYTES='7168'
export INTEG_S3_OBJECTS='3'
export INTEG_SEED_SHA='${SEED_SHA}'
export INTEG_SEED_IMAGE='${SEED_IMAGE}'
ENV
log "ready"
