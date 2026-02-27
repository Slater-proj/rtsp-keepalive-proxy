package proxy

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"log/slog"
	"os"
	"os/exec"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph265"
	"github.com/pion/rtp"

	"rtsp-keepalive-proxy/internal/config"
	"rtsp-keepalive-proxy/internal/fallback"
)

// CameraState represents the current state of a proxied camera.
type CameraState int32

const (
	StateDisconnected CameraState = iota
	StateConnecting
	StateOnline
	StateSleeping
)

func (s CameraState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateOnline:
		return "online"
	case StateSleeping:
		return "sleeping"
	default:
		return "unknown"
	}
}

// StreamHandler manages the lifecycle of a single camera stream:
// connect → relay → detect sleep → fallback → reconnect.
type StreamHandler struct {
	Name string
	cfg  config.ResolvedCamera
	log  *slog.Logger

	// State
	state      atomic.Int32
	lastOnline atomic.Int64 // unix timestamp

	// Detected source codec
	mu            sync.RWMutex
	detectedCodec string // "h264" or "h265"

	// Output
	stream *gortsplib.ServerStream
	desc   *description.Session

	// Fallback
	fallbackGen    *fallback.Generator
	fallbackMu     sync.Mutex
	fallbackCancel context.CancelFunc
	fallbackWg     sync.WaitGroup

	// Silent audio (PCMA) — keeps go2rtc/Frigate happy.
	silenceMu     sync.Mutex
	silenceCancel context.CancelFunc
	silenceWg     sync.WaitGroup

	// Rate-limiting keyframe captures (unix seconds).
	lastCaptureTS atomic.Int64

	// Pre-generated SPS/PPS for the initial SDP.
	// Without these in the SDP, Frigate's FFmpeg (analyzeduration=0)
	// cannot determine video dimensions and fails.
	initialSPS []byte
	initialPPS []byte

	// Lifecycle
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewStreamHandler creates a handler for the given resolved camera config.
func NewStreamHandler(name string, cfg config.ResolvedCamera) *StreamHandler {
	codec := "h264"
	if cfg.Codec == "h265" {
		codec = "h265"
	}

	sh := &StreamHandler{
		Name:          name,
		cfg:           cfg,
		detectedCodec: codec,
		fallbackGen:   fallback.NewGenerator(name, cfg.FallbackMode),
		log:           slog.With("camera", name),
	}
	sh.state.Store(int32(StateDisconnected))

	// Pre-generate SPS/PPS so the SDP always has sprop-parameter-sets.
	// This is critical: Frigate sets analyzeduration=0 (no probing),
	// so without SPS/PPS in the SDP, FFmpeg can't determine dimensions
	// and its decoder misinterprets NAL data.
	if codec == "h264" && cfg.FallbackMode != "none" {
		sh.preGenerateSPSPPS()
	}

	return sh
}

// preGenerateSPSPPS runs FFmpeg synchronously to encode one placeholder
// frame, extracting H.264 SPS/PPS NAL units. These are embedded into
// the SDP (sprop-parameter-sets) so consumers with analyzeduration=0
// can determine video dimensions without probing the RTP stream.
func (sh *StreamHandler) preGenerateSPSPPS() {
	pngData, w, h := sh.fallbackGen.GetFramePNG()
	if len(pngData) == 0 {
		sh.log.Warn("preGenerateSPSPPS: no PNG available, SDP will lack sprop-parameter-sets")
		return
	}

	if w%2 != 0 {
		w++
	}
	if h%2 != 0 {
		h++
	}

	tmpFile, err := os.CreateTemp("", fmt.Sprintf("init-sps-%s-*.png", sh.Name))
	if err != nil {
		sh.log.Warn("preGenerateSPSPPS: temp file", "error", err)
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(pngData); err != nil {
		tmpFile.Close()
		sh.log.Warn("preGenerateSPSPPS: write temp file", "error", err)
		return
	}
	tmpFile.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", tmpPath,
		"-c:v", "libx264",
		"-frames:v", "1",
		"-preset", "ultrafast",
		"-profile:v", "baseline",
		"-level", "3.1",
		"-x264-params", "keyint=1:min-keyint=1:scenecut=0:bframes=0:repeat-headers=1",
		"-s", fmt.Sprintf("%dx%d", w, h),
		"-pix_fmt", "yuv420p",
		"-f", "h264",
		"pipe:1",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		sh.log.Warn("preGenerateSPSPPS: FFmpeg failed",
			"error", err, "stderr", stderr.String())
		return
	}

	nalus := fallback.ParseAnnexB(output)
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1F {
		case 7: // SPS
			sh.initialSPS = make([]byte, len(nalu))
			copy(sh.initialSPS, nalu)
		case 8: // PPS
			sh.initialPPS = make([]byte, len(nalu))
			copy(sh.initialPPS, nalu)
		}
	}

	if sh.initialSPS != nil && sh.initialPPS != nil {
		sh.log.Info("pre-generated SPS/PPS for SDP",
			"sps_size", len(sh.initialSPS),
			"pps_size", len(sh.initialPPS),
			"frame", fmt.Sprintf("%dx%d", w, h))
	} else {
		sh.log.Warn("preGenerateSPSPPS: could not extract SPS/PPS from FFmpeg output")
	}
}

// GetState returns the current camera state.
func (sh *StreamHandler) GetState() CameraState {
	return CameraState(sh.state.Load())
}

// GetCodec returns the detected (or configured) codec.
func (sh *StreamHandler) GetCodec() string {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.detectedCodec
}

// LastOnline returns the last time the camera was online.
func (sh *StreamHandler) LastOnline() time.Time {
	ts := sh.lastOnline.Load()
	if ts == 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// SetStream binds the output RTSP ServerStream.
func (sh *StreamHandler) SetStream(stream *gortsplib.ServerStream) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.stream = stream
}

// BuildDescription returns a session description matching the expected codec.
// It advertises both a video track and a PCMA audio track so that consumers
// like go2rtc / Frigate never complain about missing audio.
func (sh *StreamHandler) BuildDescription() *description.Session {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	var vf format.Format
	if sh.detectedCodec == "h265" {
		vf = &format.H265{PayloadTyp: 96}
	} else {
		vf = &format.H264{
			PayloadTyp:        96,
			PacketizationMode: 1,
			SPS:               sh.initialSPS,
			PPS:               sh.initialPPS,
		}
	}

	// PCMA (G.711 a-law) audio — static payload type 8.
	af := &format.G711{
		PayloadTyp:   8,
		MULaw:        false, // a-law = PCMA
		SampleRate:   8000,
		ChannelCount: 1,
	}

	sh.desc = &description.Session{
		Medias: []*description.Media{
			{
				Type:    description.MediaTypeVideo,
				Formats: []format.Format{vf},
			},
			{
				Type:    description.MediaTypeAudio,
				Formats: []format.Format{af},
			},
		},
	}
	return sh.desc
}

// Start begins the connection/relay/fallback loop and the silent audio generator.
func (sh *StreamHandler) Start(ctx context.Context) {
	ctx, sh.cancel = context.WithCancel(ctx)
	sh.wg.Add(1)
	go func() {
		defer sh.wg.Done()
		sh.loop(ctx)
	}()
	sh.startSilenceAudio(ctx)
}

// Stop terminates the handler and waits for all goroutines to finish.
func (sh *StreamHandler) Stop() {
	if sh.cancel != nil {
		sh.cancel()
	}
	sh.wg.Wait()
	sh.stopFallback()
	sh.stopSilenceAudio()
}

// -----------------------------------------------------------------------
// Main loop
// -----------------------------------------------------------------------

func (sh *StreamHandler) loop(ctx context.Context) {
	sh.log.Info("handler started",
		"battery_mode", sh.cfg.BatteryMode,
		"retry", sh.cfg.RetryInterval,
		"timeout", sh.cfg.Timeout,
	)

	for {
		select {
		case <-ctx.Done():
			sh.log.Info("handler stopped")
			return
		default:
		}

		sh.state.Store(int32(StateConnecting))

		err := sh.connectAndRelay(ctx)
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			sh.log.Warn("relay ended", "error", err)

			if sh.cfg.BatteryMode && sh.cfg.FallbackMode != "none" {
				sh.state.Store(int32(StateSleeping))
				sh.startFallback(ctx)
			} else {
				sh.state.Store(int32(StateDisconnected))
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(sh.cfg.RetryInterval):
		}
	}
}

// -----------------------------------------------------------------------
// RTSP source connection
// -----------------------------------------------------------------------

func (sh *StreamHandler) connectAndRelay(ctx context.Context) error {
	sh.log.Debug("connecting to source")

	u, err := base.ParseURL(sh.cfg.Source)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}

	c := &gortsplib.Client{
		ReadTimeout:  sh.cfg.Timeout,
		WriteTimeout: sh.cfg.Timeout,
	}

	if err := c.Start(u.Scheme, u.Host); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Close()

	// Bail out early if context was cancelled during connection.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	desc, _, err := c.Describe(u)
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}

	// Detect video media & codec.
	videoMedia, codec := sh.findVideoMedia(desc)
	if videoMedia == nil {
		return fmt.Errorf("no H.264/H.265 video track found")
	}

	sh.mu.Lock()
	sh.detectedCodec = codec
	// Copy source SPS/PPS to the output format so the SDP advertises
	// sprop-parameter-sets. This lets consumers determine dimensions
	// without probing the RTP stream.
	if codec == "h264" {
		var srcFmt *format.H264
		if desc.FindFormat(&srcFmt) != nil && srcFmt != nil && srcFmt.SPS != nil {
			if outFmt, ok := sh.desc.Medias[0].Formats[0].(*format.H264); ok {
				outFmt.SPS = srcFmt.SPS
				outFmt.PPS = srcFmt.PPS
			}
		}
	} else if codec == "h265" {
		var srcFmt *format.H265
		if desc.FindFormat(&srcFmt) != nil && srcFmt != nil && srcFmt.VPS != nil {
			if outFmt, ok := sh.desc.Medias[0].Formats[0].(*format.H265); ok {
				outFmt.VPS = srcFmt.VPS
				outFmt.SPS = srcFmt.SPS
				outFmt.PPS = srcFmt.PPS
			}
		}
	}
	sh.mu.Unlock()
	sh.log.Info("codec detected", "codec", codec)

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if _, err := c.Setup(desc.BaseURL, videoMedia, 0, 0); err != nil {
		return fmt.Errorf("setup: %w", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Packet arrival tracker.
	var (
		lastPktTime   = time.Now()
		lastPktTimeMu sync.Mutex
	)

	// Stop fallback BEFORE relay to prevent concurrent writes that
	// corrupt the H.264 bitstream (causing "data partitioning" errors).
	sh.stopFallback()

	// Register RTP callback.
	sh.registerCallback(c, desc, &lastPktTime, &lastPktTimeMu)

	if _, err := c.Play(nil); err != nil {
		return fmt.Errorf("play: %w", err)
	}

	sh.state.Store(int32(StateOnline))
	sh.lastOnline.Store(time.Now().Unix())
	sh.log.Info("camera online, relaying")

	// Monitor connection.
	ticker := time.NewTicker(sh.cfg.Timeout / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			lastPktTimeMu.Lock()
			elapsed := time.Since(lastPktTime)
			lastPktTimeMu.Unlock()

			if elapsed > sh.cfg.Timeout {
				return fmt.Errorf("timeout: no packet for %s", elapsed.Round(time.Millisecond))
			}
		}
	}
}

func (sh *StreamHandler) findVideoMedia(desc *description.Session) (*description.Media, string) {
	var h264f *format.H264
	if m := desc.FindFormat(&h264f); m != nil {
		return m, "h264"
	}
	var h265f *format.H265
	if m := desc.FindFormat(&h265f); m != nil {
		return m, "h265"
	}
	return nil, ""
}

func (sh *StreamHandler) registerCallback(
	c *gortsplib.Client,
	desc *description.Session,
	lastPktTime *time.Time,
	lastPktTimeMu *sync.Mutex,
) {
	// Find the concrete format to create the appropriate decoder.
	var h264f *format.H264
	var h265f *format.H265

	if desc.FindFormat(&h264f) != nil {
		dec, _ := h264f.CreateDecoder()
		c.OnPacketRTPAny(func(medi *description.Media, forma format.Format, pkt *rtp.Packet) {
			defer func() {
				if r := recover(); r != nil {
					sh.log.Error("panic in RTP callback", "error", r, "stack", string(debug.Stack()))
				}
			}()

			lastPktTimeMu.Lock()
			*lastPktTime = time.Now()
			lastPktTimeMu.Unlock()

			sh.writeToStream(pkt)

			if dec != nil {
				if au, err := dec.Decode(pkt); err == nil {
					sh.captureKeyframeH264(au)
				}
			}
		})
	} else if desc.FindFormat(&h265f) != nil {
		dec, _ := h265f.CreateDecoder()
		c.OnPacketRTPAny(func(medi *description.Media, forma format.Format, pkt *rtp.Packet) {
			defer func() {
				if r := recover(); r != nil {
					sh.log.Error("panic in RTP callback", "error", r, "stack", string(debug.Stack()))
				}
			}()

			lastPktTimeMu.Lock()
			*lastPktTime = time.Now()
			lastPktTimeMu.Unlock()

			sh.writeToStream(pkt)

			if dec != nil {
				if au, err := dec.Decode(pkt); err == nil {
					sh.captureKeyframeH265(au)
				}
			}
		})
	}
}

func (sh *StreamHandler) writeToStream(pkt *rtp.Packet) {
	defer func() {
		if r := recover(); r != nil {
			sh.log.Error("panic writing RTP packet",
				"error", r,
				"stack", string(debug.Stack()),
			)
		}
	}()

	sh.mu.RLock()
	stream := sh.stream
	desc := sh.desc
	sh.mu.RUnlock()

	if stream == nil || desc == nil || len(desc.Medias) == 0 {
		return
	}

	// Remap payload type to match our output format (96).
	// The source camera may use a different dynamic PT.
	pkt.PayloadType = 96

	stream.WritePacketRTP(desc.Medias[0], pkt)
}

// writeAudioToStream safely writes an audio RTP packet to the output server
// stream (media index 1 = audio).
func (sh *StreamHandler) writeAudioToStream(pkt *rtp.Packet) {
	defer func() {
		if r := recover(); r != nil {
			sh.log.Error("panic writing audio RTP packet",
				"error", r,
				"stack", string(debug.Stack()),
			)
		}
	}()

	sh.mu.RLock()
	stream := sh.stream
	desc := sh.desc
	sh.mu.RUnlock()

	if stream == nil || desc == nil || len(desc.Medias) < 2 {
		return
	}

	stream.WritePacketRTP(desc.Medias[1], pkt)
}

// -----------------------------------------------------------------------
// Silent audio (PCMA) — continuous G.711 a-law silence so go2rtc is happy
// -----------------------------------------------------------------------

func (sh *StreamHandler) startSilenceAudio(ctx context.Context) {
	sh.silenceMu.Lock()
	defer sh.silenceMu.Unlock()

	if sh.silenceCancel != nil {
		return
	}

	silCtx, cancel := context.WithCancel(ctx)
	sh.silenceCancel = cancel
	sh.silenceWg.Add(1)
	go func() {
		defer sh.silenceWg.Done()
		sh.runSilenceAudio(silCtx)
	}()
}

func (sh *StreamHandler) stopSilenceAudio() {
	sh.silenceMu.Lock()
	defer sh.silenceMu.Unlock()

	if sh.silenceCancel != nil {
		sh.silenceCancel()
		sh.silenceCancel = nil
	}
	sh.silenceWg.Wait()
}

func (sh *StreamHandler) runSilenceAudio(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			sh.log.Error("panic in silence audio", "error", r, "stack", string(debug.Stack()))
		}
	}()

	// PCMA 8 kHz, 20 ms packets → 160 samples/packet.
	const (
		sampleRate       = 8000
		packetDurationMs = 20
		samplesPerPacket = sampleRate * packetDurationMs / 1000 // 160
	)

	// G.711 a-law silence byte.
	silence := make([]byte, samplesPerPacket)
	for i := range silence {
		silence[i] = 0xD5
	}

	ticker := time.NewTicker(time.Duration(packetDurationMs) * time.Millisecond)
	defer ticker.Stop()

	var seq uint16
	var ts uint32

	sh.log.Debug("silence audio started")

	for {
		select {
		case <-ctx.Done():
			sh.log.Debug("silence audio stopped")
			return
		case <-ticker.C:
		}

		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    8, // PCMA
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           0xDEADABCD,
			},
			Payload: silence,
		}
		sh.writeAudioToStream(pkt)

		seq++
		ts += samplesPerPacket
	}
}

// -----------------------------------------------------------------------
// Keyframe capture (for fallback image)
// -----------------------------------------------------------------------

func (sh *StreamHandler) captureKeyframeH264(au [][]byte) {
	// Also extract and propagate SPS/PPS to the output format when
	// we see them from the real camera. This updates the SDP's
	// sprop-parameter-sets for new consumer sessions.
	var sps, pps []byte
	hasIDR := false

	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1F {
		case 7:
			sps = nalu
		case 8:
			pps = nalu
		case 5:
			hasIDR = true
		}
	}

	// Update SPS/PPS in the output format.
	if sps != nil && pps != nil {
		sh.mu.Lock()
		if h264Fmt, ok := sh.desc.Medias[0].Formats[0].(*format.H264); ok {
			h264Fmt.SPS = sps
			h264Fmt.PPS = pps
		}
		sh.mu.Unlock()
	}

	if hasIDR {
		sh.decodeAndStore(au, "h264")
	}
}

func (sh *StreamHandler) captureKeyframeH265(au [][]byte) {
	for _, nalu := range au {
		if len(nalu) < 2 {
			continue
		}
		naluType := (nalu[0] >> 1) & 0x3F
		if naluType == 19 || naluType == 20 { // IDR_W_RADL / IDR_N_LP
			sh.decodeAndStore(au, "h265")
			return
		}
	}
}

func (sh *StreamHandler) decodeAndStore(nalus [][]byte, codec string) {
	// Rate-limit: max one capture every 5 seconds to avoid FFmpeg storms.
	now := time.Now().Unix()
	if last := sh.lastCaptureTS.Load(); now-last < 5 {
		return
	}
	sh.lastCaptureTS.Store(now)

	// Build Annex-B stream.
	var buf bytes.Buffer
	for _, nalu := range nalus {
		buf.Write([]byte{0x00, 0x00, 0x00, 0x01})
		buf.Write(nalu)
	}

	inFmt := "h264"
	if codec == "h265" {
		inFmt = "hevc"
	}

	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", inFmt, "-i", "pipe:0",
		"-frames:v", "1",
		"-f", "image2pipe", "-vcodec", "png",
		"pipe:1",
	)
	cmd.Stdin = &buf

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		sh.log.Debug("keyframe decode failed", "error", err, "stderr", stderr.String())
		return
	}

	img, err := png.Decode(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		sh.log.Debug("png decode failed", "error", err)
		return
	}

	b := img.Bounds()
	sh.fallbackGen.UpdateFrame(stdout.Bytes(), b.Dx(), b.Dy())
	sh.log.Debug("keyframe captured", "w", b.Dx(), "h", b.Dy())
}

// -----------------------------------------------------------------------
// Fallback stream generation
// -----------------------------------------------------------------------

func (sh *StreamHandler) startFallback(ctx context.Context) {
	sh.fallbackMu.Lock()
	defer sh.fallbackMu.Unlock()

	if sh.fallbackCancel != nil {
		return // already running
	}

	fbCtx, cancel := context.WithCancel(ctx)
	sh.fallbackCancel = cancel
	sh.wg.Add(1)
	sh.fallbackWg.Add(1)
	go func() {
		defer sh.wg.Done()
		defer sh.fallbackWg.Done()
		// Clear fallbackCancel when the goroutine exits for ANY reason
		// (context cancelled, FFmpeg failure, no PNG data, etc.).
		// Without this, an early exit leaves fallbackCancel non-nil,
		// and startFallback would think the goroutine is still running.
		defer func() {
			sh.fallbackMu.Lock()
			sh.fallbackCancel = nil
			sh.fallbackMu.Unlock()
		}()
		sh.runFallback(fbCtx)
	}()
}

// stopFallback cancels the fallback goroutine and WAITS for it to finish.
// This guarantees no fallback writes can race with the real camera relay.
func (sh *StreamHandler) stopFallback() {
	sh.fallbackMu.Lock()
	cancel := sh.fallbackCancel
	sh.fallbackCancel = nil
	sh.fallbackMu.Unlock()

	if cancel != nil {
		cancel()
	}
	sh.fallbackWg.Wait()
}

func (sh *StreamHandler) runFallback(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			sh.log.Error("panic in fallback loop", "error", r, "stack", string(debug.Stack()))
		}
	}()

	codec := sh.GetCodec()
	fps := sh.cfg.FallbackFPS
	if fps <= 0 {
		fps = 5
	}

	// Get fallback PNG from the generator.
	pngData, w, h := sh.fallbackGen.GetFramePNG()
	if len(pngData) == 0 {
		sh.log.Error("no fallback frame available")
		return
	}

	// H.264/H.265 require even dimensions.
	if w%2 != 0 {
		w++
	}
	if h%2 != 0 {
		h++
	}

	// Write PNG to a temp file for FFmpeg.
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("fallback-%s-*.png", sh.Name))
	if err != nil {
		sh.log.Error("fallback: create temp file", "error", err)
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(pngData); err != nil {
		tmpFile.Close()
		sh.log.Error("fallback: write temp file", "error", err)
		return
	}
	tmpFile.Close()

	// Bail out early if context was cancelled during file I/O.
	if ctx.Err() != nil {
		return
	}

	// ── Encode ONE IDR frame with FFmpeg ─────────────────────────────
	//
	// Previous approach used a continuous FFmpeg process with a streaming
	// Annex-B parser piped through goroutines/channels. This was fragile:
	// pipe buffering and byte-level parser edge cases could silently
	// corrupt NAL unit boundaries, producing garbage NAL types (2/3/4)
	// that FFmpeg on the consumer side reports as "data partitioning is
	// not implemented".
	//
	// New approach: run FFmpeg once (-frames:v 1), batch-parse the output
	// with ParseAnnexB, and re-emit the same [SPS, PPS, IDR] access unit
	// at the configured FPS. Zero goroutines, zero channels, zero
	// streaming parser state — just a simple ticker loop.

	encCodec := "libx264"
	outFmt := "h264"
	var encoderArgs []string

	switch codec {
	case "h265":
		encCodec = "libx265"
		outFmt = "hevc"
		encoderArgs = []string{
			"-preset", "ultrafast",
			"-x265-params", "keyint=1:min-keyint=1:scenecut=0:bframes=0:repeat-headers=1:log-level=error",
		}
	default:
		encoderArgs = []string{
			"-preset", "ultrafast",
			"-profile:v", "baseline",
			"-level", "3.1",
			"-x264-params", "keyint=1:min-keyint=1:scenecut=0:bframes=0:repeat-headers=1",
		}
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-i", tmpPath,
		"-c:v", encCodec,
		"-frames:v", "1",
	}
	args = append(args, encoderArgs...)
	args = append(args,
		"-s", fmt.Sprintf("%dx%d", w, h),
		"-pix_fmt", "yuv420p",
		"-f", outFmt,
		"pipe:1",
	)

	// Use a dedicated timeout so encoding completes even if the
	// parent context is cancelled while FFmpeg is running.
	encCtx, encCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer encCancel()

	cmd := exec.CommandContext(encCtx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		sh.log.Error("fallback: FFmpeg encode failed",
			"error", err, "stderr", stderr.String())
		return
	}

	// Batch-parse the Annex-B output into individual NAL units.
	au := fallback.ParseAnnexB(output)
	if len(au) == 0 {
		sh.log.Error("fallback: no NAL units in FFmpeg output",
			"outputLen", len(output))
		return
	}

	// ── Classify NALUs and update SDP ────────────────────────────────
	// Extract SPS/PPS so we can:
	// 1. Set sprop-parameter-sets in the SDP → consumers know dimensions
	//    immediately without probing.
	// 2. Filter out AUD/filler NALUs that some consumers don't expect.

	var sps, pps []byte
	var filteredAU [][]byte
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		nalType := nalu[0] & 0x1F
		if codec == "h265" && len(nalu) >= 2 {
			nalType = (nalu[0] >> 1) & 0x3F
		}

		sh.log.Info("fallback NAL unit",
			"type", nalType, "size", len(nalu))

		if codec != "h265" {
			switch nalType {
			case 7: // SPS
				sps = nalu
				filteredAU = append(filteredAU, nalu)
			case 8: // PPS
				pps = nalu
				filteredAU = append(filteredAU, nalu)
			case 5, 1: // IDR, non-IDR slice
				filteredAU = append(filteredAU, nalu)
			case 6: // SEI — include for compatibility
				filteredAU = append(filteredAU, nalu)
			// Skip AUD (9), filler (12), and other non-essential NALUs.
			default:
				sh.log.Debug("fallback: skipping NALU type", "type", nalType)
			}
		} else {
			// H.265: keep all NALUs
			filteredAU = append(filteredAU, nalu)
		}
	}

	if len(filteredAU) == 0 {
		sh.log.Error("fallback: no usable NAL units after filtering")
		return
	}
	au = filteredAU

	// Update SDP with SPS/PPS so consumers get sprop-parameter-sets.
	if codec == "h264" && sps != nil && pps != nil {
		sh.mu.Lock()
		if h264Fmt, ok := sh.desc.Medias[0].Formats[0].(*format.H264); ok {
			h264Fmt.SPS = sps
			h264Fmt.PPS = pps
		}
		sh.mu.Unlock()
		sh.log.Info("fallback: updated SPS/PPS in SDP",
			"sps_size", len(sps), "pps_size", len(pps))
	}

	// ── Set up RTP encoder ───────────────────────────────────────────
	//
	// CRITICAL: we encode each NALU individually to avoid STAP-A
	// aggregation. gortsplib's rtph264.Encoder packs small NALUs into
	// STAP-A packets. If ANY intermediary (go2rtc, FFmpeg depayloader)
	// mishandles STAP-A de-aggregation, the H.264 decoder sees garbage
	// NAL types → "data partitioning is not implemented" errors.
	//
	// By sending each NALU as a separate single-NALU RTP packet, we
	// use the simplest, most universally compatible RTP format.

	var h264Enc *rtph264.Encoder
	var h265Enc *rtph265.Encoder

	if codec == "h265" {
		h265Enc = &rtph265.Encoder{PayloadType: 96, PayloadMaxSize: 1200}
		if err := h265Enc.Init(); err != nil {
			sh.log.Error("h265 RTP encoder init", "error", err)
			return
		}
	} else {
		h264Enc = &rtph264.Encoder{PayloadType: 96, PayloadMaxSize: 1200}
		if err := h264Enc.Init(); err != nil {
			sh.log.Error("h264 RTP encoder init", "error", err)
			return
		}
	}

	clockRate := uint32(90000)
	tsInc := clockRate / uint32(fps)
	var ts uint32

	sh.log.Info("fallback stream started",
		"codec", codec, "fps", fps,
		"size", fmt.Sprintf("%dx%d", w, h),
		"nalus", len(au))

	// ── Emission loop ────────────────────────────────────────────────
	// Re-emit the same pre-encoded access unit at the configured FPS.
	// Each NALU is sent as a SEPARATE RTP packet (no STAP-A) for
	// maximum compatibility with all RTSP consumers.

	ticker := time.NewTicker(time.Second / time.Duration(fps))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			sh.log.Info("fallback stream stopped")
			return
		case <-ticker.C:
		}

		if codec == "h265" && h265Enc != nil {
			// For H.265: encode each NALU individually too.
			var allPkts []*rtp.Packet
			for _, nalu := range au {
				if pkts, err := h265Enc.Encode([][]byte{nalu}); err == nil {
					allPkts = append(allPkts, pkts...)
				}
			}
			for i, pkt := range allPkts {
				pkt.Header.Marker = (i == len(allPkts)-1)
				pkt.Timestamp = ts
				sh.writeToStream(pkt)
			}
		} else if h264Enc != nil {
			// Encode each NALU separately → one RTP packet per NALU
			// (or FU-A fragments for large NALUs). This avoids
			// STAP-A aggregation entirely.
			var allPkts []*rtp.Packet
			for _, nalu := range au {
				if pkts, err := h264Enc.Encode([][]byte{nalu}); err == nil {
					allPkts = append(allPkts, pkts...)
				}
			}
			// Marker bit: only on the very last packet of the AU.
			for i, pkt := range allPkts {
				pkt.Header.Marker = (i == len(allPkts)-1)
				pkt.Timestamp = ts
				sh.writeToStream(pkt)
			}
		}
		ts += tsInc
	}
}
