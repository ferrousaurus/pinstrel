package discord

import (
	"fmt"
	"log"

	"github.com/bwmarrin/discordgo"
)

// Command is the contract a slash command implements. The adapter's
// RegisterSlashCommands bulk-overwrites the bot's application commands from a
// slice of Command and routes incoming InteractionCreate events to the
// matching Handle by Name. Add a new command by implementing Command and
// passing an instance to RegisterSlashCommands alongside the existing ones.
type Command interface {
	// Name is the slash command name as registered with Discord (e.g. "ping").
	// Must be lowercase, 1-32 chars, match ^[-_a-z0-9]{1,32}$ — Discord rejects
	// the bulk overwrite otherwise.
	Name() string
	// Description is the user-visible slash command description (1-100 chars).
	Description() string
	// Handle is invoked when an InteractionCreate event arrives whose command
	// name matches Name. It MUST respond (via s.InteractionRespond or another
	// interaction response endpoint); Discord shows an "interaction failed"
	// message to the invoker if no response arrives within ~3s.
	Handle(s *discordgo.Session, i *discordgo.InteractionCreate)
}

// RegisterSlashCommands bulk-overwrites the bot's global application commands
// to exactly the given set and registers a single InteractionCreate handler
// that dispatches to the matching Command.Handle. Must be called after Open
// (it reads d.dg.State.User.ID, which is populated by the READY event).
//
// Bulk overwrite is idempotent for unchanged command definitions, so calling
// this every daemon boot is the intended pattern — redefinitions propagate on
// the next restart without manual cleanup. Calling it more than once per
// *Discord is a no-op (guarded by slashRegistered) to avoid stacking duplicate
// InteractionCreate handlers.
//
// An error here is fatal in daemon.Run: a half-registered bot is worse than a
// clean failure, mirroring the "missing config is a hard error" posture. If a
// future bot lacks the applications.commands scope and we want non-fatal
// slash, demote this to a logged warning in the caller rather than here —
// keep the adapter honest about failures.
func (d *Discord) RegisterSlashCommands(cmds []Command) error {
	d.slashMu.Lock()
	defer d.slashMu.Unlock()
	if d.slashRegistered {
		log.Println("Slash commands already registered — skipping duplicate call")
		return nil
	}
	if d.dg.State == nil || d.dg.State.User == nil {
		return fmt.Errorf("discord session not ready: State.User is nil (Open must complete first)")
	}

	apps := make([]*discordgo.ApplicationCommand, 0, len(cmds))
	byName := make(map[string]Command, len(cmds))
	for _, c := range cmds {
		apps = append(apps, &discordgo.ApplicationCommand{
			Name:        c.Name(),
			Description: c.Description(),
		})
		byName[c.Name()] = c
	}

	appID := d.dg.State.User.ID
	if _, err := d.dg.ApplicationCommandBulkOverwrite(appID, "", apps); err != nil {
		return fmt.Errorf("bulk overwrite application commands: %w", err)
	}

	// Single dispatch handler — one AddHandler call per *Discord, regardless
	// of how many commands are registered. byName is captured by closure.
	d.dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		data := i.ApplicationCommandData()
		cmd, ok := byName[data.Name]
		if !ok {
			log.Printf("InteractionCreate: no handler for command %q", data.Name)
			return
		}
		cmd.Handle(s, i)
	})

	d.slashRegistered = true
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.Name())
	}
	log.Printf("Registered slash commands: %v", names)
	return nil
}

// PingCommand is the canonical "ping → Pong!" slash command. Useful as a
// smoke test that the bot is alive and slash-command routing works.
type PingCommand struct{}

func (PingCommand) Name() string        { return "ping" }
func (PingCommand) Description() string { return "Ping the bot" }

// Handle responds with "Pong!" as a channel message. If InteractionRespond
// fails, the error is logged — by that point Discord has already shown the
// invoker "the application did not respond" if it timed out, so there is no
// further user-facing recovery to attempt.
//
// Note on the i.Interaction dereference: discordgo's InteractionCreate event
// embeds *Interaction, and the pinned DAVE-capable fork's InteractionRespond
// takes *Interaction (not *InteractionCreate as upstream once did). The
// embedded field access is the fork-idiomatic way to bridge the two.
func (PingCommand) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	resp := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Pong!",
		},
	}
	if err := s.InteractionRespond(i.Interaction, resp); err != nil {
		log.Printf("ping: InteractionRespond failed: %v", err)
	}
}

type PlayCommand struct{}

func (PlayCommand) Name() string        { return "play" }
func (PlayCommand) Description() string { return "Play audio via a link" }

func IsPlayable(url string) error {
	return fmt.Errorf("%s is an invalid link.", url)
}

func (PlayCommand) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	err := IsPlayable("")
	if err != nil {
		resp := &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: err.Error(),
			},
		}
		if err := s.InteractionRespond(i.Interaction, resp); err != nil {
			log.Printf("play: InteractionRespond failed: %v", err)
			return
		}
		log.Printf("play: An error occurred: %v", err)
		return
	}
	resp := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Playing the audio",
		},
	}
	if err := s.InteractionRespond(i.Interaction, resp); err != nil {
		log.Printf("play: InteractionRespond failed: %v", err)
	}
}

// Compile-time assertion that PingCommand satisfies Command. Mirrors the
// pattern in client_test.go for the Session/Connection interfaces — a drift
// is caught by go test rather than at the call site in daemon.Run.
var _ Command = PingCommand{}
