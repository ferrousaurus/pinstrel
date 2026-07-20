package discord

import (
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

// discordConnection adapts *discordgo.VoiceConnection to the Connection
// interface. It is unexported because the daemon only ever holds a
// Connection — the concrete type never escapes this package.
type discordConnection struct {
	vc *discordgo.VoiceConnection
}

func (c *discordConnection) Speaking(on bool) error  { return c.vc.Speaking(on) }
func (c *discordConnection) Disconnect() error       { return c.vc.Disconnect() }
func (c *discordConnection) OpusSend() chan<- []byte { return c.vc.OpusSend }

// ResolveUserVoiceState returns the guild ID and channel ID of the voice
// channel the given user is currently in. It walks the bot's gateway state
// cache (populated from GUILD_CREATE on Open() and maintained by
// VOICE_STATE_UPDATE thereafter), so this is a cache read, not a REST call.
//
// Returns:
//   - (guildID, channelID, nil) on the first guild in which the user has a
//     voice state with a non-empty ChannelID.
//   - ("", "", ErrUserNotInVoice) if the user has no voice state in any of
//     the bot's guilds (i.e. is not currently in a voice channel anywhere
//     the bot can see).
//   - ("", "", ErrUserSharesNoGuild) if the bot is in zero guilds, in which
//     case it cannot observe any user's voice state at all.
//
// Precondition: the bot and the configured user must share at least one guild.
// This is documented in the README; if violated, ErrUserSharesNoGuild fires
// only when the bot is in zero guilds total — if the bot shares a guild with
// some other users but not the configured one, the lookup falls through to the
// ErrUserNotInVoice path (the bot can't distinguish "shares a guild but user
// is in no voice channel" from "shares no guild" beyond that).
func (d *Discord) ResolveUserVoiceState(userID string) (string, string, error) {
	// discordgo's State is a cached structure guarded by an internal mutex.
	// Guilds() (the field, not a method) is the slice of all guilds the bot
	// knows about; VoiceState(guildID, userID) is the locked accessor that
	// returns the user's cached VoiceState for that guild or an error if the
	// user has no cached voice state there.
	d.dg.State.RLock()
	guilds := d.dg.State.Guilds
	d.dg.State.RUnlock()

	if len(guilds) == 0 {
		log.Printf("ResolveUserVoiceState: bot is in zero guilds; cannot observe user %s", userID)
		return "", "", ErrUserSharesNoGuild
	}
	for _, g := range guilds {
		vs, err := d.dg.State.VoiceState(g.ID, userID)
		if err != nil || vs == nil {
			// discordgo.State.VoiceState returns a non-nil error when the
			// user has no cached voice state in this guild. Continue walking.
			continue
		}
		if vs.ChannelID == "" {
			// Voice state exists but the user has left the channel (empty
			// ChannelID = not in voice). Continue walking other guilds in
			// case the user is in voice in a different shared guild.
			continue
		}
		log.Printf("ResolveUserVoiceState: user %s is in channel %s of guild %s", userID, vs.ChannelID, g.ID)
		return g.ID, vs.ChannelID, nil
	}
	log.Printf("ResolveUserVoiceState: user %s is not in a voice channel in any of %d shared guilds", userID, len(guilds))
	return "", "", ErrUserNotInVoice
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