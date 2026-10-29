---
name: database
description: Work with the shared self-hosted PostgreSQL — query it, check health, diagnose slowness or connection failures, change schemas/roles/grants, run migrations, rotate passwords, repair replication, and verify backups and restores.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Database

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

One self-hosted PostgreSQL instance (Bitnami chart, replication architecture)
serves every tenant. Isolation is schema-per-tenant inside the shared `postgres`
database, plus a separate `zitadel` database for the identity provider.

Load access first with the `cluster-access` skill. In a non-persistent shell,
source and act in one invocation.

## Rules

- Never print a password, connection string, or secret value. Feed passwords via
  mounted files or stdin, never as a command-line argument — the process list is
  readable.
- Never create or alter roles, schemas, or grants by hand. They are Terraform
  resources; a manual change is drift that the next apply reverts. The one
  documented exception is a password rotation, which requires an in-instance
  `ALTER USER` *before* the apply.
- Never run `--include-query-text` observations outside a short, deliberate
  diagnostic session. SQL can carry credentials and personal data.
- Diagnosis is read-only by default. Ask before mutating anything in a live
  database.

## Where the truth lives

Read these rather than trusting a value written here.

| Question | Source |
|----------|--------|
| Connection cap, timeouts, WAL and memory tuning, `pg_hba` | `helm/values/postgresql.yaml` |
| Chart version, architecture, backup schedule and retention | `terraform/main/main.tf` |
| Tenant roles, schemas, grants, connection string shape | `framework/terraform/modules/tenant/main.tf` |
| Backup and Zitadel roles and their grants | `terraform/main/main.tf` |
| Health thresholds (replication lag, expected replicas) | `scripts/check-postgresql-health.sh` — the `PG_*` defaults at the top |
| Live pod and service names | `kubectl get pods,svc -n infra` |

```bash
grep -nE 'max_connections|statement_timeout|idle_in_transaction' helm/values/postgresql.yaml
kubectl get pods,svc -n infra -l app.kubernetes.io/name=postgresql
```

## Shape

Replication architecture: one primary, one read replica. The shared `postgres`
database holds one schema and one owning role per tenant; the identity provider
gets its own database with `PUBLIC` connect revoked. TLS is required for all
remote connections and tenants use `sslmode=verify-ca`.

Tenants connect with `search_path` scoping, not a separate database — the exact
connection string is built in the tenant module's `database_url` secret.

## Get a session

```bash
source scripts/env-from-keychain.sh
kubectl port-forward -n infra svc/postgresql-primary 5432:5432
```

Then connect on `localhost:5432`. Loopback inside the pod is non-TLS by design —
the port-forward is the encrypted tunnel.

Read-only queries can also run in-pod without a port-forward:

```bash
kubectl exec -n infra postgresql-primary-0 -c postgresql -- sh -lc \
  'PGPASSWORD=$(cat /opt/bitnami/postgresql/secrets/postgres-password) \
   psql -h 127.0.0.1 -U postgres -d postgres -Atqc "<query>"'
```

## Health

```bash
scripts/check-postgresql-health.sh
kubectl top pod -n infra --containers
```

The script encodes the thresholds and exits non-zero when they are breached —
read its `PG_*` defaults if you need the current numbers. Broadly: both pods
Ready, the primary's last termination was not `OOMKilled`, the expected number of
`streaming` replicas present, replay lag within budget, and the replica reporting
`pg_is_in_recovery() = true`.

## Slowness and pressure

```bash
scripts/postgresql-observe.sh --limit 25 --min-active-ms 250
scripts/postgresql-observe.sh --watch 5 > observations.ndjson
```

Emits JSON: connections by role and application name, active queries with
duration and wait state, and cumulative `pg_stat_statements` cost with
`risk_flags`. Query text is omitted by default.

Check the connection cap before blaming a query. It is deliberately low
(`max_connections` in `helm/values/postgresql.yaml`), so a single tenant with an
unbounded pool exhausts it for everyone:

```sql
SELECT count(*), state FROM pg_stat_activity GROUP BY state;
SHOW max_connections;
```

## "My app can't connect"

Five causes, in the order worth checking:

1. **Network policy.** The tenant's `allow-postgres` egress is pod-scoped to
   `app.kubernetes.io/name=postgresql`. Use the `network` skill.
2. **Missing CA.** `sslmode=verify-ca` requires the `pg-ca-cert` secret mounted
   at `/etc/ssl/pg/` and the driver configured to load it. This is the most
   common one.
3. **`search_path`.** Unqualified table names resolve through the connection
   string's `options=-csearch_path%3D<tenant>`. A driver that rewrites or drops
   connection options lands the app in an empty schema.
4. **Connection exhaustion.** See above.
5. **Wrong role.** The tenant role can read only its own schema — by design.

## Migrations

Port-forward, then run with the tenant's `DATABASE_URL`. Three constraints that
break naive migration tooling:

- The tenant role **cannot** `CREATE SCHEMA`, and `public` is revoked. The
  schema already exists; migrations must target it, not create it.
- A one-shot migration Job must close its connection pool and exit, or the Helm
  hook never completes and the deploy hangs.
- Pods must satisfy the namespace's `restricted` PodSecurity standard.

See `docs/tenant-deployment-guide.md` for the full pattern.

## Change a schema, role, or grant

Terraform only. Tenant roles and schemas come from
`framework/terraform/modules/tenant/main.tf`; backup and Zitadel roles are in
`terraform/main/main.tf`. Edit, then hand off to the `infra-apply` skill.

`postgresql_*` resources need a live port-forward during apply — both locally
and in CI, where the workflow starts one automatically.

## Rotate a password

**A plain `tofu apply` does nothing here, and fails silently.** The tenant module
carries `ignore_changes = [password]` on `postgresql_role.tenant` and
`ignore_changes = [data]` on `kubernetes_secret.database_url`. Terraform will
neither ALTER the live role nor rewrite the secret. You get a green apply, a
clean plan, and an unrotated credential.

> The comment above the rotation section in `terraform/main/main.tf` used to
> claim a plain apply was sufficient. It was wrong for the same reason. If you
> find guidance anywhere saying "just run apply", it predates this note.

**Step 0 — inventory the consumers.** This decides whether you need an outage
window or the rotation is a no-op. **If the count is zero — no pods and no
sessions — there is no outage window**, steps 5 and 6 carry no risk, and step 6
is a documented no-op you should report as such rather than claim as passed:

```bash
kubectl get deploy,sts,cronjob,pods -n <tenant>
kubectl exec -n infra postgresql-primary-0 -c postgresql -- sh -lc \
  'PGPASSWORD=$(cat /opt/bitnami/postgresql/secrets/postgres-password) \
   psql -h 127.0.0.1 -U postgres -d postgres -Atqc \
   "SELECT count(*) FROM pg_stat_activity WHERE usename = '"'"'<tenant>'"'"';"'
```

**Step 1 — change it in PostgreSQL first.** The exposed credential must stop
working before anything else happens.

Generate the value first (`openssl rand -base64 24`), and keep it out of shell
history and the process list.

Use `\password <role>` in an interactive superuser psql session — **not**
`ALTER USER ... PASSWORD`. `\password` is interactive-only; for a scripted or
agent-driven rotation, suppress logging in the same session instead:

```bash
kubectl exec -n infra postgresql-primary-0 -c postgresql -- sh -lc '
  printf "SET log_statement = '"'"'none'"'"';\nALTER ROLE <role> PASSWORD '"'"'%s'"'"';\n" "$NEW_PW" |
    PGPASSWORD=$(cat /opt/bitnami/postgresql/secrets/postgres-password) \
    psql -h 127.0.0.1 -U postgres -d postgres -X -v ON_ERROR_STOP=1
'
```

Note `\password` still emits the SCRAM *verifier* to the log — not cleartext,
but offline-crackable. Suppressing `log_statement` removes even that. `helm/values/postgresql.yaml` sets
`log_statement = 'ddl'`, and PostgreSQL does not redact DDL: a literal
`ALTER ROLE` writes the new password in cleartext into the
`postgresql-primary-0` container log. That re-leaks the very credential you are
rotating. `\password` computes the SCRAM verifier client-side, so only the
verifier reaches the server. If you must script it, `SET log_statement = 'none'`
for the session first.

**Step 2 — update the Keychain entry**, prompting rather than passing the value
on the command line:

```bash
security add-generic-password -U -s "infra/TF_VAR_<tenant>_db_password" -a "$USER" -w
```

**Step 3 — update the GitHub Actions secret**, piping the value rather than
passing it as an argument:

```bash
gh secret set TF_VAR_<TENANT>_DB_PASSWORD --repo <owner>/infra   # value on stdin
```

The exact name is the one passed in `.github/workflows/terraform-apply.yml`;
note the case differs from the Keychain entry. CI is a second writer of this
secret — left stale, it silently reinstates the old password later.

**Step 4 — re-escrow** with `scripts/keychain-import.sh --escrow`, or disaster
recovery restores the old password.

**Step 5 — force the Kubernetes secret to be rewritten.** This is the step that
actually propagates the change, and the one every earlier version of this
procedure omitted:

```bash
kubectl port-forward -n infra svc/postgresql-primary 5432:5432
scripts/tf-apply.sh plan  -replace='module.tenant_<tenant>.kubernetes_secret.database_url'
scripts/tf-apply.sh apply -replace='module.tenant_<tenant>.kubernetes_secret.database_url'
```

`ignore_changes` suppresses diffs, not creation values, so the recreated secret
is written from the current variable — which is why step 2 has to come first.
The plan must show **exactly one resource replaced**. If it proposes touching
`postgresql_role.tenant`, `postgresql_schema.tenant`, or the namespace, stop —
never `-replace` the role, it owns the tenant's schema.

**Step 6 — restart the consumers.** `DATABASE_URL` is injected as a
`secretKeyRef` environment variable, which is snapshotted at pod start and never
refreshed by kubelet. A rollout restart is mandatory, not housekeeping.

**There is no CI path for this.** A rotation changes no tracked file, and the
apply workflow triggers only on pushes to specific paths with no
`workflow_dispatch`. Local apply is the only route, and `infra-apply`'s "the
code must still land on main" rule does not apply — there is nothing to land.

**If it fails between steps 1 and 5**, PostgreSQL has the new password and the
secret has the old one: a hard outage. Recover by completing the `-replace`, or
by setting the role's password back to the previous value.

**Proving it worked** — a clean plan proves nothing here, because
`ignore_changes` makes the plan clean either way. Neither does a green apply,
nor a fresh `creationTimestamp` on the secret (a `-replace` run before the
Keychain update recreates it with the *old* value and looks identical), nor
checking with the password from your own shell (that proves PostgreSQL changed,
not that the secret agrees).

Contract tests C2 and C3 **are** meaningful here: they authenticate with the
credential parsed from the `database-url` secret before asserting anything, and
fail loudly if it cannot connect. Real evidence:

1. The old password is rejected — expect `FATAL ... 28P01`.
2. The new password, **read out of the Kubernetes secret** rather than your
   shell variable, connects and returns the right `current_user` and
   `current_schema`. Sourcing it from the secret is what proves the secret and
   the database agree.
3. `kubectl logs -n infra postgresql-primary-0 -c postgresql --since=15m | grep -c 'ALTER ROLE'`
   returns 0 — no credential landed in the log.
4. The consuming pods reconnect with no `28P01` in their logs. If the tenant has
   no workload deployed, say so rather than reporting this as passed.

## Replication and recovery

`docs/runbooks/postgresql-recovery.md` has the exact commands. Summary:

- **Credential repair** — verify the mounted replication secret is accepted,
  then reset only `repl_user` from that same secret and delete the replica pod.
- **Stale replica rebuild** — when the replica needs a removed WAL segment,
  scale the read StatefulSet to zero, delete *only* `data-postgresql-read-0`,
  scale back. Never touch the primary PVC.
- **OOM triage** — read the previous container's termination reason, correlate
  timestamps with CronJobs, suspend only the confirmed trigger, and do not
  resume until the query has a bounded timeout.

## Backups

A CronJob with `Forbid` concurrency dumps each database with its own
least-privilege role, encrypts before upload, ships to Swiss Backup, and prunes
old dumps. Schedule, retention, and the exact databases are in the
`module.platform.kubernetes_cron_job_v1.pg_backup` resource in `terraform/main/main.tf`:

```bash
kubectl get cronjob -n infra pg-backup -o jsonpath='{.spec.schedule}{"\n"}'
```

```bash
kubectl get cronjob -n infra pg-backup
kubectl get jobs -n infra -l app=pg-backup
kubectl logs -n infra job/<most-recent> --tail=50
```

The read replica is **not** a backup — it faithfully replicates accidental
deletes. Validate a real restore into a throwaway instance at least monthly and
record the actual RTO and RPO.

## Evidence

A database change is done when the live database shows it:

```bash
# schemas and roles
psql -Atqc '\dn'
psql -Atqc '\du'
# a specific grant
psql -Atqc "SELECT has_schema_privilege('<role>','<schema>','USAGE');"
# isolation still holds
tests/contracts.sh   # C2, C3, C7, C8
```

For a password rotation, evidence is the *application* reconnecting — a running
pod with no authentication errors in its recent logs, not just a changed secret.

Ship the change with `infra-apply`, then run `infra-verify`.
