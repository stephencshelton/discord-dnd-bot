package gateway

import (
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
)

// commandDefs returns every slash command the bot exposes.
func commandDefs() []*discordgo.ApplicationCommand {
	adminPerm := int64(discordgo.PermissionManageServer)

	return []*discordgo.ApplicationCommand{
		// ---- Campaigns ----
		{
			Name:        "campaign",
			Description: "Manage campaigns (stories) in this server",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "create",
					Description: "Create a new campaign",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "name", Description: "Campaign name", Required: true},
						{Type: discordgo.ApplicationCommandOptionString, Name: "system", Description: "Game system (e.g. D&D 5e)", Required: false},
						{Type: discordgo.ApplicationCommandOptionString, Name: "premise", Description: "One-line premise", Required: false},
					},
				},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "List campaigns"},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "activate",
					Description: "Set the active campaign",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "name", Description: "Campaign name", Required: true, Autocomplete: true},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "archive",
					Description: "Archive a campaign",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "name", Description: "Campaign name", Required: true, Autocomplete: true},
					},
				},
			},
		},

		// ---- Characters ----
		{
			Name:        "character",
			Description: "Manage player characters",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "add",
					Description: "Add or update your character",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "name", Description: "Character name", Required: true},
						{Type: discordgo.ApplicationCommandOptionString, Name: "class", Description: "Class", Required: false},
						{Type: discordgo.ApplicationCommandOptionString, Name: "race", Description: "Race/ancestry", Required: false},
						{Type: discordgo.ApplicationCommandOptionInteger, Name: "level", Description: "Level", Required: false},
						{Type: discordgo.ApplicationCommandOptionString, Name: "notes", Description: "Short bio/notes", Required: false},
					},
				},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "List characters in the active campaign"},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "remove",
					Description: "Remove a character",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "name", Description: "Character name", Required: true, Autocomplete: true},
					},
				},
			},
		},

		// ---- Worldbuilding (NPCs, locations, factions, quests) ----
		{
			Name:        "world",
			Description: "Manage worldbuilding entries (NPCs, locations, factions, quests)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "add",
					Description: "Add a world entry",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "kind", Description: "Type of entry", Required: true, Choices: []*discordgo.ApplicationCommandOptionChoice{
							{Name: "NPC", Value: "npc"},
							{Name: "Location", Value: "location"},
							{Name: "Faction", Value: "faction"},
							{Name: "Quest", Value: "quest"},
						}},
						{Type: discordgo.ApplicationCommandOptionString, Name: "name", Description: "Name", Required: true},
						{Type: discordgo.ApplicationCommandOptionString, Name: "description", Description: "Description", Required: false},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "list",
					Description: "List world entries",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "kind", Description: "Filter by type", Required: false, Choices: []*discordgo.ApplicationCommandOptionChoice{
							{Name: "NPC", Value: "npc"},
							{Name: "Location", Value: "location"},
							{Name: "Faction", Value: "faction"},
							{Name: "Quest", Value: "quest"},
						}},
					},
				},
			},
		},

		// ---- Sessions (voice recording -> AI notes) ----
		{
			Name:        "session",
			Description: "Record a game session and generate AI notes",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "start", Description: "Join your voice channel and start recording"},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "stop", Description: "Stop recording and generate session notes"},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status", Description: "Show the current/last session status"},
			},
		},

		// ---- Dice roller (free, no AI) ----
		{
			Name:        "roll",
			Description: "Roll dice, e.g. 2d6+3, d20, or 4d6kh3",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "dice", Description: "Dice notation (default 1d20)", Required: false},
				{Type: discordgo.ApplicationCommandOptionString, Name: "reason", Description: "What's this roll for? (optional)", Required: false},
			},
		},

		// ---- AI lore assistant ----
		{
			Name:        "lore",
			Description: "Ask the AI for worldbuilding help",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "prompt", Description: "What do you want?", Required: true},
			},
		},

		// ---- Recap ----
		{
			Name:        "recap",
			Description: "Generate a 'previously on' recap from recent sessions",
		},

		// ---- Session memory search ----
		{
			Name:        "search",
			Description: "Search completed session notes and transcripts",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "query", Description: "What do you remember?", Required: true},
			},
		},

		// ---- Grounded Q&A over session notes (RAG) ----
		{
			Name:        "ask",
			Description: "Ask a question answered from your campaign's session notes",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "question", Description: "What do you want to know?", Required: true},
			},
		},

		// ---- Scene art ----
		{
			Name:        "art",
			Description: "Generate scene art with AI",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "scene", Description: "Describe the scene", Required: true},
			},
		},

		// ---- Reminders ----
		{
			Name:        "remind",
			Description: "Manage the recurring session reminder for the active campaign",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "set",
					Description: "Set a weekly reminder",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionString, Name: "weekday", Description: "Day of week", Required: true, Choices: weekdayChoices()},
						{Type: discordgo.ApplicationCommandOptionString, Name: "time", Description: "24h time UTC, e.g. 18:30", Required: true},
					},
				},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "clear", Description: "Clear the reminder"},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "show", Description: "Show the current reminder"},
			},
		},

		// ---- Notes channel (admin) ----
		{
			Name:                     "notes-channel",
			Description:              "Set the channel where session notes are posted (admin)",
			DefaultMemberPermissions: &adminPerm,
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionChannel, Name: "channel", Description: "Target channel", Required: true, ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText}},
			},
		},

		// ---- Reindex campaign memory for /ask (admin) ----
		{
			Name:                     "reindex",
			Description:              "Rebuild /ask search memory from all completed sessions (admin)",
			DefaultMemberPermissions: &adminPerm,
		},

		// ---- Feedback ----
		{
			Name:        "feedback",
			Description: "Send feedback to the bot team",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "message", Description: "Your feedback", Required: true},
			},
		},

		// ---- Help ----
		{Name: "help", Description: "How to use the bot", Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "command", Description: "Show detailed options for one command", Required: false, Autocomplete: true},
		}},
	}
}

func weekdayChoices() []*discordgo.ApplicationCommandOptionChoice {
	days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	out := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(days))
	for i, d := range days {
		out = append(out, &discordgo.ApplicationCommandOptionChoice{Name: d, Value: i})
	}
	return out
}

// commandRegistrar is the slice of the Discord session that registerCommands
// needs. Abstracting it lets the registration logic be unit-tested without a
// live *discordgo.Session.
type commandRegistrar interface {
	ApplicationCommandBulkOverwrite(appID, guildID string, commands []*discordgo.ApplicationCommand, options ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error)
}

// registerCommands removes global commands and installs commands in each
// allowlisted guild, where Discord makes them available immediately.
func (g *Gateway) registerCommands() error {
	ids, err := registerGuildCommands(g.sess, g.log, g.appID, g.cfg.Discord.AllowedGuildIDs(), commandDefs())
	g.regIDs = ids
	return err
}

// registerGuildCommands clears global commands and installs the given command
// defs into each guild. It is resilient: one bad guild does not abort the rest.
//
// A single guild can legitimately fail (e.g. the bot was invited there without
// the applications.commands scope -> HTTP 403 "Missing Access"), and that must
// not prevent commands from registering in the other guilds. Failures are
// logged and skipped; an error is returned only if EVERY guild fails (which
// usually points at a systemic problem like a bad app ID/token rather than a
// per-guild access issue). Returns the IDs of all successfully created commands.
func registerGuildCommands(r commandRegistrar, log *slog.Logger, appID string, guildIDs []string, defs []*discordgo.ApplicationCommand) ([]string, error) {
	if _, err := r.ApplicationCommandBulkOverwrite(appID, "", []*discordgo.ApplicationCommand{}); err != nil {
		return nil, fmt.Errorf("remove global commands: %w", err)
	}

	if len(guildIDs) == 0 {
		log.Warn("no Discord guilds configured; guild commands disabled")
		return nil, nil
	}

	var (
		regIDs    []string
		succeeded int
		lastErr   error
	)
	for _, guildID := range guildIDs {
		created, err := r.ApplicationCommandBulkOverwrite(appID, guildID, defs)
		if err != nil {
			lastErr = err
			log.Error("failed to register guild commands; skipping guild",
				"guild", guildID, "err", err)
			continue
		}
		succeeded++
		for _, c := range created {
			regIDs = append(regIDs, c.ID)
		}
		log.Info("registered guild commands", "count", len(created), "guild", guildID)
	}

	if succeeded == 0 {
		return regIDs, fmt.Errorf("register commands: all %d guild(s) failed; last error: %w", len(guildIDs), lastErr)
	}
	return regIDs, nil
}
