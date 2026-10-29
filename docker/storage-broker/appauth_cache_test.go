package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"
)

type countingReviewer struct {
	calls int
	err   error
}

func (r *countingReviewer) Review(context.Context, string) (string, string, error) {
	r.calls++
	if r.err != nil {
		return "", "", r.err
	}
	return "acme", "publisher", nil
}

func testJWT(expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiry.Unix())))
	return header + "." + payload + ".signature"
}

func TestAppReviewCacheIsBoundedByTokenExpiryAndMaximumTTL(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	next := &countingReviewer{}
	cache := newCachedAppReviewer(next).(*cachedAppReviewer)
	cache.now = func() time.Time { return base }
	token := testJWT(base.Add(time.Minute))
	if _, _, err := cache.Review(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Review(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if next.calls != 1 {
		t.Fatalf("TokenReview calls = %d, want 1", next.calls)
	}
	cache.now = func() time.Time { return base.Add(appReviewCacheMaxTTL + time.Second) }
	if _, _, err := cache.Review(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if next.calls != 2 {
		t.Fatalf("TokenReview calls after maximum TTL = %d, want 2", next.calls)
	}
}

func TestAppReviewCacheFailsClosedAndDoesNotCacheInvalidTokens(t *testing.T) {
	next := &countingReviewer{err: errors.New("invalid")}
	cache := newCachedAppReviewer(next)
	for range 2 {
		if _, _, err := cache.Review(context.Background(), testJWT(time.Now().Add(time.Minute))); err == nil {
			t.Fatal("invalid token was accepted")
		}
	}
	if next.calls != 2 {
		t.Fatalf("invalid TokenReview calls = %d, want 2", next.calls)
	}
}

func TestAppReviewCacheDoesNotCacheUnparseableOrExpiredJWT(t *testing.T) {
	for _, token := range []string{"opaque-token", testJWT(time.Now().Add(-time.Minute))} {
		next := &countingReviewer{}
		cache := newCachedAppReviewer(next)
		for range 2 {
			if _, _, err := cache.Review(context.Background(), token); err != nil {
				t.Fatal(err)
			}
		}
		if next.calls != 2 {
			t.Fatalf("token %q was cached without a usable expiry", token)
		}
	}
}

func TestAppReviewCacheEntryCountIsBounded(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	next := &countingReviewer{}
	cache := newCachedAppReviewer(next).(*cachedAppReviewer)
	cache.now = func() time.Time { return base }
	for index := 0; index < appReviewCacheMaxEntries+10; index++ {
		token := testJWT(base.Add(time.Minute)) + fmt.Sprintf("-%d", index)
		if _, _, err := cache.Review(context.Background(), token); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) > appReviewCacheMaxEntries {
		t.Fatalf("cache entries = %d", len(cache.entries))
	}
}
