// Package health exposes an HTTP server for health checks and status.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"rtsp-keepalive-proxy/internal/proxy"
)

// StatusProvider gives access to camera handler states.
type StatusProvider interface {
	Handlers() map[string]*proxy.StreamHandler
}

// Server is a lightweight HTTP server for health and metrics.
type Server struct {
	httpSrv  *http.Server
	provider StatusProvider
}

// NewServer creates a new health HTTP server.
func NewServer(port int, provider StatusProvider) *Server {
	mux := http.NewServeMux()
	s := &Server{
		httpSrv: &http.Server{
			Addr:         fmt.Sprintf(":%d", port),
			Handler:      mux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
		},
		provider: provider,
	}

	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/status", s.handleStatus)

	return s
}

// Start begins listening (non-blocking, runs in a goroutine).
func (s *Server) Start() {
	slog.Info("health server listening", "addr", s.httpSrv.Addr)
	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("health server error", "error", err)
	}
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		slog.Error("health server shutdown error", "error", err)
	}
}

// handleHealth returns 200 if the service is running.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// CameraStatus is the JSON representation of one camera's state.
type CameraStatus struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Codec      string `json:"codec"`
	LastOnline string `json:"last_online,omitempty"`
	// FallbackActive is true while the generated placeholder stream is what
	// consumers receive instead of the camera.
	FallbackActive bool `json:"fallback_active"`
}

// handleStatus returns the state of every proxied camera.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	handlers := s.provider.Handlers()
	statuses := make([]CameraStatus, 0, len(handlers))

	for name, h := range handlers {
		cs := CameraStatus{
			Name:           name,
			State:          h.GetState().String(),
			Codec:          h.GetCodec(),
			FallbackActive: h.FallbackActive(),
		}
		if lo := h.LastOnline(); !lo.IsZero() {
			cs.LastOnline = lo.Format(time.RFC3339)
		}
		statuses = append(statuses, cs)
	}

	// Map iteration order is random: keep the output stable for diffing,
	// dashboards and humans.
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statuses)
}
