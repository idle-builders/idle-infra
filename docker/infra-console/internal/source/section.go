// Package source fans a console request across the systems that own platform
// state, and normalizes the result.
//
// The contract: an aggregate must never fail whole because one backend is
// down. Each contribution carries its own status, and an unavailable source's
// data is ABSENT rather than zero-valued — so a client that ignores Status
// renders a visible null, not a plausible number.
package source

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Status mirrors the spec's SectionStatus.
type Status string

const (
	StatusOK Status = "ok"
	// Data is present but stale: a refresh failed and a cached value is served.
	StatusDegraded Status = "degraded"
	// No usable data: Data is nil and Err is set.
	StatusUnavailable Status = "unavailable"
)

// Name identifies a backing system; values mirror the spec's SourceName.
type Name string

const (
	Kubernetes  Name = "kubernetes"
	Postgres    Name = "postgres"
	Zitadel     Name = "zitadel"
	ObjectStore Name = "objectStorage"
	Cloudflare  Name = "cloudflare"
	GitHub      Name = "github"
	Telemetry   Name = "telemetry"
)

// Section is one backing system's contribution to an aggregate response. Data
// is a pointer on purpose: nil is the wire-visible signal that this section
// carries nothing, which stops a dead backend rendering as a confident zero.
type Section[T any] struct {
	Status    Status
	Source    Name
	Data      *T
	Err       string
	FetchedAt time.Time
	Stale     bool
}

// OK builds a fresh section.
func OK[T any](src Name, data T, at time.Time) Section[T] {
	return Section[T]{Status: StatusOK, Source: src, Data: &data, FetchedAt: at}
}

// Degraded builds a section carrying stale data after a failed refresh. The
// data is still returned — stale beats absent for a human reading a dashboard —
// but Stale and Err say so.
func Degraded[T any](src Name, data T, at time.Time, err error) Section[T] {
	return Section[T]{
		Status:    StatusDegraded,
		Source:    src,
		Data:      &data,
		Err:       SafeError(err),
		FetchedAt: at,
		Stale:     true,
	}
}

// Unavailable builds a section with no data. Data stays nil.
func UnavailableSection[T any](src Name, err error) Section[T] {
	return Section[T]{Status: StatusUnavailable, Source: src, Err: SafeError(err)}
}

// Fetcher loads one source's data.
type Fetcher[T any] func(context.Context) (T, error)

// Fetch resolves one section through the cache, degrading rather than failing.
//
//	fresh cache hit          -> ok, no upstream call
//	miss, fetch succeeds     -> ok, value cached
//	miss, fetch fails, stale -> degraded, stale value returned
//	miss, fetch fails, cold  -> unavailable, Data nil
//
// A context error (deadline, cancellation) is treated like any other failure:
// one slow backend must not fail the whole aggregate.
func Fetch[T any](ctx context.Context, c *Cache, src Name, key string, ttl time.Duration, fn Fetcher[T]) Section[T] {
	if c != nil {
		if v, at, ok := c.Get(key); ok && time.Since(at) < ttl {
			if typed, ok := v.(T); ok {
				return OK(src, typed, at)
			}
			// A type mismatch means two callers share a cache key. Treat the
			// entry as absent rather than returning another source's data.
			c.Delete(key)
		}
	}

	data, err := fn(ctx)
	if err == nil {
		now := time.Now()
		if c != nil {
			c.Set(key, data, now)
		}
		return OK(src, data, now)
	}

	if c != nil {
		if v, at, ok := c.Get(key); ok {
			if typed, ok := v.(T); ok {
				return Degraded(src, typed, at, err)
			}
		}
	}
	return UnavailableSection[T](src, err)
}

// credentialish matches the parts of an error message most likely to carry a
// secret: a URL query string (presigned URLs, tokens) and bearer values.
var (
	queryString = regexp.MustCompile(`\?[^\s"']*`)
	// Two forms, deliberately separate. "Bearer <value>" spans a space; the
	// others must not, or prose gets eaten — matching across a space turned
	// "Invalid access token (code 9109)" into "token=[redacted] 9109)".
	bearerValue = regexp.MustCompile(`(?i)bearer\s+\S+`)
	secretKV    = regexp.MustCompile(`(?i)(bearer|token|api[_-]?key|key|secret|password|passwd|credential)\s*[=:]\s*\S+`)
)

// SafeError strips the obvious credential carriers from an error before it
// reaches a response or a log. A backstop, not a licence: upstream clients
// routinely put the request URL — and its signed query — in transport errors.
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		msg = "timed out"
	} else if errors.Is(err, context.Canceled) {
		msg = "cancelled"
	}
	msg = queryString.ReplaceAllString(msg, "?[redacted]")
	msg = bearerValue.ReplaceAllString(msg, "Bearer [redacted]")
	msg = secretKV.ReplaceAllString(msg, "$1=[redacted]")
	msg = strings.Join(strings.Fields(msg), " ")

	if len(msg) > maxErrorLen {
		// Cut on a rune boundary. Slicing raw bytes can split a multi-byte
		// rune and emit invalid UTF-8, which then breaks JSON encoding of the
		// very error we are trying to report.
		cut := maxErrorLen
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + ellipsis
	}
	return msg
}

const (
	// maxErrorLen bounds an error message in bytes before truncation.
	maxErrorLen = 300
	ellipsis    = "…"
)
