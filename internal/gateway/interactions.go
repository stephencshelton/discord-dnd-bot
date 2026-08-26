package gateway

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
)

// interactionTimeout bounds any single interaction handler.
const interactionTimeout = 15 * time.Second

// onInteraction is the top-level router for slash commands and autocomplete.
func (g *Gateway) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		g.routeCommand(s, i)
	case discordgo.InteractionApplicationCommandAutocomplete:
		g.routeAutocomplete(s, i)
	}
}

func (g *Gateway) routeCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	name := i.ApplicationCommandData().Name

	// DM-capable global commands (e.g. /dm-server) arrive with an empty GuildID
	// and must bypass the guild allowlist check. Everything else must come from
	// a configured guild.
	if i.GuildID != "" || !isDMCommand(name) {
		if !g.allowsGuild(i.GuildID) {
			g.log.Warn("ignored command from unconfigured guild", "guild", i.GuildID, "command", name)
			_ = g.reply(s, i, "This bot is not configured for this server.", true)
			return
		}
	}

	start := time.Now()

	// Build a per-interaction correlation ID + child logger so every log line
	// for this command (and any job it enqueues) can be correlated, and stash
	// them on the context threaded into handlers.
	corrID := logging.NewCorrelationID()
	userID := interactionUserID(i)
	log := g.log.With(
		logging.CorrelationIDField, corrID,
		"command", name,
		"guild", i.GuildID,
		"user", userID,
	)
	ctx, cancel := context.WithTimeout(context.Background(), interactionTimeout)
	defer cancel()
	ctx = logging.WithLogger(logging.WithCorrelationID(ctx, g.log, corrID), log)

	log.Info("command received")

	status := "ok"
	// Recover from panics in any handler so a single bad command can never
	// crash the gateway (discordgo dispatches handlers in their own goroutines,
	// where an unrecovered panic would take down the whole process).
	defer func() {
		if r := recover(); r != nil {
			status = "panic"
			metrics.PanicsRecovered.WithLabelValues("interaction").Inc()
			log.Error("command panicked; recovered",
				"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			g.replyError(s, i, fmt.Errorf("internal error"))
		}
		metrics.CommandsTotal.WithLabelValues(name, status).Inc()
		dur := time.Since(start)
		metrics.CommandDuration.WithLabelValues(name).Observe(dur.Seconds())
		log.Info("command handled", "status", status, "duration_ms", dur.Milliseconds())
	}()

	var err error
	switch name {
	case "campaign":
		err = g.handleCampaign(ctx, s, i)
	case "character":
		err = g.handleCharacter(ctx, s, i)
	case "world":
		err = g.handleWorld(ctx, s, i)
	case "session":
		err = g.handleSession(ctx, s, i)
	case "roll":
		err = g.handleRoll(ctx, s, i)
	case "lore":
		err = g.handleLore(ctx, s, i)
	case "recap":
		err = g.handleRecap(ctx, s, i)
	case "search":
		err = g.handleSearch(ctx, s, i)
	case "ask":
		err = g.handleAsk(ctx, s, i)
	case "art":
		err = g.handleArt(ctx, s, i)
	case "remind":
		err = g.handleRemind(ctx, s, i)
	case "notes-channel":
		err = g.handleNotesChannel(ctx, s, i)
	case "reindex":
		err = g.handleReindex(ctx, s, i)
	case "feedback":
		err = g.handleFeedback(ctx, s, i)
	case "help":
		err = g.handleHelp(ctx, s, i)
	case "dm-server":
		err = g.handleDMServer(ctx, s, i)
	default:
		err = fmt.Errorf("unknown command %q", name)
	}

	if err != nil {
		status = "error"
		log.Error("command failed", "err", err)
		g.replyError(s, i, err)
	}
}

// interactionUserID extracts the invoking user's ID from an interaction,
// handling both guild (Member) and DM (User) contexts.
func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// isDMCommand reports whether a command is a global, DM-capable command that
// must be allowed to run outside a configured guild.
func isDMCommand(name string) bool {
	_, ok := dmCapableCommands()[name]
	return ok
}

// --- interaction reply helpers ---

// ack sends a deferred ("thinking...") response so we have up to 15 minutes to
// follow up. Used for anything that touches the DB or AI.
func (g *Gateway) ack(s *discordgo.Session, i *discordgo.InteractionCreate, ephemeral bool) error {
	data := &discordgo.InteractionResponseData{}
	if ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: data,
	})
}

// followup edits the deferred response with final content.
func (g *Gateway) followup(s *discordgo.Session, i *discordgo.InteractionCreate, content string) error {
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
	return err
}

// followupEmbed edits the deferred response with an embed.
func (g *Gateway) followupEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, e *discordgo.MessageEmbed) error {
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{e},
	})
	return err
}

// reply sends an immediate ephemeral text response (for validation errors etc.).
func (g *Gateway) reply(s *discordgo.Session, i *discordgo.InteractionCreate, content string, ephemeral bool) error {
	data := &discordgo.InteractionResponseData{Content: content}
	if ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: data,
	})
}

// replyEmbed sends an immediate embed response.
func (g *Gateway) replyEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}},
	})
}

// replyError reports a generic error to the user; internal details are logged
// by the caller, never shown.
func (g *Gateway) replyError(s *discordgo.Session, i *discordgo.InteractionCreate, err error) {
	msg := "Something went wrong. Please try again."
	// If we've already acked, follow up; otherwise send fresh.
	if _, e := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg}); e != nil {
		_ = g.reply(s, i, msg, true)
	}
}

// optString extracts a string option (empty if absent).
func optString(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, o := range opts {
		if o.Name == name {
			return o.StringValue()
		}
	}
	return ""
}

// optInt extracts an integer option (0 if absent).
func optInt(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) int {
	for _, o := range opts {
		if o.Name == name {
			return int(o.IntValue())
		}
	}
	return 0
}

// optChannel extracts a channel option ID.
func optChannel(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, o := range opts {
		if o.Name == name {
			return o.ChannelValue(nil).ID
		}
	}
	return ""
}

// interactionUser returns the invoking user's ID, whether in a guild or DM.
func interactionUser(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}
