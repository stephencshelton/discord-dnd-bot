package gateway

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"

	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
)

// interactionTimeout bounds any single interaction handler.
const interactionTimeout = 15 * time.Second

// ictx adapts a disgo slash-command interaction event to the helper-based API
// the command handlers use. It centralizes replies, deferrals, followups, and
// option reads so handler bodies stay small and disgo-agnostic.
type ictx struct {
	g    *Gateway
	e    *events.ApplicationCommandInteractionCreate
	data discord.SlashCommandInteractionData
}

// guildID returns the interaction's guild ID as a string, or "" in a DM.
func (ic *ictx) guildID() string {
	if gid := ic.e.GuildID(); gid != nil {
		return gid.String()
	}
	return ""
}

// userID returns the invoking user's ID as a string.
func (ic *ictx) userID() string { return ic.e.User().ID.String() }

// displayName returns the best display name for the invoking user: guild nick >
// global name > username, falling back to "someone".
func (ic *ictx) displayName() string {
	if m := ic.e.Member(); m != nil {
		if name := m.EffectiveName(); name != "" {
			return name
		}
	}
	if name := ic.e.User().EffectiveName(); name != "" {
		return name
	}
	return "someone"
}

// channelID returns the interaction's channel ID as a string.
func (ic *ictx) channelID() string { return ic.e.Channel().ID().String() }

// commandName returns the top-level command name.
func (ic *ictx) commandName() string { return ic.data.CommandName() }

// subcommand returns the invoked subcommand name (last path segment), or "".
func (ic *ictx) subcommand() string {
	// CommandPath is like "campaign/create" or "campaign/sub/leaf".
	path := ic.data.CommandPath()
	last := ""
	seg := ""
	for _, r := range path {
		if r == '/' {
			last = seg
			seg = ""
			continue
		}
		seg += string(r)
	}
	if seg != "" && last != "" {
		return seg
	}
	return seg
}

// option reads (disgo flattens options across subcommands).
func (ic *ictx) optString(name string) string { return ic.data.String(name) }
func (ic *ictx) optInt(name string) int       { return ic.data.Int(name) }

// optChannel returns a selected channel option's ID as a string, or "".
func (ic *ictx) optChannel(name string) string {
	if ch, ok := ic.data.OptChannel(name); ok {
		return ch.ID.String()
	}
	return ""
}

// --- reply helpers ---

func ephemeralFlags(ephemeral bool) discord.MessageFlags {
	if ephemeral {
		return discord.MessageFlagEphemeral
	}
	return 0
}

// reply sends an immediate text response.
func (ic *ictx) reply(content string, ephemeral bool) error {
	return ic.e.CreateMessage(discord.MessageCreate{Content: content, Flags: ephemeralFlags(ephemeral)})
}

// replyEmbed sends an immediate embed response.
func (ic *ictx) replyEmbed(embed discord.Embed) error {
	return ic.e.CreateMessage(discord.MessageCreate{Embeds: []discord.Embed{embed}})
}

// replyEmbedEphemeral sends an immediate ephemeral embed response.
func (ic *ictx) replyEmbedEphemeral(embed discord.Embed) error {
	return ic.e.CreateMessage(discord.MessageCreate{
		Embeds: []discord.Embed{embed},
		Flags:  discord.MessageFlagEphemeral,
	})
}

// ack sends a deferred ("thinking...") response so we have up to 15 minutes to
// follow up. Used for anything that touches the DB or AI.
func (ic *ictx) ack(ephemeral bool) error {
	return ic.e.DeferCreateMessage(ephemeral)
}

// followup edits the deferred response with final text content.
func (ic *ictx) followup(content string) error {
	_, err := ic.e.Client().Rest.UpdateInteractionResponse(
		ic.e.ApplicationID(), ic.e.Token(),
		discord.MessageUpdate{Content: &content},
	)
	return err
}

// followupEmbed edits the deferred response with an embed.
func (ic *ictx) followupEmbed(embed discord.Embed) error {
	embeds := []discord.Embed{embed}
	_, err := ic.e.Client().Rest.UpdateInteractionResponse(
		ic.e.ApplicationID(), ic.e.Token(),
		discord.MessageUpdate{Embeds: &embeds},
	)
	return err
}

// replyError reports a generic error to the user; details are logged by the
// caller. It tries to edit an existing deferred response, else sends fresh.
func (ic *ictx) replyError() {
	msg := "Something went wrong. Please try again."
	if _, err := ic.e.Client().Rest.UpdateInteractionResponse(
		ic.e.ApplicationID(), ic.e.Token(),
		discord.MessageUpdate{Content: &msg},
	); err != nil {
		_ = ic.reply(msg, true)
	}
}

// onInteraction is the disgo listener for slash-command interactions.
//
// disgo dispatches events SYNCHRONOUSLY while holding its event-listener mutex
// (asyncEventsEnabled is off). Some commands (notably /session start) block for
// many seconds inside conn.Open waiting for VOICE_STATE_UPDATE/
// VOICE_SERVER_UPDATE — but disgo delivers those very events through the same
// mutex-guarded dispatch path, so running the handler inline deadlocks the
// gateway: the voice events can't arrive until the handler returns, and the
// handler can't return until the voice events arrive (they only flush after our
// timeout). Running the command on its own goroutine frees the dispatch loop so
// the events flow while the handler waits. routeCommand has its own panic
// recovery and acks the interaction first, so the token stays valid.
func (g *Gateway) onInteraction(e *events.ApplicationCommandInteractionCreate) {
	data, ok := e.Data.(discord.SlashCommandInteractionData)
	if !ok {
		return // not a slash command (e.g. user/message command); unused here
	}
	ic := &ictx{g: g, e: e, data: data}
	go g.routeCommand(ic)
}

func (g *Gateway) routeCommand(ic *ictx) {
	name := ic.commandName()
	guildID := ic.guildID()

	// DM-capable global commands (e.g. /dm-server) arrive with an empty guild
	// and must bypass the guild allowlist check. Everything else must come from
	// a configured guild.
	if guildID != "" || !isDMCommand(name) {
		if !g.allowsGuild(guildID) {
			g.log.Warn("ignored command from unconfigured guild", "guild", guildID, "command", name)
			_ = ic.reply("This bot is not configured for this server.", true)
			return
		}
	}

	start := time.Now()

	corrID := logging.NewCorrelationID()
	userID := ic.userID()
	log := g.log.With(
		logging.CorrelationIDField, corrID,
		"command", name,
		"guild", guildID,
		"user", userID,
	)
	ctx, cancel := context.WithTimeout(context.Background(), interactionTimeout)
	defer cancel()
	ctx = logging.WithLogger(logging.WithCorrelationID(ctx, g.log, corrID), log)

	log.Info("command received")

	status := "ok"
	defer func() {
		if r := recover(); r != nil {
			status = "panic"
			metrics.PanicsRecovered.WithLabelValues("interaction").Inc()
			log.Error("command panicked; recovered",
				"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			ic.replyError()
		}
		metrics.CommandsTotal.WithLabelValues(name, status).Inc()
		dur := time.Since(start)
		metrics.CommandDuration.WithLabelValues(name).Observe(dur.Seconds())
		log.Info("command handled", "status", status, "duration_ms", dur.Milliseconds())
	}()

	var err error
	switch name {
	case "campaign":
		err = g.handleCampaign(ctx, ic)
	case "character":
		err = g.handleCharacter(ctx, ic)
	case "world":
		err = g.handleWorld(ctx, ic)
	case "session":
		err = g.handleSession(ctx, ic)
	case "roll":
		err = g.handleRoll(ctx, ic)
	case "lore":
		err = g.handleLore(ctx, ic)
	case "recap":
		err = g.handleRecap(ctx, ic)
	case "search":
		err = g.handleSearch(ctx, ic)
	case "ask":
		err = g.handleAsk(ctx, ic)
	case "art":
		err = g.handleArt(ctx, ic)
	case "remind":
		err = g.handleRemind(ctx, ic)
	case "notes-channel":
		err = g.handleNotesChannel(ctx, ic)
	case "reindex":
		err = g.handleReindex(ctx, ic)
	case "feedback":
		err = g.handleFeedback(ctx, ic)
	case "help":
		err = g.handleHelp(ctx, ic)
	case "dm-server":
		err = g.handleDMServer(ctx, ic)
	default:
		err = fmt.Errorf("unknown command %q", name)
	}

	if err != nil {
		status = "error"
		log.Error("command failed", "err", err)
		ic.replyError()
	}
}

// isDMCommand reports whether a command is a global, DM-capable command that
// must be allowed to run outside a configured guild.
func isDMCommand(name string) bool {
	_, ok := dmCapableCommands()[name]
	return ok
}
