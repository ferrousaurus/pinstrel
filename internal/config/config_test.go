package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Bitrate != 128000 {
		t.Errorf("Expected default Bitrate to be 128000, got %d", cfg.Bitrate)
	}
	if cfg.PipePath != "/tmp/shairport-sync-audio" {
		t.Errorf("Expected default PipePath to be /tmp/shairport-sync-audio, got %s", cfg.PipePath)
	}
	if cfg.SocketPath != "/tmp/pinstrel.sock" {
		t.Errorf("Expected default SocketPath to be /tmp/pinstrel.sock, got %s", cfg.SocketPath)
	}
	if cfg.VoiceReadyTimeout != 30 {
		t.Errorf("Expected default VoiceReadyTimeout to be 30 (seconds), got %d", cfg.VoiceReadyTimeout)
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	// A missing config file is a hard error: pinstrel is wired to a fixed
	// system path and must not silently substitute defaults.
	_, err := LoadConfig("non_existent_file.toml")
	if err == nil {
		t.Fatal("Expected error for non-existent config file, got nil")
	}
}

func writeTestConfig(t *testing.T, tomlData string) (*Config, string) {
	t.Helper()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(tomlData), 0644); err != nil {
		t.Fatalf("Failed to write test config file: %v", err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	return cfg, configPath
}

func TestLoadConfig_ValidFile(t *testing.T) {
	tomlData := `
DISCORD_TOKEN = "test-token"
DISCORD_CHANNEL_IDS = ["test-channel-id"]
BITRATE = 192000
PIPE_PATH = "/tmp/custom-audio"
SOCKET_PATH = "/tmp/custom-socket.sock"
`
	loadedCfg, _ := writeTestConfig(t, tomlData)

	if loadedCfg.DiscordToken != "test-token" {
		t.Errorf("Expected DiscordToken 'test-token', got '%s'", loadedCfg.DiscordToken)
	}
	if len(loadedCfg.ChannelIDs) != 1 || loadedCfg.ChannelIDs[0] != "test-channel-id" {
		t.Errorf("Expected ChannelIDs [\"test-channel-id\"], got %v", loadedCfg.ChannelIDs)
	}
	if loadedCfg.Bitrate != 192000 {
		t.Errorf("Expected Bitrate 192000, got %d", loadedCfg.Bitrate)
	}
	if loadedCfg.PipePath != "/tmp/custom-audio" {
		t.Errorf("Expected PipePath '/tmp/custom-audio', got '%s'", loadedCfg.PipePath)
	}
	if loadedCfg.SocketPath != "/tmp/custom-socket.sock" {
		t.Errorf("Expected SocketPath '/tmp/custom-socket.sock', got '%s'", loadedCfg.SocketPath)
	}
	// VoiceReadyTimeout is not set in the TOML, so it must fall back to the default.
	if loadedCfg.VoiceReadyTimeout != 30 {
		t.Errorf("Expected default VoiceReadyTimeout 30, got %d", loadedCfg.VoiceReadyTimeout)
	}
}

func TestLoadConfig_MultipleChannelIDs(t *testing.T) {
	// Multi-element array preserved in order, including across guilds.
	tomlData := `
DISCORD_TOKEN = "test-token"
DISCORD_CHANNEL_IDS = ["chan-a", "chan-b", "chan-c"]
`
	loadedCfg, _ := writeTestConfig(t, tomlData)

	if len(loadedCfg.ChannelIDs) != 3 {
		t.Fatalf("Expected 3 ChannelIDs, got %d: %v", len(loadedCfg.ChannelIDs), loadedCfg.ChannelIDs)
	}
	want := []string{"chan-a", "chan-b", "chan-c"}
	for i, w := range want {
		if loadedCfg.ChannelIDs[i] != w {
			t.Errorf("ChannelIDs[%d]: expected %q, got %q", i, w, loadedCfg.ChannelIDs[i])
		}
	}
}

func TestLoadConfig_ChannelIDsNormalization(t *testing.T) {
	// Surrounding whitespace is trimmed and empty entries are dropped at
	// LoadConfig time so a stray trailing comma or whitespace doesn't
	// masquerade as a valid channel at daemon startup.
	tomlData := `
DISCORD_TOKEN = "test-token"
DISCORD_CHANNEL_IDS = ["  chan-a  ", "chan-b", "", "  "]
`
	loadedCfg, _ := writeTestConfig(t, tomlData)

	if len(loadedCfg.ChannelIDs) != 2 {
		t.Fatalf("Expected 2 ChannelIDs after normalization, got %d: %v",
			len(loadedCfg.ChannelIDs), loadedCfg.ChannelIDs)
	}
	if loadedCfg.ChannelIDs[0] != "chan-a" {
		t.Errorf("ChannelIDs[0]: expected trimmed %q, got %q", "chan-a", loadedCfg.ChannelIDs[0])
	}
	if loadedCfg.ChannelIDs[1] != "chan-b" {
		t.Errorf("ChannelIDs[1]: expected %q, got %q", "chan-b", loadedCfg.ChannelIDs[1])
	}
}
