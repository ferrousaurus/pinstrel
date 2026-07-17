// Package ipc implements the Unix-socket IPC protocol between the pinstrel
// daemon and the shairport-sync hooks (pinstrel start / pinstrel stop).
package ipc

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
)

// CommandHandler handles start/stop IPC commands dispatched from the Unix
// socket server. The daemon implements this interface; the IPC server holds
// it as a dependency so the dispatch logic is testable without a Daemon.
type CommandHandler interface {
	HandleStart() error
	HandleStop() error
}

// Server listens on a Unix domain socket and dispatches one-line commands
// ("start" / "stop") to a CommandHandler, writing "OK" or "ERR: ..." back to
// the client. It is the bridge between shairport-sync's
// run_this_before_play_begins / run_this_after_play_ends hooks and the daemon.
type Server struct {
	socketPath string
	handler    CommandHandler
}

// NewServer creates an IPC server bound to the given socket path.
func NewServer(socketPath string, handler CommandHandler) *Server {
	return &Server{socketPath: socketPath, handler: handler}
}

// SocketPath returns the filesystem path of the Unix socket.
func (s *Server) SocketPath() string { return s.socketPath }

// ListenAndServe binds the Unix socket, sets world-writable permissions so
// shairport-sync (which may run as a different user) can connect, and serves
// connections until a fatal Accept error. The socket is removed on return.
// The socket permissions (0666) must match the pipe permissions so a
// non-shared-user shairport can talk to both.
func (s *Server) ListenAndServe() error {
	_ = os.Remove(s.socketPath)

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on socket %s: %w", s.socketPath, err)
	}
	defer listener.Close()
	defer os.Remove(s.socketPath)

	if err := os.Chmod(s.socketPath, 0666); err != nil {
		log.Printf("Warning: failed to chmod socket %s: %v", s.socketPath, err)
	}

	log.Printf("pinstrel daemon started. Socket: %s", s.socketPath)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Error accepting socket connection: %v", err)
			continue
		}
		go s.handleConn(conn)
	}
}

// handleConn reads a single command line and dispatches it, writing a
// one-line response back. Unknown commands get an error response.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	cmd := scanner.Text()
	log.Printf("Received IPC command: %s", cmd)
	switch cmd {
	case "start":
		s.dispatch(conn, "start", s.handler.HandleStart)
	case "stop":
		s.dispatch(conn, "stop", s.handler.HandleStop)
	default:
		writeResponse(conn, "ERR: Unknown command")
	}
}

// dispatch invokes a handler method and writes the result to the client.
func (s *Server) dispatch(conn net.Conn, cmd string, fn func() error) {
	if err := fn(); err != nil {
		log.Printf("Error handling %q: %v", cmd, err)
		writeResponse(conn, fmt.Sprintf("ERR: %v", err))
		return
	}
	writeResponse(conn, "OK")
}

// writeResponse sends a single line to the IPC client, logging any write
// failure. The client (shairport hook) ignores the body but expects a line so
// its blocking read completes.
func writeResponse(conn net.Conn, msg string) {
	if _, err := conn.Write([]byte(msg + "\n")); err != nil {
		log.Printf("Failed to write IPC response: %v", err)
	}
}
