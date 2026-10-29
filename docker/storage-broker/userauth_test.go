package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeIssuer serves just enough OIDC for UserVerifier: discovery, a JWKS with
// one RS256 key, and a userinfo endpoint that accepts only the tokens listed in
// active (mapping token -> subject), as Zitadel does for live access tokens.
type fakeIssuer struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	active map[string]string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key, active: map[string]string{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"jwks_uri":                              f.srv.URL + "/keys",
			"userinfo_endpoint":                     f.srv.URL + "/userinfo",
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()),
			"e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		sub, ok := f.active[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": sub})
	})
	return f
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// sign builds an RS256 JWT over claims with key and kid, and registers it with
// userinfo as belonging to userinfoSub ("" leaves it unknown to userinfo).
func (f *fakeIssuer) sign(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any, userinfoSub string) string {
	t.Helper()
	header := b64([]byte(`{"alg":"RS256","kid":"` + kid + `","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	signing := header + "." + b64(body)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	tok := signing + "." + b64(sig)
	if userinfoSub != "" {
		f.active[tok] = userinfoSub
	}
	return tok
}

// claims returns valid access-token claims for sub minted by clientID, with
// overrides applied (a nil override value deletes the claim).
func (f *fakeIssuer) claims(sub, clientID string, overrides map[string]any) map[string]any {
	c := map[string]any{
		"iss": f.srv.URL, "sub": sub, "aud": []string{clientID, "project-id"},
		"client_id": clientID,
		"iat":       time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

// jwt is a validly signed access token for sub from clientID.
func (f *fakeIssuer) jwt(t *testing.T, sub, clientID string, overrides map[string]any, userinfoSub string) string {
	t.Helper()
	return f.sign(t, f.key, "k1", f.claims(sub, clientID, overrides), userinfoSub)
}

func readyVerifier(t *testing.T, issuer string) *UserVerifier {
	t.Helper()
	uv := NewUserVerifier(context.Background(), &Config{OIDCIssuer: issuer})
	for i := 0; i < 200 && !uv.Ready(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !uv.Ready() {
		t.Fatal("verifier never became ready")
	}
	return uv
}

func TestUserVerifierUserTokenClients(t *testing.T) {
	f := newFakeIssuer(t)
	uv := readyVerifier(t, f.srv.URL)
	pinned := []string{"nimmly-login"}

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := b64([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
		b64(mustJSON(t, f.claims("user-1", "nimmly-login", nil))) + "."
	f.active[unsigned] = "user-1"

	f.active["opaque-token"] = "user-1"
	nimmly := f.jwt(t, "user-1", "nimmly-login", nil, "user-1")
	azpOnly := f.jwt(t, "user-1", "nimmly-login", map[string]any{"client_id": nil, "azp": "nimmly-login"}, "user-1")
	other := f.jwt(t, "user-1", "ambrovia-web", nil, "user-1")
	// Another client asking for nimmly's ids in aud must not pass.
	otherWithAud := f.jwt(t, "user-1", "ambrovia-web", map[string]any{"aud": []string{"nimmly-login", "project-id"}}, "user-1")
	otherWithAZP := f.jwt(t, "user-1", "ambrovia-web", map[string]any{"azp": "nimmly-login"}, "user-1")
	noSubject := f.jwt(t, "user-1", "nimmly-login", map[string]any{"sub": nil}, "user-1")
	noClient := f.jwt(t, "user-1", "nimmly-login", map[string]any{"client_id": nil}, "user-1")
	idToken := f.jwt(t, "user-1", "nimmly-login", map[string]any{"at_hash": "x"}, "user-1")
	revoked := f.jwt(t, "user-1", "nimmly-login", map[string]any{"jti": "revoked"}, "")
	mismatched := f.jwt(t, "user-2", "nimmly-login", nil, "user-1")
	expired := f.jwt(t, "user-1", "nimmly-login", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, "user-1")
	wrongIssuer := f.jwt(t, "user-1", "nimmly-login", map[string]any{"iss": "https://elsewhere.example"}, "user-1")
	foreignKey := f.sign(t, otherKey, "k1", f.claims("user-1", "nimmly-login", nil), "user-1")
	unknownKid := f.sign(t, otherKey, "k2", f.claims("user-1", "nimmly-login", nil), "user-1")

	cases := []struct {
		name    string
		token   string
		clients []string
		wantSub string // "" = must be rejected
	}{
		{"unpinned accepts opaque token", "opaque-token", nil, "user-1"},
		{"unpinned accepts any audience", other, nil, "user-1"},
		{"pinned accepts its client's token", nimmly, pinned, "user-1"},
		{"pinned accepts its client named only in azp", azpOnly, pinned, "user-1"},
		{"pinned rejects another app's token", other, pinned, ""},
		{"pinned rejects another app's token naming it in aud", otherWithAud, pinned, ""},
		{"pinned rejects another app's token naming it in azp", otherWithAZP, pinned, ""},
		{"pinned rejects a token with no subject", noSubject, pinned, ""},
		{"pinned rejects a token naming no client", noClient, pinned, ""},
		{"pinned rejects an ID token", idToken, pinned, ""},
		{"pinned rejects opaque token", "opaque-token", pinned, ""},
		{"pinned rejects an unsigned token", unsigned, pinned, ""},
		{"pinned rejects an expired token", expired, pinned, ""},
		{"pinned rejects another issuer's token", wrongIssuer, pinned, ""},
		{"pinned rejects a token signed by another key", foreignKey, pinned, ""},
		{"pinned rejects a token with an unknown key id", unknownKid, pinned, ""},
		{"pinned still rejects a revoked token", revoked, pinned, ""},
		{"pinned rejects a subject userinfo disagrees with", mismatched, pinned, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := uv.Verify(context.Background(), tc.token, tc.clients)
			if tc.wantSub == "" {
				if err == nil {
					t.Fatalf("accepted, sub=%q; want rejection", sub)
				}
				return
			}
			if err != nil || sub != tc.wantSub {
				t.Fatalf("got sub=%q err=%v; want sub=%q", sub, err, tc.wantSub)
			}
		})
	}
}

func TestPolicyUserTokenClients(t *testing.T) {
	p := testPolicy()
	p.Namespaces["nimmly"] = NamespacePolicy{Bucket: "nimmly-files", UserTokenClients: []string{"a"}}
	if got := p.UserTokenClients("nimmly"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("nimmly clients = %v", got)
	}
	if got := p.UserTokenClients("ambrovia"); got != nil {
		t.Fatalf("unpinned namespace clients = %v, want nil", got)
	}
	if got := p.UserTokenClients("unknown"); got != nil {
		t.Fatalf("unknown namespace clients = %v, want nil", got)
	}
}

func TestLoadPolicyUserTokenClients(t *testing.T) {
	cases := map[string]bool{
		`{"namespaces":{"nimmly":{"bucket":"b","userTokenClients":["379428288789021199"]}}}`: true,
		`{"namespaces":{"nimmly":{"bucket":"b"}}}`:                                           true,
		`{"namespaces":{"nimmly":{"bucket":"b","userTokenClients":[""]}}}`:                   false,
		`{"namespaces":{"nimmly":{"bucket":"b","userTokenClients":[" 3794 "]}}}`:             false,
	}
	for payload, wantOK := range cases {
		path := filepath.Join(t.TempDir(), "policy.json")
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadPolicy(path)
		if (err == nil) != wantOK {
			t.Errorf("LoadPolicy(%s) err = %v, want ok=%v", payload, err, wantOK)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
