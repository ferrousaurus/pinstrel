package daemon

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"pinstrel/internal/audio"
	"pinstrel/internal/config"
	"pinstrel/internal/voice"
)

// --- test mocks ---

// mockConn is a test voice.Connection. It records Speaking/Disconnect and
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

// mockSession is a test voice.Session. It lets the test control JoinChannel
// behavior (success/error/blocking) to exercise the stream state machine.
type mockSession struct {
	joinErr   error
	joinConn  *mockConn
	joinDelay time.Duration
}

func (s *mockSession) Open() error     { return nil }
func (s *mockSession) Close() error    { return nil }
func (s *mockSession) AddDiagnostics() {}
func (s *mockSession) ResolveGuild(channelID string) (string, error) {
	return "guild-test", nil
}
func (s *mockSession) JoinChannel(guildID, channelID string, mute, deaf bool) (voice.Connection, error) {
	if s.joinDelay > 0 {
		time.Sleep(s.joinDelay)
	}
	if s.joinErr != nil {
		return nil, s.joinErr
	}
	return s.joinConn, nil
}

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
	return &config.Config{
		DiscordToken:      "test-token",
		ChannelID:         "test-channel",
		Bitrate:           128000,
		PipePath:          "/tmp/test-pipe",
		SocketPath:        "/tmp/test-sock",
		VoiceReadyTimeout: 5,
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

// --- tests ---

func TestNew_RequiresToken(t *testing.T) {
	cfg := testConfig()
	cfg.DiscordToken = ""
	_, err := New(cfg, &mockSession{})
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
}

func TestNew_RequiresChannelID(t *testing.T) {
	cfg := testConfig()
	cfg.ChannelID = ""
	_, err := New(cfg, &mockSession{})
	if err == nil {
		t.Fatal("expected error for empty channel ID, got nil")
	}
}

func TestHandleStart_Idempotent(t *testing.T) {
	cfg := testConfig()
	cfg.VoiceReadyTimeout = 1
	conn := newMockConn(8)
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(make([]byte, audio.FrameBytes))}}
	d, err := NewWithOpener(cfg, &mockSession{joinConn: conn}, opener)
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
	d, err := NewWithOpener(cfg, &mockSession{joinConn: newMockConn(1)}, &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(nil)}})
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
	frame := make([]byte, audio.FrameBytes)
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(frame)}}
	d, err := NewWithOpener(cfg, &mockSession{joinConn: conn}, opener)
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
	d, err := NewWithOpener(cfg, &mockSession{joinErr: errors.New("discord down")}, opener)
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
	d, err := NewWithOpener(cfg, &mockSession{joinConn: conn}, opener)
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
		joinConn:  conn,
		joinDelay: 500 * time.Millisecond,
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
