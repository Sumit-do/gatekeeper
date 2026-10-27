// Package proxy implements the routing, load-balancing reverse proxy at the
// heart of gatekeeper.
//
// A Router owns a set of Routes built from a config.Config. Each Route holds
// one ReverseProxy per upstream, a circuit breaker per upstream, an optional
// per-client rate limiter, and is wrapped in metrics and timeout middleware.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sumit-do/gatekeeper/internal/breaker"
	"github.com/sumit-do/gatekeeper/internal/config"
	"github.com/sumit-do/gatekeeper/internal/metrics"
	"github.com/sumit-do/gatekeeper/internal/middleware"
	"github.com/sumit-do/gatekeeper/internal/ratelimit"
)

// APIKeyHeader is the request header used as the rate-limit client key when
// present. Requests without it are keyed by client IP.
const APIKeyHeader = "X-API-Key"

// UnmatchedRoute is the metrics label used for requests that match no route.
const UnmatchedRoute = "unmatched"

// Options tunes a Router. Zero values select sensible defaults.
type Options struct {
	Logger    *slog.Logger
	Metrics   *metrics.Registry
	Transport http.RoundTripper
	Now       func() time.Time
}

// Upstream is one backend server of a Route.
type Upstream struct {
	URL     *url.URL
	Breaker *breaker.Breaker
	proxy   *httputil.ReverseProxy
}

// Route proxies requests under a path prefix to its upstreams.
type Route struct {
	Name        string
	Prefix      string
	StripPrefix bool

	upstreams []*Upstream
	next      atomic.Uint64
	limiter   *ratelimit.Limiter
	metrics   *metrics.Registry
	logger    *slog.Logger
	handler   http.Handler
}

// Upstreams returns a copy of the route's upstream list.
func (rt *Route) Upstreams() []*Upstream {
	return append([]*Upstream(nil), rt.upstreams...)
}

// Router dispatches requests to the Route with the longest matching prefix.
type Router struct {
	routes  []*Route
	metrics *metrics.Registry
	cancel  context.CancelFunc
}

// New builds a Router from a validated configuration.
func New(cfg *config.Config, opts Options) (*Router, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Metrics == nil {
		opts.Metrics = metrics.New()
	}
	if opts.Transport == nil {
		opts.Transport = http.DefaultTransport
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &Router{metrics: opts.Metrics, cancel: cancel}
	for _, rc := range cfg.Routes {
		rt, err := newRoute(ctx, rc, opts)
		if err != nil {
			cancel()
			return nil, err
		}
		r.routes = append(r.routes, rt)
	}
	// Longest prefix first so that "/api/v2" beats "/api".
	sort.SliceStable(r.routes, func(i, j int) bool {
		return len(r.routes[i].Prefix) > len(r.routes[j].Prefix)
	})
	return r, nil
}

// ServeHTTP dispatches to the matching route or responds 404.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	for _, rt := range r.routes {
		if rt.matches(req.URL.Path) {
			rt.handler.ServeHTTP(w, req)
			return
		}
	}
	start := time.Now()
	http.NotFound(w, req)
	r.metrics.ObserveRequest(UnmatchedRoute, http.StatusNotFound, time.Since(start))
}

// Routes returns the routes in matching order (longest prefix first).
func (r *Router) Routes() []*Route {
	return append([]*Route(nil), r.routes...)
}

// Ready reports whether every route has at least one upstream whose circuit
// is not open.
func (r *Router) Ready() bool {
	for _, rt := range r.routes {
		healthy := false
		for _, up := range rt.upstreams {
			if up.Breaker.State() != breaker.Open {
				healthy = true
				break
			}
		}
		if !healthy {
			return false
		}
	}
	return true
}

// Close stops background goroutines (rate-limiter eviction). In-flight
// requests are unaffected.
func (r *Router) Close() {
	r.cancel()
}

func newRoute(ctx context.Context, rc config.Route, opts Options) (*Route, error) {
	rt := &Route{
		Name:        rc.Name,
		Prefix:      rc.PathPrefix,
		StripPrefix: rc.StripPrefix,
		metrics:     opts.Metrics,
		logger:      opts.Logger.With("route", rc.Name),
	}

	for _, raw := range rc.Upstreams {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("route %q: upstream %q: %w", rc.Name, raw, err)
		}
		up := &Upstream{URL: u}
		name := u.String()
		up.Breaker = breaker.New(
			rc.CircuitBreaker.FailureThreshold,
			rc.CircuitBreaker.Cooldown.Duration,
			breaker.WithClock(opts.Now),
			breaker.WithOnStateChange(func(from, to breaker.State) {
				rt.logger.Warn("circuit state change",
					"upstream", name, "from", from.String(), "to", to.String())
				rt.metrics.SetCircuitState(rt.Name, name, float64(to))
			}),
		)
		rt.metrics.SetCircuitState(rt.Name, name, float64(breaker.Closed))
		up.proxy = rt.newReverseProxy(up, opts.Transport)
		rt.upstreams = append(rt.upstreams, up)
	}

	if rl := rc.RateLimit; rl != nil {
		idle := rl.IdleTimeout.Duration
		rt.limiter = ratelimit.New(rl.RPS, rl.Burst,
			ratelimit.WithClock(opts.Now), ratelimit.WithIdleTimeout(idle))
		go rt.limiter.Run(ctx, max(idle/2, time.Second))
	}

	rt.handler = middleware.Chain(http.HandlerFunc(rt.serve),
		middleware.Metrics(opts.Metrics, rt.Name),
		middleware.Timeout(rc.Timeout.Duration),
	)
	return rt, nil
}

// matches reports whether path falls under the route prefix on a segment
// boundary: "/api" matches "/api" and "/api/x" but not "/apix".
func (rt *Route) matches(path string) bool {
	if rt.Prefix == "/" {
		return true
	}
	p := strings.TrimSuffix(rt.Prefix, "/")
	return path == p || strings.HasPrefix(path, p+"/")
}

func (rt *Route) serve(w http.ResponseWriter, r *http.Request) {
	if rt.limiter != nil {
		if ok, retry := rt.limiter.Allow(ClientKey(r)); !ok {
			secs := int(math.Ceil(retry.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			rt.metrics.IncRateLimited(rt.Name)
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	up := rt.pick()
	if up == nil {
		http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
		return
	}
	up.proxy.ServeHTTP(w, r)
}

// pick returns the next upstream in round-robin order whose circuit admits a
// request, or nil when all circuits are open.
func (rt *Route) pick() *Upstream {
	n := uint64(len(rt.upstreams))
	if n == 0 {
		return nil
	}
	start := rt.next.Add(1) - 1
	for i := uint64(0); i < n; i++ {
		up := rt.upstreams[(start+i)%n]
		if up.Breaker.Allow() {
			return up
		}
	}
	return nil
}

func (rt *Route) newReverseProxy(up *Upstream, transport http.RoundTripper) *httputil.ReverseProxy {
	target := up.URL
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			if rt.StripPrefix {
				pr.Out.URL.Path = stripPath(pr.In.URL.Path, rt.Prefix)
				pr.Out.URL.RawPath = ""
			}
			pr.SetURL(target)
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= http.StatusInternalServerError {
				up.Breaker.Failure()
			} else {
				up.Breaker.Success()
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			up.Breaker.Failure()
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			rt.logger.Warn("upstream error",
				"upstream", target.String(),
				"request_id", middleware.RequestIDFromContext(r.Context()),
				"status", status,
				"err", err.Error())
			http.Error(w, http.StatusText(status), status)
		},
		ErrorLog: slog.NewLogLogger(rt.logger.Handler(), slog.LevelDebug),
	}
}

// ClientKey derives the rate-limit key for a request: the X-API-Key header
// when present, otherwise the client IP without its port.
func ClientKey(r *http.Request) string {
	if k := r.Header.Get(APIKeyHeader); k != "" {
		return "key:" + k
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "ip:" + host
}

// stripPath removes prefix from path, always leaving a leading slash.
func stripPath(path, prefix string) string {
	p := strings.TrimPrefix(path, strings.TrimSuffix(prefix, "/"))
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
