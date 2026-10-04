// Package config handles loading and validating the YAML configuration.
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root application configuration.
type Config struct {
	Server   ServerConfig            `yaml:"server"`
	Defaults CameraDefaults          `yaml:"defaults"`
	Cameras  map[string]CameraConfig `yaml:"cameras"`
}

// ServerConfig holds server-level settings.
type ServerConfig struct {
	RTSPPort   int    `yaml:"rtsp_port"`
	HealthPort int    `yaml:"health_port"`
	LogLevel   string `yaml:"log_level"`
	// DataDir, when set, is where the last camera keyframe of each stream is
	// kept across restarts (empty = feature off).
	DataDir string `yaml:"data_dir"`
}

// CameraDefaults holds default values applied to every camera.
type CameraDefaults struct {
	BatteryMode   bool          `yaml:"battery_mode"`
	RetryInterval time.Duration `yaml:"retry_interval"`
	Timeout       time.Duration `yaml:"timeout"`
	DialTimeout   time.Duration `yaml:"dial_timeout"`  // 0 = automatic (3 s battery, 5 s otherwise)
	StallTimeout  time.Duration `yaml:"stall_timeout"` // no video for this long -> start fallback
	FallbackFPS   int           `yaml:"fallback_fps"`
	FallbackMode  string        `yaml:"fallback_mode"` // "offline", "last_frame", "none"
	Codec         string        `yaml:"codec"`
	Transport     string        `yaml:"transport"` // "tcp", "udp", "auto"
	Audio         string        `yaml:"audio"`     // "auto", "none"
}

// CameraConfig holds per-camera settings. Pointer fields allow distinguishing
// "not set" from zero values so the defaults layer works correctly.
type CameraConfig struct {
	Source    string `yaml:"source"`
	SourceLow string `yaml:"source_low"`

	// Optional resolution hints for fallback frames. When the camera has
	// never connected, these are used instead of the default 1920x1080.
	// Particularly important for low streams that are typically 640x480.
	Width  *int `yaml:"width,omitempty"`
	Height *int `yaml:"height,omitempty"`

	// Low stream resolution (used for source_low fallback).
	WidthLow  *int `yaml:"width_low,omitempty"`
	HeightLow *int `yaml:"height_low,omitempty"`

	BatteryMode   *bool          `yaml:"battery_mode,omitempty"`
	RetryInterval *time.Duration `yaml:"retry_interval,omitempty"`
	Timeout       *time.Duration `yaml:"timeout,omitempty"`
	DialTimeout   *time.Duration `yaml:"dial_timeout,omitempty"`
	StallTimeout  *time.Duration `yaml:"stall_timeout,omitempty"`
	FallbackFPS   *int           `yaml:"fallback_fps,omitempty"`
	FallbackMode  *string        `yaml:"fallback_mode,omitempty"` // "offline", "last_frame", "none"
	Codec         *string        `yaml:"codec,omitempty"`
	CodecLow      *string        `yaml:"codec_low,omitempty"` // codec for source_low (defaults to codec)
	Transport     *string        `yaml:"transport,omitempty"`
	Audio         *string        `yaml:"audio,omitempty"`
}

// Effective returns a resolved copy where nil fields are filled from defaults.
func (c CameraConfig) Effective(d CameraDefaults) ResolvedCamera {
	r := ResolvedCamera{
		Source:        c.Source,
		SourceLow:     c.SourceLow,
		BatteryMode:   d.BatteryMode,
		RetryInterval: d.RetryInterval,
		Timeout:       d.Timeout,
		DialTimeout:   d.DialTimeout,
		StallTimeout:  d.StallTimeout,
		FallbackFPS:   d.FallbackFPS,
		FallbackMode:  d.FallbackMode,
		Codec:         d.Codec,
		Transport:     d.Transport,
		Audio:         d.Audio,
	}
	if c.Width != nil {
		r.Width = *c.Width
	}
	if c.Height != nil {
		r.Height = *c.Height
	}
	if c.BatteryMode != nil {
		r.BatteryMode = *c.BatteryMode
	}
	if c.RetryInterval != nil {
		r.RetryInterval = *c.RetryInterval
	}
	if c.Timeout != nil {
		r.Timeout = *c.Timeout
	}
	if c.DialTimeout != nil {
		r.DialTimeout = *c.DialTimeout
	}
	if c.StallTimeout != nil {
		r.StallTimeout = *c.StallTimeout
	}
	if c.FallbackFPS != nil {
		r.FallbackFPS = *c.FallbackFPS
	}
	if c.FallbackMode != nil {
		r.FallbackMode = *c.FallbackMode
	}
	if c.Codec != nil {
		r.Codec = *c.Codec
	}
	if c.Transport != nil {
		r.Transport = *c.Transport
	}
	if c.Audio != nil {
		r.Audio = *c.Audio
	}
	return r
}

// ResolvedCamera is a fully-resolved camera configuration with no nil fields.
type ResolvedCamera struct {
	Source        string
	SourceLow     string
	Width         int // fallback resolution hint (0 = auto-detect or default 1920)
	Height        int // fallback resolution hint (0 = auto-detect or default 1080)
	BatteryMode   bool
	RetryInterval time.Duration
	Timeout       time.Duration
	DialTimeout   time.Duration // 0 = automatic
	StallTimeout  time.Duration
	FallbackFPS   int
	FallbackMode  string // "offline", "last_frame", "none"
	Codec         string
	Transport     string // "tcp", "udp", "auto"
	Audio         string // "auto", "none"
}

// Load reads and parses a YAML configuration file.
// Environment variables in the form ${VAR} are expanded before parsing.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return Parse(data)
}

// envRef matches ${VAR}. The bare $VAR form is deliberately NOT expanded so
// that passwords containing a literal "$" survive untouched.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} with the value of the environment variable and
// returns an error listing every variable that is not set, instead of
// silently substituting an empty string (which yields empty camera
// credentials and a confusing "unauthorized" at runtime).
func expandEnv(s string) (string, error) {
	var missing []string
	seen := map[string]bool{}
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			if !seen[name] {
				seen[name] = true
				missing = append(missing, name)
			}
			return m
		}
		return v
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("config: undefined environment variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Parse parses raw YAML bytes into a validated Config.
func Parse(data []byte) (*Config, error) {
	expanded, err := expandEnv(string(data))
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// battery_mode is a plain bool, so "omitted" and "false" look the same
	// after unmarshalling. Probe the raw YAML to apply the documented
	// default (true) only when the key is really absent.
	var probe struct {
		Defaults struct {
			BatteryMode *bool `yaml:"battery_mode"`
		} `yaml:"defaults"`
	}
	if err := yaml.Unmarshal([]byte(expanded), &probe); err == nil && probe.Defaults.BatteryMode == nil {
		cfg.Defaults.BatteryMode = true
	}

	applyServerDefaults(&cfg.Server)
	applyGlobalDefaults(&cfg.Defaults)

	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyServerDefaults(s *ServerConfig) {
	if s.RTSPPort == 0 {
		s.RTSPPort = 8554
	}
	if s.HealthPort == 0 {
		s.HealthPort = 8080
	}
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
}

func applyGlobalDefaults(d *CameraDefaults) {
	if d.RetryInterval == 0 {
		d.RetryInterval = 3 * time.Second
	}
	if d.Timeout == 0 {
		d.Timeout = 10 * time.Second
	}
	if d.StallTimeout == 0 {
		d.StallTimeout = 3 * time.Second
	}
	if d.FallbackFPS == 0 {
		d.FallbackFPS = 5
	}
	if d.FallbackMode == "" {
		d.FallbackMode = "offline"
	}
	if d.Codec == "" {
		d.Codec = "auto"
	}
	if d.Transport == "" {
		d.Transport = "tcp"
	}
	if d.Audio == "" {
		d.Audio = "auto"
	}
}

// cameraName restricts names to what is safe in an RTSP path, a log line and
// an ffmpeg drawtext expression.
var cameraName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func validate(cfg *Config) error {
	if len(cfg.Cameras) == 0 {
		return fmt.Errorf("config: no cameras defined")
	}
	if err := validateSettings("defaults", cfg.Defaults.FallbackMode, cfg.Defaults.Codec,
		cfg.Defaults.Transport, cfg.Defaults.Audio, cfg.Defaults.FallbackFPS,
		cfg.Defaults.RetryInterval, cfg.Defaults.Timeout, cfg.Defaults.StallTimeout, cfg.Defaults.DialTimeout); err != nil {
		return err
	}

	// Output paths: <name> and, when source_low is set, low_<name>.
	paths := map[string]string{}
	claim := func(path, owner string) error {
		if prev, dup := paths[path]; dup {
			return fmt.Errorf("config: output path %q is used by both %s and %s", path, prev, owner)
		}
		paths[path] = owner
		return nil
	}

	names := make([]string, 0, len(cfg.Cameras))
	for name := range cfg.Cameras {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		cam := cfg.Cameras[name]
		if !cameraName.MatchString(name) {
			return fmt.Errorf("config: camera name %q is invalid (use letters, digits, '_' and '-')", name)
		}
		if cam.Source == "" {
			return fmt.Errorf("config: camera %q missing source URL", name)
		}
		if err := claim(name, fmt.Sprintf("camera %q", name)); err != nil {
			return err
		}
		if cam.SourceLow != "" {
			if err := claim("low_"+name, fmt.Sprintf("camera %q (source_low)", name)); err != nil {
				return err
			}
		}

		r := cam.Effective(cfg.Defaults)
		if err := validateSettings(fmt.Sprintf("camera %q", name), r.FallbackMode, r.Codec,
			r.Transport, r.Audio, r.FallbackFPS, r.RetryInterval, r.Timeout, r.StallTimeout, r.DialTimeout); err != nil {
			return err
		}
		if cam.CodecLow != nil && !oneOf(*cam.CodecLow, "auto", "h264", "h265") {
			return fmt.Errorf("config: camera %q invalid codec_low %q (must be auto, h264 or h265)", name, *cam.CodecLow)
		}
	}
	return nil
}

func validateSettings(who, fallbackMode, codec, transport, audio string, fps int, retry, timeout, stall, dial time.Duration) error {
	if !oneOf(fallbackMode, "offline", "last_frame", "none") {
		return fmt.Errorf("config: %s: invalid fallback_mode %q (must be offline, last_frame, or none)", who, fallbackMode)
	}
	if !oneOf(codec, "auto", "h264", "h265") {
		return fmt.Errorf("config: %s: invalid codec %q (must be auto, h264 or h265)", who, codec)
	}
	if !oneOf(transport, "tcp", "udp", "auto") {
		return fmt.Errorf("config: %s: invalid transport %q (must be tcp, udp or auto)", who, transport)
	}
	if !oneOf(audio, "auto", "none") {
		return fmt.Errorf("config: %s: invalid audio %q (must be auto or none)", who, audio)
	}
	if fps < 1 || fps > 60 {
		return fmt.Errorf("config: %s: fallback_fps %d out of range (1-60)", who, fps)
	}
	if retry <= 0 {
		return fmt.Errorf("config: %s: retry_interval must be positive", who)
	}
	if timeout <= 0 {
		return fmt.Errorf("config: %s: timeout must be positive", who)
	}
	if stall <= 0 {
		return fmt.Errorf("config: %s: stall_timeout must be positive", who)
	}
	if dial < 0 {
		return fmt.Errorf("config: %s: dial_timeout must not be negative", who)
	}
	return nil
}
