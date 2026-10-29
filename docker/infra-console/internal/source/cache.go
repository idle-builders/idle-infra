package source

import (
	"sync"
	"time"
)

// Cache is a small in-process store keyed by source and scope.
//
// It deliberately does NOT evict on expiry: an expired entry is what the
// degraded path serves when a refresh fails. A janitor drops entries far past
// any plausible TTL.
type Cache struct {
	mu  sync.RWMutex
	m   map[string]entry
	now func() time.Time
}

type entry struct {
	val any
	at  time.Time
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{m: make(map[string]entry), now: time.Now}
}

// Get returns the stored value and when it was stored. The bool reports
// presence only — the caller decides whether it is fresh enough.
func (c *Cache) Get(key string) (any, time.Time, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[key]
	if !ok {
		return nil, time.Time{}, false
	}
	return e.val, e.at, true
}

// Set stores a value with the time it was fetched.
func (c *Cache) Set(key string, val any, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = entry{val: val, at: at}
}

// Delete drops an entry.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

// Len reports how many entries are held. Test and metrics helper.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}

// Sweep drops entries older than maxAge and returns how many it removed.
//
// This bounds memory for keys that stop being requested — a decommissioned
// tenant, say — without interfering with the degraded path, which only ever
// wants entries a few TTLs old. Call it on a slow interval with a generous
// maxAge.
func (c *Cache) Sweep(maxAge time.Duration) int {
	cutoff := c.now().Add(-maxAge)
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for k, e := range c.m {
		if e.at.Before(cutoff) {
			delete(c.m, k)
			removed++
		}
	}
	return removed
}

// Key builds a cache key from a source and scope parts. Using this rather than
// ad-hoc concatenation keeps two sources from colliding on the same key, which
// Fetch would otherwise surface as a type mismatch.
func Key(src Name, parts ...string) string {
	key := string(src)
	for _, p := range parts {
		key += "\x1f" + p
	}
	return key
}
