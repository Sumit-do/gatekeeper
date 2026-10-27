package main

import (
	"encoding/json"
	"net/http"

	"github.com/sumit-do/gatekeeper/internal/metrics"
)

// newAdminHandler serves the operational endpoints:
//
//	GET  /healthz       liveness, always 200 while the process runs
//	GET  /readyz        200 when every route has a non-open upstream, else 503
//	GET  /metrics       Prometheus text exposition
//	POST /admin/reload  re-read the config file and swap routes atomically
func newAdminHandler(g *gateway, reg *metrics.Registry) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if g.Ready() {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "reason": "all upstreams of at least one route are open"})
	})

	mux.Handle("GET /metrics", reg)

	mux.HandleFunc("POST /admin/reload", func(w http.ResponseWriter, _ *http.Request) {
		n, err := g.Reload()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "reloaded", "routes": n})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
