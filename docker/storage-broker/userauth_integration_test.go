//go:build integration

// Integration test for user-token validation against a real Zitadel instance.
// Opt-in via `-tags integration`; never runs in the unit suite/CI.
//
//	OIDC_ISSUER=https://id.example.com USER_TOKEN="<zitadel access token or PAT>" \
//	go test -tags integration -run TestUserVerifier -v ./...
package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestUserVerifierUserInfo(t *testing.T) {
	tok := os.Getenv("USER_TOKEN")
	if tok == "" {
		t.Skip("requires USER_TOKEN (a Zitadel access token or PAT)")
	}
	cfg := &Config{OIDCIssuer: envOr("OIDC_ISSUER", "https://id.example.com")}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uv := NewUserVerifier(ctx, cfg)
	for i := 0; i < 80 && !uv.Ready(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if !uv.Ready() {
		t.Fatal("verifier not ready — OIDC discovery failed")
	}

	sub, err := uv.Verify(ctx, tok, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if sub == "" {
		t.Fatal("empty subject")
	}
	t.Logf("token validated via userinfo, sub=%s", sub)
}
