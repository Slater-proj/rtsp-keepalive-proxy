package proxy

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"time"
)

// Neutral fallback
//
// A battery camera that goes to sleep with a person still in view leaves that
// person in its last keyframe. Replaying that keyframe for hours makes the
// object detector (Frigate) see a permanent, motionless person.
//
// With fallback_mode: neutral, a LOW (detection) stream keeps the last real
// frame for fallback_hold, then switches to a neutral image: the generated
// "OFFLINE" frame, a black/grey frame, or a user file. Detection streams are
// never recorded, so the neutral frame may carry its own parameter sets: a
// decoder re-initialises on the in-band SPS/PPS at the IDR. Main (recorded)
// streams keep replaying the camera keyframe, whose parameter sets must match
// the recorder's codec configuration.

// neutralFPS is the frame rate of the neutral image (flat frames are tiny).
const neutralFPS = 2

// neutralFrame is an encoded neutral access unit and the parameters it was
// encoded for.
type neutralFrame struct {
	key string
	au  [][]byte
}

// neutralEncoder turns an image into an access unit (IDR with its own
// parameter sets). It is a field so tests can run without ffmpeg.
type neutralEncoder func(img []byte, w, h int, codec, profile, level string) ([][]byte, error)

// neutralEnabled reports whether this handler serves a neutral image.
func (sh *StreamHandler) neutralEnabled() bool {
	return sh.cfg.FallbackMode == "neutral" && sh.cfg.Low
}

func (sh *StreamHandler) neutralKind() string {
	if k := sh.cfg.FallbackNeutral; k != "" {
		return k
	}
	return "offline"
}

// neutralWanted returns the encoding parameters the neutral frame should have
// right now: the camera's picture size and, when known, its H.264 profile and
// level so that a hardware decoder has no reason to re-initialise.
func (sh *StreamHandler) neutralWanted() (key string, w, h int, codec, profile, level string) {
	w, h = sh.fallbackGen.Dimensions()
	if w%2 != 0 {
		w++
	}
	if h%2 != 0 {
		h++
	}
	codec = sh.GetCodec()

	sh.dimMu.Lock()
	profile, level = sh.camProfile, sh.camLevel
	sh.dimMu.Unlock()

	if codec != "h264" {
		profile, level = "", ""
	}
	key = fmt.Sprintf("%s|%s|%dx%d|%s|%s", sh.neutralKind(), codec, w, h, profile, level)
	return key, w, h, codec, profile, level
}

// ensureNeutral returns the neutral access unit for the current parameters,
// encoding it when needed. Safe for concurrent use.
func (sh *StreamHandler) ensureNeutral() ([][]byte, error) {
	sh.neutralMu.Lock()
	defer sh.neutralMu.Unlock()

	key, w, h, codec, profile, level := sh.neutralWanted()
	if sh.neutral.key == key && len(sh.neutral.au) > 0 {
		return sh.neutral.au, nil
	}

	img, err := sh.neutralImage(sh.neutralKind(), w, h)
	if err != nil {
		return nil, err
	}

	enc := sh.neutralEncoder
	if enc == nil {
		enc = sh.encodeNeutralFFmpeg
	}
	au, err := enc(img, w, h, codec, profile, level)
	if err != nil {
		return nil, err
	}
	if len(au) == 0 {
		return nil, fmt.Errorf("neutral frame: encoder returned no NAL units")
	}

	sh.neutral = neutralFrame{key: key, au: au}
	sh.log.Info("neutral frame ready",
		"kind", sh.neutralKind(), "codec", codec, "size", fmt.Sprintf("%dx%d", w, h),
		"profile", profile, "bytes", auBytes(au))
	return au, nil
}

// neutralImage builds the source picture of the neutral frame.
func (sh *StreamHandler) neutralImage(kind string, w, h int) ([]byte, error) {
	switch {
	case kind == "offline":
		data, _, _ := sh.fallbackGen.GetFramePNG()
		if len(data) == 0 {
			return nil, fmt.Errorf("neutral frame: no offline image available")
		}
		return data, nil

	case kind == "black", kind == "grey":
		c := color.RGBA{A: 255}
		if kind == "grey" {
			c = color.RGBA{R: 128, G: 128, B: 128, A: 255}
		}
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil

	case strings.HasPrefix(kind, "file:"):
		path := strings.TrimPrefix(kind, "file:")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("neutral frame: read %s: %w", path, err)
		}
		// ffmpeg detects the format and scales it to w x h.
		return data, nil
	}
	return nil, fmt.Errorf("neutral frame: unknown kind %q", kind)
}

// encodeNeutralFFmpeg is the production neutralEncoder.
func (sh *StreamHandler) encodeNeutralFFmpeg(img []byte, w, h int, codec, profile, level string) ([][]byte, error) {
	au, _, _, _ := sh.encodePNGToAUProfile(img, w, h, codec, profile, level)
	if len(au) == 0 {
		return nil, fmt.Errorf("neutral frame: ffmpeg produced nothing (is ffmpeg installed?)")
	}
	return au, nil
}

// h264Profile maps an H.264 profile_idc to an x264 profile name.
func h264Profile(idc uint8) string {
	switch idc {
	case 66:
		return "baseline"
	case 77, 88:
		return "main"
	case 100, 110, 122, 244:
		return "high"
	}
	return ""
}

// h264Level formats a level_idc (31) as an x264 level ("3.1").
func h264Level(idc uint8) string {
	if idc < 10 {
		return ""
	}
	return fmt.Sprintf("%d.%d", idc/10, idc%10)
}

// --- fallback state exposed by /status ---------------------------------

func (sh *StreamHandler) setFallbackKind(kind string) {
	sh.fbMu.Lock()
	sh.fbKind = kind
	sh.fbMu.Unlock()
}

func (sh *StreamHandler) markFallbackStart() {
	sh.fbMu.Lock()
	sh.fbSince = time.Now()
	sh.fbKind = "last_keyframe"
	sh.fbMu.Unlock()
}

func (sh *StreamHandler) clearFallbackState() {
	sh.fbMu.Lock()
	sh.fbKind = ""
	sh.fbSince = time.Time{}
	sh.fbMu.Unlock()
}

// FallbackKind tells what consumers currently receive instead of the camera:
// "last_keyframe" (replay of the camera's last frame), "neutral" (generated
// image) or "none" (the camera itself, or nothing).
func (sh *StreamHandler) FallbackKind() string {
	sh.fbMu.Lock()
	defer sh.fbMu.Unlock()
	if sh.fbKind == "" {
		return "none"
	}
	return sh.fbKind
}

// FallbackSince is when the current fallback started (zero when none).
func (sh *StreamHandler) FallbackSince() time.Time {
	sh.fbMu.Lock()
	defer sh.fbMu.Unlock()
	return sh.fbSince
}
