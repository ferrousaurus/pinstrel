package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"gopkg.in/hraban/opus.v2"
)

const (
	// sampleRate is the PCM sample rate Discord voice expects. shairport-sync
	// is configured (configs/shairport-sync.conf.template) to resample
	// AirPlay's 44.1kHz up to this so pinstrel never resamples.
	sampleRate = 48000
	// numChannels is the channel count (stereo) for both PCM input and the
	// Opus encoder output.
	numChannels = 2
	// stopPollInterval is how often the stopCh ticker goroutine polls
	// d.isStreaming. 100ms gives imperceptible stop latency with negligible
	// lock contention.
	stopPollInterval = 100 * time.Millisecond
	// maxOpusPacketSize is the output buffer for a single Opus frame. The
	// RFC 7587 max is 1275 bytes; 1024 covers our 128kbps frames with margin.
	maxOpusPacketSize = 1024

	// PCM frame layout for a 20ms Opus frame at 48kHz stereo S16LE:
	//   48000 Hz * 0.02s = 960 samples/channel
	//   960 * 2 channels = 1920 total samples
	//   1920 * 2 bytes/sample = 3840 bytes
	frameSamples  = sampleRate / 1000 * 20 // 960 samples per channel per 20ms frame
	pcmFrameSize  = frameSamples * numChannels
	pcmFrameBytes = pcmFrameSize * 2 // 16-bit samples

	// guildMute and guildDeaf are the voice-state flags pinstrel requests on
	// join: muted=false (we send audio), deafened=true (play-only bot — we
	// never receive audio). The deaf=true UI badge is expected and does not
	// affect sending.
	guildMute = false
	guildDeaf = true
)

// Daemon manages the Discord session, Unix socket IPC server, and audio stream.
type Daemon struct {
	config      *Config
	dg          *discordgo.Session
	vc          *discordgo.VoiceConnection
	guildID     string
	activePipe  *os.File
	activeMu    sync.Mutex
	isStreaming bool
	// joining is true between the moment we send OP4 (inside ChannelVoiceJoin)
	// and the moment the voice WS/UDP handshake either completes (vc.Ready) or
	// times out. It prevents concurrent start commands from issuing redundant
	// VOICE_STATE_UPDATEs against the same guild.
	joining bool
}

// NewDaemon initializes a new Daemon, resolving the GuildID from the ChannelID.
func NewDaemon(cfg *Config) (*Daemon, error) {
	dg, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create Discord session: %w", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates
	dg.LogLevel = discordgo.LogInformational

	log.Printf("Connecting to Discord to resolve channel info for %s...", cfg.ChannelID)
	ch, err := dg.Channel(cfg.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve channel details: %w (verify Discord Token and Channel ID)", err)
	}
	log.Printf("Successfully resolved channel %s to Guild ID %s", cfg.ChannelID, ch.GuildID)

	return &Daemon{
		config:  cfg,
		dg:      dg,
		guildID: ch.GuildID,
	}, nil
}

// Start opens the Discord session and listens for IPC commands on the Unix socket.
func (d *Daemon) Start() error {
	if err := d.dg.Open(); err != nil {
		return fmt.Errorf("failed to open Discord session: %w", err)
	}
	defer d.dg.Close()

	// Phase 1 diagnostics: log the two gateway events that drive the voice
	// handshake. If `timeout waiting for voice` happens and neither of these
	// fires, the gateway never dispatched the join — see README troubleshooting.
	// onVoiceServerUpdate (discordgo) only sets sessionID/endpoint and calls
	// voice.open(); these handlers are read-only observers that add pinstrel
	// context about *which* event arrived and with what payload.
	d.dg.AddHandler(func(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
		// Filter VOICE_STATE_UPDATE events to those fired by the bot
		selfID := ""
		if s.State != nil && s.State.User != nil {
			selfID = s.State.User.ID
		}
		if vs.UserID != selfID {
			return
		}
		log.Printf("VOICE_STATE_UPDATE: guild=%s channel=%s session_id=%q user=%s",
			vs.GuildID, vs.ChannelID, vs.SessionID, vs.UserID)
	})
	d.dg.AddHandler(func(s *discordgo.Session, vss *discordgo.VoiceServerUpdate) {
		log.Printf("VOICE_SERVER_UPDATE: guild=%s endpoint=%s token_present=%t",
			vss.GuildID, vss.Endpoint, vss.Token != "")
	})

	_ = os.Remove(d.config.SocketPath)

	listener, err := net.Listen("unix", d.config.SocketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket %s: %w", d.config.SocketPath, err)
	}
	defer listener.Close()
	defer os.Remove(d.config.SocketPath)

	// 0666 (rw-rw-rw-) so shairport-sync — which may run as a different user —
	// can connect() to the socket without sharing a group with pinstrel. See
	// pinstrel.service: PrivateTmp is deliberately disabled for the same reason.
	if err := os.Chmod(d.config.SocketPath, 0666); err != nil {
		log.Printf("Warning: failed to chmod socket %s: %v", d.config.SocketPath, err)
	}

	log.Printf("pinstrel daemon started. Socket: %s", d.config.SocketPath)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Error accepting socket connection: %v", err)
			continue
		}
		go d.handleIPC(conn)
	}
}

// handleIPCStart processes a "start" IPC command: kicks off streaming and
// writes the IPC response ("OK" or "ERR: ...") to the client.
func (d *Daemon) handleIPCStart(conn net.Conn) {
	if err := d.startStreaming(); err != nil {
		log.Printf("Error starting stream: %v", err)
		d.writeIPCResponse(conn, fmt.Sprintf("ERR: %v", err))
		return
	}
	d.writeIPCResponse(conn, "OK")
}

// handleIPCStop processes a "stop" IPC command: tears down streaming and
// writes the IPC response ("OK" or "ERR: ...") to the client.
func (d *Daemon) handleIPCStop(conn net.Conn) {
	if err := d.stopStreaming(); err != nil {
		log.Printf("Error stopping stream: %v", err)
		d.writeIPCResponse(conn, fmt.Sprintf("ERR: %v", err))
		return
	}
	d.writeIPCResponse(conn, "OK")
}

// writeIPCResponse sends a single line response to the IPC client, logging
// any write failure. The IPC client (shairport-sync hook) ignores the body
// but expects a line so its blocking read completes.
func (d *Daemon) writeIPCResponse(conn net.Conn, msg string) {
	if _, err := conn.Write([]byte(msg + "\n")); err != nil {
		log.Printf("Failed to write IPC response: %v", err)
	}
}

// handleIPC reads a single IPC command from the Unix socket connection and
// dispatches it. Unknown commands get an error response.
func (d *Daemon) handleIPC(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	cmd := scanner.Text()
	log.Printf("Received IPC command: %s", cmd)
	switch cmd {
	case "start":
		d.handleIPCStart(conn)
	case "stop":
		d.handleIPCStop(conn)
	default:
		d.writeIPCResponse(conn, "ERR: Unknown command")
	}
}

// startStreaming kicks off a Discord voice join in the background and returns
// OK to the IPC client (shairport) immediately, without waiting for the voice
// WS/UDP handshake. This replaces the previous synchronous ChannelVoiceJoin
// call, which blocked shairport's `run_this_before_play_begins` hook for up to
// 10 seconds while polling voice.Ready. When the handshake then failed — the
// common case on discordgo v0.29.0, which advertises only the legacy
// `xsalsa20_poly1305` Select Protocol mode that modern Discord voice servers
// reject — the hook would time out, shairport aborted the AirPlay session, fired
// `stop`, then `start` again, producing the "drops and rejoins" loop the user
// saw. Decoupling the hook from voice readiness (this function) plus upgrading
// discordgo to master (which advertises `aead_aes256_gcm_rtpsize`) together fix
// both the symptom and the root cause. See README troubleshooting.
//
// Why we still call ChannelVoiceJoin (blocking) and not ChannelVoiceJoinManual:
// discordgo's VoiceConnection.session/deaf/mute fields are unexported, and
// voice.open() dereferences v.session.Dialer when VOICE_SERVER_UPDATE arrives.
// Only ChannelVoiceJoin (in-package) sets v.session. So we run it on a goroutine
// in streamLoop and apply our own outer deadline via VOICE_READY_TIMEOUT.
func (d *Daemon) startStreaming() error {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()

	if d.isStreaming || d.joining {
		log.Println("Already streaming or joining, ignoring start command")
		return nil
	}

	// Pre-create the named pipe (FIFO) if it does not exist. shairport opens
	// this for writing on the `pipe` backend; we open it for reading once the
	// voice handshake completes. Creating it up-front lets shairport's writer
	// open succeed even if our reader side isn't ready yet (the writer open
	// blocks on a FIFO until a reader exists, but creating the FIFO itself
	// unblocks shairport's setup).
	if _, err := os.Stat(d.config.PipePath); os.IsNotExist(err) {
		log.Printf("Creating named pipe FIFO at %s...", d.config.PipePath)
		if err := syscall.Mkfifo(d.config.PipePath, 0666); err != nil {
			return fmt.Errorf("failed to create named pipe: %w", err)
		}
		// Set pipe permissions to 666 so anyone can write to it
		_ = os.Chmod(d.config.PipePath, 0666)
	}

	d.joining = true
	d.isStreaming = true

	// streamLoop owns the blocking ChannelVoiceJoin + FIFO open + Opus send
	// loop and all cleanup of d.vc/d.activePipe. It is the single place we
	// transition out of the joining/streaming state on EOF/error/timeout.
	go d.streamLoop()

	return nil
}

// stopStreaming closes the named pipe and disconnects the Discord voice bot.
func (d *Daemon) stopStreaming() error {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()

	if !d.isStreaming && !d.joining {
		log.Println("Not streaming, ignoring stop command")
		return nil
	}

	log.Println("Stopping stream and disconnecting from voice...")
	// Clear flags first so streamLoop's pending voice-join / pipe-read bails on
	// its next iteration rather than racing this Disconnect to set d.vc = nil.
	d.isStreaming = false
	d.joining = false

	if d.activePipe != nil {
		// Closing the file descriptor unblocks any pending io.ReadFull in
		// streamLoop, causing it to exit its read loop and run the deferred
		// cleanup that calls vc.Disconnect().
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
// goroutine: it runs the blocking ChannelVoiceJoin in a sub-goroutine, opens
// the audio FIFO for reading in another, and once both have succeeded enters
// the Opus encode/send loop. It owns d.activePipe and is the single place
// that transitions out of the joining/streaming state on failure. IPC stop is
// handled separately in stopStreaming and works by closing the pipe (EOF) and
// Disconnecting the vc.
func (d *Daemon) streamLoop() {
	log.Println("Audio streaming loop started")

	// Initialize Opus encoder (48kHz, Stereo) up-front so it's ready the moment
	// voice flips Ready. Encoder construction is cheap and doesn't touch Discord.
	enc, err := opus.NewEncoder(sampleRate, numChannels, opus.AppAudio)
	if err != nil {
		log.Printf("Failed to create Opus encoder: %v", err)
		d.cleanupStreamState()
		return
	}
	if err := enc.SetBitrate(d.config.Bitrate); err != nil {
		log.Printf("Warning: failed to set bitrate to %d: %v", d.config.Bitrate, err)
	}

	// Outer deadline for the whole join+ready dance. ChannelVoiceJoin has its
	// own ~10s internal waitUntilConnected poll; VOICE_READY_TIMEOUT bounds the
	// entire handshake from pinstrel's perspective so a hung UDP IP-discovery
	// reply (e.g. discordgo's connected-socket dial dropping the reply, or the
	// legacy Select Protocol mode being rejected by the voice server) can't
	// leave the bot "joined but deafened" forever. On expiry we Disconnect
	// cleanly (sends the nil-channel OP4) so no ghost lingers.
	readyTimeout := time.Duration(d.config.VoiceReadyTimeout) * time.Second
	if readyTimeout <= 0 {
		readyTimeout = time.Duration(defaultVoiceReadyTimeout) * time.Second // guard against misconfigured 0/negative
	}
	deadline := time.Now().Add(readyTimeout)
	remaining := time.Until(deadline)

	// stopCh unifies the stopStreaming path with the deadline-based waits
	// below. A tiny ticker goroutine polls d.isStreaming every stopPollInterval
	// and signals stopCh when it flips false. stopCh is buffered (cap 1) so the
	// ticker's blocking send can never be lost: streamLoop may be parked in
	// another select arm when the stop arrives, and an unbuffered non-blocking
	// send would drop the signal and leave streamLoop blocked until the
	// deadline. We close stopCh when streamLoop exits so the ticker can return.
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
	// against our outer deadline and against stopStreaming. The result channel
	// is buffered so the sub-goroutine never leaks if we leave early.
	type voiceResult struct {
		vc  *discordgo.VoiceConnection
		err error
	}
	voiceCh := make(chan voiceResult, 1)
	log.Printf("Joining Discord voice channel %s (async; deadline %s)", d.config.ChannelID, readyTimeout)
	joinStart := time.Now()
	go func() {
		vc, err := d.dg.ChannelVoiceJoin(d.guildID, d.config.ChannelID, guildMute, guildDeaf)
		voiceCh <- voiceResult{vc, err}
	}()

	// Open the audio FIFO for reading in parallel. FIFO open blocks until a
	// writer (shairport) connects; in the common AirPlay-playback flow this
	// happens right after shairport's `run_this_before_play_begins` hook
	// (which fired our IPC `start` and got its `OK` back) returns. Running
	// this concurrently with the voice join means we don't serialize the two
	// waits, and we can fail fast if either side exceeds the deadline.
	type fifoResult struct {
		pipe *os.File
		err  error
	}
	fifoCh := make(chan fifoResult, 1)
	go func() {
		pipe, err := os.OpenFile(d.config.PipePath, os.O_RDONLY, 0)
		fifoCh <- fifoResult{pipe, err}
	}()

	// Wait for the voice join result. We block here until it settles.
	var vc *discordgo.VoiceConnection
	select {
	case vr := <-voiceCh:
		log.Printf("ChannelVoiceJoin returned in %s (err=%v)", time.Since(joinStart), vr.err)
		if vr.err != nil {
			// ChannelVoiceJoin already called voice.Close() internally on
			// timeout, but Close() does NOT send the gateway nil-channel OP4,
			// so the bot stays visible in the channel. Call Disconnect() to
			// clear the ghost. vr.vc may be nil if the failure happened before
			// voice registration, so guard.
			if vr.vc != nil {
				_ = vr.vc.Disconnect()
			}
			d.cleanupStreamState()
			// Drain the FIFO goroutine: it's blocked on shairport opening the
			// writer side. We can't cancel a blocking os.OpenFile on a FIFO
			// from here, so it will leak until shairport connects or the
			// process exits. Documented as an acceptable tradeoff in README.
			return
		}
		vc = vr.vc
		d.activeMu.Lock()
		d.vc = vc
		d.joining = false
		d.activeMu.Unlock()
	case <-time.After(remaining):
		log.Printf("Voice join exceeded %s deadline — abandoning. The in-flight "+
			"ChannelVoiceJoin sub-goroutine will time out internally (~10s) and "+
			"clean up its own VoiceConnection. No ghost remains. See README "+
			"troubleshooting.", readyTimeout)
		d.cleanupStreamState()
		// voiceCh is buffered so the leaked join goroutine won't block when it
		// eventually completes (we're no longer reading). Same for fifoCh.
		return
	case <-stopCh:
		// stopStreaming was called while we were waiting for the voice join.
		// The in-flight ChannelVoiceJoin will eventually time out internally
		// (~10s) and Close its own VoiceConnection; we don't need to do
		// anything further here.
		log.Println("Stop received while waiting for voice join; abandoning.")
		d.cleanupStreamState()
		return
	}

	// Voice joined. Now wait for the FIFO reader side to connect (shairport
	// opening the writer). Use the same deadline; if playback never
	// materializes we bail and Disconnect.
	remaining = time.Until(deadline)
	var pipe *os.File
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
		// stopStreaming was called while we were waiting for the FIFO writer.
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

	byteBuf := make([]byte, pcmFrameBytes)
	pcmBuf := make([]int16, pcmFrameSize)
	opusBuf := make([]byte, maxOpusPacketSize)

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

		// Convert little-endian PCM byte buffer to int16 samples.
		decodePCMFrame(byteBuf, pcmBuf)

		// Encode PCM samples to an Opus packet.
		n, err := enc.Encode(pcmBuf, opusBuf)
		if err != nil {
			log.Printf("Opus encoding error: %v", err)
			return
		}

		// Clone the slice since the channel send is async and we reuse opusBuf.
		packet := make([]byte, n)
		copy(packet, opusBuf[:n])

		select {
		case vc.OpusSend <- packet:
		default:
			log.Println("Warning: OpusSend channel full, dropping packet")
		}
	}
}

// decodePCMFrame converts a little-endian S16LE byte buffer into int16 samples.
// It is the pure (side-effect-free) inverse of the wire format shairport-sync
// emits on its pipe backend. byteBuf must be exactly 2*len(pcmBuf) bytes.
func decodePCMFrame(byteBuf []byte, pcmBuf []int16) {
	for i := range pcmBuf {
		pcmBuf[i] = int16(binary.LittleEndian.Uint16(byteBuf[i*2 : i*2+2]))
	}
}
