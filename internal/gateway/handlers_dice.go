package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"

	"github.com/stephencshelton/discord-dnd-bot/internal/dice"
)

// handleRoll evaluates standard dice notation (e.g. "2d6+3", "d20", "4d6kh3")
// and posts the result. It is free (no AI, no quota) so it stays instant and
// unlimited — the most-used action at a live table.
func (g *Gateway) handleRoll(_ context.Context, ic *ictx) error {
	expr := strings.TrimSpace(ic.optString("dice"))
	if expr == "" {
		expr = "1d20"
	}
	res, err := dice.Roll(expr)
	if err != nil {
		return ic.reply(fmt.Sprintf("🎲 I couldn't read `%s`: %s\nTry `2d6+3`, `d20`, or `4d6kh3`.", expr, err.Error()), true)
	}

	reason := strings.TrimSpace(ic.optString("reason"))
	title := "🎲 Roll"
	if reason != "" {
		title = "🎲 " + reason
	}
	e := discord.Embed{
		Title:       title,
		Description: fmt.Sprintf("**%d**\n`%s`", res.Total, res.Detail),
		Color:       0x22c55e,
		Footer:      &discord.EmbedFooter{Text: fmt.Sprintf("rolled by %s", ic.displayName())},
	}
	return ic.replyEmbed(e)
}
