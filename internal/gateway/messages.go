package gateway

import (
	"context"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/prompts"
)

// onMessageCreate powers conversational chat: @mentions in a guild, and DMs to
// the bot (opt-in via DISCORD_ALLOW_DIRECT_MESSAGES). Both route through the
// chat model with the active campaign as context.
func (g *Gateway) onMessageCreate(s *discordgo.Session, mc *discordgo.MessageCreate) {
	if mc.Author == nil || mc.Author.Bot {
		return
	}

	isDM := mc.GuildID == ""
	var guildID string
	if isDM {
		var allowed bool
		guildID, allowed = g.directMessageGuildID(mc.Author.ID)
		if !allowed {
			return
		}
	} else if !g.allowsGuild(mc.GuildID) {
		return
	} else {
		guildID = mc.GuildID
	}

	// State.User is populated on Ready; guard against an early message.
	if s.State == nil || s.State.User == nil {
		return
	}
	selfID := s.State.User.ID

	mentioned := false
	for _, u := range mc.Mentions {
		if u.ID == selfID {
			mentioned = true
			break
		}
	}
	if !isDM && !mentioned {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), interactionTimeout)
	defer cancel()

	content := stripMention(mc.Content, selfID)
	if strings.TrimSpace(content) == "" {
		content = "Say hello and briefly explain what you can do."
	}

	_ = s.ChannelTyping(mc.ChannelID)

	sys := prompts.LoreSystem
	userMsg := content
	if guildID != "" {
		if camp, err := g.store.GetActiveCampaign(ctx, guildID); err == nil {
			userMsg = prompts.LoreUser(camp.Name, camp.System, camp.Premise, content)
		}
	}

	start := time.Now()
	answer, err := g.ai.Chat(ctx, g.cfg.LiteLLM.ChatModel, []litellm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: userMsg},
	}, 500)
	metrics.CommandDuration.WithLabelValues("chat").Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.AIRequests.WithLabelValues("chat", "error").Inc()
		g.log.Error("chat failed", "err", err)
		g.sendReply(s, mc, "Sorry, I couldn't answer that right now.")
		return
	}
	metrics.AIRequests.WithLabelValues("chat", "ok").Inc()
	g.sendReply(s, mc, truncateForDiscord(answer))
}

// sendReply posts a reply referencing the triggering message.
func (g *Gateway) sendReply(s *discordgo.Session, mc *discordgo.MessageCreate, content string) {
	_, err := s.ChannelMessageSendReply(mc.ChannelID, content, mc.Reference())
	if err != nil {
		// Fall back to a plain message if reply referencing fails (e.g. in DMs).
		_, _ = s.ChannelMessageSend(mc.ChannelID, content)
	}
}

// stripMention removes a leading <@id> / <@!id> mention from message content.
func stripMention(content, botID string) string {
	content = strings.ReplaceAll(content, "<@"+botID+">", "")
	content = strings.ReplaceAll(content, "<@!"+botID+">", "")
	return strings.TrimSpace(content)
}
