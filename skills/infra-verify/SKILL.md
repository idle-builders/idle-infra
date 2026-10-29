---
name: infra-verify
description: Prove an infrastructure change is correct and the platform's guarantees still hold. Picks the right test tier — structural validation, isolation contracts, acceptance criteria, or the storage-broker matrix — and interprets failures against the contract they encode.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Infra Verify

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

Run the proof that matches the change. Use after `infra-apply` reaches
production, before `ship`, and any time someone asks whether the platform is
still correct.

## Rules

- Never skip the contract tests for a change that touches isolation —
  namespaces, network policies, PostgreSQL roles, schemas, or grants.
- Never report a pass you did not run. If a tier could not run because
  credentials or tooling were missing, say which tier and why.
- The tests are idempotent and clean up after themselves. They are safe to run
  against production, which is the only place they are meaningful.

## Pick the tier

| Change | Run |
|--------|-----|
| Any Terraform or Helm edit | `tests/validate.sh` |
| Namespace, network policy, PG role, schema, grant, quota, RBAC | `tests/validate.sh` + `tests/contracts.sh` |
| A completed apply that changed platform state | add `tests/acceptance.sh` |
| Storage broker policy, image, or object storage | add `scripts/test-storage-broker.sh` |
| New tenant onboarded | all four |

## Tier 1 — structural

```bash
tests/validate.sh
```

No credentials. Checks that expected files exist, Terraform parses and is
formatted, and Helm values are valid YAML. Run it on every change; it is fast
and catches the things CI will reject anyway.

## Tier 2 — isolation contracts

Needs a PostgreSQL port-forward in a second shell:

```bash
source scripts/env-from-keychain.sh
kubectl port-forward -n infra svc/postgresql-primary 5432:5432
```

Then, in the shell where the environment is loaded:

```bash
export PG_HOST=localhost PG_ADMIN_USER=postgres
export PG_ADMIN_PASSWORD="$TF_VAR_pg_admin_password"
tests/contracts.sh
```

`env-from-keychain.sh` already exports `TF_VAR_pg_admin_password`; reference it,
never paste the value.

The contracts are enumerated in the header of `tests/contracts.sh`, each
tagged `C<n>` and cross-referenced to the Contracts section of
`framework/docs/concepts/platform.md`. Read that header for the current set — it covers
cross-namespace traffic, cross-schema access, the revoked `public` schema, the
deployer's lack of secrets access, quota enforcement, and the one-namespace /
one-schema / one-role-per-tenant invariants.

A contract failure is a security finding, not a flaky test. Report which
boundary broke and stop — do not proceed to ship.

## Tier 3 — acceptance

Same environment and port-forward as tier 2:

```bash
tests/acceptance.sh
```

The acceptance criteria are listed in the header of `tests/acceptance.sh`,
tagged `AC<n>` against the Acceptance Criteria section of
`docs/architecture/platform.md`. Run after an apply, against the live platform.

## Tier 4 — storage broker

```bash
source scripts/env-from-keychain.sh
[USER_TOKEN="<zitadel access token>"] scripts/test-storage-broker.sh
```

Runs the allow/deny matrix against the deployed broker plus a real S3
round-trip. Port-forwards, because the broker's ingress policy only admits
tenant namespaces. Without `USER_TOKEN` it exercises app-scope and the negative
cases (401/403/400); with one it also covers user-scoped prefixes.

## Interpreting failures

Read a failure against the contract it encodes, not as a generic red test.

- **C1 or C10 fails** — a network policy was removed or scoped to a namespace
  where it must be pod-scoped. Use the `network` skill.
- **C2 or C3 fails** — a grant leaked. Check `postgresql_grant.revoke_public`
  and the tenant's schema grants. Use the `database` skill.
- **C4 fails** — the deployer Role gained secrets access. That role is shared by
  every tenant; treat it as a blast-radius incident.
- **C6–C8 fail after onboarding** — the apply partially succeeded. Re-plan and
  look for a resource that errored without failing the run.
- **AC6 or AC7 fails** — ingress or certificates. Note that Cloudflare
  terminates TLS at the edge for tunnel-served hosts, so a missing in-cluster
  certificate is not always the fault. Use the `cloudflare` skill.
- **Broker 403 vs 401 vs 400** — 401 is identity (token missing or invalid), 403
  is policy (namespace, bucket, prefix, or operation not granted), 400 is a
  malformed request. Use the `storage` skill.

## Report

State which tiers ran, the pass and fail counts, and for any failure, the
contract it violates and the skill that owns the fix. Do not summarize a partial
run as "tests pass".
