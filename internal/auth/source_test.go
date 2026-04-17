package auth

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// writeToken writes contents to path and bumps the mtime forward
// from any prior state so reloadIfChanged will pick the change up
// even when the test runs fast enough to share a filesystem tick.
func writeToken(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// Advance mtime by a full second to defeat coarse-grained
	// filesystem timestamp resolution (ext4 without nsec, macOS HFS+).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestSource_ValidToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "super-secret-token")

	s := NewSource(path)
	if err := s.Validate("super-secret-token"); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func TestSource_StripsTrailingWhitespace(t *testing.T) {
	t.Parallel()
	// Secret files written by `echo "token" > file` include a
	// trailing newline; a common source of "but the token matches!"
	// incidents in prod. Source must trim.
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "token-with-newline\n")

	s := NewSource(path)
	if err := s.Validate("token-with-newline"); err != nil {
		t.Fatalf("expected token to match after trim: %v", err)
	}
}

func TestSource_InvalidToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "right-token")

	s := NewSource(path)
	err := s.Validate("wrong-token")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

func TestSource_EmptyTokenFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "")

	s := NewSource(path)
	err := s.Validate("anything")
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("want ErrNoToken on empty file, got %v", err)
	}
}

func TestSource_WhitespaceOnlyTokenFile(t *testing.T) {
	t.Parallel()
	// A file with only a trailing newline is effectively empty once
	// trimmed; treat it the same as an empty file so operators who
	// `touch` the secret by accident get a clear signal.
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "\n\n")

	s := NewSource(path)
	err := s.Validate("x")
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("want ErrNoToken on whitespace-only file, got %v", err)
	}
}

func TestSource_MissingFile(t *testing.T) {
	t.Parallel()
	s := NewSource(filepath.Join(t.TempDir(), "does-not-exist"))
	err := s.Validate("anything")
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("want ErrNoToken for missing file, got %v", err)
	}
}

func TestSource_FileRotation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "v1-token")

	s := NewSource(path)
	if err := s.Validate("v1-token"); err != nil {
		t.Fatalf("v1 token should validate: %v", err)
	}
	// Simulate a Kubernetes Secret rotation: new contents, new mtime.
	writeToken(t, path, "v2-token")

	if err := s.Validate("v1-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("post-rotation v1 token should fail with ErrInvalidToken, got %v", err)
	}
	if err := s.Validate("v2-token"); err != nil {
		t.Fatalf("post-rotation v2 token should succeed: %v", err)
	}
}

func TestSource_RotationThenEmpty(t *testing.T) {
	t.Parallel()
	// An operator who rotates to an empty secret has misconfigured
	// the gateway. Subsequent requests must fail with ErrNoToken,
	// not silently keep honouring the stale token.
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "initial")

	s := NewSource(path)
	if err := s.Validate("initial"); err != nil {
		t.Fatalf("initial validate: %v", err)
	}
	writeToken(t, path, "")
	if err := s.Validate("initial"); !errors.Is(err, ErrNoToken) {
		t.Fatalf("want ErrNoToken after emptying file, got %v", err)
	}
}

func TestSource_ConcurrentValidate(t *testing.T) {
	t.Parallel()
	// Guard against a regression where the read/write lock is taken
	// in the wrong order during reload. Run many Validate goroutines
	// while the file rotates underneath.
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "tok-a")

	s := NewSource(path)

	const workers = 32
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// Ignore the error — we only care that this
					// does not deadlock or race under -race.
					_ = s.Validate("tok-a")
					_ = s.Validate("tok-b")
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	writeToken(t, path, "tok-b")
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
}
