package auth

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencost/opencost-ai/pkg/apiv1"
)

// stubValidator lets middleware tests exercise each outcome without
// spinning up a real Source per case.
type stubValidator struct{ err error }

func (s stubValidator) Validate(string) error { return s.err }

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMiddleware_ValidToken(t *testing.T) {
	t.Parallel()
	h := Middleware(stubValidator{err: nil}, discardLogger())(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer the-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Fatalf("body = %q, want ok", got)
	}
}

func TestMiddleware_MissingHeader(t *testing.T) {
	t.Parallel()
	h := Middleware(stubValidator{err: nil}, discardLogger())(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertProblem(t, rec, http.StatusUnauthorized, "missing Authorization header")
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Errorf("WWW-Authenticate header missing on 401")
	}
}

func TestMiddleware_WrongScheme(t *testing.T) {
	t.Parallel()
	h := Middleware(stubValidator{err: nil}, discardLogger())(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertProblem(t, rec, http.StatusUnauthorized, "unsupported authorization scheme")
}

func TestMiddleware_EmptyBearerToken(t *testing.T) {
	t.Parallel()
	h := Middleware(stubValidator{err: nil}, discardLogger())(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer   ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertProblem(t, rec, http.StatusUnauthorized, "empty bearer token")
}

func TestMiddleware_MalformedHeader(t *testing.T) {
	t.Parallel()
	h := Middleware(stubValidator{err: nil}, discardLogger())(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "NoSpaceAfterScheme")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertProblem(t, rec, http.StatusUnauthorized, "malformed Authorization header")
}

func TestMiddleware_InvalidToken(t *testing.T) {
	t.Parallel()
	h := Middleware(stubValidator{err: ErrInvalidToken}, discardLogger())(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertProblem(t, rec, http.StatusUnauthorized, "invalid bearer token")
}

func TestMiddleware_NoTokenConfigured(t *testing.T) {
	t.Parallel()
	h := Middleware(stubValidator{err: ErrNoToken}, discardLogger())(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertProblem(t, rec, http.StatusServiceUnavailable,
		"authentication is not configured on this gateway")
}

func TestMiddleware_ValidatorInternalError(t *testing.T) {
	t.Parallel()
	h := Middleware(stubValidator{err: errors.New("disk on fire")}, discardLogger())(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Crucially, the raw error string "disk on fire" must not leak
	// to the caller — this is the non-negotiable in CLAUDE.md.
	assertProblem(t, rec, http.StatusInternalServerError, "authentication check failed")
	if got := rec.Body.String(); contains(got, "disk on fire") {
		t.Fatalf("raw error leaked to client: %s", got)
	}
}

// End-to-end with a real Source: valid token, then rotation invalidates it.
func TestMiddleware_FileBackedEndToEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "v1")

	src := NewSource(path)
	h := Middleware(src, discardLogger())(okHandler())

	call := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("v1"); rec.Code != http.StatusOK {
		t.Fatalf("v1 token via middleware: status %d", rec.Code)
	}
	writeToken(t, path, "v2")
	if rec := call("v1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("rotated: v1 should 401, got %d", rec.Code)
	}
	if rec := call("v2"); rec.Code != http.StatusOK {
		t.Fatalf("rotated: v2 should 200, got %d", rec.Code)
	}
}

// assertProblem verifies the response is a well-formed problem+json
// with the expected status and a detail that includes detailSub.
func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, detailSub string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != apiv1.ProblemContentType {
		t.Errorf("content-type = %q, want %q", ct, apiv1.ProblemContentType)
	}
	var prob apiv1.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatalf("decode problem: %v; body=%s", err, rec.Body.String())
	}
	if prob.Status != wantStatus {
		t.Errorf("problem.status = %d, want %d", prob.Status, wantStatus)
	}
	if detailSub != "" && !contains(prob.Detail, detailSub) {
		t.Errorf("problem.detail = %q, missing substring %q", prob.Detail, detailSub)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

// indexOf is stdlib-equivalent but kept local so this test file has
// no third-party-style imports — matches the stdlib-only style of
// internal packages.
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Smoke-test that a world-readable token file is not the happy path.
// The Source does not enforce mode itself — deployment does — but
// this test fails loudly if the test fixtures ever ship with a 0644
// fixture, which would get copied into production by accident.
func TestSourceFixturePermissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "x")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("test fixture has group/other bits set: %v", mode)
	}
}
