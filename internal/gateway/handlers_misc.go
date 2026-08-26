package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
)

// handleRemind manages the per-campaign weekly reminder.
func (g *Gateway) handleRemind(ctx context.Context, ic *ictx) error {
	guildID := ic.guildID()
	if guildID == "" {
		return ic.reply("Use `/remind` inside a server.", true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}

	switch ic.subcommand() {
	case "set":
		weekday := time.Weekday(ic.optInt("weekday"))
		clock := ic.optString("time")
		next, err := nextWeekly(time.Now().UTC(), weekday, clock)
		if err != nil {
			return ic.reply("Invalid time — use 24h UTC like `18:30`.", true)
		}
		// Store the canonical zero-padded HH:MM so the scheduler parses it back
		// identically regardless of how the user typed it (e.g. "9:30").
		clock = next.Format("15:04")
		if _, err := g.store.CreateReminder(ctx, db.Reminder{
			CampaignID: camp.ID,
			GuildID:    guildID,
			ChannelID:  ic.channelID(),
			Schedule:   fmt.Sprintf("weekly:%d:%s", int(weekday), clock),
			NextRun:    next,
		}); err != nil {
			return err
		}
		return ic.reply(fmt.Sprintf("⏰ Reminder set for every **%s at %s UTC**. Next: <t:%d:F>", weekday, clock, next.Unix()), false)

	case "clear":
		if err := g.store.DeleteReminder(ctx, camp.ID); err != nil {
			return err
		}
		return ic.reply("Reminder cleared.", false)

	case "show":
		r, err := g.store.GetReminder(ctx, camp.ID)
		if err != nil {
			return ic.reply("No reminder set. Use `/remind set`.", true)
		}
		return ic.reply(fmt.Sprintf("⏰ Next reminder: <t:%d:F> (schedule `%s`)", r.NextRun.Unix(), r.Schedule), true)
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
func (g *Gateway) handleNotesChannel(ctx context.Context, ic *ictx) error {
	guildID := ic.guildID()
	if guildID == "" {
		return ic.reply("Use this inside a server.", true)
	}
	if _, err := g.store.EnsureGuild(ctx, guildID); err != nil {
		return err
	}
	channelID := ic.optChannel("channel")
	if err := g.store.SetNotesChannel(ctx, guildID, channelID); err != nil {
		return err
	}
	return ic.reply(fmt.Sprintf("📌 Session notes will be posted in <#%s>.", channelID), false)
}

// handleFeedback stores user feedback.
func (g *Gateway) handleFeedback(ctx context.Context, ic *ictx) error {
	msg := ic.optString("message")
	if strings.TrimSpace(msg) == "" {
		return ic.reply("Please include a message.", true)
	}
	if err := g.store.AddFeedback(ctx, ic.guildID(), ic.userID(), msg); err != nil {
		return err
	}
	return ic.reply("🙏 Thank you! Your feedback was recorded.", true)
}

// handleDMServer lets a user who shares multiple servers with the bot choose
// which server's campaign their DM chat/recaps use, and switch between them.
// With no argument it reports the current selection and lists the options.
func (g *Gateway) handleDMServer(ctx context.Context, ic *ictx) error {
	userID := ic.userID()
	if userID == "" {
		return ic.reply("Couldn't identify you.", true)
	}

	shared := g.sharedGuildIDs(userID)
	if len(shared) == 0 {
		return ic.reply("You're not in any server I'm configured for, so there's nothing to select.", true)
	}

	selected := ic.optString("server")

	// No argument: show current selection + available servers.
	if selected == "" {
		current, _ := g.store.GetDMGuildID(ctx, userID)
		var b strings.Builder
		if current != "" && g.isMemberOfAllowlisted(current, userID) {
			fmt.Fprintf(&b, "Your DMs currently use **%s**.\n\n", g.guildName(current))
		} else if len(shared) == 1 {
			fmt.Fprintf(&b, "Your DMs use **%s** (the only server we share).\n\n", g.guildName(shared[0]))
		} else {
			b.WriteString("You haven't picked a server for DMs yet.\n\n")
		}
		b.WriteString("Available servers:\n")
		for _, gid := range shared {
			fmt.Fprintf(&b, "• %s\n", g.guildName(gid))
		}
		b.WriteString("\nRun `/dm-server server:<name>` to choose or switch.")
		return ic.reply(b.String(), true)
	}

	// Setting a selection: validate it's a server the user actually shares.
	if !g.isMemberOfAllowlisted(selected, userID) {
		return ic.reply("That isn't a server we share (or I'm not configured for it). Pick one from the suggestions.", true)
	}
	if err := g.store.SetDMGuildID(ctx, userID, selected); err != nil {
		return err
	}
	return ic.reply(fmt.Sprintf("✅ Your DMs will now use **%s**. Run `/dm-server` again anytime to switch.", g.guildName(selected)), true)
}

// isMemberOfAllowlisted reports whether guildID is allowlisted AND the user is a
// member of it.
func (g *Gateway) isMemberOfAllowlisted(guildID, userID string) bool {
	if _, ok := g.allowedGuilds[guildID]; !ok {
		return false
	}
	return g.isGuildMember != nil && g.isGuildMember(guildID, userID)
}

// guildName resolves a guild's display name from the cache, falling back to the
// ID when it isn't cached.
func (g *Gateway) guildName(guildID string) string {
	if gid, err := snowflake.Parse(guildID); err == nil {
		if gd, ok := g.client.Caches.Guild(gid); ok && gd.Name != "" {
			return gd.Name
		}
	}
	return guildID
}

// onAutocomplete provides suggestions for campaign/character/help/dm-server
// name options. disgo invokes this for every autocomplete interaction.
func (g *Gateway) onAutocomplete(e *events.AutocompleteInteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data := e.Data

	guildID := ""
	if gid := e.GuildID(); gid != nil {
		guildID = gid.String()
	}
	userID := e.User().ID.String()

	var choices []discord.AutocompleteChoice
	add := func(name, value string) bool {
		choices = append(choices, discord.AutocompleteChoiceString{Name: name, Value: value})
		return len(choices) < 25
	}

	switch data.CommandName {
	case "campaign":
		gid, _ := g.resolveGuild(ctx, guildID, userID) // "" -> no suggestions
		camps, _ := g.store.ListCampaigns(ctx, gid, true)
		for _, c := range camps {
			if !add(c.Name, c.Name) {
				break
			}
		}
	case "character":
		gid, _ := g.resolveGuild(ctx, guildID, userID)
		if camp, err := g.store.GetActiveCampaign(ctx, gid); err == nil {
			pcs, _ := g.store.ListPCs(ctx, camp.ID)
			for _, pc := range pcs {
				if !add(pc.Name, pc.Name) {
					break
				}
			}
		}
	case "help":
		partial := strings.ToLower(data.String("command"))
		for _, name := range helpCommandNames() {
			if partial == "" || strings.Contains(name, partial) {
				if !add(name, name) {
					break
				}
			}
		}
	case "dm-server":
		// Suggest servers the invoking user shares with the bot. The choice
		// value is the guild ID; the label is the guild's name.
		for _, gid := range g.sharedGuildIDs(userID) {
			if !add(g.guildName(gid), gid) {
				break
			}
		}
	}
	if err := e.AutocompleteResult(choices); err != nil {
		g.log.Debug("autocomplete result failed", "err", err, "command", data.CommandName)
	}
}
