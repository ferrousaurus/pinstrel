package main

import (
	"flag"
	"fmt"
	"log"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	subcommand := os.Args[1]

	// Set up command-specific flag sets
	fs := flag.NewFlagSet(subcommand, flag.ExitOnError)
	configPath := fs.String("config", "config.toml", "path to pinstrel TOML configuration file")

	// Parse flags ignoring the subcommand
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("Error parsing flags: %v", err)
	}

	// Elegant path fallback:
	// If the default "config.toml" does not exist locally,
	// check if "/etc/pinstrel.toml" exists and use it as a fallback.
	resolvedPath := *configPath
	if resolvedPath == "config.toml" {
		if _, err := os.Stat("config.toml"); os.IsNotExist(err) {
			if _, err := os.Stat("/etc/pinstrel.toml"); err == nil {
				resolvedPath = "/etc/pinstrel.toml"
			}
		}
	}

	cfg, err := LoadConfig(resolvedPath)
	if err != nil {
		log.Fatalf("Error loading configuration from %s: %v", resolvedPath, err)
	}

	switch subcommand {
	case "daemon":
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

		log.Printf("Starting pinstrel daemon using config: %s", resolvedPath)
		if err := daemon.Start(); err != nil {
			log.Fatalf("Daemon runtime error: %v", err)
		}

	case "start":
		log.Printf("Sending start command to daemon socket: %s", cfg.SocketPath)
		if err := SendIPCCommand(cfg.SocketPath, "start"); err != nil {
			log.Fatalf("CLI Error: %v", err)
		}

	case "stop":
		log.Printf("Sending stop command to daemon socket: %s", cfg.SocketPath)
		if err := SendIPCCommand(cfg.SocketPath, "stop"); err != nil {
			log.Fatalf("CLI Error: %v", err)
		}

	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: pinstrel <command> [options]")
	fmt.Println("\nCommands:")
	fmt.Println("  daemon    Start the background streaming daemon")
	fmt.Println("  start     Signal the daemon to join voice and start streaming")
	fmt.Println("  stop      Signal the daemon to stop streaming and leave voice")
	fmt.Println("\nOptions:")
	fmt.Println("  --config  Path to config.toml (default: config.toml, fallback: /etc/pinstrel.toml)")
}
