package proxy

import (
	"context"
	"sync"
	"time"
)

// handshakeSpacing is the pause kept between two RTSP handshakes towards the
// same camera.
const handshakeSpacing = 300 * time.Millisecond

// hostGate lets one stream at a time run its RTSP handshake
// (DESCRIBE/SETUP/PLAY) towards a given camera address. A camera has a main
// and a sub stream, both opened the moment it wakes up; measured on the
// IMOU Cell 2 that is when it resets or ignores connections.
type hostGate struct{ sem chan struct{} }

var hostGates sync.Map // "host:port" -> *hostGate

func gateFor(host string) *hostGate {
	v, _ := hostGates.LoadOrStore(host, &hostGate{sem: make(chan struct{}, 1)})
	return v.(*hostGate)
}

func (g *hostGate) acquire(ctx context.Context) error {
	select {
	case g.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseAfter frees the gate after d, so the next handshake starts a
// little later instead of right behind this one.
func (g *hostGate) releaseAfter(d time.Duration) {
	if d <= 0 {
		<-g.sem
		return
	}
	time.AfterFunc(d, func() { <-g.sem })
}
