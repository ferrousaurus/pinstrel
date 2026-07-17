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
	guildID    string
	pipeOpener PipeOpener

	activeMu    sync.Mutex
	isStreaming bool
	// joining is true between the moment we send OP4 (inside JoinChannel) and
	// the moment the voice WS/UDP handshake either completes or times out. It
	// prevents concurrent start commands from issuing redundant
	// VOICE_STATE_UPDATEs against the same guild.
	joining    bool
	vc         voice.Connection
	activePipe io.ReadCloser
}

// New creates a Daemon from the given config and Discord session, resolving
// the GuildID from the configured ChannelID. The DiscordToken and ChannelID
// must be non-empty.
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
	if cfg.ChannelID == "" {
		return nil, errors.New("DISCORD_CHANNEL_ID is required in config")
	}
	guildID, err := v.ResolveGuild(cfg.ChannelID)
	if err != nil {
		return nil, err
	}
	return &Daemon{
		config:     cfg,
		voice:      v,
		guildID:    guildID,
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
	log.Printf("Starting pinstrel daemon (channel %s, guild %s)", d.config.ChannelID, d.guildID)
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
// goroutine: it runs the blocking JoinChannel in a sub-goroutine, opens the
// audio pipe for reading in another, and once both succeed enters the Opus
// encode/send loop. It owns d.activePipe and is the single place that
// transitions out of the joining/streaming state on failure. IPC stop is
// handled separately in stopStreaming and works by closing the pipe (EOF) and
// disconnecting the vc.
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

	// Kick off the blocking voice join in a sub-goroutine so we can race it
	// against our outer deadline and against stopStreaming. The result
	// channel is buffered so the sub-goroutine never leaks if we leave early.
	type voiceResult struct {
		vc  voice.Connection
		err error
	}
	voiceCh := make(chan voiceResult, 1)
	log.Printf("Joining Discord voice channel %s (async; deadline %s)", d.config.ChannelID, readyTimeout)
	joinStart := time.Now()
	go func() {
		vc, err := d.voice.JoinChannel(d.guildID, d.config.ChannelID, guildMute, guildDeaf)
		voiceCh <- voiceResult{vc, err}
	}()

	// Open the audio pipe for reading in parallel. The FIFO open blocks until
	// a writer (shairport) connects; running it concurrently with the voice
	// join means we don't serialize the two waits, and we can fail fast if
	// either side exceeds the deadline.
	type fifoResult struct {
		pipe io.ReadCloser
		err  error
	}
	fifoCh := make(chan fifoResult, 1)
	go func() {
		pipe, err := d.pipeOpener.Open()
		fifoCh <- fifoResult{pipe, err}
	}()

	// Wait for the voice join result.
	var vc voice.Connection
	select {
	case vr := <-voiceCh:
		log.Printf("JoinChannel returned in %s (err=%v)", time.Since(joinStart), vr.err)
		if vr.err != nil {
			// JoinChannel already called voice.Close() internally on timeout,
			// but Close does NOT send the gateway nil-channel OP4, so the bot
			// stays visible. Call Disconnect to clear the ghost. vr.vc may be
			// nil if the failure happened before voice registration.
			if vr.vc != nil {
				_ = vr.vc.Disconnect()
			}
			d.cleanupStreamState()
			// The FIFO goroutine is blocked on shairport opening the writer
			// side. We can't cancel a blocking os.OpenFile on a FIFO from
			// here, so it leaks until shairport connects or the process
			// exits. Documented as an acceptable tradeoff in AGENTS.md.
			return
		}
		vc = vr.vc
		d.activeMu.Lock()
		d.vc = vc
		d.joining = false
		d.activeMu.Unlock()
	case <-time.After(remaining):
		log.Printf("Voice join exceeded %s deadline — abandoning. The in-flight "+
			"JoinChannel sub-goroutine will time out internally (~10s) and clean "+
			"up its own voice connection. No ghost remains. See README.", readyTimeout)
		d.cleanupStreamState()
		return
	case <-stopCh:
		log.Println("Stop received while waiting for voice join; abandoning.")
		d.cleanupStreamState()
		return
	}

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

		// Clone the slice since the channel send is async and we reuse opusBuf.
		packet := make([]byte, n)
		copy(packet, opusBuf[:n])

		select {
		case vc.OpusSend() <- packet:
		default:
			log.Println("Warning: OpusSend channel full, dropping packet")
		}
	}
}
