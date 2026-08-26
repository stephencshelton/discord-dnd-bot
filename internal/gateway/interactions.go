package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

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
	if !g.allowsGuild(i.GuildID) {
		g.log.Warn("ignored command from unconfigured guild", "guild", i.GuildID)
		_ = g.reply(s, i, "This bot is not configured for this server.", true)
		return
	}

	name := i.ApplicationCommandData().Name
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), interactionTimeout)
	defer cancel()

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
	default:
		err = fmt.Errorf("unknown command %q", name)
	}

	status := "ok"
	if err != nil {
		status = "error"
		g.log.Error("command failed", "command", name, "err", err)
		g.replyError(s, i, err)
	}
	metrics.CommandsTotal.WithLabelValues(name, status).Inc()
	metrics.CommandDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())
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
