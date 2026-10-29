// Auth tests against a real Zitadel.
//
// No stub: token verification depends on Zitadel's actual behaviour, including
// two things a fake would never have shown. Zitadel resolves its instance by
// hostname, so calling 127.0.0.1 when ExternalDomain is localhost returns
// "Instance not found"; and a PAT is opaque JWE, verifiable only through
// USERINFO, never offline.
package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set; run integration/up.sh first", key)
	}
	return v
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestRealTokenVerifies(t *testing.T) {
	v := NewVerifier(env(t, "INTEG_ZITADEL_URL"), nil)
	pat := env(t, "INTEG_ZITADEL_TOKEN")

	p, err := v.Verify(ctx(t), pat)
	if err != nil {
		t.Fatalf("Verify against real Zitadel: %v", err)
	}
	if p.Subject == "" {
		t.Fatal("a verified principal must carry a subject")
	}
}

func TestRealInvalidTokenIsRejected(t *testing.T) {
	v := NewVerifier(env(t, "INTEG_ZITADEL_URL"), nil)

	if _, err := v.Verify(ctx(t), "not-a-real-token"); err == nil {
		t.Fatal("a garbage token must be rejected by real Zitadel")
	}
	if _, err := v.Verify(ctx(t), ""); err == nil {
		t.Fatal("an empty token must be rejected without a round trip")
	}
}

// The instance is bound to its ExternalDomain. Reaching it by IP fails, which
// is a real deployment hazard worth pinning.
func TestRealInstanceIsResolvedByHostname(t *testing.T) {
	base := env(t, "INTEG_ZITADEL_URL")
	if !strings.Contains(base, "localhost") {
		t.Skip("integration Zitadel is not on localhost")
	}
	byIP := strings.Replace(base, "localhost", "127.0.0.1", 1)

	v := NewVerifier(byIP, nil)
	if _, err := v.Verify(ctx(t), env(t, "INTEG_ZITADEL_TOKEN")); err == nil {
		t.Fatal("reaching Zitadel by IP should fail instance resolution; if this passes, the hazard is gone and the note in login.go can be relaxed")
	}
}

// USERINFO is revocation-aware, so the cache must be short. This proves the
// cache exists by counting real round trips indirectly: a second Verify of the
// same token must not fail even if the network is slow, and must be fast.
func TestRealVerificationIsCached(t *testing.T) {
	v := NewVerifier(env(t, "INTEG_ZITADEL_URL"), nil)
	pat := env(t, "INTEG_ZITADEL_TOKEN")
	c := ctx(t)

	if _, err := v.Verify(c, pat); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	start := time.Now()
	if _, err := v.Verify(c, pat); err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("second verify took %s — it should have hit the cache", elapsed)
	}
}

// Authority comes from the caller's Zitadel membership. The seeded PAT belongs
// to the bootstrap machine user, which holds IAM_OWNER.
//
// This replaces a test that asserted a valid token WITHOUT the console's scopes
// was refused. That test passed for the wrong reason: no Zitadel credential can
// ever carry those scopes, because OIDC UserInfo returns claims about the user
// and never the granted scope. It proved the deny half of a model whose grant
// half did not exist.
func TestRealOperatorIsAdmitted(t *testing.T) {
	v := NewVerifier(env(t, "INTEG_ZITADEL_URL"), nil)

	principal, err := v.Verify(ctx(t), env(t, "INTEG_ZITADEL_TOKEN"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !principal.IsOperator() {
		t.Fatalf("the seeded credential holds %v, want %s — the console would refuse every caller",
			principal.Roles, OperatorRole)
	}

	h := Middleware(v, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("reached the handler"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/platform", nil)
	req.Header.Set("Authorization", "Bearer "+env(t, "INTEG_ZITADEL_TOKEN"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an operator: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reached the handler") {
		t.Error("an operator must reach the handler")
	}
}

// The other half, and the one that matters for a shared identity provider: a
// real, valid Zitadel credential with no membership is refused. Tenant end
// users live in this same instance.
func TestRealNonOperatorIsForbidden(t *testing.T) {
	token := env(t, "INTEG_ZITADEL_PLAIN_TOKEN")
	v := NewVerifier(env(t, "INTEG_ZITADEL_URL"), nil)

	principal, err := v.Verify(ctx(t), token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if principal.Subject == "" {
		t.Fatal("the token must authenticate; it is the authorization that must fail")
	}
	if principal.IsOperator() {
		t.Fatalf("a plain tenant user holds %v; it must hold no operator membership", principal.Roles)
	}

	h := Middleware(v, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("reached the handler"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/platform", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a valid non-operator credential", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "reached the handler") {
		t.Fatal("a non-operator reached the handler")
	}
}

func TestRealMissingCredentialIs401(t *testing.T) {
	v := NewVerifier(env(t, "INTEG_ZITADEL_URL"), nil)
	h := Middleware(v, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/platform", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q", ct)
	}
}

// The sign-in flow discovers its endpoints from the real issuer.
func TestRealDiscoveryFindsAuthorizationEndpoints(t *testing.T) {
	base := env(t, "INTEG_ZITADEL_URL")
	l := NewLogin(base, "some-client-id", "http://localhost:9999", nil)

	d, err := l.discover(ctx(t))
	if err != nil {
		t.Fatalf("discovery against real Zitadel: %v", err)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		t.Fatalf("discovery returned %+v", d)
	}
	if !strings.HasPrefix(d.AuthorizationEndpoint, base) {
		t.Errorf("authorization endpoint %q should be on the issuer", d.AuthorizationEndpoint)
	}
}

// A real redirect, built from real discovered endpoints, carrying PKCE.
func TestRealLoginRedirectUsesPKCE(t *testing.T) {
	l := NewLogin(env(t, "INTEG_ZITADEL_URL"), "console-client", "http://localhost:9999", nil)

	rec := httptest.NewRecorder()
	l.HandleLogin(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "code_challenge_method=S256") {
		t.Errorf("redirect lacks a PKCE challenge: %s", loc)
	}
	if strings.Contains(loc, "client_secret") {
		t.Error("a PKCE public client must not send a secret")
	}
}

// The authorization URL must request Zitadel's own project in the token's
// audience. Without the reserved scope the session token authenticates but
// Zitadel's Auth API refuses it, so /auth/v1/memberships/me/_search returns 401
// and every browser login fails authorization — the console would sign a user
// in and then refuse them. PATs are unaffected, which is why the agent path
// worked while the browser path would not have.
func TestLoginRequestsTheZitadelAudienceScope(t *testing.T) {
	issuer := env(t, "INTEG_ZITADEL_URL")
	l := NewLogin(issuer, "some-client-id", "https://console.example.com", nil)
	if !l.Configured() {
		t.Fatal("login should be configured with an issuer and client id")
	}

	rec := httptest.NewRecorder()
	l.HandleLogin(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatalf("expected a redirect to the authorize endpoint, got %d", rec.Code)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("authorize URL: %v", err)
	}
	scope := u.Query().Get("scope")
	if !strings.Contains(scope, "urn:zitadel:iam:org:project:id:zitadel:aud") {
		t.Errorf("scope = %q, missing the Zitadel audience scope — the session "+
			"token would be refused by Zitadel's own API", scope)
	}
	for _, want := range []string{"openid", "profile", "email"} {
		if !strings.Contains(scope, want) {
			t.Errorf("scope = %q, missing %s", scope, want)
		}
	}
	if u.Query().Get("code_challenge_method") != "S256" {
		t.Error("PKCE must use S256")
	}
}
