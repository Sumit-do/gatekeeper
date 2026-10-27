// Package config loads, defaults and validates the gatekeeper JSON
// configuration file.
//
// A configuration describes where the gateway listens, where the admin
// endpoints are served, and a list of routes. Each route maps a path prefix
// to one or more upstream URLs and carries its own rate-limit, timeout and
// circuit-breaker settings.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
)

// Defaults applied to fields that are left unset in the configuration file.
const (
	DefaultListen           = ":8080"
	DefaultAdmin            = ":9090"
	DefaultLogLevel         = "info"
	DefaultShutdownTimeout  = 10 * time.Second
	DefaultRouteTimeout     = 30 * time.Second
	DefaultFailureThreshold = 5
	DefaultCooldown         = 30 * time.Second
	DefaultIdleTimeout      = 5 * time.Minute
)

// Duration wraps time.Duration so that it can be written in JSON as a
// human-readable string such as "250ms" or "30s". A bare JSON number is
// interpreted as a number of seconds.
type Duration struct {
	time.Duration
}

// MarshalJSON encodes the duration as a string accepted by time.ParseDuration.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON decodes either a duration string or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		dur, err := time.ParseDuration(x)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", x, err)
		}
		d.Duration = dur
	case float64:
		d.Duration = time.Duration(x * float64(time.Second))
	default:
		return fmt.Errorf("invalid duration %s: expected a string like \"5s\" or a number of seconds", string(b))
	}
	return nil
}

// Config is the top-level gateway configuration.
type Config struct {
	// Listen is the address the proxy listener binds to, e.g. ":8080".
	Listen string `json:"listen"`
	// Admin is the address of the admin listener (/healthz, /readyz,
	// /metrics, /admin/reload).
	Admin string `json:"admin"`
	// LogLevel is one of "debug", "info", "warn" or "error".
	LogLevel string `json:"log_level"`
	// ShutdownTimeout bounds how long in-flight requests may drain on
	// SIGINT/SIGTERM.
	ShutdownTimeout Duration `json:"shutdown_timeout"`
	// Routes is the ordered list of proxy routes. Matching uses the longest
	// prefix, so the order in the file does not matter.
	Routes []Route `json:"routes"`
	// APIKeys lists the credentials accepted in the X-API-Key header. When
	// it is empty the header is ignored and clients are identified by IP
	// only, so a caller cannot mint fresh rate-limit buckets by inventing
	// keys.
	APIKeys []APIKey `json:"api_keys,omitempty"`
}

// MinAPIKeyLength is the shortest API key secret that Validate accepts.
const MinAPIKeyLength = 16

// APIKey is a named client credential. Name identifies the client in logs,
// in its rate-limit bucket and in the X-Gatekeeper-Client header sent
// upstream; Key is the secret the client presents.
type APIKey struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Route maps a URL path prefix to a set of upstream servers.
type Route struct {
	// Name identifies the route in logs and metrics. Defaults to PathPrefix.
	Name string `json:"name"`
	// PathPrefix must begin with "/". A request matches when its path equals
	// the prefix or continues it with a "/" segment boundary.
	PathPrefix string `json:"path_prefix"`
	// Upstreams are absolute http(s) URLs load-balanced round-robin.
	Upstreams []string `json:"upstreams"`
	// StripPrefix removes PathPrefix from the path before forwarding.
	StripPrefix bool `json:"strip_prefix"`
	// RequireAPIKey rejects requests without a valid X-API-Key with 401.
	// It needs at least one entry in the top-level api_keys list.
	RequireAPIKey bool `json:"require_api_key"`
	// RateLimit, when set, enables a per-client token bucket on the route.
	RateLimit *RateLimit `json:"rate_limit,omitempty"`
	// Timeout bounds the total time spent proxying one request.
	Timeout Duration `json:"timeout"`
	// CircuitBreaker tunes the per-upstream breaker.
	CircuitBreaker CircuitBreaker `json:"circuit_breaker"`
}

// RateLimit configures a token bucket. Each client key (X-API-Key header or
// client IP) gets its own bucket that refills at RPS tokens per second up to
// Burst tokens.
type RateLimit struct {
	RPS   float64 `json:"rps"`
	Burst int     `json:"burst"`
	// IdleTimeout is how long an unused bucket is kept before eviction.
	IdleTimeout Duration `json:"idle_timeout"`
}

// CircuitBreaker configures per-upstream failure handling. After
// FailureThreshold consecutive failures the upstream is skipped for Cooldown,
// then a single probe request decides whether it closes again.
type CircuitBreaker struct {
	FailureThreshold int      `json:"failure_threshold"`
	Cooldown         Duration `json:"cooldown"`
}

// Load reads, defaults and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes JSON from r, applies defaults and validates the result.
// Unknown fields are rejected so that typos surface immediately.
func Parse(r io.Reader) (*Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults fills zero-valued fields with their documented defaults.
func (c *Config) ApplyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.Admin == "" {
		c.Admin = DefaultAdmin
	}
	if c.LogLevel == "" {
		c.LogLevel = DefaultLogLevel
	}
	if c.ShutdownTimeout.Duration == 0 {
		c.ShutdownTimeout.Duration = DefaultShutdownTimeout
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Name == "" {
			r.Name = r.PathPrefix
		}
		if r.Timeout.Duration == 0 {
			r.Timeout.Duration = DefaultRouteTimeout
		}
		if r.CircuitBreaker.FailureThreshold == 0 {
			r.CircuitBreaker.FailureThreshold = DefaultFailureThreshold
		}
		if r.CircuitBreaker.Cooldown.Duration == 0 {
			r.CircuitBreaker.Cooldown.Duration = DefaultCooldown
		}
		if r.RateLimit != nil && r.RateLimit.IdleTimeout.Duration == 0 {
			r.RateLimit.IdleTimeout.Duration = DefaultIdleTimeout
		}
	}
}

// Validate reports every problem found in the configuration, joined into a
// single error, or nil when the configuration is usable.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if strings.TrimSpace(c.Listen) == "" {
		fail("listen address must not be empty")
	}
	if strings.TrimSpace(c.Admin) == "" {
		fail("admin address must not be empty")
	}
	if c.Listen != "" && c.Listen == c.Admin {
		fail("listen and admin addresses must differ (both %q)", c.Listen)
	}
	if _, err := parseLevel(c.LogLevel); err != nil {
		fail("log_level: %v", err)
	}
	if c.ShutdownTimeout.Duration <= 0 {
		fail("shutdown_timeout must be positive, got %s", c.ShutdownTimeout)
	}
	if len(c.Routes) == 0 {
		fail("at least one route is required")
	}

	seen := make(map[string]int, len(c.Routes))
	for i, r := range c.Routes {
		where := fmt.Sprintf("routes[%d] (%s)", i, r.Name)
		if !strings.HasPrefix(r.PathPrefix, "/") {
			fail("%s: path_prefix %q must start with \"/\"", where, r.PathPrefix)
		}
		if j, dup := seen[r.PathPrefix]; dup {
			fail("%s: duplicate path_prefix %q (also used by routes[%d])", where, r.PathPrefix, j)
		} else {
			seen[r.PathPrefix] = i
		}
		if len(r.Upstreams) == 0 {
			fail("%s: at least one upstream is required", where)
		}
		for _, raw := range r.Upstreams {
			if err := validateUpstream(raw); err != nil {
				fail("%s: upstream %q: %v", where, raw, err)
			}
		}
		if r.Timeout.Duration <= 0 {
			fail("%s: timeout must be positive, got %s", where, r.Timeout)
		}
		if r.RequireAPIKey && len(c.APIKeys) == 0 {
			fail("%s: require_api_key is set but no api_keys are configured", where)
		}
		if r.CircuitBreaker.FailureThreshold < 1 {
			fail("%s: circuit_breaker.failure_threshold must be >= 1, got %d", where, r.CircuitBreaker.FailureThreshold)
		}
		if r.CircuitBreaker.Cooldown.Duration <= 0 {
			fail("%s: circuit_breaker.cooldown must be positive, got %s", where, r.CircuitBreaker.Cooldown)
		}
		if rl := r.RateLimit; rl != nil {
			if rl.RPS <= 0 {
				fail("%s: rate_limit.rps must be positive, got %v", where, rl.RPS)
			}
			if rl.Burst < 1 {
				fail("%s: rate_limit.burst must be >= 1, got %d", where, rl.Burst)
			}
			if rl.IdleTimeout.Duration <= 0 {
				fail("%s: rate_limit.idle_timeout must be positive, got %s", where, rl.IdleTimeout)
			}
		}
	}

	names := make(map[string]bool, len(c.APIKeys))
	secrets := make(map[string]bool, len(c.APIKeys))
	for i, k := range c.APIKeys {
		where := fmt.Sprintf("api_keys[%d]", i)
		switch {
		case strings.TrimSpace(k.Name) == "":
			fail("%s: name must not be empty", where)
		case names[k.Name]:
			fail("%s: duplicate name %q", where, k.Name)
		}
		names[k.Name] = true
		switch {
		case len(k.Key) < MinAPIKeyLength:
			fail("%s (%s): key must be at least %d characters", where, k.Name, MinAPIKeyLength)
		case secrets[k.Key]:
			fail("%s (%s): key is already used by another entry", where, k.Name)
		}
		secrets[k.Key] = true
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid config:\n%w", errors.Join(errs...))
}

// SlogLevel converts LogLevel into a slog.Level. It falls back to Info for
// unknown values; Validate reports those as errors.
func (c *Config) SlogLevel() slog.Level {
	lvl, err := parseLevel(c.LogLevel)
	if err != nil {
		return slog.LevelInfo
	}
	return lvl
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown level %q (want debug, info, warn or error)", s)
	}
}

func validateUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("host must not be empty")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not contain a query string or fragment")
	}
	return nil
}
