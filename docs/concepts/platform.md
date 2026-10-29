# Shared infrastructure platform

Idle Infra supplies a common foundation for several applications on a Kubernetes
cluster. Operators declare the deployment in their own repository; application
repositories own their workloads and release pipelines.

## Ownership

| Owner | Responsibility |
| --- | --- |
| Framework | Reusable OpenTofu modules, Helm values, runtime components, API contracts and operational skills |
| Deployment | Cloud accounts, cluster configuration, state backend, credentials, domains, tenant inventory and dependency pins |
| Application | Images, charts, migrations, release pipeline, user authorization and business data |

Framework CI validates and publishes reusable code. Updating the framework does
not apply a change to an operator's deployment.

## Tenants and shared services

A platform tenant is an application boundary: a Kubernetes namespace, resource
quota, scoped deployer role, default-deny network policies and a PostgreSQL schema
with its owning role. A tenant can have several workloads or none. It is separate
from a human user, workspace or organization inside an application.

PostgreSQL, Zitadel, ingress, certificates and the storage broker are shared
services. The optional console exposes read-only operational information through
its UI, HTTP API and MCP tools. Optional telemetry supplies bounded historical
evidence alongside live sources. The framework also includes optional KEDA
integration for application-specific scaling definitions.

Applications can use shared OIDC authentication while keeping their own
permission model. The storage broker resolves workload identity and checks
configured transfer or governed-object authority; it does not decide workspace
membership or business permissions for an application.

## Isolation and its limits

Tenant boundaries are enforced through Kubernetes RBAC, network policy, quota
and PostgreSQL grants. Namespace separation requires a CNI that enforces the
policies; a new provider must prove both permitted and denied traffic paths.
A cluster administrator and the shared database administrator retain privileged
access. These boundaries do not provide separate control planes or dedicated
hardware for mutually untrusted operators.

On the current OpenStack storage implementation, raw S3 credentials can have
project-wide authority. The broker is the application-facing authorization
boundary; separate bucket names alone do not create credential isolation.
Applications that receive raw credentials need an explicit review of that wider
authority. See [governed storage](governed-object-storage.md).

## Change and evidence

Infrastructure changes are reviewed as plans in the deployment repository and
applied through one serialized writer per state. Source revisions and runtime
images are pinned. State-address migrations belong beside the deployment that
owns those addresses; changing a source pin does not roll back state.

A successful plan proves configuration consistency, not operational isolation.
Deployments must verify readiness, restore behavior, scoped deployment rights,
network policy, schema grants and storage allow/deny cases. Fresh cloud bootstrap
and new provider support need their own end-to-end evidence.

## Health evidence and remediation

Evidence identifies its source, observation time, coverage and availability.
An unavailable source must remain visibly unavailable, rather than becoming a
plausible zero. Optional telemetry is bounded evidence, not a complete audit log.

Read-only observation does not authorize a repair. Changes to tenant data,
schema definitions or application projections stay with the owning application;
platform operators own engine health and reviewed infrastructure repairs. See
[database index stewardship](database-index-stewardship.md).

Continue with [setup](../getting-started.md) or the [provider plan](../providers.md).
