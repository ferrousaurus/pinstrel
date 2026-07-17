package main

import (
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

// Config holds the application configuration.
type Config struct {
	DiscordToken      string `toml:"DISCORD_TOKEN"`
	ChannelID         string `toml:"DISCORD_CHANNEL_ID"`
	Bitrate           int    `toml:"BITRATE"`
	PipePath          string `toml:"PIPE_PATH"`
	SocketPath        string `toml:"SOCKET_PATH"`
	VoiceReadyTimeout int    `toml:"VOICE_READY_TIMEOUT"` // seconds; deadline for the voice WS/UDP handshake before we abandon the join
}

// DefaultConfig returns a Config initialized with default values. Partial TOML
// files are unmarshaled onto these defaults, so unset keys fall back to these.
func DefaultConfig() *Config {
	return &Config{
		Bitrate:           128000,
		PipePath:          "/tmp/shairport-sync-audio",
		SocketPath:        "/tmp/pinstrel.sock",
		VoiceReadyTimeout: 30,
	}
}

// LoadConfig reads the TOML file at path and unmarshals it onto DefaultConfig.
// A missing file is a hard error: pinstrel is a system daemon wired to a fixed
// config location (/etc/pinstrel.toml) and silently substituting defaults
// would mask a misconfigured deployment.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file %s does not exist: %w", path, err)
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config %s: %w", path, err)
	}
	return cfg, nil
}
