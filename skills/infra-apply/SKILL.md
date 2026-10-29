---
name: infra-apply
description: Take an infrastructure change all the way to production — format, validate, plan, PR, merge, CI apply, and live evidence that the change is real and reachable. Use for any edit under terraform/, helm/, or k8s/, and whenever another infra skill needs to ship its change.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Infra Apply

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

The change-to-production loop. Every other infra skill makes its edit, then hands
off here. A change is not done when the file is edited, not when the PR is green,
and not when CI reports success. **It is done when a live query proves the new
state exists in production and is reachable.**

## Rules

- All changes go through code in this repository. Never `kubectl edit`,
  `kubectl patch`, or `helm upgrade` as the fix — a live mutation is drift that
  the next CI apply silently reverts.
- Never hand-edit OpenTofu state. Use `tofu import` or `tofu state rm`.
- Never print, log, or interpolate a credential value. Report verification
  results without exposing environment values or secret contents.
- Never start a local apply while CI is applying. `scripts/tf-apply.sh` checks
  this; a raw `tofu apply` does not.
- Prefer CI apply on merge. Local apply is the exception, and the code must still
  land on `main` afterwards or state and repository diverge.

## Step 1 — Make the change in code

Edit the Terraform, Helm values, or manifests. Keep the change minimal and
reviewable.

## Step 2 — Local preflight

```bash
TOFU="$(command -v tofu || echo /tmp/tf/tofu)"
"$TOFU" fmt -recursive terraform/
cd terraform/main && "$TOFU" init -backend=false -input=false -lockfile=readonly && "$TOFU" validate
```

Two things that will otherwise cost you time:

- **`-lockfile=readonly` is required.** Without it, `init` rewrites both
  `.terraform.lock.hcl` files with hashes for your local platform. It is easy to
  commit a hundred lines of churn without noticing.
- **Resolve the binary; do not hardcode `/tmp/tf/tofu`.** `CLAUDE.md` records
  that path as a convention, but nothing in the repo creates it and `/tmp` is
  reaped on reboot — a normally-installed `tofu` is usually what you have. Check
  `TOFU_VERSION` in the workflows and use a version that satisfies the same
  constraint.

CI enforces `fmt -check` and `validate` as required checks, plus a Trivy config
scan that fails the build. `.trivyignore` holds the accepted exceptions. Fixing
these locally is faster than a red PR.

## Step 3 — Plan against real state

Required for anything touching PostgreSQL resources, persistent volumes,
namespaces, or generated passwords. Optional for a DNS record or a value tweak.

```bash
scripts/tf-apply.sh plan     # sources the environment itself
```

Use `scripts/tf-apply.sh`, never raw `tofu`. The wrapper adds two guards a bare
apply skips: it refuses to run while a CI apply is in flight, and it preflights
GHCR pull access with a real `docker pull` (kubelet fails on layer authorization
where a manifest lookup succeeds).

**A port-forward is required, not optional, for a complete plan.** Every
`postgresql_*` resource targets `localhost:5432`:

```bash
kubectl port-forward -n infra svc/postgresql-primary 5432:5432
```

Without it the plan emits connection-refused errors for those resources **and
still prints a confident `Plan: N to add, M to change…` summary** covering only
the other providers. That summary is partial and easily misread as complete.
Read the errors, not just the count.

If the preflight fails with `denied: denied`, the GHCR token is stale — and
because the wrapper runs it before *every* subcommand, that blocks read-only
`plan` too. Fix the token; do not conclude there is no drift.

**Read the plan before proceeding.** Stop and confirm with the operator if it
shows any of:

- a destroy or replace of `postgresql_schema`, `postgresql_role`, or a
  `kubernetes_namespace` — this drops tenant data
- a replace of a PVC or `module.platform.helm_release.postgresql`
- a change to `random_password` — regenerating a password breaks every consumer
  that already holds it
- more resources changing than your edit explains

## Step 4 — Pull request

```bash
git checkout -b <branch>
git commit
git push -u origin <branch>
gh pr create
```

Required checks on `main`: **Format and Validate** and **Security Scan**. Both
run on every PR regardless of paths.

```bash
gh pr checks --watch
```

## Step 5 — Merge, and know whether that applies anything

```bash
gh pr merge
```

`terraform-apply.yml` runs on push to `main` **only** for the paths in its
`on.push.paths` filter. Read them before assuming your change applies:

```bash
sed -n '/^on:/,/^permissions:/p' .github/workflows/terraform-apply.yml
```

Broadly: the Terraform and Helm trees and the workflow's own dependencies.
Everything else merges without applying, which is the most common way a change
silently fails to reach production:

- A change to `framework/docker/storage-broker/**` or `framework/docker/pg-backup/**` builds and
  pushes an image. **It deploys nothing.** The image only reaches the cluster
  when you repin the digest or tag in `terraform/main/variables.tf` — a second
  PR, in a path that does trigger apply.
- A change to `k8s/network-policies/**` alone applies nothing. Those manifests
  are reference copies; the live policies are Terraform resources.
- A docs-only change applies nothing, which is correct.

If the merge triggers no apply and the change needed one, say so and open the
follow-up PR.

## Step 6 — Watch the apply

```bash
gh run list --workflow=terraform-apply.yml --limit 3
gh run watch <run-id>
```

The workflow holds a `terraform-apply` concurrency group and queues rather than
cancels — a second merge waits for the first. Swift object storage has no state
locking, so this GitHub-level guard is the only thing preventing state
corruption.

If the apply fails, read the failing step. Common causes:

- port-forward not ready — the workflow waits a bounded time for PostgreSQL
  (see its port-forward step); a PG restart during a Helm upgrade can outlast it
- GHCR preflight failure — the token lacks `read:packages` or per-package access
- a provider error on a resource your plan already flagged

A successful run is **not** evidence. Continue to step 7.

## Step 7 — Prove it in production

Run the live check that corresponds to the change. Each domain skill has an
**Evidence** section; use it. The general shape:

| Change | Evidence |
|--------|----------|
| Cluster / node pool | `kubectl get nodes` — count and `Ready` state |
| Helm release | `helm list -n infra` shows the new chart version, pods rolled |
| PostgreSQL role, schema, grant | `\dn` / `\du` in psql, or `scripts/check-postgresql-health.sh` |
| Kubernetes secret | secret exists and the consuming pod restarted — **never print its value** |
| Network policy | an actual connection attempt from a pod, not just `get netpol` |
| DNS record | `dig +short <name>` from outside the cluster |
| Published hostname | `curl -sSI https://<host>` returns the app, not the "nothing deployed" page |
| Image digest repin | the running pod's image matches the new digest |
| Tenant | namespace, quota, policies, schema, and role all present |

Then run the matching test tier with the `infra-verify` skill. Any change that
touches isolation — namespaces, network policies, PG roles or grants — must
re-run the contract tests before you call it done.

## Step 8 — Report

State plainly: what changed, which run applied it, and what live check proved
it. If any step was skipped or a check could not be run, say which and why.
Do not describe a change as deployed on the strength of a green pipeline alone.

## Local apply — the exception

Some changes have no CI path at all and must run locally:

- Node scaling and some Helm upgrades, which are slower than the workflow's
  `timeout-minutes`.
- **Credential rotations**, which change no tracked file. The apply workflow
  triggers on paths and has no `workflow_dispatch`, so there is nothing to merge
  and the "code must land on main" rule below does not apply. See the `database`
  skill.

```bash
source scripts/env-from-keychain.sh
scripts/tf-apply.sh apply -target=infomaniak_kaas_instance_pool.workers
```

Node pool changes take on the order of ten minutes or more. After any local
apply, the same change must still be merged to `main`, or the next CI apply
reconciles against stale code and reverts it.

## Rollback

Revert the PR and merge the revert — CI reapplies the previous declared state.
This is preferred over a corrective apply because it keeps code and state in
step. For a partial failure that left the cluster inconsistent, fix forward with
a targeted apply, then reconcile with a full apply.

## Drift

```bash
source scripts/env-from-keychain.sh
scripts/tf-apply.sh plan
```

A clean plan against unchanged code means production matches the repository.
Anything else is drift: someone changed the cluster by hand, or a local apply
never got merged. Report what drifted and which direction to reconcile.
