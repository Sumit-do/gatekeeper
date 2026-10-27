package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumit-do/gatekeeper/internal/breaker"
	"github.com/sumit-do/gatekeeper/internal/config"
	"github.com/sumit-do/gatekeeper/internal/metrics"
	"github.com/sumit-do/gatekeeper/internal/middleware"
)

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// echoUpstream returns a server that identifies itself in X-Upstream and
// echoes the path and request ID it received. status may be changed at
// runtime to simulate failures.
func echoUpstream(t *testing.T, name string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", name)
		w.Header().Set("X-Echo-Path", r.URL.Path)
		w.Header().Set("X-Echo-Request-ID", r.Header.Get(middleware.RequestIDHeader))
		w.Header().Set("X-Echo-Forwarded-For", r.Header.Get("X-Forwarded-For"))
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	return srv, &status
}

type harness struct {
	t      *testing.T
	router *Router
	reg    *metrics.Registry
	front  *httptest.Server
	client *http.Client
}

func newHarness(t *testing.T, clk *fakeClock, routes ...config.Route) *harness {
	t.Helper()
	cfg := &config.Config{Routes: routes}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	opts := Options{Metrics: metrics.New()}
	if clk != nil {
		opts.Now = clk.Now
	}
	router, err := New(cfg, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(router.Close)

	front := httptest.NewServer(middleware.Chain(router, middleware.RequestID()))
	t.Cleanup(front.Close)
	return &harness{t: t, router: router, reg: opts.Metrics, front: front, client: front.Client()}
}

func (h *harness) get(path string, headers ...string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.front.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func (h *harness) metricsText() string {
	var sb strings.Builder
	_, _ = h.reg.WriteTo(&sb)
	return sb.String()
}

func route(prefix string, upstreams ...string) config.Route {
	return config.Route{PathPrefix: prefix, Upstreams: upstreams}
}

func TestRoundRobin(t *testing.T) {
	a, _ := echoUpstream(t, "a")
	b, _ := echoUpstream(t, "b")
	h := newHarness(t, nil, route("/api", a.URL, b.URL))

	var got []string
	for i := 0; i < 6; i++ {
		resp := h.get("/api/x")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		got = append(got, resp.Header.Get("X-Upstream"))
	}
	if want := "a,b,a,b,a,b"; strings.Join(got, ",") != want {
		t.Fatalf("round robin order = %v, want %s", got, want)
	}
}

func TestStripPrefix(t *testing.T) {
	a, _ := echoUpstream(t, "a")
	stripped := route("/api", a.URL)
	stripped.StripPrefix = true
	kept := route("/keep", a.URL)
	h := newHarness(t, nil, stripped, kept)

	cases := map[string]string{
		"/api/users/1": "/users/1",
		"/api":         "/",
		"/api/":        "/",
		"/keep/x":      "/keep/x",
	}
	for in, want := range cases {
		if got := h.get(in).Header.Get("X-Echo-Path"); got != want {
			t.Errorf("GET %s forwarded path = %q, want %q", in, got, want)
		}
	}
}

func TestRequestIDPropagation(t *testing.T) {
	a, _ := echoUpstream(t, "a")
	h := newHarness(t, nil, route("/", a.URL))

	resp := h.get("/anything")
	id := resp.Header.Get(middleware.RequestIDHeader)
	if !hexID.MatchString(id) {
		t.Fatalf("response X-Request-ID = %q, want generated hex id", id)
	}
	if got := resp.Header.Get("X-Echo-Request-ID"); got != id {
		t.Fatalf("upstream saw %q, client got %q", got, id)
	}
	if resp.Header.Get("X-Echo-Forwarded-For") == "" {
		t.Error("X-Forwarded-For not set on upstream request")
	}

	resp = h.get("/anything", middleware.RequestIDHeader, "trace-123")
	if resp.Header.Get("X-Echo-Request-ID") != "trace-123" || resp.Header.Get(middleware.RequestIDHeader) != "trace-123" {
		t.Fatal("client-supplied request ID was not preserved end to end")
	}
}

func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	a, _ := echoUpstream(t, "a")
	r := route("/api", a.URL)
	r.RateLimit = &config.RateLimit{RPS: 1, Burst: 2}
	h := newHarness(t, newClock(), r)

	for i := 0; i < 2; i++ {
		if resp := h.get("/api", APIKeyHeader, "alice"); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, resp.StatusCode)
		}
	}
	resp := h.get("/api", APIKeyHeader, "alice")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "1" {
		t.Fatalf("Retry-After = %q, want \"1\"", ra)
	}
	// A different API key has its own bucket.
	if resp := h.get("/api", APIKeyHeader, "bob"); resp.StatusCode != http.StatusOK {
		t.Fatalf("other key: status = %d, want 200", resp.StatusCode)
	}
	// Requests without a key fall back to the client IP bucket.
	if resp := h.get("/api"); resp.StatusCode != http.StatusOK {
		t.Fatalf("ip key: status = %d, want 200", resp.StatusCode)
	}

	m := h.metricsText()
	if !strings.Contains(m, `gatekeeper_rate_limited_total{route="/api"} 1`) {
		t.Fatalf("rate_limited_total missing:\n%s", m)
	}
	if !strings.Contains(m, `gatekeeper_requests_total{route="/api",status="429"} 1`) {
		t.Fatalf("429 not counted in requests_total:\n%s", m)
	}
}

func TestBreakerOpensOn5xxAndTrafficShifts(t *testing.T) {
	good, _ := echoUpstream(t, "good")
	bad, badStatus := echoUpstream(t, "bad")
	badStatus.Store(http.StatusInternalServerError)

	r := route("/api", good.URL, bad.URL)
	r.CircuitBreaker = config.CircuitBreaker{FailureThreshold: 2, Cooldown: config.Duration{Duration: time.Minute}}
	h := newHarness(t, newClock(), r)

	var statuses []int
	var servedBy []string
	for i := 0; i < 8; i++ {
		resp := h.get("/api/x")
		statuses = append(statuses, resp.StatusCode)
		servedBy = append(servedBy, resp.Header.Get("X-Upstream"))
	}

	// good, bad(500), good, bad(500) -> bad trips; everything after goes to good.
	fails := 0
	for _, s := range statuses {
		if s == http.StatusInternalServerError {
			fails++
		}
	}
	if fails != 2 {
		t.Fatalf("saw %d 500s, want exactly 2 before the breaker opened: %v", fails, statuses)
	}
	for _, who := range servedBy[4:] {
		if who != "good" {
			t.Fatalf("after breaker opened traffic went to %q: %v", who, servedBy)
		}
	}

	ups := h.router.Routes()[0].Upstreams()
	if ups[1].Breaker.State() != breaker.Open || ups[0].Breaker.State() != breaker.Closed {
		t.Fatalf("states: good=%s bad=%s", ups[0].Breaker.State(), ups[1].Breaker.State())
	}
	if !h.router.Ready() {
		t.Fatal("router should remain ready while one upstream is healthy")
	}
	m := h.metricsText()
	if !strings.Contains(m, `gatekeeper_circuit_state{route="/api",upstream="`+bad.URL+`"} 1`) {
		t.Fatalf("circuit_state gauge not 1 for bad upstream:\n%s", m)
	}
}

func TestAllCircuitsOpenReturns503ThenRecovers(t *testing.T) {
	up, status := echoUpstream(t, "only")
	status.Store(http.StatusBadGateway)
	clk := newClock()

	r := route("/api", up.URL)
	r.CircuitBreaker = config.CircuitBreaker{FailureThreshold: 1, Cooldown: config.Duration{Duration: 30 * time.Second}}
	h := newHarness(t, clk, r)

	if resp := h.get("/api"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("first: status = %d, want 502 from upstream", resp.StatusCode)
	}
	if resp := h.get("/api"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second: status = %d, want 503 (circuit open)", resp.StatusCode)
	}
	if h.router.Ready() {
		t.Fatal("router must not be ready when its only upstream is open")
	}

	status.Store(http.StatusOK)
	clk.Advance(30 * time.Second)
	if resp := h.get("/api"); resp.StatusCode != http.StatusOK {
		t.Fatalf("probe: status = %d, want 200", resp.StatusCode)
	}
	if st := h.router.Routes()[0].Upstreams()[0].Breaker.State(); st != breaker.Closed {
		t.Fatalf("state after successful probe = %s, want closed", st)
	}
	if !h.router.Ready() {
		t.Fatal("router should be ready again")
	}
}

func TestUpstreamDownReturns502(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // nothing listening any more
	h := newHarness(t, nil, route("/", dead.URL))
	if resp := h.get("/x"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestTimeoutReturns504(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	r := route("/slow", slow.URL)
	r.Timeout = config.Duration{Duration: 50 * time.Millisecond}
	h := newHarness(t, nil, r)

	start := time.Now()
	resp := h.get("/slow")
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %s, timeout not enforced", time.Since(start))
	}
	if !strings.Contains(h.metricsText(), `gatekeeper_requests_total{route="/slow",status="504"} 1`) {
		t.Fatal("504 not recorded in metrics")
	}
}

func TestUnmatchedRouteIs404(t *testing.T) {
	a, _ := echoUpstream(t, "a")
	h := newHarness(t, nil, route("/api", a.URL))

	if resp := h.get("/other"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	// Prefix matching respects segment boundaries.
	if resp := h.get("/apix"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/apix status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(h.metricsText(), `gatekeeper_requests_total{route="unmatched",status="404"} 2`) {
		t.Fatal("unmatched requests not counted")
	}
}

func TestLongestPrefixWins(t *testing.T) {
	v1, _ := echoUpstream(t, "v1")
	v2, _ := echoUpstream(t, "v2")
	h := newHarness(t, nil, route("/api", v1.URL), route("/api/v2", v2.URL))

	if got := h.get("/api/v2/thing").Header.Get("X-Upstream"); got != "v2" {
		t.Fatalf("/api/v2/thing -> %q, want v2", got)
	}
	if got := h.get("/api/v1/thing").Header.Get("X-Upstream"); got != "v1" {
		t.Fatalf("/api/v1/thing -> %q, want v1", got)
	}
}

func TestClientKey(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	if got := ClientKey(req); got != "ip:10.1.2.3" {
		t.Fatalf("ClientKey = %q", got)
	}
	req.Header.Set(APIKeyHeader, "secret")
	if got := ClientKey(req); got != "key:secret" {
		t.Fatalf("ClientKey = %q", got)
	}
	req.Header.Del(APIKeyHeader)
	req.RemoteAddr = "weird"
	if got := ClientKey(req); got != "ip:weird" {
		t.Fatalf("ClientKey = %q", got)
	}
}

func TestStripPath(t *testing.T) {
	cases := []struct{ path, prefix, want string }{
		{"/api/users", "/api", "/users"},
		{"/api/users", "/api/", "/users"},
		{"/api", "/api", "/"},
		{"/api/", "/api", "/"},
	}
	for _, c := range cases {
		if got := stripPath(c.path, c.prefix); got != c.want {
			t.Errorf("stripPath(%q, %q) = %q, want %q", c.path, c.prefix, got, c.want)
		}
	}
}

func TestNewRejectsBadUpstream(t *testing.T) {
	cfg := &config.Config{Routes: []config.Route{route("/", "http://[::1")}}
	cfg.ApplyDefaults()
	if _, err := New(cfg, Options{}); err == nil {
		t.Fatal("expected error for unparsable upstream URL")
	}
}
