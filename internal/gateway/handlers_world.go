package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/discordfmt"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
)

// activeCampaign resolves the guild's active campaign or returns a user-facing
// error suitable for followup.
func (g *Gateway) activeCampaign(ctx context.Context, guildID string) (*db.Campaign, error) {
	c, err := g.store.GetActiveCampaign(ctx, guildID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, errors.New("no active campaign — create one with `/campaign create` then `/campaign activate`")
	}
	return c, err
}

// listLineLimit caps a single entry's line in a listing. Listings are indexes:
// one entry must not be able to crowd out the rest (or blow the message limit)
// just because its description is long. Full detail lives on the entry itself.
const listLineLimit = 240

// listLine bounds one listing row, cutting on a word boundary so it reads as
// abbreviated rather than corrupted.
func listLine(s string) string {
	return discordfmt.Truncate(s, listLineLimit)
}

// plural picks the singular or plural suffix for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (g *Gateway) handleCampaign(ctx context.Context, ic *ictx) error {
	guildID, ok := g.resolveGuild(ctx, ic.guildID(), ic.userID())
	if !ok {
		return ic.reply(dmGuildHelp, true)
	}
	if _, err := g.store.EnsureGuild(ctx, guildID); err != nil {
		return err
	}

	switch ic.subcommand() {
	case "create":
		name := ic.optString("name")
		system := ic.optString("system")
		premise := ic.optString("premise")
		c, err := g.store.CreateCampaign(ctx, guildID, name, system, premise)
		if err != nil {
			return err
		}
		// Auto-activate the first campaign for convenience.
		if existing, _ := g.store.ListCampaigns(ctx, guildID, false); len(existing) == 1 {
			_ = g.store.SetActiveCampaign(ctx, guildID, c.ID)
		}
		return ic.reply(fmt.Sprintf("📖 Created campaign **%s**. Activate it with `/campaign activate`.", c.Name), false)

	case "list":
		camps, err := g.store.ListCampaigns(ctx, guildID, true)
		if err != nil {
			return err
		}
		if len(camps) == 0 {
			return ic.reply("No campaigns yet. Create one with `/campaign create`.", true)
		}
		active, _ := g.store.GetActiveCampaign(ctx, guildID)
		var b strings.Builder
		for _, c := range camps {
			marker := "•"
			if active != nil && active.ID == c.ID {
				marker = "▶"
			}
			line := fmt.Sprintf("%s **%s**", marker, c.Name)
			if c.System != "" {
				line += " _(" + c.System + ")_"
			}
			if c.Archived {
				line += " — archived"
			}
			b.WriteString(listLine(line) + "\n")
		}
		return ic.replyLong(b.String(), true)

	case "activate":
		name := ic.optString("name")
		c, err := g.findCampaignByName(ctx, guildID, name)
		if err != nil {
			return err
		}
		if err := g.store.SetActiveCampaign(ctx, guildID, c.ID); err != nil {
			return err
		}
		return ic.reply(fmt.Sprintf("▶ Active campaign is now **%s**.", c.Name), false)

	case "archive":
		name := ic.optString("name")
		c, err := g.findCampaignByName(ctx, guildID, name)
		if err != nil {
			return err
		}
		if err := g.store.SetCampaignArchived(ctx, c.ID, true); err != nil {
			return err
		}
		return ic.reply(fmt.Sprintf("🗄️ Archived **%s**.", c.Name), false)

	case "delete":
		return g.campaignDelete(ctx, ic, guildID)
	}
	return fmt.Errorf("unknown campaign subcommand")
}

// campaignDelete permanently removes a campaign and everything under it. The DB
// cascade (ON DELETE CASCADE on campaign_id) handles sessions, notes,
// embeddings, participants, characters, world entities, reminders, and the
// active-campaign pointer. Raw session audio in object storage is NOT part of
// that cascade, so we purge each session's chunk prefix from S3 first. Requires
// the caller to retype the campaign name in `confirm` as a guard against
// accidental, irreversible deletion.
func (g *Gateway) campaignDelete(ctx context.Context, ic *ictx, guildID string) error {
	name := ic.optString("name")
	confirm := ic.optString("confirm")
	c, err := g.findCampaignByName(ctx, guildID, name)
	if err != nil {
		return ic.reply(err.Error(), true)
	}
	if !strings.EqualFold(strings.TrimSpace(confirm), c.Name) {
		return ic.reply(fmt.Sprintf(
			"⚠️ This permanently deletes **%s** and ALL its sessions, transcripts, notes, characters, world entries, and recorded audio — this can't be undone.\n"+
				"Re-run with `confirm` set to the exact campaign name (`%s`) to proceed.", c.Name, c.Name), true)
	}

	// Purging audio from S3 can take a moment; ack first (ephemeral).
	if err := ic.ack(true); err != nil {
		return err
	}

	// 1) Purge raw audio chunks from object storage (not covered by DB cascade).
	prefixes, perr := g.store.ListSessionChunkPrefixes(ctx, c.ID)
	if perr != nil {
		return ic.followup("I couldn't read the session list to clean up audio. Nothing was deleted; please try again.")
	}
	audioDeleted := 0
	for _, prefix := range prefixes {
		if g.storage == nil || prefix == "" {
			continue
		}
		n, derr := g.storage.DeletePrefix(ctx, prefix)
		audioDeleted += n
		if derr != nil {
			// Don't leave the campaign half-deleted: stop before the DB delete so
			// the operator can retry rather than orphan audio silently.
			logging.FromContext(ctx, g.log).Error("campaign delete: purge audio", "campaign", c.ID, "prefix", prefix, "err", derr)
			return ic.followup("I couldn't fully delete the recorded audio, so I stopped before removing the campaign. Please try again.")
		}
	}

	// 2) Delete the campaign row; the DB cascade removes all dependent data.
	if err := g.store.DeleteCampaign(ctx, c.ID); err != nil {
		return ic.followup("I purged the audio but couldn't delete the campaign record. Please try again.")
	}
	logging.FromContext(ctx, g.log).Info("campaign deleted",
		"campaign", c.ID, "name", c.Name, "guild", guildID, "audio_objects_deleted", audioDeleted, "user", ic.userID())
	return ic.followup(fmt.Sprintf("🗑️ Deleted **%s** and all its data (%d audio file(s) purged).", c.Name, audioDeleted))
}

// findCampaignByName does a case-insensitive lookup within a guild.
func (g *Gateway) findCampaignByName(ctx context.Context, guildID, name string) (*db.Campaign, error) {
	camps, err := g.store.ListCampaigns(ctx, guildID, true)
	if err != nil {
		return nil, err
	}
	for i := range camps {
		if strings.EqualFold(camps[i].Name, name) {
			return &camps[i], nil
		}
	}
	return nil, fmt.Errorf("campaign %q not found", name)
}

func (g *Gateway) handleCharacter(ctx context.Context, ic *ictx) error {
	guildID, ok := g.resolveGuild(ctx, ic.guildID(), ic.userID())
	if !ok {
		return ic.reply(dmGuildHelp, true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}
	userID := ic.userID()

	switch ic.subcommand() {
	case "add":
		// Open a structured form (modal). Prefill it with the user's existing
		// character so /character add doubles as an edit. The modal's submit
		// (handleCharacterAddModalSubmit) persists the result.
		existing, _ := g.userCharacter(ctx, camp.ID, userID)
		return ic.e.Modal(characterAddModal(existing))

	case "edit":
		// Explicit edit: open the pre-filled form for the user's character. If
		// they have none yet, guide them to add first.
		existing, cerr := g.userCharacter(ctx, camp.ID, userID)
		if cerr != nil || existing == nil {
			return ic.reply("You don't have a character in this campaign yet — add one with `/character add`.", true)
		}
		return ic.e.Modal(characterAddModal(existing))

	case "list":
		pcs, err := g.store.ListPCs(ctx, camp.ID)
		if err != nil {
			return err
		}
		if len(pcs) == 0 {
			return ic.reply("No characters yet. Add one with `/character add`.", true)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "**Characters in %s:**\n", camp.Name)
		for _, pc := range pcs {
			fmt.Fprintf(&b, "%s\n", listLine(fmt.Sprintf("• **%s** — Lv %d %s %s (<@%s>)",
				pc.Name, pc.Level, pc.Race, pc.Class, pc.DiscordUserID)))
		}
		b.WriteString("\n_See a full sheet with `/character show`._")
		return ic.replyLong(b.String(), true)

	case "show":
		return g.handleCharacterShow(ctx, ic, camp)

	case "delete":
		name := ic.optString("name")
		pc, err := g.store.GetPCByName(ctx, camp.ID, name)
		if errors.Is(err, db.ErrNotFound) {
			return ic.reply(fmt.Sprintf("No character named %q.", name), true)
		}
		if err != nil {
			return err
		}
		if err := g.store.DeletePC(ctx, pc.ID); err != nil {
			return err
		}
		// Drop it from /ask retrieval too (best-effort).
		_ = g.store.DeleteCanonEmbedding(ctx, db.CanonSourceCharacter, pc.ID)
		return ic.reply(fmt.Sprintf("🗑️ Deleted **%s**.", pc.Name), false)
	}
	return fmt.Errorf("unknown character subcommand")
}

func (g *Gateway) handleWorld(ctx context.Context, ic *ictx) error {
	guildID, ok := g.resolveGuild(ctx, ic.guildID(), ic.userID())
	if !ok {
		return ic.reply(dmGuildHelp, true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}

	switch ic.subcommand() {
	case "add":
		// Open the kind-specific structured form (modal). The submit handler
		// (handleWorldEntityModalSubmit) persists the entity + metadata.
		kind := db.WorldEntityKind(ic.optString("kind"))
		if !db.ValidWorldKind(kind) {
			return ic.reply("Pick a valid kind (NPC, Location, Faction, Quest, or Story hook).", true)
		}
		return ic.e.Modal(worldAddModal(kind))

	case "edit":
		// Open a PRE-FILLED form for an existing entry; submit REPLACES its
		// fields (deliberate correction, distinct from add's append).
		kind := db.WorldEntityKind(ic.optString("kind"))
		if !db.ValidWorldKind(kind) {
			return ic.reply("Pick a valid kind (NPC, Location, Faction, Quest, or Story hook).", true)
		}
		name := ic.optString("name")
		entity, gerr := g.store.GetWorldEntityByName(ctx, camp.ID, kind, name)
		if errors.Is(gerr, db.ErrNotFound) {
			return ic.reply(fmt.Sprintf("No %s named %q. Add it with `/world add` first.", entityKindLabel(kind), name), true)
		}
		if gerr != nil {
			return gerr
		}
		return ic.e.Modal(worldEntityModal(kind, entity, true))

	case "list":
		kind := db.WorldEntityKind(ic.optString("kind"))
		entries, err := g.store.ListWorldEntities(ctx, camp.ID, kind)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return ic.reply("No world entries yet. Add one with `/world add`.", true)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "**World of %s** — %d entr%s:\n", camp.Name, len(entries), plural(len(entries), "y", "ies"))
		for _, e := range entries {
			line := fmt.Sprintf("• _[%s]_ **%s**", e.Kind, e.Name)
			if e.Description != "" {
				line += " — " + e.Description
			}
			if summary := worldMetaSummary(e); summary != "" {
				line += " _(" + summary + ")_"
			}
			// A listing is an INDEX, so each entry gets a bounded one-liner. Entity
			// descriptions grow every time a /review-session proposal is approved
			// (approval appends), so without this cap one long-lived NPC can crowd
			// out every other entry — or push the whole message past Discord's limit.
			b.WriteString(listLine(line) + "\n")
		}
		b.WriteString("\n_Read one in full with `/world show name:<name>`._")
		return ic.replyLong(b.String(), true)

	case "show":
		return g.handleWorldShow(ctx, ic, camp)

	case "delete":
		kind := db.WorldEntityKind(ic.optString("kind"))
		if !db.ValidWorldKind(kind) {
			return ic.reply("Pick a valid kind (NPC, Location, Faction, Quest, or Story hook).", true)
		}
		name := ic.optString("name")
		entity, gerr := g.store.GetWorldEntityByName(ctx, camp.ID, kind, name)
		if errors.Is(gerr, db.ErrNotFound) {
			return ic.reply(fmt.Sprintf("No %s named %q.", entityKindLabel(kind), name), true)
		}
		if gerr != nil {
			return gerr
		}
		if err := g.store.DeleteWorldEntity(ctx, entity.ID); err != nil {
			return err
		}
		// Drop it from /ask retrieval too (best-effort).
		_ = g.store.DeleteCanonEmbedding(ctx, db.CanonSourceEntity, entity.ID)
		logging.FromContext(ctx, g.log).Info("world entity deleted",
			"campaign", camp.ID, "kind", kind, "name", entity.Name, "user", ic.userID())
		return ic.reply(fmt.Sprintf("🗑️ Deleted %s **%s**.", entityKindLabel(kind), entity.Name), false)
	}
	return fmt.Errorf("unknown world subcommand")
}

// worldMetaSummary renders a short "key: value" summary of an entity's
// structured metadata for the /world list view (e.g. a quest's status). It's
// intentionally compact — the full detail lives in the entity record.
func worldMetaSummary(e db.WorldEntity) string {
	if len(e.Metadata) == 0 {
		return ""
	}
	// Show the most useful field per kind first, then any status.
	order := map[db.WorldEntityKind][]string{
		db.KindNPC:      {"role", "status"},
		db.KindLocation: {"region"},
		db.KindFaction:  {"status", "goals"},
		db.KindQuest:    {"status", "objective"},
		db.KindHook:     {"status", "related"},
	}
	var parts []string
	for _, k := range order[e.Kind] {
		if v, ok := e.Metadata[k].(string); ok && strings.TrimSpace(v) != "" {
			parts = append(parts, k+": "+v)
		}
	}
	return strings.Join(parts, "; ")
}
