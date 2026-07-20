package discord

import (
	"errors"
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
// methods and lets the test control ResolveUserVoiceState's and JoinChannel's
// behavior to exercise the daemon's stream state machine. RegisterSlashCommands
// is a no-op here — the daemon test only cares that the call succeeds so
// daemon.Run can proceed.
//
// ResolveUserVoiceState honors resolveGuild/resolveChannel when resolveErr is
// nil, mirroring the real Discord adapter's contract: a non-empty channelID
// implies the user is in voice in that guild. With resolveErr set, the field
// is returned verbatim (use ErrUserNotInVoice / ErrUserSharesNoGuild).
type fakeSession struct {
	joinErr       error
	joinDelay     time.Duration
	joinConn      *fakeConnection
	opens         int
	closes        int
	diagnostics   int
	resolveGuild  string
	resolveChannel string
	resolveErr    error
}

func (s *fakeSession) Open() error {
	s.opens++
	return nil
}

func (s *fakeSession) Close() error {
	s.closes++
	return nil
}

// ResolveUserVoiceState mirrors Discord.ResolveUserVoiceState's contract for
// the daemon tests. A non-nil resolveErr short-circuits (used to simulate
// ErrUserNotInVoice / ErrUserSharesNoGuild); otherwise the configured
// (resolveGuild, resolveChannel) pair is returned — tests that want a
// successful "user is in voice" path set resolveChannel to a non-empty
// channel ID and resolveGuild to the matching guild.
func (s *fakeSession) ResolveUserVoiceState(userID string) (string, string, error) {
	if s.resolveErr != nil {
		return "", "", s.resolveErr
	}
	if s.resolveChannel == "" {
		// No channel configured → simulate "user is not in voice" using
		// the same sentinel the real adapter returns.
		return "", "", ErrUserNotInVoice
	}
	return s.resolveGuild, s.resolveChannel, nil
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

// RegisterSlashCommands is a no-op for the fake; the daemon tests don't
// exercise slash dispatch, they only require Session to be satisfiable.
func (s *fakeSession) RegisterSlashCommands(_ []Command) error { return nil }

// TestDiscordImplementsSession confirms *Discord satisfies the Session
// interface and *discordConnection satisfies Connection at compile time. This
// test exists so that an interface drift is caught by `go test` rather than
// at the call site in the daemon package.
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

// TestFakeSessionResolveUserVoiceState exercises the fakeSession's
// ResolveUserVoiceState contract: happy path returns the configured pair,
// an empty resolveChannel produces ErrUserNotInVoice, and an explicit
// resolveErr is returned verbatim (the daemon tests use this to simulate
// ErrUserSharesNoGuild and other adapter-level failures).
func TestFakeSessionResolveUserVoiceState(t *testing.T) {
	// Happy path: user is in a voice channel.
	s := &fakeSession{resolveGuild: "guild-123", resolveChannel: "chan-456"}
	gGuild, gChan, err := s.ResolveUserVoiceState("user-789")
	if err != nil {
		t.Fatalf("ResolveUserVoiceState: %v", err)
	}
	if gGuild != "guild-123" || gChan != "chan-456" {
		t.Fatalf("expected (guild-123, chan-456), got (%s, %s)", gGuild, gChan)
	}

	// Not-in-voice path: empty channel triggers the sentinel.
	s2 := &fakeSession{}
	if _, _, err := s2.ResolveUserVoiceState("user-789"); !errors.Is(err, ErrUserNotInVoice) {
		t.Fatalf("expected ErrUserNotInVoice, got %v", err)
	}

	// Explicit adapter error propagates verbatim (used by daemon tests to
	// simulate ErrUserSharesNoGuild).
	s3 := &fakeSession{resolveErr: ErrUserSharesNoGuild}
	if _, _, err := s3.ResolveUserVoiceState("user-789"); !errors.Is(err, ErrUserSharesNoGuild) {
		t.Fatalf("expected ErrUserSharesNoGuild, got %v", err)
	}
}