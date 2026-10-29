---
name: network
description: Reason about and change in-cluster connectivity — find which network policy governs a source-to-destination pair, author a new allowlist in house style, and diagnose connections that hang or are refused between namespaces.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Network

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

Every namespace is default-deny for both ingress and egress. Connectivity exists
only where an explicit allowlist grants it. This is the platform's primary
isolation boundary, and it **fails closed and silently** — a missing rule looks
exactly like an application bug or a slow dependency.

Load access first with the `cluster-access` skill.

## Rules

- Never widen a policy to "make it work". Scope the new rule to the specific
  pod, port, and direction that is actually needed.
- Never grant namespace-wide access where pod-scoped access will do. The tenant
  module deliberately moved off a broad `allow-infra` policy to pod-scoped rules
  for exactly this reason.
- Never edit a NetworkPolicy live. They are Terraform resources. The manifests
  in `k8s/network-policies/` are reference copies — changing those applies
  nothing.
- Any policy change must re-run the isolation contracts before it is done.

## Where policies live

| Scope | File |
|-------|------|
| `infra` namespace | `terraform/main/main.tf` |
| Every tenant namespace | `framework/terraform/modules/tenant/main.tf` |
| Storage broker ingress/egress | `framework/terraform/modules/storage-broker/main.tf` |
| cloudflared egress | `framework/terraform/modules/edge/main.tf` |
| Reference copies, **not live** | `k8s/network-policies/` |

## What a tenant gets by default

Read the current set from the module or the cluster — do not work from memory:

```bash
grep -n 'resource "kubernetes_network_policy"' framework/terraform/modules/tenant/main.tf
kubectl get netpol -n <tenant>
kubectl get netpol -n <tenant> <name> -o yaml
```

The shape is always the same: a `default-deny` covering both directions, plus a
narrow pod-scoped allowlist for each thing a tenant legitimately reaches — DNS,
the database, the identity provider, the storage broker, outbound HTTPS — and an
ingress rule admitting the ingress controller.

Anything outside that set — reaching another tenant, a non-HTTPS external port,
a new shared service — needs a new rule.

## Find what governs a connection

```bash
kubectl get netpol -A
kubectl get netpol -n <src-ns> -o yaml
kubectl describe netpol -n <dst-ns> <name>
kubectl get pod -n <dst-ns> --show-labels
```

Both directions must permit the traffic: **egress from the source** and
**ingress to the destination**. Most "it still doesn't work after I added the
rule" cases are one side only.

Check the destination pod's labels against the selector in the rule. A
pod-scoped policy that names a label the pod does not carry silently matches
nothing.

## Diagnose a blocked connection

Symptom tells you a lot:

- **Hangs, then times out** — almost always a NetworkPolicy.
- **Connection refused immediately** — the service or pod is down, not a policy.
  Use the `cluster` skill.
- **DNS resolution fails** — `allow-dns` is missing or the destination name is
  wrong.

**Passive evidence first.** PostgreSQL runs with `log_connections = on`, so for
any database-connectivity question the cheapest discriminator needs no pod at
all:

```bash
kubectl logs -n infra postgresql-primary-0 -c postgresql --since=1h | grep '<tenant>'
```

A connection that was dropped by a NetworkPolicy leaves **no trace** — the
packet never arrived. A connection that arrived and was rejected on TLS, auth,
or `pg_hba` **is logged**. That single check splits "blocked in the network"
from "reached the server and was refused", which is the fork you are usually
trying to resolve.

When you do need an active probe:

```bash
kubectl run netcheck -n <src-ns> --restart=Never --rm --attach --quiet \
  --image=busybox:1.37 \
  --overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":1000,
  "runAsGroup":1000,"seccompProfile":{"type":"RuntimeDefault"}},
  "containers":[{"name":"netcheck","image":"busybox:1.37",
  "securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}},
  "command":["sh","-c","nc -zv -w3 <host> <port>"]}]}}'
```

Three things this gets right that a bare `kubectl run` does not: no `-it`, so it
works in a non-interactive shell; a full security context, because tenant
namespaces enforce `restricted` and reject a plain busybox pod; and
`--restart=Never` with `--rm` so nothing is left behind.

**This creates a pod — it is a mutation.** Ask the operator before running it
during a read-only diagnosis, and prefer the passive evidence below when it
answers the question.

## Author a new rule

Copy the shape of an existing pod-scoped policy in the same file. A rule needs:
direction (`policy_types`), a `pod_selector` for who it applies to, a
`namespace_selector` **plus** `pod_selector` for the other end, and explicit
ports.

Three traps that have bitten this repo:

- **Post-DNAT ports.** The infra API-egress policy must allow the port *after*
  DNAT, not the service port — matching the pre-DNAT port silently blocks. The
  live value is in the `infra_allow_apiserver_egress` policy in
  `terraform/main/main.tf`; it is tied to the cluster's API endpoint and can
  change if that endpoint does.
- **Per-component internet egress.** cert-manager, Zitadel, cloudflared, and the
  backup job each have their own narrow egress rule. There is no shared "allow
  outbound" in `infra` — adding a component that talks to the internet means
  adding its rule.
- **Tenant-wide blast radius.** A change in `modules/tenant/main.tf` applies to
  every tenant. If only one needs it, say so explicitly in the code comment, and
  consider whether it belongs in the module at all.

## Evidence

A network change is done when a real connection proves it — not when the policy
object exists:

```bash
# the rule is live
kubectl get netpol -n <ns> <name> -o yaml

# the connection now succeeds from where it must — see the probe above,
# which carries the security context the namespace requires

# and isolation did not regress
tests/contracts.sh     # C1 cross-namespace, C10 default-deny
```

Both halves matter. A rule that opens the intended path *and* leaves C1/C10
passing is the standard. If the contracts fail, you widened something you did
not intend to.

Ship with `infra-apply`, then run `infra-verify`.
