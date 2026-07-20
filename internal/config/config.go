package config

import (
	"fmt"
	"os"

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
	DiscordToken string `toml:"DISCORD_TOKEN"`
	// DiscordUserID is the snowflake of the Discord user pinstrel follows. On
	// each AirPlay start, pinstrel looks up this user's current voice channel
	// from its gateway state cache and joins it. If the user isn't in a voice
	// channel (or shares no guild with the bot), the start is rejected — see
	// discord.ErrUserNotInVoice / discord.ErrUserSharesNoGuild.
	DiscordUserID string `toml:"DISCORD_USER_ID"`
	Bitrate       int    `toml:"BITRATE"`
	PipePath      string `toml:"PIPE_PATH"`
	SocketPath    string `toml:"SOCKET_PATH"`
	// VoiceReadyTimeout is the wall-clock deadline (in seconds) bounding the
	// single Discord voice WS/UDP handshake for an AirPlay attempt. The
	// daemon's streamLoop re-checks this against a 0/negative value at runtime.
	VoiceReadyTimeout int `toml:"VOICE_READY_TIMEOUT"`
	// SourceSampleRate is the PCM sample rate (in Hz) expected on the audio FIFO
	// backend (PIPE_PATH). Defaults to 44100 Hz (AirPlay's native sample rate,
	// written by stock shairport-sync apt builds). Set to 48000 if shairport-sync
	// is built --with-ffmpeg and configured for 48 kHz output (disables resampling).
	SourceSampleRate int `toml:"SOURCE_SAMPLE_RATE"`
	// TapsPerPhase is the number of FIR taps per polyphase phase when resampling.
	// Defaults to 16. Range 4..128. Ignored when SOURCE_SAMPLE_RATE = 48000.
	TapsPerPhase int `toml:"TAPS_PER_PHASE"`
}

// DefaultConfig returns a Config initialized with default values. Partial TOML
// files are unmarshaled onto these defaults, so unset keys fall back to these.
func DefaultConfig() *Config {
	return &Config{
		Bitrate:           128000,
		PipePath:          "/tmp/shairport-sync-audio",
		SocketPath:        "/tmp/pinstrel.sock",
		VoiceReadyTimeout: DefaultVoiceReadyTimeout,
		SourceSampleRate:  44100,
		TapsPerPhase:      16,
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
