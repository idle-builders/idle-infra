---
name: infra-check
description: Health sweep across the whole platform or one part of it — cluster and nodes, PostgreSQL, edge and certificates, identity, storage, and tenant isolation. Read-only. Use for "is everything healthy?" and for a before/after snapshot around a change.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Infra Check

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

A read-only status report. Use it to answer "is everything OK?", to snapshot
state before a change, and to confirm the platform is still whole afterwards.

Load access first with the `cluster-access` skill. In a non-persistent shell,
source and act in one invocation.

## Rules

- Read-only. This skill never mutates. If a check reveals a problem, hand off to
  `infra-debug` for diagnosis and to the owning domain skill for the fix.
- Never print secret values. Confirm a secret **exists**; never read its
  contents.
- Report a check that could not run as unknown, not as healthy. A missing
  signal is evidence, not silence.

## Scope

Default to everything. With an argument, run only that section:
`cluster`, `db`, `edge`, `auth`, `storage`, `tenants`.

## Cluster and nodes

```bash
kubectl get nodes -o wide
kubectl top nodes
kubectl get pods -A -o wide | grep -v Running
kubectl get events -A --sort-by=.lastTimestamp | tail -20
```

Expected node count is `node_pool_min_size` in `terraform/main/variables.tf`.
The cluster is small, so sustained high utilisation is a real finding, not
noise. Any pod not `Running` or `Completed` is worth naming in the report.

## PostgreSQL

```bash
scripts/check-postgresql-health.sh
kubectl top pod -n infra --containers | grep postgresql
kubectl exec -n infra postgresql-primary-0 -c postgresql -- df -h /bitnami/postgresql
```

For an ad-hoc query inside the pod, authenticate from the mounted secret file —
there is no `$POSTGRES_PASSWORD` environment variable, Bitnami exposes
`POSTGRES_PASSWORD_FILE`:

```bash
kubectl exec -n infra postgresql-primary-0 -c postgresql -- sh -lc \
  'PGPASSWORD=$(cat /opt/bitnami/postgresql/secrets/postgres-password) \
   psql -h 127.0.0.1 -U postgres -d postgres -Atqc \
   "SELECT count(*) FROM pg_stat_activity;"'
kubectl get cronjob -n infra pg-backup          # NOT -l app=pg-backup: the CronJob has no labels
kubectl get jobs -n infra -l app=pg-backup      # the Jobs do carry the label
```

`check-postgresql-health.sh` is narrower than its name suggests: it checks
streaming replica count, replication lag, and historical OOM. It does **not**
check connection saturation, disk, locks, or long-running queries. Check
connections against the cap and disk separately — both are more likely to bite.

**On backups, be honest about what you proved.** A `Complete` Job means the pod
exited zero. Container logs are garbage-collected within hours, and nothing in
this repo lists the destination bucket, so a green Job is **not** evidence of a
restorable dump. Report backups as verified only if you actually inspected the
stored object; otherwise report them as unknown.

## Edge and certificates

```bash
kubectl get pods -n infra -l app.kubernetes.io/name=cloudflared
kubectl get pods -n infra -l app.kubernetes.io/name=ingress-nginx
kubectl get ingress -A
kubectl get clusterissuer
kubectl get certificate,certificaterequest -A
```

Then confirm the platform actually serves from outside — the in-cluster view
cannot tell you this. Check **every** published host, not just the identity
domain, since tenant hosts are where an outage actually shows:

```bash
kubectl get ingress -A -o jsonpath='{range .items[*]}{range .spec.rules[*]}{.host}{"\n"}{end}{end}' \
  | sort -u | while read -r h; do
      [ -n "$h" ] || continue
      code="$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "https://$h" 2>/dev/null || echo 'unreachable')"
      printf '%-34s %s\n' "$h" "$code"
    done
```

The nested `range` matters — a single-level one silently drops every host after
the first on a multi-rule Ingress. A host printing `000` or `unreachable` is
usually a DNS failure, not an application fault: check whether the record exists
before treating it as an outage.

**A 5xx here is not automatically a fault.** An unclaimed host — or a path the
Ingress does not route — falls through to the deliberate "nothing deployed"
default backend, which answers **503**, not 404. Before reporting an outage,
check whether an Ingress claims the host and which paths it actually routes; the
app's real health path may answer 200 while `/` answers 503 by design.

A `Certificate` that is not Ready is not automatically a problem for
tunnel-served hosts, where Cloudflare terminates TLS at the edge. Note it, do
not panic about it. Do check for **orphaned** ACME challenges — a challenge with
no parent Certificate retries forever and masks real cert failures:

```bash
kubectl get order,challenge -A
```

## Identity

Read `var.identity_domain` from `terraform/main/variables.tf`, then:

```bash
kubectl get pods -n infra -l app.kubernetes.io/name=zitadel
curl -s "https://<identity-domain>/.well-known/openid-configuration" | head -c 200
```

The discovery document responding is the meaningful check; a Ready pod behind a
broken ingress still fails every login.

## Storage

```bash
kubectl get pods -n infra -l app.kubernetes.io/name=storage-broker
kubectl get deploy -n infra storage-broker \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
kubectl get configmap -n infra storage-broker-policy -o jsonpath='{.data.policy\.json}'
```

Confirm the running image matches the digest pinned in
`terraform/main/variables.tf`. A mismatch means an image was built but never
deployed.

## Tenants

Enumerate tenants from the cluster rather than a hardcoded list — the label is
applied by the tenant module:

```bash
TENANTS=$(kubectl get ns -l platform.infra/tenant=true -o jsonpath='{.items[*].metadata.name}')
for ns in $TENANTS; do
  echo "=== $ns ==="
  kubectl get pods -n "$ns"
  kubectl get netpol,quota,limitrange -n "$ns" --no-headers
  kubectl get secret -n "$ns" --no-headers | awk '{print $1}'
  kubectl get ingress -n "$ns"
done
```

Every tenant should look the same, because they come from one module — compare
them against each other and against
`framework/terraform/modules/tenant/main.tf`. A tenant with fewer network policies or
a missing secret than its siblings is the finding. List secret **names** only.

## Drift

Production matching the repository is part of being healthy — and this is the
check most likely to be silently unavailable, so confirm it actually ran.

```bash
scripts/tf-apply.sh plan     # sources the environment itself
```

Two things make this fail in ways that look like "no drift":

- **The GHCR preflight gates it.** `tf-apply.sh` verifies registry pull access
  before running any subcommand. A stale or revoked GHCR token therefore blocks
  a read-only `plan`. If you see `denied: denied`, drift detection did not run —
  fix the token, do not skip the check.
- **Half the state needs a PostgreSQL port-forward.** Every `postgresql_*`
  resource targets `localhost:5432`. Without a forward you get connection-refused
  errors *and still get a confident `Plan: N to add…` summary* covering only the
  other providers. That summary is **partial**. Read the errors, not just the
  count.

A genuinely clean plan means no drift. Anything else means someone changed
production by hand or a local apply was never merged — report what drifted, in
which direction to reconcile, and whether the DB layer was covered at all.

## Deeper verification

For contractual correctness rather than liveness — isolation boundaries,
acceptance criteria — use the `infra-verify` skill. This skill answers "is it
up"; that one answers "is it correct".

## Report

Present a table, then findings.

| Component | Status | Detail |
|-----------|--------|--------|
| Nodes | | |
| kube-system addons | | DNS replica count, metrics-server, PDBs |
| PostgreSQL | | include connections vs cap, and disk |
| Backups | | unknown unless you inspected a stored dump |
| ingress-nginx | | |
| cloudflared / tunnel | | |
| cert-manager | | include orphaned orders/challenges |
| Zitadel | | |
| storage-broker | | running digest vs pinned digest |
| Tenant: *(one row per tenant)* | | |
| Published hosts | | status per Ingress host |
| DNS | | records resolving vs declared |
| Orphaned resources | | unbound PVCs, dangling solver pods |
| Terraform drift | | say explicitly whether the DB layer was covered |

Mark each finding CRITICAL, WARNING, or INFO, name the skill that owns the fix,
and list explicitly anything that could not be checked.
