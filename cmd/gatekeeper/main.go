// Command gatekeeper is a lightweight HTTP API gateway and reverse proxy.
//
// Usage:
//
//	gatekeeper [-config gatekeeper.json]
//
// It serves proxied traffic on the configured listen address and health,
// readiness, metrics and reload endpoints on the admin address. SIGINT or
// SIGTERM triggers a graceful shutdown bounded by shutdown_timeout.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sumit-do/gatekeeper/internal/config"
	"github.com/sumit-do/gatekeeper/internal/metrics"
	"github.com/sumit-do/gatekeeper/internal/middleware"
)

func main() {
	configPath := flag.String("config", "gatekeeper.json", "path to the JSON configuration file")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, "gatekeeper:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	reg := metrics.New()

	gw, err := newGateway(configPath, cfg, logger, reg)
	if err != nil {
		return err
	}
	defer gw.Close()

	proxyServer := &http.Server{
		Addr: cfg.Listen,
		Handler: middleware.Chain(gw,
			middleware.RequestID(),
			middleware.Logging(logger),
			middleware.Recover(logger),
		),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	adminServer := &http.Server{
		Addr:              cfg.Admin,
		Handler:           newAdminHandler(gw, reg),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 2)
	go func() { errCh <- serve("proxy", proxyServer, logger) }()
	go func() { errCh <- serve("admin", adminServer, logger) }()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received", "drain_timeout", cfg.ShutdownTimeout.String())
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout.Duration)
	defer cancel()
	err = errors.Join(proxyServer.Shutdown(shutdownCtx), adminServer.Shutdown(shutdownCtx))
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}

func serve(name string, srv *http.Server, logger *slog.Logger) error {
	logger.Info("listening", "server", name, "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s server: %w", name, err)
	}
	return nil
}
