---
name: app-deploy
description: Set up and run a tenant application's deployment from its own repository — build the Dockerfile, Helm chart, migration job, and CI pipeline that this platform requires, mint the scoped CI credential, then deploy and prove the app is live and reachable.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# App Deploy

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

Run this from **the application's repository**. It builds the deployment
machinery and takes the app to a URL that serves.

The platform provides the target — namespace, database, secrets, registry
access, ingress path. The app owns its Dockerfile, chart, and pipeline. Write
those to fit the app; this skill tells you what the platform *requires* of them
and how to check you got it right.

## Rules

- **Never use the cluster-admin kubeconfig in an app's CI.** `KUBECONFIG_B64` in
  an app repo must be a namespace-scoped `deployer` ServiceAccount credential.
  Cluster-admin there gives a tenant workflow full control of the platform.
- Never create platform secrets from CI. The deployer deliberately has no
  secrets access. App secrets go through the `tenant` skill in the infra repo.
- Never print a token or kubeconfig. Pipe credentials straight into
  `gh secret set`.
- Never work around a rejected Ingress by disabling the admission policy.

## Where to find the current requirements

Do not trust a remembered value. These are the sources.

| Question | Source |
|----------|--------|
| What secrets and accounts the namespace already has | `kubectl get secret,sa -n <tenant>` |
| What the deployer may and may not do | the `deployer` Role in `framework/terraform/modules/tenant/main.tf`, or `kubectl auth can-i --list -n <tenant>` |
| Quota and container bounds the pod must fit | `kubectl get quota,limitrange -n <tenant> -o yaml` |
| Which hostnames the namespace may claim | `kubectl get ns <tenant> -o jsonpath='{.metadata.annotations.platform\.infra/allowed-hosts}'` |
| The connection string shape and its TLS params | the `database_url` secret built in the tenant module |
| Storage broker request contract and prefixes | `docs/tenant-deployment-guide.md`, plus `tenant_policies` in `terraform/main/main.tf` |
| **A working reference implementation** | an already-deployed tenant: `kubectl get deploy <app> -n <tenant> -o yaml` |

That last row is the most useful. A tenant already running on this platform has
a Deployment that satisfies every constraint below — read it before writing a
new one.

> `docs/tenant-deployment-guide.md` has accurate prose on migrations and driver
> behaviour, but its **chart examples predate the current platform**: they omit
> the security context and the PostgreSQL CA mount, and they request a
> cert-manager certificate the platform does not use. Use the guide for the
> reasoning, not as a template.

## Step 0 — Confirm the platform side exists

The namespace must be provisioned and the hostname permitted before anything
else. Check the annotation, the secrets, and the `deployer` account. If the
namespace is missing or the hostname is not covered, stop and fix that in the
infra repo with the `tenant` skill — the Ingress is rejected at deploy time
otherwise. DNS is the `cloudflare` skill.

## Step 1 — Establish the facts

From the app's code, not from assumption: the tenant namespace, the public
hostname (must fall within `allowed-hosts`), the listen port, an unauthenticated
health path, the non-root UID the image runs as, and whether the app needs the
database, migrations, or object storage.

## Step 2 — Dockerfile

Standard multi-stage build. The only platform constraint: **the image must run
as a non-root user**. A root container is rejected by `restricted` PodSecurity
before it is ever scheduled, so set a `USER` and make sure the filesystem
permissions suit it.

## Step 3 — Helm chart

Write a normal chart. It must satisfy these, all of which are enforced and all
of which fail in a way that looks like an application bug:

**Pod security.** The namespace enforces the `restricted` Pod Security Standard.
Every pod spec — Deployment, Job, init containers — needs a pod-level security
context (run as non-root with an explicit UID/GID, `fsGroup`, and the default
seccomp profile) and a container-level one (no privilege escalation, all
capabilities dropped). Read the exact fields from a deployed tenant rather than
recalling them; `kubectl explain` and the admission error message both name what
is missing.

**Image pull.** Reference the platform's GHCR pull secret in
`imagePullSecrets`. The name is whatever the tenant module creates — check
`kubectl get secret -n <tenant>`.

**Database TLS.** If the app connects to PostgreSQL it must mount the platform's
CA secret at the path the connection string's `sslrootcert` points to. Read both
the secret name and the path from the tenant module rather than guessing — the
mount path and the connection string have to agree, and a mismatch fails
permanently at connect time. Mount it in the migration Job too, not just the
Deployment.

**Ingress — no TLS block, no cert-manager annotation.** Cloudflare terminates
TLS at the edge and the tunnel reaches nginx over plain HTTP. The platform's own
Zitadel Ingress in `terraform/main/main.tf` is the reference implementation and
carries a comment saying exactly this. A `tls:` block also gives the hostname
admission policy a second surface to reject, for no benefit.

**Resources.** Requests and limits must fit inside the namespace's LimitRange
container ceiling and leave room within the ResourceQuota. Read both from the
live namespace.

**Spread.** Prefer a soft topology spread over a hard one. The cluster has few
small nodes, so a hard constraint turns a busy node into an unschedulable
replica.

**Migrations.** A Helm `pre-install,pre-upgrade` hook Job is the supported
pattern — the deployer Role grants `batch/jobs` create and delete precisely for
it, including the delete that `before-hook-creation` needs. The Job carries the
same security context and CA mount as the Deployment.

## Step 4 — The migration code

The chart is not enough; three platform constraints land in the migration code
itself.

**The role cannot CREATE SCHEMA.** It has privileges on its own schema only,
`public` is revoked, and there is no database-level `CREATE`. PostgreSQL checks
the privilege *before* evaluating `IF NOT EXISTS`, so even creating its own
schema fails. Keep the migration journal table unqualified so `search_path`
places it correctly, strip any `"public".` qualifier from generated DDL, and do
not use an ORM migrator that creates its own journal schema.

**The driver must load the CA itself.** The connection string uses libpq syntax
that many drivers do not honour — some ignore the CA path and verify against the
system trust store, some forward the unknown parameters to the server as startup
options and get rejected. Strip the TLS parameters from the string, read the
mounted CA file explicitly, require verification, and skip hostname checking
since the internal service name will not match the certificate.

**The Job must exit.** An open connection pool keeps the runtime alive, the pod
stays `Running`, and the Helm hook times out even though every statement
succeeded. Close the client after migrating.

## Step 5 — CI pipeline

The workflow must: build and push to GHCR (the built-in `GITHUB_TOKEN` covers
the push), install kubectl and Helm, materialise `KUBECONFIG_B64` to a mode-600
file, `helm upgrade --install` into the tenant namespace pinned to the commit
SHA, and then verify the rollout rather than trusting `--wait`.

Details worth matching to the infra repo's own workflows: pin actions by SHA,
set a concurrency group that queues rather than cancels, and give the deploy a
timeout longer than the migration hook. If Helm fails on secret access, set
`HELM_DRIVER=configmap` — the deployer has ConfigMap access specifically so
release history can live there without granting secrets.

Read `.github/workflows/` in the infra repo for the house conventions.

## Step 6 — Mint the CI credential

Prefer a bound ServiceAccount token Secret — non-expiring while the account
exists, no rotation to forget. The alternative, a TokenRequest token, has its
`--duration` **silently clamped** by the API server; if you use one, decode the
token and compare `exp` with `iat` to learn the duration you actually got.

Build the kubeconfig from the token plus the cluster CA and server URL, and pipe
it base64-encoded into `gh secret set` without displaying it. Use the selected deployment's credential-loading and CI configuration procedures.

Then prove the credential is scoped correctly:

```bash
KUBECONFIG=<deployer> kubectl auth can-i create deployments -n <tenant>   # expect yes
KUBECONFIG=<deployer> kubectl auth can-i get secrets -n <tenant>          # expect no
KUBECONFIG=<deployer> kubectl auth can-i get nodes                        # expect no
```

If the last returns yes, you built it from the admin credential. Stop and
rebuild.

## Step 7 — Dry-run before pushing

```bash
helm template ./chart --set image.tag=test | kubectl apply --dry-run=server -f -
```

A **server-side** dry run runs real admission control, so it catches PodSecurity
rejections and hostname-policy rejections without burning a deploy cycle. This
is the cheapest place to find the two most common failures.

## Failure modes

| Symptom | Cause |
|---------|-------|
| Pods never created, `violates PodSecurity "restricted"` | Missing or incomplete security context, or a root image |
| `UNABLE_TO_VERIFY_LEAF_SIGNATURE` | CA not mounted, or the driver ignored it |
| `unrecognized configuration parameter "sslrootcert"` | TLS params forwarded to the server as startup options |
| `permission denied for database postgres` on first DDL | Migration tries to create a schema |
| Migration logs success, hook times out | Connection pool never closed |
| ImagePullBackOff | Pull secret not referenced; for platform images the credential itself is wrong |
| Ingress rejected at deploy | Host outside the namespace's `allowed-hosts` |
| Serves the "nothing deployed" page | No Ingress claims the host |

Never pin pods to nodes with cached images to work around a pull failure — that
is drift masking a broken credential.

## Evidence

The deploy is done when the app answers from the public internet:

```bash
kubectl rollout status deploy/<app> -n <tenant>
kubectl get deploy <app> -n <tenant> \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
kubectl get ingress -n <tenant>
kubectl get endpoints -n <tenant> <app>
curl -sSI https://<host>
curl -s https://<host><health-path>
kubectl logs -n <tenant> deploy/<app> --tail=30
```

Pods Ready is not enough — a running pod behind an unclaimed hostname still
returns the platform's "nothing deployed" page, and a stale image tag rolls out
cleanly while serving the previous build. The public `curl` together with the
running image reference is the proof.

If the deploy needed an infra-side change — an allowed domain, a DNS record, a
secret, a bucket — that has its own PR, merge, and apply in the infra repo.
Confirm both halves before reporting done.
