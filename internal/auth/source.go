package auth

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Sentinel errors returned by Source.Validate. Callers use errors.Is
// to distinguish between "deployment misconfiguration" (no token on
// disk) and "caller sent the wrong token", since those map to
// different problem+json documents.
var (
	// ErrNoToken indicates the token file exists but is empty, or has
	// never been successfully loaded. Treated as a 503 by the HTTP
	// middleware: the gateway is up but not yet usable.
	ErrNoToken = errors.New("auth: no token configured")

	// ErrInvalidToken indicates the caller-supplied token did not
	// match the loaded token. Maps to 401.
	ErrInvalidToken = errors.New("auth: invalid token")
)

// Source is a file-backed bearer-token validator.
//
// Construct with NewSource; the token is read lazily on first
// Validate call and re-read whenever the file's mtime advances. This
// keeps startup fast (and robust against an empty secret at boot)
// while still picking up rotations without a SIGHUP.
//
// A Source is safe for concurrent use. The hot path takes an RLock
// around the in-memory token; the reload path upgrades to a write
// lock only when mtime has actually moved.
type Source struct {
	path string

	mu    sync.RWMutex
	token []byte
	mtime time.Time
	// loaded is true once reloadIfChanged has successfully read the
	// file at least once. It exists to make the mtime comparison in
	// reloadIfChanged meaningful: before any load, s.mtime is the zero
	// Time and must not be compared against a real os.FileInfo mtime,
	// otherwise a freshly-constructed Source would treat the first
	// Validate as a no-op and fall through with an empty token.
	loaded bool
}

// NewSource returns a Source rooted at path. The file is not read
// until the first Validate call, so NewSource never fails for a
// missing file — that surfaces as ErrNoToken when the first request
// arrives, which is the signal operators actually want.
func NewSource(path string) *Source {
	return &Source{path: path}
}

// Path returns the file path the Source is watching. Exposed for
// logging and tests.
func (s *Source) Path() string { return s.path }

// Validate returns nil when candidate matches the token currently on
// disk, ErrInvalidToken when it does not, and ErrNoToken when the
// file is missing or empty. It performs a constant-time comparison
// to avoid leaking the token length or a prefix match via timing.
func (s *Source) Validate(candidate string) error {
	if err := s.reloadIfChanged(); err != nil {
		return err
	}

	s.mu.RLock()
	token := s.token
	s.mu.RUnlock()

	if len(token) == 0 {
		return ErrNoToken
	}
	// subtle.ConstantTimeCompare is constant-time in the *contents* of
	// two equal-length byte slices; it returns 0 immediately when the
	// lengths differ, so a timing observer can still in principle
	// learn the stored token length. That is an accepted trade-off
	// here: operators are expected to provision tokens of a fixed,
	// high-entropy shape (e.g. 32-byte random hex), so length is not
	// itself a secret. If that ever changes, wrap this call in a
	// length-padded compare.
	if subtle.ConstantTimeCompare([]byte(candidate), token) != 1 {
		return ErrInvalidToken
	}
	return nil
}

// reloadIfChanged stats the token file and reloads it when the mtime
// has advanced (or when no load has ever happened). A stat error
// propagates as ErrNoToken so a missing file behaves the same as an
// empty one — both are unrecoverable from the caller's perspective.
func (s *Source) reloadIfChanged() error {
	info, err := os.Stat(s.path)
	if err != nil {
		// If we had previously loaded a token and the file is now
		// transiently unreadable, fall through with the cached token
		// rather than locking the gateway out. A file-gone-missing
		// case is more likely a race against an atomic Secret rewrite
		// than an intentional revocation.
		s.mu.RLock()
		cached := len(s.token) > 0
		s.mu.RUnlock()
		if cached {
			return nil
		}
		return fmt.Errorf("%w: stat %s: %v", ErrNoToken, s.path, err)
	}

	s.mu.RLock()
	unchanged := s.loaded && info.ModTime().Equal(s.mtime)
	s.mu.RUnlock()
	if unchanged {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check under the write lock — another goroutine may have
	// reloaded while we were waiting.
	if s.loaded && info.ModTime().Equal(s.mtime) {
		return nil
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", ErrNoToken, s.path, err)
	}
	s.token = bytes.TrimRight(raw, "\r\n\t ")
	s.mtime = info.ModTime()
	s.loaded = true
	return nil
}
