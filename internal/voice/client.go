// Package voice adapts discordgo's voice connection to a narrow interface so
// the daemon's stream lifecycle can be tested without a live Discord gateway.
package voice

import (
	"fmt"
	"log"

	"github.com/bwmarrin/discordgo"
)

// Connection is the subset of discordgo.VoiceConnection that the daemon's
// stream loop uses: speaking state, disconnect, and the Opus send channel.
type Connection interface {
	Speaking(on bool) error
	Disconnect() error
	OpusSend() chan<- []byte
}

// Session is the full Discord interface the daemon needs: gateway lifecycle,
// guild resolution from a channel ID, diagnostic log handlers, and voice
// channel joins. *Discord implements it against a real *discordgo.Session;
// tests substitute a mock to exercise the stream state machine.
type Session interface {
	Open() error
	Close() error
	ResolveGuild(channelID string) (guildID string, err error)
	AddDiagnostics()
	JoinChannel(guildID, channelID string, mute, deaf bool) (Connection, error)
}

// Discord adapts *discordgo.Session to the Session interface. It owns the
// gateway connection and exposes voice join as a Connection interface so
// the daemon never touches discordgo types directly.
type Discord struct {
	dg *discordgo.Session
}

// New creates a Discord adapter from a bot token. The gateway is not opened
// until Open is called.
func New(token string) (*Discord, error) {
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates
	dg.LogLevel = discordgo.LogInformational
	return &Discord{dg: dg}, nil
}

// Open opens the Discord gateway session.
func (d *Discord) Open() error {
	if err := d.dg.Open(); err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}
	return nil
}

// Close closes the Discord gateway session.
func (d *Discord) Close() error {
	return d.dg.Close()
}

// ResolveGuild looks up the guild (server) ID for a voice channel ID by
// querying the Discord REST API.
func (d *Discord) ResolveGuild(channelID string) (string, error) {
	log.Printf("Connecting to Discord to resolve channel info for %s...", channelID)
	ch, err := d.dg.Channel(channelID)
	if err != nil {
		return "", fmt.Errorf("retrieve channel details: %w (verify Discord Token and Channel ID)", err)
	}
	log.Printf("Successfully resolved channel %s to Guild ID %s", channelID, ch.GuildID)
	return ch.GuildID, nil
}

// AddDiagnostics registers log handlers for the two gateway events that drive
// the voice handshake (VOICE_STATE_UPDATE and VOICE_SERVER_UPDATE). If a
// "timeout waiting for voice" occurs and neither fires, the gateway never
// dispatched the join — see README troubleshooting. The handlers are read-only
// observers; discordgo's own onVoiceServerUpdate does the real work.
func (d *Discord) AddDiagnostics() {
	d.dg.AddHandler(func(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
		// Filter VOICE_STATE_UPDATE events to those fired by the bot.
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
}

// JoinChannel joins a Discord voice channel and returns a Connection. The
// mute/deaf flags set the bot's voice state on join. This call blocks until
// the voice WS/UDP handshake completes or discordgo's internal timeout fires.
func (d *Discord) JoinChannel(guildID, channelID string, mute, deaf bool) (Connection, error) {
	vc, err := d.dg.ChannelVoiceJoin(guildID, channelID, mute, deaf)
	if err != nil {
		return nil, err
	}
	return &discordConnection{vc: vc}, nil
}

// discordConnection adapts *discordgo.VoiceConnection to the Connection interface.
type discordConnection struct {
	vc *discordgo.VoiceConnection
}

func (c *discordConnection) Speaking(on bool) error  { return c.vc.Speaking(on) }
func (c *discordConnection) Disconnect() error       { return c.vc.Disconnect() }
func (c *discordConnection) OpusSend() chan<- []byte { return c.vc.OpusSend }
