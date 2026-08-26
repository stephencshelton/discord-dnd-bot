package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// handleSearch searches the active campaign's completed session memory without
// consuming an AI quota. Results are snippets so the command stays useful in a
// live session and does not dump private transcripts into a channel.
func (g *Gateway) handleSearch(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	if i.GuildID == "" {
		return g.reply(s, i, "Use `/search` inside a server with an active campaign.", true)
	}
	query := strings.TrimSpace(optString(i.ApplicationCommandData().Options, "query"))
	if query == "" {
		return g.reply(s, i, "Give me a word or phrase to search for.", true)
	}
	campaign, err := g.activeCampaign(ctx, i.GuildID)
	if err != nil {
		return g.reply(s, i, err.Error(), true)
	}
	results, err := g.store.SearchSessions(ctx, campaign.ID, query, 8)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		return g.reply(s, i, fmt.Sprintf("I couldn't find `%s` in this campaign's completed sessions.", query), true)
	}

	fields := make([]*discordgo.MessageEmbedField, 0, len(results))
	for _, result := range results {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   fmt.Sprintf("Session · <t:%d:d>", result.StartedAt.Unix()),
			Value:  truncateForEmbed(result.Snippet),
			Inline: false,
		})
	}
	return g.replyEmbed(s, i, &discordgo.MessageEmbed{
		Title:  "Campaign memory",
		Color:  0x0ea5e9,
		Fields: fields,
	})
}
