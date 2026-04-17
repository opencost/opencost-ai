// Package requestid provides the per-request correlation token that
// all gateway packages share.
//
// Three packages consume the ID:
//
//   - internal/server stamps X-Request-ID on the response and assigns
//     a new ID when the caller did not supply one.
//   - internal/auth reads the ID so its 401/503 problem+json documents
//     correlate with the gateway's audit log without having to parse
//     Instance.
//   - internal/audit (future) stamps it on every structured log line.
//
// Keeping the ctx key and getter in one tiny stdlib-only package
// avoids a circular dependency between server and auth, and matches
// the review guidance on opencost/opencost-ai#5.
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// HeaderName is the HTTP header the gateway uses for in-bound
// and out-bound request-ID propagation. Exported as a constant so
// middleware and tests never type the string literal twice.
const HeaderName = "X-Request-ID"

// maxCallerIDLen bounds caller-supplied X-Request-ID values. Long
// enough for UUIDs, W3C trace IDs, and OpenTelemetry span IDs;
// short enough that a rogue client cannot cheaply balloon log lines.
const maxCallerIDLen = 128

// ctxKey is unexported so values put on the context can only be
// read via FromContext. Kept a distinct type so the key does not
// collide with any other string-typed key.
type ctxKey struct{}

var key = ctxKey{}

// WithValue returns a context carrying id. Pass the returned ctx to
// downstream handlers so they can stamp the ID on logs and error
// responses.
func WithValue(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key, id)
}

// FromContext returns the request ID stored on ctx, or the empty
// string when no middleware has set one. An empty string is a
// valid signal: callers rendering a problem+json should omit the
// request_id field rather than emit "request_id":"" on the wire.
func FromContext(ctx context.Context) string {
	v, _ := ctx.Value(key).(string)
	return v
}

// New returns 16 random hex chars. Not a UUID — we do not need
// the RFC 4122 formatting guarantees — just a collision-resistant
// correlation token.
func New() string {
	var buf [8]byte
	// crypto/rand.Read on an 8-byte buffer cannot fail on any
	// supported platform; fall back to a literal on the impossible
	// error path so the request still carries *some* ID.
	if _, err := rand.Read(buf[:]); err != nil {
		return "req-fallback"
	}
	return hex.EncodeToString(buf[:])
}

// Middleware assigns a request ID to every incoming request,
// writes it on the response as X-Request-ID, and stores it on the
// request ctx for downstream handlers to read via FromContext.
// Caller-supplied X-Request-ID values are honoured when they fit
// within maxCallerIDLen, so a distributed trace stitches together.
func Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderName)
			if id == "" || len(id) > maxCallerIDLen {
				id = New()
			}
			w.Header().Set(HeaderName, id)
			next.ServeHTTP(w, r.WithContext(WithValue(r.Context(), id)))
		})
	}
}
