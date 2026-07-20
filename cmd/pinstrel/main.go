// Command pinstrel is the AirPlay-to-Discord streaming daemon and CLI.
// It has three subcommands: daemon (long-running), start/stop (one-shot IPC
// clients invoked by shairport-sync hooks).
package main

import (
	"fmt"
	"log"
	"os"

	"pinstrel/internal/config"
	"pinstrel/internal/daemon"
	"pinstrel/internal/discord"
	"pinstrel/internal/ipc"
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

func runDaemon(cfg *config.Config) {
	v, err := discord.New(cfg.DiscordToken)
	if err != nil {
		log.Fatalf("Error initializing Discord session: %v", err)
	}
	d, err := daemon.New(cfg, v)
	if err != nil {
		log.Fatalf("Error initializing daemon: %v", err)
	}
	if err := d.Run(); err != nil {
		log.Fatalf("Daemon runtime error: %v", err)
	}
}

func runStart(cfg *config.Config) {
	log.Printf("Sending start command to daemon socket: %s", cfg.SocketPath)
	if err := ipc.Send(cfg.SocketPath, "start"); err != nil {
		log.Fatalf("CLI Error: %v", err)
	}
}

func runStop(cfg *config.Config) {
	log.Printf("Sending stop command to daemon socket: %s", cfg.SocketPath)
	if err := ipc.Send(cfg.SocketPath, "stop"); err != nil {
		log.Fatalf("CLI Error: %v", err)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}

	log.Printf("Loading pinstrel configuration from: %s", configPath)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Error loading pinstrel configuration from %s: %v", configPath, err)
	}

	switch os.Args[1] {
	case "daemon":
		runDaemon(cfg)
	case "start":
		runStart(cfg)
	case "stop":
		runStop(cfg)
	default:
		fmt.Print(usage)
		os.Exit(1)
	}
}
