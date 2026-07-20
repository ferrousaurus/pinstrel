package daemon

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"pinstrel/internal/audio"
	"pinstrel/internal/config"
	"pinstrel/internal/discord"
)

// --- test mocks ---

// mockConn is a test discord.Connection. It records Speaking/Disconnect and
// exposes a buffered OpusSend the test can drain.
type mockConn struct {
	mu           sync.Mutex
	speaking     []bool
	disconnected bool
	opusSend     chan []byte
}

func newMockConn(buf int) *mockConn {
	return &mockConn{opusSend: make(chan []byte, buf)}
}

func (c *mockConn) Speaking(on bool) error {
	c.mu.Lock()
	c.speaking = append(c.speaking, on)
	c.mu.Unlock()
	return nil
}

func (c *mockConn) Disconnect() error {
	c.mu.Lock()
	c.disconnected = true
	c.mu.Unlock()
	return nil
}

func (c *mockConn) OpusSend() chan<- []byte { return c.opusSend }

func (c *mockConn) speakingCalls() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]bool, len(c.speaking))
	copy(cp, c.speaking)
	return cp
}

func (c *mockConn) wasDisconnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disconnected
}

// mockSession is a test discord.Session. It lets the test control
// ResolveUserVoiceState's and JoinChannel's behavior (success/error/blocking)
// to exercise the stream state machine. Set resolveGuild/resolveChannel to
// non-empty strings for a successful "user is in voice" resolution; leave
// resolveChannel empty (or set resolveErr to one of the sentinels) to simulate
// the hard-reject paths.
type mockSession struct {
	joinErr        error
	joinConn       *mockConn
	joinDelay      time.Duration
	resolveGuild   string
	resolveChannel string
	resolveErr     error
}

func (s *mockSession) Open() error     { return nil }
func (s *mockSession) Close() error    { return nil }
func (s *mockSession) AddDiagnostics() {}

// ResolveUserVoiceState mirrors the real adapter's contract. With resolveErr
// set, returns the error verbatim (used to simulate ErrUserNotInVoice via the
// sentinel directly, or ErrUserSharesNoGuild). With resolveChannel empty,
// falls through to the not-in-voice sentinel. Otherwise returns the
// configured guild+channel pair.
func (s *mockSession) ResolveUserVoiceState(userID string) (string, string, error) {
	if s.resolveErr != nil {
		return "", "", s.resolveErr
	}
	if s.resolveChannel == "" {
		return "", "", discord.ErrUserNotInVoice
	}
	return s.resolveGuild, s.resolveChannel, nil
}

func (s *mockSession) JoinChannel(guildID, channelID string, mute, deaf bool) (discord.Connection, error) {
	if s.joinDelay > 0 {
		time.Sleep(s.joinDelay)
	}
	if s.joinErr != nil {
		return nil, s.joinErr
	}
	if s.joinConn == nil {
		return nil, errors.New("mockSession: no joinConn configured")
	}
	return s.joinConn, nil
}

// RegisterSlashCommands is a no-op for the mock; the daemon tests don't
// exercise slash dispatch, only that Session is satisfiable so daemon.Run
// can proceed past the registration step.
func (s *mockSession) RegisterSlashCommands(_ []discord.Command) error { return nil }

// mockOpener returns a pre-built reader (no blocking FIFO open).
type mockOpener struct {
	r   io.ReadCloser
	err error
}

func (o *mockOpener) Open() (io.ReadCloser, error) { return o.r, o.err }

// nopReadCloser wraps a reader with a no-op Close.
type nopReadCloser struct{ io.Reader }

func (nopReadCloser) Close() error { return nil }

// --- helpers ---

func testConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.DiscordToken = "test-token"
	cfg.DiscordUserID = "test-user"
	cfg.PipePath = "/tmp/test-pipe"
	cfg.SocketPath = "/tmp/test-sock"
	cfg.VoiceReadyTimeout = 5
	return cfg
}

// inVoiceSession returns a mockSession configured to resolve the configured
// test-user into a fixed channel/guild and join with the supplied conn.
func inVoiceSession(conn *mockConn) *mockSession {
	return &mockSession{
		resolveGuild:   "guild-test",
		resolveChannel: "test-channel",
		joinConn:       conn,
	}
}

// waitDone polls until the daemon is no longer streaming/joining, with a
// timeout. streamLoop runs on its own goroutine; the test needs to wait for
// its cleanup to complete before asserting state.
func waitDone(t *testing.T, d *Daemon, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d.activeMu.Lock()
		done := !d.isStreaming && !d.joining
		d.activeMu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("daemon still streaming/joining after %s", timeout)
}

// removeIfExists deletes a file/FIFO at path, ignoring "not exist" errors.
// Used by the hard-reject tests to clean up any leftover FIFO from a prior
// run before the test asserts that the daemon did NOT create one.
func removeIfExists(path string) error {
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// --- tests ---

func TestNew_RequiresToken(t *testing.T) {
	cfg := testConfig()
	cfg.DiscordToken = ""
	_, err := New(cfg, &mockSession{})
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
}

func TestNew_RequiresUserID(t *testing.T) {
	// Empty DiscordUserID must be a hard error: the daemon has nobody to
	// follow.
	cfg := testConfig()
	cfg.DiscordUserID = ""
	if _, err := New(cfg, &mockSession{}); err == nil {
		t.Fatal("expected error for nil DiscordUserID, got nil")
	}
}

func TestHandleStart_Idempotent(t *testing.T) {
	cfg := testConfig()
	cfg.VoiceReadyTimeout = 1
	conn := newMockConn(8)
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(make([]byte, audio.SourceFrameBytes))}}
	d, err := NewWithOpener(cfg, inVoiceSession(conn), opener)
	if err != nil {
		t.Fatalf("NewWithOpener: %v", err)
	}

	if err := d.HandleStart(); err != nil {
		t.Fatalf("HandleStart: %v", err)
	}
	// Second start should be a no-op (already streaming/joining).
	if err := d.HandleStart(); err != nil {
		t.Fatalf("second HandleStart: %v", err)
	}
	waitDone(t, d, 3*time.Second)

	// Only one streamLoop should have run: exactly one Speaking(true).
	calls := conn.speakingCalls()
	trues := 0
	for _, v := range calls {
		if v {
			trues++
		}
	}
	if trues != 1 {
		t.Fatalf("expected exactly 1 Speaking(true), got %d (calls: %v)", trues, calls)
	}
}

func TestHandleStop_WhenIdle(t *testing.T) {
	cfg := testConfig()
	d, err := NewWithOpener(cfg, inVoiceSession(newMockConn(1)), &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(nil)}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// stop before any start should be a no-op, not an error.
	if err := d.HandleStop(); err != nil {
		t.Fatalf("HandleStop when idle: %v", err)
	}
}

func TestStreamLoop_HappyPath_EOF(t *testing.T) {
	// One PCM frame of zeros, then EOF → streamLoop exits and cleans up.
	cfg := testConfig()
	cfg.VoiceReadyTimeout = 2
	conn := newMockConn(8)
	frame := make([]byte, audio.SourceFrameBytes)
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(frame)}}
	d, err := NewWithOpener(cfg, inVoiceSession(conn), opener)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.HandleStart(); err != nil {
		t.Fatalf("HandleStart: %v", err)
	}
	waitDone(t, d, 3*time.Second)

	// After EOF cleanup: vc disconnected, flags clear.
	if !conn.wasDisconnected() {
		t.Error("expected vc.Disconnect() to be called after EOF")
	}
	// Speaking sequence should be [true, false] (on for encode loop, off on cleanup).
	calls := conn.speakingCalls()
	if len(calls) < 2 {
		t.Fatalf("expected at least 2 Speaking calls, got %d: %v", len(calls), calls)
	}
	if calls[0] != true {
		t.Errorf("first Speaking call: expected true, got %v", calls[0])
	}
	if calls[len(calls)-1] != false {
		t.Errorf("last Speaking call: expected false, got %v", calls[len(calls)-1])
	}
}

func TestStreamLoop_JoinFailure(t *testing.T) {
	cfg := testConfig()
	cfg.VoiceReadyTimeout = 2
	conn := newMockConn(8) // should never be used
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(nil)}}
	d, err := NewWithOpener(cfg, &mockSession{
		resolveGuild:   "guild-test",
		resolveChannel: "test-channel",
		joinErr:        errors.New("discord down"),
	}, opener)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.HandleStart(); err != nil {
		t.Fatalf("HandleStart: %v", err)
	}
	waitDone(t, d, 3*time.Second)

	// Join failed: vc never connected, so Disconnect should not be needed,
	// but flags must be clear (waitDone already checks that). The conn was
	// never returned by JoinChannel, so it should not have been touched.
	if len(conn.speakingCalls()) != 0 {
		t.Errorf("expected no Speaking calls on failed join, got %v", conn.speakingCalls())
	}
}

func TestStreamLoop_PipeOpenFailure(t *testing.T) {
	cfg := testConfig()
	cfg.VoiceReadyTimeout = 2
	conn := newMockConn(8)
	opener := &mockOpener{err: errors.New("pipe broken")}
	d, err := NewWithOpener(cfg, inVoiceSession(conn), opener)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.HandleStart(); err != nil {
		t.Fatalf("HandleStart: %v", err)
	}
	waitDone(t, d, 3*time.Second)

	// Pipe failed after voice join: vc was connected then cleaned up.
	if !conn.wasDisconnected() {
		t.Error("expected vc.Disconnect() after pipe open failure")
	}
}

func TestStreamLoop_StopDuringJoin(t *testing.T) {
	// JoinChannel blocks for 500ms; stop fires during that window. The
	// stopCh ticker (100ms poll) should signal stop and streamLoop should
	// bail without waiting for the join.
	cfg := testConfig()
	cfg.VoiceReadyTimeout = 5 // generous; stop should win, not the deadline
	conn := newMockConn(8)
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(nil)}}
	d, err := NewWithOpener(cfg, &mockSession{
		resolveGuild:   "guild-test",
		resolveChannel: "test-channel",
		joinConn:       conn,
		joinDelay:      500 * time.Millisecond,
	}, opener)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.HandleStart(); err != nil {
		t.Fatalf("HandleStart: %v", err)
	}

	// Wait for the stopCh ticker to pick up the stop.
	time.Sleep(150 * time.Millisecond)
	if err := d.HandleStop(); err != nil {
		t.Fatalf("HandleStop: %v", err)
	}
	waitDone(t, d, 3*time.Second)
}

// drainConn extracts all buffered Opus packets from a mockConn without
// blocking. Returns nil if the channel is empty.
func drainConn(c *mockConn) [][]byte {
	var out [][]byte
	for {
		select {
		case pkt := <-c.opusSend:
			out = append(out, pkt)
		default:
			return out
		}
	}
}

// TestHandleStart_UserNotInVoice verifies the Question 5 hard-reject contract:
// when ResolveUserVoiceState returns ErrUserNotInVoice, HandleStart returns
// the sentinel error, no streamLoop is spawned, and no FIFO is created.
func TestHandleStart_UserNotInVoice(t *testing.T) {
	cfg := testConfig()
	// Point the pipe path at a tmp file we can Stat to confirm no FIFO was
	// created on the error path.
	cfg.PipePath = "/tmp/test-pinstrel-pipe-notcreated"
	// Defensive: ensure no leftover from a prior run confuses the Stat.
	_ = removeIfExists(cfg.PipePath)

	conn := newMockConn(8) // should never be used
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(nil)}}
	sess := &mockSession{
		resolveErr: discord.ErrUserNotInVoice,
		joinConn:   conn,
	}
	d, err := NewWithOpener(cfg, sess, opener)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.HandleStart(); !errors.Is(err, discord.ErrUserNotInVoice) {
		t.Fatalf("expected ErrUserNotInVoice, got %v", err)
	}

	// streamLoop must not have spawned → flags stay clear.
	d.activeMu.Lock()
	streaming := d.isStreaming
	joining := d.joining
	d.activeMu.Unlock()
	if streaming || joining {
		t.Errorf("expected idle flags after hard-reject, got streaming=%v joining=%v", streaming, joining)
	}

	// No connection should have been touched.
	if len(conn.speakingCalls()) != 0 {
		t.Errorf("expected no Speaking calls, got %v", conn.speakingCalls())
	}
	if conn.wasDisconnected() {
		t.Error("expected Disconnect() NOT to be called on hard-reject")
	}

	// No FIFO should have been created on the error path.
	if _, err := os.Stat(cfg.PipePath); !os.IsNotExist(err) {
		t.Errorf("expected FIFO to NOT exist after hard-reject; Stat err=%v", err)
	}
}

// TestHandleStart_UserSharesNoGuild verifies the same hard-reject contract for
// the "bot is in zero guilds" precondition from Question 7.
func TestHandleStart_UserSharesNoGuild(t *testing.T) {
	cfg := testConfig()
	cfg.PipePath = "/tmp/test-pinstrel-pipe-noguild"
	_ = removeIfExists(cfg.PipePath)

	conn := newMockConn(8)
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(nil)}}
	sess := &mockSession{
		resolveErr: discord.ErrUserSharesNoGuild,
		joinConn:   conn,
	}
	d, err := NewWithOpener(cfg, sess, opener)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.HandleStart(); !errors.Is(err, discord.ErrUserSharesNoGuild) {
		t.Fatalf("expected ErrUserSharesNoGuild, got %v", err)
	}

	d.activeMu.Lock()
	streaming := d.isStreaming
	joining := d.joining
	d.activeMu.Unlock()
	if streaming || joining {
		t.Errorf("expected idle flags after hard-reject, got streaming=%v joining=%v", streaming, joining)
	}

	if _, err := os.Stat(cfg.PipePath); !os.IsNotExist(err) {
		t.Errorf("expected FIFO to NOT exist after hard-reject; Stat err=%v", err)
	}
}