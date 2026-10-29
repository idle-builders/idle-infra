package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Policy is the per-tenant authorization model, loaded from a mounted JSON file
// (the storage-broker-policy ConfigMap). It maps a Kubernetes namespace (the
// verified app identity) to the single bucket that app may touch, the allowed
// operations, and the key prefixes it may act within.
//
// Prefix rules support a single placeholder, ${sub}, which is substituted with
// the verified Zitadel subject — binding the key to a specific end user (e.g.
// "u/${sub}/" => personal files only). A rule WITHOUT a placeholder is a literal
// app-layer prefix (e.g. a shared corpus); user-scope for those stays app-layer
// until Zitadel carries membership claims (see docs/plans/presign-storage-broker.md).
type Policy struct {
	DefaultTTLSeconds TTLConfig                  `json:"defaultTTLSeconds"`
	Namespaces        map[string]NamespacePolicy `json:"namespaces"`
}

type NamespacePolicy struct {
	Bucket           string   `json:"bucket"`
	Ops              []string `json:"ops"`
	RequireUserToken bool     `json:"requireUserToken"`
	Prefixes         []string `json:"prefixes"`
	// UserTokenClients pins which Zitadel OIDC clients may have minted the user
	// tokens this tenant presents. Non-empty: a user token must be a JWT signed
	// by the issuer whose client_id (or azp) is one of these, so a token minted
	// by another app on the shared issuer is refused. Client ids, not project
	// ids: any client can ask for a project id in its token's aud. Empty: any
	// access token from the issuer (opaque included) is accepted.
	UserTokenClients []string `json:"userTokenClients,omitempty"`
}

type TTLConfig struct {
	Get       int `json:"get"`
	Put       int `json:"put"`
	Delete    int `json:"delete"`
	Multipart int `json:"multipart"`
}

// Decision is the result of an allowed authorization: where to sign, for how
// long, and which resolved prefix matched (recorded in the audit log).
type Decision struct {
	Bucket     string
	TTLSeconds int
	Prefix     string
}

// DenyError carries a reason safe to return to the caller (never a secret).
type DenyError struct{ Reason string }

func (e *DenyError) Error() string { return e.Reason }

func deny(reason string) error { return &DenyError{Reason: reason} }

// Zitadel subjects are numeric/uuid-ish; restrict to a safe charset so a subject
// can never inject path separators or traversal sequences into a resolved prefix.
var subPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)

func LoadPolicy(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}
	var p Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse policy %s: %w", path, err)
	}
	if len(p.Namespaces) == 0 {
		return nil, fmt.Errorf("policy %s has no namespaces", path)
	}
	bucketOwners := make(map[string]string, len(p.Namespaces))
	for namespace, namespacePolicy := range p.Namespaces {
		for _, client := range namespacePolicy.UserTokenClients {
			if client == "" || strings.TrimSpace(client) != client {
				return nil, fmt.Errorf("policy %s: namespace %q has an empty or padded user token client %q",
					path, namespace, client)
			}
		}
		if namespacePolicy.Bucket == "" {
			continue
		}
		if owner, exists := bucketOwners[namespacePolicy.Bucket]; exists {
			return nil, fmt.Errorf("policy %s assigns bucket %q to both %q and %q",
				path, namespacePolicy.Bucket, owner, namespace)
		}
		bucketOwners[namespacePolicy.Bucket] = namespace
	}
	if p.DefaultTTLSeconds.Get == 0 {
		p.DefaultTTLSeconds.Get = 900
	}
	if p.DefaultTTLSeconds.Put == 0 {
		p.DefaultTTLSeconds.Put = 300
	}
	if p.DefaultTTLSeconds.Delete == 0 {
		p.DefaultTTLSeconds.Delete = 120
	}
	if p.DefaultTTLSeconds.Multipart == 0 {
		// TTL of a single presigned UploadPart URL. Parts are signed just-in-time,
		// one at a time, so this only needs to cover one part upload — but we keep
		// it generous for slow links / large parts.
		p.DefaultTTLSeconds.Multipart = 900
	}
	return &p, nil
}

// UserTokenClients returns the clients a namespace's user tokens must come
// from; nil means unpinned (or an unknown namespace, which Authorize then denies).
func (p *Policy) UserTokenClients(namespace string) []string {
	return p.Namespaces[namespace].UserTokenClients
}

// Authorize is the single decision point. It is deliberately deny-by-default:
// every step must explicitly pass or the request is rejected.
func (p *Policy) Authorize(namespace, op, key, sub string) (*Decision, error) {
	ns, ok := p.Namespaces[namespace]
	if !ok {
		return nil, deny("namespace not provisioned for storage")
	}
	if !contains(ns.Ops, op) {
		return nil, deny("operation not allowed")
	}
	if err := validKey(key); err != nil {
		return nil, err
	}
	if ns.RequireUserToken && sub == "" {
		return nil, deny("user token required")
	}
	if sub != "" && !subPattern.MatchString(sub) {
		return nil, deny("invalid subject")
	}

	for _, rule := range ns.Prefixes {
		resolved, ok := resolvePrefix(rule, sub)
		if !ok {
			continue // a ${sub} rule with no user token in hand — skip it
		}
		if strings.HasPrefix(key, resolved) {
			return &Decision{
				Bucket:     ns.Bucket,
				TTLSeconds: ttlFor(op, p.DefaultTTLSeconds),
				Prefix:     resolved,
			}, nil
		}
	}
	return nil, deny("key not within an allowed prefix")
}

// resolvePrefix substitutes ${sub} if present. A ${sub} rule is unusable (ok=false)
// without a verified subject, so it can never collapse to a wildcard.
func resolvePrefix(rule, sub string) (resolved string, ok bool) {
	if strings.Contains(rule, "${sub}") {
		if sub == "" {
			return "", false
		}
		return strings.ReplaceAll(rule, "${sub}", sub), true
	}
	return rule, true
}

// validKey rejects anything that isn't a well-formed single-object key: no
// traversal, no absolute paths, no empty segments, no directory references.
func validKey(key string) error {
	switch {
	case key == "":
		return deny("empty key")
	case len(key) > 1024:
		return deny("key too long")
	case strings.HasPrefix(key, "/"):
		return deny("key must not start with /")
	case strings.HasSuffix(key, "/"):
		return deny("key must reference an object, not a prefix")
	case strings.Contains(key, "\\"):
		return deny("backslash not allowed in key")
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return deny("control character in key")
		}
	}
	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "":
			return deny("empty path segment in key")
		case ".", "..":
			return deny("path traversal in key")
		}
	}
	return nil
}

func ttlFor(op string, t TTLConfig) int {
	switch op {
	case "get":
		return t.Get
	case "put":
		return t.Put
	case "delete":
		return t.Delete
	case "multipart":
		return t.Multipart
	default:
		return 60
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
