//go:build integration

// Integration smoke tests for the presigner against a real S3-compatible
// gateway (Infomaniak). Opt-in via `-tags integration`; never run in the unit
// suite/CI.
//
// They exercise the EXACT signing and staging-promotion paths the broker uses.
//
//	S3_ENDPOINT=https://s3.pub2.infomaniak.cloud S3_REGION=us-east-1 \
//	S3_BUCKET=<bucket> S3_ACCESS_KEY_ID=... S3_SECRET_ACCESS_KEY=... \
//	go test -tags integration -v ./...
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestPresignRoundTrip(t *testing.T) {
	cfg, bucket := integrationS3Config(t)
	p := NewPresigner(cfg)
	ctx := context.Background()
	key := fmt.Sprintf("broker-selftest/%d.txt", time.Now().UnixNano())
	payload := []byte("storage-broker presign selftest")

	put, err := p.Presign(ctx, "put", bucket, key, "text/plain", 120*time.Second)
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	do(t, "PUT", put.Method, put.URL, payload, "text/plain", http.StatusOK)

	get, err := p.Presign(ctx, "get", bucket, key, "", 120*time.Second)
	if err != nil {
		t.Fatalf("presign get: %v", err)
	}
	body := do(t, "GET", get.Method, get.URL, nil, "", http.StatusOK)
	if !bytes.Equal(body, payload) {
		t.Fatalf("round-trip body mismatch: got %q want %q", body, payload)
	}

	del, err := p.Presign(ctx, "delete", bucket, key, "", 120*time.Second)
	if err != nil {
		t.Fatalf("presign delete: %v", err)
	}
	do(t, "DELETE", del.Method, del.URL, nil, "", http.StatusNoContent)

	t.Logf("round-trip OK: put+get+delete %s/%s via %s", bucket, key, cfg.S3Endpoint)
}

func TestPromoteStagedObject(t *testing.T) {
	cfg, bucket := integrationS3Config(t)
	p := NewPresigner(cfg)
	ctx := context.Background()
	testID := fmt.Sprintf("%d", time.Now().UnixNano())
	sourceKey := "governed-staging/integration/" + testID
	payload := []byte("governed promotion integration proof")
	digest := sha256.Sum256(payload)
	contentHash := hex.EncodeToString(digest[:])
	targetKey := "governed/integration/sha256/" + contentHash + "/" + testID
	t.Cleanup(func() {
		_ = p.DeleteObject(ctx, bucket, sourceKey)
		_ = p.DeleteObject(ctx, bucket, targetKey)
	})

	put, err := p.Presign(ctx, "put", bucket, sourceKey, "text/plain", 120*time.Second)
	if err != nil {
		t.Fatalf("presign staged put: %v", err)
	}
	do(t, "PUT staged", put.Method, put.URL, payload, "text/plain", http.StatusOK)

	if err := p.PromoteObject(ctx, bucket, sourceKey, targetKey, contentHash, int64(len(payload))); err != nil {
		t.Fatalf("promote staged object: %v", err)
	}
	get, err := p.Presign(ctx, "get", bucket, targetKey, "", 120*time.Second)
	if err != nil {
		t.Fatalf("presign promoted get: %v", err)
	}
	body := do(t, "GET promoted", get.Method, get.URL, nil, "", http.StatusOK)
	if !bytes.Equal(body, payload) {
		t.Fatalf("promoted body mismatch: got %q want %q", body, payload)
	}
}

func TestDeleteAndConfirmAbsent(t *testing.T) {
	cfg, bucket := integrationS3Config(t)
	p := NewPresigner(cfg)
	ctx := context.Background()
	key := fmt.Sprintf("governed-delete-selftest/%d.txt", time.Now().UnixNano())
	payload := []byte("governed deletion integration proof")
	t.Cleanup(func() { _ = p.DeleteObject(ctx, bucket, key) })

	put, err := p.Presign(ctx, "put", bucket, key, "text/plain", 120*time.Second)
	if err != nil {
		t.Fatalf("presign delete proof put: %v", err)
	}
	do(t, "PUT delete proof", put.Method, put.URL, payload, "text/plain", http.StatusOK)
	if err := p.DeleteAndConfirmAbsent(ctx, bucket, key); err != nil {
		t.Fatalf("delete and confirm absent: %v", err)
	}
}

func TestWalkKeysFindsOnlyExactStagingPrefix(t *testing.T) {
	cfg, bucket := integrationS3Config(t)
	p := NewPresigner(cfg)
	ctx := context.Background()
	base := fmt.Sprintf("governed-staging/selftest/%d", time.Now().UnixNano())
	prefix := base + "/"
	inside := prefix + "inside.txt"
	outside := base + "-outside.txt"
	for _, key := range []string{inside, outside} {
		key := key
		t.Cleanup(func() { _ = p.DeleteObject(ctx, bucket, key) })
		put, err := p.Presign(ctx, "put", bucket, key, "text/plain", 120*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		do(t, "PUT list proof", put.Method, put.URL, []byte(key), "text/plain", http.StatusOK)
	}

	var keys []string
	err := p.WalkKeys(ctx, bucket, prefix, func(key string) error {
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != inside {
		t.Fatalf("listed keys = %#v, want [%q]", keys, inside)
	}
}

func integrationS3Config(t *testing.T) (*Config, string) {
	t.Helper()
	cfg := &Config{
		S3Endpoint:  os.Getenv("S3_ENDPOINT"),
		S3Region:    envOr("S3_REGION", "us-east-1"),
		S3AccessKey: os.Getenv("S3_ACCESS_KEY_ID"),
		S3SecretKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
	}
	bucket := os.Getenv("S3_BUCKET")
	if cfg.S3Endpoint == "" || cfg.S3AccessKey == "" || cfg.S3SecretKey == "" || bucket == "" {
		t.Skip("requires S3_ENDPOINT, S3_ACCESS_KEY_ID, S3_SECRET_ACCESS_KEY, S3_BUCKET")
	}
	return cfg, bucket
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// do executes the presigned request and asserts the status. The URL is never
// logged (it carries the SigV4 signature).
func do(t *testing.T, label, method, url string, body []byte, contentType string, want int) []byte {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("%s build req: %v", label, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s do: %v", label, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s -> HTTP %d (want %d): %s", label, resp.StatusCode, want, string(out))
	}
	return out
}
