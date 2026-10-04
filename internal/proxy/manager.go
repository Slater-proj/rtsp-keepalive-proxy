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
		handlers[name].EnablePersistence(cfg.Server.DataDir)
		slog.Info("registered stream", "name", name, "source", RedactURL(resolved.Source))

		if resolved.SourceLow != "" {
			lowName := "low_" + name
			lowResolved := resolved
			lowResolved.Source = resolved.SourceLow

			// Apply codec_low override if configured.
			if camCfg.CodecLow != nil {
				lowResolved.Codec = *camCfg.CodecLow
			}

			lowResolved.Width, lowResolved.Height = lowDimensions(
				resolved.Width, resolved.Height, camCfg.WidthLow, camCfg.HeightLow)

			handlers[lowName] = NewStreamHandler(lowName, lowResolved)
			handlers[lowName].EnablePersistence(cfg.Server.DataDir)
			slog.Info("registered stream", "name", lowName, "source", RedactURL(lowResolved.Source))
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

// lowDimensions picks the fallback resolution hint of the low stream. Explicit
// width_low/height_low win; otherwise a 640-pixel-wide picture is assumed,
// keeping the aspect ratio of the main stream when it is known (so a 16:9
// camera gets 640x360, not a stretched 640x480). Without any hint it falls
// back to 640x480.
func lowDimensions(mainW, mainH int, wLow, hLow *int) (int, int) {
	w, h := 640, 480
	if mainW > 0 && mainH > 0 {
		h = 640 * mainH / mainW
		h -= h % 2 // yuv420p needs even dimensions
	}
	if wLow != nil {
		w = *wLow
		if hLow == nil && mainW > 0 && mainH > 0 {
			h = w * mainH / mainW
			h -= h % 2
		}
	}
	if hLow != nil {
		h = *hLow
	}
	return w, h
}
