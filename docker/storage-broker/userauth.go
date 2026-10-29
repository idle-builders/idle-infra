package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// UserVerifier validates an end-user's Zitadel access token by calling the IdP's
// OIDC **userinfo** endpoint (online validation) and returns the subject.
//
// Why userinfo and not offline JWKS verification: Zitadel's default access tokens
// — and all Personal Access Tokens — are opaque/encrypted (JWE), not signed JWTs
// (JWS), so they cannot be verified offline against the JWKS. Userinfo accepts any
// valid access token (opaque, PAT, or JWT), returns its claims (incl. `sub` and
// the `urn:zitadel:*` org/role claims used later for membership-based scope), and
// is revocation-aware: a revoked token yields a non-200 immediately.
//
// Userinfo does not say who a token was minted for, and every app shares one
// issuer. A tenant whose policy pins user token clients therefore also requires
// the token to be a JWT verified offline against the issuer's JWKS whose
// client_id (or azp) is on that list; userinfo still runs after it, for
// revocation. ID tokens are JWTs signed by the same keys, so tokens carrying
// ID-token-only claims are refused locally rather than trusting userinfo to.
//
// The OIDC provider is discovered in the background (for the userinfo endpoint);
// Ready() / readyz stay false until discovery succeeds — fail closed.
type UserVerifier struct {
	issuer string

	mu       sync.RWMutex
	provider *oidc.Provider
	ready    bool
}

func NewUserVerifier(ctx context.Context, c *Config) *UserVerifier {
	uv := &UserVerifier{issuer: c.OIDCIssuer}
	go uv.initLoop(ctx)
	return uv
}

func (uv *UserVerifier) initLoop(ctx context.Context) {
	for {
		if err := uv.tryInit(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (uv *UserVerifier) tryInit(ctx context.Context) error {
	provider, err := oidc.NewProvider(ctx, uv.issuer)
	if err != nil {
		return err
	}
	uv.mu.Lock()
	uv.provider = provider
	uv.ready = true
	uv.mu.Unlock()
	return nil
}

func (uv *UserVerifier) Ready() bool {
	uv.mu.RLock()
	defer uv.mu.RUnlock()
	return uv.ready
}

// Verify validates the access token via the userinfo endpoint and returns the
// (validated) subject. A non-200 from userinfo (expired/revoked/invalid token)
// surfaces as an error. With clients non-empty the token must also be a JWT
// access token from the issuer minted by one of them, naming the same subject.
func (uv *UserVerifier) Verify(ctx context.Context, raw string, clients []string) (sub string, err error) {
	uv.mu.RLock()
	p := uv.provider
	ready := uv.ready
	uv.mu.RUnlock()
	if !ready || p == nil {
		return "", fmt.Errorf("user verifier not ready")
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var jwtSub string
	if len(clients) > 0 {
		// The verifier enforces signature, issuer, expiry and the discovered
		// algorithms; which client minted the token is checked below.
		tok, err := p.Verifier(&oidc.Config{SkipClientIDCheck: true}).Verify(cctx, raw)
		if err != nil {
			return "", fmt.Errorf("client-pinned tenant needs a JWT access token: %w", err)
		}
		var claims struct {
			ClientID string `json:"client_id"`
			AZP      string `json:"azp"`
			Nonce    string `json:"nonce"`
			AtHash   string `json:"at_hash"`
			CHash    string `json:"c_hash"`
		}
		if err := tok.Claims(&claims); err != nil {
			return "", fmt.Errorf("decode token claims: %w", err)
		}
		if claims.Nonce != "" || claims.AtHash != "" || claims.CHash != "" {
			return "", fmt.Errorf("token is an ID token, not an access token")
		}
		// client_id names the minting client; azp is only a fallback for a
		// token that does not carry it.
		minter := claims.ClientID
		if minter == "" {
			minter = claims.AZP
		}
		if !contains(clients, minter) {
			return "", fmt.Errorf("token client %q (azp %q) not accepted for this tenant",
				claims.ClientID, claims.AZP)
		}
		if tok.Subject == "" {
			return "", fmt.Errorf("token has no subject")
		}
		jwtSub = tok.Subject
	}

	info, err := p.UserInfo(cctx, oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: raw,
		TokenType:   "Bearer",
	}))
	if err != nil {
		return "", err
	}
	if info.Subject == "" {
		return "", fmt.Errorf("userinfo returned no subject")
	}
	if jwtSub != "" && jwtSub != info.Subject {
		return "", fmt.Errorf("token subject does not match userinfo subject")
	}
	return info.Subject, nil
}
