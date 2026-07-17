package ipc

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHandler records the order of HandleStart/HandleStop calls and lets the
// test control each method's return error.
type fakeHandler struct {
	mu       sync.Mutex
	starts   int
	stops    int
	startErr error
	stopErr  error
}

func (h *fakeHandler) HandleStart() error {
	h.mu.Lock()
	h.starts++
	err := h.startErr
	h.mu.Unlock()
	return err
}

func (h *fakeHandler) HandleStop() error {
	h.mu.Lock()
	h.stops++
	err := h.stopErr
	h.mu.Unlock()
	return err
}

// dial sends a command to a Unix socket and returns the response line.
func dial(socketPath, cmd string) (string, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\n"), nil
}

// dialRetry dials the server with retries until it succeeds or times out.
// The server's ListenAndServe runs on a goroutine, so the first few dial
// attempts may fail before the listener is ready; retrying avoids a flaky
// sleep-based readiness check.
func dialRetry(t *testing.T, socketPath, cmd string) string {
	t.Helper()
	var lastErr error
	for i := 0; i < 200; i++ {
		resp, err := dial(socketPath, cmd)
		if err == nil {
			return resp
		}
		lastErr = err
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("could not dial %s after 200 retries: %v", socketPath, lastErr)
	return ""
}

func startServer(t *testing.T, socketPath string, h CommandHandler) {
	t.Helper()
	srv := NewServer(socketPath, h)
	go func() { _ = srv.ListenAndServe() }()
}

func TestServerStartStop(t *testing.T) {
	dir := t.TempDir()
	socketPath := dir + "/s.sock"
	h := &fakeHandler{}
	startServer(t, socketPath, h)

	if resp := dialRetry(t, socketPath, "start"); resp != "OK" {
		t.Fatalf("start response: expected OK, got %q", resp)
	}
	if h.starts != 1 {
		t.Fatalf("expected 1 start call, got %d", h.starts)
	}

	if resp := dialRetry(t, socketPath, "stop"); resp != "OK" {
		t.Fatalf("stop response: expected OK, got %q", resp)
	}
	if h.stops != 1 {
		t.Fatalf("expected 1 stop call, got %d", h.stops)
	}
}

func TestServerErrorResponse(t *testing.T) {
	dir := t.TempDir()
	socketPath := dir + "/s.sock"
	h := &fakeHandler{startErr: errors.New("boom")}
	startServer(t, socketPath, h)

	if resp := dialRetry(t, socketPath, "start"); resp != "ERR: boom" {
		t.Fatalf("expected 'ERR: boom', got %q", resp)
	}
}

func TestServerUnknownCommand(t *testing.T) {
	dir := t.TempDir()
	socketPath := dir + "/s.sock"
	h := &fakeHandler{}
	startServer(t, socketPath, h)

	if resp := dialRetry(t, socketPath, "bogus"); resp != "ERR: Unknown command" {
		t.Fatalf("expected 'ERR: Unknown command', got %q", resp)
	}
	if h.starts != 0 || h.stops != 0 {
		t.Fatalf("expected no handler calls, got start=%d stop=%d", h.starts, h.stops)
	}
}
