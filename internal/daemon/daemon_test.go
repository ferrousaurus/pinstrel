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
// behavior (success/error/blocking) to exercise the stream state machine. For
// multi-channel fan-out tests, set joinConnsByChannel to return a distinct
// connection per channel ID; otherwise JoinChannel returns the shared
// joinConn for every call (which is fine for the single-channel fixtures).
type mockSession struct {
	joinErr             error
	joinConn            *mockConn
	joinConnsByChannel  map[string]*mockConn
	joinDelay           time.Duration
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
	if c, ok := s.joinConnsByChannel[channelID]; ok {
		return c, nil
	}
	if s.joinConn == nil {
		// No connection configured for this channel → simulate a
		// real-world failure (e.g. permissions). Returning (nil, nil)
		// here would be a successful join with no connection, which
		// streamLoop can't do anything with — so we fail it explicitly.
		return nil, errors.New("mockSession: no connection configured for channel " + channelID)
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
		ChannelIDs:        []string{"test-channel"},
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

func TestNew_RequiresChannelIDs(t *testing.T) {
	// Empty list (or all-empty entries after normalization at LoadConfig
	// time, which the daemon never sees) must be a hard error: the daemon
	// has nothing to broadcast to.
	cfg := testConfig()
	cfg.ChannelIDs = nil
	if _, err := New(cfg, &mockSession{}); err == nil {
		t.Fatal("expected error for nil ChannelIDs, got nil")
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

func TestStreamLoop_MultiChannelFanOut(t *testing.T) {
	// Two configured channels get two distinct mock connections. Both should
	// join concurrently, both should receive Speaking(true), both should
	// receive Opus packets, and both should be Disconnected on cleanup.
	cfg := testConfig()
	cfg.ChannelIDs = []string{"chan-a", "chan-b"}
	cfg.VoiceReadyTimeout = 2

	connA := newMockConn(8)
	connB := newMockConn(8)
	sess := &mockSession{
		joinConnsByChannel: map[string]*mockConn{
			"chan-a": connA,
			"chan-b": connB,
		},
	}
	// One PCM frame then EOF — enough for one Opus packet to flow to both vcs.
	frame := make([]byte, audio.FrameBytes)
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(frame)}}
	d, err := NewWithOpener(cfg, sess, opener)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.HandleStart(); err != nil {
		t.Fatalf("HandleStart: %v", err)
	}
	waitDone(t, d, 3*time.Second)

	// Both connections received Speaking(true) on enter and Speaking(false)
	// on cleanup, and were disconnected.
	for label, c := range map[string]*mockConn{"a": connA, "b": connB} {
		calls := c.speakingCalls()
		trues := 0
		for _, v := range calls {
			if v {
				trues++
			}
		}
		if trues != 1 {
			t.Errorf("conn %s: expected exactly 1 Speaking(true), got %d (calls: %v)", label, trues, calls)
		}
		if !c.wasDisconnected() {
			t.Errorf("conn %s: expected Disconnect() to be called", label)
		}
	}

	// Both connections received at least one Opus packet (the single frame's
	// worth). Drain whatever's in each buffer and assert it's non-empty.
	gotA := drainConn(connA)
	gotB := drainConn(connB)
	if len(gotA) == 0 {
		t.Error("conn a: expected at least one Opus packet, got 0")
	}
	if len(gotB) == 0 {
		t.Error("conn b: expected at least one Opus packet, got 0")
	}
	// Sanity: the fan-out sends the SAME bytes (encoded once, copied per
	// connection) so the first packets should be byte-equal.
	if len(gotA) > 0 && len(gotB) > 0 && !bytes.Equal(gotA[0], gotB[0]) {
		t.Errorf("fan-out packets differ: a=%v b=%v", gotA[0], gotB[0])
	}
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

func TestStreamLoop_PartialJoin(t *testing.T) {
	// Two configured channels: chan-a succeeds, chan-b fails. streamLoop
	// should skip chan-b, broadcast to chan-a only, and NOT abort the whole
	// attempt (which was the all-or-nothing alternative).
	cfg := testConfig()
	cfg.ChannelIDs = []string{"chan-a", "chan-b"}
	cfg.VoiceReadyTimeout = 2

	connA := newMockConn(8)
	connB := newMockConn(8) // should never receive packets
	sess := &mockSession{
		// No entry for chan-b → JoinChannel falls through to the nil joinConn
		// path and returns the "no joinConn configured" error. That's the
		// simulated join failure.
		joinConnsByChannel: map[string]*mockConn{
			"chan-a": connA,
			// chan-b intentionally absent
		},
	}
	frame := make([]byte, audio.FrameBytes)
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(frame)}}
	d, err := NewWithOpener(cfg, sess, opener)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.HandleStart(); err != nil {
		t.Fatalf("HandleStart: %v", err)
	}
	waitDone(t, d, 3*time.Second)

	// chan-a: joined, streamed, cleaned up.
	if !connA.wasDisconnected() {
		t.Error("conn a: expected Disconnect after EOF")
	}
	if gotA := drainConn(connA); len(gotA) == 0 {
		t.Error("conn a: expected at least one Opus packet")
	}
	// chan-b: never joined, so it should never have received any
	// Speaking/Disconnect/Opus calls.
	if len(connB.speakingCalls()) != 0 {
		t.Errorf("conn b: expected no Speaking calls, got %v", connB.speakingCalls())
	}
	if connB.wasDisconnected() {
		t.Error("conn b: should not have been Disconnected (never joined)")
	}
	if gotB := drainConn(connB); len(gotB) != 0 {
		t.Errorf("conn b: expected no Opus packets, got %d", len(gotB))
	}
}

func TestStreamLoop_AllJoinsFail(t *testing.T) {
	// Two configured channels, both fail. streamLoop should abort cleanly
	// with no packets streamed; no connection could have been established.
	cfg := testConfig()
	cfg.ChannelIDs = []string{"chan-a", "chan-b"}
	cfg.VoiceReadyTimeout = 2

	// joinConnsByChannel has no entries for either channel → JoinChannel
	// returns the "no joinConn configured" error for both.
	sess := &mockSession{}
	frame := make([]byte, audio.FrameBytes)
	opener := &mockOpener{r: nopReadCloser{Reader: bytes.NewReader(frame)}}
	d, err := NewWithOpener(cfg, sess, opener)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.HandleStart(); err != nil {
		t.Fatalf("HandleStart: %v", err)
	}
	waitDone(t, d, 3*time.Second)

	// No connection was ever stored, so cleanup is a no-op for vcs. The
	// important assertion is that streamLoop exited (waitDone enforces it)
	// and that the daemon is back in the idle state for a future start.
	if d.isStreaming || d.joining {
		t.Errorf("expected idle flags, got streaming=%v joining=%v", d.isStreaming, d.joining)
	}
}
