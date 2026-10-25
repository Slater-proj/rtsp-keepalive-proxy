package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rtsp-keepalive-proxy/internal/config"
)

func newPersistHandler(t *testing.T, dir, codec string) *StreamHandler {
	t.Helper()
	cfg := config.ResolvedCamera{
		Source: "rtsp://127.0.0.1:1/x", Codec: codec, FallbackMode: "none",
		FallbackFPS: 5, Timeout: time.Second, RetryInterval: time.Second,
		StallTimeout: time.Second,
	}
	sh := NewStreamHandler("cam", cfg)
	sh.log = discardLogger()
	sh.EnablePersistence(dir)
	return sh
}

func TestKeyframeSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	idr := append([]byte{0x65}, bytes.Repeat([]byte{7}, 200)...)
	au := [][]byte{sps264CIF, pps, idr}

	a := newPersistHandler(t, dir, "auto")
	a.storeLastCameraKeyframe(au, "h264")

	// the write is asynchronous
	file := filepath.Join(dir, "cam.h264")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(file); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("keyframe file was never written")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// "restart": a brand new handler on the same directory
	b := newPersistHandler(t, dir, "auto")
	b.lastCameraVideoAUMu.RLock()
	got := b.lastCameraVideoAU
	b.lastCameraVideoAUMu.RUnlock()
	if len(got) != 3 || !bytes.Equal(got[2], idr) {
		t.Fatalf("restored AU = %d NALUs, want the stored [sps pps idr]", len(got))
	}
	if !bytes.Equal(b.initialSPS, sps264CIF) || !bytes.Equal(b.initialPPS, pps) {
		t.Error("the advertised SPS/PPS must be the camera's, not an encoder's")
	}
	if b.GetCodec() != "h264" {
		t.Errorf("codec = %s, want h264", b.GetCodec())
	}
	if w, h := b.fallbackGen.Dimensions(); w != 352 || h != 288 {
		t.Errorf("fallback size = %dx%d, want the camera's 352x288", w, h)
	}
}

func TestPersistedKeyframeIgnoredOnCodecMismatch(t *testing.T) {
	dir := t.TempDir()
	a := newPersistHandler(t, dir, "auto")
	a.storeLastCameraKeyframe([][]byte{sps264CIF, {0x68, 1}, {0x65, 1, 2}}, "h264")
	time.Sleep(300 * time.Millisecond)

	// config now pins h265: an H.264 file must not leak into the SDP
	b := newPersistHandler(t, dir, "h265")
	if b.GetCodec() != "h265" {
		t.Errorf("codec = %s, want h265", b.GetCodec())
	}
	b.lastCameraVideoAUMu.RLock()
	n := len(b.lastCameraVideoAU)
	b.lastCameraVideoAUMu.RUnlock()
	if n != 0 {
		t.Error("an H.264 keyframe was restored into an H.265 stream")
	}
}

func TestPersistenceOffByDefault(t *testing.T) {
	sh := newPersistHandler(t, "", "auto")
	sh.storeLastCameraKeyframe([][]byte{sps264CIF, {0x68, 1}, {0x65, 1, 2}}, "h264")
	if sh.persist != nil {
		t.Error("persistence must stay off without data_dir")
	}
}

func TestCorruptKeyframeFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cam.h264"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh := newPersistHandler(t, dir, "auto")
	sh.lastCameraVideoAUMu.RLock()
	n := len(sh.lastCameraVideoAU)
	sh.lastCameraVideoAUMu.RUnlock()
	if n != 0 {
		t.Error("garbage file must not be restored")
	}
}
