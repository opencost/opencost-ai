// Package config loads and validates gateway configuration from
// environment variables.
//
// The schema is defined in docs/architecture.md §7.7. Defaults are
// duplicated there and in DefaultConfig; the two must stay in sync.
// This package has no third-party dependencies by design — env-var
// parsing stays in stdlib until it genuinely outgrows it.
package config
