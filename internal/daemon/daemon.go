// Package daemon orchestrates the Discord voice session, audio encoding, and
// IPC command dispatch. It owns the stream lifecycle state machine and depends
// on narrow interfaces (discord.Session, PipeOpener) so the state machine is
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
	"pinstrel/internal/discord"
	"pinstrel/internal/ipc"
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
	config    *config.Config
	session   discord.Session
	pipeOpener PipeOpener

	activeMu    sync.Mutex
	isStreaming bool
	// joining is true between the moment we send OP4 (inside JoinChannel) and
	// the moment the voice WS/UDP handshake either completes or times out.
	// It prevents concurrent start commands from issuing redundant
	// VOICE_STATE_UPDATEs against the same guild.
	joining bool
	// targetGuildID and targetChannelID are the channel HandleStart resolved
	// from the configured user's voice state at start time. streamLoop reads
	// them to drive the single JoinChannel call. Set under activeMu by
	// HandleStart, read by streamLoop.
	targetGuildID  string
	targetChannelID string
	// vc is the single joined voice connection. streamLoop owns it after a
	// successful join until cleanup. Guarded by activeMu.
	vc          discord.Connection
	activePipe  io.ReadCloser
}

// New creates a Daemon from the given config and Discord session. The
// DiscordToken and DiscordUserID must both be non-empty. The user's current
// voice channel is NOT resolved at construction time — that happens lazily on
// each HandleStart, so the daemon follows the user across channels and guilds
// without reconfiguration.
func New(cfg *config.Config, v discord.Session) (*Daemon, error) {
	return newDaemon(cfg, v, &fifoOpener{path: cfg.PipePath})
}

// NewWithOpener is like New but injects a custom PipeOpener, for testing the
// stream state machine without a real FIFO.
func NewWithOpener(cfg *config.Config, v discord.Session, opener PipeOpener) (*Daemon, error) {
	return newDaemon(cfg, v, opener)
}

func newDaemon(cfg *config.Config, v discord.Session, opener PipeOpener) (*Daemon, error) {
	if cfg.DiscordToken == "" {
		return nil, errors.New("DISCORD_TOKEN is required in config")
	}
	if cfg.DiscordUserID == "" {
		return nil, errors.New("DISCORD_USER_ID is required in config")
	}
	return &Daemon{
		config:     cfg,
		session:    v,
		pipeOpener: opener,
	}, nil
}

// Run opens the Discord gateway, registers diagnostic handlers and slash
// commands, and serves IPC commands until a fatal error. It blocks the
// calling goroutine.
//
// Slash registration is fatal on error: a bot that connected but failed to
// register commands is in an inconsistent state, and a partial boot is
// worse than a clean restart (mirrors the "missing config is a hard error"
// posture). The set of registered commands is currently fixed to PingCommand;
// future builtins (start/stop/status) will append to the slice here.
func (d *Daemon) Run() error {
	if err := d.session.Open(); err != nil {
		return err
	}
	defer d.session.Close()
	d.session.AddDiagnostics()

	if err := d.session.RegisterSlashCommands([]discord.Command{discord.PingCommand{}}); err != nil {
		return fmt.Errorf("register slash commands: %w", err)
	}

	// The IPC server holds the Daemon as its CommandHandler; HandleStart and
	// HandleStop are the entry points shairport-sync's hooks invoke.
	log.Printf("Starting pinstrel daemon (following user %s)", d.config.DiscordUserID)
	srv := ipc.NewServer(d.config.SocketPath, d)
	return srv.ListenAndServe()
}

// HandleStart is the ipc.CommandHandler entry for "start": resolves the
// configured user's current voice channel, creates the audio FIFO, and kicks
// off streaming. Returns an error if the user is not in voice or shares no
// guild with the bot — the IPC server surfaces this as "ERR: ..." and the CLI
// exits non-zero so shairport's run_this_before_play_begins hook aborts the
// play. On hard-reject, no observable side effects occur (no FIFO created, no
// streamLoop spawned).
func (d *Daemon) HandleStart() error { return d.startStreaming() }

// HandleStop is the ipc.CommandHandler entry for "stop": tears down streaming.
func (d *Daemon) HandleStop() error { return d.stopStreaming() }

// startStreaming resolves the configured user's current voice channel and, on
// success, kicks off a Discord voice join in the background before returning.
// The return is immediate — it does NOT wait for the voice WS/UDP handshake.
// This decouples shairport-sync's run_this_before_play_begins hook from voice
// readiness: a handshake failure produces one clean join+disconnect per
// AirPlay attempt, not a retry loop. See README troubleshooting and AGENTS.md.
//
// On voice-state resolution failure we hard-reject: nothing observable happens
// (no FIFO created, no flags set, no streamLoop spawned) and the error flows
// back through the IPC server to the shairport hook as a non-zero exit.
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

	// Resolve the configured user's current voice channel from the gateway
	// state cache. This is a synchronous cache read, cheap enough to do under
	// activeMu. A non-nil error hard-rejects the start BEFORE any side effects
	// (no FIFO created, no flags set) so a misconfigured or stale AirPlay
	// attempt aborts cleanly rather than leaving a stale FIFO wedged in /tmp.
	guildID, channelID, err := d.session.ResolveUserVoiceState(d.config.DiscordUserID)
	if err != nil {
		log.Printf("Rejecting start: %v", err)
		return err
	}
	log.Printf("User %s is in voice channel %s (guild %s); joining",
		d.config.DiscordUserID, channelID, guildID)
	d.targetGuildID = guildID
	d.targetChannelID = channelID

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

	// streamLoop owns the blocking JoinChannel + pipe open + Opus send loop
	// and all cleanup of d.vc/d.activePipe. It is the single place we
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

	if d.vc != nil {
		// Disconnect (not Close) sends the OP4-with-nil-channel so Discord
		// removes us from the channel UI — Close alone would tear down the
		// voice WS/UDP but leave a "ghost" presence in the channel.
		_ = d.vc.Speaking(false)
		_ = d.vc.Disconnect()
		d.vc = nil
	}

	return nil
}

// cleanupStreamState tears down the streaming session state: clears the
// joining/streaming flags, disconnects the voice connection, and closes the
// active pipe. Idempotent — safe to call from multiple cleanup paths (early
// exit, deadline, stop, and the main streamLoop defer). The caller must not
// hold d.activeMu.
func (d *Daemon) cleanupStreamState() {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()
	d.isStreaming = false
	d.joining = false
	if d.vc != nil {
		_ = d.vc.Speaking(false)
		_ = d.vc.Disconnect()
		d.vc = nil
	}
	if d.activePipe != nil {
		d.activePipe.Close()
		d.activePipe = nil
	}
}

// streamLoop performs the full lifecycle of a streaming session on a single
// goroutine: it issues one blocking JoinChannel call, opens the audio pipe in
// parallel, and once both succeed enters the Opus encode/send loop. It owns
// d.activePipe and d.vc and is the single place that transitions out of the
// joining/streaming state on failure. IPC stop is handled separately in
// stopStreaming and works by closing the pipe (EOF) and disconnecting the vc.
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

	// Issue the single blocking JoinChannel on its own goroutine so the outer
	// select can race it against the deadline and stopCh. discordgo's
	// ChannelVoiceJoin has no cancellation API; if we abandon the goroutine
	// via the deadline or stopCh arm, the orphaned VoiceConnection will be
	// cleaned up by discordgo's internal ~10s waitUntilConnected timeout
	// (documented as an accepted tradeoff in AGENTS.md — the goroutine leaks
	// only an FD slot, no Discord state).
	type joinResult struct {
		vc  discord.Connection
		err error
	}
	joinCh := make(chan joinResult, 1)
	joinStart := time.Now()
	log.Printf("Joining Discord voice channel %s (async; deadline %s)", d.targetChannelID, readyTimeout)
	go func() {
		vc, err := d.session.JoinChannel(d.targetGuildID, d.targetChannelID, guildMute, guildDeaf)
		joinCh <- joinResult{vc, err}
	}()

	// Open the audio pipe for reading in parallel. The FIFO open blocks until
	// a writer (shairport) connects; running it concurrently with the voice
	// join means we don't serialize the waits, and we can fail fast if either
	// side exceeds the deadline.
	type fifoResult struct {
		pipe io.ReadCloser
		err  error
	}
	fifoCh := make(chan fifoResult, 1)
	go func() {
		pipe, err := d.pipeOpener.Open()
		fifoCh <- fifoResult{pipe, err}
	}()

	// Wait for the single join result, the deadline, or stop. A single
	// failure (timeout or error) aborts the stream cleanly.
	var vc discord.Connection
	var deadlineCh <-chan time.Time
	if remaining > 0 {
		deadlineCh = time.After(remaining)
	}
	select {
	case r := <-joinCh:
		log.Printf("JoinChannel for %s returned in %s (err=%v)", d.targetChannelID, time.Since(joinStart), r.err)
		if r.err != nil {
			// JoinChannel already called voice.Close() internally on timeout,
			// but Close does NOT send the gateway nil-channel OP4, so the bot
			// stays visible. Call Disconnect to clear the ghost. r.vc may be
			// nil if the failure happened before voice registration.
			if r.vc != nil {
				_ = r.vc.Disconnect()
			}
			log.Printf("Voice join failed: %v — aborting stream. No audio will be streamed.", r.err)
			d.cleanupStreamState()
			// The FIFO goroutine is blocked on shairport opening the writer
			// side. We can't cancel a blocking os.OpenFile on a FIFO from
			// here, so it leaks until shairport connects or the process
			// exits. Documented as an acceptable tradeoff in AGENTS.md.
			return
		}
		vc = r.vc
	case <-deadlineCh:
		log.Printf("Voice join exceeded %s deadline — aborting stream. "+
			"The in-flight JoinChannel goroutine will time out internally "+
			"(~10s) and clean up its own voice connection. See README.",
			readyTimeout)
		d.cleanupStreamState()
		return
	case <-stopCh:
		log.Println("Stop received while waiting for voice join; abandoning.")
		d.cleanupStreamState()
		return
	}

	log.Printf("Successfully joined voice channel %s", d.targetChannelID)
	d.activeMu.Lock()
	d.vc = vc
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

	// Tell Discord we're speaking. Failures here are non-fatal — the
	// connection still ships Opus packets over UDP, just without the speaking
	// indicator, so we log nothing and move on.
	_ = vc.Speaking(true)

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

		// Send the encoded packet to the single joined connection. We clone
		// the packet bytes before handing them to OpusSend because discordgo's
		// per-connection opusSender goroutine reads the slice asynchronously
		// after a 20ms ticker-paced wait — between receiving from the channel
		// and consuming the bytes (DAVE-encrypt → AEAD-seal → UDP-write) the
		// producer's next iteration can re-fill opusBuf via enc.Encode, which
		// would race on the backing array. The clone is a single allocation
		// per 20ms frame.
		pkt := make([]byte, n)
		copy(pkt, opusBuf[:n])

		// Backpressure the producer to the consumer's 20ms tick rate rather
		// than free-running against shairport's source clock. The OpusSend
		// channel's 16-frame (≈320ms) buffer absorbs jitter; when it fills,
		// blocking here throttles the FIFO read, which blocks shairport's
		// write() on the named pipe and back-pressures the AirPlay transport.
		// Previously a select-default arm dropped the packet instead, which
		// decoupled the producer from the consumer's real-time pace and
		// surfaced as audible speedup (dropped frames skip source audio
		// forward while the RTP timestamp still advances by 960) and slowdown
		// (when shairport starved the channel and discordgo's free-running
		// 20ms ticker stretched the inter-packet gap). The stopCh arm keeps
		// teardown responsive when the voice connection dies: discordgo's
		// opusSender exits via its <-close arm without closing OpusSend, so
		// a plain blocking send would otherwise wedge the producer. stopCh is
		// signalled within stopPollInterval (100ms) by the isStreaming-poll
		// goroutine above on HandleStop / cleanupStreamState.
		select {
		case vc.OpusSend() <- pkt:
		case <-stopCh:
			log.Println("Stop received during Opus send; aborting stream.")
			return
		}
	}
}