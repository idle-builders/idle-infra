---
name: tenant
description: Manage the platform side of an application — onboard a new tenant, add an application secret, adjust quotas or allowed ingress domains, and decommission a tenant. Covers namespace, schema, role, policies, and secrets.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Tenant

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

A tenant is an application consuming the platform, represented as a Kubernetes
namespace plus a PostgreSQL schema plus the isolation scaffolding around them.
It is not a user and not a deployment — the tenant exists before any workload
does.

This skill owns the **platform side**. Deploying the app's own workload is the
`app-deploy` skill, run from the app's repository.

Which tenants exist today:

```bash
grep -n 'module "tenant_' terraform/main/main.tf
```

## Rules

- Never create a namespace, schema, or role by hand. The tenant module owns all
  of it.
- Never give a tenant a separate database. The model is schema-per-tenant inside
  the shared `postgres` database, scoped by `search_path`.
- A change to `modules/tenant/main.tf` applies to **every** tenant. If only one
  needs it, pass a different value from that tenant's module block instead.
- Never print a tenant password or the contents of a generated secret.

## What a tenant gets

`framework/terraform/modules/tenant/main.tf` is the definition — read it for the
current specifics. The module is organised in labelled sections and provisions:

- a namespace enforcing the `restricted` Pod Security Standard, labelled as a
  tenant and annotated with the hostnames it may claim
- a default ServiceAccount with token automount disabled
- a default-deny network policy plus a pod-scoped allowlist per dependency
- a ResourceQuota and a LimitRange
- a `deployer` ServiceAccount, Role, and binding — deliberately **without**
  secrets access
- a PostgreSQL login role and the schema it owns, with `public` revoked
- the secrets an app consumes: database connection, database CA, registry pull

Check what a live tenant actually has:

```bash
kubectl get netpol,quota,limitrange,sa,role -n <tenant>
kubectl get secret -n <tenant>          # names only, never values
```

## Onboard a new tenant

Order matters — the password must exist everywhere before the apply.

1. **Add the variable** in `terraform/main/variables.tf` — a `sensitive` string
   for the tenant's database password. Copy the shape of an existing
   `*_db_password` variable in that file.

2. **Store the value** in Keychain as `infra/TF_VAR_<name>_db_password`, and add
   the matching GitHub Actions secret `TF_VAR_<NAME>_DB_PASSWORD`. Generate a
   strong value; never reuse another tenant's.

3. **Add the secret to the apply workflow** — `.github/workflows/terraform-apply.yml`
   passes each `TF_VAR_*` explicitly. A variable that is not listed there is
   unset in CI and the apply fails.

4. **Add the module block** in `terraform/main/main.tf`. Copy an existing
   `module "tenant_*"` block verbatim and change the name, the domain, and the
   password variable — the remaining arguments (database host, database name,
   registry credentials, CA, dependencies) are identical for every tenant and
   copying keeps them correct as they change.

   `allowed_ingress_domains` is enforced fail-closed by a
   ValidatingAdmissionPolicy. Get it right now or the app's first Ingress is
   rejected with a message that looks like an app bug.

   Check `framework/terraform/modules/tenant/variables.tf` for arguments you may want
   to override, such as the CPU and memory bounds.

5. **Add the backup grants.** They are hand-written per tenant in `main.tf` —
   `grep -n 'pg_backup_usage_\|pg_backup_select_' terraform/main/main.tf` — and
   the tenant module does **not** create them. Skip this and the nightly backup
   silently omits the new tenant's tables. You discover it during a restore.

6. **Update the local secret plumbing.** `scripts/env-from-keychain.sh` reads
   each `TF_VAR_*` explicitly and **aborts on a missing entry**, so committing a
   new variable there before the Keychain entry exists breaks credential loading
   for every operator, not just for this tenant. Add the variable to
   `scripts/env-from-keychain.sh` and `scripts/keychain-import.sh`, and create
   the Keychain entry in the same sitting.

7. **Add the tenant to the test suites.** `tests/contracts.sh` and
   `tests/acceptance.sh` derive their tenant list from the repository — confirm
   the new tenant appears, because a tenant missing from that list gets a green
   isolation run that tested nothing.

8. **Ship it** with the `infra-apply` skill. The apply needs a live PostgreSQL
   port-forward for the role and schema.

9. **Verify** — see Evidence below. Then, if the app needs them:
   DNS with the `cloudflare` skill, a bucket with the `storage` skill, login
   with the `zitadel` skill, and CI credentials with `app-deploy`.

Also worth updating, though nothing breaks without them: the outputs in
`terraform/main/outputs.tf`, `.env.example`, and
`terraform/main/terraform.tfvars.example`.

## Add an application secret

Platform secrets are Terraform-managed so the namespace deployer can reference
them but cannot read or mutate them. Four places, all required:

1. A `sensitive` variable in `variables.tf`
2. The value in Keychain as `infra/TF_VAR_<name>`
3. A GitHub Actions secret, **and** a line passing it in
   `terraform-apply.yml`
4. A `kubernetes_secret` resource in `main.tf` targeting the tenant namespace

Miss step 3 and the apply fails in CI while working locally. Copy an existing
app-credential secret in `main.tf` as the pattern —
`grep -n 'resource "kubernetes_secret"' terraform/main/main.tf`.

## Adjust quotas or allowed domains

Override the relevant argument from that tenant's module block. See
`framework/terraform/modules/tenant/variables.tf` for what is overridable and what
the defaults are. Changing the module defaults changes every tenant.

## Decommission

1. Remove the module block from `main.tf`.
2. Remove the variable, the workflow line, and any tenant-specific secrets.
3. Apply. Terraform destroys the namespace, schema, role, and secrets.
4. Remove the tenant's DNS records and its entry in `tenant_policies` on the
   storage broker if it had one.

**Read the plan first.** Destroying a schema destroys its data. Confirm with the
operator and take a backup before applying, and remove the Keychain entry only
after the apply succeeds.

## Evidence

A tenant is real when the live platform shows every layer:

```bash
# namespace with the right labels and the hosts annotation
kubectl get ns <name> -o jsonpath='{.metadata.labels}{"\n"}{.metadata.annotations}{"\n"}'

# isolation scaffolding
kubectl get netpol,quota,limitrange -n <name>
kubectl get sa,role,rolebinding -n <name>

# secrets exist — do not print their values
kubectl get secret -n <name>

# schema and role exist and are owned correctly
psql -Atqc "\dn"
psql -Atqc "SELECT rolname FROM pg_roles WHERE rolname = '<name>';"

# the contracts still hold
tests/contracts.sh    # the per-tenant contracts in particular
```

For a decommission, the evidence is absence: namespace gone, schema gone, role
gone, and the contracts still passing for the remaining tenants.

Ship with `infra-apply`, then run `infra-verify`.
