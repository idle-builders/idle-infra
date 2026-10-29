---
name: storage
description: Manage application object storage and the credential-mediating storage broker — provision buckets, grant prefixes or operations, develop and debug governed DuckLake S3 operations and cleanup, diagnose broker 4xx/5xx responses, and ship digest-pinned broker changes.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Storage

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

Apps do not hold S3 credentials. They ask the **storage-broker** in the `infra`
namespace for a short-TTL presigned URL, and the broker decides what they are
allowed to touch based on verified identity.

Reference: `docs/plans/presign-storage-broker.md`,
`framework/terraform/modules/storage-broker/`, `framework/docker/storage-broker/`.

## The isolation caveat — state this plainly

Infomaniak Swift has **no per-bucket credential scoping and no bucket
policies**. An EC2/S3 credential is **project-scoped** — it can reach every
container in the project, including other tenants' buckets, `infra-backups`, and
`infra-tfstate`.

So cross-tenant isolation here is app-layer plus blast-radius reduction, **not a
cryptographic boundary**. Each app gets its own credential so one can be revoked
independently, and each app is told only its own bucket. Hard isolation would
require a separate OpenStack project per tenant, deliberately deferred.

Terraform state is AES-256-GCM encrypted, so a leaked app credential yields
ciphertext only. Never describe this setup as hard multi-tenant isolation.

## The identity model

Two independent identities on every request:

- **App identity** — the pod's projected ServiceAccount token, verified by
  Kubernetes TokenReview. This determines the **bucket**; app A can only ever
  act within bucket A. It is not overridable by the caller.
- **User identity** — a Zitadel access token, verified against USERINFO. This
  resolves the `${sub}` placeholder in a prefix rule.

A prefix containing `${sub}` is **unusable without a verified Zitadel subject** —
the rule is skipped entirely, so a token-less request can never match
`u/${sub}/`. That is the enforcement mechanism.

`require_user_token` is a separate, blanket gate meaning "every request to this
namespace must carry a user token". It is therefore **incompatible** with a
namespace that also serves a service-scoped, credential-less path. Nimmly is
exactly that shape: `require_user_token = false` so the token-less crawler can
reach `corpus/`, which does not weaken `u/${sub}/` isolation at all.

## Where the truth lives

| Question | Source |
|----------|--------|
| Who may touch which bucket, ops, and prefixes | `tenant_policies` on the `storage_broker` module in `terraform/main/main.tf` — heavily commented, read the comments |
| What the broker actually loaded | `kubectl get configmap -n infra storage-broker-policy -o jsonpath='{.data.policy\.json}'` |
| Which buckets exist | the `module "object_storage_*"` blocks in `main.tf` |
| Pinned broker image | `storage_broker_image` in `terraform/main/variables.tf` |
| Authorization semantics | `framework/docker/storage-broker/policy.go` and `policy_test.go` |
| The app-side request contract | `docs/tenant-deployment-guide.md` |

The Terraform block is the declaration; the ConfigMap is what is live. If they
disagree, the pods did not roll.

## Give an app a bucket

Add an `object_storage_<tenant>` module block in `main.tf`. It creates the
container, an app-specific S3 credential, and an `object-storage` secret in the
tenant namespace — mirroring how the tenant module ships `database-url`.

Rotate a credential by tainting the `openstack_identity_ec2_credential_v3`
resource and re-applying; the secret refreshes with it.

## Grant a new prefix or operation

Edit `tenant_policies` in `main.tf`. Keep prefixes as narrow as the app actually
needs — a literal prefix is app-layer scope with no user binding, so only use
one where the app itself enforces ownership.

The broker loads its policy **once at startup**; there is no hot reload. The
module hashes the policy into the pod template annotation (`checksum/policy`),
so an apply rolls the pods automatically. If the pods did not roll, the new
policy is not live.

**That same mechanism is a trap when diagnosing a stale binary.** A PR touching
both the policy and the Go code rolls the pods — onto the *old* image — so
"did the pods restart?" answers **yes** while the fix is not deployed. A restart
is not a deployment. Compare digests, never ages.

## Ship a broker code change

Two PRs. This is the trap.

1. Change `framework/docker/storage-broker/**` and merge. CI runs `go vet` and
   `go test`, builds, and pushes to GHCR. **This deploys nothing.**
2. Repin `storage_broker_image` in `terraform/main/variables.tf` to the new
   digest and merge. That path triggers the apply that actually rolls the
   broker.

The image is pinned by digest deliberately. Do not switch it to a floating tag.

Run the tests locally first:

```bash
cd docker/storage-broker && go vet ./... && go test -count=1 ./...
```

## Debug a presign failure

The status code tells you which layer rejected:

| Code | Layer | Look at |
|------|-------|---------|
| 401 | Identity | The SA token is missing, has the wrong audience (`storage-broker`), or the Zitadel token failed verification |
| 403 | Policy | Namespace not in the policy, wrong bucket, operation not granted, or the key does not match an allowed prefix |
| 400 | Request | Malformed body, unknown `op`, missing key |

```bash
kubectl get configmap -n infra storage-broker-policy -o jsonpath='{.data.policy\.json}'
kubectl logs -n infra -l app.kubernetes.io/name=storage-broker --tail=50
kubectl get pod -n infra -l app.kubernetes.io/name=storage-broker
```

The broker logs the resolved prefix on an allowed decision and a safe reason
string on a deny — never a secret.

If the app cannot reach the broker at all (timeout rather than a status code),
that is the tenant's `allow-storage-broker` egress policy or the broker's
ingress policy. Use the `network` skill.

Also check the mounted token: the app must project a ServiceAccount token with
audience `storage-broker`. A default token will not do.

A `409` "bucket ownership conflict" means `storage_broker.storage_bucket_owners`
records another tenant for that bucket, or object history in it is shared. Never
update or delete an owner row to "fix" it: owner rows are immutable by contract
and each broker replica caches a positive ownership answer for 30 seconds. See
`framework/docs/concepts/governed-object-storage.md`.

## Evidence

A storage change is done when the live broker enforces it:

```bash
# the policy the broker actually loaded
kubectl get configmap -n infra storage-broker-policy -o jsonpath='{.data.policy\.json}'

# the pods rolled after the policy changed
kubectl get pod -n infra -l app.kubernetes.io/name=storage-broker

# the running image, and the digest actually running on each pod
kubectl get deploy -n infra storage-broker \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
kubectl get pod -n infra -l app.kubernetes.io/name=storage-broker \
  -o jsonpath='{range .items[*]}{.status.containerStatuses[0].imageID}{"\n"}{end}'

# ...and the digest of the most recent successful build on main.
# Comparing the running image to variables.tf is CIRCULAR — variables.tf is
# itself the artifact that goes stale. This is the external reference point:
gh run list --workflow=build-storage-broker.yml --branch main --limit 1 \
  --json databaseId --jq '.[0].databaseId' \
  | xargs -I{} gh run view {} --log | grep -i 'containerimage.digest'

# a failed apply is a second, independent way a green PR misses production
gh run list --workflow=terraform-apply.yml --limit 5 \
  --json conclusion,displayTitle --jq '.[] | "\(.conclusion)\t\(.displayTitle)"'

# allow/deny matrix plus a real S3 round-trip
source scripts/env-from-keychain.sh
[USER_TOKEN="<zitadel access token>"] scripts/test-storage-broker.sh
```

The acceptance script is the real proof — it exercises the same path an app
does, including the negative cases. A ConfigMap containing the right JSON is not
sufficient if the pods never restarted.

Ship with `infra-apply`, then run `infra-verify`.
