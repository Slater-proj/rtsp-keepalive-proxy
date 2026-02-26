package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"rtsp-keepalive-proxy/internal/config"
	"rtsp-keepalive-proxy/internal/proxy"
)

type mockProvider struct {
	handlers map[string]*proxy.StreamHandler
}

func (m *mockProvider) Handlers() map[string]*proxy.StreamHandler {
	return m.handlers
}

func newMockProvider() *mockProvider {
	cfg := config.ResolvedCamera{
		Source:      "rtsp://localhost/test",
		BatteryMode: true,
		Codec:      "h264",
	}
	return &mockProvider{
		handlers: map[string]*proxy.StreamHandler{
			"test_cam": proxy.NewStreamHandler("test_cam", cfg),
		},
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv := NewServer(0, newMockProvider())
	// Use httptest to call the handler directly.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func TestStatusEndpoint(t *testing.T) {
	srv := NewServer(0, newMockProvider())
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()

	srv.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	var statuses []CameraStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &statuses); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("got %d cameras, want 1", len(statuses))
	}
	if statuses[0].Name != "test_cam" {
		t.Errorf("name = %q, want test_cam", statuses[0].Name)
	}
	if statuses[0].State != "disconnected" {
		t.Errorf("state = %q, want disconnected", statuses[0].State)
	}
	if statuses[0].Codec != "h264" {
		t.Errorf("codec = %q, want h264", statuses[0].Codec)
	}
}
