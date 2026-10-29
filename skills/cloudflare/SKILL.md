---
name: cloudflare
description: Manage DNS and the edge — publish a hostname, add mail or verification records, bring a zone under management, check the Cloudflare Tunnel, reason about TLS, and diagnose 502s or the "nothing deployed" page.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Cloudflare

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

All DNS for the managed zones is code in `terraform/main/cloudflare.tf`. The
edge path is code in `framework/terraform/modules/edge/main.tf`.

Load access first with the `cluster-access` skill.

## The one fact that governs everything

**The cluster has no public origin.** There is no load balancer and no floating
IP; ingress-nginx is `ClusterIP`. The only path in is the Cloudflare Tunnel, and
the tunnel's config is a single catch-all route to
`ingress-nginx-controller.infra.svc.cluster.local:80`.

Consequences:

- Cluster-facing records are **proxied CNAMEs** to
  `<tunnel-id>.cfargotunnel.com`. Never point a cluster record at an IP address.
- There is **no per-host tunnel rule** to add. Publishing a new hostname does
  not touch `tunnel.tf`.
- Cloudflare terminates TLS at the edge for tunnel-served hosts.

## Where the truth lives

| Question | Source |
|----------|--------|
| Which zones are managed, and their IDs | `local.cloudflare_zone_ids` in `terraform/main/cloudflare.tf` |
| Which zones exist but are unmanaged | the header comment of that file |
| Every managed record | the `cloudflare_dns_record` resources in that file |
| The tunnel and its routing | `framework/terraform/modules/edge/main.tf` |
| The identity hostname | `var.identity_domain` in `terraform/main/variables.tf` |
| What is actually resolving | `dig +short <host>` |

The file header also records import dates and the conventions for adding
records. Read it before adding anything.

Apex NS and SOA are deliberately unmanaged — Cloudflare owns delegation.

## Publish a hostname

Up to three things must be true. **Check each before editing — often only the
first is actually missing.**

1. **DNS record** — add a `cloudflare_dns_record` in `cloudflare.tf`: proxied
   CNAME to the tunnel, reusing `local.cloudflare_zone_ids[...]`. Follow an
   existing cluster-facing record as the pattern. *Usually the only edit needed.*

2. **The namespace may already permit the host — check first.** The admission
   policy matches `host == d || host.endsWith('.' + d)`, so a namespace allowed
   `example.com` **already permits every subdomain of it**. Verify before
   touching anything:

   ```bash
   kubectl get ns <tenant> -o jsonpath='{.metadata.annotations.platform\.infra/allowed-hosts}{"\n"}'
   ```

   Only if the host falls outside that set do you widen
   `allowed_ingress_domains` in the tenant module. Do not widen it reflexively —
   it is a fail-closed security control, and a needless edit enlarges what every
   tenant workload may claim.

3. **An Ingress claiming the host** — in the app's own chart. Without it the
   request reaches the cluster and falls through to the default backend.

   Two things bite here. The host is often **not** a literal in the chart — it
   may be injected at deploy time from a CI variable, so grep the app's deploy
   workflow, not just `values.yaml`. And the app may not be deployed at all:
   confirm `kubectl get deploy,ingress -n <tenant>` is non-empty before calling
   a hostname published.

Edits 1 and 2 are this repo (hand off to `infra-apply`). Edit 3 lives in the app
repo (see the `app-deploy` skill).

## Add a mail or verification record

Append a `cloudflare_dns_record` — MX, TXT for SPF/DMARC/DKIM, or a CNAME for a
provider. These are not proxied and have nothing to do with the tunnel. Reuse
the zone ID local. Ship with `infra-apply`.

## Bring a new zone under management

1. Add the zone ID to `local.cloudflare_zone_ids`.
2. Write resources matching the live records exactly.
3. `tofu import` each one and plan until it reports **zero changes**.

A non-zero plan on first import means the code does not match reality — fix the
code, never let the apply "correct" a live zone you have not read.

## Tunnel health

```bash
kubectl get deploy,pod -n infra -l app.kubernetes.io/name=cloudflared
kubectl logs -n infra -l app.kubernetes.io/name=cloudflared --tail=50
```

The connector is a Deployment in `infra` with a PodDisruptionBudget and a
dedicated egress policy. If it cannot reach Cloudflare, every hostname is down
at once — that is the signature.

## TLS

Two different systems, and confusing them wastes time:

- **Tunnel-served hosts** — Cloudflare terminates TLS at the edge. A missing or
  pending in-cluster Certificate is often irrelevant to whether the site works.
- **cert-manager** — issues in-cluster certificates via the Let's Encrypt
  ClusterIssuer for anything that needs them.

```bash
kubectl get clusterissuer
kubectl get certificate,certificaterequest -A
```

If the browser shows a Cloudflare TLS error, look at the zone's SSL/TLS mode and
the proxy status of the record, not at cert-manager.

Be aware the zone's SSL/TLS mode is **not** managed as code — there is no
`cloudflare_zone_setting` resource in `terraform/`. It is dashboard state with
no drift detection, so a plan will never tell you it changed.

## Diagnose: 502, 404, or "nothing deployed"

Walk the path in order and stop at the first break.

| Check | Command | Meaning if it fails |
|-------|---------|---------------------|
| Record exists and is proxied | `dig +short <host>` | Should return Cloudflare IPs, never an origin IP |
| Tunnel connector up | `kubectl get pod -n infra -l app.kubernetes.io/name=cloudflared` | All hosts down together |
| ingress-nginx up | `kubectl get pod -n infra -l app.kubernetes.io/name=ingress-nginx` | All hosts down together |
| An Ingress claims the host | `kubectl get ingress -A` | **"nothing deployed" page** — this is the single most common cause |
| The Ingress was admitted | `kubectl describe ingress -n <ns> <name>` | Rejected by the hostname admission policy — the namespace annotation does not cover the host |
| Backing service has endpoints | `kubectl get endpoints -n <ns> <svc>` | 502 — no ready pods behind the service |
| Pods are Ready | `kubectl get pod -n <ns>` | 502 — use the `cluster` skill |
| Tenant admits ingress traffic | `kubectl get netpol -n <ns>` | 504/timeout — use the `network` skill |

The "nothing deployed" page is the default backend, and it means exactly one
thing: the request reached the cluster and no Ingress claimed that hostname —
or an Ingress claimed it but does not route the path you asked for.

**It answers HTTP 503, not 404.** That reads like a backend outage and is the
most common false alarm on this platform. Before escalating, check whether the
app's real health path answers 200 while `/` answers 503 by design.

## Evidence

A DNS or edge change is done when it resolves and serves from outside:

```bash
dig +short <host>                    # proxied Cloudflare addresses
curl -sSI https://<host>             # status and headers from the app
curl -s https://<host> | head        # not the "nothing deployed" page
kubectl get ingress -A | grep <host> # an Ingress claims it
```

For a mail or verification record, query the record type directly:

```bash
dig +short TXT <name>
dig +short MX <domain>
```

Cloudflare edge caches can lag a change by a minute or two; re-check before
concluding it failed. Ship with `infra-apply`.
