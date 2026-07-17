package main

import (
	"flag"
	"fmt"
	"log"
	"os"
)

var usage = `Usage: pinstrel <command> [options]

Commands:
    daemon    Start the background streaming daemon
    start     Signal the daemon to join voice and start streaming
    stop      Signal the daemon to stop streaming and leave voice

Options:
	--config  Path to config.toml (default: config.toml, fallback: /etc/pinstrel.toml`

func printUsage() {
	fmt.Println(usage)
}

func daemon(cfg *Config) {
	if cfg.DiscordToken == "" {
		log.Fatal("Error: DISCORD_TOKEN is required in config for daemon mode")
	}
	if cfg.ChannelID == "" {
		log.Fatal("Error: DISCORD_CHANNEL_ID is required in config for daemon mode")
	}

	daemon, err := NewDaemon(cfg)
	if err != nil {
		log.Fatalf("Error initializing daemon: %v", err)
	}

	if err := daemon.Start(); err != nil {
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
		printUsage()
		os.Exit(1)
	}

	subcommand := os.Args[1]

	fs := flag.NewFlagSet(subcommand, flag.ExitOnError)
	configPath := *fs.String("config", "/etc/pinstrel.toml", "path to pinstrel TOML configuration file")

	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("Error parsing flags: %v", err)
	}

	log.Printf("Loading Pinstrel configuration from: %s", configPath)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Error loading Pinstrel configuration from %s: %v", configPath, err)
	}

	switch subcommand {
	case "daemon":
		daemon(cfg)

	case "start":
		start(cfg)

	case "stop":
		stop(cfg)

	default:
		printUsage()
		os.Exit(1)
	}
}
