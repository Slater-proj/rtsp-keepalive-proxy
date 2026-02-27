package fallback

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"
)

func TestNewGenerator(t *testing.T) {
	g := NewGenerator("test_cam", "offline")
	if !g.HasFrame() {
		t.Error("new generator should have default dimensions")
	}
	w, h := g.Dimensions()
	if w != 1920 || h != 1080 {
		t.Errorf("default dimensions = %dx%d, want 1920x1080", w, h)
	}
}

func TestUpdateFrame(t *testing.T) {
	g := NewGenerator("test_cam", "last_frame")

	pngData := createTestPNG(t, 640, 480)
	g.UpdateFrame(pngData, 640, 480)

	if !g.HasFrame() {
		t.Error("HasFrame should be true after UpdateFrame")
	}
	w, h := g.Dimensions()
	if w != 640 || h != 480 {
		t.Errorf("dimensions = %dx%d, want 640x480", w, h)
	}
}

func TestUpdateFrameInvalidatesCache(t *testing.T) {
	g := NewGenerator("test_cam", "offline")

	png1 := createTestPNG(t, 320, 240)
	g.UpdateFrame(png1, 320, 240)
	// Manually set offline PNG cache.
	g.mu.Lock()
	g.cachedOfflinePNG = []byte{0x89, 0x50, 0x4E, 0x47}
	g.cachedOfflineW = 320
	g.cachedOfflineH = 240
	g.mu.Unlock()

	png2 := createTestPNG(t, 320, 240)
	g.UpdateFrame(png2, 320, 240)

	g.mu.RLock()
	if g.cachedOfflinePNG != nil {
		t.Error("cachedOfflinePNG should be nil after UpdateFrame")
	}
	g.mu.RUnlock()
}

func TestUpdateFrameIsolation(t *testing.T) {
	g := NewGenerator("test_cam", "offline")

	g.UpdateFrame(nil, 640, 480)

	w, h := g.Dimensions()
	if w != 640 || h != 480 {
		t.Errorf("dimensions = %dx%d, want 640x480", w, h)
	}

	// Updating again should change dimensions.
	g.UpdateFrame(nil, 1280, 720)
	w, h = g.Dimensions()
	if w != 1280 || h != 720 {
		t.Errorf("dimensions = %dx%d, want 1280x720", w, h)
	}
}

func TestPlaceholder(t *testing.T) {
	g := NewGenerator("test_cam", "offline")
	data := g.placeholder(64, 64)
	if data == nil {
		t.Fatal("placeholder returned nil")
	}

	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("placeholder is not valid PNG: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != 64 || bounds.Dy() != 64 {
		t.Errorf("placeholder size = %dx%d, want 64x64", bounds.Dx(), bounds.Dy())
	}
}

func TestPlaceholderDefaultSize(t *testing.T) {
	g := NewGenerator("test_cam", "offline")
	data := g.placeholder(0, 0)
	if data == nil {
		t.Fatal("placeholder returned nil")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("placeholder is not valid PNG: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != 1920 || bounds.Dy() != 1080 {
		t.Errorf("placeholder default size = %dx%d, want 1920x1080", bounds.Dx(), bounds.Dy())
	}
}

func TestGetFramePNGOffline(t *testing.T) {
	g := NewGenerator("test_cam", "offline")
	pngData, w, h := g.GetFramePNG()
	if len(pngData) == 0 {
		t.Fatal("GetFramePNG returned empty data")
	}
	if w != 1920 || h != 1080 {
		t.Errorf("dimensions = %dx%d, want 1920x1080", w, h)
	}
	// Verify it's a valid PNG.
	_, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		t.Fatalf("GetFramePNG is not valid PNG: %v", err)
	}
}

func TestGetFramePNGLastFrame(t *testing.T) {
	g := NewGenerator("test_cam", "last_frame")

	// Before any frame, should return a placeholder.
	pngData, _, _ := g.GetFramePNG()
	if len(pngData) == 0 {
		t.Fatal("GetFramePNG returned empty data before first frame")
	}

	// After updating with a real frame, should return that frame.
	realPNG := createTestPNG(t, 640, 480)
	g.UpdateFrame(realPNG, 640, 480)
	pngData, w, h := g.GetFramePNG()
	if w != 640 || h != 480 {
		t.Errorf("dimensions = %dx%d, want 640x480", w, h)
	}
	if !bytes.Equal(pngData, realPNG) {
		t.Error("GetFramePNG should return the last captured frame in last_frame mode")
	}
}

// createTestPNG creates a minimal PNG image for testing.
func createTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{128, 128, 128, 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("createTestPNG: %v", err)
	}
	return buf.Bytes()
}
