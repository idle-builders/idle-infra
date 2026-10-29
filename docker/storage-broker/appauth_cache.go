package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

const (
	appReviewCacheMaxEntries = 1024
	appReviewCacheMaxTTL     = 30 * time.Second
)

type cachedAppIdentity struct {
	namespace string
	account   string
	expiresAt time.Time
}

type cachedAppReviewer struct {
	next AppReviewer
	now  func() time.Time

	mu      sync.Mutex
	entries map[[sha256.Size]byte]cachedAppIdentity
}

func newCachedAppReviewer(next AppReviewer) AppReviewer {
	return &cachedAppReviewer{next: next, now: time.Now, entries: make(map[[sha256.Size]byte]cachedAppIdentity)}
}

func (c *cachedAppReviewer) Review(ctx context.Context, token string) (string, string, error) {
	key := sha256.Sum256([]byte(token))
	now := c.now()
	c.mu.Lock()
	entry, ok := c.entries[key]
	if ok && now.Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.namespace, entry.account, nil
	}
	delete(c.entries, key)
	c.mu.Unlock()

	namespace, account, err := c.next.Review(ctx, token)
	if err != nil {
		return "", "", err
	}
	tokenExpiry, ok := projectedTokenExpiry(token)
	if !ok || !tokenExpiry.After(now) {
		return namespace, account, nil
	}
	expiresAt := now.Add(appReviewCacheMaxTTL)
	if tokenExpiry.Before(expiresAt) {
		expiresAt = tokenExpiry
	}
	c.mu.Lock()
	if len(c.entries) >= appReviewCacheMaxEntries {
		for existingKey, existing := range c.entries {
			if !now.Before(existing.expiresAt) || len(c.entries) >= appReviewCacheMaxEntries {
				delete(c.entries, existingKey)
			}
			if len(c.entries) < appReviewCacheMaxEntries {
				break
			}
		}
	}
	c.entries[key] = cachedAppIdentity{namespace: namespace, account: account, expiresAt: expiresAt}
	c.mu.Unlock()
	return namespace, account, nil
}

func projectedTokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		ExpiresAt json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil {
		return time.Time{}, false
	}
	seconds, err := claims.ExpiresAt.Int64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}
