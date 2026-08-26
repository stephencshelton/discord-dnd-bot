package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
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

func (g *Gateway) handleCampaign(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	sub := i.ApplicationCommandData().Options[0]
	guildID, ok := g.resolveGuild(ctx, i)
	if !ok {
		return g.reply(s, i, dmGuildHelp, true)
	}
	if _, err := g.store.EnsureGuild(ctx, guildID); err != nil {
		return err
	}

	switch sub.Name {
	case "create":
		name := optString(sub.Options, "name")
		system := optString(sub.Options, "system")
		premise := optString(sub.Options, "premise")
		c, err := g.store.CreateCampaign(ctx, guildID, name, system, premise)
		if err != nil {
			return err
		}
		// Auto-activate the first campaign for convenience.
		if existing, _ := g.store.ListCampaigns(ctx, guildID, false); len(existing) == 1 {
			_ = g.store.SetActiveCampaign(ctx, guildID, c.ID)
		}
		return g.reply(s, i, fmt.Sprintf("📖 Created campaign **%s**. Activate it with `/campaign activate`.", c.Name), false)

	case "list":
		camps, err := g.store.ListCampaigns(ctx, guildID, true)
		if err != nil {
			return err
		}
		if len(camps) == 0 {
			return g.reply(s, i, "No campaigns yet. Create one with `/campaign create`.", true)
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
		return g.reply(s, i, b.String(), true)

	case "activate":
		name := optString(sub.Options, "name")
		c, err := g.findCampaignByName(ctx, guildID, name)
		if err != nil {
			return err
		}
		if err := g.store.SetActiveCampaign(ctx, guildID, c.ID); err != nil {
			return err
		}
		return g.reply(s, i, fmt.Sprintf("▶ Active campaign is now **%s**.", c.Name), false)

	case "archive":
		name := optString(sub.Options, "name")
		c, err := g.findCampaignByName(ctx, guildID, name)
		if err != nil {
			return err
		}
		if err := g.store.SetCampaignArchived(ctx, c.ID, true); err != nil {
			return err
		}
		return g.reply(s, i, fmt.Sprintf("🗄️ Archived **%s**.", c.Name), false)
	}
	return fmt.Errorf("unknown campaign subcommand")
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

func (g *Gateway) handleCharacter(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	sub := i.ApplicationCommandData().Options[0]
	guildID, ok := g.resolveGuild(ctx, i)
	if !ok {
		return g.reply(s, i, dmGuildHelp, true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return g.reply(s, i, err.Error(), true)
	}
	userID := interactionUser(i)

	switch sub.Name {
	case "add":
		pc := db.PlayerCharacter{
			CampaignID:    camp.ID,
			DiscordUserID: userID,
			Name:          optString(sub.Options, "name"),
			Class:         optString(sub.Options, "class"),
			Race:          optString(sub.Options, "race"),
			Level:         optInt(sub.Options, "level"),
			Notes:         optString(sub.Options, "notes"),
		}
		if pc.Level == 0 {
			pc.Level = 1
		}
		if _, err := g.store.CreatePC(ctx, pc); err != nil {
			return err
		}
		return g.reply(s, i, fmt.Sprintf("🗡️ Saved **%s** (Lv %d %s %s).", pc.Name, pc.Level, pc.Race, pc.Class), false)

	case "list":
		pcs, err := g.store.ListPCs(ctx, camp.ID)
		if err != nil {
			return err
		}
		if len(pcs) == 0 {
			return g.reply(s, i, "No characters yet. Add one with `/character add`.", true)
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("**Characters in %s:**\n", camp.Name))
		for _, pc := range pcs {
			b.WriteString(fmt.Sprintf("• **%s** — Lv %d %s %s (<@%s>)\n", pc.Name, pc.Level, pc.Race, pc.Class, pc.DiscordUserID))
		}
		return g.reply(s, i, b.String(), true)

	case "remove":
		name := optString(sub.Options, "name")
		pc, err := g.store.GetPCByName(ctx, camp.ID, name)
		if errors.Is(err, db.ErrNotFound) {
			return g.reply(s, i, fmt.Sprintf("No character named %q.", name), true)
		}
		if err != nil {
			return err
		}
		if err := g.store.DeletePC(ctx, pc.ID); err != nil {
			return err
		}
		return g.reply(s, i, fmt.Sprintf("Removed **%s**.", pc.Name), false)
	}
	return fmt.Errorf("unknown character subcommand")
}

func (g *Gateway) handleWorld(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	sub := i.ApplicationCommandData().Options[0]
	guildID, ok := g.resolveGuild(ctx, i)
	if !ok {
		return g.reply(s, i, dmGuildHelp, true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return g.reply(s, i, err.Error(), true)
	}

	switch sub.Name {
	case "add":
		e := db.WorldEntity{
			CampaignID:  camp.ID,
			Kind:        db.WorldEntityKind(optString(sub.Options, "kind")),
			Name:        optString(sub.Options, "name"),
			Description: optString(sub.Options, "description"),
		}
		if _, err := g.store.CreateWorldEntity(ctx, e); err != nil {
			return err
		}
		return g.reply(s, i, fmt.Sprintf("🌍 Added %s **%s**.", e.Kind, e.Name), false)

	case "list":
		kind := db.WorldEntityKind(optString(sub.Options, "kind"))
		entries, err := g.store.ListWorldEntities(ctx, camp.ID, kind)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return g.reply(s, i, "No world entries yet. Add one with `/world add`.", true)
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("**World of %s:**\n", camp.Name))
		for _, e := range entries {
			line := fmt.Sprintf("• _[%s]_ **%s**", e.Kind, e.Name)
			if e.Description != "" {
				line += " — " + e.Description
			}
			b.WriteString(line + "\n")
		}
		return g.reply(s, i, b.String(), true)
	}
	return fmt.Errorf("unknown world subcommand")
}
