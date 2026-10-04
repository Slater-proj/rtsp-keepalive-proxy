package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/pkg/codecs/mpeg4audio"
	"github.com/pion/rtp"

	"rtsp-keepalive-proxy/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout after %s waiting for: %s", d, what)
}

// fakeCamera is a minimal RTSP server standing in for a battery camera: one
// H.264 video track plus an AAC audio track (which the proxy cannot relay).
type fakeCamera struct {
	srv    *gortsplib.Server
	stream *gortsplib.ServerStream
	desc   *description.Session
	enc    *rtph264.Encoder
	port   int
	ts     uint32
}

func (f *fakeCamera) OnDescribe(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, f.stream, nil
}
func (f *fakeCamera) OnSetup(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, f.stream, nil
}
func (f *fakeCamera) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func newFakeCamera(t *testing.T) *fakeCamera {
	t.Helper()
	f := &fakeCamera{port: freePort(t)}
	f.desc = &description.Session{Medias: []*description.Media{
		{
			Type: description.MediaTypeVideo,
			Formats: []format.Format{&format.H264{
				PayloadTyp: 96, PacketizationMode: 1,
				SPS: sps264CIF, PPS: []byte{0x68, 0xee, 0x3c, 0x80},
			}},
		},
		{
			Type: description.MediaTypeAudio,
			Formats: []format.Format{&format.MPEG4Audio{
				PayloadTyp: 97,
				Config:     &mpeg4audio.Config{Type: mpeg4audio.ObjectTypeAACLC, SampleRate: 48000, ChannelCount: 2},
				SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3,
			}},
		},
	}}
	f.srv = &gortsplib.Server{Handler: f, RTSPAddress: fmt.Sprintf("127.0.0.1:%d", f.port)}
	if err := f.srv.Start(); err != nil {
		t.Fatal(err)
	}
	f.stream = gortsplib.NewServerStream(f.srv, f.desc)
	f.enc = &rtph264.Encoder{PayloadType: 96, PayloadMaxSize: 1200, PacketizationMode: 1}
	if err := f.enc.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.close)
	return f
}

func (f *fakeCamera) close() {
	f.stream.Close()
	f.srv.Close()
}

// sendFrame emits one access unit; idr selects an IDR slice (type 5) or a
// P slice (type 1). The AU carries the parameter sets too for IDR.
func (f *fakeCamera) sendFrame(t *testing.T, idr bool) {
	t.Helper()
	var au [][]byte
	if idr {
		au = [][]byte{sps264CIF, {0x68, 0xee, 0x3c, 0x80}, append([]byte{0x65}, make([]byte, 300)...)}
	} else {
		au = [][]byte{append([]byte{0x41}, make([]byte, 60)...)}
	}
	pkts, err := f.enc.Encode(au)
	if err != nil {
		t.Fatal(err)
	}
	f.ts += 9000 // 10 fps
	for _, p := range pkts {
		p.Timestamp = f.ts
		_ = f.stream.WritePacketRTP(f.desc.Medias[0], p)
	}
	// An AAC-looking audio packet; the proxy must never forward it.
	_ = f.stream.WritePacketRTP(f.desc.Medias[1], &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 97, Timestamp: f.ts, SSRC: 1},
		Payload: []byte{0, 16, 0, 8, 0xAA, 0xAA, 0xAA, 0xAA},
	})
}

// outputClient plays the proxy's output like Frigate/go2rtc would.
type outputClient struct {
	mu        sync.Mutex
	video     []rtp.Header
	audioSeen int
	audioBad  int    // audio payloads that came from the (AAC) camera
	sps       []byte // SPS announced in the SDP
	c         *gortsplib.Client
}

func playOutput(t *testing.T, port int, path string) *outputClient {
	t.Helper()
	oc := &outputClient{}
	tcp := gortsplib.TransportTCP
	oc.c = &gortsplib.Client{Transport: &tcp}
	u, err := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/%s", port, path))
	if err != nil {
		t.Fatal(err)
	}
	if err := oc.c.Start(u.Scheme, u.Host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oc.c.Close)
	desc, _, err := oc.c.Describe(u)
	if err != nil {
		t.Fatalf("describe output: %v", err)
	}
	var h264 *format.H264
	if desc.FindFormat(&h264) != nil {
		oc.sps = h264.SPS
	}
	if len(desc.Medias) != 2 {
		t.Fatalf("output SDP has %d medias, want video+audio", len(desc.Medias))
	}
	if err := oc.c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("setup output: %v", err)
	}
	oc.c.OnPacketRTPAny(func(m *description.Media, _ format.Format, p *rtp.Packet) {
		oc.mu.Lock()
		defer oc.mu.Unlock()
		if m.Type == description.MediaTypeVideo {
			oc.video = append(oc.video, p.Header)
			return
		}
		oc.audioSeen++
		if p.PayloadType != 8 {
			oc.audioBad++
		}
		for _, b := range p.Payload {
			if b == 0xAA {
				oc.audioBad++
				break
			}
		}
	})
	if _, err := oc.c.Play(nil); err != nil {
		t.Fatalf("play output: %v", err)
	}
	return oc
}

func (oc *outputClient) videoCount() int {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	return len(oc.video)
}

// checkContinuity verifies the RTP normaliser: one SSRC and gap-free,
// monotonic sequence numbers across every source switch.
func (oc *outputClient) checkContinuity(t *testing.T) {
	t.Helper()
	oc.mu.Lock()
	defer oc.mu.Unlock()
	for i := 1; i < len(oc.video); i++ {
		a, b := oc.video[i-1], oc.video[i]
		if a.SSRC != b.SSRC {
			t.Fatalf("SSRC changed at packet %d: %#x -> %#x", i, a.SSRC, b.SSRC)
		}
		if b.SequenceNumber != a.SequenceNumber+1 {
			t.Fatalf("sequence gap at packet %d: %d -> %d", i, a.SequenceNumber, b.SequenceNumber)
		}
	}
}

// TestRelayFallbackLifecycle drives the whole chain with a fake camera:
//
//	fallback at start -> camera takes over on its first IDR -> video stall
//	-> fallback again -> P frames alone do not take back -> IDR does ->
//	camera closes the connection -> fallback again.
func TestRelayFallbackLifecycle(t *testing.T) {
	cam := newFakeCamera(t)

	cfg := config.ResolvedCamera{
		Source:        fmt.Sprintf("rtsp://user:pw@127.0.0.1:%d/cam", cam.port),
		BatteryMode:   true,
		RetryInterval: 200 * time.Millisecond,
		Timeout:       20 * time.Second, // long: the stall / close paths must fire first
		StallTimeout:  time.Second,
		FallbackFPS:   10,
		FallbackMode:  "offline",
		Codec:         "h264",
		Transport:     "tcp",
		Audio:         "auto",
	}
	sh := NewStreamHandler("cam", cfg)
	// ffmpeg may be missing on the test machine: inject a tiny fallback AU.
	fbSPS, fbPPS := sps264CIF, []byte{0x68, 0xee, 0x3c, 0x80}
	sh.preBaseAU = [][]byte{fbSPS, fbPPS, append([]byte{0x65}, make([]byte, 40)...)}
	sh.preDotAU = nil
	sh.initialSPS, sh.initialPPS = fbSPS, fbPPS

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

	oc := playOutput(t, outPort, "cam")

	// 1. Fallback streams from the very first moment.
	waitFor(t, 5*time.Second, "fallback packets before the camera streams", func() bool {
		return oc.videoCount() > 5 && sh.fallbackRunning()
	})

	// 2. The camera connects; the fallback keeps running until its first IDR.
	waitFor(t, 5*time.Second, "camera connected", func() bool { return sh.GetState() == StateOnline })
	for i := 0; i < 3; i++ {
		cam.sendFrame(t, false)
		time.Sleep(50 * time.Millisecond)
	}
	if !sh.fallbackRunning() {
		t.Fatal("fallback stopped before the first camera IDR")
	}
	cam.sendFrame(t, true)
	waitFor(t, 3*time.Second, "fallback stops on the first IDR", func() bool { return !sh.fallbackRunning() })

	// Stream a bit of live video.
	for i := 0; i < 10; i++ {
		cam.sendFrame(t, i == 0)
		time.Sleep(50 * time.Millisecond)
	}
	if sh.fallbackRunning() {
		t.Fatal("fallback restarted while the camera is streaming")
	}

	// 3. Stall: connection open, no video. Fallback must take over well
	// before the 20 s packet timeout.
	started := time.Now()
	waitFor(t, 5*time.Second, "fallback on video stall", func() bool { return sh.fallbackRunning() })
	if d := time.Since(started); d > 4*time.Second {
		t.Errorf("stall detected after %s, want about StallTimeout (1s)", d)
	}
	if sh.GetState() != StateSleeping {
		t.Errorf("state = %s during stall, want sleeping", sh.GetState())
	}

	// 4. P frames alone must not replace the fallback; an IDR does.
	for i := 0; i < 5; i++ {
		cam.sendFrame(t, false)
		time.Sleep(50 * time.Millisecond)
	}
	if !sh.fallbackRunning() {
		t.Fatal("P frames took over from the fallback without a keyframe")
	}
	cam.sendFrame(t, true)
	waitFor(t, 3*time.Second, "camera takes back on IDR", func() bool {
		return !sh.fallbackRunning() && sh.GetState() == StateOnline
	})

	// 5. The camera drops the RTSP connection (goes to sleep): immediate
	// fallback, no waiting for the 20 s timeout.
	cam.srv.Close()
	started = time.Now()
	waitFor(t, 5*time.Second, "fallback after connection close", func() bool { return sh.fallbackRunning() })
	if d := time.Since(started); d > 3*time.Second {
		t.Errorf("close detected after %s, want near-immediate", d)
	}

	// The consumer saw one uninterrupted RTP stream through all of it.
	before := oc.videoCount()
	waitFor(t, 3*time.Second, "output keeps flowing", func() bool { return oc.videoCount() > before+5 })
	oc.checkContinuity(t)

	oc.mu.Lock()
	defer oc.mu.Unlock()
	if oc.audioSeen == 0 {
		t.Error("no audio at all: the silent track should always run")
	}
	if oc.audioBad != 0 {
		t.Errorf("%d audio packets carried the camera's AAC payload / wrong payload type", oc.audioBad)
	}
}

// TestColdStartRebuildsStreamWithCameraParameters reproduces the recording
// corruption seen on a real deployment: after a restart with an empty
// data_dir, the SDP carries the generated fallback's SPS/PPS. A consumer that
// connected then keeps that SDP, and an MP4 recorder (ffmpeg -c copy) writes a
// codec configuration that does not match the camera's data. When the camera
// shows up with different parameter sets, the stream must be rebuilt so the
// consumer is disconnected and reconnects on the camera's real parameters.
func TestColdStartRebuildsStreamWithCameraParameters(t *testing.T) {
	cam := newFakeCamera(t)

	cfg := config.ResolvedCamera{
		Source:        fmt.Sprintf("rtsp://127.0.0.1:%d/cam", cam.port),
		BatteryMode:   true,
		RetryInterval: 200 * time.Millisecond,
		Timeout:       20 * time.Second,
		StallTimeout:  time.Second,
		FallbackFPS:   10,
		FallbackMode:  "offline",
		Codec:         "h264",
		Transport:     "tcp",
		Audio:         "none",
	}
	sh := NewStreamHandler("cam", cfg)
	// What a cold start advertises: the generated fallback's parameter sets.
	genSPS := append([]byte{0x67, 0x42, 0xc0, 0x1f}, bytes.Repeat([]byte{0xAB}, 12)...)
	genPPS := []byte{0x68, 0xce, 0x3c, 0x80}
	sh.initialSPS, sh.initialPPS = genSPS, genPPS
	sh.preBaseAU = [][]byte{genSPS, genPPS, append([]byte{0x65}, make([]byte, 40)...)}

	// The camera must be reachable only after the first consumer connected.
	if !sh.cameraParamsDiffer("h264", cam.desc) {
		t.Fatal("the generated and the camera parameter sets are supposed to differ")
	}

	outPort := freePort(t)
	srv := NewServer(outPort, map[string]*StreamHandler{"cam": sh})
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

	// Consumer connects BEFORE the camera ever woke up (Frigate's ffmpeg).
	oc := playOutput(t, outPort, "cam")
	before := oc.describedSPS(t)
	if !bytes.Equal(before, genSPS) {
		t.Fatalf("setup: consumer should see the generated SPS first, got %x", before)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sh.Start(ctx)
	t.Cleanup(sh.Stop)

	waitFor(t, 5*time.Second, "stream rebuilt for the camera's parameters", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return rebuilds == 1
	})

	// The old consumer is cut so that it reconnects.
	closed := make(chan error, 1)
	go func() { closed <- oc.c.Wait() }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the connected consumer was not disconnected by the rebuild")
	}

	// A new consumer gets the camera's real SPS/PPS.
	oc2 := playOutput(t, outPort, "cam")
	if got := oc2.describedSPS(t); !bytes.Equal(got, sps264CIF) {
		t.Errorf("new consumer SPS = %x, want the camera's %x", got, sps264CIF)
	}
	if sh.cameraParamsDiffer("h264", cam.desc) {
		t.Error("advertised and camera parameter sets must now match")
	}

	// And the camera relay works end to end on the rebuilt stream.
	waitFor(t, 5*time.Second, "camera online", func() bool { return sh.GetState() == StateOnline })
	cam.sendFrame(t, true)
	for i := 0; i < 5; i++ {
		cam.sendFrame(t, false)
		time.Sleep(50 * time.Millisecond)
	}
	waitFor(t, 3*time.Second, "video reaches the new consumer", func() bool { return oc2.videoCount() > 0 })

	mu.Lock()
	defer mu.Unlock()
	if rebuilds != 1 {
		t.Errorf("stream rebuilt %d times, want exactly once", rebuilds)
	}
}

// describedSPS returns the SPS the consumer was given in the SDP.
func (oc *outputClient) describedSPS(t *testing.T) []byte {
	t.Helper()
	return oc.sps
}
