package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/stephencshelton/discord-dnd-bot/internal/dice"
)

// handleRoll evaluates standard dice notation (e.g. "2d6+3", "d20", "4d6kh3")
// and posts the result. It is free (no AI, no quota) so it stays instant and
// unlimited — the most-used action at a live table.
func (g *Gateway) handleRoll(_ context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	expr := strings.TrimSpace(optString(i.ApplicationCommandData().Options, "dice"))
	if expr == "" {
		expr = "1d20"
	}
	res, err := dice.Roll(expr)
	if err != nil {
		return g.reply(s, i, fmt.Sprintf("🎲 I couldn't read `%s`: %s\nTry `2d6+3`, `d20`, or `4d6kh3`.", expr, err.Error()), true)
	}

	reason := strings.TrimSpace(optString(i.ApplicationCommandData().Options, "reason"))
	title := "🎲 Roll"
	if reason != "" {
		title = "🎲 " + reason
	}
	e := &discordgo.MessageEmbed{
		Title:       title,
		Description: fmt.Sprintf("**%d**\n`%s`", res.Total, res.Detail),
		Color:       0x22c55e,
		Footer:      &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("rolled by %s", displayNameFor(i))},
	}
	return g.replyEmbed(s, i, e)
}

// displayNameFor returns the best display name for the invoking user.
func displayNameFor(i *discordgo.InteractionCreate) string {
	if i.Member != nil {
		if i.Member.Nick != "" {
			return i.Member.Nick
		}
		if i.Member.User != nil {
			return i.Member.User.Username
		}
	}
	if i.User != nil {
		return i.User.Username
	}
	return "someone"
}
