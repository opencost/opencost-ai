package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// ctxKey is an unexported type to keep context keys local to this
// package; anyone wanting the request ID calls requestIDFromContext.
type ctxKey struct{}

// requestIDKey is the single context value we propagate per request.
// Keeping it a package-level var (not exported) lets middleware in
// this package set it while forcing external callers through the
// getter.
var requestIDKey = ctxKey{}

// withRequestID attaches id to ctx so downstream code can stamp it
// into audit entries and problem+json responses.
func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// requestIDFromContext returns the request ID stored on ctx, or ""
// when no middleware set one.
func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// requestIDMiddleware assigns a per-request hex ID, propagates it on
// the response as X-Request-ID, and stores it on the request ctx.
// If the caller supplied X-Request-ID we honour it so logs correlate
// across a distributed trace.
func requestIDMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" || len(id) > 128 {
				id = newRequestID()
			}
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
		})
	}
}

// newRequestID returns 16 random hex chars. Not a UUID because we
// do not need the RFC 4122 formatting guarantees — we just need a
// collision-resistant correlation token.
func newRequestID() string {
	var buf [8]byte
	// crypto/rand.Read on a 16-byte buffer cannot fail on any
	// supported platform; fall back to a literal on the impossible
	// error path so the request still carries *some* ID.
	if _, err := rand.Read(buf[:]); err != nil {
		return "req-fallback"
	}
	return hex.EncodeToString(buf[:])
}
