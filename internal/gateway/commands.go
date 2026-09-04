package gateway

import (
	"fmt"
	"log/slog"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// commandSpec pairs a command definition with whether it can be used in DMs.
// DM-capable commands are registered GLOBALLY (with DM interaction contexts) so
// they work in both servers and DMs without appearing twice; guild-only commands
// are registered per guild.
type commandSpec struct {
	def discord.SlashCommandCreate
	dm  bool
}

// guildContexts restricts a command to guilds only.
var guildContexts = []discord.InteractionContextType{discord.InteractionContextTypeGuild}

// dmContexts allows a command in guilds, bot DMs, and group DMs (DM-capable).
var dmContexts = []discord.InteractionContextType{
	discord.InteractionContextTypeGuild,
	discord.InteractionContextTypeBotDM,
	discord.InteractionContextTypePrivateChannel,
}

// commandDefs returns the guild-scoped command definitions (guild-only commands),
// with guild-only interaction contexts.
func commandDefs() []discord.ApplicationCommandCreate {
	var out []discord.ApplicationCommandCreate
	for _, sp := range allCommandSpecs() {
		if !sp.dm {
			def := sp.def
			def.Contexts = guildContexts
			out = append(out, def)
		}
	}
	return out
}

// globalCommandDefs returns the globally-registered commands (DM-capable). A
// global command with DM interaction contexts is available in both servers and
// DMs, which is why the DM-capable set lives here rather than being duplicated
// per guild. Global commands can take up to ~1h to propagate after a change.
func globalCommandDefs() []discord.ApplicationCommandCreate {
	var out []discord.ApplicationCommandCreate
	for _, sp := range allCommandSpecs() {
		if sp.dm {
			def := sp.def
			def.Contexts = dmContexts
			out = append(out, def)
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
func allCommandDefs() []discord.SlashCommandCreate {
	specs := allCommandSpecs()
	out := make([]discord.SlashCommandCreate, 0, len(specs))
	for _, sp := range specs {
		out = append(out, sp.def)
	}
	return out
}

// allCommandSpecs is the single source of truth for every slash command and
// whether it is DM-capable.
func allCommandSpecs() []commandSpec {
	return []commandSpec{
		{dm: true, def: discord.SlashCommandCreate{
			Name:        "campaign",
			Description: "Manage campaigns (stories)",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionSubCommand{
					Name:        "create",
					Description: "Create a new campaign",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "name", Description: "Campaign name", Required: true},
						discord.ApplicationCommandOptionString{Name: "system", Description: "Game system (e.g. D&D 5e)"},
						discord.ApplicationCommandOptionString{Name: "premise", Description: "One-line premise"},
					},
				},
				discord.ApplicationCommandOptionSubCommand{Name: "list", Description: "List campaigns"},
				discord.ApplicationCommandOptionSubCommand{
					Name:        "activate",
					Description: "Set the active campaign",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "name", Description: "Campaign name", Required: true, Autocomplete: true},
					},
				},
				discord.ApplicationCommandOptionSubCommand{
					Name:        "archive",
					Description: "Archive a campaign",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "name", Description: "Campaign name", Required: true, Autocomplete: true},
					},
				},
				discord.ApplicationCommandOptionSubCommand{
					Name:        "delete",
					Description: "Permanently delete a campaign and all its sessions, notes, and audio",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "name", Description: "Campaign name", Required: true, Autocomplete: true},
						discord.ApplicationCommandOptionString{Name: "confirm", Description: "Type the campaign name again to confirm deletion", Required: true},
					},
				},
			},
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "character",
			Description: "Manage player characters",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionSubCommand{Name: "add", Description: "Add or update your character (opens a form)"},
				discord.ApplicationCommandOptionSubCommand{Name: "edit", Description: "Edit your character (opens a pre-filled form)"},
				discord.ApplicationCommandOptionSubCommand{Name: "list", Description: "List characters in the active campaign"},
				discord.ApplicationCommandOptionSubCommand{
					Name:        "delete",
					Description: "Delete a character",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "name", Description: "Character name", Required: true, Autocomplete: true},
					},
				},
			},
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "world",
			Description: "Manage worldbuilding entries (NPCs, locations, factions, quests, story hooks)",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionSubCommand{
					Name:        "add",
					Description: "Add to a world entry (opens a form; adds detail, never overwrites)",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "kind", Description: "Type of entry", Required: true, Choices: worldKindChoices()},
					},
				},
				discord.ApplicationCommandOptionSubCommand{
					Name:        "edit",
					Description: "Edit a world entry (opens a pre-filled form; replaces its fields)",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "kind", Description: "Type of entry", Required: true, Choices: worldKindChoices()},
						discord.ApplicationCommandOptionString{Name: "name", Description: "Which entry to edit", Required: true, Autocomplete: true},
					},
				},
				discord.ApplicationCommandOptionSubCommand{
					Name:        "list",
					Description: "List world entries",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "kind", Description: "Filter by type", Choices: worldKindChoices()},
					},
				},
				discord.ApplicationCommandOptionSubCommand{
					Name:        "delete",
					Description: "Delete a world entry",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "kind", Description: "Type of entry", Required: true, Choices: worldKindChoices()},
						discord.ApplicationCommandOptionString{Name: "name", Description: "Which entry to delete", Required: true, Autocomplete: true},
					},
				},
			},
		}},

		{dm: false, def: discord.SlashCommandCreate{
			Name:        "session",
			Description: "Record a game session and generate AI notes",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionSubCommand{Name: "start", Description: "Join your voice channel and start recording"},
				discord.ApplicationCommandOptionSubCommand{Name: "stop", Description: "Stop recording and generate session notes"},
				discord.ApplicationCommandOptionSubCommand{Name: "status", Description: "Show the current/last session status"},
				discord.ApplicationCommandOptionSubCommand{
					Name:        "list",
					Description: "List recent sessions and their status",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{
							Name:        "status",
							Description: "Filter by status (default: failed)",
							Choices: []discord.ApplicationCommandOptionChoiceString{
								{Name: "failed", Value: "failed"},
								{Name: "processing", Value: "processing"},
								{Name: "recording", Value: "recording"},
								{Name: "complete", Value: "complete"},
							},
						},
					},
				},
				discord.ApplicationCommandOptionSubCommand{
					Name:        "requeue",
					Description: "Re-run transcription/notes for a session",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "session_id", Description: "Session ID from /session list", Required: true},
						// Re-running only the extraction step is cheap (the transcript and
						// notes already exist), whereas a full requeue re-transcribes hours
						// of audio. Worth its own switch so recovering missing
						// /review-session proposals doesn't cost a whole transcription.
						discord.ApplicationCommandOptionBool{
							Name:        "proposals_only",
							Description: "Only re-derive /review-session proposals (keeps the existing transcript and notes)",
						},
					},
				},
			},
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "roll",
			Description: "Roll dice, e.g. 2d6+3, d20, or 4d6kh3",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "dice", Description: "Dice notation (default 1d20)"},
				discord.ApplicationCommandOptionString{Name: "reason", Description: "What's this roll for? (optional)"},
			},
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "lore",
			Description: "Ask the AI for worldbuilding help",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "prompt", Description: "What do you want?", Required: true},
			},
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "recap",
			Description: "Generate a 'previously on' recap from recent sessions",
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "prep",
			Description: "Get a 'where we left off & what's next' briefing to start the next session",
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "search",
			Description: "Search completed session notes and transcripts",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "query", Description: "What do you remember?", Required: true},
			},
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "ask",
			Description: "Ask a question answered from your campaign's session notes",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "question", Description: "What do you want to know?", Required: true},
			},
		}},

		{dm: false, def: discord.SlashCommandCreate{
			Name:        "art",
			Description: "Generate scene art with AI",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "scene", Description: "Describe the scene", Required: true},
			},
		}},

		{dm: false, def: discord.SlashCommandCreate{
			Name:        "remind",
			Description: "Manage the recurring session reminder for the active campaign",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionSubCommand{
					Name:        "set",
					Description: "Set a weekly reminder",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionInt{Name: "weekday", Description: "Day of week", Required: true, Choices: weekdayChoices()},
						discord.ApplicationCommandOptionString{Name: "time", Description: "24h time UTC, e.g. 18:30", Required: true},
					},
				},
				discord.ApplicationCommandOptionSubCommand{Name: "clear", Description: "Clear the reminder"},
				discord.ApplicationCommandOptionSubCommand{Name: "show", Description: "Show the current reminder"},
			},
		}},

		{dm: false, def: discord.SlashCommandCreate{
			Name:        "notes-channel",
			Description: "Set the channel where session notes are posted",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionChannel{Name: "channel", Description: "Target channel", Required: true, ChannelTypes: []discord.ChannelType{discord.ChannelTypeGuildText}},
			},
		}},

		{dm: false, def: discord.SlashCommandCreate{
			Name:        "reindex",
			Description: "Rebuild /ask search memory from all completed sessions",
		}},

		{dm: false, def: discord.SlashCommandCreate{
			Name:        "review-session",
			Description: "Review AI-proposed campaign-world changes and approve/reject them",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "session_id", Description: "Review a specific session's proposals (leave empty for all pending)", Autocomplete: true},
			},
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "feedback",
			Description: "Send feedback to the bot team",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "message", Description: "Your feedback", Required: true},
			},
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "help",
			Description: "How to use the bot",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "command", Description: "Show detailed options for one command", Autocomplete: true},
			},
		}},

		{dm: true, def: discord.SlashCommandCreate{
			Name:        "dm-server",
			Description: "Choose which server's campaign your DMs with me use (and switch between them)",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "server", Description: "The server to use for DM chat/recaps (leave empty to see your current choice)", Autocomplete: true},
			},
		}},
	}
}

// worldKindChoices are the world-entity kinds shared by /world add and list.
func worldKindChoices() []discord.ApplicationCommandOptionChoiceString {
	return []discord.ApplicationCommandOptionChoiceString{
		{Name: "NPC", Value: "npc"},
		{Name: "Location", Value: "location"},
		{Name: "Faction", Value: "faction"},
		{Name: "Quest", Value: "quest"},
		{Name: "Story hook", Value: "hook"},
	}
}

func weekdayChoices() []discord.ApplicationCommandOptionChoiceInt {
	days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	out := make([]discord.ApplicationCommandOptionChoiceInt, 0, len(days))
	for i, d := range days {
		out = append(out, discord.ApplicationCommandOptionChoiceInt{Name: d, Value: i})
	}
	return out
}

// commandRegistrar is the slice of the Discord REST client that registerCommands
// needs. Abstracting it lets the registration logic be unit-tested without a
// live client.
type commandRegistrar interface {
	SetGlobalCommands(appID snowflake.ID, commands []discord.ApplicationCommandCreate, opts ...rest.RequestOpt) ([]discord.ApplicationCommand, error)
	SetGuildCommands(appID snowflake.ID, guildID snowflake.ID, commands []discord.ApplicationCommandCreate, opts ...rest.RequestOpt) ([]discord.ApplicationCommand, error)
}

// registerCommands installs global (DM-capable) commands and per-guild commands.
func (g *Gateway) registerCommands() error {
	return registerGuildCommands(g.client.Rest, g.log, g.appID, g.cfg.Discord.AllowedGuildIDs(), commandDefs(), globalCommandDefs())
}

// registerGuildCommands installs globalDefs as GLOBAL commands (so DM-only
// commands work) and guildDefs into each allowlisted guild. It is resilient:
// one bad guild does not abort the rest. An error is returned only if the global
// set fails or EVERY guild fails.
func registerGuildCommands(r commandRegistrar, log *slog.Logger, appID snowflake.ID, guildIDs []string, guildDefs, globalDefs []discord.ApplicationCommandCreate) error {
	if _, err := r.SetGlobalCommands(appID, globalDefs); err != nil {
		return fmt.Errorf("register global commands: %w", err)
	}

	if len(guildIDs) == 0 {
		log.Warn("no Discord guilds configured; guild commands disabled")
		return nil
	}

	var (
		succeeded int
		lastErr   error
	)
	for _, guildID := range guildIDs {
		gid, perr := snowflake.Parse(guildID)
		if perr != nil {
			lastErr = perr
			log.Error("invalid guild ID; skipping", "guild", guildID, "err", perr)
			continue
		}
		created, err := r.SetGuildCommands(appID, gid, guildDefs)
		if err != nil {
			lastErr = err
			log.Error("failed to register guild commands; skipping guild", "guild", guildID, "err", err)
			continue
		}
		succeeded++
		log.Info("registered guild commands", "count", len(created), "guild", guildID)
	}

	if succeeded == 0 {
		return fmt.Errorf("register commands: all %d guild(s) failed; last error: %w", len(guildIDs), lastErr)
	}
	return nil
}
