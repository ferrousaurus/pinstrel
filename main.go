package main

import (
	"fmt"
	"log"
	"os"
)

// configPath is the single canonical location of the pinstrel TOML config.
// pinstrel is a system daemon wired to shairport-sync via /etc; there is no
// per-invocation config flag, and a missing file is a hard error.
const configPath = "/etc/pinstrel.toml"

const usage = `Usage: pinstrel <command>

Commands:
  daemon    Start the background streaming daemon
  start     Signal the daemon to join voice and start streaming
  stop      Signal the daemon to stop streaming and leave voice
`

func daemon(cfg *Config) {
	if cfg.DiscordToken == "" {
		log.Fatal("Error: DISCORD_TOKEN is required in config for daemon mode")
	}
	if cfg.ChannelID == "" {
		log.Fatal("Error: DISCORD_CHANNEL_ID is required in config for daemon mode")
	}

	d, err := NewDaemon(cfg)
	if err != nil {
		log.Fatalf("Error initializing daemon: %v", err)
	}
	if err := d.Start(); err != nil {
		log.Fatalf("Daemon runtime error: %v", err)
	}
}

func start(cfg *Config) {
	log.Printf("Sending start command to daemon socket: %s", cfg.SocketPath)
	if err := SendIPCCommand(cfg.SocketPath, "start"); err != nil {
		log.Fatalf("CLI Error: %v", err)
	}
}

func stop(cfg *Config) {
	log.Printf("Sending stop command to daemon socket: %s", cfg.SocketPath)
	if err := SendIPCCommand(cfg.SocketPath, "stop"); err != nil {
		log.Fatalf("CLI Error: %v", err)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}

	log.Printf("Loading pinstrel configuration from: %s", configPath)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Error loading pinstrel configuration from %s: %v", configPath, err)
	}

	switch os.Args[1] {
	case "daemon":
		daemon(cfg)
	case "start":
		start(cfg)
	case "stop":
		stop(cfg)
	default:
		fmt.Print(usage)
		os.Exit(1)
	}
}
