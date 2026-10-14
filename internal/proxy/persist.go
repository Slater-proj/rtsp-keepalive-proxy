package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"rtsp-keepalive-proxy/internal/fallback"
)

// persistEvery bounds how often the last camera keyframe is rewritten.
const persistEvery = 60 * time.Second

var annexBStart = []byte{0x00, 0x00, 0x00, 0x01}

// persistence keeps the last camera keyframe on disk so that, after a restart
// of the proxy, the very first fallback frames and the SDP already carry the
// camera's REAL parameter sets and resolution. Without it the first fallback
// is encoded by libx264/libx265, whose parameter sets differ from the
// camera's: an MP4 recording segment spanning "fallback -> camera" then has a
// codec configuration that does not match half of its data.
type persistence struct {
	dir       string
	lastWrite atomic.Int64 // unix seconds
}

func (p *persistence) path(name, codec string) string {
	ext := ".h264"
	if codec == "h265" {
		ext = ".h265"
	}
	return filepath.Join(p.dir, name+ext)
}

// EnablePersistence turns on keyframe persistence under dir and restores a
// previously saved keyframe, if any. It must be called before the output
// description is built (i.e. before the RTSP server starts). An empty dir
// leaves the feature off.
func (sh *StreamHandler) EnablePersistence(dir string) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		sh.log.Warn("keyframe persistence disabled: cannot create data dir", "dir", dir, "error", err)
		return
	}
	sh.persist = &persistence{dir: dir}
	sh.restoreKeyframe()
}

// restoreKeyframe loads the saved keyframe and seeds the codec, the
// advertised parameter sets, the fallback resolution and the fallback frame.
func (sh *StreamHandler) restoreKeyframe() {
	for _, codec := range []string{"h264", "h265"} {
		// An explicit codec in the config wins over what is on disk.
		if sh.cfg.Codec != "auto" && sh.cfg.Codec != "" && sh.cfg.Codec != codec {
			continue
		}
		data, err := os.ReadFile(sh.persist.path(sh.Name, codec))
		if err != nil {
			continue
		}
		au := fallback.ParseAnnexB(data)
		if !isKeyframeAU(au, codec) {
			sh.log.Warn("saved keyframe ignored: not a keyframe", "codec", codec)
			continue
		}
		vps, sps, pps := paramSets(au, codec)
		if sps == nil || pps == nil || (codec == "h265" && vps == nil) {
			sh.log.Warn("saved keyframe ignored: parameter sets missing", "codec", codec)
			continue
		}

		sh.mu.Lock()
		sh.detectedCodec = codec
		sh.initialVPS, sh.initialSPS, sh.initialPPS = copyBytes(vps), copyBytes(sps), copyBytes(pps)
		sh.mu.Unlock()

		sh.lastCameraVideoAUMu.Lock()
		sh.lastCameraVideoAU = au
		sh.lastCameraVideoAUMu.Unlock()

		sh.noteSPS(codec, sps)
		sh.log.Info("restored last camera keyframe from disk",
			"codec", codec, "bytes", auBytes(au))
		return
	}
}

// saveKeyframe writes the keyframe to disk, at most once per persistEvery.
func (sh *StreamHandler) saveKeyframe(au [][]byte, codec string) {
	p := sh.persist
	if p == nil {
		return
	}
	now := time.Now().Unix()
	if last := p.lastWrite.Load(); now-last < int64(persistEvery/time.Second) {
		return
	}
	p.lastWrite.Store(now)

	var buf bytes.Buffer
	for _, nalu := range au {
		buf.Write(annexBStart)
		buf.Write(nalu)
	}
	go func(data []byte) {
		if err := writeFileAtomic(p.path(sh.Name, codec), data); err != nil {
			sh.log.Warn("cannot save camera keyframe", "error", err)
			return
		}
		// A different codec's stale file would shadow this one on restore.
		other := "h265"
		if codec == "h265" {
			other = "h264"
		}
		_ = os.Remove(p.path(sh.Name, other))
	}(buf.Bytes())
}

// writeFileAtomic writes via a temp file + rename so a crash never leaves a
// truncated keyframe behind.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kf-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// isKeyframeAU reports whether au contains an IDR (H.264) / IRAP (H.265) slice.
func isKeyframeAU(au [][]byte, codec string) bool {
	for _, nalu := range au {
		if codec == "h265" {
			if len(nalu) >= 2 {
				if nt := (nalu[0] >> 1) & 0x3F; nt >= 16 && nt <= 21 {
					return true
				}
			}
		} else if len(nalu) > 0 && nalu[0]&0x1F == 5 {
			return true
		}
	}
	return false
}

// paramSets extracts VPS (H.265 only), SPS and PPS from an access unit.
func paramSets(au [][]byte, codec string) (vps, sps, pps []byte) {
	for _, nalu := range au {
		if codec == "h265" {
			if len(nalu) < 2 {
				continue
			}
			switch (nalu[0] >> 1) & 0x3F {
			case 32:
				vps = nalu
			case 33:
				sps = nalu
			case 34:
				pps = nalu
			}
			continue
		}
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1F {
		case 7:
			sps = nalu
		case 8:
			pps = nalu
		}
	}
	return vps, sps, pps
}
