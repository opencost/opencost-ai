// Package apiv1 defines the stable, exported request and response types
// for the opencost-ai gateway HTTP API (version v1).
//
// Types in this package must not carry behaviour: they exist so that SDKs
// and clients can vendor them without pulling in gateway internals. Any
// breaking change to a field that ships in a tagged release requires a
// new /vN package and a deprecation window on the previous one.
//
// See docs/architecture.md §7.1–§7.5 for the on-the-wire contract and
// docs/api.md for endpoint-level documentation.
package apiv1
