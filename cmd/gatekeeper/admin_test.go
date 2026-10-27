package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumit-do/gatekeeper/internal/config"
	"github.com/sumit-do/gatekeeper/internal/metrics"
)

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func oneRoute(prefix string) string {
	return `{"routes":[{"path_prefix":"` + prefix + `","upstreams":["http://127.0.0.1:1"]}]}`
}

func newTestAdmin(t *testing.T) (*gateway, http.Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gatekeeper.json")
	writeConfig(t, path, oneRoute("/api"))

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.New()
	gw, err := newGateway(path, cfg, slog.New(slog.DiscardHandler), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	return gw, newAdminHandler(gw, reg), path
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHealthAndReady(t *testing.T) {
	_, h, _ := newTestAdmin(t)

	if rec := do(h, "GET", "/healthz"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("/healthz: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "GET", "/readyz"); rec.Code != 200 {
		t.Fatalf("/readyz: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMetricsEndpoint(t *testing.T) {
	_, h, _ := newTestAdmin(t)
	rec := do(h, "GET", "/metrics")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != metrics.ContentType {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `gatekeeper_circuit_state{route="/api",upstream="http://127.0.0.1:1"} 0`) {
		t.Fatalf("initial circuit gauge missing:\n%s", rec.Body.String())
	}
}

func TestReloadSwapsRoutes(t *testing.T) {
	gw, h, path := newTestAdmin(t)

	// The initial config serves /api only.
	if rec := do(gw, "GET", "/v2/x"); rec.Code != http.StatusNotFound {
		t.Fatalf("before reload /v2 status = %d, want 404", rec.Code)
	}

	writeConfig(t, path, `{"routes":[
	  {"path_prefix":"/api","upstreams":["http://127.0.0.1:1"]},
	  {"path_prefix":"/v2","upstreams":["http://127.0.0.1:1"]}]}`)
	rec := do(h, "POST", "/admin/reload")
	if rec.Code != 200 {
		t.Fatalf("reload status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		Routes int    `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Status != "reloaded" || body.Routes != 2 {
		t.Fatalf("reload body = %s (%v)", rec.Body.String(), err)
	}

	// /v2 now routes (and fails with 502 because nothing listens on port 1).
	if rec := do(gw, "GET", "/v2/x"); rec.Code != http.StatusBadGateway {
		t.Fatalf("after reload /v2 status = %d, want 502", rec.Code)
	}
}

func TestReloadInvalidConfigKeepsOldRouter(t *testing.T) {
	gw, h, path := newTestAdmin(t)
	before := gw.router.Load()

	writeConfig(t, path, `{"routes":[]}`)
	rec := do(h, "POST", "/admin/reload")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "at least one route") {
		t.Fatalf("error message not surfaced: %s", rec.Body.String())
	}
	if gw.router.Load() != before {
		t.Fatal("router was swapped despite invalid config")
	}
}

func TestReloadRequiresPOST(t *testing.T) {
	_, h, _ := newTestAdmin(t)
	if rec := do(h, "GET", "/admin/reload"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /admin/reload status = %d, want 405", rec.Code)
	}
}
