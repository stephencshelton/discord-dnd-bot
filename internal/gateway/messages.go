package gateway

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/prompts"
)

// onMessageCreate powers conversational chat: @mentions in a guild, and DMs to
// the bot. Both route through the chat model with the active campaign as context.
func (g *Gateway) onMessageCreate(e *events.MessageCreate) {
	msg := e.Message
	if msg.Author.Bot {
		return
	}
	authorID := msg.Author.ID.String()
	channelID := msg.ChannelID

	// Recover so a panic while handling one message can't crash the gateway.
	defer func() {
		if r := recover(); r != nil {
			metrics.PanicsRecovered.WithLabelValues("message").Inc()
			g.log.Error("message handler panicked; recovered",
				"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()),
				"user", authorID, "channel", channelID.String())
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), interactionTimeout)
	defer cancel()

	isDM := e.GuildID == nil
	var guildID string
	if isDM {
		var allowed bool
		guildID, allowed = g.directMessageGuildID(ctx, authorID)
		if !allowed {
			reason := g.dmRejectReason(authorID)
			g.log.Debug("ignoring DM", "user", authorID, "reason", reason)
			if len(g.sharedGuildIDs(authorID)) > 1 {
				g.sendReply(e, "You're in more than one of my servers, so I don't know which campaign to use here. Run `/dm-server` in this DM to pick one (and to switch later).")
			}
			return
		}
	} else if !g.allowsGuild(e.GuildID.String()) {
		return
	} else {
		guildID = e.GuildID.String()
	}

	// Self ID is populated after Ready; guard against an early message.
	self, ok := e.Client().Caches.SelfUser()
	if !ok {
		return
	}
	selfID := self.ID

	mentioned := false
	for _, u := range msg.Mentions {
		if u.ID == selfID {
			mentioned = true
			break
		}
	}
	if !isDM && !mentioned {
		return
	}

	content := stripMention(msg.Content, selfID)
	if strings.TrimSpace(content) == "" {
		content = "Say hello and briefly explain what you can do."
	}

	_ = e.Client().Rest.SendTyping(channelID)

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
		g.log.Error("chat failed", "err", err, "user", authorID, "channel", channelID.String())
		g.sendReply(e, "Sorry, I couldn't answer that right now.")
		return
	}
	g.sendReply(e, truncateForDiscord(answer))
}

// sendReply posts a reply referencing the triggering message, falling back to a
// plain message if the referenced-reply send fails (e.g. in DMs).
func (g *Gateway) sendReply(e *events.MessageCreate, content string) {
	msgID := e.MessageID
	ref := &discord.MessageReference{MessageID: &msgID}
	if _, err := e.Client().Rest.CreateMessage(e.ChannelID, discord.MessageCreate{
		Content:          content,
		MessageReference: ref,
	}); err != nil {
		_, _ = e.Client().Rest.CreateMessage(e.ChannelID, discord.MessageCreate{Content: content})
	}
}

// stripMention removes a leading <@id> / <@!id> mention from message content.
func stripMention(content string, botID snowflake.ID) string {
	id := botID.String()
	content = strings.ReplaceAll(content, "<@"+id+">", "")
	content = strings.ReplaceAll(content, "<@!"+id+">", "")
	return strings.TrimSpace(content)
}
