package proxy

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
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
// connect -> relay -> detect sleep -> fallback -> reconnect.
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

	// Silent audio (PCMA) -- keeps go2rtc/Frigate happy.
	silenceMu     sync.Mutex
	silenceCancel context.CancelFunc
	silenceWg     sync.WaitGroup

	// Rate-limiting keyframe captures (unix seconds).
	lastCaptureTS atomic.Int64

	// Pre-generated SPS/PPS for the initial SDP.
	initialSPS []byte
	initialPPS []byte

	// Pre-encoded fallback NAL units. Generated synchronously at init
	// time so that runFallback can start sending RTP packets within
	// milliseconds instead of waiting for FFmpeg encoding (seconds).
	preBaseAU [][]byte // base fallback frame (Access Unit)
	preDotAU  [][]byte // dot-indicator variant

	// Callback to rebuild the server stream when the detected codec changes
	// at runtime (e.g. camera SDP reveals H.265 but initial SDP was H.264).
	rebuildCb func(*description.Session) *gortsplib.ServerStream

	// -- RTP output normaliser --
	// Delivers a single continuous RTP stream (constant SSRC, monotonic
	// SeqNum, smooth timestamps) to consumers regardless of source
	// switches between camera relay and fallback.
	videoSSRC    uint32
	videoSeq     uint16
	videoSrcBase uint32 // first RTP TS from current source
	videoOutBase uint32 // output TS corresponding to srcBase
	videoLastOut uint32 // last emitted output TS
	videoInited  bool
	videoMu      sync.Mutex // serialises all video writes

	// -- Audio RTP normaliser --
	audioSSRC uint32
	audioSeq  uint16
	audioMu   sync.Mutex

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
	sh.videoSSRC = 0x46414C4C // "FALL" -- constant across source switches
	sh.audioSSRC = 0x41554449 // "AUDI" -- constant across source switches

	// Apply configured resolution hints to the fallback generator.
	if cfg.Width > 0 && cfg.Height > 0 {
		sh.fallbackGen.UpdateFrame(nil, cfg.Width, cfg.Height)
		sh.log.Info("fallback resolution from config",
			"width", cfg.Width, "height", cfg.Height)
	}

	// Pre-encode fallback frames so the SDP has sprop-parameter-sets
	// and runFallback can send data within milliseconds.
	if cfg.FallbackMode != "none" {
		sh.preEncodeFallback()
	}

	return sh
}

// preEncodeFallback runs FFmpeg synchronously to encode the fallback
// placeholder into H.264 (or H.265) NAL units. Both the base frame and
// the dot-indicator variant are encoded, so runFallback can start
// sending RTP packets instantly (no FFmpeg call at runtime).
func (sh *StreamHandler) preEncodeFallback() {
	codec := sh.detectedCodec

	pngData, w, h := sh.fallbackGen.GetFramePNG()
	if len(pngData) == 0 {
		sh.log.Warn("preEncodeFallback: no PNG available")
		return
	}

	if w%2 != 0 {
		w++
	}
	if h%2 != 0 {
		h++
	}

	// Encode base frame.
	baseAU, sps, pps := sh.encodePNGToAU(pngData, w, h, codec)
	if len(baseAU) == 0 {
		sh.log.Warn("preEncodeFallback: base frame encoding failed")
		return
	}

	// Store SPS/PPS for the SDP.
	if codec != "h265" && sps != nil && pps != nil {
		sh.initialSPS = copyBytes(sps)
		sh.initialPPS = copyBytes(pps)
	}

	// Deep-copy the NALUs so they're fully owned.
	sh.preBaseAU = make([][]byte, len(baseAU))
	for i, nalu := range baseAU {
		sh.preBaseAU[i] = copyBytes(nalu)
	}

	// Encode dot-indicator variant.
	dotPng := addIndicatorDot(pngData)
	if dotPng != nil {
		dotAU, _, _ := sh.encodePNGToAU(dotPng, w, h, codec)
		if len(dotAU) > 0 {
			sh.preDotAU = make([][]byte, len(dotAU))
			for i, nalu := range dotAU {
				sh.preDotAU[i] = copyBytes(nalu)
			}
		}
	}

	sh.log.Info("pre-encoded fallback frames",
		"codec", codec,
		"sps_len", len(sh.initialSPS),
		"pps_len", len(sh.initialPPS),
		"base_nalus", len(sh.preBaseAU),
		"dot_nalus", len(sh.preDotAU),
		"size", fmt.Sprintf("%dx%d", w, h))
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
// Video-only: no audio track is advertised. Battery cameras typically use
// G.711 (pcm_alaw) which cannot be muxed into MP4 containers. Omitting
// audio entirely avoids "Could not find tag for codec pcm_alaw" errors
// in Frigate's recording FFmpeg.
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

	af := &format.G711{
		PayloadTyp:   8,
		MULaw:        false,
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

	// Log SDP readiness so operators can verify sprop-parameter-sets.
	if sh.detectedCodec != "h265" {
		if h, ok := vf.(*format.H264); ok {
			sh.log.Info("SDP built",
				"has_sps", h.SPS != nil,
				"has_pps", h.PPS != nil,
				"sps_len", len(h.SPS),
				"pps_len", len(h.PPS))
		}
	}

	return sh.desc
}

// SetRebuildCallback registers the function called when the output server
// stream must be rebuilt (e.g. codec auto-detection switches H.264↔H.265).
func (sh *StreamHandler) SetRebuildCallback(cb func(*description.Session) *gortsplib.ServerStream) {
	sh.rebuildCb = cb
}

// rebuildForCodec handles a runtime codec change: stops current output,
// re-encodes fallback frames with the new codec, rebuilds the description
// and server stream, then restarts fallback.
func (sh *StreamHandler) rebuildForCodec(ctx context.Context, newCodec string) {
	sh.log.Info("codec change detected, rebuilding stream",
		"old_codec", sh.detectedCodec, "new_codec", newCodec)

	// Stop current output producers.
	sh.stopFallback()
	sh.stopSilenceAudio()

	// Update codec.
	sh.mu.Lock()
	sh.detectedCodec = newCodec
	sh.mu.Unlock()

	// Re-encode fallback frames with the new codec (sets initialSPS/PPS).
	sh.preEncodeFallback()

	// Rebuild SDP and server stream.
	newDesc := sh.BuildDescription()
	if sh.rebuildCb != nil {
		// Clear stream to block writes during transition.
		sh.mu.Lock()
		sh.stream = nil
		sh.mu.Unlock()

		newStream := sh.rebuildCb(newDesc)

		sh.mu.Lock()
		sh.stream = newStream
		sh.mu.Unlock()
	}

	// Restart fallback (with new codec) so consumers have data immediately.
	sh.startFallback(ctx)
}

// Start begins the connection/relay/fallback loop.
func (sh *StreamHandler) Start(ctx context.Context) {
	ctx, sh.cancel = context.WithCancel(ctx)
	sh.wg.Add(1)
	go func() {
		defer sh.wg.Done()
		sh.loop(ctx)
	}()
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

	// For battery cameras, start fallback IMMEDIATELY so consumers
	// (Frigate FFmpeg) always have video from the very first moment
	// they connect. Without this, the first connectAndRelay() blocks
	// for up to `timeout` seconds while trying the camera, during
	// which zero RTP packets are written --> FFmpeg times out on
	// format probing --> "no recording segments created".
	if sh.cfg.BatteryMode && sh.cfg.FallbackMode != "none" {
		sh.state.Store(int32(StateSleeping))
		sh.startFallback(ctx)
	}

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

	if ctx.Err() != nil {
		return ctx.Err()
	}

	srcDesc, _, err := c.Describe(u)
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}

	// Detect video media & codec.
	videoMedia, codec := sh.findVideoMedia(srcDesc)
	if videoMedia == nil {
		return fmt.Errorf("no H.264/H.265 video track found")
	}

	// If the camera's actual codec differs from what the SDP was built with
	// (e.g. SDP=H.264 but camera=H.265), rebuild the entire server stream
	// so consumers get the correct SDP and codec.
	sh.mu.RLock()
	currentCodec := sh.detectedCodec
	sh.mu.RUnlock()
	if codec != currentCodec {
		sh.rebuildForCodec(ctx, codec)
	}

	sh.mu.Lock()
	sh.detectedCodec = codec

	// Copy source SPS/PPS to the output format so the SDP advertises
	// the camera's actual sprop-parameter-sets. Also extract dimensions
	// to update the fallback generator.
	if codec == "h264" {
		var srcFmt *format.H264
		if srcDesc.FindFormat(&srcFmt) != nil && srcFmt != nil {
			if outFmt, ok := sh.desc.Medias[0].Formats[0].(*format.H264); ok {
				if srcFmt.SPS != nil {
					outFmt.SPS = srcFmt.SPS
					outFmt.PPS = srcFmt.PPS
				}
			}
			// Extract resolution from SPS for fallback generator.
			sh.updateDimensionsFromSPS(srcFmt.SPS)
		}
	} else if codec == "h265" {
		var srcFmt *format.H265
		if srcDesc.FindFormat(&srcFmt) != nil && srcFmt != nil {
			if outFmt, ok := sh.desc.Medias[0].Formats[0].(*format.H265); ok {
				if srcFmt.VPS != nil {
					outFmt.VPS = srcFmt.VPS
					outFmt.SPS = srcFmt.SPS
					outFmt.PPS = srcFmt.PPS
				}
			}
		}
	}
	sh.mu.Unlock()
	sh.log.Info("codec detected", "codec", codec)

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if _, err := c.Setup(srcDesc.BaseURL, videoMedia, 0, 0); err != nil {
		return fmt.Errorf("setup video: %w", err)
	}

	// Also setup audio if the camera offers it.
	audioMedia := sh.findAudioMedia(srcDesc)
	if audioMedia != nil {
		if _, err := c.Setup(srcDesc.BaseURL, audioMedia, 0, 0); err != nil {
			sh.log.Warn("audio setup failed, continuing video-only", "error", err)
			audioMedia = nil
		} else {
			sh.log.Info("camera audio track set up")
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Packet arrival tracker.
	var (
		lastPktTime   = time.Now()
		lastPktTimeMu sync.Mutex
	)

	// DO NOT stop fallback here. Fallback keeps running and producing
	// video frames until the camera sends its first IDR. The RTP callback
	// in registerCallback performs a seamless, gap-free transition:
	// stopFallback → resetTimeline → forward IDR.
	//
	// Previously we stopped fallback here, creating a dead period with
	// NO video output until the first camera IDR. During that gap,
	// go2rtc / Frigate FFmpeg would fail with "Could not find codec
	// parameters" or produce garbage frames.

	// Register RTP callback with decode+re-encode.
	sh.registerCallback(c, srcDesc, audioMedia, &lastPktTime, &lastPktTimeMu)

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

func (sh *StreamHandler) findAudioMedia(desc *description.Session) *description.Media {
	for _, m := range desc.Medias {
		if m.Type == description.MediaTypeAudio {
			return m
		}
	}
	return nil
}

// registerCallback sets up the RTP callback that DECODES camera RTP packets
// into NAL units, then RE-ENCODES them using our own encoder. This guarantees
// every output packet exactly matches our SDP format (PacketizationMode=1,
// PT=96, correct FU-A/STAP-A boundaries). Raw packet forwarding is the root
// cause of "data partitioning is not implemented" errors -- the camera's RTP
// packetization does not necessarily match our declared format.
func (sh *StreamHandler) registerCallback(
	c *gortsplib.Client,
	desc *description.Session,
	audioMedia *description.Media,
	lastPktTime *time.Time,
	lastPktTimeMu *sync.Mutex,
) {
	var h264f *format.H264
	var h265f *format.H265

	if desc.FindFormat(&h264f) != nil {
		// Create decoder from source format -- this handles FU-A reassembly,
		// STAP-A splitting, reordering, etc.
		dec, decErr := h264f.CreateDecoder()
		if decErr != nil {
			sh.log.Error("failed to create H264 decoder", "error", decErr)
			return
		}

		// Create our OWN encoder matching the output SDP exactly.
		enc := &rtph264.Encoder{
			PayloadType:       96,
			PayloadMaxSize:    1200,
			PacketizationMode: 1,
		}
		if err := enc.Init(); err != nil {
			sh.log.Error("failed to create H264 output encoder", "error", err)
			return
		}

		// Track SPS/PPS from the camera (initially from DESCRIBE SDP,
		// updated from in-band parameter sets). Used to inject SPS/PPS
		// before IDR frames when the camera omits them in-band.
		camSPS := copyBytes(h264f.SPS)
		camPPS := copyBytes(h264f.PPS)
		seenIDR := false
		silenceStopped := false

		c.OnPacketRTPAny(func(medi *description.Media, forma format.Format, pkt *rtp.Packet) {
			defer func() {
				if r := recover(); r != nil {
					sh.log.Error("panic in RTP callback", "error", r, "stack", string(debug.Stack()))
				}
			}()

			lastPktTimeMu.Lock()
			*lastPktTime = time.Now()
			lastPktTimeMu.Unlock()

			// Forward audio packets from the camera.
			if audioMedia != nil && medi == audioMedia {
				if !silenceStopped {
					sh.stopSilenceAudio()
					silenceStopped = true
					sh.log.Info("camera audio detected, silence stopped")
				}
				sh.writeAudioToStream(pkt)
				return
			}

			au, err := dec.Decode(pkt)
			if err != nil || au == nil {
				return
			}

			// Filter out invalid / unsupported NAL types and ensure
			// the AU contains at least one VCL NALU (slice data).
			// AUs with only parameter sets / SEI cause FFmpeg to
			// report "missing picture in access unit" then
			// mis-parse subsequent data as "data partitioning".
			au = filterH264NALUs(au)
			if len(au) == 0 {
				return
			}
			if !hasVCL(au) {
				return // No picture data -- skip entirely
			}

			// Wait for the first IDR before forwarding anything.
			if !seenIDR {
				hasIDR := false
				for _, nalu := range au {
					if len(nalu) > 0 && (nalu[0]&0x1F) == 5 {
						hasIDR = true
						break
					}
				}
				if !hasIDR {
					return
				}
				// Seamless transition: stop fallback and reset timeline
				// immediately before forwarding the first camera IDR.
				// This ensures zero gap in the output stream.
				sh.stopFallback()
				sh.resetVideoTimeline()
				seenIDR = true
				sh.log.Info("first IDR received, seamless transition to camera")
			}

			// Track in-band SPS/PPS updates from the camera.
			for _, nalu := range au {
				if len(nalu) == 0 {
					continue
				}
				switch nalu[0] & 0x1F {
				case 7:
					camSPS = copyBytes(nalu)
				case 8:
					camPPS = copyBytes(nalu)
				}
			}

			// Ensure every IDR AU includes SPS + PPS so the bitstream
			// is fully self-contained. Without this, downstream muxers
			// fail with "Could not write header" because they rely on
			// in-band parameter sets rather than SDP sprop.
			au = ensureSPSPPS(au, camSPS, camPPS)

			sh.captureKeyframeH264(au)

			outPkts, err := enc.Encode(au)
			if err != nil {
				sh.log.Debug("H264 re-encode error", "error", err)
				return
			}

			for _, outPkt := range outPkts {
				outPkt.Timestamp = pkt.Timestamp
				sh.writeToStream(outPkt)
			}
		})

	} else if desc.FindFormat(&h265f) != nil {
		dec, decErr := h265f.CreateDecoder()
		if decErr != nil {
			sh.log.Error("failed to create H265 decoder", "error", decErr)
			return
		}

		enc := &rtph265.Encoder{
			PayloadType:    96,
			PayloadMaxSize: 1200,
		}
		if err := enc.Init(); err != nil {
			sh.log.Error("failed to create H265 output encoder", "error", err)
			return
		}

		camVPS := copyBytes(h265f.VPS)
		camSPS := copyBytes(h265f.SPS)
		camPPS := copyBytes(h265f.PPS)
		seenIRAP := false
		silenceStopped := false

		c.OnPacketRTPAny(func(medi *description.Media, forma format.Format, pkt *rtp.Packet) {
			defer func() {
				if r := recover(); r != nil {
					sh.log.Error("panic in RTP callback", "error", r, "stack", string(debug.Stack()))
				}
			}()

			lastPktTimeMu.Lock()
			*lastPktTime = time.Now()
			lastPktTimeMu.Unlock()

			// Forward audio packets from the camera.
			if audioMedia != nil && medi == audioMedia {
				if !silenceStopped {
					sh.stopSilenceAudio()
					silenceStopped = true
					sh.log.Info("camera audio detected, silence stopped")
				}
				sh.writeAudioToStream(pkt)
				return
			}

			au, err := dec.Decode(pkt)
			if err != nil || au == nil {
				return
			}

			au = filterH265NALUs(au)
			if len(au) == 0 {
				return
			}
			if !hasH265VCL(au) {
				return // No picture data -- skip entirely
			}

			if !seenIRAP {
				hasIRAP := false
				for _, nalu := range au {
					if len(nalu) >= 2 {
						nt := (nalu[0] >> 1) & 0x3F
						if nt >= 16 && nt <= 21 {
							hasIRAP = true
							break
						}
					}
				}
				if !hasIRAP {
					return
				}
				sh.stopFallback()
				sh.resetVideoTimeline()
				seenIRAP = true
				sh.log.Info("first IRAP received, seamless transition to camera")
			}

			// Track in-band VPS/SPS/PPS.
			for _, nalu := range au {
				if len(nalu) < 2 {
					continue
				}
				nt := (nalu[0] >> 1) & 0x3F
				switch nt {
				case 32:
					camVPS = copyBytes(nalu)
				case 33:
					camSPS = copyBytes(nalu)
				case 34:
					camPPS = copyBytes(nalu)
				}
			}

			au = ensureVPSSPSPPS(au, camVPS, camSPS, camPPS)

			sh.captureKeyframeH265(au)

			outPkts, err := enc.Encode(au)
			if err != nil {
				sh.log.Debug("H265 re-encode error", "error", err)
				return
			}

			for _, outPkt := range outPkts {
				outPkt.Timestamp = pkt.Timestamp
				sh.writeToStream(outPkt)
			}
		})
	}
}

// updateDimensionsFromSPS parses the H.264 SPS to extract width/height
// and updates the fallback generator accordingly. This ensures the fallback
// frame matches the camera's actual resolution (critical for low streams
// which are typically 640x480 or similar, not 1920x1080).
func (sh *StreamHandler) updateDimensionsFromSPS(sps []byte) {
	if len(sps) < 4 {
		return
	}

	// Minimal SPS parsing: extract pic_width/pic_height from the bitstream.
	// Full SPS parsing is complex (Exp-Golomb coded), so we use FFmpeg
	// to probe the resolution from a minimal Annex-B stream.
	var annexB bytes.Buffer
	annexB.Write([]byte{0x00, 0x00, 0x00, 0x01})
	annexB.Write(sps)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner",
		"-f", "h264", "-i", "pipe:0",
		"-f", "null", "-",
	)
	cmd.Stdin = &annexB
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	cmd.Run() // Intentionally ignore error -- we parse stderr for dimensions.

	output := stderr.String()
	w, h := parseDimensionsFromFFmpeg(output)
	if w > 0 && h > 0 {
		sh.fallbackGen.UpdateFrame(nil, w, h)
		sh.log.Info("updated dimensions from camera SPS",
			"width", w, "height", h)
	}
}

// parseDimensionsFromFFmpeg extracts WxH from FFmpeg stderr output.
// Looks for patterns like "1920x1080" or "640x480" in stream info lines.
func parseDimensionsFromFFmpeg(output string) (int, int) {
	// FFmpeg prints lines like: "Stream #0:0: Video: h264 ..., 1920x1080, ..."
	// We look for NxM patterns where N and M are reasonable dimensions.
	var w, h int
	for i := 0; i < len(output)-4; i++ {
		if output[i] >= '1' && output[i] <= '9' {
			// Try to parse WxH
			j := i
			for j < len(output) && output[j] >= '0' && output[j] <= '9' {
				j++
			}
			if j < len(output) && output[j] == 'x' {
				k := j + 1
				for k < len(output) && output[k] >= '0' && output[k] <= '9' {
					k++
				}
				if k > j+1 {
					wStr := output[i:j]
					hStr := output[j+1 : k]
					var tw, th int
					fmt.Sscanf(wStr, "%d", &tw)
					fmt.Sscanf(hStr, "%d", &th)
					if tw >= 160 && tw <= 7680 && th >= 120 && th <= 4320 {
						w, h = tw, th
						break
					}
				}
			}
		}
	}
	return w, h
}

// resetVideoTimeline is called whenever the video source changes
// (fallback -> camera or camera -> fallback). The next writeToStream
// call will map the new source's first timestamp so that the output
// timeline continues smoothly from where the previous source left off.
func (sh *StreamHandler) resetVideoTimeline() {
	sh.videoMu.Lock()
	defer sh.videoMu.Unlock()
	sh.videoInited = false
}

// writeToStream normalises an RTP packet and writes it to the output
// server stream. All output packets share a single SSRC with monotonic
// sequence numbers and a smooth timestamp timeline, regardless of
// whether the packet originates from the camera relay or fallback.
func (sh *StreamHandler) writeToStream(pkt *rtp.Packet) {
	sh.videoMu.Lock()
	defer sh.videoMu.Unlock()

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

	// -- Normalise --
	pkt.PayloadType = 96
	pkt.SSRC = sh.videoSSRC
	pkt.SequenceNumber = sh.videoSeq
	sh.videoSeq++

	if !sh.videoInited {
		sh.videoSrcBase = pkt.Timestamp
		if sh.videoLastOut == 0 {
			sh.videoOutBase = 0
		} else {
			// Leave a 40 ms gap (~3600 ticks at 90 kHz) between
			// old and new source.
			sh.videoOutBase = sh.videoLastOut + 3600
		}
		sh.videoInited = true
	}

	outTS := sh.videoOutBase + (pkt.Timestamp - sh.videoSrcBase)
	sh.videoLastOut = outTS
	pkt.Timestamp = outTS

	// -- Write --
	if err := stream.WritePacketRTP(desc.Medias[0], pkt); err != nil {
		sh.log.Debug("video WritePacketRTP error", "error", err)
	}
}

// writeAudioToStream normalises and writes an audio RTP packet to the output
// server stream (media index 1 = audio). Constant SSRC and monotonic sequence
// numbers are enforced so source switches (silence ↔ camera) are seamless.
func (sh *StreamHandler) writeAudioToStream(pkt *rtp.Packet) {
	sh.audioMu.Lock()
	defer sh.audioMu.Unlock()

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

	pkt.SSRC = sh.audioSSRC
	pkt.PayloadType = 8 // G.711 a-law (must match SDP)
	pkt.SequenceNumber = sh.audioSeq
	sh.audioSeq++

	stream.WritePacketRTP(desc.Medias[1], pkt)
}

// -----------------------------------------------------------------------
// Silent audio (PCMA) -- continuous G.711 a-law silence so go2rtc is happy
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

	const (
		sampleRate       = 8000
		packetDurationMs = 20
		samplesPerPacket = sampleRate * packetDurationMs / 1000
	)

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
				PayloadType:    8,
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
		spsChanged := false
		sh.mu.Lock()
		if sh.desc != nil && len(sh.desc.Medias) > 0 {
			if h264Fmt, ok := sh.desc.Medias[0].Formats[0].(*format.H264); ok {
				spsChanged = !bytes.Equal(h264Fmt.SPS, sps)
				h264Fmt.SPS = sps
				h264Fmt.PPS = pps
			}
		}
		sh.mu.Unlock()

		// Only probe dimensions when SPS actually changes.
		if spsChanged {
			sh.updateDimensionsFromSPS(sps)
		}
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
		if naluType == 19 || naluType == 20 {
			sh.decodeAndStore(au, "h265")
			return
		}
	}
}

func (sh *StreamHandler) decodeAndStore(nalus [][]byte, codec string) {
	now := time.Now().Unix()
	if last := sh.lastCaptureTS.Load(); now-last < 5 {
		return
	}
	sh.lastCaptureTS.Store(now)

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
		return
	}

	fbCtx, cancel := context.WithCancel(ctx)
	sh.fallbackCancel = cancel
	sh.resetVideoTimeline()

	// Start silence audio alongside fallback video.
	sh.startSilenceAudio(ctx)

	sh.wg.Add(1)
	sh.fallbackWg.Add(1)
	go func() {
		defer sh.wg.Done()
		defer sh.fallbackWg.Done()
		defer func() {
			sh.fallbackMu.Lock()
			sh.fallbackCancel = nil
			sh.fallbackMu.Unlock()
		}()
		sh.runFallback(fbCtx)
	}()
}

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

	// ---------------------------------------------------------------
	// Use pre-encoded frames for instant startup (no FFmpeg delay).
	// Falls back to on-the-fly encoding only if pre-encoding failed.
	// ---------------------------------------------------------------
	var frameVariants [][][]byte

	if len(sh.preBaseAU) > 0 {
		// Fast path: pre-encoded during init, zero latency.
		frameVariants = append(frameVariants, sh.preBaseAU)
		if len(sh.preDotAU) > 0 {
			frameVariants = append(frameVariants, sh.preDotAU)
		}
		sh.log.Info("fallback: using pre-encoded frames")
	} else {
		// Slow path: encode on-the-fly (should rarely happen).
		sh.log.Warn("fallback: no pre-encoded frames, encoding on-the-fly")

		pngData, w, h := sh.fallbackGen.GetFramePNG()
		if len(pngData) == 0 {
			sh.log.Error("no fallback frame available")
			return
		}
		if w%2 != 0 {
			w++
		}
		if h%2 != 0 {
			h++
		}

		baseAU, _, _ := sh.encodePNGToAU(pngData, w, h, codec)
		if len(baseAU) == 0 {
			sh.log.Error("fallback: no NAL units from base frame")
			return
		}
		frameVariants = append(frameVariants, baseAU)

		if ctx.Err() != nil {
			return
		}

		dotPng := addIndicatorDot(pngData)
		if dotPng != nil {
			dotAU, _, _ := sh.encodePNGToAU(dotPng, w, h, codec)
			if len(dotAU) > 0 {
				frameVariants = append(frameVariants, dotAU)
			}
		}
	}

	if ctx.Err() != nil {
		return
	}

	// Set up RTP encoder.
	var h264Enc *rtph264.Encoder
	var h265Enc *rtph265.Encoder

	if codec == "h265" {
		h265Enc = &rtph265.Encoder{PayloadType: 96, PayloadMaxSize: 1200}
		if err := h265Enc.Init(); err != nil {
			sh.log.Error("h265 RTP encoder init", "error", err)
			return
		}
	} else {
		h264Enc = &rtph264.Encoder{
			PayloadType:       96,
			PayloadMaxSize:    1200,
			PacketizationMode: 1,
		}
		if err := h264Enc.Init(); err != nil {
			sh.log.Error("h264 RTP encoder init", "error", err)
			return
		}
	}

	clockRate := uint32(90000)
	tsInc := clockRate / uint32(fps)
	var ts uint32

	// Animation: alternate between frame variants every ~1 second.
	variantIdx := 0
	switchEvery := fps
	if switchEvery < 1 {
		switchEvery = 1
	}
	tickCount := 0

	sh.log.Info("fallback stream started",
		"codec", codec, "fps", fps,
		"variants", len(frameVariants))

	ticker := time.NewTicker(time.Second / time.Duration(fps))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			sh.log.Info("fallback stream stopped")
			return
		case <-ticker.C:
		}

		au := frameVariants[variantIdx]

		if codec == "h265" && h265Enc != nil {
			pkts, err := h265Enc.Encode(au)
			if err != nil {
				sh.log.Error("fallback: H265 encode", "error", err)
				continue
			}
			for _, pkt := range pkts {
				pkt.Timestamp = ts
				sh.writeToStream(pkt)
			}
		} else if h264Enc != nil {
			pkts, err := h264Enc.Encode(au)
			if err != nil {
				sh.log.Error("fallback: H264 encode", "error", err)
				continue
			}
			for _, pkt := range pkts {
				pkt.Timestamp = ts
				sh.writeToStream(pkt)
			}
		}

		ts += tsInc
		tickCount++
		if tickCount >= switchEvery {
			tickCount = 0
			variantIdx = (variantIdx + 1) % len(frameVariants)
		}
	}
}

// encodePNGToAU encodes PNG data to H.264/H.265 NAL units via FFmpeg.
// Returns the filtered access unit, plus SPS and PPS (H.264 only).
func (sh *StreamHandler) encodePNGToAU(pngData []byte, w, h int, codec string) (au [][]byte, sps, pps []byte) {
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("fb-enc-%s-*.png", sh.Name))
	if err != nil {
		sh.log.Error("encodePNGToAU: temp file", "error", err)
		return nil, nil, nil
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(pngData); err != nil {
		tmpFile.Close()
		return nil, nil, nil
	}
	tmpFile.Close()

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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		sh.log.Error("encodePNGToAU: FFmpeg failed",
			"error", err, "stderr", stderr.String())
		return nil, nil, nil
	}

	nalus := fallback.ParseAnnexB(output)
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		if codec != "h265" {
			switch nalu[0] & 0x1F {
			case 7:
				sps = nalu
				au = append(au, nalu)
			case 8:
				pps = nalu
				au = append(au, nalu)
			case 5, 1, 6:
				au = append(au, nalu)
			default:
				// Skip AUD, filler, etc.
			}
		} else {
			au = append(au, nalu)
		}
	}
	return au, sps, pps
}

// addIndicatorDot overlays a tiny (6x6 px) semi-transparent dot in the
// bottom-right corner of a PNG image. The dot is far too small to trigger
// Frigate's motion detection but visible to humans as a "stream alive"
// indicator when the fallback alternates between frames with/without it.
func addIndicatorDot(pngData []byte) []byte {
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil
	}

	b := img.Bounds()
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, img, b.Min, draw.Src)

	// 6x6 dot, 14 px from the edges, subtle gray with alpha blending.
	const dotSize = 6
	const padding = 14
	dotColor := image.NewUniform(color.NRGBA{R: 130, G: 130, B: 130, A: 90})
	dotRect := image.Rect(
		b.Max.X-padding-dotSize, b.Max.Y-padding-dotSize,
		b.Max.X-padding, b.Max.Y-padding,
	)
	draw.Draw(rgba, dotRect, dotColor, image.Point{}, draw.Over)

	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		return nil
	}
	return buf.Bytes()
}

// -----------------------------------------------------------------------
// NAL unit helpers
// -----------------------------------------------------------------------

// copyBytes returns a deep copy of a byte slice (nil-safe).
func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// filterH264NALUs applies a strict whitelist of allowed H.264 NALU types.
// Only types that are safe for downstream muxers/decoders are kept:
//
//	1  = non-IDR slice  (VCL)
//	5  = IDR slice       (VCL)
//	6  = SEI
//	7  = SPS
//	8  = PPS
//
// Returns nil (discarding the entire AU) if data partitioning NALUs
// (types 2-4) are found, since no mainstream decoder supports them.
func filterH264NALUs(au [][]byte) [][]byte {
	var filtered [][]byte
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		nt := nalu[0] & 0x1F
		if nt >= 2 && nt <= 4 {
			return nil // Data partitioning: discard entire AU
		}
		// Whitelist: only keep safe, well-understood types.
		switch nt {
		case 1, 5, 6, 7, 8:
			filtered = append(filtered, nalu)
		default:
			// Discard AUD (9), end-of-seq (10), end-of-stream (11),
			// filler (12), SPS-ext (13), and any other non-standard
			// types that can confuse downstream parsers.
		}
	}
	return filtered
}

// hasVCL returns true if the AU contains at least one VCL NALU
// (type 1 = non-IDR slice, type 5 = IDR slice). AUs without VCL
// data cause FFmpeg to emit "missing picture in access unit" and
// cascade into false "data partitioning" errors.
func hasVCL(au [][]byte) bool {
	for _, nalu := range au {
		if len(nalu) > 0 {
			nt := nalu[0] & 0x1F
			if nt == 1 || nt == 5 {
				return true
			}
		}
	}
	return false
}

// hasH265VCL returns true if the AU contains at least one H.265 VCL NALU
// (types 0-31). AUs with only parameter sets (VPS/SPS/PPS) or SEI would
// confuse downstream muxers/decoders.
func hasH265VCL(au [][]byte) bool {
	for _, nalu := range au {
		if len(nalu) >= 2 {
			nt := (nalu[0] >> 1) & 0x3F
			if nt <= 31 {
				return true
			}
		}
	}
	return false
}

// filterH265NALUs removes empty NALUs and filters invalid types.
func filterH265NALUs(au [][]byte) [][]byte {
	var filtered [][]byte
	for _, nalu := range au {
		if len(nalu) < 2 {
			continue
		}
		nt := (nalu[0] >> 1) & 0x3F
		if nt > 47 {
			continue // Reserved / unspecified
		}
		filtered = append(filtered, nalu)
	}
	return filtered
}

// ensureSPSPPS guarantees that every IDR access unit includes SPS and PPS
// NALUs. Many cameras only send SPS/PPS in the SDP, not in-band. Without
// in-band parameter sets, downstream muxers (Frigate recording) fail with
// "Could not write header (incorrect codec parameters)" because they need
// SPS/PPS to write the MP4/segment header.
func ensureSPSPPS(au [][]byte, sps, pps []byte) [][]byte {
	if sps == nil || pps == nil {
		return au
	}

	hasIDR := false
	for _, nalu := range au {
		if len(nalu) > 0 && (nalu[0]&0x1F) == 5 {
			hasIDR = true
			break
		}
	}
	if !hasIDR {
		return au
	}

	// Rebuild AU: SPS, PPS, then all non-parameter-set NALUs.
	result := make([][]byte, 0, len(au)+2)
	result = append(result, sps, pps)
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		nt := nalu[0] & 0x1F
		if nt == 7 || nt == 8 {
			continue // Skip original SPS/PPS; we prepended ours
		}
		result = append(result, nalu)
	}
	return result
}

// ensureVPSSPSPPS is the H.265 equivalent: ensures VPS, SPS, and PPS are
// present before IRAP frames.
func ensureVPSSPSPPS(au [][]byte, vps, sps, pps []byte) [][]byte {
	if vps == nil || sps == nil || pps == nil {
		return au
	}

	hasIRAP := false
	for _, nalu := range au {
		if len(nalu) >= 2 {
			nt := (nalu[0] >> 1) & 0x3F
			if nt >= 16 && nt <= 21 {
				hasIRAP = true
				break
			}
		}
	}
	if !hasIRAP {
		return au
	}

	result := make([][]byte, 0, len(au)+3)
	result = append(result, vps, sps, pps)
	for _, nalu := range au {
		if len(nalu) < 2 {
			continue
		}
		nt := (nalu[0] >> 1) & 0x3F
		if nt == 32 || nt == 33 || nt == 34 {
			continue // Skip original VPS/SPS/PPS
		}
		result = append(result, nalu)
	}
	return result
}
