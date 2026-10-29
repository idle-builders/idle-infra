---
name: infra-debug
description: Diagnose a platform problem when the symptom does not name its cause — the site is down, it's slow, the app can't connect, the deploy failed, a request is being rejected. Walks from symptom to evidence to the domain that owns the fix. Read-only.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Infra Debug

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

The entry point when something is wrong and you do not yet know which part of
the platform owns it. This skill finds the cause and hands off; the domain
skills make the change.

Load access first with the `cluster-access` skill.

## Rules

- **Read-only by default.** Diagnose, then propose. Ask before mutating
  anything, including restarts.
- Containment that is reversible and narrow — suspending a CronJob, deleting a
  pod to force a restart — may be proposed with the reason stated, but it is not
  a fix and must be reported as containment.
- Never repair by hand what Terraform owns. A live patch that "fixes" it creates
  drift and hides the real defect.
- Never print secret values while investigating.
- Say what you could not check. An unchecked layer is not a ruled-out layer.

## Method

1. **Establish the symptom precisely.** "Down" is not a symptom. A status code,
   an error string, a timeout, a pod state, a timestamp — those are.
2. **Find the narrowest true statement.** One host or all hosts? One tenant or
   all? Started when? Correlate with the last apply:
   `gh run list --workflow=terraform-apply.yml --limit 5`.
3. **Follow the table below** to the owning layer.
4. **Prove the cause** with a command whose result would differ if you were
   wrong.
5. **Hand off** to the domain skill, and ship the fix through `infra-apply`.

## Symptom router

### "The site is down" / 502 / 404 / "nothing deployed"

Walk the request path outside-in and stop at the first break.

| Check | Failure means |
|-------|---------------|
| `dig +short <host>` | No record, or pointing at an IP instead of the tunnel |
| `kubectl get pod -n infra -l app.kubernetes.io/name=cloudflared` | Tunnel down — **all** hosts fail together |
| `kubectl get pod -n infra -l app.kubernetes.io/name=ingress-nginx` | Ingress controller down — all hosts fail together |
| `kubectl get ingress -A` | No Ingress claims the host → the "nothing deployed" page |
| `kubectl describe ingress -n <ns> <name>` | Rejected by the hostname admission policy |
| `kubectl get endpoints -n <ns> <svc>` | No ready pods → 502 |
| `kubectl get pod -n <ns>` | Pod not running → see below |

→ `cloudflare` skill, or `cluster` skill for the pod.

### "My app can't reach the database"

Check these in order. The first two resolve most cases in seconds and are
skipped surprisingly often.

| Check | Failure means |
|-------|---------------|
| `kubectl get pod -n <ns>` and the ResourceQuota usage | **No pod at all** — nothing is knocking on the door. Either it was never deployed, or admission rejected it |
| `kubectl logs -n infra postgresql-primary-0 -c postgresql --since=1h \| grep '<tenant>'` | **No lines** → the traffic never arrived (network, or the client is outside the cluster). **Lines present** → it arrived and was refused; the reason is in the log |
| Is the client actually a pod in the cluster? | PostgreSQL is ClusterIP-only. A laptop, a hosted front end, or external CI dialling the internal service name hangs and times out by design |
| `kubectl get events -n <ns>` | `violates PodSecurity "restricted"` — the pod was never created. See `app-deploy` |
| `kubectl get netpol -n <ns>` | `allow-postgres` mis-scoped, or the app dials a port/label the rule does not cover. Note the rule is created identically for every tenant, so a *missing* policy in an established namespace is unlikely |
| Is `pg-ca-cert` mounted at `/etc/ssl/pg/`? | `verify-ca` fails — but this errors, it does not hang |
| `SELECT count(*) FROM pg_stat_activity;` vs `SHOW max_connections;` | At the cap → exhausted for everyone |
| `scripts/check-postgresql-health.sh` | Replication is unwell (this script checks little else) |

→ `network`, `database`, `cluster`, or `app-deploy` skill.

### "A pod won't start"

| State | First command | Owner |
|-------|---------------|-------|
| Pending | `kubectl describe pod` — capacity, quota, PodSecurity, PVC | `cluster`, or `app-deploy` for PodSecurity |
| ImagePullBackOff | `scripts/check-ghcr-pull.sh <image>` | `cluster` |
| CrashLoopBackOff | `kubectl logs --previous` | app owner |
| OOMKilled | `lastState.terminated.reason` | `cluster`, or `database` for PostgreSQL |
| Never created | events — `violates PodSecurity "restricted"` | `app-deploy` |

### "It's slow"

```bash
kubectl top nodes
kubectl top pod -n <ns> --containers
scripts/postgresql-observe.sh --limit 25 --min-active-ms 250
```

Separate the layers: node saturation, a single pod at its limit, or database
pressure. `risk_flags` in the observation output points at expensive queries.
Check the connection count before blaming any single query.

→ `cluster` or `database` skill.

### "My deploy failed"

| Where | Check |
|-------|-------|
| App CI | Rejected Ingress host, PodSecurity, missing image pull secret, hook Job timeout |
| Infra CI | `gh run list --workflow=terraform-apply.yml` — port-forward timeout, GHCR preflight, provider error |
| Merged but nothing happened | The changed paths do not trigger apply — see `infra-apply` |

The last one is the quiet killer: a `framework/docker/**` change builds an image and
deploys nothing until the digest is repinned in `variables.tf`.

→ `app-deploy` or `infra-apply` skill.

### "A request is being rejected"

| Code | Meaning |
|------|---------|
| 401 from an app API | Token identity — see `zitadel` |
| 401 from the broker | SA token missing or wrong audience, or Zitadel token invalid |
| 403 from the broker | Policy — namespace, bucket, op, or prefix |
| 400 from the broker | Malformed request |
| Token will not decode | Auth Token Type is not JWT — see `zitadel` |
| Ingress rejected at deploy | Hostname admission policy — see `tenant` |

→ `zitadel` or `storage` skill.

### "Something changed and I don't know what"

```bash
gh run list --workflow=terraform-apply.yml --limit 10
source scripts/env-from-keychain.sh && scripts/tf-apply.sh plan
kubectl get events -A --sort-by=.lastTimestamp | tail -30
```

A non-empty plan against unchanged code is drift: a hand-edit, or a local apply
never merged. Report which direction to reconcile — the repository is the source
of truth.

## Escalation to whole-platform state

If the symptom is diffuse, take a full snapshot with `infra-check` first, then
come back to this router with a narrower symptom.

## Report

State: the symptom as observed, the layer that owns it, the evidence that proves
it, what you did **not** check, and the recommended fix with the skill that owns
it. If containment was applied, say so explicitly and separate it from the fix.

Do not offer a cause you have not evidenced. "Probably the network policy" with
no connection test is a guess, and guesses here cost hours.
