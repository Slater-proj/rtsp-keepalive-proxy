package proxy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"rtsp-keepalive-proxy/internal/config"
)

func newNeutralHandler(t *testing.T, mode, kind string, low bool) *StreamHandler {
	t.Helper()
	sh := NewStreamHandler("low_cam", config.ResolvedCamera{
		Source:          "rtsp://127.0.0.1:1/x",
		Codec:           "h264",
		FallbackMode:    mode,
		FallbackNeutral: kind,
		FallbackHold:    time.Second,
		FallbackFPS:     5,
		Timeout:         time.Second,
		RetryInterval:   time.Second,
		StallTimeout:    time.Second,
		Width:           640,
		Height:          480,
		Low:             low,
	})
	sh.log = discardLogger()
	return sh
}

func TestNeutralOnlyOnLowStreamsInNeutralMode(t *testing.T) {
	cases := []struct {
		mode string
		low  bool
		want bool
	}{
		{"neutral", true, true},
		{"neutral", false, false}, // the recorded main stream keeps replaying the camera frame
		{"offline", true, false},
		{"last_frame", true, false},
		{"none", true, false},
	}
	for _, c := range cases {
		sh := newNeutralHandler(t, c.mode, "offline", c.low)
		if got := sh.neutralEnabled(); got != c.want {
			t.Errorf("mode=%s low=%v: neutralEnabled = %v, want %v", c.mode, c.low, got, c.want)
		}
	}
}

func TestEnsureNeutralEncodesOnceAndCaches(t *testing.T) {
	sh := newNeutralHandler(t, "neutral", "grey", true)
	var calls atomic.Int32
	sh.neutralEncoder = func(img []byte, w, h int, codec, profile, level string) ([][]byte, error) {
		calls.Add(1)
		if len(img) == 0 {
			t.Error("encoder received an empty image")
		}
		if w != 640 || h != 480 {
			t.Errorf("neutral encoded at %dx%d, want the camera's 640x480", w, h)
		}
		return [][]byte{{0x67, 1}, {0x68, 1}, {0x65, 0xEE}}, nil
	}

	for i := 0; i < 3; i++ {
		au, err := sh.ensureNeutral()
		if err != nil || len(au) != 3 {
			t.Fatalf("ensureNeutral: au=%d err=%v", len(au), err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("encoder called %d times, want 1 (cached)", calls.Load())
	}

	// a new camera resolution invalidates the cache
	sh.fallbackGen.UpdateFrame(nil, 704, 576)
	sh.neutralEncoder = func(img []byte, w, h int, codec, profile, level string) ([][]byte, error) {
		calls.Add(1)
		if w != 704 || h != 576 {
			t.Errorf("after the size change: %dx%d, want 704x576", w, h)
		}
		return [][]byte{{0x65, 0xEE}}, nil
	}
	if _, err := sh.ensureNeutral(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("encoder calls = %d, want 2 after a size change", calls.Load())
	}
}

func TestNeutralFollowsCameraH264Profile(t *testing.T) {
	sh := newNeutralHandler(t, "neutral", "black", true)
	sh.noteSPS("h264", sps264CIF) // High profile, level 1.2, 352x288

	var gotProfile, gotLevel string
	sh.neutralEncoder = func(img []byte, w, h int, codec, profile, level string) ([][]byte, error) {
		gotProfile, gotLevel = profile, level
		return [][]byte{{0x65, 0xEE}}, nil
	}
	if _, err := sh.ensureNeutral(); err != nil {
		t.Fatal(err)
	}
	if gotProfile != "high" || gotLevel != "1.2" {
		t.Errorf("profile/level = %q/%q, want high/1.2 (the camera's)", gotProfile, gotLevel)
	}
}

func TestNeutralImageKinds(t *testing.T) {
	sh := newNeutralHandler(t, "neutral", "black", true)

	for _, kind := range []string{"black", "grey"} {
		img, err := sh.neutralImage(kind, 64, 48)
		if err != nil || len(img) == 0 {
			t.Fatalf("%s: err=%v len=%d", kind, err, len(img))
		}
		if !bytes.HasPrefix(img, []byte("\x89PNG")) {
			t.Errorf("%s: not a PNG", kind)
		}
	}
	black, _ := sh.neutralImage("black", 64, 48)
	grey, _ := sh.neutralImage("grey", 64, 48)
	if bytes.Equal(black, grey) {
		t.Error("black and grey must differ")
	}

	// user file
	path := filepath.Join(t.TempDir(), "neutral.jpg")
	want := []byte("fake image bytes")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sh.neutralImage("file:"+path, 64, 48)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("file kind: err=%v", err)
	}
	if _, err := sh.neutralImage("file:"+filepath.Join(t.TempDir(), "missing.jpg"), 64, 48); err == nil {
		t.Error("a missing file must be an error")
	}
	if _, err := sh.neutralImage("sepia", 64, 48); err == nil {
		t.Error("an unknown kind must be an error")
	}
}

func TestEnsureNeutralReportsEncoderFailure(t *testing.T) {
	sh := newNeutralHandler(t, "neutral", "grey", true)
	sh.neutralEncoder = func([]byte, int, int, string, string, string) ([][]byte, error) {
		return nil, errors.New("ffmpeg missing")
	}
	if _, err := sh.ensureNeutral(); err == nil {
		t.Fatal("encoder failure must surface")
	}
	sh.neutralEncoder = func([]byte, int, int, string, string, string) ([][]byte, error) {
		return nil, nil
	}
	if _, err := sh.ensureNeutral(); err == nil {
		t.Fatal("an empty access unit must be an error")
	}
}

func TestH264ProfileAndLevelMapping(t *testing.T) {
	for idc, want := range map[uint8]string{66: "baseline", 77: "main", 100: "high", 99: ""} {
		if got := h264Profile(idc); got != want {
			t.Errorf("h264Profile(%d) = %q, want %q", idc, got, want)
		}
	}
	for idc, want := range map[uint8]string{31: "3.1", 40: "4.0", 12: "1.2", 9: ""} {
		if got := h264Level(idc); got != want {
			t.Errorf("h264Level(%d) = %q, want %q", idc, got, want)
		}
	}
}

func TestFallbackStateReporting(t *testing.T) {
	sh := newNeutralHandler(t, "neutral", "offline", true)
	if sh.FallbackKind() != "none" || !sh.FallbackSince().IsZero() {
		t.Fatalf("idle state: kind=%s since=%v", sh.FallbackKind(), sh.FallbackSince())
	}
	sh.markFallbackStart()
	if sh.FallbackKind() != "last_keyframe" || sh.FallbackSince().IsZero() {
		t.Errorf("after start: kind=%s since=%v", sh.FallbackKind(), sh.FallbackSince())
	}
	sh.setFallbackKind("neutral")
	if sh.FallbackKind() != "neutral" {
		t.Errorf("kind = %s, want neutral", sh.FallbackKind())
	}
	sh.clearFallbackState()
	if sh.FallbackKind() != "none" || !sh.FallbackSince().IsZero() {
		t.Errorf("after clear: kind=%s since=%v", sh.FallbackKind(), sh.FallbackSince())
	}
}
