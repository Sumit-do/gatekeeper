package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validJSON = `{
  "listen": ":8080",
  "admin": ":9090",
  "routes": [
    {
      "path_prefix": "/api",
      "upstreams": ["http://127.0.0.1:9001", "http://127.0.0.1:9002"],
      "strip_prefix": true,
      "rate_limit": {"rps": 10, "burst": 20},
      "timeout": "5s"
    }
  ]
}`

func TestParseValidAppliesDefaults(t *testing.T) {
	cfg, err := Parse(strings.NewReader(validJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.LogLevel != DefaultLogLevel {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, DefaultLogLevel)
	}
	if cfg.ShutdownTimeout.Duration != DefaultShutdownTimeout {
		t.Errorf("shutdown_timeout = %s, want %s", cfg.ShutdownTimeout, DefaultShutdownTimeout)
	}
	r := cfg.Routes[0]
	if r.Name != "/api" {
		t.Errorf("name = %q, want path prefix", r.Name)
	}
	if r.Timeout.Duration != 5*time.Second {
		t.Errorf("timeout = %s, want 5s", r.Timeout)
	}
	if r.CircuitBreaker.FailureThreshold != DefaultFailureThreshold || r.CircuitBreaker.Cooldown.Duration != DefaultCooldown {
		t.Errorf("breaker defaults not applied: %+v", r.CircuitBreaker)
	}
	if r.RateLimit.IdleTimeout.Duration != DefaultIdleTimeout {
		t.Errorf("rate_limit.idle_timeout = %s, want %s", r.RateLimit.IdleTimeout, DefaultIdleTimeout)
	}
}

func TestParseMinimalUsesDefaults(t *testing.T) {
	cfg, err := Parse(strings.NewReader(`{"routes":[{"path_prefix":"/","upstreams":["http://localhost:1"]}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Listen != DefaultListen || cfg.Admin != DefaultAdmin {
		t.Errorf("defaults not applied: listen=%q admin=%q", cfg.Listen, cfg.Admin)
	}
	if cfg.Routes[0].Timeout.Duration != DefaultRouteTimeout {
		t.Errorf("timeout = %s, want %s", cfg.Routes[0].Timeout, DefaultRouteTimeout)
	}
	if cfg.Routes[0].RateLimit != nil {
		t.Errorf("rate_limit should stay nil when omitted")
	}
}

func TestParseErrors(t *testing.T) {
	route := func(extra string) string {
		return `{"routes":[{"path_prefix":"/api","upstreams":["http://localhost:1"]` + extra + `}]}`
	}
	cases := []struct {
		name, json, want string
	}{
		{"malformed json", `{"routes": [`, "parse:"},
		{"unknown field", `{"listne": ":1", "routes":[]}`, "unknown field"},
		{"no routes", `{"routes":[]}`, "at least one route"},
		{"prefix without slash", `{"routes":[{"path_prefix":"api","upstreams":["http://x"]}]}`, `must start with "/"`},
		{"no upstreams", `{"routes":[{"path_prefix":"/a","upstreams":[]}]}`, "at least one upstream"},
		{"bad scheme", `{"routes":[{"path_prefix":"/a","upstreams":["ftp://x"]}]}`, "scheme must be http or https"},
		{"no host", `{"routes":[{"path_prefix":"/a","upstreams":["http://"]}]}`, "host must not be empty"},
		{"query in upstream", `{"routes":[{"path_prefix":"/a","upstreams":["http://x?y=1"]}]}`, "query string"},
		{"duplicate prefix", `{"routes":[{"path_prefix":"/a","upstreams":["http://x"]},{"path_prefix":"/a","upstreams":["http://y"]}]}`, "duplicate path_prefix"},
		{"zero rps", route(`,"rate_limit":{"rps":0,"burst":1}`), "rate_limit.rps must be positive"},
		{"zero burst", route(`,"rate_limit":{"rps":1,"burst":0}`), "rate_limit.burst must be >= 1"},
		{"negative timeout", route(`,"timeout":"-1s"`), "timeout must be positive"},
		{"bad duration", route(`,"timeout":"soon"`), "invalid duration"},
		{"negative cooldown", route(`,"circuit_breaker":{"cooldown":"-5s"}`), "cooldown must be positive"},
		{"same listen and admin", `{"listen":":1","admin":":1","routes":[{"path_prefix":"/","upstreams":["http://x"]}]}`, "must differ"},
		{"require key without keys", route(`,"require_api_key":true`), "no api_keys are configured"},
		{"short api key", `{"api_keys":[{"name":"a","key":"short"}],"routes":[{"path_prefix":"/","upstreams":["http://x"]}]}`, "at least 16 characters"},
		{"unnamed api key", `{"api_keys":[{"key":"0123456789abcdef"}],"routes":[{"path_prefix":"/","upstreams":["http://x"]}]}`, "name must not be empty"},
		{"duplicate api key name", `{"api_keys":[{"name":"a","key":"0123456789abcdef"},{"name":"a","key":"fedcba9876543210"}],"routes":[{"path_prefix":"/","upstreams":["http://x"]}]}`, "duplicate name"},
		{"reused api key secret", `{"api_keys":[{"name":"a","key":"0123456789abcdef"},{"name":"b","key":"0123456789abcdef"}],"routes":[{"path_prefix":"/","upstreams":["http://x"]}]}`, "already used"},
		{"bad log level", `{"log_level":"loud","routes":[{"path_prefix":"/","upstreams":["http://x"]}]}`, "unknown level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.json))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	cfg := &Config{Routes: []Route{{PathPrefix: "nope"}}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"listen address", "admin address", `must start with "/"`, "at least one upstream"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestDurationJSON(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"1m30s"`), &d); err != nil || d.Duration != 90*time.Second {
		t.Fatalf("string: got %v, %v", d, err)
	}
	if err := json.Unmarshal([]byte(`2.5`), &d); err != nil || d.Duration != 2500*time.Millisecond {
		t.Fatalf("number: got %v, %v", d, err)
	}
	if err := json.Unmarshal([]byte(`true`), &d); err == nil {
		t.Fatal("bool: expected error")
	}
	out, err := json.Marshal(Duration{250 * time.Millisecond})
	if err != nil || string(out) != `"250ms"` {
		t.Fatalf("marshal: got %s, %v", out, err)
	}
}

func TestSlogLevel(t *testing.T) {
	for in, want := range map[string]string{"debug": "DEBUG", "info": "INFO", "warn": "WARN", "warning": "WARN", "error": "ERROR", "": "INFO"} {
		c := &Config{LogLevel: in}
		if got := c.SlogLevel().String(); got != want {
			t.Errorf("SlogLevel(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gatekeeper.json")
	if err := os.WriteFile(path, []byte(validJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Routes) != 1 {
		t.Fatalf("routes = %d, want 1", len(cfg.Routes))
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
