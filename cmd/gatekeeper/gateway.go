package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/sumit-do/gatekeeper/internal/config"
	"github.com/sumit-do/gatekeeper/internal/metrics"
	"github.com/sumit-do/gatekeeper/internal/proxy"
)

// gateway holds the live proxy.Router and swaps it atomically on reload so
// that configuration changes never interrupt in-flight requests.
type gateway struct {
	path    string
	logger  *slog.Logger
	metrics *metrics.Registry

	reloadMu sync.Mutex // serialises Reload calls
	router   atomic.Pointer[proxy.Router]
}

func newGateway(path string, cfg *config.Config, logger *slog.Logger, reg *metrics.Registry) (*gateway, error) {
	g := &gateway{path: path, logger: logger, metrics: reg}
	rt, err := proxy.New(cfg, proxy.Options{Logger: logger, Metrics: reg})
	if err != nil {
		return nil, err
	}
	g.router.Store(rt)
	return g, nil
}

// ServeHTTP forwards to the current router.
func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.router.Load().ServeHTTP(w, r)
}

// Ready reports readiness of the current router.
func (g *gateway) Ready() bool {
	return g.router.Load().Ready()
}

// Reload re-reads the configuration file and swaps in a new router. On any
// error the previous router stays active. It returns the number of routes
// now being served. Listener addresses are not changed by a reload.
func (g *gateway) Reload() (int, error) {
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()

	cfg, err := config.Load(g.path)
	if err != nil {
		return 0, err
	}
	rt, err := proxy.New(cfg, proxy.Options{Logger: g.logger, Metrics: g.metrics})
	if err != nil {
		return 0, fmt.Errorf("build router: %w", err)
	}
	if old := g.router.Swap(rt); old != nil {
		old.Close()
	}
	g.logger.Info("configuration reloaded", "path", g.path, "routes", len(cfg.Routes))
	return len(cfg.Routes), nil
}

// Close releases the current router's background resources.
func (g *gateway) Close() {
	if rt := g.router.Load(); rt != nil {
		rt.Close()
	}
}
