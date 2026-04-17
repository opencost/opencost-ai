// Package audit emits the gateway's structured audit log.
//
// Each completed /v1/ask request produces exactly one JSON object on
// stdout (or whatever io.Writer the Logger was constructed with). The
// shape is stable and documented on Event.
//
// CLAUDE.md non-negotiable: the query text and completion text are
// never logged unless the operator sets OPENCOST_AI_AUDIT_LOG_QUERY=true.
// Token counts, tool-call names, and per-tool durations are always
// logged — they are operational data, not caller payload.
//
// Caller identity is derived from the bearer token via a one-way
// hash so the audit trail correlates repeated calls from the same
// credential without revealing or enabling replay of the credential
// itself. The hash function and prefix length are part of the
// audit-line schema: do not change them without updating the docs.
package audit
