# Shared Auth

## Required Reading

- `CLAUDE.md` -- platform principles, architecture overview, tech stack
- `docs/concepts/platform.md` -- platform, tenant, shared service, isolation boundary definitions
- `docs/concepts/shared-auth.md` -- this document
- `docs/architecture/shared-auth.md` -- technical plan
- `docs/zitadel-app-integration.md` -- **practical guide**: how to wire an application to login (OIDC/PKCE, token gotchas, userinfo, SMTP, custom domains)

---

## Identity Provider

**Definition.** The identity provider is a Zitadel deployment in the `infra` namespace that provides centralized authentication (OAuth2/OIDC) for tenant applications. It is a shared service that tenants optionally integrate with.

**Essential properties.**
- Single Zitadel instance serving all tenants, deployed in the `infra` namespace
- Uses the existing self-hosted PostgreSQL with its own database (`zitadel`)
- Multi-tenant via Zitadel Organizations: each tenant application gets its own Organization
- Supports multiple authentication methods: email/password, magic links, social login (Google, Microsoft), passkeys
- Provides machine-to-machine authentication via service accounts and Personal Access Tokens (PATs)
- Accessible externally via ingress (users must reach the login UI from browsers)
- Accessible internally via ClusterIP for token validation and API calls

**What it is not.**
- **Not a mandate.** Tenants opt in. HyperIdle currently uses API keys and invite codes -- it is not required to adopt OIDC. The identity provider is available when a tenant needs it, not imposed on all tenants. We do not replace app-level auth models; we offer a shared option.
- **Not an authorization system.** The identity provider handles authentication (who are you?) and identity (what are your attributes?). Authorization (what can you do?) remains an application concern. Ambrovia's project-level RBAC (owner, collaborator) stays in Ambrovia. HyperIdle's user roles (admin, member) stay in HyperIdle. The identity provider does not enforce permissions inside tenant applications.
- **Not a user directory.** The identity provider stores authentication credentials and identity attributes (email, name, login methods). Application-specific user data (billing info, preferences, team membership) stays in each tenant's schema. The identity provider is not a master user database.
- **Not Supabase Auth.** Ambrovia currently uses Supabase Auth with deep integration (RLS policies, auth triggers, admin client). The identity provider replaces Supabase Auth's authentication layer, not its tight coupling with application data. Migrating Ambrovia requires decoupling auth from data -- a separate effort.
- **Not a custom auth framework.** We deploy Zitadel as-is. No wrappers, no abstraction layers, no custom middleware. Applications integrate directly with Zitadel's standard OIDC endpoints.

**Composition.**
- Hosts: Zitadel Organizations (one per tenant)
- Carries: user identities, authentication sessions, OIDC tokens, service account credentials
- References: PostgreSQL (own schema), ingress-nginx (external access), cert-manager (TLS)
- Referenced by: tenant applications (OIDC client configuration), cron jobs (service account PATs)

**Lifecycle.** Deployed (Helm via `terraform/main`) -> Configured (managed
Organizations, Projects, Applications, role definitions, and instance defaults
in `terraform/identity`) -> Active (tenants authenticate users) -> Updated
(reviewed identity plan and exact-plan apply).

Initial bootstrap creates the Zitadel instance and the credentials that let the
provider authenticate. Tenant structure is then declared in the identity root.
The console/API remain the runtime and break-glass interfaces, not the normal
path for managed structure.

**Authority.** This repo has two identity roots. `terraform/main` owns the
deployment (Helm, database, secrets, ingress, and network policy).
`terraform/identity` owns the four existing Organizations, three custom
Projects, four custom Applications, the nimmly role definition, and the two
selected instance defaults. Zitadel runtime remains authoritative for humans,
machine users, memberships, user grants, sessions, tokens, PATs/keys, external
IdPs, SMTP, branding, and optional objects not explicitly adopted.

---

## Organization (Zitadel)

**Definition.** A Zitadel Organization is the tenant isolation boundary within the identity provider. Each consuming application gets its own Organization, which contains its users, identity provider connections, and branding.

**Essential properties.**
- One Organization per tenant application (hyperidle, ambrovia, nimmly)
- Users within an Organization are isolated from other Organizations by default
- Each Organization can have its own identity provider connections (e.g., Ambrovia can enable Google OAuth while HyperIdle does not)
- Each Organization can have its own branding (login page appearance)
- Organizations contain Projects, which contain Applications (OIDC clients)

**What it is not.**
- **Not the same as a platform tenant.** A platform tenant is a K8s namespace + PG schema. A Zitadel Organization is a logical grouping within Zitadel. They correspond 1:1 by convention, but they are different things managed by different systems. The platform tenant exists before the Organization is created.
- **Not a team or account within an application.** Ambrovia has its own concept of "account" (personal or org). That is an application concept, not an identity provider concept. The Zitadel Organization for Ambrovia contains all Ambrovia users, regardless of which Ambrovia account they belong to.

**Composition.**
- Contains: Projects, Users, Identity Provider connections, Branding settings
- Contained by: Zitadel Instance
- Corresponds to: Platform tenant (by naming convention)

**Lifecycle.** Declared/imported through `terraform/identity` -> Active (users
authenticate through it) -> Decommissioned only through a separately designed
migration. The steady-state workflow intentionally refuses deletes.

**Authority.** `terraform/identity` owns Organization identity, name, and the
current default-Organization pointer. Users, membership, domains beyond the
provider-read primary domain, branding, and IdP connections remain runtime.

---

## Vocabulary

| Term | Definition | Aliases NOT to use |
|------|------------|--------------------|
| **identity provider** | The shared Zitadel deployment that handles authentication for tenant applications | auth service, auth server, IdP (acceptable as abbreviation only), SSO server, login service |
| **organization** (Zitadel) | The tenant isolation boundary within Zitadel; contains users, projects, and apps for one tenant | realm (Keycloak term), tenant (ambiguous with platform tenant), workspace |
| **project** (Zitadel) | A container within a Zitadel Organization that holds Applications and Roles for a specific software product | application (ambiguous), client (refers to the OIDC client, not the container) |
| **application** (Zitadel) | An OIDC/OAuth2 client registered in a Zitadel Project; represents a specific frontend or backend that authenticates users | client (acceptable in OIDC context), app (ambiguous with tenant application) |
| **service account** | A non-human Zitadel user for machine-to-machine authentication | machine user (deprecated Zitadel term), bot, API user |

---

## Contracts

### Cardinality
- One Zitadel instance per platform. No multi-instance.
- One Zitadel Organization per platform tenant. Naming matches tenant name.
- One Zitadel Project per application within a tenant. A tenant with separate frontend and backend may have multiple Applications within one Project.
- Zero or one Zitadel Organization per tenant (opt-in). Tenants that do not use the identity provider have no Organization.

### Closure
- Authentication methods are limited to what Zitadel supports natively: email/password, magic links (email OTP), social login (Google, Microsoft, Apple, GitHub, etc.), passkeys/WebAuthn, service accounts with PATs or client credentials.
- No custom authentication method plugins.

### Mutability
- The Zitadel Helm deployment (schema, secrets, Helm values) is immutable except through Terraform changes to this repo.
- Adopted Organizations, Projects, Applications, role definitions, and selected
  defaults change only through the identity workflows on `main`. Users,
  memberships, grants, sessions, and credentials remain mutable runtime state.
- External domain (`auth.<domain>`) is set at deployment time and should not change (OIDC issuer URLs depend on it).

### Identity
- Zitadel assigns opaque IDs to users, organizations, projects, and applications.
- Tenant applications receive OIDC tokens with Zitadel user IDs as the `sub` claim. Applications map these to their internal user records.
- The user's email address is the human-readable identifier but is NOT the identity key (email can change, sub cannot).

### Hard Rules
- Zitadel's schema is isolated from tenant schemas. The `zitadel` PG role cannot access tenant schemas and vice versa.
- Zitadel's external domain must match the OIDC issuer. Changing the domain after tokens are issued breaks token validation for all consumers.
- Zitadel must be accessible from the internet (users need to reach the login UI from their browsers). This is different from LiteLLM, which is internal-only.
- The identity provider does NOT store application data. It stores authentication credentials and identity attributes only.
- Tenant applications own the mapping between Zitadel user IDs and application user records. The identity provider does not manage this mapping.

---

## Rationale

This shape was chosen because:

1. **Opt-in, not mandate.** HyperIdle explicitly does not want OAuth/SSO. Forcing it would be migration tax with no benefit. The identity provider is a shared service that tenants use when they need it -- same pattern as the LLM gateway.

2. **Zitadel over alternatives.** Single Go binary, ~300-500MB idle, uses existing PostgreSQL (no Redis), first-class multi-tenant model (Instance > Organization > Project > Application), Apache 2.0 license. Compared to alternatives: Keycloak needs 1.2-1.5GB + separate JVM (won't fit the 12GB cluster alongside everything else); Authentik needs ~750MB + Redis; Ory Kratos+Hydra is ~500-800MB with no native multi-tenant in OSS. Zitadel fits the resource budget and the multi-tenant architecture.

3. **Authentication, not authorization.** Keeping authorization in each application avoids the complexity of a centralized permission system. Ambrovia's project roles and HyperIdle's team roles have different semantics -- forcing them into a shared model would be wrong. The identity provider answers "who is this user?" and each application answers "what can they do?"

4. **Standard OIDC integration.** Applications integrate via standard OIDC flows, not a proprietary API. This means standard libraries work (NextAuth.js for Ambrovia, any OIDC library for HyperIdle if it ever opts in, Supabase-compatible auth for Nimmly's migration).

5. **Organization-per-tenant isolation.** Maps directly to the platform's tenant isolation model. Users in the Ambrovia Organization cannot authenticate to the Nimmly application, and vice versa. This is structural, not advisory.

---

## Deferred Concepts

### Cross-tenant identity (single sign-on across applications)

**Status:** Deferred. Not part of this deployment.

**Rationale.** Zitadel supports Project Grants (granting one Organization's project to another Organization), which could enable users in one tenant to access another tenant's application. This is a B2B multi-tenancy feature. We have no use case for it now -- all three applications serve different user populations. If a user needs access to both Ambrovia and Nimmly, they create separate accounts in each. Revisit if user overlap becomes significant.

### Zitadel Actions (custom logic hooks)

**Status:** Deferred. Not part of this deployment.

**Rationale.** Zitadel Actions allow running custom code on auth events (pre-login, post-login, token customization). Ambrovia's user initialization flow (create account + billing + user records on first login) could be implemented as a Zitadel Action or as application-side logic triggered by the OIDC callback. The application-side approach is simpler for v1 and doesn't couple business logic to the identity provider.
