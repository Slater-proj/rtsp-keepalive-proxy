// Package proxy implements the RTSP relay with fallback capability.
package proxy

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
)

// Server is the output RTSP server that Frigate (or any consumer) connects to.
type Server struct {
	port     int
	rtsp     *gortsplib.Server
	handlers map[string]*StreamHandler
	streams  map[string]*gortsplib.ServerStream
	mu       sync.RWMutex
}

// NewServer creates a new RTSP output server.
func NewServer(port int, handlers map[string]*StreamHandler) *Server {
	return &Server{
		port:     port,
		handlers: handlers,
		streams:  make(map[string]*gortsplib.ServerStream),
	}
}

// Start initialises the RTSP server and creates a ServerStream per camera path.
func (s *Server) Start() error {
	s.rtsp = &gortsplib.Server{
		Handler:     s,
		RTSPAddress: fmt.Sprintf(":%d", s.port),
	}

	if err := s.rtsp.Start(); err != nil {
		return fmt.Errorf("rtsp server start: %w", err)
	}

	for name, handler := range s.handlers {
		desc := handler.BuildDescription()
		stream := gortsplib.NewServerStream(s.rtsp, desc)

		s.mu.Lock()
		s.streams[name] = stream
		s.mu.Unlock()

		handler.SetStream(stream)
		slog.Info("rtsp output stream ready", "path", "/"+name)
	}

	slog.Info("rtsp server listening", "port", s.port)
	return nil
}

// Stop gracefully shuts down the RTSP server.
func (s *Server) Stop() {
	s.mu.Lock()
	for _, st := range s.streams {
		st.Close()
	}
	s.mu.Unlock()

	if s.rtsp != nil {
		s.rtsp.Close()
	}
	slog.Info("rtsp server stopped")
}

// pathFromCtx returns the cleaned camera name from a request path.
// Query parameters and fragments are stripped.
func pathFromCtx(rawPath string) string {
	p := strings.TrimPrefix(strings.TrimPrefix(rawPath, "/"), "/")
	if idx := strings.IndexByte(p, '?'); idx >= 0 {
		p = p[:idx]
	}
	return p
}

// ---------------------------------------------------------------------------
// gortsplib server handler interfaces
// ---------------------------------------------------------------------------

// OnConnOpen is called when a consumer connects.
func (s *Server) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	slog.Debug("rtsp client connected", "remote", ctx.Conn.NetConn().RemoteAddr())
}

// OnConnClose is called when a consumer disconnects.
func (s *Server) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	slog.Debug("rtsp client disconnected", "remote", ctx.Conn.NetConn().RemoteAddr(), "error", ctx.Error)
}

// OnSessionOpen is called when a new RTSP session starts.
func (s *Server) OnSessionOpen(ctx *gortsplib.ServerHandlerOnSessionOpenCtx) {
	slog.Debug("rtsp session opened")
}

// OnSessionClose is called when an RTSP session ends.
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	slog.Debug("rtsp session closed")
}

// OnDescribe handles DESCRIBE requests from consumers.
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	name := pathFromCtx(ctx.Path)

	s.mu.RLock()
	stream, ok := s.streams[name]
	s.mu.RUnlock()

	if !ok {
		slog.Warn("describe: unknown stream", "path", name)
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}

	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

// OnAnnounce rejects publish attempts — this server is read-only.
func (s *Server) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusMethodNotAllowed}, nil
}

// OnSetup handles SETUP requests.
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	name := pathFromCtx(ctx.Path)

	s.mu.RLock()
	stream, ok := s.streams[name]
	s.mu.RUnlock()

	if !ok {
		slog.Warn("setup: unknown stream", "path", name)
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}

	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

// OnPlay handles PLAY requests.
func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	slog.Debug("rtsp play started", "path", pathFromCtx(ctx.Path))
	return &base.Response{StatusCode: base.StatusOK}, nil
}
