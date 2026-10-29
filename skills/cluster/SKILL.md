---
name: cluster
description: Manage the Kubernetes cluster — inspect nodes and workloads, scale the node pool, upgrade the Kubernetes version or a Helm release, adjust tenant quotas, and triage pods that are Pending, ImagePullBackOff, CrashLooping, or OOMKilled.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Cluster

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

Infomaniak managed Kubernetes. The cluster is deliberately small — a handful of
modest worker nodes sized for N+1 redundancy — so scheduling failures are
usually genuine capacity, not a bug. Check real headroom with `kubectl top
nodes` before assuming otherwise.

Load access first with the `cluster-access` skill.

## Where the truth lives

Read these rather than trusting any value written here. They are the source of
truth and they change.

| Question | File |
|----------|------|
| Node count, flavor, Kubernetes version | `terraform/main/variables.tf` — `node_pool_*`, `kubernetes_version` |
| Cluster and pool definition | `terraform/main/main.tf` — `infomaniak_kaas`, `infomaniak_kaas_instance_pool` |
| Which Helm releases exist and at what version | `terraform/main/main.tf` — every `helm_release` block |
| Chart configuration | `helm/values/` |
| Tenant quotas and container bounds | `framework/terraform/modules/tenant/main.tf` |
| Which tenants exist | `terraform/main/main.tf` — the `module "tenant_*"` blocks |

```bash
grep -n 'resource "helm_release"' -A4 terraform/main/main.tf | grep -E 'helm_release|chart|version'
grep -n 'module "tenant_' terraform/main/main.tf
```

## Rules

- Never fix a workload with `kubectl edit`, `kubectl patch`, or a manual
  `helm upgrade`. Everything here is a Terraform resource; a live mutation is
  drift the next apply reverts. The exceptions are deliberately reversible
  containment actions — suspending a CronJob, deleting a pod to force a
  restart — and even those should be reported and followed by a real fix.
- Never pin a workload to a node to work around an image pull failure. That is
  drift masking a broken credential.
- Diagnosis is read-only. Ask before restarting, scaling, or draining anything.

## What runs here

The `infra` namespace holds the shared services — PostgreSQL, the identity
provider, ingress, cert-manager, metrics, the tunnel connector, the storage
broker, and the backup CronJob. Each tenant gets its own namespace.

Ask the cluster rather than a list that goes stale:

```bash
kubectl get ns
kubectl get deploy,sts,cronjob -n infra
helm list -A
```

## Inspect

```bash
kubectl get nodes -o wide
kubectl top nodes
kubectl get pods -A -o wide
kubectl top pod -n <ns> --containers
kubectl get deploy,sts,cronjob -A
helm list -A
```

## Scale the node pool

Edit `node_pool_min_size` / `node_pool_max_size` / `node_pool_flavor` in
`terraform/main/variables.tf`. Read the current defaults there first — equal min
and max means a fixed-size pool, max greater than min enables autoscaling.

This takes 10–15 minutes — longer than the CI apply's 30-minute budget is
comfortable with. Run it locally with a target, then merge the code:

```bash
source scripts/env-from-keychain.sh
scripts/tf-apply.sh apply -target=infomaniak_kaas_instance_pool.workers
```

The merge is not optional. Code and state must agree or the next CI apply
reconciles against stale values.

## Upgrade Kubernetes

Change `kubernetes_version` in `variables.tf`. Read the current value there and
check Infomaniak's supported versions — the control plane is managed and will
reject an unsupported target.

Floor: the tenant Ingress hostname policy uses `ValidatingAdmissionPolicy`,
which is GA from 1.30. Never downgrade below that — the policy would stop
admitting and tenants could claim any hostname.

## Upgrade a Helm release

1. Find the current version:
   ```bash
   helm repo update && helm search repo <repo>/<chart> --versions | head -5
   ```
2. Read the chart's changelog for breaking value changes.
3. Update the `version` field on the `helm_release` in `main.tf`, and any
   affected file under `helm/values/`.
4. Apply. For a slow or risky release, target it:
   ```bash
   scripts/tf-apply.sh apply -target=helm_release.<name>
   ```

PostgreSQL deserves extra care: a chart upgrade restarts the primary, which can
outlast the CI workflow's port-forward wait and fail the apply mid-run. Prefer a
targeted local apply, and check replication afterwards with the `database`
skill.

## Adjust tenant resources

`ResourceQuota` and `LimitRange` live in
`framework/terraform/modules/tenant/main.tf`. Read the current values there; the
CPU and memory bounds are module variables passed per tenant, while the object
counts and container defaults are fixed in the module.

```bash
kubectl get quota,limitrange -n <tenant> -o yaml   # what is actually enforced
```

Changing the module changes all tenants at once. If only one tenant needs more,
pass a different value from its module block in `main.tf` rather than raising
the shared default.

## Triage a pod that will not run

**Pending**

```bash
kubectl describe pod -n <ns> <pod> | tail -20
```

- `Insufficient cpu/memory` — genuine capacity. Check `kubectl top nodes`; the
  cluster is small.
- `exceeded quota` — the namespace's ResourceQuota.
- `violates PodSecurity "restricted"` — the workload is missing the required
  security context. Use the `app-deploy` skill.
- No PV available — check the PVC and its StorageClass
  (`csi-cinder-sc-retain`).

**ImagePullBackOff**

```bash
kubectl describe pod -n <ns> <pod> | grep -A5 Events
kubectl get secret -n <ns> ghcr-pull
```

For infra images, the `ghcr-pull` secret is Terraform-managed and the token
needs `read:packages` **plus** per-package access. Verify the credential rather
than guessing:

```bash
scripts/check-ghcr-pull.sh <image-ref>
```

This does a real `docker pull` on purpose — kubelet has failed on layer
authorization where a manifest lookup succeeded. Do not repair the secret by
hand; fix the token and re-apply.

**CrashLoopBackOff**

```bash
kubectl logs -n <ns> <pod> --tail=50
kubectl logs -n <ns> <pod> --previous --tail=50
```

The previous container's logs hold the actual error; the current ones often
show only a restart.

**OOMKilled**

```bash
kubectl get pod -n <ns> <pod> \
  -o jsonpath='{.status.containerStatuses[*].lastState.terminated.reason}{"\n"}'
```

Raise the limit in code, or find what made it grow. For PostgreSQL specifically,
use the OOM triage path in the `database` skill — correlate with CronJobs and
suspend only the confirmed trigger.

## Restart a workload

```bash
kubectl rollout restart deploy/<name> -n <ns>
kubectl rollout status deploy/<name> -n <ns>
```

Safe and non-destructive, but it is containment, not a fix. If a restart is what
made things work, the underlying cause is still there.

## Evidence

A cluster change is done when the live cluster shows it:

```bash
kubectl get nodes                              # count and Ready state after scaling
kubectl version                                # server version after an upgrade
helm list -n infra                             # chart version after a release upgrade
kubectl rollout status deploy/<name> -n <ns>   # pods actually rolled
kubectl get quota,limitrange -n <tenant>       # quota change landed
kubectl get pod -n <ns> -o jsonpath='{.items[*].spec.containers[*].image}'
```

For an upgrade, "the apply succeeded" is not evidence — confirm the new version
is what is running and that dependent workloads are still Ready. Ship with
`infra-apply`, then run `infra-verify`.
