package ipc

import (
	"fmt"
	"net"
)

// Send dials the pinstrel daemon's Unix socket and sends a single command
// line, then reads and prints the daemon's response. Used by the one-shot
// CLI subcommands (pinstrel start / pinstrel stop) invoked by shairport-sync
// hooks.
func Send(socketPath, cmd string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("could not connect to pinstrel daemon at %s: %w. Is the daemon running?", socketPath, err)
	}
	defer conn.Close()

	if _, err = conn.Write([]byte(cmd + "\n")); err != nil {
		return fmt.Errorf("failed to write command to socket: %w", err)
	}

	// Read response (like "OK" or "ERR: ...").
	buf := make([]byte, 128)
	if n, err := conn.Read(buf); err == nil && n > 0 {
		fmt.Printf("Daemon response: %s", string(buf[:n]))
	}
	return nil
}
