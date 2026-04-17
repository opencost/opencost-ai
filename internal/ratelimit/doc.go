// Package ratelimit implements the per-caller token-bucket rate
// limiter documented in docs/architecture.md §7.5.
//
// The limiter keys buckets by the audit-log caller identity (a hash
// of the bearer token), which means a single credential presented
// from many IPs still counts as one caller and rotating credentials
// naturally resets the bucket. Per-IP limiting is explicitly out of
// scope: the gateway is meant to run behind a NetworkPolicy that
// already constrains who can reach it at all.
//
// The limit is expressed in requests per minute. Bursts of up to one
// minute's worth of capacity are permitted so a dashboard opening a
// handful of queries in quick succession is not throttled.
//
// golang.org/x/time/rate is a tiny, stdlib-adjacent module from the
// Go team; using it keeps the token-bucket implementation out of this
// repo, and the LOC budget in CLAUDE.md is better spent elsewhere.
package ratelimit
