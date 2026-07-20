package ipc

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// Send dials the pinstrel daemon's Unix socket, sends a single command line,
// reads the daemon's response, prints it, and returns a non-nil error if the
// daemon reported a failure. The daemon's response protocol is a single
// line terminated by '\n': "OK" on success, or "ERR: <message>" on a
// handler-level failure (e.g. discord.ErrUserNotInVoice when the configured
// user is not in a voice channel at start time).
//
// Translating "ERR:" responses into non-nil errors is load-bearing: it makes
// `pinstrel start` exit non-zero on a rejected attempt so the
// shairport-sync run_this_before_play_begins hook aborts the AirPlay play
// rather than proceeding to open the (rejected) FIFO writer. Without this
// check, the daemon's hard-reject would surface only as a printed line and a
// zero exit, leaving the iOS client with silent dead air.
//
// Used by the one-shot CLI subcommands (pinstrel start / pinstrel stop)
// invoked by shairport-sync hooks.
func Send(socketPath, cmd string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("could not connect to pinstrel daemon at %s: %w. Is the daemon running?", socketPath, err)
	}
	defer conn.Close()

	if _, err = conn.Write([]byte(cmd + "\n")); err != nil {
		return fmt.Errorf("failed to write command to socket: %w", err)
	}

	// Read the daemon's single-line response. The daemon writes either "OK\n"
	// or "ERR: <message>\n" — see server.writeResponse. Echo the response for
	// interactive `pinstrel start` runs, then translate "ERR:" into a Go error
	// so the CLI's log.Fatalf surfaces a non-zero exit to shairport's hook.
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		// Transport-level read failure (daemon closed the connection without
		// writing, network reset, etc.). Treat as a hard error so the CLI
		// exits non-zero and the shairport hook aborts the play.
		return fmt.Errorf("failed to read daemon response: %w", err)
	}
	resp := strings.TrimRight(string(buf[:n]), "\n")
	fmt.Printf("Daemon response: %s\n", resp)
	if strings.HasPrefix(resp, "ERR:") {
		return errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR:")))
	}
	return nil
}