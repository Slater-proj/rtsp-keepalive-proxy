package proxy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph265"
	"github.com/pion/rtp"

	"rtsp-keepalive-proxy/internal/config"
)

// These tests replay the incident that motivated the neutral fallback: the
// camera goes to sleep with a person in view, the fallback keeps replaying
// that last frame, and the detector sees a permanent person.
//
// The "person" is a marker byte (0xCC) in the slice data of every camera
// frame; the neutral image carries 0xEE. A consumer-side decoder tells the
// two apart without any real video.

const (
	personByte  = 0xCC
	neutralByte = 0xEE
)

// codecFixture holds what differs between H.264 and H.265.
type codecFixture struct {
	name       string
	vps        []byte
	sps        []byte
	pps        []byte
	idrHeader  []byte
	pHeader    []byte
	neutralAU  [][]byte
	camParams  func() format.Format
	newEncoder func() (interface {
		Encode([][]byte) ([]*rtp.Packet, error)
	}, error)
}

func h264Fixture() codecFixture {
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	return codecFixture{
		name:      "h264",
		sps:       sps264CIF,
		pps:       pps,
		idrHeader: []byte{0x65},
		pHeader:   []byte{0x41},
		// the neutral frame deliberately carries its OWN parameter sets
		neutralAU: [][]byte{
			{0x67, 0x42, 0xc0, 0x1f, 0xAB, 0xAB, 0xAB, 0xAB},
			{0x68, 0xce, 0x3c, 0x80},
			append([]byte{0x65}, bytes.Repeat([]byte{neutralByte}, 40)...),
		},
		camParams: func() format.Format {
			return &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: sps264CIF, PPS: pps}
		},
		newEncoder: func() (interface {
			Encode([][]byte) ([]*rtp.Packet, error)
		}, error) {
			e := &rtph264.Encoder{PayloadType: 96, PayloadMaxSize: 1200, PacketizationMode: 1}
			return e, e.Init()
		},
	}
}

func h265Fixture() codecFixture {
	vps := []byte{0x40, 0x01, 0x0c, 0x01, 0xff, 0xff, 0x01, 0x60}
	pps := []byte{0x44, 0x01, 0xc0, 0x73, 0xc1, 0x89}
	return codecFixture{
		name:      "h265",
		vps:       vps,
		sps:       sps2651080,
		pps:       pps,
		idrHeader: []byte{0x26, 0x01}, // IDR_W_RADL
		pHeader:   []byte{0x02, 0x01}, // TRAIL_R
		neutralAU: [][]byte{
			{0x40, 0x01, 0x0c, 0x01, 0xAB, 0xAB},
			{0x42, 0x01, 0x01, 0xAB, 0xAB, 0xAB},
			{0x44, 0x01, 0xAB, 0xAB},
			append([]byte{0x26, 0x01}, bytes.Repeat([]byte{neutralByte}, 40)...),
		},
		camParams: func() format.Format {
			return &format.H265{PayloadTyp: 96, VPS: vps, SPS: sps2651080, PPS: pps}
		},
		newEncoder: func() (interface {
			Encode([][]byte) ([]*rtp.Packet, error)
		}, error) {
			e := &rtph265.Encoder{PayloadType: 96, PayloadMaxSize: 1200}
			return e, e.Init()
		},
	}
}

// personCamera is a fake RTSP camera whose every frame shows "a person".
type personCamera struct {
	fx     codecFixture
	srv    *gortsplib.Server
	stream *gortsplib.ServerStream
	desc   *description.Session
	enc    interface {
		Encode([][]byte) ([]*rtp.Packet, error)
	}
	port int
	ts   uint32
}

func (c *personCamera) OnDescribe(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, c.stream, nil
}
func (c *personCamera) OnSetup(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, c.stream, nil
}
func (c *personCamera) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func newPersonCamera(t *testing.T, fx codecFixture) *personCamera {
	t.Helper()
	c := &personCamera{fx: fx, port: freePort(t)}
	c.desc = &description.Session{Medias: []*description.Media{{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{fx.camParams()},
	}}}
	c.srv = &gortsplib.Server{Handler: c, RTSPAddress: fmt.Sprintf("127.0.0.1:%d", c.port)}
	if err := c.srv.Start(); err != nil {
		t.Fatal(err)
	}
	c.stream = gortsplib.NewServerStream(c.srv, c.desc)
	enc, err := fx.newEncoder()
	if err != nil {
		t.Fatal(err)
	}
	c.enc = enc
	t.Cleanup(func() { c.stream.Close(); c.srv.Close() })
	return c
}

func (c *personCamera) send(t *testing.T, idr bool) {
	t.Helper()
	body := bytes.Repeat([]byte{personByte}, 300)
	var au [][]byte
	if idr {
		if c.fx.vps != nil {
			au = append(au, c.fx.vps)
		}
		au = append(au, c.fx.sps, c.fx.pps, append(append([]byte{}, c.fx.idrHeader...), body...))
	} else {
		au = append(au, append(append([]byte{}, c.fx.pHeader...), body[:60]...))
	}
	pkts, err := c.enc.Encode(au)
	if err != nil {
		t.Fatal(err)
	}
	c.ts += 9000
	for _, p := range pkts {
		p.Timestamp = c.ts
		_ = c.stream.WritePacketRTP(c.desc.Medias[0], p)
	}
}

// stream sends frames for d (an IDR first, then P frames, 10 fps).
func (c *personCamera) streamFor(t *testing.T, d time.Duration) {
	t.Helper()
	end := time.Now().Add(d)
	for i := 0; time.Now().Before(end); i++ {
		c.send(t, i == 0)
		time.Sleep(100 * time.Millisecond)
	}
}

// auWatcher is a consumer that decodes the output and records what it sees.
type auWatcher struct {
	mu         sync.Mutex
	kinds      []string // "person" or "neutral", one per decoded slice AU
	neutralSPS [][]byte // SPS NAL units that came with neutral frames
	sps        []byte
	vps        []byte
	closed     chan struct{}
	client     *gortsplib.Client
	isH265     bool
	decode     func(*rtp.Packet) ([][]byte, error)
	timeout    time.Duration
}

func watchOutput(t *testing.T, port int, path string) *auWatcher {
	t.Helper()
	w := &auWatcher{closed: make(chan struct{})}
	tcp := gortsplib.TransportTCP
	w.client = &gortsplib.Client{Transport: &tcp}
	u, err := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/%s", port, path))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.client.Start(u.Scheme, u.Host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.client.Close)
	desc, _, err := w.client.Describe(u)
	if err != nil {
		t.Fatalf("describe output: %v", err)
	}

	var f264 *format.H264
	var f265 *format.H265
	switch {
	case desc.FindFormat(&f264) != nil:
		w.sps = f264.SPS
		dec, err := f264.CreateDecoder()
		if err != nil {
			t.Fatal(err)
		}
		w.decode = func(p *rtp.Packet) ([][]byte, error) { return dec.Decode(p) }
	case desc.FindFormat(&f265) != nil:
		w.isH265, w.sps, w.vps = true, f265.SPS, f265.VPS
		dec, err := f265.CreateDecoder()
		if err != nil {
			t.Fatal(err)
		}
		w.decode = func(p *rtp.Packet) ([][]byte, error) { return dec.Decode(p) }
	default:
		t.Fatal("no H.264/H.265 video in the output SDP")
	}

	if err := w.client.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("setup output: %v", err)
	}
	w.client.OnPacketRTPAny(func(m *description.Media, _ format.Format, p *rtp.Packet) {
		if m.Type != description.MediaTypeVideo {
			return
		}
		au, err := w.decode(p)
		if err != nil || len(au) == 0 {
			return
		}
		if k := w.classify(au); k != "" {
			w.mu.Lock()
			w.kinds = append(w.kinds, k)
			if k == "neutral" {
				if sps := w.spsOf(au); sps != nil {
					w.neutralSPS = append(w.neutralSPS, sps)
				}
			}
			w.mu.Unlock()
		}
	})
	if _, err := w.client.Play(nil); err != nil {
		t.Fatalf("play output: %v", err)
	}
	go func() { _ = w.client.Wait(); close(w.closed) }()
	return w
}

// classify returns "person" / "neutral" from the marker in the slice data.
func (w *auWatcher) classify(au [][]byte) string {
	for _, n := range au {
		if w.isH265 {
			if len(n) < 3 {
				continue
			}
			if nt := (n[0] >> 1) & 0x3F; nt <= 21 {
				return markerName(n[2])
			}
			continue
		}
		if len(n) < 2 {
			continue
		}
		if nt := n[0] & 0x1F; nt == 1 || nt == 5 {
			return markerName(n[1])
		}
	}
	return ""
}

// spsOf returns the SPS NAL unit carried by an access unit, if any.
func (w *auWatcher) spsOf(au [][]byte) []byte {
	for _, n := range au {
		if len(n) < 2 {
			continue
		}
		if w.isH265 && (n[0]>>1)&0x3F == 33 {
			return n
		}
		if !w.isH265 && n[0]&0x1F == 7 {
			return n
		}
	}
	return nil
}

func markerName(b byte) string {
	switch b {
	case personByte:
		return "person"
	case neutralByte:
		return "neutral"
	}
	return ""
}

func (w *auWatcher) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.kinds...)
}

func (w *auWatcher) count(kind string) int {
	n := 0
	for _, k := range w.snapshot() {
		if k == kind {
			n++
		}
	}
	return n
}

// lowHandler builds a low_ handler in neutral mode wired to a fake camera.
func lowHandler(t *testing.T, fx codecFixture, camPort int, hold time.Duration) *StreamHandler {
	t.Helper()
	cfg := config.ResolvedCamera{
		Source:          fmt.Sprintf("rtsp://127.0.0.1:%d/cam", camPort),
		BatteryMode:     true,
		RetryInterval:   200 * time.Millisecond,
		Timeout:         20 * time.Second,
		StallTimeout:    time.Second,
		FallbackFPS:     10,
		FallbackMode:    "neutral",
		FallbackHold:    hold,
		FallbackNeutral: "offline",
		Codec:           fx.name,
		Transport:       "tcp",
		Audio:           "none",
		Low:             true,
	}
	sh := NewStreamHandler("low_cam", cfg)
	sh.initialVPS, sh.initialSPS, sh.initialPPS = fx.vps, fx.sps, fx.pps
	sh.preBaseAU = nil
	// The encoder is injected so that the test does not need ffmpeg.
	sh.neutralEncoder = func([]byte, int, int, string, string, string) ([][]byte, error) {
		return fx.neutralAU, nil
	}
	return sh
}

func TestNeutralReplacesThePersonAfterTheHold(t *testing.T) {
	for _, fx := range []codecFixture{h264Fixture(), h265Fixture()} {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			cam := newPersonCamera(t, fx)
			sh := lowHandler(t, fx, cam.port, 700*time.Millisecond)

			outPort := freePort(t)
			srv := NewServer(outPort, map[string]*StreamHandler{"low_cam": sh})
			if err := srv.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(srv.Stop)

			rebuilds := 0
			var mu sync.Mutex
			orig := sh.rebuildCb
			sh.rebuildCb = func(d *description.Session) *gortsplib.ServerStream {
				mu.Lock()
				rebuilds++
				mu.Unlock()
				return orig(d)
			}

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			sh.Start(ctx)
			t.Cleanup(sh.Stop)

			w := watchOutput(t, outPort, "low_cam")
			advertisedSPS := append([]byte(nil), w.sps...)
			if !bytes.Equal(advertisedSPS, fx.sps) {
				t.Fatalf("the SDP must advertise the camera's SPS")
			}

			// 1. The camera is awake and shows the person.
			waitFor(t, 5*time.Second, "camera online", func() bool { return sh.GetState() == StateOnline })
			cam.streamFor(t, 1500*time.Millisecond)
			waitFor(t, 3*time.Second, "the person reaches the consumer", func() bool { return w.count("person") > 0 })
			if sh.FallbackActive() {
				t.Fatal("fallback active while the camera streams")
			}

			// 2. The camera stops with the person still in view: stall.
			waitFor(t, 5*time.Second, "fallback after the stall", func() bool { return sh.FallbackActive() })
			if k := sh.FallbackKind(); k != "last_keyframe" {
				t.Errorf("right after the stall the kind is %q, want last_keyframe (hold)", k)
			}
			if sh.FallbackSince().IsZero() {
				t.Error("FallbackSince must be set while the fallback runs")
			}

			// 3. After the hold only the neutral image is served.
			waitFor(t, 5*time.Second, "neutral image served", func() bool { return w.count("neutral") >= 2 })
			if k := sh.FallbackKind(); k != "neutral" {
				t.Errorf("kind = %q, want neutral", k)
			}
			kinds := w.snapshot()
			firstNeutral := -1
			for i, k := range kinds {
				if k == "neutral" {
					firstNeutral = i
					break
				}
			}
			for _, k := range kinds[firstNeutral:] {
				if k == "person" {
					t.Fatalf("a person frame was served again after the switch: %v", kinds)
				}
			}
			// The neutral frame carries its OWN parameter sets (detection streams
			// are decoded, never recorded); they must not be rewritten to the
			// camera's, or the neutral slice would be decoded with foreign ones.
			w.mu.Lock()
			seen := append([][]byte(nil), w.neutralSPS...)
			w.mu.Unlock()
			if len(seen) == 0 {
				t.Fatal("no SPS came with the neutral frames")
			}
			var wantSPS []byte
			for _, n := range fx.neutralAU {
				if (fx.name == "h264" && n[0]&0x1F == 7) || (fx.name == "h265" && (n[0]>>1)&0x3F == 33) {
					wantSPS = n
				}
			}
			for _, got := range seen {
				if !bytes.Equal(got, wantSPS) {
					t.Fatalf("neutral SPS was rewritten: got %x, want its own %x", got, wantSPS)
				}
			}

			// still nothing but neutral for another second
			before := len(w.snapshot())
			time.Sleep(1200 * time.Millisecond)
			for _, k := range w.snapshot()[before:] {
				if k != "neutral" {
					t.Fatalf("expected neutral only, got %q", k)
				}
			}

			// 4. Wake-up: the real stream takes back, no rebuild, same SPS.
			cam.send(t, true)
			cam.streamFor(t, 800*time.Millisecond)
			waitFor(t, 3*time.Second, "the camera takes back", func() bool {
				return !sh.FallbackActive() && sh.FallbackKind() == "none" && sh.GetState() == StateOnline
			})
			after := w.snapshot()
			if after[len(after)-1] != "person" {
				t.Errorf("last frame after the wake-up is %q, want the real stream", after[len(after)-1])
			}

			mu.Lock()
			defer mu.Unlock()
			if rebuilds != 0 {
				t.Errorf("stream rebuilt %d times: the SDP must not change", rebuilds)
			}
			select {
			case <-w.closed:
				t.Error("the consumer was disconnected during the whole scenario")
			default:
			}
		})
	}
}

// After a restart the persisted keyframe contains the person. It must never
// be shown: the neutral image is served from the start.
func TestRestartServesNeutralNotTheStoredPerson(t *testing.T) {
	fx := h264Fixture()
	dir := t.TempDir()

	// a keyframe saved by a previous run: the person
	var file bytes.Buffer
	for _, n := range [][]byte{fx.sps, fx.pps, append([]byte{0x65}, bytes.Repeat([]byte{personByte}, 300)...)} {
		file.Write(annexBStart)
		file.Write(n)
	}
	if err := os.WriteFile(filepath.Join(dir, "low_cam.h264"), file.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// the camera is asleep: nothing listens on its port
	deadPort := freePort(t)
	sh := lowHandler(t, fx, deadPort, 5*time.Second)
	sh.EnablePersistence(dir)

	sh.lastCameraVideoAUMu.RLock()
	restored := len(sh.lastCameraVideoAU)
	sh.lastCameraVideoAUMu.RUnlock()
	if restored == 0 {
		t.Fatal("setup: the persisted keyframe was not restored")
	}

	outPort := freePort(t)
	srv := NewServer(outPort, map[string]*StreamHandler{"low_cam": sh})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sh.Start(ctx)
	t.Cleanup(sh.Stop)

	w := watchOutput(t, outPort, "low_cam")
	waitFor(t, 5*time.Second, "neutral frames", func() bool { return w.count("neutral") >= 3 })
	if n := w.count("person"); n != 0 {
		t.Fatalf("%d frames showed the stored person after a restart", n)
	}
	if k := sh.FallbackKind(); k != "neutral" {
		t.Errorf("kind = %q, want neutral", k)
	}
}

// The recorded main stream is unaffected by the neutral mode: it keeps
// replaying the camera keyframe so that the recorder's codec configuration
// stays valid.
func TestMainStreamKeepsReplayingTheCameraFrame(t *testing.T) {
	fx := h264Fixture()
	cam := newPersonCamera(t, fx)
	sh := lowHandler(t, fx, cam.port, 300*time.Millisecond)
	sh.cfg.Low = false // main stream
	sh.Name = "cam"

	outPort := freePort(t)
	srv := NewServer(outPort, map[string]*StreamHandler{"cam": sh})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sh.Start(ctx)
	t.Cleanup(sh.Stop)

	w := watchOutput(t, outPort, "cam")
	waitFor(t, 5*time.Second, "camera online", func() bool { return sh.GetState() == StateOnline })
	cam.streamFor(t, 1200*time.Millisecond)
	waitFor(t, 5*time.Second, "fallback after the stall", func() bool { return sh.FallbackActive() })

	// well past the (short) hold: still the real frame, never the neutral one
	time.Sleep(1500 * time.Millisecond)
	if n := w.count("neutral"); n != 0 {
		t.Fatalf("the main stream served %d neutral frames", n)
	}
	if k := sh.FallbackKind(); k != "last_keyframe" {
		t.Errorf("kind = %q, want last_keyframe", k)
	}
}
