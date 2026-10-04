package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"rtsp-keepalive-proxy/internal/config"
	"rtsp-keepalive-proxy/internal/fallback"
)

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"rtsp://admin:secret@192.168.1.50:554/cam/realmonitor?channel=1&subtype=0",
			"rtsp://admin:***@192.168.1.50:554/cam/realmonitor?channel=1&subtype=0"},
		// raw '@' and '#' in the password make the URL unparseable by net/url
		{"rtsp://admin:p@ss#word@10.0.0.1/s", "rtsp://admin:***@10.0.0.1/s"},
		{"rtsp://token@10.0.0.1/s", "rtsp://***@10.0.0.1/s"},
		{"rtsp://10.0.0.1:554/s", "rtsp://10.0.0.1:554/s"},
		{"rtsp://10.0.0.1:554/s@x", "rtsp://10.0.0.1:554/s@x"},
		{"", ""},
	}
	for _, c := range cases {
		if got := RedactURL(c.in); got != c.want {
			t.Errorf("RedactURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactErrHidesSourcePassword(t *testing.T) {
	sh := &StreamHandler{cfg: config.ResolvedCamera{Source: "rtsp://admin:hunter2@10.0.0.1/s"}}
	err := errors.New(`dial rtsp://admin:hunter2@10.0.0.1/s: connection refused`)
	got := sh.redactErr(err)
	if strings.Contains(got, "hunter2") {
		t.Errorf("password leaked: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("error text lost: %q", got)
	}
}

func TestUlawToAlaw(t *testing.T) {
	// mu-law silence (0xFF) must map to A-law silence (0xD5).
	if got := ulawToAlaw([]byte{0xFF}); got[0] != 0xD5 {
		t.Errorf("ulaw silence -> %#x, want 0xD5", got[0])
	}
	// The sign must be preserved for every input: A-law has the sign bit set
	// for positive samples (after the 0x55 toggle).
	for i := 0; i < 256; i++ {
		lin := ulawToLinear(byte(i))
		a := linearToAlaw(lin)
		positive := a&0x80 != 0
		if lin != 0 && positive != (lin > 0) {
			t.Errorf("ulaw %#02x (linear %d) -> alaw %#02x: sign flipped", i, lin, a)
		}
	}
	in := []byte{0x00, 0x7F, 0x80, 0xFF}
	out := ulawToAlaw(in)
	if len(out) != len(in) {
		t.Fatalf("length changed: %d -> %d", len(in), len(out))
	}
	if &out[0] == &in[0] {
		t.Error("ulawToAlaw must not convert in place")
	}
}

func TestLowDimensions(t *testing.T) {
	ip := func(v int) *int { return &v }
	cases := []struct {
		name   string
		mw, mh int
		wl, hl *int
		ww, wh int
	}{
		{"nothing known", 0, 0, nil, nil, 640, 480},
		{"16:9 main keeps ratio", 1920, 1080, nil, nil, 640, 360},
		{"4:3 main", 1600, 1200, nil, nil, 640, 480},
		{"explicit low wins", 1920, 1080, ip(704), ip(576), 704, 576},
		{"width only keeps ratio", 2560, 1440, ip(800), nil, 800, 450},
		{"height only", 1920, 1080, nil, ip(300), 640, 300},
	}
	for _, c := range cases {
		w, h := lowDimensions(c.mw, c.mh, c.wl, c.hl)
		if w != c.ww || h != c.wh {
			t.Errorf("%s: got %dx%d, want %dx%d", c.name, w, h, c.ww, c.wh)
		}
	}
}

// Reference SPS taken from the mediacommon test-suite.
var (
	sps264CIF = []byte{
		0x67, 0x64, 0x00, 0x0c, 0xac, 0x3b, 0x50, 0xb0,
		0x4b, 0x42, 0x00, 0x00, 0x03, 0x00, 0x02, 0x00,
		0x00, 0x03, 0x00, 0x3d, 0x08,
	}
	sps2651080 = []byte{
		0x42, 0x01, 0x01, 0x01, 0x60, 0x00, 0x00, 0x03,
		0x00, 0x90, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03,
		0x00, 0x78, 0xa0, 0x03, 0xc0, 0x80, 0x10, 0xe5,
		0x96, 0x66, 0x69, 0x24, 0xca, 0xe0, 0x10, 0x00,
		0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x03, 0x01,
		0xe0, 0x80,
	}
)

func TestNoteSPSUpdatesFallbackDimensions(t *testing.T) {
	for _, c := range []struct {
		codec string
		sps   []byte
		w, h  int
	}{
		{"h264", sps264CIF, 352, 288},
		{"h265", sps2651080, 1920, 1080},
	} {
		sh := &StreamHandler{
			Name:        "t",
			fallbackGen: fallback.NewGenerator("t", "offline"),
			log:         discardLogger(),
		}
		sh.noteSPS(c.codec, c.sps)
		w, h := sh.fallbackGen.Dimensions()
		if w != c.w || h != c.h {
			t.Errorf("%s: dimensions %dx%d, want %dx%d", c.codec, w, h, c.w, c.h)
		}
	}
}

func TestNoteSPSIgnoresGarbage(t *testing.T) {
	sh := &StreamHandler{
		fallbackGen: fallback.NewGenerator("t", "offline"),
		log:         discardLogger(),
	}
	sh.noteSPS("h264", []byte{0x67, 0x00})             // too short
	sh.noteSPS("h264", []byte{0x67, 0xff, 0xff, 0xff}) // not a valid SPS
	sh.noteSPS("h265", []byte{0x42, 0x01, 0xff, 0xff, 0xff})
	if w, h := sh.fallbackGen.Dimensions(); w != 1920 || h != 1080 {
		t.Errorf("garbage SPS changed dimensions to %dx%d", w, h)
	}
}

func TestFilterH264NALUs(t *testing.T) {
	sps, pps, idr, slice := []byte{0x67, 1}, []byte{0x68, 1}, []byte{0x65, 1}, []byte{0x41, 1}
	aud, sei := []byte{0x09, 0xf0}, []byte{0x06, 1}

	got := filterH264NALUs([][]byte{aud, sps, pps, idr})
	if len(got) != 3 || !bytes.Equal(got[0], sps) {
		t.Errorf("AUD should be dropped, got %v", got)
	}
	if got := filterH264NALUs([][]byte{sei, slice}); len(got) != 2 {
		t.Errorf("SEI and slice must be kept, got %v", got)
	}
	// data partitioning (types 2-4) discards the whole AU
	if got := filterH264NALUs([][]byte{slice, {0x42, 1}}); got != nil {
		t.Errorf("data partitioning must drop the AU, got %v", got)
	}
	if hasVCL([][]byte{sps, pps}) {
		t.Error("parameter sets alone are not VCL")
	}
	if !hasVCL([][]byte{sps, idr}) {
		t.Error("IDR is VCL")
	}
}

func TestEnsureSPSPPS(t *testing.T) {
	sps, pps := []byte{0x67, 1}, []byte{0x68, 1}
	idr, p := []byte{0x65, 9}, []byte{0x41, 9}
	oldSPS := []byte{0x67, 7}

	got := ensureSPSPPS([][]byte{oldSPS, idr}, sps, pps)
	if len(got) != 3 || !bytes.Equal(got[0], sps) || !bytes.Equal(got[1], pps) || !bytes.Equal(got[2], idr) {
		t.Errorf("IDR AU must be [sps pps idr] with the camera parameter sets, got %v", got)
	}
	if got := ensureSPSPPS([][]byte{p}, sps, pps); len(got) != 1 {
		t.Errorf("non-IDR AU must be untouched, got %v", got)
	}
	if got := ensureSPSPPS([][]byte{idr}, nil, pps); len(got) != 1 {
		t.Errorf("without parameter sets the AU must be untouched, got %v", got)
	}
}

func TestFilterAndEnsureH265(t *testing.T) {
	// H.265 header: type = (b0 >> 1) & 0x3f
	vps, sps, pps := []byte{32 << 1, 1}, []byte{33 << 1, 1}, []byte{34 << 1, 1}
	idr, trail := []byte{19 << 1, 1, 9}, []byte{1 << 1, 1, 9}
	aud := []byte{35 << 1, 1}

	got := filterH265NALUs([][]byte{aud, vps, sps, pps, idr})
	if len(got) != 4 {
		t.Errorf("AUD must be dropped, got %d NALUs", len(got))
	}
	if hasH265VCL([][]byte{vps, sps, pps}) {
		t.Error("parameter sets alone are not VCL")
	}
	if !hasH265VCL([][]byte{trail}) {
		t.Error("TRAIL_R is VCL")
	}

	newVPS := []byte{32 << 1, 2}
	out := ensureVPSSPSPPS([][]byte{vps, idr}, newVPS, sps, pps)
	if len(out) != 4 || !bytes.Equal(out[0], newVPS) || !bytes.Equal(out[3], idr) {
		t.Errorf("IRAP AU must start with the camera VPS/SPS/PPS, got %v", out)
	}
	if out := ensureVPSSPSPPS([][]byte{trail}, vps, sps, pps); len(out) != 1 {
		t.Errorf("non-IRAP AU must be untouched, got %v", out)
	}
}

func TestCapFallbackFPS(t *testing.T) {
	cases := []struct{ fps, bytes, want int }{
		{15, 100_000, 2},   // 1440p HEVC keyframe: 200 KB/s budget allows 2 fps
		{15, 15_000, 13},   // 640x480 keyframe
		{5, 1_000, 5},      // tiny generated frame: untouched
		{15, 1_000_000, 2}, // huge frame: never below the 2 fps floor
		{5, 0, 5},          // unknown size: untouched
	}
	for _, c := range cases {
		if got := capFallbackFPS(c.fps, c.bytes); got != c.want {
			t.Errorf("capFallbackFPS(%d, %d) = %d, want %d", c.fps, c.bytes, got, c.want)
		}
	}
}

func TestIsUnreachable(t *testing.T) {
	dial := fmt.Errorf("connect: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")})
	if !isUnreachable(dial) {
		t.Error("a dial error means the camera is unreachable")
	}
	if !isUnreachable(errors.New("dial tcp 192.168.1.50:554: i/o timeout")) {
		t.Error("textual dial errors must be recognised too")
	}
	if isUnreachable(errors.New("timeout: no packet for 10s")) {
		t.Error("a mid-session timeout is not 'unreachable'")
	}
}
