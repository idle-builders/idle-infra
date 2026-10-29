package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Login implements the browser sign-in flow: OIDC authorization code with
// PKCE, against the same issuer the middleware verifies tokens with.
//
// PKCE with a public client means no client secret exists to store or rotate.
// That is the right shape here: the console is a first-party browser app, and a
// secret in a public client is a secret in a browser.
type Login struct {
	issuer      string
	clientID    string
	redirectURI string
	hc          *http.Client
	log         *slog.Logger

	mu        sync.RWMutex
	endpoints *discovery
	fetchedAt time.Time
}

type discovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// NewLogin builds the flow. consoleURL is the public origin, used to form the
// redirect URI that must be registered on the Zitadel application.
func NewLogin(issuer, clientID, consoleURL string, log *slog.Logger) *Login {
	redirect := ""
	if consoleURL != "" {
		redirect = strings.TrimRight(consoleURL, "/") + "/auth/callback"
	}
	return &Login{
		issuer:      strings.TrimRight(issuer, "/"),
		clientID:    clientID,
		redirectURI: redirect,
		hc:          &http.Client{Timeout: 10 * time.Second},
		log:         log,
	}
}

// Configured reports whether browser sign-in is available. Without it the
// console still serves bearer-token callers (agents), which is why this is
// separate from the verifier being configured.
func (l *Login) Configured() bool {
	return l != nil && l.issuer != "" && l.clientID != "" && l.redirectURI != ""
}

// pkceCookie carries the one-time state and verifier between the redirect and
// the callback. Keeping them in an HttpOnly cookie rather than server memory
// means any replica can complete a flow another replica started.
const pkceCookie = "__Host-infra-console-pkce"

func (l *Login) discover(ctx context.Context) (*discovery, error) {
	l.mu.RLock()
	if l.endpoints != nil && time.Since(l.fetchedAt) < time.Hour {
		defer l.mu.RUnlock()
		return l.endpoints, nil
	}
	l.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		l.issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery returned status %d", resp.StatusCode)
	}

	var d discovery
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("decode discovery: %w", err)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return nil, fmt.Errorf("discovery document is missing endpoints")
	}

	l.mu.Lock()
	l.endpoints, l.fetchedAt = &d, time.Now()
	l.mu.Unlock()
	return &d, nil
}

// Scopes requested at sign-in: only the standard ones.
//
// Zitadel advertises exactly openid/profile/email/phone/address/offline_access
// and silently drops anything else, so the eight console scopes this used to
// request were never granted and never appeared in any token. Authority comes
// from the caller's IAM_OWNER membership, looked up per verification.
var loginScopes = strings.Join([]string{
	"openid", "profile", "email",
	// Reserved Zitadel scope: adds Zitadel's own project to the token's
	// audience. Without it the session token authenticates but Zitadel's Auth
	// API refuses it, so /auth/v1/memberships/me/_search returns 401 and every
	// browser login fails authorization — regardless of which project the
	// application was created in. PATs are unaffected; only the OIDC flow needs
	// this, which is why the agent path worked while the browser path would not.
	"urn:zitadel:iam:org:project:id:zitadel:aud",
}, " ")

// HandleLogin starts the flow.
func (l *Login) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !l.Configured() {
		problem(w, http.StatusNotImplemented, "Sign-in is not configured",
			"set the console's OIDC client id to enable browser sign-in")
		return
	}

	endpoints, err := l.discover(r.Context())
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "Identity provider unreachable", "")
		return
	}

	verifier, err := randomString(64)
	if err != nil {
		problem(w, http.StatusInternalServerError, "Could not start sign-in", "")
		return
	}
	state, err := randomString(32)
	if err != nil {
		problem(w, http.StatusInternalServerError, "Could not start sign-in", "")
		return
	}

	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// state and verifier travel together in one short-lived cookie. Comparing
	// the callback's state against this cookie is the CSRF defence.
	http.SetCookie(w, &http.Cookie{
		Name:     pkceCookie,
		Value:    state + ":" + verifier,
		Path:     "/",
		MaxAge:   600,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	params := url.Values{}
	params.Set("client_id", l.clientID)
	params.Set("redirect_uri", l.redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", loginScopes)
	params.Set("state", state)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")

	http.Redirect(w, r, endpoints.AuthorizationEndpoint+"?"+params.Encode(), http.StatusFound)
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// HandleCallback completes the flow and sets the session cookie.
func (l *Login) HandleCallback(w http.ResponseWriter, r *http.Request) {
	if !l.Configured() {
		problem(w, http.StatusNotImplemented, "Sign-in is not configured", "")
		return
	}

	if errCode := r.URL.Query().Get("error"); errCode != "" {
		// Surface the provider's refusal without echoing arbitrary text back
		// into the page.
		if l.log != nil {
			l.log.Warn("sign-in refused by provider", "error", errCode)
		}
		problem(w, http.StatusUnauthorized, "Sign-in was refused", "")
		return
	}

	cookie, err := r.Cookie(pkceCookie)
	if err != nil {
		problem(w, http.StatusBadRequest, "Sign-in expired", "start again from /auth/login")
		return
	}
	state, verifier, found := strings.Cut(cookie.Value, ":")
	if !found || state == "" || verifier == "" {
		problem(w, http.StatusBadRequest, "Sign-in state is malformed", "")
		return
	}
	// Constant-time-ish comparison is unnecessary here (the value is not a
	// secret), but the match itself is what stops a forged callback.
	if r.URL.Query().Get("state") != state {
		problem(w, http.StatusBadRequest, "Sign-in state did not match", "")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		problem(w, http.StatusBadRequest, "Sign-in returned no code", "")
		return
	}

	endpoints, err := l.discover(r.Context())
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "Identity provider unreachable", "")
		return
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", l.redirectURI)
	form.Set("client_id", l.clientID)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		endpoints.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		problem(w, http.StatusInternalServerError, "Could not complete sign-in", "")
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := l.hc.Do(req)
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "Identity provider unreachable", "")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		if l.log != nil {
			l.log.Warn("token exchange rejected", "status", resp.StatusCode)
		}
		problem(w, http.StatusUnauthorized, "Sign-in could not be completed", "")
		return
	}

	var token tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil || token.AccessToken == "" {
		problem(w, http.StatusUnauthorized, "Identity provider returned no token", "")
		return
	}

	maxAge := token.ExpiresIn
	if maxAge <= 0 || maxAge > 12*3600 {
		maxAge = 3600
	}

	http.SetCookie(w, &http.Cookie{
		Name: pkceCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, SecureCookie(token.AccessToken, maxAge))

	http.Redirect(w, r, "/", http.StatusFound)
}

// HandleLogout clears the session. It does not call the provider's end-session
// endpoint: signing out of the console should not sign the operator out of
// every other Zitadel application in the same browser.
func (l *Login) HandleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// Routes registers the sign-in endpoints on a mux. They sit outside the
// authenticated API on purpose — requiring a session to obtain a session would
// be a loop.
func (l *Login) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", l.HandleLogin)
	mux.HandleFunc("GET /auth/callback", l.HandleCallback)
	mux.HandleFunc("GET /auth/logout", l.HandleLogout)
}

func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
