package main

import (
	"os"
	"path/filepath"
	"testing"
)

func testPolicy() *Policy {
	return &Policy{
		DefaultTTLSeconds: TTLConfig{Get: 900, Put: 300, Delete: 120, Multipart: 900},
		Namespaces: map[string]NamespacePolicy{
			"ambrovia": {
				Bucket:           "ambrovia-files",
				Ops:              []string{"get", "put", "delete"},
				RequireUserToken: true,
				Prefixes:         []string{"u/${sub}/"},
			},
			"nimmly": {
				Bucket:           "nimmly-files",
				Ops:              []string{"get", "put", "delete", "multipart"},
				RequireUserToken: false,
				// One user-bound prefix and two literal app-layer prefixes.
				Prefixes: []string{"u/${sub}/", "corpus/", "private-intake/workspace/"},
			},
		},
	}
}

func TestAuthorize(t *testing.T) {
	p := testPolicy()

	cases := []struct {
		name      string
		namespace string
		op        string
		key       string
		sub       string
		wantOK    bool
		wantBkt   string
		wantTTL   int
	}{
		// App scope / bucket derivation.
		{"personal put allowed", "ambrovia", "put", "u/user-123/report.pdf", "user-123", true, "ambrovia-files", 300},
		{"personal get allowed", "ambrovia", "get", "u/user-123/report.pdf", "user-123", true, "ambrovia-files", 900},
		{"delete allowed", "ambrovia", "delete", "u/user-123/old.pdf", "user-123", true, "ambrovia-files", 120},
		{"shared literal prefix allowed (nimmly)", "nimmly", "get", "corpus/registry/zh.json", "", true, "nimmly-files", 900},
		{"private intake prefix allowed (nimmly)", "nimmly", "put", "private-intake/workspace/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/raw/sha256/ab/abcdef", "", true, "nimmly-files", 300},

		// User scope.
		{"other user's prefix denied", "ambrovia", "get", "u/user-999/report.pdf", "user-123", false, "", 0},
		{"user token required but missing", "ambrovia", "put", "u/user-123/x.pdf", "", false, "", 0},

		// Op scope.
		{"delete allowed (nimmly scratchpad mapping)", "nimmly", "delete", "corpus/scratchpads/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/result.json", "", true, "nimmly-files", 120},

		// Multipart op (streaming large corpus objects).
		{"multipart allowed (nimmly corpus)", "nimmly", "multipart", "corpus/shab/y.txt", "", true, "nimmly-files", 900},
		{"multipart denied (ambrovia lacks op)", "ambrovia", "multipart", "u/user-123/big.bin", "user-123", false, "", 0},

		// Cross-app / unknown namespace.
		{"unknown namespace denied", "evil", "get", "u/user-123/x", "user-123", false, "", 0},

		// Key not within any prefix.
		{"key outside allowed prefixes", "nimmly", "get", "secret/passwd", "", false, "", 0},

		// Key validation.
		{"path traversal denied", "ambrovia", "get", "u/user-123/../../etc/passwd", "user-123", false, "", 0},
		{"absolute key denied", "ambrovia", "get", "/u/user-123/x", "user-123", false, "", 0},
		{"trailing slash (prefix) denied", "ambrovia", "get", "u/user-123/", "user-123", false, "", 0},
		{"empty key denied", "ambrovia", "get", "", "user-123", false, "", 0},
		{"backslash denied", "ambrovia", "get", "u/user-123/a\\b", "user-123", false, "", 0},
		{"double slash denied", "ambrovia", "get", "u/user-123//x", "user-123", false, "", 0},

		// Subject injection.
		{"subject with slash denied", "ambrovia", "get", "u/a/b/x", "a/b", false, "", 0},
		{"subject with traversal denied", "ambrovia", "get", "u/../x", "..", false, "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := p.Authorize(tc.namespace, tc.op, tc.key, tc.sub)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("expected allow, got deny: %v", err)
				}
				if d.Bucket != tc.wantBkt {
					t.Errorf("bucket = %q, want %q", d.Bucket, tc.wantBkt)
				}
				if d.TTLSeconds != tc.wantTTL {
					t.Errorf("ttl = %d, want %d", d.TTLSeconds, tc.wantTTL)
				}
			} else if err == nil {
				t.Fatalf("expected deny, got allow (bucket=%q prefix=%q)", d.Bucket, d.Prefix)
			}
		})
	}
}

func TestAuthorizeNeverCrossesBuckets(t *testing.T) {
	p := testPolicy()
	// A valid request in ambrovia must never yield nimmly's bucket, regardless of key.
	for _, key := range []string{"u/u1/nimmly-files/x", "u/u1/../nimmly/x", "u/u1/x"} {
		d, err := p.Authorize("ambrovia", "get", key, "u1")
		if err == nil && d.Bucket != "ambrovia-files" {
			t.Fatalf("ambrovia request resolved to bucket %q for key %q", d.Bucket, key)
		}
	}
}

func TestLoadPolicyRejectsBucketReuseAcrossTenants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	payload := []byte(`{"namespaces":{"tenant-a":{"bucket":"shared-files"},"tenant-b":{"bucket":"shared-files"}}}`)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(path); err == nil {
		t.Fatal("policy accepted one physical bucket for multiple tenants")
	}
}

func TestEmptyPrefixesDeniesAll(t *testing.T) {
	// A namespace with no prefixes is a valid, security-critical config: deny all.
	p := &Policy{
		DefaultTTLSeconds: TTLConfig{Get: 900, Put: 300, Delete: 120},
		Namespaces: map[string]NamespacePolicy{
			"locked": {Bucket: "locked-files", Ops: []string{"get", "put"}, Prefixes: []string{}},
		},
	}
	for _, key := range []string{"u/u1/x", "anything", "corpus/x"} {
		if _, err := p.Authorize("locked", "get", key, "u1"); err == nil {
			t.Fatalf("empty prefixes must deny all keys, but allowed %q", key)
		}
	}
}

func TestResolvePrefix(t *testing.T) {
	if _, ok := resolvePrefix("u/${sub}/", ""); ok {
		t.Error("a ${sub} rule must be unusable without a subject")
	}
	got, ok := resolvePrefix("u/${sub}/", "abc")
	if !ok || got != "u/abc/" {
		t.Errorf("resolvePrefix = %q,%v want u/abc/,true", got, ok)
	}
	got, ok = resolvePrefix("corpus/", "")
	if !ok || got != "corpus/" {
		t.Errorf("literal prefix = %q,%v want corpus/,true", got, ok)
	}
}
