package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/disgoorg/disgo/discord"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/discordfmt"
)

// Read-only detail views: /world show and /character show.
//
// These exist because `list` is an INDEX — its rows are deliberately abbreviated
// (see listLine) so one entry can't crowd out the rest or breach Discord's
// message limit. Without a detail view the only way to read an entry in full was
// `/world edit`, which opens a WRITE form whose submit replaces the entry's
// fields: browsing your own canon shouldn't risk destroying it.
//
// Length matters here more than anywhere else in the bot. Approving a
// /review-session proposal APPENDS to an entity's description (and to a
// character's notes), so these are the exact fields that grow without bound over
// a campaign. The renderers therefore spill overflow into follow-up messages
// rather than silently cutting the record short.

// showDescBudget is how much of a description goes in the embed itself. It's
// below the 4096 embed-description cap to leave room for the header lines and to
// keep the card readable; the remainder continues in follow-up messages.
const showDescBudget = 2400

// handleWorldShow renders one world entry in full, read-only.
//
// kind is optional: a name is unique per kind, so the same name can exist as both
// an NPC and a location. With no kind we look across all kinds and only ask which
// one when the name is genuinely ambiguous.
func (g *Gateway) handleWorldShow(ctx context.Context, ic *ictx, camp *db.Campaign) error {
	name := strings.TrimSpace(ic.optString("name"))
	if name == "" {
		return ic.reply("Which entry? Pass a `name` (the suggestions list what's recorded).", true)
	}

	kind := db.WorldEntityKind(strings.TrimSpace(ic.optString("kind")))
	var entity *db.WorldEntity

	if kind != "" {
		if !db.ValidWorldKind(kind) {
			return ic.reply("Pick a valid kind (NPC, Location, Faction, Quest, or Story hook).", true)
		}
		found, err := g.store.GetWorldEntityByName(ctx, camp.ID, kind, name)
		if errors.Is(err, db.ErrNotFound) {
			return ic.reply(fmt.Sprintf("No %s named %q in **%s**. Check `/world list`.", entityKindLabel(kind), name, camp.Name), true)
		}
		if err != nil {
			return err
		}
		entity = found
	} else {
		matches, err := g.store.FindWorldEntitiesByName(ctx, camp.ID, name)
		if err != nil {
			return err
		}
		switch len(matches) {
		case 0:
			return ic.reply(fmt.Sprintf("Nothing named %q in **%s**. Check `/world list`, or search the sessions with `/search`.", name, camp.Name), true)
		case 1:
			entity = &matches[0]
		default:
			// Ambiguous: name the kinds so the retry is one click, not a guess.
			kinds := make([]string, 0, len(matches))
			for _, m := range matches {
				kinds = append(kinds, fmt.Sprintf("`%s`", m.Kind))
			}
			return ic.reply(fmt.Sprintf(
				"There's more than one **%s** on record (%s). Re-run `/world show` with the `kind` option to pick one.",
				name, strings.Join(kinds, ", ")), true)
		}
	}

	embed, overflow := worldEntityEmbed(*entity)
	return ic.replyEmbedLong(embed, overflow, true)
}

// worldEntityEmbed renders a world entity as a detail card. It returns the embed
// plus any description overflow that didn't fit, which the caller posts as
// follow-up messages so a long (repeatedly appended) entry is never silently cut.
func worldEntityEmbed(e db.WorldEntity) (discord.Embed, string) {
	desc := strings.TrimSpace(e.Description)
	if desc == "" {
		desc = "_No description recorded yet. Add detail with `/world add`._"
	}
	body, overflow := splitForShow(desc, showDescBudget)

	fields := make([]discord.EmbedField, 0, 6)
	// Structured metadata, using the same labels as the add/edit form so the
	// read view and the write view describe fields identically.
	for _, f := range worldFields(e.Kind) {
		if v, ok := e.Metadata[f.id].(string); ok && strings.TrimSpace(v) != "" {
			fields = append(fields, discord.EmbedField{
				Name:   f.label,
				Value:  v,
				Inline: boolPtr(true),
			})
		}
	}
	// Anything else the extractor merged in that isn't part of this kind's form,
	// so approved proposals can't hide data from the detail view.
	for _, k := range extraMetadataKeys(e) {
		fields = append(fields, discord.EmbedField{
			Name:   k,
			Value:  fmt.Sprintf("%v", e.Metadata[k]),
			Inline: boolPtr(true),
		})
	}

	title := fmt.Sprintf("%s · %s", entityKindLabel(e.Kind), e.Name)
	footer := fmt.Sprintf("Added <t:%d:D> · updated <t:%d:R>", e.CreatedAt.Unix(), e.UpdatedAt.Unix())
	embed := discord.Embed{
		Title:       discordfmt.Truncate(title, 250),
		Description: body,
		Color:       worldKindColor(e.Kind),
		Fields:      fitEmbedFields(len([]rune(title))+len([]rune(body))+len([]rune(footer)), fields),
		// Timestamps use Discord's <t:...> markup so they render in the reader's
		// own timezone rather than the server's.
		Footer: &discord.EmbedFooter{Text: footer},
	}
	return embed, overflow
}

// extraMetadataKeys returns metadata keys that aren't part of the kind's form
// fields, sorted for a stable render order.
func extraMetadataKeys(e db.WorldEntity) []string {
	if len(e.Metadata) == 0 {
		return nil
	}
	known := map[string]bool{"description": true}
	for _, f := range worldFields(e.Kind) {
		known[f.id] = true
	}
	var out []string
	for k, v := range e.Metadata {
		if known[k] {
			continue
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// worldKindColor gives each kind a consistent accent color so the card type is
// recognizable at a glance.
func worldKindColor(k db.WorldEntityKind) int {
	switch k {
	case db.KindNPC:
		return 0x3b82f6 // blue
	case db.KindLocation:
		return 0x10b981 // green
	case db.KindFaction:
		return 0xf59e0b // amber
	case db.KindQuest:
		return 0x8b5cf6 // violet
	case db.KindHook:
		return 0xec4899 // pink
	default:
		return 0x64748b // slate
	}
}

// handleCharacterShow renders a full character sheet, including the recorded
// deeds that approved /review-session proposals append to its notes. With no
// name it shows the caller's own character.
func (g *Gateway) handleCharacterShow(ctx context.Context, ic *ictx, camp *db.Campaign) error {
	name := strings.TrimSpace(ic.optString("name"))

	var pc *db.PlayerCharacter
	if name == "" {
		own, err := g.userCharacter(ctx, camp.ID, ic.userID())
		if err != nil || own == nil {
			return ic.reply("You don't have a character in this campaign yet — add one with `/character add`, or pass a `name` to view someone else's.", true)
		}
		pc = own
	} else {
		found, err := g.store.GetPCByName(ctx, camp.ID, name)
		if errors.Is(err, db.ErrNotFound) {
			return ic.reply(fmt.Sprintf("No character named %q in **%s**. Check `/character list`.", name, camp.Name), true)
		}
		if err != nil {
			return err
		}
		pc = found
	}

	embed, overflow := characterEmbed(*pc)
	return ic.replyEmbedLong(embed, overflow, true)
}

// characterEmbed renders a player character as a detail card, returning any
// notes overflow for the caller to post as follow-ups.
func characterEmbed(pc db.PlayerCharacter) (discord.Embed, string) {
	notes := strings.TrimSpace(pc.Notes)
	if notes == "" {
		notes = "_No notes yet. Recorded deeds are added here when a `/review-session` proposal is approved._"
	}
	body, overflow := splitForShow(notes, showDescBudget)

	fields := make([]discord.EmbedField, 0, 4)
	if strings.TrimSpace(pc.Class) != "" {
		fields = append(fields, discord.EmbedField{Name: "Class", Value: pc.Class, Inline: boolPtr(true)})
	}
	if strings.TrimSpace(pc.Race) != "" {
		fields = append(fields, discord.EmbedField{Name: "Race", Value: pc.Race, Inline: boolPtr(true)})
	}
	if pc.Level > 0 {
		fields = append(fields, discord.EmbedField{Name: "Level", Value: fmt.Sprintf("%d", pc.Level), Inline: boolPtr(true)})
	}
	if pc.DiscordUserID != "" {
		fields = append(fields, discord.EmbedField{Name: "Player", Value: "<@" + pc.DiscordUserID + ">", Inline: boolPtr(true)})
	}

	title := "Character · " + pc.Name
	footer := fmt.Sprintf("Added <t:%d:D> · updated <t:%d:R>", pc.CreatedAt.Unix(), pc.UpdatedAt.Unix())
	embed := discord.Embed{
		Title:       discordfmt.Truncate(title, 250),
		Description: body,
		Color:       0x22c55e,
		Fields:      fitEmbedFields(len([]rune(title))+len([]rune(body))+len([]rune(footer)), fields),
		Footer:      &discord.EmbedFooter{Text: footer},
	}
	return embed, overflow
}

// splitForShow returns the first budget characters of s (cut on a LINE boundary
// so Markdown keeps rendering) plus whatever remains. An empty remainder means
// it all fitted.
//
// It deliberately does not truncate-and-discard: the whole point of a detail view
// is to show the complete record, and these fields grow every time a proposal is
// approved.
func splitForShow(s string, budget int) (head, rest string) {
	if len([]rune(s)) <= budget {
		return s, ""
	}
	chunks := discordfmt.ChunkMarkdown(s, budget)
	if len(chunks) == 0 {
		return s, ""
	}
	return chunks[0], strings.Join(chunks[1:], "\n")
}
