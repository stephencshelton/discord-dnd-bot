package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
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
			b.WriteString(line + "\n")
		}
		return ic.reply(b.String(), true)

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
		pc := db.PlayerCharacter{
			CampaignID:    camp.ID,
			DiscordUserID: userID,
			Name:          ic.optString("name"),
			Class:         ic.optString("class"),
			Race:          ic.optString("race"),
			Level:         ic.optInt("level"),
			Notes:         ic.optString("notes"),
		}
		if pc.Level == 0 {
			pc.Level = 1
		}
		if _, err := g.store.CreatePC(ctx, pc); err != nil {
			return err
		}
		return ic.reply(fmt.Sprintf("🗡️ Saved **%s** (Lv %d %s %s).", pc.Name, pc.Level, pc.Race, pc.Class), false)

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
			fmt.Fprintf(&b, "• **%s** — Lv %d %s %s (<@%s>)\n", pc.Name, pc.Level, pc.Race, pc.Class, pc.DiscordUserID)
		}
		return ic.reply(b.String(), true)

	case "remove":
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
		return ic.reply(fmt.Sprintf("Removed **%s**.", pc.Name), false)
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
		e := db.WorldEntity{
			CampaignID:  camp.ID,
			Kind:        db.WorldEntityKind(ic.optString("kind")),
			Name:        ic.optString("name"),
			Description: ic.optString("description"),
		}
		if _, err := g.store.CreateWorldEntity(ctx, e); err != nil {
			return err
		}
		return ic.reply(fmt.Sprintf("🌍 Added %s **%s**.", e.Kind, e.Name), false)

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
		fmt.Fprintf(&b, "**World of %s:**\n", camp.Name)
		for _, e := range entries {
			line := fmt.Sprintf("• _[%s]_ **%s**", e.Kind, e.Name)
			if e.Description != "" {
				line += " — " + e.Description
			}
			b.WriteString(line + "\n")
		}
		return ic.reply(b.String(), true)
	}
	return fmt.Errorf("unknown world subcommand")
}
