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
	g := NewGenerator()
	if g.HasFrame() {
		t.Error("new generator should not have a frame")
	}
	w, h := g.Dimensions()
	if w != 1920 || h != 1080 {
		t.Errorf("default dimensions = %dx%d, want 1920x1080", w, h)
	}
}

func TestUpdateFrame(t *testing.T) {
	g := NewGenerator()

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
	g := NewGenerator()

	png1 := createTestPNG(t, 320, 240)
	g.UpdateFrame(png1, 320, 240)
	// Manually set cache
	g.mu.Lock()
	g.cachedCodec = "h264"
	g.cachedNALUs = [][]byte{{0x67}}
	g.mu.Unlock()

	png2 := createTestPNG(t, 320, 240)
	g.UpdateFrame(png2, 320, 240)

	g.mu.RLock()
	if g.cachedCodec != "" {
		t.Error("cache should be invalidated after UpdateFrame")
	}
	if g.cachedNALUs != nil {
		t.Error("cachedNALUs should be nil after UpdateFrame")
	}
	g.mu.RUnlock()
}

func TestUpdateFrameIsolation(t *testing.T) {
	g := NewGenerator()

	original := []byte{1, 2, 3, 4, 5}
	g.UpdateFrame(original, 100, 100)

	// Modify the original — should not affect stored data.
	original[0] = 99

	g.mu.RLock()
	if g.lastFrame[0] == 99 {
		t.Error("stored frame aliases the input slice")
	}
	g.mu.RUnlock()
}

func TestPlaceholder(t *testing.T) {
	g := NewGenerator()
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
	g := NewGenerator()
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
