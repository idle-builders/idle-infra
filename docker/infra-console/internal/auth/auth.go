// Package auth verifies callers against Zitadel and admits platform operators.
//
// Tokens go through USERINFO, not JWKS: Zitadel's access tokens and PATs are
// opaque JWE and cannot be verified offline (the storage broker hit the same
// constraint). USERINFO is also revocation-aware.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OperatorRole is the Zitadel membership that admits a caller.
//
// IAM_OWNER, not ORG_OWNER: one shared instance holds an Organization per
// tenant, so a tenant owning their own org must not read every other tenant.
// Not an OAuth scope either — USERINFO never returns granted scopes, so a
// scope-based design refused every caller.
const OperatorRole = "IAM_OWNER"

// Principal is a verified caller who is a platform operator.
type Principal struct {
	Subject string
	Email   string
	// Roles are the caller's Zitadel memberships, kept for the audit line.
	Roles []string
}

type principalKey struct{}

// WithPrincipal attaches a verified caller to a context so handlers can name
// them (the row-read audit line). It grants nothing: the middleware never
// reads one back to decide admission.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the verified caller, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Verifier checks bearer tokens against Zitadel.
type Verifier struct {
	issuer string
	hc     *http.Client
	log    *slog.Logger

	mu       sync.RWMutex
	cache    map[string]cacheEntry
	cacheTTL time.Duration
}

type cacheEntry struct {
	principal Principal
	at        time.Time
}

func NewVerifier(issuer string, log *slog.Logger) *Verifier {
	return &Verifier{
		issuer: strings.TrimRight(issuer, "/"),
		hc:     &http.Client{Timeout: 10 * time.Second},
		log:    log,
		cache:  map[string]cacheEntry{},
		// Short, or caching would defeat USERINFO's revocation-awareness. One
		// minute collapses a page's parallel requests into one round trip.
		cacheTTL: time.Minute,
	}
}

// Configured reports whether authentication is enforced. Unconfigured is the
// local development mode and must never be the production path.
func (v *Verifier) Configured() bool { return v != nil && v.issuer != "" }

type userinfo struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

// membershipsResponse is Zitadel's /auth/v1/memberships/me/_search reply: the
// memberships of whoever presented the token.
type membershipsResponse struct {
	Result []struct {
		Roles []string `json:"roles"`
	} `json:"result"`
}

// Verify exchanges a token for a principal.
func (v *Verifier) Verify(ctx context.Context, token string) (Principal, error) {
	if !v.Configured() {
		return Principal{}, fmt.Errorf("verifier not configured")
	}
	if token == "" {
		return Principal{}, fmt.Errorf("empty token")
	}

	if p, ok := v.cached(token); ok {
		return p, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.issuer+"/oidc/v1/userinfo", nil)
	if err != nil {
		return Principal{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := v.hc.Do(req)
	if err != nil {
		return Principal{}, fmt.Errorf("userinfo request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Principal{}, fmt.Errorf("token rejected (status %d)", resp.StatusCode)
	}

	var info userinfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return Principal{}, fmt.Errorf("decode userinfo: %w", err)
	}
	if info.Sub == "" {
		return Principal{}, fmt.Errorf("userinfo returned no subject")
	}

	roles, err := v.memberships(ctx, token)
	if err != nil {
		return Principal{}, err
	}

	principal := Principal{Subject: info.Sub, Email: info.Email, Roles: roles}
	v.store(token, principal)
	return principal, nil
}

// IsOperator reports whether the caller holds the operator membership.
func (p Principal) IsOperator() bool {
	for _, r := range p.Roles {
		if r == OperatorRole {
			return true
		}
	}
	return false
}

// memberships asks Zitadel what the CALLER is, using the caller's own token.
// This reuses Zitadel's existing admin model: an operator already holds
// IAM_OWNER, and a tenant's end users hold no membership at all.
func (v *Verifier) memberships(ctx context.Context, token string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		v.issuer+"/auth/v1/memberships/me/_search", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("memberships request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("memberships rejected (status %d)", resp.StatusCode)
	}

	var out membershipsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode memberships: %w", err)
	}

	var roles []string
	for _, m := range out.Result {
		roles = append(roles, m.Roles...)
	}
	return roles, nil
}

func (v *Verifier) cached(token string) (Principal, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	entry, ok := v.cache[token]
	if !ok || time.Since(entry.at) > v.cacheTTL {
		return Principal{}, false
	}
	return entry.principal, true
}

func (v *Verifier) store(token string, p Principal) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Bound the map: tokens rotate, and an unbounded cache keyed by token is a
	// slow memory leak.
	if len(v.cache) > 512 {
		v.cache = map[string]cacheEntry{}
	}
	v.cache[token] = cacheEntry{principal: p, at: time.Now()}
}

// SessionCookie is the console's session cookie name. The __Host- prefix
// requires Secure, Path=/ and no Domain, which browsers enforce.
const SessionCookie = "__Host-infra-console"

// Middleware verifies every request and admits only platform operators. An
// unconfigured verifier passes everything through — local development only;
// main refuses to start that way without AUTH_DISABLED.
func Middleware(v *Verifier, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !v.Configured() {
				next.ServeHTTP(w, r)
				return
			}

			// Every request is authenticated from its own credential; no path
			// trusts an identity placed on the context by earlier code. The
			// MCP adapter copies the caller's header onto its synthesized
			// request, so it needs no exemption — and an exemption would be a
			// bypass waiting to be reached another way.
			token := bearer(r.Header.Get("Authorization"))
			if token == "" {
				if c, err := r.Cookie(SessionCookie); err == nil {
					token = c.Value
				}
			}
			if token == "" {
				problem(w, http.StatusUnauthorized, "Authentication required",
					"present a bearer token or an authenticated session")
				return
			}

			principal, err := v.Verify(r.Context(), token)
			if err != nil {
				if log != nil {
					// Never log the token, only that one was rejected.
					log.Warn("token rejected", "path", r.URL.Path, "err", err.Error())
				}
				problem(w, http.StatusUnauthorized, "Invalid credential", "")
				return
			}

			// One gate for everything behind this middleware: the console is
			// read-only and platform-wide, so there is no coherent middle
			// ground between "may operate the platform" and "may not".
			if !principal.IsOperator() {
				if log != nil {
					log.Info("not an operator",
						"sub", principal.Subject, "path", r.URL.Path,
						"roles", principal.Roles, "required", OperatorRole)
				}
				problem(w, http.StatusForbidden, "Not a platform operator",
					"this console requires the "+OperatorRole+" membership in Zitadel")
				return
			}

			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

func bearer(header string) string {
	const prefix = "Bearer "
	if len(header) >= len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

func problem(w http.ResponseWriter, status int, title, detail string) {
	body := map[string]any{"type": "about:blank", "title": title, "status": status}
	if detail != "" {
		body["detail"] = detail
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// SecureCookie builds a session cookie. Secure is unconditional, not inferred:
// TLS terminates at the Cloudflare edge, so r.TLS is nil at the pod while the
// browser is on HTTPS — inferring would drop the flag exactly when it matters.
func SecureCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}
