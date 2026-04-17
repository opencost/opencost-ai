package requestid

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFromContext_EmptyWhenUnset(t *testing.T) {
	t.Parallel()
	if got := FromContext(context.Background()); got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestWithValue_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := WithValue(context.Background(), "abc123")
	if got := FromContext(ctx); got != "abc123" {
		t.Fatalf("got %q, want abc123", got)
	}
}

func TestNew_HexShape(t *testing.T) {
	t.Parallel()
	id := New()
	if len(id) != 16 {
		t.Fatalf("len = %d, want 16", len(id))
	}
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("non-hex char %q in %s", r, id)
		}
	}
}

func TestNew_IsRandom(t *testing.T) {
	t.Parallel()
	a := New()
	b := New()
	if a == b {
		t.Fatalf("two successive IDs collided: %s", a)
	}
}

func TestMiddleware_GeneratesWhenAbsent(t *testing.T) {
	t.Parallel()
	var seen string
	h := Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context())
		_, _ = io.WriteString(w, "ok")
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen == "" {
		t.Errorf("ctx id not populated")
	}
	if got := rec.Header().Get(HeaderName); got != seen {
		t.Errorf("header %q != ctx %q", got, seen)
	}
}

func TestMiddleware_HonoursCallerSupplied(t *testing.T) {
	t.Parallel()
	var seen string
	h := Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(HeaderName, "trace-xyz")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "trace-xyz" {
		t.Errorf("ctx id = %q, want caller value", seen)
	}
	if got := rec.Header().Get(HeaderName); got != "trace-xyz" {
		t.Errorf("header echoed = %q", got)
	}
}

func TestMiddleware_RejectsOverlongCallerID(t *testing.T) {
	t.Parallel()
	var seen string
	h := Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(HeaderName, strings.Repeat("a", maxCallerIDLen+1))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if len(seen) != 16 {
		t.Errorf("oversized caller ID not replaced: seen len %d", len(seen))
	}
}
