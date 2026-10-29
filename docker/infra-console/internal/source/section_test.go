package source

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type payload struct {
	Count int
}

func TestFetchFreshCallsUpstreamOnceAndCaches(t *testing.T) {
	c := NewCache()
	calls := 0
	fn := func(context.Context) (payload, error) {
		calls++
		return payload{Count: 7}, nil
	}

	first := Fetch(context.Background(), c, Kubernetes, Key(Kubernetes, "nodes"), time.Minute, fn)
	if first.Status != StatusOK {
		t.Fatalf("first status = %q, want ok", first.Status)
	}
	if first.Data == nil || first.Data.Count != 7 {
		t.Fatalf("first data = %+v, want Count 7", first.Data)
	}

	second := Fetch(context.Background(), c, Kubernetes, Key(Kubernetes, "nodes"), time.Minute, fn)
	if second.Status != StatusOK {
		t.Fatalf("second status = %q, want ok", second.Status)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (second should hit cache)", calls)
	}
	if second.Stale {
		t.Fatal("fresh cache hit must not be marked stale")
	}
}

// AC3: a failing source with nothing cached yields an unavailable section whose
// Data is nil. Nil is the whole point — a zero-valued struct would render as a
// confident zero in the UI.
func TestFetchColdFailureIsUnavailableWithNilData(t *testing.T) {
	c := NewCache()
	fn := func(context.Context) (payload, error) {
		return payload{}, errors.New("dial tcp: connection refused")
	}

	got := Fetch(context.Background(), c, ObjectStore, Key(ObjectStore, "bucket"), time.Minute, fn)

	if got.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", got.Status)
	}
	if got.Data != nil {
		t.Fatalf("Data = %+v, want nil so callers cannot read a zero value", got.Data)
	}
	if got.Err == "" {
		t.Fatal("Err must explain the failure")
	}
	if got.Source != ObjectStore {
		t.Fatalf("Source = %q, want objectStorage", got.Source)
	}
}

// A refresh failure with a stale entry present degrades rather than blanking:
// stale data beats no data on a dashboard, as long as it is labelled.
func TestFetchStaleFailureDegradesAndKeepsData(t *testing.T) {
	c := NewCache()
	key := Key(Cloudflare, "zones")

	ok := Fetch(context.Background(), c, Cloudflare, key, time.Minute,
		func(context.Context) (payload, error) { return payload{Count: 3}, nil })
	if ok.Status != StatusOK {
		t.Fatalf("seed status = %q, want ok", ok.Status)
	}

	// TTL of zero forces every subsequent read to attempt a refresh.
	got := Fetch(context.Background(), c, Cloudflare, key, 0,
		func(context.Context) (payload, error) { return payload{}, errors.New("429 rate limited") })

	if got.Status != StatusDegraded {
		t.Fatalf("status = %q, want degraded", got.Status)
	}
	if got.Data == nil || got.Data.Count != 3 {
		t.Fatalf("Data = %+v, want the cached Count 3", got.Data)
	}
	if !got.Stale {
		t.Fatal("degraded section must be marked stale")
	}
	if got.Err == "" {
		t.Fatal("degraded section must carry the refresh error")
	}
}

// One slow backend must not fail the aggregate; a deadline is just another
// failure, reported as its own section.
func TestFetchContextDeadlineDegradesNotPanics(t *testing.T) {
	c := NewCache()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	got := Fetch(ctx, c, Zitadel, Key(Zitadel, "org"), time.Minute,
		func(ctx context.Context) (payload, error) {
			<-ctx.Done()
			return payload{}, ctx.Err()
		})

	if got.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", got.Status)
	}
	if got.Err != "timed out" {
		t.Fatalf("Err = %q, want %q", got.Err, "timed out")
	}
}

// Two sources sharing a cache key must not hand each other's data back. The
// entry is dropped and refetched rather than type-asserted into a wrong shape.
func TestFetchTypeMismatchRefetchesInsteadOfReturningWrongData(t *testing.T) {
	c := NewCache()
	key := "shared-key"
	c.Set(key, "a string, not a payload", time.Now())

	calls := 0
	got := Fetch(context.Background(), c, Postgres, key, time.Minute,
		func(context.Context) (payload, error) {
			calls++
			return payload{Count: 11}, nil
		})

	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if got.Status != StatusOK || got.Data == nil || got.Data.Count != 11 {
		t.Fatalf("got %+v, want ok with Count 11", got)
	}
}

func TestFetchWithoutCacheStillWorks(t *testing.T) {
	got := Fetch(context.Background(), nil, GitHub, "k", time.Minute,
		func(context.Context) (payload, error) { return payload{Count: 1}, nil })
	if got.Status != StatusOK || got.Data == nil || got.Data.Count != 1 {
		t.Fatalf("got %+v, want ok with Count 1", got)
	}
}

func TestSafeErrorTruncatesAndNormalizes(t *testing.T) {
	long := errors.New("line one\n\tline two   spaced" + repeat(" padding", 80))
	got := SafeError(long)

	if len(got) > maxErrorLen+len(ellipsis) {
		t.Fatalf("len = %d, want <= %d", len(got), maxErrorLen+len(ellipsis))
	}
	if !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("truncated message should end with %q: %q", ellipsis, got)
	}
	if contains(got, "\n") || contains(got, "\t") {
		t.Fatalf("newlines/tabs survived: %q", got)
	}
	if SafeError(nil) != "" {
		t.Fatal("SafeError(nil) must be empty")
	}
}

// Truncation must not split a multi-byte rune, or the message becomes invalid
// UTF-8 and breaks JSON encoding of the error we are reporting.
func TestSafeErrorTruncatesOnRuneBoundary(t *testing.T) {
	// Multi-byte runes straddling the cut point.
	got := SafeError(errors.New(repeat("ä", maxErrorLen)))
	if !utf8.ValidString(got) {
		t.Fatalf("truncated message is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("want truncation marker, got %q", got)
	}
}

func TestCacheSweepDropsOnlyOldEntries(t *testing.T) {
	c := NewCache()
	now := time.Now()
	c.now = func() time.Time { return now }

	c.Set("old", payload{Count: 1}, now.Add(-2*time.Hour))
	c.Set("new", payload{Count: 2}, now.Add(-time.Minute))

	if removed := c.Sweep(time.Hour); removed != 1 {
		t.Fatalf("Sweep removed %d, want 1", removed)
	}
	if _, _, ok := c.Get("new"); !ok {
		t.Fatal("recent entry was swept")
	}
	if _, _, ok := c.Get("old"); ok {
		t.Fatal("stale entry survived the sweep")
	}
}

func TestCacheKeyIsCollisionResistant(t *testing.T) {
	// Naive concatenation would make these two equal.
	a := Key(Kubernetes, "ab", "c")
	b := Key(Kubernetes, "a", "bc")
	if a == b {
		t.Fatalf("Key collision: %q == %q", a, b)
	}
}

func TestCacheIsSafeForConcurrentUse(t *testing.T) {
	c := NewCache()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := Key(Kubernetes, fmt.Sprintf("k%d", i%8))
			c.Set(key, payload{Count: i}, time.Now())
			_, _, _ = c.Get(key)
			_ = c.Len()
			c.Sweep(time.Hour)
		}(i)
	}
	wg.Wait()
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// Redaction must remove credentials without mangling ordinary prose. The
// original pattern matched a keyword followed by whitespace, which turned
// Cloudflare's "Invalid access token (code 9109)" into
// "Invalid access token=[redacted] 9109)".
func TestSafeErrorRedactsCredentialsButNotProse(t *testing.T) {
	redacted := []struct{ name, in, mustNotContain string }{
		{"bearer header", "Get \"https://x\": Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc", "eyJhbGciOiJIUzI1NiJ9.abc"},
		{"query string", "Get \"https://s3/bucket?X-Amz-Signature=deadbeefcafe\": timeout", "deadbeefcafe"},
		{"token kv", "request failed: token=sk-live-1234567890", "sk-live-1234567890"},
		{"secret colon", "config error: secret: hunter2horse", "hunter2horse"},
		{"api key kv", "rejected api_key=AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE"},
	}
	for _, tc := range redacted {
		got := SafeError(errors.New(tc.in))
		if strings.Contains(got, tc.mustNotContain) {
			t.Errorf("%s: credential survived redaction: %q", tc.name, got)
		}
		if !strings.Contains(got, "[redacted]") {
			t.Errorf("%s: nothing was redacted: %q", tc.name, got)
		}
	}

	intact := []struct{ name, in, mustContain string }{
		{"cloudflare code", "cloudflare: Invalid access token (code 9109)", "(code 9109)"},
		{"key in prose", "no bucket mapped for namespace \"x\" in the broker policy key lookup", "key lookup"},
		{"password prose", "password authentication failed for user \"platform_reader\"", "authentication failed"},
	}
	for _, tc := range intact {
		got := SafeError(errors.New(tc.in))
		if !strings.Contains(got, tc.mustContain) {
			t.Errorf("%s: prose was mangled: %q", tc.name, got)
		}
	}
}
