package voice

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeConnection is a test double for Connection. It records Speaking and
// Disconnect calls and exposes an OpusSend channel the test can drain.
type fakeConnection struct {
	mu           sync.Mutex
	speaking     []bool
	disconnected bool
	opusSend     chan []byte
}

func newFakeConnection(buf int) *fakeConnection {
	return &fakeConnection{opusSend: make(chan []byte, buf)}
}

func (c *fakeConnection) Speaking(on bool) error {
	c.mu.Lock()
	c.speaking = append(c.speaking, on)
	c.mu.Unlock()
	return nil
}

func (c *fakeConnection) Disconnect() error {
	c.mu.Lock()
	c.disconnected = true
	c.mu.Unlock()
	return nil
}

func (c *fakeConnection) OpusSend() chan<- []byte { return c.opusSend }

func (c *fakeConnection) speakingCalls() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]bool, len(c.speaking))
	copy(cp, c.speaking)
	return cp
}

func (c *fakeConnection) wasDisconnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disconnected
}

// fakeSession is a test double for Session. It stubs the gateway lifecycle
// methods and lets the test control JoinChannel's behavior (success, error,
// or delay) to exercise the daemon's stream state machine.
type fakeSession struct {
	joinErr     error
	joinDelay   time.Duration
	joinConn    *fakeConnection
	opens       int
	closes      int
	diagnostics int
	resolvedID  string
	resolveErr  error
}

func (s *fakeSession) Open() error {
	s.opens++
	return nil
}

func (s *fakeSession) Close() error {
	s.closes++
	return nil
}

func (s *fakeSession) ResolveGuild(channelID string) (string, error) {
	if s.resolveErr != nil {
		return "", s.resolveErr
	}
	if s.resolvedID != "" {
		return s.resolvedID, nil
	}
	return "guild-fake", nil
}

func (s *fakeSession) AddDiagnostics() { s.diagnostics++ }

func (s *fakeSession) JoinChannel(guildID, channelID string, mute, deaf bool) (Connection, error) {
	if s.joinDelay > 0 {
		time.Sleep(s.joinDelay)
	}
	if s.joinErr != nil {
		return nil, s.joinErr
	}
	if s.joinConn == nil {
		return nil, errors.New("fakeSession: no joinConn configured")
	}
	return s.joinConn, nil
}

// TestDiscordJoinInterface confirms *Discord satisfies the Session interface
// and *discordConnection satisfies Connection at compile time. This test
// exists so that an interface drift is caught by `go test` rather than at
// the call site in the daemon package.
func TestDiscordImplementsSession(t *testing.T) {
	var _ Session = (*Discord)(nil)
	var _ Connection = (*discordConnection)(nil)

	// fakeSession must also satisfy Session for the daemon tests.
	var _ Session = (*fakeSession)(nil)
}

func TestFakeConnection(t *testing.T) {
	conn := newFakeConnection(2)
	if err := conn.Speaking(true); err != nil {
		t.Fatalf("Speaking: %v", err)
	}
	if err := conn.Speaking(false); err != nil {
		t.Fatalf("Speaking: %v", err)
	}
	if err := conn.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	calls := conn.speakingCalls()
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("speaking calls: expected [true false], got %v", calls)
	}
	if !conn.wasDisconnected() {
		t.Fatal("expected disconnected")
	}

	// OpusSend channel should accept packets up to its buffer.
	conn.opusSend <- []byte{1, 2, 3}
	conn.opusSend <- []byte{4, 5, 6}
	select {
	case conn.opusSend <- []byte{7}:
		t.Fatal("expected channel to be full (cap 2)")
	default:
	}
}

func TestFakeSessionResolveGuild(t *testing.T) {
	s := &fakeSession{resolvedID: "guild-123"}
	got, err := s.ResolveGuild("chan-456")
	if err != nil {
		t.Fatalf("ResolveGuild: %v", err)
	}
	if got != "guild-123" {
		t.Fatalf("expected guild-123, got %s", got)
	}

	s2 := &fakeSession{resolveErr: fmt.Errorf("boom")}
	if _, err := s2.ResolveGuild("x"); err == nil {
		t.Fatal("expected error, got nil")
	}
}
