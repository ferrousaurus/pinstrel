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

// Daemon manages the Discord session, Unix socket server, and audio stream.
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
	// Surface discordgo's internal voice-handshake logs so journald shows which
	// stage stalls when a `timeout waiting for voice` occurs (see README
	// troubleshooting). LogInformational emits the "connecting to voice endpoint",
	// "connecting to udp addr", and "udp read error" lines that diagnose the
	// known net.DialUDP connected-socket reply-drop failure mode.
	dg.LogLevel = discordgo.LogInformational

	// Query Discord API to find the Guild ID corresponding to the voice channel
	log.Printf("Connecting to Discord to resolve channel info for %s...", cfg.ChannelID)
	ch, err := dg.Channel(cfg.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve channel details: %w (verify Discord Token and Channel ID)", err)
	}
	guildID := ch.GuildID
	log.Printf("Successfully resolved channel %s to Guild ID %s", cfg.ChannelID, guildID)

	return &Daemon{
		config:  cfg,
		dg:      dg,
		guildID: guildID,
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
		// Discord fires VOICE_STATE_UPDATE for *every* user in the guild; only
		// log the bot's own state to avoid noise. We compare against the bot's
		// own user id, which discordgo populates on READY into s.State.User.
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
		// token redacted intentionally — never log Discord voice tokens.
		log.Printf("VOICE_SERVER_UPDATE: guild=%s endpoint=%s token_present=%t",
			vss.GuildID, vss.Endpoint, vss.Token != "")
	})

	// Ensure socket directory exists
	// (usually /tmp, but good to handle)
	_ = os.Remove(d.config.SocketPath)

	listener, err := net.Listen("unix", d.config.SocketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket %s: %w", d.config.SocketPath, err)
	}
	defer listener.Close()
	defer os.Remove(d.config.SocketPath)

	// Grant write permissions to the socket so other users (e.g. shairport-sync) can communicate with it
	if err := os.Chmod(d.config.SocketPath, 0777); err != nil {
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

// handleIPC handles client requests (start/stop) on the Unix domain socket.
func (d *Daemon) handleIPC(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		cmd := scanner.Text()
		log.Printf("Received IPC command: %s", cmd)
		switch cmd {
		case "start":
			if err := d.startStreaming(); err != nil {
				log.Printf("Error starting stream: %v", err)
				_, _ = conn.Write([]byte(fmt.Sprintf("ERR: %v\n", err)))
			} else {
				_, _ = conn.Write([]byte("OK\n"))
			}
		case "stop":
			if err := d.stopStreaming(); err != nil {
				log.Printf("Error stopping stream: %v", err)
				_, _ = conn.Write([]byte(fmt.Sprintf("ERR: %v\n", err)))
			} else {
				_, _ = conn.Write([]byte("OK\n"))
			}
		default:
			_, _ = conn.Write([]byte("ERR: Unknown command\n"))
		}
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
	enc, err := opus.NewEncoder(48000, 2, opus.AppAudio)
	if err != nil {
		log.Printf("Failed to create Opus encoder: %v", err)
		d.activeMu.Lock()
		d.isStreaming = false
		d.joining = false
		d.activeMu.Unlock()
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
		readyTimeout = 30 * time.Second // guard against misconfigured 0/negative
	}
	deadline := time.Now().Add(readyTimeout)
	remaining := time.Until(deadline)

	// stopCh unifies the stopStreaming path with the deadline-based waits
	// below. A tiny ticker goroutine polls d.isStreaming every 100ms and
	// signals stopCh when it flips false. We close stopCh only when streamLoop
	// itself is exiting so the ticker goroutine can return; on the happy path
	// the ticker goroutine exits via the stopCh close in the deferred cleanup.
	stopCh := make(chan struct{})
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				d.activeMu.Lock()
				stillStreaming := d.isStreaming
				d.activeMu.Unlock()
				if !stillStreaming {
					select {
					case stopCh <- struct{}{}:
					default:
					}
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
		vc, err := d.dg.ChannelVoiceJoin(d.guildID, d.config.ChannelID, false, true)
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
			d.activeMu.Lock()
			d.isStreaming = false
			d.joining = false
			d.activeMu.Unlock()
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
		d.activeMu.Lock()
		d.isStreaming = false
		d.joining = false
		d.activeMu.Unlock()
		// voiceCh is buffered so the leaked join goroutine won't block when it
		// eventually completes (we're no longer reading). Same for fifoCh.
		return
	case <-stopCh:
		// stopStreaming was called while we were waiting for the voice join.
		// The in-flight ChannelVoiceJoin will eventually time out internally
		// (~10s) and Close its own VoiceConnection; we don't need to do
		// anything further here.
		log.Println("Stop received while waiting for voice join; abandoning.")
		d.activeMu.Lock()
		d.isStreaming = false
		d.joining = false
		d.activeMu.Unlock()
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
			_ = vc.Speaking(false)
			_ = vc.Disconnect()
			d.activeMu.Lock()
			d.isStreaming = false
			if d.vc != nil {
				d.vc = nil
			}
			d.activeMu.Unlock()
			return
		}
		pipe = fr.pipe
	case <-time.After(remaining):
		log.Printf("Timed out waiting for shairport to open the audio FIFO writer "+
			"within %s — disconnecting.", readyTimeout)
		_ = vc.Speaking(false)
		_ = vc.Disconnect()
		d.activeMu.Lock()
		d.isStreaming = false
		if d.vc != nil {
			d.vc = nil
		}
		d.activeMu.Unlock()
		return
	case <-stopCh:
		// stopStreaming was called while we were waiting for the FIFO writer.
		log.Println("Stop received while waiting for FIFO writer; disconnecting.")
		_ = vc.Speaking(false)
		_ = vc.Disconnect()
		d.activeMu.Lock()
		d.isStreaming = false
		if d.vc != nil {
			d.vc = nil
		}
		d.activeMu.Unlock()
		return
	}

	d.activeMu.Lock()
	d.activePipe = pipe
	d.activeMu.Unlock()

	defer func() {
		log.Println("Audio streaming loop exited")
		d.activeMu.Lock()
		// If stopStreaming already ran, it set isStreaming=false and d.vc=nil
		// (and closed d.activePipe). Only clean up if we still own the state.
		if d.isStreaming {
			d.isStreaming = false
			if d.activePipe != nil {
				d.activePipe.Close()
				d.activePipe = nil
			}
			if d.vc != nil {
				_ = d.vc.Speaking(false)
				_ = d.vc.Disconnect()
				d.vc = nil
			}
		} else if d.activePipe != nil {
			// Defensive: stopStreaming clears d.activePipe, but if we got here
			// via the read-loop EOF with a non-nil pipe the FD must close.
			d.activePipe.Close()
			d.activePipe = nil
		}
		d.activeMu.Unlock()
	}()

	_ = vc.Speaking(true)
	defer func() { _ = vc.Speaking(false) }()

	// Calculations for 20ms frame:
	// 48000 Hz * 0.02s = 960 samples per channel
	// 960 samples * 2 channels = 1920 total samples
	// 1920 samples * 2 bytes/sample = 3840 bytes PCM frame
	const frameSamples = 960
	const channels = 2
	const pcmSize = frameSamples * channels
	const byteSize = pcmSize * 2

	byteBuf := make([]byte, byteSize)
	pcmBuf := make([]int16, pcmSize)
	opusBuf := make([]byte, 1024)

	for {
		// Read 3840 bytes. This blocks until the pipe has sufficient data.
		_, err := io.ReadFull(pipe, byteBuf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrClosed) {
				log.Println("Named pipe EOF or connection closed.")
			} else {
				log.Printf("Read error on named pipe: %v", err)
			}
			return
		}

		// Convert Little-Endian PCM byte buffer to int16 samples
		for i := 0; i < pcmSize; i++ {
			pcmBuf[i] = int16(binary.LittleEndian.Uint16(byteBuf[i*2 : i*2+2]))
		}

		// Encode PCM samples to Opus packet
		n, err := enc.Encode(pcmBuf, opusBuf)
		if err != nil {
			log.Printf("Opus encoding error: %v", err)
			return
		}

		// Clone the slice since the channel send is async and we reuse opusBuf
		packet := make([]byte, n)
		copy(packet, opusBuf[:n])

		// Send packet to Discord voice gateway
		select {
		case vc.OpusSend <- packet:
		default:
			// If the channel queue is full, we print a warning to monitor buffering issues.
			// We do not block to prevent lagging behind real-time.
			log.Println("Warning: OpusSend channel full, dropping packet")
		}
	}
}
