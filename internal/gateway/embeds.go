package gateway

import (
	"fmt"

	"github.com/disgoorg/disgo/discord"

	"github.com/stephencshelton/discord-dnd-bot/internal/discordfmt"
)

// Embed-rendering helpers that keep replies inside Discord's limits.
//
// Discord rejects an ENTIRE interaction with `50035 Invalid Form Body` if any
// part of it is over-length, so the user sees nothing at all rather than a
// slightly-clipped message. Length is therefore not cosmetic — it has to be
// enforced wherever the content is data-driven (listings, search hits, help).

// truncateForField fits text into an embed field value. Note the limit is 1024,
// NOT the 4096 that applies to an embed description — using the description
// limit for a field value is a silent over-length bug.
func truncateForField(s string) string {
	if s == "" {
		// Discord rejects an empty field value outright.
		return "—"
	}
	return discordfmt.Truncate(s, discordfmt.EmbedFieldValueLimit)
}

// fitEmbedFields trims a field list so the finished embed respects BOTH the
// per-field value cap and the 6000-char combined total (plus the 25-field cap).
//
// Individually-valid fields can still breach the aggregate limit — eight
// 1024-char search snippets are over it — and the aggregate breach is exactly the
// kind of failure that only appears once a campaign has enough data. baseLen is
// the characters already spent on the embed's title/description/footer.
//
// Fields that don't fit are dropped and a short note is appended so the reply is
// visibly partial instead of quietly short.
func fitEmbedFields(baseLen int, fields []discord.EmbedField) []discord.EmbedField {
	// Reserve room for the "more results" note we may add.
	const noteAllowance = 120
	budget := discordfmt.EmbedTotalLimit - baseLen - noteAllowance
	if budget < 0 {
		budget = 0
	}

	out := make([]discord.EmbedField, 0, len(fields))
	used := 0
	dropped := 0
	for _, f := range fields {
		f.Name = discordfmt.Truncate(f.Name, discordfmt.EmbedFieldNameLimit)
		f.Value = truncateForField(f.Value)
		cost := len([]rune(f.Name)) + len([]rune(f.Value))

		if len(out) >= discordfmt.MaxEmbedFields-1 || used+cost > budget {
			dropped++
			continue
		}
		out = append(out, f)
		used += cost
	}
	if dropped > 0 {
		out = append(out, discord.EmbedField{
			Name:  "…and more",
			Value: fmt.Sprintf("%d more result%s didn't fit — narrow your search to see them.", dropped, plural(dropped, "", "s")),
		})
	}
	return out
}
