# Idle Infra

A reusable OpenTofu platform for a multi-tenant Kubernetes cluster on Infomaniak,
with Cloudflare ingress, PostgreSQL, Zitadel, governed object storage and optional
operational telemetry. Each operator owns a separate deployment repository and
its credentials, domains, tenant inventory and encrypted state.

`ambrovia/infra` is the first consuming deployment. This framework never selects
that cluster automatically and its CI does not receive production credentials.

## Start a deployment

Install Git, OpenTofu 1.12.x, kubectl and Helm. Docker is needed to build runtime
images and run local integration suites. Create an Infomaniak Public Cloud
project and a Cloudflare account with the zones you will publish.

Clone a reviewed framework revision, then scaffold a deployment:

```bash
git clone https://github.com/idle-builders/idle-infra.git
cd idle-infra
scripts/init-instance.sh /absolute/path/to/my-infra
cd /absolute/path/to/my-infra
```

The deployment pins the complete framework in a Git submodule. Commit that pin
alongside your configuration. On every machine and in CI, clone with
`--recurse-submodules` or run `git submodule update --init --recursive`.

Set required inputs through `TF_VAR_*` environment variables or a gitignored
`.tfvars` file. See `terraform/main/variables.tf`. Supply your own cloud project
IDs, domains, ACME email, first Zitadel administrator, database credentials,
backup and escrow credentials, state-encryption passphrase and runtime images.
Never commit credentials. `tenants` declares namespace names and allowed domains;
`tenant_db_passwords` supplies their separately stored passwords.

The default Kubernetes StorageClass is expected to be Infomaniak's retained
Cinder class. Verify that the class exists and supports expansion before
provisioning PostgreSQL. Select region, node capacity, placement and disk size in
your deployment. The sample uses the current Infomaniak stack; other clouds are
not yet supported.

## Bootstrap in stages

Kubernetes and PostgreSQL providers need live endpoints, so a fresh installation
cannot be applied in one pass. The existing-cluster migration does not need these
bootstrap targets.

1. With Infomaniak/OpenStack credentials loaded, initialize your deployment and
   plan/apply `-target=module.cluster`. Keep its initial local state encrypted.
2. Retrieve the new cluster's kubeconfig through Infomaniak and load it securely.
   Do not put cluster-admin credentials in Git or tenant CI. Set `kubeconfig_path`.
3. Read the actual post-DNAT endpoint with
   `kubectl get endpoints kubernetes -n default`. Set `apiserver_cidr` and
   `apiserver_port`; copying another cluster's address breaks network policies.
4. Plan/apply `-target=module.platform.helm_release.postgresql`. Wait for primary
   and replica readiness, then port-forward `infra/postgresql-primary` to the
   `postgresql_admin_port` configured in your deployment.
5. Review and apply a full plan. The remaining providers can now create roles,
   schemas, shared services and tenant boundaries.
6. Create a versioned state bucket using `framework/terraform/bootstrap` with its
   own encrypted local state. Add an S3 backend configured for **your** bucket,
   key and endpoint, then migrate deployment state with `tofu init -migrate-state`.
   Store backend credentials and encryption keys separately. The framework never
   points a new deployment at Ambrovia's state bucket.

The bootstrap bucket is a separate root. Serialize all writes to each remote
state. Infomaniak Swift does not provide the locking used by AWS examples; use
one CI concurrency group across all deployment and identity writers, and block
local applies while any of them is queued or running.

Use `-lockfile=readonly` for normal initialization. Framework changes never
implicitly upgrade provider locks.

## Runtime images and releases

The workflows build `storage-broker`, `infra-console`, `pg-backup`,
`otel-collector` and `telemetry-data` under `ghcr.io/idle-builders/idle-infra/`.
Builds publish commit tags and print their digests. Pin the selected image digests
in the deployment; publishing framework code deploys nothing. Console and
observability are optional; their image inputs are required when enabled.

For an upgrade, check out the chosen framework commit in the submodule, update
image pins when required, inspect the plan, and commit the dependency change in
the deployment repository. Keep release-specific migration blocks. Reverting a
source pin after state-address migration requires the corresponding reverse
migration; a plain Git revert is not a state rollback.

## Operate a selected cluster

Install skills and choose an instance independently:

```bash
framework/scripts/install-plugin.sh
framework/scripts/select-instance.sh /absolute/path/to/my-infra
framework/scripts/with-infra.sh kubectl get nodes
```

`.infra-instance.json` names the OpenTofu root and an optional deployment-owned
credential loader. CI can leave the loader empty and supply its environment.
Selection precedence is `INFRA_REPO`, the current deployment, then the explicitly
selected default. An invalid explicit selection fails closed. Installing a plugin
never changes the selected cluster.

## Verify changes

Run `tests/validate.sh` in the framework for example validation, chart-value
parsing, the API contract and instance-selection/credential-cleanup tests. Runtime
workflows run Go tests, the real DuckLake probe, generated-code checks, image
builds and the Collector/compactor integration path.

Each deployment must also run its own live contract and acceptance tests. These
must attempt both allowed and forbidden paths: namespace isolation, PostgreSQL
schema access, deployer permissions and application storage authority. A clean
plan alone does not prove those boundaries. A fresh cloud installation has not
yet been exercised by this extraction's example validation; record that proof
separately from migration testing.
