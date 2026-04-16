package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// mapEnv turns a static map into a Getenv closure for tests. Present
// keys are returned with ok=true even when the value is empty, which
// matches os.LookupEnv semantics.
func mapEnv(m map[string]string) Getenv {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestDefaultConfig_ValidatesClean(t *testing.T) {
	t.Parallel()
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{
			name: "empty env yields defaults",
			env:  nil,
			want: DefaultConfig(),
		},
		{
			name: "full override",
			env: map[string]string{
				EnvBridgeURL:       "https://bridge.internal:9000",
				EnvListenAddr:      "127.0.0.1:9090",
				EnvDefaultModel:    "mistral-nemo:12b",
				EnvRequestTimeout:  "30s",
				EnvMaxRequestBytes: "16384",
				EnvAuditLogQuery:   "true",
				EnvAuthTokenFile:   "/etc/opencost-ai/token",
			},
			want: Config{
				BridgeURL:       "https://bridge.internal:9000",
				ListenAddr:      "127.0.0.1:9090",
				DefaultModel:    "mistral-nemo:12b",
				RequestTimeout:  30 * time.Second,
				MaxRequestBytes: 16384,
				AuditLogQuery:   true,
				AuthTokenFile:   "/etc/opencost-ai/token",
			},
		},
		{
			name: "partial override leaves other defaults in place",
			env: map[string]string{
				EnvDefaultModel: "llama3.1:8b-instruct",
			},
			want: func() Config {
				c := DefaultConfig()
				c.DefaultModel = "llama3.1:8b-instruct"
				return c
			}(),
		},
		{
			name: "audit flag accepts 1",
			env:  map[string]string{EnvAuditLogQuery: "1"},
			want: func() Config {
				c := DefaultConfig()
				c.AuditLogQuery = true
				return c
			}(),
		},
		{
			name: "audit flag accepts yes",
			env:  map[string]string{EnvAuditLogQuery: "YES"},
			want: func() Config {
				c := DefaultConfig()
				c.AuditLogQuery = true
				return c
			}(),
		},
		{
			name: "audit flag accepts off",
			env:  map[string]string{EnvAuditLogQuery: "off"},
			want: DefaultConfig(),
		},
		{
			name:    "empty bridge URL rejected",
			env:     map[string]string{EnvBridgeURL: ""},
			wantErr: "BRIDGE_URL: must not be empty",
		},
		{
			name:    "empty listen addr rejected",
			env:     map[string]string{EnvListenAddr: ""},
			wantErr: "LISTEN_ADDR: must not be empty",
		},
		{
			name:    "empty default model rejected",
			env:     map[string]string{EnvDefaultModel: ""},
			wantErr: "DEFAULT_MODEL: must not be empty",
		},
		{
			name:    "empty token file rejected",
			env:     map[string]string{EnvAuthTokenFile: ""},
			wantErr: "AUTH_TOKEN_FILE: must not be empty",
		},
		{
			name:    "bad timeout",
			env:     map[string]string{EnvRequestTimeout: "forever"},
			wantErr: "REQUEST_TIMEOUT",
		},
		{
			name:    "bad max bytes",
			env:     map[string]string{EnvMaxRequestBytes: "big"},
			wantErr: "MAX_REQUEST_BYTES",
		},
		{
			name:    "bad bool",
			env:     map[string]string{EnvAuditLogQuery: "maybe"},
			wantErr: "AUDIT_LOG_QUERY",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Load(mapEnv(tc.env))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("config mismatch:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

func TestLoad_NilGetenvFallsBackToOS(t *testing.T) {
	// Passing nil must not panic and must return a valid Config using
	// real process env. We unset all OPENCOST_AI_* vars (scheduling
	// restore via t.Setenv + os.Unsetenv) so the load sees a clean
	// environment and returns DefaultConfig.
	keys := []string{
		EnvBridgeURL, EnvListenAddr, EnvDefaultModel, EnvRequestTimeout,
		EnvMaxRequestBytes, EnvAuditLogQuery, EnvAuthTokenFile,
	}
	for _, k := range keys {
		t.Setenv(k, "") // registers cleanup to restore the prior value
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unsetenv %s: %v", k, err)
		}
	}
	got, err := Load(nil)
	if err != nil {
		t.Fatalf("Load(nil) with clean env: %v", err)
	}
	if got != DefaultConfig() {
		t.Fatalf("expected defaults, got %+v", got)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	base := DefaultConfig()

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "ok",
			mutate:  func(*Config) {},
			wantErr: "",
		},
		{
			name:    "bad scheme",
			mutate:  func(c *Config) { c.BridgeURL = "ftp://example.com" },
			wantErr: "scheme must be http or https",
		},
		{
			name:    "missing host",
			mutate:  func(c *Config) { c.BridgeURL = "http://" },
			wantErr: "missing host",
		},
		{
			name:    "unparseable URL",
			mutate:  func(c *Config) { c.BridgeURL = "http://[::1" },
			wantErr: "bridge URL",
		},
		{
			name:    "empty listen addr",
			mutate:  func(c *Config) { c.ListenAddr = "" },
			wantErr: "listen addr",
		},
		{
			name:    "listen addr without port",
			mutate:  func(c *Config) { c.ListenAddr = "localhost" },
			wantErr: "missing port",
		},
		{
			name:    "empty model",
			mutate:  func(c *Config) { c.DefaultModel = "" },
			wantErr: "default model",
		},
		{
			name:    "zero timeout",
			mutate:  func(c *Config) { c.RequestTimeout = 0 },
			wantErr: "request timeout",
		},
		{
			name:    "negative timeout",
			mutate:  func(c *Config) { c.RequestTimeout = -1 },
			wantErr: "request timeout",
		},
		{
			name:    "zero max bytes",
			mutate:  func(c *Config) { c.MaxRequestBytes = 0 },
			wantErr: "max request bytes",
		},
		{
			name:    "empty token file",
			mutate:  func(c *Config) { c.AuthTokenFile = "" },
			wantErr: "auth token file",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := base
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidate_AggregatesErrors(t *testing.T) {
	t.Parallel()
	c := Config{
		BridgeURL:       "ftp://bad",
		ListenAddr:      "",
		DefaultModel:    "",
		RequestTimeout:  0,
		MaxRequestBytes: 0,
		AuthTokenFile:   "",
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected aggregated error, got nil")
	}
	msg := err.Error()
	for _, needle := range []string{
		"bridge URL", "listen addr", "default model",
		"request timeout", "max request bytes", "auth token file",
	} {
		if !strings.Contains(msg, needle) {
			t.Errorf("aggregated error missing %q: %s", needle, msg)
		}
	}
}
