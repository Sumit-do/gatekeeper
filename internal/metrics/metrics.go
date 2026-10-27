// Package metrics is a small, dependency-free metrics registry that renders
// itself in the Prometheus text exposition format (version 0.0.4).
//
// It knows about exactly the series gatekeeper needs: a request counter by
// route and status, a request-duration histogram by route, a rate-limited
// counter by route and a circuit-state gauge by route and upstream.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Namespace prefixes every metric name.
const Namespace = "gatekeeper"

// ContentType is the Prometheus text exposition media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// DefaultBuckets are the histogram upper bounds in seconds used by New.
var DefaultBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

type requestKey struct {
	route  string
	status int
}

type circuitKey struct {
	route, upstream string
}

type histogram struct {
	counts []uint64 // per-bucket (non-cumulative) counts
	sum    float64
	count  uint64
}

// Registry collects metrics. The zero value is not usable; call New.
type Registry struct {
	buckets []float64

	mu          sync.Mutex
	requests    map[requestKey]uint64
	durations   map[string]*histogram
	rateLimited map[string]uint64
	circuit     map[circuitKey]float64
}

// New returns an empty registry using DefaultBuckets.
func New() *Registry {
	return NewWithBuckets(DefaultBuckets)
}

// NewWithBuckets returns an empty registry with custom histogram bounds. The
// bounds are sorted ascending.
func NewWithBuckets(buckets []float64) *Registry {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	return &Registry{
		buckets:     b,
		requests:    make(map[requestKey]uint64),
		durations:   make(map[string]*histogram),
		rateLimited: make(map[string]uint64),
		circuit:     make(map[circuitKey]float64),
	}
}

// ObserveRequest records one completed request.
func (r *Registry) ObserveRequest(route string, status int, d time.Duration) {
	secs := d.Seconds()

	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests[requestKey{route, status}]++
	h, ok := r.durations[route]
	if !ok {
		h = &histogram{counts: make([]uint64, len(r.buckets))}
		r.durations[route] = h
	}
	h.sum += secs
	h.count++
	for i, le := range r.buckets {
		if secs <= le {
			h.counts[i]++
			break
		}
	}
}

// IncRateLimited records one request rejected with 429 on route.
func (r *Registry) IncRateLimited(route string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rateLimited[route]++
}

// SetCircuitState records the breaker state of an upstream
// (0 closed, 1 open, 2 half-open).
func (r *Registry) SetCircuitState(route, upstream string, state float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.circuit[circuitKey{route, upstream}] = state
}

// ServeHTTP renders the registry in Prometheus text format.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", ContentType)
	_, _ = r.WriteTo(w)
}

// WriteTo renders the registry in Prometheus text format. Series are emitted
// in a deterministic (sorted) order.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer
	r.render(&buf)
	n, err := w.Write(buf.Bytes())
	return int64(n), err
}

func (r *Registry) render(buf *bytes.Buffer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// requests_total
	name := Namespace + "_requests_total"
	fmt.Fprintf(buf, "# HELP %s Total HTTP requests handled, by route and response status.\n", name)
	fmt.Fprintf(buf, "# TYPE %s counter\n", name)
	reqKeys := make([]requestKey, 0, len(r.requests))
	for k := range r.requests {
		reqKeys = append(reqKeys, k)
	}
	sort.Slice(reqKeys, func(i, j int) bool {
		if reqKeys[i].route != reqKeys[j].route {
			return reqKeys[i].route < reqKeys[j].route
		}
		return reqKeys[i].status < reqKeys[j].status
	})
	for _, k := range reqKeys {
		fmt.Fprintf(buf, "%s{route=%s,status=\"%d\"} %d\n", name, quote(k.route), k.status, r.requests[k])
	}

	// request_duration_seconds
	name = Namespace + "_request_duration_seconds"
	fmt.Fprintf(buf, "# HELP %s Time spent handling requests, by route.\n", name)
	fmt.Fprintf(buf, "# TYPE %s histogram\n", name)
	for _, route := range sortedKeys(r.durations) {
		h := r.durations[route]
		var cum uint64
		for i, le := range r.buckets {
			cum += h.counts[i]
			fmt.Fprintf(buf, "%s_bucket{route=%s,le=%s} %d\n", name, quote(route), quote(formatFloat(le)), cum)
		}
		fmt.Fprintf(buf, "%s_bucket{route=%s,le=\"+Inf\"} %d\n", name, quote(route), h.count)
		fmt.Fprintf(buf, "%s_sum{route=%s} %s\n", name, quote(route), formatFloat(h.sum))
		fmt.Fprintf(buf, "%s_count{route=%s} %d\n", name, quote(route), h.count)
	}

	// rate_limited_total
	name = Namespace + "_rate_limited_total"
	fmt.Fprintf(buf, "# HELP %s Requests rejected by the rate limiter, by route.\n", name)
	fmt.Fprintf(buf, "# TYPE %s counter\n", name)
	for _, route := range sortedKeys(r.rateLimited) {
		fmt.Fprintf(buf, "%s{route=%s} %d\n", name, quote(route), r.rateLimited[route])
	}

	// circuit_state
	name = Namespace + "_circuit_state"
	fmt.Fprintf(buf, "# HELP %s Circuit breaker state per upstream (0 closed, 1 open, 2 half-open).\n", name)
	fmt.Fprintf(buf, "# TYPE %s gauge\n", name)
	circKeys := make([]circuitKey, 0, len(r.circuit))
	for k := range r.circuit {
		circKeys = append(circKeys, k)
	}
	sort.Slice(circKeys, func(i, j int) bool {
		if circKeys[i].route != circKeys[j].route {
			return circKeys[i].route < circKeys[j].route
		}
		return circKeys[i].upstream < circKeys[j].upstream
	})
	for _, k := range circKeys {
		fmt.Fprintf(buf, "%s{route=%s,upstream=%s} %s\n", name, quote(k.route), quote(k.upstream), formatFloat(r.circuit[k]))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// quote escapes a label value per the exposition format and wraps it in
// double quotes.
func quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, c := range s {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
