package main

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

func TestLoadConfig_ValidFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	tomlData := `
DISCORD_TOKEN = "test-token"
DISCORD_CHANNEL_ID = "test-channel-id"
BITRATE = 192000
PIPE_PATH = "/tmp/custom-audio"
SOCKET_PATH = "/tmp/custom-socket.sock"
`
	if err := os.WriteFile(configPath, []byte(tomlData), 0644); err != nil {
		t.Fatalf("Failed to write test config file: %v", err)
	}

	loadedCfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if loadedCfg.DiscordToken != "test-token" {
		t.Errorf("Expected DiscordToken 'test-token', got '%s'", loadedCfg.DiscordToken)
	}
	if loadedCfg.ChannelID != "test-channel-id" {
		t.Errorf("Expected ChannelID 'test-channel-id', got '%s'", loadedCfg.ChannelID)
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
