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
}

// NewDaemon initializes a new Daemon, resolving the GuildID from the ChannelID.
func NewDaemon(cfg *Config) (*Daemon, error) {
	dg, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create Discord session: %w", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates

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

// startStreaming joins the Discord voice channel and begins reading from the named pipe.
func (d *Daemon) startStreaming() error {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()

	if d.isStreaming {
		log.Println("Already streaming, ignoring start command")
		return nil
	}

	// Pre-create the named pipe (FIFO) if it does not exist
	if _, err := os.Stat(d.config.PipePath); os.IsNotExist(err) {
		log.Printf("Creating named pipe FIFO at %s...", d.config.PipePath)
		if err := syscall.Mkfifo(d.config.PipePath, 0666); err != nil {
			return fmt.Errorf("failed to create named pipe: %w", err)
		}
		// Set pipe permissions to 666 so anyone can write to it
		_ = os.Chmod(d.config.PipePath, 0666)
	}

	log.Printf("Joining Discord voice channel: %s", d.config.ChannelID)
	vc, err := d.dg.ChannelVoiceJoin(d.guildID, d.config.ChannelID, false, true)
	if err != nil {
		return fmt.Errorf("failed to join voice channel: %w", err)
	}
	d.vc = vc

	log.Printf("Opening named pipe: %s", d.config.PipePath)
	// We open the pipe. Since it's a FIFO, this call will block if no writer is open yet.
	// However, we want to open it quickly. Shairport-sync typically has the pipe open or opens it.
	pipe, err := os.OpenFile(d.config.PipePath, os.O_RDONLY, 0)
	if err != nil {
		vc.Disconnect()
		return fmt.Errorf("failed to open named pipe: %w", err)
	}
	d.activePipe = pipe
	d.isStreaming = true

	go d.streamLoop(pipe, vc)

	return nil
}

// stopStreaming closes the named pipe and disconnects the Discord voice bot.
func (d *Daemon) stopStreaming() error {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()

	if !d.isStreaming {
		log.Println("Not streaming, ignoring stop command")
		return nil
	}

	log.Println("Stopping stream and disconnecting from voice...")
	d.isStreaming = false

	if d.activePipe != nil {
		// Closing the file descriptor will unblock any pending reads in streamLoop
		d.activePipe.Close()
		d.activePipe = nil
	}

	if d.vc != nil {
		_ = d.vc.Speaking(false)
		d.vc.Disconnect()
		d.vc = nil
	}

	return nil
}

// streamLoop reads PCM from the pipe, encodes to Opus, and sends to Discord.
func (d *Daemon) streamLoop(pipe *os.File, vc *discordgo.VoiceConnection) {
	log.Println("Audio streaming loop started")
	defer func() {
		log.Println("Audio streaming loop exited")
		d.activeMu.Lock()
		if d.isStreaming {
			d.isStreaming = false
			if d.activePipe != nil {
				d.activePipe.Close()
				d.activePipe = nil
			}
			if d.vc != nil {
				_ = d.vc.Speaking(false)
				d.vc.Disconnect()
				d.vc = nil
			}
		}
		d.activeMu.Unlock()
	}()

	// Initialize Opus encoder (48kHz, Stereo)
	enc, err := opus.NewEncoder(48000, 2, opus.AppAudio)
	if err != nil {
		log.Printf("Failed to create Opus encoder: %v", err)
		return
	}
	if err := enc.SetBitrate(d.config.Bitrate); err != nil {
		log.Printf("Warning: failed to set bitrate to %d: %v", d.config.Bitrate, err)
	}

	// Wait for Voice Connection to become ready
	for !vc.Ready {
		time.Sleep(10 * time.Millisecond)
		d.activeMu.Lock()
		stillStreaming := d.isStreaming
		d.activeMu.Unlock()
		if !stillStreaming {
			return
		}
	}

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
