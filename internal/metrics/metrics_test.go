package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var sb strings.Builder
	if _, err := r.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return sb.String()
}

func mustContain(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("output missing line %q\n--- output ---\n%s", l, out)
		}
	}
}

func TestExpositionFormat(t *testing.T) {
	r := New()
	r.ObserveRequest("/api", 200, 250*time.Millisecond)
	r.ObserveRequest("/api", 200, 500*time.Millisecond)
	r.ObserveRequest("/api", 502, 2*time.Second)
	r.IncRateLimited("/api")
	r.IncRateLimited("/api")
	r.SetCircuitState("/api", "http://a:1", 1)
	r.SetCircuitState("/api", "http://b:1", 0)

	out := render(t, r)
	mustContain(t, out,
		"# TYPE gatekeeper_requests_total counter",
		`gatekeeper_requests_total{route="/api",status="200"} 2`,
		`gatekeeper_requests_total{route="/api",status="502"} 1`,
		"# TYPE gatekeeper_request_duration_seconds histogram",
		`gatekeeper_request_duration_seconds_bucket{route="/api",le="0.005"} 0`,
		`gatekeeper_request_duration_seconds_bucket{route="/api",le="0.1"} 0`,
		`gatekeeper_request_duration_seconds_bucket{route="/api",le="0.25"} 1`,
		`gatekeeper_request_duration_seconds_bucket{route="/api",le="0.5"} 2`,
		`gatekeeper_request_duration_seconds_bucket{route="/api",le="1"} 2`,
		`gatekeeper_request_duration_seconds_bucket{route="/api",le="2.5"} 3`,
		`gatekeeper_request_duration_seconds_bucket{route="/api",le="10"} 3`,
		`gatekeeper_request_duration_seconds_bucket{route="/api",le="+Inf"} 3`,
		`gatekeeper_request_duration_seconds_sum{route="/api"} 2.75`,
		`gatekeeper_request_duration_seconds_count{route="/api"} 3`,
		"# TYPE gatekeeper_rate_limited_total counter",
		`gatekeeper_rate_limited_total{route="/api"} 2`,
		"# TYPE gatekeeper_circuit_state gauge",
		`gatekeeper_circuit_state{route="/api",upstream="http://a:1"} 1`,
		`gatekeeper_circuit_state{route="/api",upstream="http://b:1"} 0`,
	)
	if !strings.HasSuffix(out, "\n") {
		t.Error("output must end with a newline")
	}
}

func TestObservationAboveLargestBucketOnlyCountsInInf(t *testing.T) {
	r := New()
	r.ObserveRequest("/slow", 200, time.Minute)
	out := render(t, r)
	mustContain(t, out,
		`gatekeeper_request_duration_seconds_bucket{route="/slow",le="10"} 0`,
		`gatekeeper_request_duration_seconds_bucket{route="/slow",le="+Inf"} 1`,
		`gatekeeper_request_duration_seconds_sum{route="/slow"} 60`,
	)
}

func TestDeterministicOrdering(t *testing.T) {
	r := New()
	r.ObserveRequest("/b", 500, time.Millisecond)
	r.ObserveRequest("/a", 404, time.Millisecond)
	r.ObserveRequest("/a", 200, time.Millisecond)
	out := render(t, r)

	a200 := strings.Index(out, `route="/a",status="200"`)
	a404 := strings.Index(out, `route="/a",status="404"`)
	b500 := strings.Index(out, `route="/b",status="500"`)
	if !(a200 < a404 && a404 < b500) {
		t.Fatalf("series not sorted by route then status:\n%s", out)
	}
	if render(t, r) != out {
		t.Fatal("rendering twice produced different output")
	}
}

func TestLabelEscaping(t *testing.T) {
	r := New()
	r.IncRateLimited("a\"b\\c\nd")
	out := render(t, r)
	mustContain(t, out, `gatekeeper_rate_limited_total{route="a\"b\\c\nd"} 1`)
}

func TestEmptyRegistryStillEmitsHeaders(t *testing.T) {
	out := render(t, New())
	for _, name := range []string{"gatekeeper_requests_total", "gatekeeper_request_duration_seconds", "gatekeeper_rate_limited_total", "gatekeeper_circuit_state"} {
		if !strings.Contains(out, "# HELP "+name+" ") || !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("missing HELP/TYPE for %s", name)
		}
	}
}

func TestServeHTTP(t *testing.T) {
	r := New()
	r.ObserveRequest("/x", 200, time.Millisecond)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ContentType {
		t.Fatalf("content-type = %q, want %q", ct, ContentType)
	}
	if !strings.Contains(rec.Body.String(), `gatekeeper_requests_total{route="/x",status="200"} 1`) {
		t.Fatalf("unexpected body:\n%s", rec.Body.String())
	}
}

func TestCustomBucketsSorted(t *testing.T) {
	r := NewWithBuckets([]float64{1, 0.1, 10})
	r.ObserveRequest("/r", 200, 50*time.Millisecond)
	out := render(t, r)
	i1 := strings.Index(out, `le="0.1"`)
	i2 := strings.Index(out, `le="1"`)
	i3 := strings.Index(out, `le="10"`)
	if !(i1 >= 0 && i1 < i2 && i2 < i3) {
		t.Fatalf("buckets not sorted:\n%s", out)
	}
}
