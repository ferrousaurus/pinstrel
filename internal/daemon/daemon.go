// Package daemon orchestrates the Discord voice session, audio encoding, and
// IPC command dispatch. It owns the stream lifecycle state machine and depends
// on narrow interfaces (voice.Session, PipeOpener) so the state machine is
// testable without a live Discord connection or a real FIFO.
package daemon

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"pinstrel/internal/audio"
	"pinstrel/internal/config"
	"pinstrel/internal/ipc"
	"pinstrel/internal/voice"
)

const (
	// stopPollInterval is how often the stopCh ticker goroutine polls
	// d.isStreaming. 100ms gives imperceptible stop latency with negligible
	// lock contention.
	stopPollInterval = 100 * time.Millisecond

	// guildMute and guildDeaf are the voice-state flags pinstrel requests on
	// join: muted=false (we send audio), deafened=true (play-only bot — we
	// never receive audio). The deaf=true UI badge is expected and does not
	// affect sending.
	guildMute = false
	guildDeaf = true
)

// PipeOpener opens the audio pipe for reading. The default implementation
// blocks on a FIFO open until a writer (shairport-sync) connects; tests
// substitute an in-memory reader to exercise the stream state machine.
type PipeOpener interface {
	Open() (io.ReadCloser, error)
}

// fifoOpener opens a named pipe (FIFO) for reading. The open blocks until a
// writer connects — Go has no portable way to cancel a blocking FIFO open,
// so a goroutine stuck here leaks until shairport connects or the process
// exits. Documented as an accepted tradeoff in AGENTS.md.
type fifoOpener struct {
	path string
}

func (o *fifoOpener) Open() (io.ReadCloser, error) {
	return os.OpenFile(o.path, os.O_RDONLY, 0)
}

// Daemon orchestrates the Discord voice session, the audio stream, and IPC
// command dispatch. It implements ipc.CommandHandler.
type Daemon struct {
	config     *config.Config
	voice      voice.Session
	// guildIDs is 1:1 with config.ChannelIDs, resolved once at New() time so
	// streamLoop can fan out the Joins without any REST calls during playback.
	// Channels may live in different guilds — we deliberately do not dedupe.
	guildIDs    []string
	pipeOpener  PipeOpener

	activeMu    sync.Mutex
	isStreaming bool
	// joining is true between the moment we send OP4 (inside JoinChannel) and
	// the moment all voice WS/UDP handshakes either complete or time out. It
	// prevents concurrent start commands from issuing redundant
	// VOICE_STATE_UPDATEs against the same guild. With N configured channels,
	// joining covers the whole fan-out phase, not any single join.
	joining    bool
	// vcs holds the connections that successfully joined during the current
	// streamLoop. streamLoop fans every Opus packet out to all of them. Order
	// matches the order of successful joins, NOT d.config.ChannelIDs — failed
	// joins are skipped. Ownership is exclusive to streamLoop until cleanup.
	vcs         []voice.Connection
	activePipe  io.ReadCloser
}

// New creates a Daemon from the given config and Discord session, resolving
// the GuildID for every configured ChannelID. The DiscordToken must be
// non-empty and DISCORD_CHANNEL_IDS must list at least one channel ID. Channels
// may live in different guilds — each is resolved independently.
func New(cfg *config.Config, v voice.Session) (*Daemon, error) {
	return newDaemon(cfg, v, &fifoOpener{path: cfg.PipePath})
}

// NewWithOpener is like New but injects a custom PipeOpener, for testing the
// stream state machine without a real FIFO.
func NewWithOpener(cfg *config.Config, v voice.Session, opener PipeOpener) (*Daemon, error) {
	return newDaemon(cfg, v, opener)
}

func newDaemon(cfg *config.Config, v voice.Session, opener PipeOpener) (*Daemon, error) {
	if cfg.DiscordToken == "" {
		return nil, errors.New("DISCORD_TOKEN is required in config")
	}
	if len(cfg.ChannelIDs) == 0 {
		return nil, errors.New("DISCORD_CHANNEL_IDS must list at least one channel")
	}
	// Resolve each channel's guild up-front so streamLoop can fan out joins
	// without any REST calls during playback. A bad channel ID is a hard
	// error — pinstrel would otherwise discover it only on the first AirPlay
	// attempt, which is a poor time to fail. Config-level normalization
	// (whitespace trim, empty-drop) is already done in LoadConfig.
	guildIDs := make([]string, len(cfg.ChannelIDs))
	for i, chID := range cfg.ChannelIDs {
		g, err := v.ResolveGuild(chID)
		if err != nil {
			return nil, fmt.Errorf("resolve channel %q: %w", chID, err)
		}
		guildIDs[i] = g
	}
	return &Daemon{
		config:     cfg,
		voice:      v,
		guildIDs:   guildIDs,
		pipeOpener: opener,
	}, nil
}

// Run opens the Discord gateway, registers diagnostic handlers, and serves
// IPC commands until a fatal error. It blocks the calling goroutine.
func (d *Daemon) Run() error {
	if err := d.voice.Open(); err != nil {
		return err
	}
	defer d.voice.Close()
	d.voice.AddDiagnostics()

	// The IPC server holds the Daemon as its CommandHandler; HandleStart and
	// HandleStop are the entry points shairport-sync's hooks invoke.
	log.Printf("Starting pinstrel daemon (channels %v, guilds %v)", d.config.ChannelIDs, d.guildIDs)
	srv := ipc.NewServer(d.config.SocketPath, d)
	return srv.ListenAndServe()
}

// HandleStart is the ipc.CommandHandler entry for "start": kicks off streaming.
func (d *Daemon) HandleStart() error { return d.startStreaming() }

// HandleStop is the ipc.CommandHandler entry for "stop": tears down streaming.
func (d *Daemon) HandleStop() error { return d.stopStreaming() }

// startStreaming kicks off a Discord voice join in the background and returns
// immediately, without waiting for the voice WS/UDP handshake. This decouples
// shairport-sync's run_this_before_play_begins hook from voice readiness: a
// handshake failure produces one clean join+disconnect per AirPlay attempt,
// not a retry loop. See README troubleshooting and AGENTS.md.
//
// Why we still call JoinChannel (blocking) and not ChannelVoiceJoinManual:
// discordgo's VoiceConnection.session/deaf/mute fields are unexported, and
// voice.open() dereferences v.session.Dialer when VOICE_SERVER_UPDATE arrives.
// Only ChannelVoiceJoin (in-package) sets v.session. So we run it on a
// goroutine in streamLoop and apply our own outer deadline via
// VOICE_READY_TIMEOUT.
func (d *Daemon) startStreaming() error {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()

	if d.isStreaming || d.joining {
		log.Println("Already streaming or joining, ignoring start command")
		return nil
	}

	// Pre-create the named pipe (FIFO) if it does not exist. shairport opens
	// this for writing on the pipe backend; we open it for reading once the
	// voice handshake completes. Creating it up-front lets shairport's writer
	// open succeed even if our reader side isn't ready yet (the writer open
	// blocks on a FIFO until a reader exists, but creating the FIFO itself
	// unblocks shairport's setup).
	if _, err := os.Stat(d.config.PipePath); os.IsNotExist(err) {
		log.Printf("Creating named pipe FIFO at %s...", d.config.PipePath)
		if err := syscall.Mkfifo(d.config.PipePath, 0666); err != nil {
			return fmt.Errorf("failed to create named pipe: %w", err)
		}
		_ = os.Chmod(d.config.PipePath, 0666)
	}

	d.joining = true
	d.isStreaming = true

	// streamLoop owns the blocking JoinChannel fan-out + pipe open + Opus send
	// loop and all cleanup of d.vcs/d.activePipe. It is the single place we
	// transition out of the joining/streaming state on EOF/error/timeout.
	go d.streamLoop()

	return nil
}

// stopStreaming closes the audio pipe and disconnects the Discord voice
// connection. Clearing the flags first makes streamLoop's pending voice-join
// or pipe-read bail on its next iteration rather than racing this Disconnect.
func (d *Daemon) stopStreaming() error {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()

	if !d.isStreaming && !d.joining {
		log.Println("Not streaming, ignoring stop command")
		return nil
	}

	log.Println("Stopping stream and disconnecting from voice...")
	d.isStreaming = false
	d.joining = false

	if d.activePipe != nil {
		// Closing the reader unblocks any pending io.ReadFull in streamLoop,
		// causing it to exit its read loop and run the deferred cleanup.
		d.activePipe.Close()
		d.activePipe = nil
	}

	if len(d.vcs) > 0 {
		// Disconnect (not Close) sends the OP4-with-nil-channel so Discord
		// removes us from the channel UI — Close alone would tear down the
		// voice WS/UDP but leave a "ghost" presence in the channel. One
		// nil-channel OP4 per connection; each connection is independent.
		for _, c := range d.vcs {
			_ = c.Speaking(false)
			_ = c.Disconnect()
		}
		d.vcs = nil
	}

	return nil
}

// cleanupStreamState tears down the streaming session state: clears the
// joining/streaming flags, disconnects every joined voice connection, and
// closes the active pipe. Idempotent — safe to call from multiple cleanup
// paths (early exit, deadline, stop, and the main streamLoop defer). The
// caller must not hold d.activeMu.
func (d *Daemon) cleanupStreamState() {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()
	d.isStreaming = false
	d.joining = false
	if len(d.vcs) > 0 {
		for _, c := range d.vcs {
			_ = c.Speaking(false)
			_ = c.Disconnect()
		}
		d.vcs = nil
	}
	if d.activePipe != nil {
		d.activePipe.Close()
		d.activePipe = nil
	}
}

// streamLoop performs the full lifecycle of a streaming session on a single
// goroutine: it fans out one blocking JoinChannel sub-goroutine per
// configured channel, opens the audio pipe for reading in another, and once
// at least one join succeeds and the pipe is open enters the Opus encode/send
// loop which broadcasts to every joined connection. It owns d.activePipe and
// d.vcs and is the single place that transitions out of the
// joining/streaming state on failure. IPC stop is handled separately in
// stopStreaming and works by closing the pipe (EOF) and disconnecting the vcs.
func (d *Daemon) streamLoop() {
	log.Println("Audio streaming loop started")

	// Initialize the Opus encoder up-front so it's ready the moment voice
	// flips Ready. Encoder construction is cheap and doesn't touch Discord.
	enc, err := audio.NewEncoder(d.config.Bitrate)
	if err != nil {
		log.Printf("Failed to create Opus encoder: %v", err)
		d.cleanupStreamState()
		return
	}
	if err := enc.SetBitrate(d.config.Bitrate); err != nil {
		log.Printf("Warning: failed to set bitrate to %d: %v", d.config.Bitrate, err)
	}

	// Outer deadline for the whole join+ready dance. ChannelVoiceJoin has its
	// own ~10s internal waitUntilConnected poll; VOICE_READY_TIMEOUT bounds
	// the entire handshake from pinstrel's perspective so a hung UDP
	// IP-discovery reply can't leave the bot "joined but deafened" forever.
	// On expiry we Disconnect cleanly (sends the nil-channel OP4) so no ghost
	// lingers.
	readyTimeout := time.Duration(d.config.VoiceReadyTimeout) * time.Second
	if readyTimeout <= 0 {
		readyTimeout = time.Duration(config.DefaultVoiceReadyTimeout) * time.Second
	}
	deadline := time.Now().Add(readyTimeout)
	remaining := time.Until(deadline)

	// stopCh unifies the stopStreaming path with the deadline-based waits
	// below. A ticker goroutine polls d.isStreaming and signals stopCh when
	// it flips false. stopCh is buffered (cap 1) so the ticker's blocking send
	// can never be lost: streamLoop may be parked in another select arm when
	// the stop arrives, and an unbuffered non-blocking send would drop the
	// signal and leave streamLoop blocked until the deadline.
	stopCh := make(chan struct{}, 1)
	go func() {
		t := time.NewTicker(stopPollInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				d.activeMu.Lock()
				stillStreaming := d.isStreaming
				d.activeMu.Unlock()
				if !stillStreaming {
					stopCh <- struct{}{}
					return
				}
			case <-stopCh:
				return
			}
		}
	}()
	defer close(stopCh)

	// Fan out one JoinChannel sub-goroutine per configured channel so all N
	// handshakes run concurrently. Every sub-goroutine races against the
	// same shared wall-clock deadline (VOICE_READY_TIMEOUT) — concurrent
	// joins means concurrent budget, not VOICE_READY_TIMEOUT/N per channel.
	// The results channel is buffered to len(ChannelIDs) so a
	// sub-goroutine whose result we never read (because we left via the
	// deadline or stopCh arm) still completes its send and exits without
	// leaking. It will already have created a *discordgo.VoiceConnection; we
	// leave that for discordgo's internal ~10s waitUntilConnected timeout to
	// disconnect (same tradeoff as the single-channel code, scaled by N).
	type voiceResult struct {
		idx     int
		channel string
		vc      voice.Connection
		err     error
	}
	channels := d.config.ChannelIDs
	voiceCh := make(chan voiceResult, len(channels))
	joinStart := time.Now()
	for i, chID := range channels {
		i, chID := i, chID
		log.Printf("Joining Discord voice channel %s (async; deadline %s)", chID, readyTimeout)
		go func() {
			vc, err := d.voice.JoinChannel(d.guildIDs[i], chID, guildMute, guildDeaf)
			voiceCh <- voiceResult{i, chID, vc, err}
		}()
	}

	// Open the audio pipe for reading in parallel. The FIFO open blocks until
	// a writer (shairport) connects; running it concurrently with the voice
	// joins means we don't serialize the waits, and we can fail fast if
	// either side exceeds the deadline. There is exactly one shared reader —
	// every successfully-joined connection will be sent Opus packets read
	// from this same pipe.
	type fifoResult struct {
		pipe io.ReadCloser
		err  error
	}
	fifoCh := make(chan fifoResult, 1)
	go func() {
		pipe, err := d.pipeOpener.Open()
		fifoCh <- fifoResult{pipe, err}
	}()

	// Collect join results until either every join has reported, the shared
	// deadline fires, or stop arrives. We deliberately do NOT bail on the
	// first failure — partial-join semantics: skip the failed channel and
	// keep collecting. The whole attempt only aborts if zero channels joined.
	joined := make([]voice.Connection, 0, len(channels))
	joinedIdx := make([]int, 0, len(channels))
	pending := len(channels)
	var deadlineCh <-chan time.Time
	if remaining > 0 {
		deadlineCh = time.After(remaining)
	}
collectJoins:
	for pending > 0 {
		select {
		case vr := <-voiceCh:
			pending--
			log.Printf("JoinChannel for %s returned in %s (err=%v)", vr.channel, time.Since(joinStart), vr.err)
			if vr.err != nil {
				// JoinChannel already called voice.Close() internally on
				// timeout, but Close does NOT send the gateway nil-channel
				// OP4, so the bot stays visible. Call Disconnect to clear
				// the ghost. vr.vc may be nil if the failure happened
				// before voice registration.
				if vr.vc != nil {
					_ = vr.vc.Disconnect()
				}
				log.Printf("Channel %s join failed: %v — skipping", vr.channel, vr.err)
				continue
			}
			joined = append(joined, vr.vc)
			joinedIdx = append(joinedIdx, vr.idx)
		case <-deadlineCh:
			log.Printf("Voice joins exceeded %s deadline with %d/%d still pending — "+
				"abandoning the pending ones and streaming to the %d already joined. "+
				"In-flight JoinChannel sub-goroutines will time out internally "+
				"(~10s each) and clean up their own voice connections. See README.",
				readyTimeout, pending, len(channels), len(joined))
			break collectJoins
		case <-stopCh:
			log.Println("Stop received while waiting for voice joins; abandoning.")
			// Drain any connections we did collect before tearing down so
			// they don't leak.
			for _, c := range joined {
				_ = c.Disconnect()
			}
			d.cleanupStreamState()
			return
		}
	}

	if len(joined) == 0 {
		log.Printf("All %d voice joins failed — aborting stream. No audio will be streamed.", len(channels))
		d.cleanupStreamState()
		// The FIFO goroutine is blocked on shairport opening the writer
		// side. We can't cancel a blocking os.OpenFile on a FIFO from
		// here, so it leaks until shairport connects or the process
		// exits. Documented as an acceptable tradeoff in AGENTS.md.
		return
	}

	log.Printf("Successfully joined %d/%d voice channels", len(joined), len(channels))
	d.activeMu.Lock()
	d.vcs = joined
	d.joining = false
	d.activeMu.Unlock()

	// Voice joined. Now wait for the pipe reader side (shairport opening the
	// writer). Use the same deadline; if playback never materializes we bail
	// and disconnect.
	remaining = time.Until(deadline)
	var pipe io.ReadCloser
	select {
	case fr := <-fifoCh:
		if fr.err != nil {
			log.Printf("Failed to open named pipe: %v", fr.err)
			d.cleanupStreamState()
			return
		}
		pipe = fr.pipe
	case <-time.After(remaining):
		log.Printf("Timed out waiting for shairport to open the audio FIFO writer "+
			"within %s — disconnecting.", readyTimeout)
		d.cleanupStreamState()
		return
	case <-stopCh:
		log.Println("Stop received while waiting for FIFO writer; disconnecting.")
		d.cleanupStreamState()
		return
	}

	d.activeMu.Lock()
	d.activePipe = pipe
	d.activeMu.Unlock()

	defer func() {
		log.Println("Audio streaming loop exited")
		d.cleanupStreamState()
	}()

	// Tell Discord we're speaking on every joined connection. Failures here
	// are non-fatal — the connection still ships Opus packets over UDP, just
	// without the speaking indicator, so we log nothing and move on.
	for _, c := range joined {
		_ = c.Speaking(true)
	}
	// Snapshot the joined connections into locals for the encode/send loop.
	// `joined` and `joinedIDs` are positionally parallel — index i in one
	// corresponds to index i in the other. Capturing once (rather than
	// re-reading d.vcs under the lock each 20ms frame) matches the original
	// single-channel pattern, which had the same Disconnect-vs-send race
	// and treated it as accepted.
	joinedIDs := d.shortChannelIDs(joinedIdx)

	byteBuf := make([]byte, audio.FrameBytes)
	pcmBuf := make([]int16, audio.FrameSize)
	opusBuf := make([]byte, audio.MaxPacketSize)

	for {
		// Read a full PCM frame. This blocks until the pipe has sufficient data.
		_, err := io.ReadFull(pipe, byteBuf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrClosed) {
				log.Println("Named pipe EOF or connection closed.")
			} else {
				log.Printf("Read error on named pipe: %v", err)
			}
			return
		}

		audio.DecodePCMFrame(byteBuf, pcmBuf)

		n, err := enc.Encode(pcmBuf, opusBuf)
		if err != nil {
			log.Printf("Opus encoding error: %v", err)
			return
		}

		// Fan out the encoded packet to every joined connection. We encode
		// ONCE per frame (single Opus encoder, unchanged CPU cost on the Pi)
		// and give each connection its own freshly-allocated copy of the
		// packet bytes — never alias the shared opusBuf across the async
		// channel sends. discordgo's per-connection UDP senders read from
		// their OpusSend channel on independent goroutines and would race
		// on opusBuf if we shared it. The old single-channel code cloned
		// once here for the same reason; we now clone N times per frame.
		for i, c := range joined {
			pkt := make([]byte, n)
			copy(pkt, opusBuf[:n])
			select {
			case c.OpusSend() <- pkt:
			default:
				log.Printf("Warning: OpusSend channel full for connection %d (channel %s), dropping packet",
					i, joinedIDs[i])
			}
		}
	}
}

// shortChannelIDs returns, for each index into d.config.ChannelIDs, the
// channel ID truncated to its first 8 characters for use in per-packet log
// lines. The full ID is too noisy for a log that can fire every 20ms when a
// UDP send is backed up; 8 chars is enough to correlate against the startup
// "channels %v" log line while staying debuggable. Returns empty strings for
// out-of-range indices (defensive; should never happen given how joinedIdx is
// built).
func (d *Daemon) shortChannelIDs(idxs []int) []string {
	out := make([]string, len(idxs))
	for i, idx := range idxs {
		if idx >= 0 && idx < len(d.config.ChannelIDs) {
			id := d.config.ChannelIDs[idx]
			if len(id) > 8 {
				id = id[:8]
			}
			out[i] = id
		}
	}
	return out
}
