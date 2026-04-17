// Package server wires the gateway's HTTP handlers together.
//
// Responsibilities, per docs/architecture.md §6.1 and §7.1:
//
//   - Register the v1 endpoint tree (POST /v1/ask, GET /v1/tools,
//     GET /v1/models, plus the pre-existing /v1/health from
//     cmd/gateway).
//   - Apply the bearer-token auth middleware from internal/auth to
//     every /v1 endpoint except /v1/health (liveness must remain
//     callable by the kubelet without credentials).
//   - Translate bridge.Client failures into RFC 7807 problem+json.
//   - Enforce the request-body ceiling from config.MaxRequestBytes.
//
// The package deliberately does not own business logic: prompt
// construction, audit logging, and rate limiting each live in their
// own internal/* packages and are composed in by cmd/gateway.
package server
