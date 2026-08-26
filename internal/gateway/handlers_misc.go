package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
)

// handleRemind manages the per-campaign weekly reminder.
func (g *Gateway) handleRemind(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	guildID := i.GuildID
	if guildID == "" {
		return g.reply(s, i, "Use `/remind` inside a server.", true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return g.reply(s, i, err.Error(), true)
	}
	sub := i.ApplicationCommandData().Options[0]

	switch sub.Name {
	case "set":
		weekday := time.Weekday(optInt(sub.Options, "weekday"))
		clock := optString(sub.Options, "time")
		next, err := nextWeekly(time.Now().UTC(), weekday, clock)
		if err != nil {
			return g.reply(s, i, "Invalid time — use 24h UTC like `18:30`.", true)
		}
		// Store the canonical zero-padded HH:MM so the scheduler parses it back
		// identically regardless of how the user typed it (e.g. "9:30").
		clock = next.Format("15:04")
		if _, err := g.store.CreateReminder(ctx, db.Reminder{
			CampaignID: camp.ID,
			GuildID:    guildID,
			ChannelID:  i.ChannelID,
			Schedule:   fmt.Sprintf("weekly:%d:%s", int(weekday), clock),
			NextRun:    next,
		}); err != nil {
			return err
		}
		return g.reply(s, i, fmt.Sprintf("⏰ Reminder set for every **%s at %s UTC**. Next: <t:%d:F>", weekday, clock, next.Unix()), false)

	case "clear":
		if err := g.store.DeleteReminder(ctx, camp.ID); err != nil {
			return err
		}
		return g.reply(s, i, "Reminder cleared.", false)

	case "show":
		r, err := g.store.GetReminder(ctx, camp.ID)
		if err != nil {
			return g.reply(s, i, "No reminder set. Use `/remind set`.", true)
		}
		return g.reply(s, i, fmt.Sprintf("⏰ Next reminder: <t:%d:F> (schedule `%s`)", r.NextRun.Unix(), r.Schedule), true)
	}
	return fmt.Errorf("unknown remind subcommand")
}

// nextWeekly computes the next occurrence of weekday at HH:MM (UTC) after `from`.
func nextWeekly(from time.Time, weekday time.Weekday, clock string) (time.Time, error) {
	t, err := time.Parse("15:04", clock)
	if err != nil {
		return time.Time{}, err
	}
	daysAhead := (int(weekday) - int(from.Weekday()) + 7) % 7
	candidate := time.Date(from.Year(), from.Month(), from.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC).
		AddDate(0, 0, daysAhead)
	if !candidate.After(from) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	return candidate, nil
}

// handleNotesChannel sets the channel where session notes are posted (admin).
func (g *Gateway) handleNotesChannel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	guildID := i.GuildID
	if guildID == "" {
		return g.reply(s, i, "Use this inside a server.", true)
	}
	if _, err := g.store.EnsureGuild(ctx, guildID); err != nil {
		return err
	}
	channelID := optChannel(i.ApplicationCommandData().Options, "channel")
	if err := g.store.SetNotesChannel(ctx, guildID, channelID); err != nil {
		return err
	}
	return g.reply(s, i, fmt.Sprintf("📌 Session notes will be posted in <#%s>.", channelID), false)
}

// handleFeedback stores user feedback.
func (g *Gateway) handleFeedback(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	msg := optString(i.ApplicationCommandData().Options, "message")
	if strings.TrimSpace(msg) == "" {
		return g.reply(s, i, "Please include a message.", true)
	}
	if err := g.store.AddFeedback(ctx, i.GuildID, interactionUser(i), msg); err != nil {
		return err
	}
	return g.reply(s, i, "🙏 Thank you! Your feedback was recorded.", true)
}

// routeAutocomplete provides suggestions for campaign/character name options.
func (g *Gateway) routeAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data := i.ApplicationCommandData()
	var choices []*discordgo.ApplicationCommandOptionChoice

	switch data.Name {
	case "campaign":
		camps, _ := g.store.ListCampaigns(ctx, i.GuildID, true)
		for _, c := range camps {
			choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: c.Name, Value: c.Name})
			if len(choices) >= 25 {
				break
			}
		}
	case "character":
		if camp, err := g.store.GetActiveCampaign(ctx, i.GuildID); err == nil {
			pcs, _ := g.store.ListPCs(ctx, camp.ID)
			for _, pc := range pcs {
				choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: pc.Name, Value: pc.Name})
				if len(choices) >= 25 {
					break
				}
			}
		}
	case "help":
		partial := strings.ToLower(optString(data.Options, "command"))
		for _, name := range helpCommandNames() {
			if partial == "" || strings.Contains(name, partial) {
				choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: name, Value: name})
			}
			if len(choices) >= 25 {
				break
			}
		}
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	})
}
