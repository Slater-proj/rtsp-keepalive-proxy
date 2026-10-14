package proxy

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
)

// fallbackBudgetBytesPerSec bounds the bandwidth of the replayed fallback
// (about 1.6 Mbit/s per stream).
const fallbackBudgetBytesPerSec = 200_000

// minFallbackFPS: below 2 fps FFmpeg recording may fail to create valid
// segments because the data rate is too low for the muxer.
const minFallbackFPS = 2

// auBytes returns the total payload size of an access unit.
func auBytes(au [][]byte) int {
	n := 0
	for _, nalu := range au {
		n += len(nalu)
	}
	return n
}

// capFallbackFPS lowers fps so that replaying a frame of frameBytes bytes
// stays within fallbackBudgetBytesPerSec, never going below minFallbackFPS.
func capFallbackFPS(fps, frameBytes int) int {
	if frameBytes <= 0 {
		return fps
	}
	limit := fallbackBudgetBytesPerSec / frameBytes
	if limit < minFallbackFPS {
		limit = minFallbackFPS
	}
	if fps > limit {
		return limit
	}
	return fps
}

// isUnreachable reports whether err means "could not even open a TCP
// connection to the camera", which is the normal state of a sleeping one.
func isUnreachable(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return strings.Contains(err.Error(), "dial tcp")
}

// probeTCP opens and immediately closes a TCP connection to the camera's RTSP
// port. It returns a dial error when the camera is asleep or unreachable.
func probeTCP(ctx context.Context, u *base.URL, timeout time.Duration) error {
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		port := "554"
		if u.Scheme == "rtsps" {
			port = "322"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return err
	}
	return conn.Close()
}
