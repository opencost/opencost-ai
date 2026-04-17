package ratelimit

import (
	"sync"
	"time"

	// Third-party: golang.org/x/time is a Go-team-maintained supplement
	// to the standard library. Justification per CLAUDE.md: using it
	// avoids reimplementing a token-bucket primitive whose edge cases
	// (Reserve, Allow vs AllowN, clock monotonicity) are fiddly enough
	// to get wrong, and the module has no transitive dependencies.
	"golang.org/x/time/rate"
)

// Limiter applies a per-caller token-bucket limit.
//
// Each distinct caller identity gets its own *rate.Limiter so a noisy
// neighbour cannot starve other callers. Limiters are created on
// first use and never expire during the process's lifetime; memory
// growth is bounded by the number of distinct bearer-token identities
// the gateway has ever seen, which in practice is O(N) in the
// cluster's service accounts.
//
// A Limiter is safe for concurrent use.
type Limiter struct {
	// perMinute is the bucket refill rate expressed in requests per
	// minute, kept around so Allow can log/expose the configured
	// limit without callers re-deriving it from limit and burst.
	perMinute int

	// limit is the rate the underlying token bucket refills at. For a
	// "60 requests per minute" config this is 1 token per second.
	limit rate.Limit

	// burst is the bucket size. Set equal to perMinute so a caller
	// can consume a full minute of quota in a burst, which matches
	// typical dashboard usage patterns.
	burst int

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

// New returns a Limiter configured for perMinute requests per caller.
// A non-positive perMinute disables limiting entirely — Allow always
// returns true. That makes it cheap for operators to opt out without
// threading a nil limiter through the call graph.
func New(perMinute int) *Limiter {
	if perMinute <= 0 {
		return &Limiter{perMinute: 0}
	}
	// Convert requests-per-minute into a per-second rate. Using
	// Every keeps the conversion exact for any perMinute value;
	// dividing would introduce floating-point drift over long-running
	// processes.
	interval := time.Minute / time.Duration(perMinute)
	return &Limiter{
		perMinute: perMinute,
		limit:     rate.Every(interval),
		burst:     perMinute,
		limiters:  make(map[string]*rate.Limiter),
	}
}

// Enabled reports whether the Limiter will do any work. Handy for
// middleware code that wants to skip a metric increment when the
// rate limiter is intentionally off.
func (l *Limiter) Enabled() bool { return l.perMinute > 0 }

// PerMinute returns the configured requests-per-minute limit. Returns
// 0 when the limiter is disabled.
func (l *Limiter) PerMinute() int { return l.perMinute }

// Allow consumes one token from the bucket associated with caller.
// Returns true when the request is permitted, false when the caller
// has exceeded their quota for the current window.
func (l *Limiter) Allow(caller string) bool {
	if l.perMinute <= 0 {
		return true
	}
	l.mu.Lock()
	rl, ok := l.limiters[caller]
	if !ok {
		rl = rate.NewLimiter(l.limit, l.burst)
		l.limiters[caller] = rl
	}
	l.mu.Unlock()
	return rl.Allow()
}
