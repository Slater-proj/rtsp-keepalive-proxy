// Package config handles loading and validating the YAML configuration.
package config

import (
	"fmt"
	"os"
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
}

// CameraDefaults holds default values applied to every camera.
type CameraDefaults struct {
	BatteryMode   bool          `yaml:"battery_mode"`
	RetryInterval time.Duration `yaml:"retry_interval"`
	Timeout       time.Duration `yaml:"timeout"`
	FallbackFPS   int           `yaml:"fallback_fps"`
	FallbackMode  string        `yaml:"fallback_mode"` // "offline", "last_frame", "none"
	Codec         string        `yaml:"codec"`
}

// CameraConfig holds per-camera settings. Pointer fields allow distinguishing
// "not set" from zero values so the defaults layer works correctly.
type CameraConfig struct {
	Source    string `yaml:"source"`
	SourceLow string `yaml:"source_low"`

	BatteryMode   *bool          `yaml:"battery_mode,omitempty"`
	RetryInterval *time.Duration `yaml:"retry_interval,omitempty"`
	Timeout       *time.Duration `yaml:"timeout,omitempty"`
	FallbackFPS   *int           `yaml:"fallback_fps,omitempty"`
	FallbackMode  *string        `yaml:"fallback_mode,omitempty"` // "offline", "last_frame", "none"
	Codec         *string        `yaml:"codec,omitempty"`
}

// Effective returns a resolved copy where nil fields are filled from defaults.
func (c CameraConfig) Effective(d CameraDefaults) ResolvedCamera {
	r := ResolvedCamera{
		Source:        c.Source,
		SourceLow:     c.SourceLow,
		BatteryMode:   d.BatteryMode,
		RetryInterval: d.RetryInterval,
		Timeout:       d.Timeout,
		FallbackFPS:   d.FallbackFPS,
		FallbackMode:  d.FallbackMode,
		Codec:         d.Codec,
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
	if c.FallbackFPS != nil {
		r.FallbackFPS = *c.FallbackFPS
	}
	if c.FallbackMode != nil {
		r.FallbackMode = *c.FallbackMode
	}
	if c.Codec != nil {
		r.Codec = *c.Codec
	}
	return r
}

// ResolvedCamera is a fully-resolved camera configuration with no nil fields.
type ResolvedCamera struct {
	Source        string
	SourceLow     string
	BatteryMode   bool
	RetryInterval time.Duration
	Timeout       time.Duration
	FallbackFPS   int
	FallbackMode  string // "offline", "last_frame", "none"
	Codec         string
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

// Parse parses raw YAML bytes into a validated Config.
func Parse(data []byte) (*Config, error) {
	expanded := os.ExpandEnv(string(data))

	cfg := &Config{}
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
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
	if d.FallbackFPS == 0 {
		d.FallbackFPS = 5
	}
	if d.FallbackMode == "" {
		d.FallbackMode = "offline"
	}
	if d.Codec == "" {
		d.Codec = "auto"
	}
}

func validate(cfg *Config) error {
	if len(cfg.Cameras) == 0 {
		return fmt.Errorf("config: no cameras defined")
	}
	switch cfg.Defaults.FallbackMode {
	case "offline", "last_frame", "none":
		// valid
	default:
		return fmt.Errorf("config: invalid default fallback_mode %q (must be offline, last_frame, or none)", cfg.Defaults.FallbackMode)
	}
	for name, cam := range cfg.Cameras {
		if cam.Source == "" {
			return fmt.Errorf("config: camera %q missing source URL", name)
		}
		if cam.FallbackMode != nil {
			switch *cam.FallbackMode {
			case "offline", "last_frame", "none":
			default:
				return fmt.Errorf("config: camera %q invalid fallback_mode %q", name, *cam.FallbackMode)
			}
		}
	}
	return nil
}
