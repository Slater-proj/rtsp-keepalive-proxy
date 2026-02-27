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
	"strings"
	"sync"
)

// Generator produces still-image video NAL units for a single camera stream.
// Depending on mode it generates either an OFFLINE overlay or replays the
// last captured keyframe.
type Generator struct {
	mu sync.RWMutex

	cameraName string
	mode       string // "offline", "last_frame"

	// Source frame (PNG-encoded, used in last_frame mode).
	lastFrame []byte

	// Source frame dimensions (captured from last real keyframe).
	width  int
	height int

	// Cached offline PNG (regenerated when dimensions change).
	cachedOfflinePNG []byte
	cachedOfflineW   int
	cachedOfflineH   int
}

// NewGenerator creates a new fallback generator for the named camera.
// mode should be "offline" or "last_frame".
func NewGenerator(cameraName string, mode string) *Generator {
	if mode == "" {
		mode = "offline"
	}
	return &Generator{
		cameraName: cameraName,
		mode:       mode,
		width:      1920,
		height:     1080,
	}
}

// UpdateFrame stores the latest keyframe PNG and its dimensions.
// In last_frame mode, the PNG is kept for replay.
// In offline mode, only the dimensions are recorded.
// Previously cached NAL units are invalidated.
func (g *Generator) UpdateFrame(pngData []byte, width, height int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode == "last_frame" && len(pngData) > 0 {
		g.lastFrame = make([]byte, len(pngData))
		copy(g.lastFrame, pngData)
	}
	if width > 0 {
		g.width = width
	}
	if height > 0 {
		g.height = height
	}
	// Invalidate offline PNG cache (dimensions may have changed).
	g.cachedOfflinePNG = nil
	g.cachedOfflineW = 0
	g.cachedOfflineH = 0
}

// HasFrame reports whether dimensions have been captured from a real frame.
func (g *Generator) HasFrame() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.width > 0 && g.height > 0
}

// Dimensions returns the current frame width and height.
func (g *Generator) Dimensions() (int, int) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.width, g.height
}

// GetFramePNG returns the PNG image to use as a fallback frame along with its
// dimensions. In "last_frame" mode the last captured keyframe is returned;
// in "offline" mode a styled overlay image is generated.
func (g *Generator) GetFramePNG() (pngData []byte, width, height int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	w, h := g.width, g.height

	if g.mode == "last_frame" && len(g.lastFrame) > 0 {
		return g.lastFrame, w, h
	}

	// Generate offline overlay frame.
	data := g.offlinePNG(w, h)
	if data == nil {
		// Fallback to plain dark placeholder (no fonts available).
		data = g.placeholder(w, h)
	}
	return data, w, h
}

// offlinePNG generates a dark frame with "CAMERA OFFLINE" text via FFmpeg.
// Returns nil if FFmpeg drawtext is not available (no fonts installed).
func (g *Generator) offlinePNG(w, h int) []byte {
	if w <= 0 {
		w = 1920
	}
	if h <= 0 {
		h = 1080
	}

	// Return cached offline PNG if dimensions match.
	if g.cachedOfflinePNG != nil && g.cachedOfflineW == w && g.cachedOfflineH == h {
		return g.cachedOfflinePNG
	}

	name := strings.ToUpper(g.cameraName)
	if name == "" {
		name = "CAMERA"
	}

	titleSize := h / 12
	subSize := h / 22
	gap := h / 15

	// FFmpeg lavfi: dark background + 2 lines of centered text.
	vf := fmt.Sprintf(
		"drawtext=text='%s - OFFLINE':fontsize=%d:fontcolor=#AAAAAA:x=(w-tw)/2:y=(h/2)-%d,"+
			"drawtext=text='No motion detected':fontsize=%d:fontcolor=#555555:x=(w-tw)/2:y=(h/2)+%d",
		name, titleSize, gap,
		subSize, gap/2,
	)

	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi",
		"-i", fmt.Sprintf("color=c=#141414:s=%dx%d:d=1", w, h),
		"-vf", vf,
		"-frames:v", "1",
		"-f", "image2pipe", "-vcodec", "png",
		"pipe:1",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		slog.Debug("offline frame generation failed (no fonts?), using plain placeholder",
			"error", err, "stderr", stderr.String())
		return nil
	}

	g.cachedOfflinePNG = stdout.Bytes()
	g.cachedOfflineW = w
	g.cachedOfflineH = h
	return g.cachedOfflinePNG
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
