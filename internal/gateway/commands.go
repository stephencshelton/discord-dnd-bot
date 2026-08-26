package gateway

import (
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
)

// commandSpec pairs a command definition with whether it can be used in DMs.
// DM-capable commands are registered GLOBALLY (with DMPermission) so they work
// in both servers and DMs without appearing twice; guild-only commands are
// registered per guild.
type commandSpec struct {
	def *discordgo.ApplicationCommand
	dm  bool
}

// commandDefs returns the guild-scoped command definitions (guild-only commands).
func commandDefs() []*discordgo.ApplicationCommand {
	var out []*discordgo.ApplicationCommand
	for _, sp := range allCommandSpecs() {
		if !sp.dm {
			out = append(out, sp.def)
		}
	}
	return out
}

// globalCommandDefs returns the globally-registered commands (DM-capable). A
// global command with DMPermission=true is available in both servers and DMs,
// which is why the DM-capable set lives here rather than being duplicated per
// guild. Global commands can take up to ~1h to propagate after a change.
func globalCommandDefs() []*discordgo.ApplicationCommand {
	dmAllowed := true
	var out []*discordgo.ApplicationCommand
	for _, sp := range allCommandSpecs() {
		if sp.dm {
			sp.def.DMPermission = &dmAllowed
			out = append(out, sp.def)
		}
	}
	return out
}

// dmCapableCommands returns the set of command names usable in DMs.
func dmCapableCommands() map[string]struct{} {
	m := make(map[string]struct{})
	for _, sp := range allCommandSpecs() {
		if sp.dm {
			m[sp.def.Name] = struct{}{}
		}
	}
	return m
}

// allCommandDefs returns every command definition (guild-only and DM-capable),
// for surfaces that must list them all, like /help.
func allCommandDefs() []*discordgo.ApplicationCommand {
	specs := allCommandSpecs()
	out := make([]*discordgo.ApplicationCommand, 0, len(specs))
	for _, sp := range specs {
		out = append(out, sp.def)
	}
	return out
}

// allCommandSpecs is the single source of truth for every slash command and
// whether it is DM-capable.
//
// DM-capable (work in servers AND DMs): commands that only need a resolvable
// campaign/guild (resolved via /dm-server for multi-server users) or no guild
// at all. Guild-only: voice recording (session), admin config whose
// ManageServer gating is meaningless in DMs (notes-channel, reindex), and
// commands that target a specific guild channel (remind, art).
func allCommandSpecs() []commandSpec {
	adminPerm := int64(discordgo.PermissionManageServer)

	return []commandSpec{
		// ---- Campaigns ----
		{dm: true, def: &discordgo.ApplicationCommand{
			Name:        "campaign",
			Description: "Manage campaigns (stories)",
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
		}},

		// ---- Characters ----
		{dm: true, def: &discordgo.ApplicationCommand{
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
		}},

		// ---- Worldbuilding (NPCs, locations, factions, quests) ----
		{dm: true, def: &discordgo.ApplicationCommand{
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
		}},

		// ---- Sessions (voice recording -> AI notes) — GUILD ONLY (needs voice) ----
		{dm: false, def: &discordgo.ApplicationCommand{
			Name:        "session",
			Description: "Record a game session and generate AI notes",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "start", Description: "Join your voice channel and start recording"},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "stop", Description: "Stop recording and generate session notes"},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status", Description: "Show the current/last session status"},
			},
		}},

		// ---- Dice roller (free, no AI) ----
		{dm: true, def: &discordgo.ApplicationCommand{
			Name:        "roll",
			Description: "Roll dice, e.g. 2d6+3, d20, or 4d6kh3",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "dice", Description: "Dice notation (default 1d20)", Required: false},
				{Type: discordgo.ApplicationCommandOptionString, Name: "reason", Description: "What's this roll for? (optional)", Required: false},
			},
		}},

		// ---- AI lore assistant ----
		{dm: true, def: &discordgo.ApplicationCommand{
			Name:        "lore",
			Description: "Ask the AI for worldbuilding help",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "prompt", Description: "What do you want?", Required: true},
			},
		}},

		// ---- Recap ----
		{dm: true, def: &discordgo.ApplicationCommand{
			Name:        "recap",
			Description: "Generate a 'previously on' recap from recent sessions",
		}},

		// ---- Session memory search ----
		{dm: true, def: &discordgo.ApplicationCommand{
			Name:        "search",
			Description: "Search completed session notes and transcripts",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "query", Description: "What do you remember?", Required: true},
			},
		}},

		// ---- Grounded Q&A over session notes (RAG) ----
		{dm: true, def: &discordgo.ApplicationCommand{
			Name:        "ask",
			Description: "Ask a question answered from your campaign's session notes",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "question", Description: "What do you want to know?", Required: true},
			},
		}},

		// ---- Scene art — GUILD ONLY (worker posts to the invoking channel) ----
		{dm: false, def: &discordgo.ApplicationCommand{
			Name:        "art",
			Description: "Generate scene art with AI",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "scene", Description: "Describe the scene", Required: true},
			},
		}},

		// ---- Reminders — GUILD ONLY (targets a guild channel) ----
		{dm: false, def: &discordgo.ApplicationCommand{
			Name:        "remind",
			Description: "Manage the recurring session reminder for the active campaign",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "set",
					Description: "Set a weekly reminder",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionInteger, Name: "weekday", Description: "Day of week", Required: true, Choices: weekdayChoices()},
						{Type: discordgo.ApplicationCommandOptionString, Name: "time", Description: "24h time UTC, e.g. 18:30", Required: true},
					},
				},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "clear", Description: "Clear the reminder"},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "show", Description: "Show the current reminder"},
			},
		}},

		// ---- Notes channel (admin) — GUILD ONLY (admin + guild channel) ----
		{dm: false, def: &discordgo.ApplicationCommand{
			Name:                     "notes-channel",
			Description:              "Set the channel where session notes are posted (admin)",
			DefaultMemberPermissions: &adminPerm,
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionChannel, Name: "channel", Description: "Target channel", Required: true, ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText}},
			},
		}},

		// ---- Reindex campaign memory for /ask (admin) — GUILD ONLY (admin) ----
		{dm: false, def: &discordgo.ApplicationCommand{
			Name:                     "reindex",
			Description:              "Rebuild /ask search memory from all completed sessions (admin)",
			DefaultMemberPermissions: &adminPerm,
		}},

		// ---- Feedback ----
		{dm: true, def: &discordgo.ApplicationCommand{
			Name:        "feedback",
			Description: "Send feedback to the bot team",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "message", Description: "Your feedback", Required: true},
			},
		}},

		// ---- Help ----
		{dm: true, def: &discordgo.ApplicationCommand{
			Name: "help", Description: "How to use the bot", Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "command", Description: "Show detailed options for one command", Required: false, Autocomplete: true},
			},
		}},

		// ---- DM server selection (DM chat context) ----
		{dm: true, def: &discordgo.ApplicationCommand{
			Name:        "dm-server",
			Description: "Choose which server's campaign your DMs with me use (and switch between them)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:         discordgo.ApplicationCommandOptionString,
					Name:         "server",
					Description:  "The server to use for DM chat/recaps (leave empty to see your current choice)",
					Required:     false,
					Autocomplete: true,
				},
			},
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

// registerCommands installs global (DM-capable) commands and per-guild commands.
func (g *Gateway) registerCommands() error {
	ids, err := registerGuildCommands(g.sess, g.log, g.appID, g.cfg.Discord.AllowedGuildIDs(), commandDefs(), globalCommandDefs())
	g.regIDs = ids
	return err
}

// registerGuildCommands installs globalDefs as GLOBAL commands (so DM-only
// commands work) and guildDefs into each allowlisted guild. It is resilient:
// one bad guild does not abort the rest.
//
// A single guild can legitimately fail (e.g. the bot was invited there without
// the applications.commands scope -> HTTP 403 "Missing Access"), and that must
// not prevent commands from registering in the other guilds. Failures are
// logged and skipped; an error is returned only if the global set fails or
// EVERY guild fails (which usually points at a systemic problem like a bad app
// ID/token). Returns the IDs of all successfully created guild commands.
func registerGuildCommands(r commandRegistrar, log *slog.Logger, appID string, guildIDs []string, guildDefs, globalDefs []*discordgo.ApplicationCommand) ([]string, error) {
	// Register the global (DM-capable) command set. This replaces the previous
	// "clear all globals" call; global commands can take up to ~1h to propagate,
	// which is fine for a rarely-changed set like /dm-server.
	if _, err := r.ApplicationCommandBulkOverwrite(appID, "", globalDefs); err != nil {
		return nil, fmt.Errorf("register global commands: %w", err)
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
		created, err := r.ApplicationCommandBulkOverwrite(appID, guildID, guildDefs)
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
