// rtsp-keepalive-proxy keeps RTSP streams alive for battery/wifi cameras.
//
// Usage:
//
//	rtsp-keepalive-proxy -config config.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"rtsp-keepalive-proxy/internal/config"
	"rtsp-keepalive-proxy/internal/health"
	"rtsp-keepalive-proxy/internal/logger"
	"rtsp-keepalive-proxy/internal/proxy"
)

// Set at build time via -ldflags.
var version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("rtsp-keepalive-proxy", version)
		os.Exit(0)
	}

	// ── Load configuration ───────────────────────────────────────────
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}

	logger.Setup(cfg.Server.LogLevel)
	slog.Info("starting rtsp-keepalive-proxy", "version", version)

	// ── Create and start proxy manager ───────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr, err := proxy.NewManager(cfg)
	if err != nil {
		slog.Error("manager creation failed", "error", err)
		os.Exit(1)
	}

	if err := mgr.Start(ctx); err != nil {
		slog.Error("manager start failed", "error", err)
		os.Exit(1)
	}

	// ── Health / status HTTP server ──────────────────────────────────
	healthSrv := health.NewServer(cfg.Server.HealthPort, mgr)
	go healthSrv.Start()

	// ── Wait for termination signal ──────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig)

	cancel()
	mgr.Stop()
	healthSrv.Stop()

	slog.Info("shutdown complete")
}
