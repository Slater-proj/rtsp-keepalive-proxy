package fallback

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"os/exec"
	"sync"
)

// Generator produces still-image video NAL units for a single camera stream.
// It caches the last captured keyframe and re-encodes it via FFmpeg on demand.
type Generator struct {
	mu sync.RWMutex

	// Source frame (PNG-encoded).
	lastFrame []byte
	width     int
	height    int

	// Cached encoded NAL units (invalidated when frame changes).
	cachedCodec string
	cachedNALUs [][]byte
}

// NewGenerator creates a new fallback generator.
func NewGenerator() *Generator {
	return &Generator{
		width:  1920,
		height: 1080,
	}
}

// UpdateFrame stores a new reference frame (PNG bytes).
// Previously cached NAL units are invalidated.
func (g *Generator) UpdateFrame(pngData []byte, width, height int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastFrame = make([]byte, len(pngData))
	copy(g.lastFrame, pngData)
	g.width = width
	g.height = height
	g.cachedNALUs = nil // invalidate cache
	g.cachedCodec = ""
}

// HasFrame reports whether at least one keyframe has been captured.
func (g *Generator) HasFrame() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.lastFrame) > 0
}

// Dimensions returns the current frame width and height.
func (g *Generator) Dimensions() (int, int) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.width, g.height
}

// GenerateNALUs returns H.264 or H.265 NAL units for the current frame.
// codec must be "h264" or "h265". Results are cached until the frame changes.
func (g *Generator) GenerateNALUs(codec string) ([][]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Return cache if still valid.
	if g.cachedCodec == codec && len(g.cachedNALUs) > 0 {
		return g.cachedNALUs, nil
	}

	frameData := g.lastFrame
	w, h := g.width, g.height
	if len(frameData) == 0 {
		frameData = g.placeholder(w, h)
	}

	nalus, err := encode(frameData, w, h, codec)
	if err != nil {
		return nil, err
	}

	g.cachedNALUs = nalus
	g.cachedCodec = codec
	return nalus, nil
}

// placeholder generates a dark-grey PNG image.
func (g *Generator) placeholder(w, h int) []byte {
	if w <= 0 {
		w = 1920
	}
	if h <= 0 {
		h = 1080
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{20, 20, 20, 255}}, image.Point{}, draw.Src)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		slog.Error("fallback: placeholder encode failed", "error", err)
		return nil
	}
	return buf.Bytes()
}

// encode encodes a PNG frame into NAL units using FFmpeg.
func encode(pngData []byte, width, height int, codec string) ([][]byte, error) {
	if len(pngData) == 0 {
		return nil, fmt.Errorf("no frame data")
	}

	encoder := "libx264"
	outFmt := "h264"
	var extraArgs []string

	switch codec {
	case "h265":
		encoder = "libx265"
		outFmt = "hevc"
		extraArgs = []string{
			"-preset", "ultrafast",
			"-x265-params", "keyint=1:min-keyint=1:scenecut=0:bframes=0:repeat-headers=1:log-level=error",
		}
	default: // h264
		extraArgs = []string{
			"-preset", "ultrafast",
			"-tune", "stillimage",
			"-profile:v", "baseline",
			"-level", "3.1",
			"-x264-params", "keyint=1:min-keyint=1:scenecut=0:bframes=0:repeat-headers=1",
		}
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "image2pipe",
		"-i", "pipe:0",
		"-frames:v", "1",
		"-c:v", encoder,
	}
	args = append(args, extraArgs...)
	args = append(args,
		"-s", fmt.Sprintf("%dx%d", width, height),
		"-pix_fmt", "yuv420p",
		"-f", outFmt,
		"pipe:1",
	)

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdin = bytes.NewReader(pngData)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg %s encode: %w [%s]", codec, err, stderr.String())
	}

	nalus := ParseAnnexB(stdout.Bytes())
	if len(nalus) == 0 {
		return nil, fmt.Errorf("ffmpeg produced no NAL units")
	}
	return nalus, nil
}
