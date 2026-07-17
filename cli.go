package main

import (
	"fmt"
	"net"
)

// SendIPCCommand sends a command to the pinstral daemon via Unix domain socket.
func SendIPCCommand(socketPath, cmd string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("could not connect to pinstral daemon at %s: %w. Is the daemon running?", socketPath, err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte(cmd + "\n"))
	if err != nil {
		return fmt.Errorf("failed to write command to socket: %w", err)
	}

	// Read response (like "OK" or error message)
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		fmt.Printf("Daemon response: %s", string(buf[:n]))
	}

	return nil
}
