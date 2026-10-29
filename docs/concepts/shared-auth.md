# Shared authentication

Zitadel gives applications a common OIDC identity provider without requiring
each application to operate its own login service. Applications opt in and
retain their own authorization, user records and workspace membership rules.

## Identity boundaries

A deployment runs its own Zitadel instance, backed by a separate PostgreSQL role
and schema. Each participating platform tenant can have a Zitadel organization
with projects and OIDC applications. A project groups clients and roles for a
software product; an OIDC application identifies a browser or server client.

A Zitadel organization is different from an application's team or workspace.
Those business concepts remain application-owned. Cross-organization login and
project grants require deliberate policy; the organization naming convention
alone is not proof of client isolation. Verify that an unintended organization's
user cannot sign in to a tenant's client.

## Responsibility

The framework's platform module deploys Zitadel, its database integration,
secrets, ingress and network policy. Operators supply their issuer domain,
initial administrator and credentials in their deployment repository.

Organizations, projects, client registrations and identity policy are configured
by the operator. The example does not include an identity-management OpenTofu
root. If an operator adds one, keep its state ownership separate from runtime
users, memberships, sessions and tokens. Inspect plans for destructive identity
changes before applying them.

Applications use standard OIDC libraries, validate issuer, audience and token
expiry, and map identity subjects to their own application accounts. Backend
APIs need tokens intended for that API. Never interpret an identity attribute as
proof of permission to an application resource.

## Operational requirements

Keep the external issuer stable: changing its domain affects token validation
for every consumer. Verify discovery and JWKS reachability during changes.
Configure SMTP or another supported verification flow before depending on email
verification, password recovery or email OTP. These are deployment choices, not
assumptions about every cluster.

Keep client secrets, machine-user keys, tokens and initial administrator secrets
out of Git and logs. Service integrations need scoped identities; a console
credential should not also be a general application administrator credential.

See the [Zitadel skill](../../skills/zitadel/SKILL.md) for operational procedures
and the [platform concept](platform.md) for shared-service ownership.
