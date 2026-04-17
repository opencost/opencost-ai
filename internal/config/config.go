package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env var names. Kept as exported constants so tests and ops docs can
// reference them without string-matching on literals.
const (
	EnvBridgeURL        = "OPENCOST_AI_BRIDGE_URL"
	EnvListenAddr       = "OPENCOST_AI_LISTEN_ADDR"
	EnvDefaultModel     = "OPENCOST_AI_DEFAULT_MODEL"
	EnvRequestTimeout   = "OPENCOST_AI_REQUEST_TIMEOUT"
	EnvMaxRequestBytes  = "OPENCOST_AI_MAX_REQUEST_BYTES"
	EnvAuditLogQuery    = "OPENCOST_AI_AUDIT_LOG_QUERY"
	EnvAuthTokenFile    = "OPENCOST_AI_AUTH_TOKEN_FILE"
)

// Defaults for each field. Mirrors docs/architecture.md §7.7.
const (
	DefaultBridgeURL       = "http://ollama-mcp-bridge:8000"
	DefaultListenAddr      = ":8080"
	DefaultModel           = "qwen2.5:7b-instruct"
	DefaultRequestTimeout  = 120 * time.Second
	DefaultMaxRequestBytes = 8192
	DefaultAuditLogQuery   = false
	DefaultAuthTokenFile   = "/var/run/secrets/opencost-ai/token"
)

// Config captures the full runtime configuration of the gateway.
//
// A zero-valued Config is not usable; callers should construct via
// DefaultConfig or Load, then call Validate before handing it to the
// server wire-up.
type Config struct {
	// BridgeURL is the base URL of the upstream ollama-mcp-bridge
	// instance. Must be http or https and include a host.
	BridgeURL string

	// ListenAddr is the address the HTTP server binds to, in
	// host:port form. An empty host binds all interfaces.
	ListenAddr string

	// DefaultModel names the Ollama model to use when an AskRequest
	// does not specify one.
	DefaultModel string

	// RequestTimeout is the per-request deadline applied by the
	// gateway to upstream calls. Must be positive.
	RequestTimeout time.Duration

	// MaxRequestBytes is the maximum size of a request body the
	// gateway will accept. Must be positive. Applies to the JSON
	// envelope; Query length is validated separately at the handler.
	MaxRequestBytes int64

	// AuditLogQuery, when true, includes the raw query text and
	// completion in the audit log. Off by default — see CLAUDE.md
	// non-negotiables.
	AuditLogQuery bool

	// AuthTokenFile is the path to the bearer-token secret file. The
	// auth middleware watches this path for rotation.
	AuthTokenFile string
}

// DefaultConfig returns a Config populated with the documented
// defaults. Useful as a starting point in tests and as the fallback
// when no environment is set.
func DefaultConfig() Config {
	return Config{
		BridgeURL:       DefaultBridgeURL,
		ListenAddr:      DefaultListenAddr,
		DefaultModel:    DefaultModel,
		RequestTimeout:  DefaultRequestTimeout,
		MaxRequestBytes: DefaultMaxRequestBytes,
		AuditLogQuery:   DefaultAuditLogQuery,
		AuthTokenFile:   DefaultAuthTokenFile,
	}
}

// Getenv abstracts os.LookupEnv so Load is testable without mutating
// the real process environment. Implementations must return (value,
// true) when the variable is set (including empty strings).
type Getenv func(key string) (string, bool)

// OSGetenv is the production Getenv implementation, reading from the
// running process environment.
func OSGetenv(key string) (string, bool) { return os.LookupEnv(key) }

// Load builds a Config by starting from DefaultConfig and overlaying
// any values supplied by get. An unset variable leaves the default
// in place; a set-but-empty variable is rejected for every string
// and numeric field. For OPENCOST_AI_AUDIT_LOG_QUERY an empty value
// is treated as unset (leaving the default in place), because many
// Kubernetes/shell workflows export the var as "" to mean "not
// configured" rather than an explicit off.
//
// Load does not call Validate; callers should invoke Validate on the
// returned Config before use so that a single error path covers both
// parse and semantic failures.
func Load(get Getenv) (Config, error) {
	if get == nil {
		get = OSGetenv
	}
	cfg := DefaultConfig()

	if v, ok := get(EnvBridgeURL); ok {
		if v == "" {
			return cfg, fmt.Errorf("%s: must not be empty", EnvBridgeURL)
		}
		cfg.BridgeURL = v
	}
	if v, ok := get(EnvListenAddr); ok {
		if v == "" {
			return cfg, fmt.Errorf("%s: must not be empty", EnvListenAddr)
		}
		cfg.ListenAddr = v
	}
	if v, ok := get(EnvDefaultModel); ok {
		if v == "" {
			return cfg, fmt.Errorf("%s: must not be empty", EnvDefaultModel)
		}
		cfg.DefaultModel = v
	}
	if v, ok := get(EnvRequestTimeout); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("%s: parse duration %q: %w", EnvRequestTimeout, v, err)
		}
		cfg.RequestTimeout = d
	}
	if v, ok := get(EnvMaxRequestBytes); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("%s: parse int %q: %w", EnvMaxRequestBytes, v, err)
		}
		cfg.MaxRequestBytes = n
	}
	if v, ok := get(EnvAuditLogQuery); ok && v != "" {
		b, err := parseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", EnvAuditLogQuery, err)
		}
		cfg.AuditLogQuery = b
	}
	if v, ok := get(EnvAuthTokenFile); ok {
		if v == "" {
			return cfg, fmt.Errorf("%s: must not be empty", EnvAuthTokenFile)
		}
		cfg.AuthTokenFile = v
	}
	return cfg, nil
}

// Validate checks Config for semantic errors (bad URLs, non-positive
// durations, etc.). It is separate from Load so callers that
// synthesise a Config in tests still get the same checks.
func (c Config) Validate() error {
	var errs []error

	u, err := url.Parse(c.BridgeURL)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("bridge URL %q: %w", c.BridgeURL, err))
	case u.Scheme != "http" && u.Scheme != "https":
		errs = append(errs, fmt.Errorf("bridge URL %q: scheme must be http or https", c.BridgeURL))
	case u.Host == "":
		errs = append(errs, fmt.Errorf("bridge URL %q: missing host", c.BridgeURL))
	}

	if c.ListenAddr == "" {
		errs = append(errs, errors.New("listen addr: must not be empty"))
	} else if err := validateListenAddr(c.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("listen addr %q: %w", c.ListenAddr, err))
	}

	if c.DefaultModel == "" {
		errs = append(errs, errors.New("default model: must not be empty"))
	}
	if c.RequestTimeout <= 0 {
		errs = append(errs, fmt.Errorf("request timeout %s: must be positive", c.RequestTimeout))
	}
	if c.MaxRequestBytes <= 0 {
		errs = append(errs, fmt.Errorf("max request bytes %d: must be positive", c.MaxRequestBytes))
	}
	if c.AuthTokenFile == "" {
		errs = append(errs, errors.New("auth token file: must not be empty"))
	}

	return errors.Join(errs...)
}

// validateListenAddr mirrors the checks net.Listen performs before
// binding, so misconfiguration surfaces at Validate time rather than
// at server start. Accepts the same input shapes net.Listen does on
// a "tcp" network: ":8080", "host:8080", "[::1]:8080", "0.0.0.0:8080".
// Rejects "host:" (empty port) and bare hostnames.
func validateListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if port == "" {
		return errors.New("missing port")
	}
	if _, perr := net.LookupPort("tcp", port); perr != nil {
		return fmt.Errorf("invalid port %q: %w", port, perr)
	}
	// host may be empty (":8080" means all interfaces); that's fine.
	_ = host
	return nil
}

// parseBool accepts the usual true/false spellings plus 1/0, yes/no,
// on/off. Anything else is an error — we refuse to guess.
func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid bool %q", v)
	}
}
