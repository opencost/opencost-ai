// Package auth implements the gateway's bearer-token authentication.
//
// The v0.1 scheme described in docs/architecture.md §7.3 is a single
// static bearer token, read from a file mounted from a Kubernetes
// Secret. The token is compared against the Authorization header in
// constant time; the file is watched for rotation by re-reading its
// contents when the inode's mtime advances.
//
// The package exposes two surfaces:
//
//   - Source: an mtime-triggered reloader around the token file. It
//     performs a constant-time compare against a candidate token and
//     reports "no token configured" when the file is present but empty
//     so operators can distinguish a missing-secret deployment bug from
//     a credential mismatch.
//   - Middleware: an http.Handler wrapper that extracts the bearer
//     token from the Authorization header, defers to Source for
//     validation, and emits RFC 7807 problem+json on failure.
//
// Nothing in this package imports net/http beyond the middleware
// helper; the Source itself is transport-agnostic so it can be reused
// from a future gRPC or SSE path.
package auth
