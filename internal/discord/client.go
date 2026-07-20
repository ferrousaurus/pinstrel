// Package discord adapts discordgo's gateway, voice, and slash-command surface
// to a narrow interface so the daemon's stream lifecycle and command dispatch
// can be tested without a live Discord gateway.
//
// The adapter is split across three files:
//   - client.go: *Discord core (New/Open/Close) and the Session interface that
//     the daemon consumes.
//   - voice.go: voice-specific surface (Connection, JoinChannel,
//     ResolveUserVoiceState, AddDiagnostics).
//   - slash.go: slash command framework (Command, RegisterSlashCommands) and
//     builtins (PingCommand).
package discord

import (
	"errors"
	"fmt"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// Session is the full Discord interface the daemon needs: gateway lifecycle,
// resolution of a configured user's current voice channel (from the gateway
// state cache), diagnostic log handlers, voice channel joins, and slash-command
// registration. *Discord implements it against a real *discordgo.Session; tests
// substitute a mock to exercise the stream state machine and command dispatch
// without a live gateway.
type Session interface {
	Open() error
	Close() error
	// ResolveUserVoiceState returns the guild ID and channel ID of the voice
	// channel the given user is currently in, by walking the bot's gateway
	// state cache. Returns ErrUserNotInVoice if the user is not currently in
	// any voice channel (across all guilds the bot shares with the user), or
	// ErrUserSharesNoGuild if the bot is in zero guilds and therefore cannot
	// observe any user's voice state. Both are sentinel errors suitable for
	// errors.Is.
	ResolveUserVoiceState(userID string) (guildID, channelID string, err error)
	AddDiagnostics()
	JoinChannel(guildID, channelID string, mute, deaf bool) (Connection, error)
	RegisterSlashCommands(cmds []Command) error
}

// Sentinel errors returned by ResolveUserVoiceState. They are exported so the
// daemon can errors.Is-match them against the resolution outcome and translate
// into precise journalctl / shairport-hook diagnostics.
var (
	// ErrUserNotInVoice is returned when the configured user is not currently
	// in any voice channel across all guilds the bot shares with the user.
	ErrUserNotInVoice = errors.New("configured user is not currently in a voice channel")
	// ErrUserSharesNoGuild is returned when the bot is in zero guilds and
	// therefore cannot observe any user's voice state. The operator should
	// invite the bot to a server the configured user is in.
	ErrUserSharesNoGuild = errors.New("bot and configured user share no guild; invite the bot to a server the user is in")
)

// Discord adapts *discordgo.Session to the Session interface. It owns the
// gateway connection and exposes voice join as a Connection interface so the
// daemon never touches discordgo types directly.
type Discord struct {
	dg *discordgo.Session
	// slashMu guards slashRegistered. RegisterSlashCommands is a "call once
	// at startup" method; the mutex + flag keep it safe if it isn't.
	slashMu sync.Mutex
	// slashRegistered is set by RegisterSlashCommands to avoid stacking
	// duplicate InteractionCreate handlers on repeated calls.
	slashRegistered bool
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