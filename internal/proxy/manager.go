package proxy

import (
	"context"
	"fmt"
	"log/slog"

	"rtsp-keepalive-proxy/internal/config"
)

// Manager orchestrates all camera stream handlers and the output RTSP server.
type Manager struct {
	cfg      *config.Config
	handlers map[string]*StreamHandler
	server   *Server
}

// NewManager creates a Manager from the loaded configuration.
// It builds one StreamHandler per camera path (high quality),
// and a second one (low_<name>) when source_low is defined.
func NewManager(cfg *config.Config) (*Manager, error) {
	handlers := make(map[string]*StreamHandler)

	for name, camCfg := range cfg.Cameras {
		resolved := camCfg.Effective(cfg.Defaults)

		handlers[name] = NewStreamHandler(name, resolved)
		slog.Info("registered stream", "name", name, "source", resolved.Source)

		if resolved.SourceLow != "" {
			lowName := "low_" + name
			lowResolved := resolved
			lowResolved.Source = resolved.SourceLow
			handlers[lowName] = NewStreamHandler(lowName, lowResolved)
			slog.Info("registered stream", "name", lowName, "source", lowResolved.Source)
		}
	}

	if len(handlers) == 0 {
		return nil, fmt.Errorf("no stream handlers created")
	}

	return &Manager{
		cfg:      cfg,
		handlers: handlers,
		server:   NewServer(cfg.Server.RTSPPort, handlers),
	}, nil
}

// Start launches the RTSP server and all stream handlers.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.server.Start(); err != nil {
		return fmt.Errorf("server start: %w", err)
	}

	for _, h := range m.handlers {
		h.Start(ctx)
	}

	slog.Info("all handlers started", "count", len(m.handlers))
	return nil
}

// Stop gracefully shuts everything down.
func (m *Manager) Stop() {
	for _, h := range m.handlers {
		h.Stop()
	}
	m.server.Stop()
	slog.Info("manager stopped")
}

// Handlers returns the map of active stream handlers (for health endpoint).
func (m *Manager) Handlers() map[string]*StreamHandler {
	return m.handlers
}
