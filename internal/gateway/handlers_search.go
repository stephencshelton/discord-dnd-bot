package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"
)

// handleSearch searches the active campaign's completed session memory without
// consuming an AI quota. Results are snippets so the command stays useful in a
// live session and does not dump private transcripts into a channel.
func (g *Gateway) handleSearch(ctx context.Context, ic *ictx) error {
	guildID, ok := g.resolveGuild(ctx, ic.guildID(), ic.userID())
	if !ok {
		return ic.reply(dmGuildHelp, true)
	}
	query := strings.TrimSpace(ic.optString("query"))
	if query == "" {
		return ic.reply("Give me a word or phrase to search for.", true)
	}
	campaign, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}
	results, err := g.store.SearchSessions(ctx, campaign.ID, query, 8)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		return ic.reply(fmt.Sprintf("I couldn't find `%s` in this campaign's completed sessions.", query), true)
	}

	fields := make([]discord.EmbedField, 0, len(results))
	for _, result := range results {
		fields = append(fields, discord.EmbedField{
			Name: fmt.Sprintf("Session · <t:%d:d>", result.StartedAt.Unix()),
			// Field values cap at 1024 (not the 4096 an embed DESCRIPTION allows),
			// and the whole embed caps at 6000 — so the snippets are budgeted
			// together by fitEmbedFields rather than trimmed independently.
			Value:  result.Snippet,
			Inline: boolPtr(false),
		})
	}
	const title = "Campaign memory"
	return ic.replyEmbed(discord.Embed{
		Title:  title,
		Color:  0x0ea5e9,
		Fields: fitEmbedFields(len(title), fields),
	})
}
