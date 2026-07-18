package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	// DefaultVoiceReadyTimeout is the deadline (in seconds) for the Discord
	// voice WS/UDP handshake before pinstrel abandons the join. Referenced by
	// both DefaultConfig (the TOML default) and the daemon's streamLoop guard
	// (which catches a misconfigured 0/negative value) so the two can't drift.
	DefaultVoiceReadyTimeout = 30
)

// Config holds the application configuration.
type Config struct {
	DiscordToken string   `toml:"DISCORD_TOKEN"`
	ChannelIDs   []string `toml:"DISCORD_CHANNEL_IDS"`
	Bitrate      int      `toml:"BITRATE"`
	PipePath     string   `toml:"PIPE_PATH"`
	SocketPath   string   `toml:"SOCKET_PATH"`
	// VoiceReadyTimeout is the shared wall-clock deadline (in seconds) bounding
	// all concurrent Discord voice joins for a single AirPlay attempt. Each
	// join races against this same budget (they run in parallel, not in
	// series), so for N channels every JoinChannel sub-goroutine gets the
	// full remaining window — not VOICE_READY_TIMEOUT/N. The daemon's
	// streamLoop re-checks this against a 0/negative value at runtime.
	VoiceReadyTimeout int `toml:"VOICE_READY_TIMEOUT"`
}

// DefaultConfig returns a Config initialized with default values. Partial TOML
// files are unmarshaled onto these defaults, so unset keys fall back to these.
func DefaultConfig() *Config {
	return &Config{
		Bitrate:           128000,
		PipePath:          "/tmp/shairport-sync-audio",
		SocketPath:        "/tmp/pinstrel.sock",
		VoiceReadyTimeout: DefaultVoiceReadyTimeout,
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
	// Normalize ChannelIDs: trim whitespace and drop empty entries so a stray
	// `["123", ""]` (e.g. trailing comma in the TOML) doesn't masquerade as a
	// valid channel at daemon startup. This is a parse-time concern, not
	// daemon logic — the daemon only needs to check len(ChannelIDs) > 0.
	cfg.ChannelIDs = normalizeChannelIDs(cfg.ChannelIDs)
	return cfg, nil
}

// normalizeChannelIDs trims surrounding whitespace from each channel ID and
// drops any elements that are empty after the trim. It preserves order and
// does not deduplicate — duplicate IDs are a deployment error best caught at
// join time (the second join will displace the first in Discord's voice state
// for the same guild, which is loud and debuggable).
func normalizeChannelIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
