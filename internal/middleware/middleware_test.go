package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sumit-do/gatekeeper/internal/metrics"
)

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+":in")
				next.ServeHTTP(w, r)
				order = append(order, name+":out")
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mk("a"), mk("b"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	want := "a:in,b:in,handler,b:out,a:out"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

func TestRequestIDGenerated(t *testing.T) {
	var seen string
	h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		if r.Header.Get(RequestIDHeader) != seen {
			t.Error("request header not set to the generated ID")
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if !hexID.MatchString(seen) {
		t.Fatalf("generated ID %q is not 32 hex chars", seen)
	}
	if got := rec.Header().Get(RequestIDHeader); got != seen {
		t.Fatalf("response header = %q, want %q", got, seen)
	}
}

func TestRequestIDPreserved(t *testing.T) {
	var seen string
	h := RequestID()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(RequestIDHeader, "client-supplied")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "client-supplied" || rec.Header().Get(RequestIDHeader) != "client-supplied" {
		t.Fatalf("ID not preserved: ctx=%q header=%q", seen, rec.Header().Get(RequestIDHeader))
	}
}

func TestRequestIDFromContextMissing(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestLoggingEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	}), RequestID(), Logging(logger))

	req := httptest.NewRequest("POST", "/tea", nil)
	req.Header.Set(RequestIDHeader, "rid-1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	if entry["msg"] != "request" || entry["method"] != "POST" || entry["path"] != "/tea" {
		t.Errorf("unexpected fields: %v", entry)
	}
	if entry["status"] != float64(418) || entry["bytes"] != float64(15) {
		t.Errorf("status/bytes wrong: %v", entry)
	}
	if entry["request_id"] != "rid-1" {
		t.Errorf("request_id = %v", entry["request_id"])
	}
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN for 4xx", entry["level"])
	}
	if _, ok := entry["duration_ms"]; !ok {
		t.Error("duration_ms missing")
	}
}

func TestRecoverReturns500AndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(buf.String(), "kaboom") || !strings.Contains(buf.String(), "panic recovered") {
		t.Fatalf("panic not logged: %s", buf.String())
	}
}

func TestRecoverLeavesWrittenResponseAlone(t *testing.T) {
	h := Recover(slog.New(slog.DiscardHandler))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("late")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want the already-written 202", rec.Code)
	}
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	h := Recover(slog.New(slog.DiscardHandler))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want ErrAbortHandler to propagate", r)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestTimeoutSetsDeadline(t *testing.T) {
	var hasDeadline bool
	h := Timeout(50 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, hasDeadline = r.Context().Deadline()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !hasDeadline {
		t.Fatal("context has no deadline")
	}

	var noDeadline bool
	Timeout(0)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, noDeadline = r.Context().Deadline()
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if noDeadline {
		t.Fatal("Timeout(0) must be a no-op")
	}
}

func TestMetricsRecordsStatus(t *testing.T) {
	reg := metrics.New()
	h := Metrics(reg, "/r")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	var sb strings.Builder
	_, _ = reg.WriteTo(&sb)
	if !strings.Contains(sb.String(), `gatekeeper_requests_total{route="/r",status="502"} 1`) {
		t.Fatalf("metric missing:\n%s", sb.String())
	}
}

func TestRecorder(t *testing.T) {
	rec := NewRecorder(httptest.NewRecorder())
	if rec.Status() != 200 || rec.Written() {
		t.Fatal("fresh recorder must report 200 and not written")
	}
	rec.WriteHeader(http.StatusCreated)
	rec.WriteHeader(http.StatusInternalServerError) // ignored
	n, _ := rec.Write([]byte("abc"))
	if n != 3 || rec.Bytes() != 3 || rec.Status() != http.StatusCreated || !rec.Written() {
		t.Fatalf("status=%d bytes=%d written=%v", rec.Status(), rec.Bytes(), rec.Written())
	}
	rec.Flush()
	if rec.Unwrap() == nil {
		t.Fatal("Unwrap returned nil")
	}
}

func TestNewIDUnique(t *testing.T) {
	a, b := NewID(), NewID()
	if a == b || !hexID.MatchString(a) {
		t.Fatalf("ids not unique/hex: %q %q", a, b)
	}
}
