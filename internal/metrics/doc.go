// Package metrics exposes the gateway's Prometheus metrics per
// docs/architecture.md §7.6.
//
// The package is a narrow wrapper over prometheus/client_golang.
// Going through the reference implementation rather than a bespoke
// text-exposition emitter is a deliberate trade per CLAUDE.md: the
// Prometheus text and OpenMetrics formats are stable but not tiny,
// and label-value escaping, exemplar handling, and the coming
// native-histogram variants are the kind of thing that reward a
// well-tested library over 500 lines of custom code. The wrapper
// types here keep the call sites in internal/server unaware of
// which client library backs them, so a future swap (OpenTelemetry
// exporter, multi-registry split) stays contained.
//
// Metric names, label sets, bucket boundaries, and HELP strings are
// a public contract with operators: grep stability is the rule, not
// a nice-to-have. Any change here is a documented metric-API change
// and requires a note in docs/architecture.md §7.6.
package metrics
