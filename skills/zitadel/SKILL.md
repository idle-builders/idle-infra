---
name: zitadel
description: Work with the shared Zitadel identity provider — add login to an application, create orgs/projects/apps, debug rejected tokens and missing claims, handle email verification without SMTP, and manage the Zitadel deployment itself.
---


Resolve the selected **deployment** with `scripts/infra-root.sh` from this plugin.
Run commands through `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh` when outside it.
`INFRA_REPO` overrides selection; otherwise the current deployment or the explicitly
selected default is used. The installed plugin checkout never selects a cluster.
Deployment settings are in `terraform/main/framework.tf` and `terraform/main/main.tf`;
shared implementations are under `framework/terraform/modules/`. Read both when
following an implementation reference below. Credentials and state belong to the deployment.

# Zitadel

> **Working outside the infra repo?** Prefix commands with
> `"${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh"` — it enters the repo and loads
> credentials for that one command. Paths below are relative to that repo.

The shared identity provider, deployed as a Helm release in the `infra`
namespace with its own PostgreSQL database. Its public hostname — and therefore
the OIDC issuer — is `var.identity_domain` in `terraform/main/variables.tf`.
Read it once and use it throughout; it is written as `<issuer>` below.

```bash
grep -A4 'variable "identity_domain"' terraform/main/variables.tf
```

**The model:** Zitadel does authentication and identity only. It issues a stable
`sub`. The **application** owns the mapping from that `sub` to its own user,
workspace, and account records. Authorization is entirely the app's concern.
Zitadel stores no application data.

Reference: `docs/zitadel-app-integration.md` (the gotcha-laden practical guide),
`framework/docs/concepts/shared-auth.md` (the model and contracts).

**Production state:** the existing managed inventory has already been adopted
into the encrypted `terraform/identity` state, and scheduled read-only drift
detection is active. The one-time adoption workflow is not a normal change
path. Use the identity plan and exact-plan apply workflows for managed changes.

## Rules

- Never paste a signed token into a third-party decoder site. It is a bearer
  credential. Decode the payload locally and share only the claims.
- Never print an access token, client secret, masterkey, or admin password.
- Managed Organizations, Projects, Applications, role definitions, and selected
  defaults live in `terraform/identity`. Change them through its manually
  dispatched plan/apply workflows. Humans, machine users, memberships, grants,
  sessions, keys/PATs, SMTP, and unadopted optional objects remain runtime.
  Console edits to managed structure are break-glass and must be transcribed
  back to HCL immediately.
- Use an incognito window when testing logins. Zitadel reuses its session cookie
  and will silently log you in as whoever was there before — usually the
  instance admin.

## Add login to an application

Declare the required Applications in `terraform/identity`; do not create an
untracked production client in the Console. A new integration normally needs
the following two resource shapes, although the live adopted inventory includes
intentional exceptions documented in the identity root.

The steady-state plan guard is closed over the current 14 addresses and rejects
creates, deletes, and replacements. Adding a new Organization, Project,
Application, or role therefore requires an explicit migration design that
updates the inventory and guard; do not weaken or bypass the guard during an
ordinary apply.

1. **Login client** — type *User Agent* (SPA, public + PKCE) or *Web*
   (confidential). Set the redirect URIs. Turn on Development Mode only if a
   redirect URI is plain `http://localhost`. Record the **Client ID** — the
   field labelled Client ID in the OIDC config, not the Resource Id at the top
   of the page.
2. **API application** — type *API*. This is the backend resource server. Its
   **Client ID is the app's `aud`**.

An API-type app cannot run a login. Pointing `/authorize` at one returns
`Errors.App.NotFound`.

Then, on the login client: **set Auth Token Type to JWT.** The default is an
opaque JWE that a resource server cannot verify locally.

The app requests:

```
scope=openid profile email urn:zitadel:iam:org:project:id:<PROJECT_ID>:aud
```

without which the access token's `aud` will not contain the API app.

The app's environment contract:

```
ZITADEL_ISSUER       = https://<issuer>
ZITADEL_JWKS_URI     = https://<issuer>/oauth/v2/keys        (default: ${issuer}/oauth/v2/keys)
ZITADEL_USERINFO_URI = https://<issuer>/oidc/v1/userinfo     (default: ${issuer}/oidc/v1/userinfo)
ZITADEL_AUDIENCE     = <API app client id>
```

`docs/zitadel-app-integration.md` carries the canonical version of this contract
and the endpoint paths — check it if a path stops resolving.

## Verification recipe for a resource server

1. Verify the access-token JWT locally against JWKS — signature, `iss`, `aud`,
   `exp`. No network call. Extract `sub`.
2. Map `sub` to the app's own records.
3. Only when the verified email is needed, call `userinfo` with the bearer
   token. Treat **any** failure — non-200, network error, missing or false
   `email_verified` — as a deny. Fail closed.

## Debug a rejected token

| Symptom | Cause |
|---------|-------|
| Token payload will not decode; header says `A256GCMKW` / `A256GCM` | It is a JWE. Auth Token Type is not JWT on the **login client**. |
| Signature verifies but `aud` lacks the API client id | The project-audience scope was not requested, or `ZITADEL_AUDIENCE` is set to the wrong id. Decode a real token and look — Zitadel may emit the project id rather than the client id depending on settings. |
| `email` missing from the token | Correct and by design. Access tokens carry only `sub`, `aud`, `iss`, `exp`, `iat`, `client_id`, `jti`. Call `userinfo`. |
| "User Info inside ID Token" is on and it still doesn't help | That puts claims in the ID token, which the resource server never receives. Use `userinfo`. |
| `/authorize` returns 400 `Errors.App.NotFound` | The client id is an API app, not a login client. |
| A PAT or default token will not verify against JWKS | Those are opaque JWE by design and are not JWKS-verifiable. |

Check the issuer is reachable and serving the expected keys:

```bash
curl -s https://<issuer>/.well-known/openid-configuration | head
curl -s https://<issuer>/oauth/v2/keys
```

The discovery document is authoritative for the endpoint paths — read them from
it rather than assuming.

`docs/runbooks/nimmly-deploy.md` includes a JWKS stability check — fetch the
endpoint repeatedly and confirm the key set does not flap during cold start or
rotation.

## Email verification — there is no SMTP

No SMTP is configured on the instance. A self-registered user cannot receive a
verification email and stays `email_verified = false`, so any app requiring a
verified email denies them. Magic-link and email-OTP login also do not work.

Pick one before relying on self-serve signup:

- **Social login** — enable Google or Microsoft as an org IdP. The provider
  asserts a pre-verified email, no SMTP needed. Recommended where the domain is
  Google Workspace or M365.
- **Configure SMTP** on the instance via a transactional provider.
- **Manual verification** — an admin creates the user and flips the email to
  verified. Testing only.

## Custom domains

The OIDC issuer is an **instance-level** property — the instance's primary
custom domain *is* its issuer. Organization-level domains do not give a separate
issuer.

Apps are currently modelled as Organizations inside one shared instance. A
per-app auth domain means a virtual instance per app, routed by Host header —
a structural change, cheapest to make before an app has real users.

## The deployment itself

Terraform-managed: the Helm release, the identity database and its roles with
`PUBLIC` connect revoked, the masterkey and DB credential secrets, the ingress,
and network policies for tenant-to-Zitadel, ingress-to-Zitadel, and Zitadel's
internet egress. Read the `module.platform.helm_release.zitadel` block in
`terraform/main/main.tf` for the chart version and settings.

```bash
kubectl get pods -n infra -l app.kubernetes.io/name=zitadel
kubectl logs -n infra -l app.kubernetes.io/name=zitadel --tail=50
kubectl get ingress -n infra
```

Known operational gotchas: `disable_webhooks`, the v2 login path, and
re-initialization behaviour. Check `helm/values/zitadel.yaml` and the
architecture doc before changing chart values. The masterkey encrypts data at
rest and must be exactly 32 characters — changing it makes existing data
unreadable.

## Evidence

Deployment changes are proven live:

```bash
kubectl get pods -n infra -l app.kubernetes.io/name=zitadel   # Ready
curl -sS https://<issuer>/.well-known/openid-configuration | head
curl -sSI https://<issuer>                                    # reachable through the tunnel
```

An **application integration** is proven by a real login producing a token that
verifies:

- complete a login in an incognito window
- decode the access token locally and confirm `iss`, `aud` contains the API
  app's client id, and `sub` is present
- call `userinfo` with that token and confirm `email_verified` is true
- call the app's own authenticated endpoint and confirm it accepts the token

Do not report an integration as working on the strength of Console
configuration alone — a token has to come out and be accepted end to end.

Deployment and managed structural changes ship with `infra-apply`. Runtime-only
Console changes follow `docs/runbooks/zitadel-identity.md`; a break-glass edit
to managed structure is incomplete until HCL is reconciled and a no-op plan is
proven.
