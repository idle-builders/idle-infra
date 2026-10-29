# Shared Infrastructure Platform

## Platform

**Definition.** The platform is a declarative infrastructure layer — Terraform modules and Helm value files in this repo — that provisions and configures a shared Kubernetes cluster, self-hosted PostgreSQL instance, and shared services on Infomaniak's Swiss cloud.

**Essential properties.**
- Provisions a single K8s cluster (Infomaniak Managed Kubernetes)
- Provisions a single self-hosted PostgreSQL instance (Bitnami Helm chart — Infomaniak has no managed PG)
- Defines tenant isolation boundaries (namespace + schema per tenant)
- Deploys shared services (LLM gateway, ingress controller, cert-manager)
- All changes are version-controlled and applied via `terraform apply`
- Outputs connection info and credentials that app repos consume

**What it is not.**
- **Not a PaaS.** Tenants interact directly with K8s APIs and PG connections. The platform provisions infrastructure; it doesn't abstract it away. Boring technology means using K8s directly, not building a layer on top.
- **Not a deployment pipeline.** CI/CD (GitHub Actions) lives in each app repo. The platform provides the target (cluster, registry, database); the app decides when and how to deploy. Infra changes and app changes have different review cycles.
- **Not a control plane.** No runtime API, no dashboard, no tenant self-service. Tenants are onboarded by adding config to this repo and running `terraform apply`. IaC-only is simpler, auditable, and sufficient for 3 tenants.
- **Not a mono-repo for applications.** App code, Helm charts, and GitHub Actions live in each app's own repo. This repo contains only shared infrastructure definitions.

**Composition.**
- Terraform modules: cluster, database, networking, DNS, object storage, tenant scaffolding
- Helm value files: shared service configuration (LiteLLM, ingress)
- Outputs: kubeconfig, PG connection strings, namespace names, GHCR paths

**Lifecycle.** `plan` → `apply` → running. Changes via PRs to this repo, reviewed, then applied.

**Authority.** This repo is the source of truth for infrastructure state. App repos are the source of truth for application workloads.

---

## Tenant

**Definition.** A tenant is an application that consumes shared infrastructure, represented as a K8s namespace + PG schema + RBAC bindings + network policies.

**Essential properties.**
- Name (unique identifier, used as namespace and schema name)
- K8s namespace (one per tenant, name matches tenant name)
- PG schema (one per tenant, name matches tenant name)
- PG role (owns exactly its schema)
- Resource quotas (CPU, memory limits per namespace)
- Network policies (default-deny with allowlist for shared services)

**What it is not.**
- **Not a user.** A tenant is an application, not a person. Users exist within a tenant's application layer. User identity is an application concern.
- **Not a deployment.** A tenant can have zero, one, or many deployments. The tenant exists before any workload is deployed — namespace and schema are pre-provisioned.
- **Not a billing entity.** Infomaniak bills the account holder. Per-tenant cost tracking is observability, not a platform primitive. Three tenants owned by one person doesn't need chargeback.

**Composition.**
- Owns: K8s namespace, PG schema, PG role, K8s service accounts, network policies, resource quotas
- References: shared services (LLM gateway, ingress), GHCR image paths
- Referenced by: app repo's Helm chart and GitHub Actions (targets the tenant's namespace)

**Lifecycle.** Registered (added to Terraform config) → Provisioned (`terraform apply` creates namespace + schema) → Active (workloads deployed by app repo) → Decommissioned (resources cleaned up).

**Authority.** Tenant definition lives in this repo (Terraform). Tenant workloads live in the app repo (Helm chart + GitHub Actions).

**Current tenants.**

| Tenant | Namespace | Schema | Status |
|--------|-----------|--------|--------|
| hyperidle | `hyperidle` | `hyperidle` | Deployed — namespace, schema, secrets provisioned |
| ambrovia | `ambrovia` | `ambrovia` | Deployed — namespace, schema, secrets provisioned |
| nimmly | `nimmly` | `nimmly` | Deployed — namespace, schema, secrets provisioned |

---

## Shared Service

**Definition.** A shared service is a workload deployed by the platform that runs once in the cluster and serves all tenants. It lives in the `infra` namespace.

**Essential properties.**
- Single deployment, owned by the platform (this repo)
- Accessible by all tenant namespaces via network policies
- Tenant-agnostic API — tenants authenticate explicitly, not by network origin
- May have its own PG schema for internal state

**What it is not.**
- **Not infrastructure.** K8s, PG, load balancers are infrastructure managed by Terraform. Shared services are application-level workloads that happen to serve all tenants. The distinction matters for ownership: infra is Terraform resources, shared services are Helm releases.
- **Not a tenant workload.** Even though it runs in K8s, a shared service is owned by the platform, not by any tenant. Different lifecycle, RBAC, and network policies.

**Composition.**
- K8s deployment + service + (optional) ingress
- Helm chart or value file in this repo
- May reference external APIs (e.g., Infomaniak AI API, OpenAI)
- May have its own PG schema in the shared database

**Lifecycle.** Deployed by Helm via this repo. Updated via PRs to this repo.

**Authority.** This repo.

**Current shared services.**

| Service | Purpose | Status |
|---------|---------|--------|
| LLM gateway (LiteLLM) | Unified LLM API proxy | Deployed |
| ingress-nginx | External traffic routing to tenant services | Deployed |
| cert-manager | TLS certificate automation | Deployed |
| storage broker | Credential mediation and governed object authorization | Deployed v1; governed-row v2 in progress |

---

## Isolation Boundary

**Definition.** The structural mechanism that separates tenants from each other and from shared services. Enforced by K8s network policies and PG role permissions, not by application code.

**Essential properties.**
- **K8s layer:** namespace-per-tenant, default-deny network policies, resource quotas
- **PG layer:** schema-per-tenant, role-per-tenant with schema ownership, no cross-schema grants
- **Structural enforcement:** isolation is not advisory — it's enforced by the platform, not trusted to application code

**What it is not.**
- **Not application-level access control.** RLS, user permissions, and authorization are application concerns within a tenant's schema. The isolation boundary separates tenants from each other, not users within a tenant.
- **Not network segmentation between environments.** There's one cluster, one database. Isolation is between tenants, not between dev/staging/prod.

**Contracts.**
- New namespaces deny all ingress/egress by default except: DNS, shared services in `infra` namespace, internet egress
- Each tenant's PG role can only access its own schema. `SET search_path` defaults to the tenant's schema.
- No cross-schema queries without explicit shared views/functions in a `shared` schema
- Resource quotas prevent one tenant from starving others

---

## LLM Gateway

**Definition.** The LLM gateway is a LiteLLM deployment in the `infra` namespace that provides a unified, OpenAI-compatible API for accessing LLM models across providers.

**Essential properties.**
- OpenAI-compatible API endpoint (drop-in replacement for OpenAI SDK)
- Routes requests to multiple providers: Infomaniak AI API, OpenAI, Anthropic, Google Gemini
- Per-tenant virtual keys for usage tracking and rate limiting
- Single deployment serving all tenants

**What it is not.**
- **Not a model host.** It doesn't run models. It proxies requests to external API providers. We use Infomaniak's hosted models, OpenAI, etc. — not self-hosted inference.
- **Not an AI feature.** It's infrastructure for AI features. Applications build intelligence on top; the gateway just routes and tracks.

**Composition.**
- LiteLLM Helm chart in `infra` namespace
- Provider credentials as K8s secrets (Infomaniak API key, OpenAI API key, Anthropic API key)
- LiteLLM config (model routing, fallbacks, rate limits)
- Optional: PG schema for usage/spend tracking

**Providers.**

| Provider | Endpoint | Models | Notes |
|----------|----------|--------|-------|
| Infomaniak AI | `api.infomaniak.com/2/ai/{product_id}/openai/v1` | Llama 3, Mistral 3, Qwen 3, Gemma, DeepSeek | Swiss-hosted, cheapest, OpenAI-compatible |
| OpenAI | `api.openai.com/v1` | GPT-4o, GPT-4o-mini | Primary commercial provider |
| Anthropic | `api.anthropic.com/v1` | Claude Opus, Sonnet, Haiku | Via LiteLLM Anthropic adapter |
| Google | Vertex AI / AI Studio | Gemini Pro, Flash | Via LiteLLM Google adapter |

**Lifecycle.** Deployed as Helm release. Config updated for new models/providers via PRs to this repo.

---

## Health Evidence

**Definition.** Health evidence is the versioned, machine-readable account of
cluster, database, shared-service, and tenant-workload behavior that an operator
or authorized agent can query to determine whether the platform is healthy and
why. It combines current state with bounded history while preserving the shared
identity of a signal across Kubernetes resources, PostgreSQL activity, and
application output.

**Essential properties.**
- Covers cluster capacity, workload lifecycle, scheduled jobs, PostgreSQL health,
  query pressure, backups, and application-provided health signals
- Uses stable platform and workload identities so signals can be correlated
- Is queryable without a dashboard and produces deterministic structured output
- Retains enough bounded history to distinguish an incident from a trend
- Excludes secrets and sensitive payloads by default
- Can be consumed by maintainers, command-line routines, and scheduled agents

**What it is not.**
- **Not dashboard-dependent.** The machine-readable API and MCP contract is
  primary. The infra console visualizes the same evidence for humans.
- **Not an autonomous control plane.** Evidence describes reality. Any agent
  acting on it remains constrained by a separate remediation policy.
- **Not unlimited telemetry.** Signals, labels, retention, and sampling earn
  their cost by supporting a named health or diagnostic decision.
- **Not tenant-specific.** Applications contribute identities and health signals,
  but collection and querying remain deployment-independent.

**Lifecycle.** Collect → normalize identity → retain within policy → query →
evaluate → expire. A missing or stale signal is itself explicit evidence rather
than silently appearing healthy.

**Authority.** This repo owns the health-evidence contract and shared collection
services. Application repositories own their instrumentation and workload
identity values.

---

## Remediation Policy

**Definition.** The remediation policy is the safety boundary that determines
what a scheduled health agent may observe, contain automatically, or propose for
review. Diagnosis and action are separate phases with separately auditable
permissions.

**Essential properties.**
- Read-only diagnosis is the default
- Automatic containment is limited to explicitly allowlisted, reversible actions
- Persistent configuration and optimization changes are proposed through
  reviewed pull requests
- Destructive data, storage, credential, and access-control changes always
  require human approval
- Every run records its evidence, reasoning, actions, and outcome

**What it is not.**
- **Not permission to optimize from one sample.** Optimization requires repeated
  evidence or a reproducible threshold breach.
- **Not an unrestricted cluster administrator.** Agent access is least-privilege
  and separated by diagnostic and action capability.
- **Not self-expanding authority.** An agent cannot add actions to its own
  allowlist.

**Authority.** This repo defines the shared safety policy. Maintainers approve
allowlist changes and all persistent changes before they reach the cluster.

---

## Deferred Concepts

### Shared Identity Provider

**Status:** Deferred. Not part of v1.

**Rationale.** Research into both consuming applications shows fundamentally different auth models:

- **Ambrovia** is deeply coupled to Supabase Auth (magic link email, Google OAuth, Azure OAuth, JWT sessions, RLS policies, account-based architecture). Replacing Supabase Auth requires swapping ~8 dependencies and rewriting session management, middleware, and database triggers.
- **HyperIdle** uses invite-code onboarding with bearer token sessions (shared mode) and API key auth (local mode). No OAuth/OIDC. Adding OIDC is explicitly deferred to a future workpackage (S5+).

Neither app currently needs or benefits from a shared identity provider. When apps migrate to K8s and the shared PostgreSQL, a shared identity provider (Authentik, Keycloak, or similar) can be deployed as a shared service that tenants optionally delegate OAuth/OIDC to. This is a separate concept to probe at that time.

**For v1:** Each tenant handles its own authentication. The platform provides infrastructure-level access control only (K8s RBAC, PG roles, network policies).

---

## Vocabulary

| Term | Definition | Aliases NOT to use |
|------|------------|--------------------|
| **platform** | The shared infrastructure layer provisioned by this repo | infra (too vague), cluster (one component), environment |
| **tenant** | An application consuming shared infrastructure | app (ambiguous with K8s), client, project, consumer |
| **namespace** | The K8s namespace assigned to a tenant | project (GCP term), environment |
| **schema** | The PostgreSQL schema assigned to a tenant | database (that's the instance), partition |
| **shared service** | A workload deployed by the platform that serves all tenants | platform service, common service, core service |
| **LLM gateway** | The LiteLLM proxy shared service | AI proxy, model proxy |
| **isolation boundary** | The structural separation between tenants (namespace + schema + network policies + PG roles) | fence, wall, partition |
| **health evidence** | Versioned structured observations used to decide platform health and explain abnormal behavior | monitoring data, telemetry dump |
| **remediation policy** | The declared boundary between diagnosis, automatic containment, and reviewed change | agent permissions, auto-fix |

---

## Contracts

### Platform
- **Single cluster.** One K8s cluster serves all tenants. No multi-cluster.
- **Single PG instance.** One managed PostgreSQL instance serves all tenants. Schemas provide isolation.
- **IaC-only.** All infrastructure changes go through Terraform in this repo. No manual console changes.
- **Remote state.** Terraform state is stored in Infomaniak S3-compatible object storage (OpenStack Swift). State locking via DynamoDB-compatible backend or file-based locking.
- **Idempotent apply.** `terraform apply` and `helm upgrade` are safe to run repeatedly.
- **GHCR for images.** Container images are pushed to GitHub Container Registry. No self-hosted registry.

### Tenant
- **One namespace per tenant.** Exactly one. Name matches tenant name.
- **One schema per tenant.** Exactly one. Name matches tenant name.
- **One PG role per tenant.** Owns exactly its schema. Cannot access other schemas.
- **No cross-namespace traffic by default.** Network policies deny it unless explicitly allowed.
- **No cross-schema queries.** Shared data goes in shared schemas with explicit grants.
- **Resource quotas on every namespace.** CPU and memory limits prevent noisy-neighbor.

### Shared Service
- **`infra` namespace.** All shared services run there.
- **Reachable by all tenants.** Network policies allow tenant → `infra` namespace traffic on declared ports.
- **Explicit authentication.** Tenants authenticate to shared services via credentials (API keys, connection strings), not network identity.

### LLM Gateway
- **OpenAI-compatible API.** All models accessed via OpenAI SDK-compatible endpoints.
- **Per-tenant virtual keys.** Each tenant gets its own LiteLLM key for tracking and rate limiting.
- **Provider-agnostic.** Adding or removing a backend provider doesn't change the tenant-facing API.

### Isolation Boundary
- **Structural, not advisory.** Enforced by K8s and PG, not application code.
- **Default-deny networking.** New namespaces deny all ingress/egress except DNS, `infra` namespace, and internet egress.
- **Schema owner role.** Each tenant's PG role owns exactly its schema. `search_path` defaults to tenant schema.

### Health Evidence
- **Machine-readable first.** Every shared health result is available as
  structured data without requiring a dashboard.
- **Stable identity.** Cluster, namespace, workload, pod, container, service,
  scheduled job, database, and application identities remain correlatable.
- **Bounded cost.** Collection and retention have explicit resource and storage
  budgets; unused signals are removed.
- **Sensitive by default.** Raw SQL, credentials, request payloads, and personal
  data are excluded unless a time-bounded privileged diagnostic explicitly
  enables them.
- **Missing is not healthy.** Collection gaps and stale observations are reported
  explicitly.

### Remediation Policy
- **Read-only first.** Scheduled agents diagnose without mutation by default.
- **Allowlisted containment only.** Automatic cluster mutation is reversible,
  narrowly scoped, and declared ahead of time.
- **Persistent change by review.** Configuration and optimization changes flow
  through reviewed pull requests and normal deployment controls.
- **No destructive autonomy.** Database mutation, volume deletion, secret
  rotation, access changes, and authority expansion require a maintainer.

---

## Rationale

This shape was chosen because:

1. **Three tenants, one operator.** A full PaaS or control plane is overengineered for 3 apps owned by one person. IaC-only is the right abstraction level — auditable, version-controlled, and zero runtime overhead.

2. **Structural isolation over trust.** Even with one operator, treating isolation as structural (network policies, PG roles) rather than advisory means bugs in one tenant can't accidentally corrupt another's data. Security posture is correct by default, not by discipline.

3. **Infomaniak alignment.** Infomaniak offers managed K8s (free tier sufficient to start, SLA tier for production) and managed PostgreSQL as first-class products. Terraform via OpenStack provider is officially supported. The platform maps 1:1 to their product catalog — no workarounds needed except for container registry (GHCR fills the gap).

4. **LLM gateway as first shared service.** Both Ambrovia and HyperIdle consume LLM APIs. LiteLLM provides unified access, usage tracking, and provider fallback. Infomaniak's OpenAI-compatible AI API slots in as a provider alongside commercial APIs. This delivers immediate value to both tenants.

5. **Auth deferred.** Both apps have working, incompatible auth systems. Forcing a shared identity provider now would be a migration tax with no user-facing benefit. Defer until apps actually run on the shared cluster and a unifying identity layer makes sense.

6. **App repos own their deployment.** Each app repo defines its Helm chart and GitHub Actions. This repo defines what exists; app repos define what runs. Clean separation — infra PRs don't block app deploys and vice versa.

---

## Required Reading for `/architecture`

- `CLAUDE.md` — platform principles, architecture overview, tech stack
- `docs/concepts/platform.md` — this document
- Infomaniak docs: Managed Kubernetes, Managed Database, AI Tools API
- Terraform OpenStack provider docs
- LiteLLM deployment docs (Helm chart, proxy configuration)

## Sequencing Implications

The `/architecture` plan should sequence work as:

1. **Foundation** — Terraform state backend (OpenStack Swift object storage), provider configuration, networking (VPC, security groups)
2. **Cluster** — Managed K8s via Terraform, kubeconfig output, Cilium verification
3. **Database** — Managed PG via Terraform, tenant schemas + roles, connection string output
4. **Shared services** — Ingress controller, cert-manager, LLM gateway (LiteLLM)
5. **Tenant scaffolding** — Per-tenant namespace, network policies, resource quotas, RBAC, PG schema
6. **First workload** — HyperIdle deployment (already has Docker + VPS profile, easiest to containerize)
7. **Second workload** — Ambrovia migration (separate effort, requires containerizing Next.js + replacing Supabase)
