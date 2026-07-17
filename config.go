package main

import (
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

// DefaultConfig returns a Config initialized with default values.
func DefaultConfig() *Config {
	return &Config{
		Bitrate:           128000,
		PipePath:          "/tmp/shairport-sync-audio",
		SocketPath:        "/tmp/pinstrel.sock",
		VoiceReadyTimeout: 30,
	}
}

// LoadConfig reads the TOML file at path and unmarshals it.
// If the file does not exist, it returns the default configuration.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
