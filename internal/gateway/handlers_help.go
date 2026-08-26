package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// handleHelp renders usage help generated directly from the registered command
// definitions, so it always reflects the real command surface (no drift). With
// no argument it lists every command; with `command:<name>` it details one.
func (g *Gateway) handleHelp(_ context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	want := strings.TrimSpace(strings.ToLower(optString(i.ApplicationCommandData().Options, "command")))
	var e *discordgo.MessageEmbed
	if want != "" {
		e = commandDetailEmbed(want)
	} else {
		e = commandListEmbed()
	}
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{e},
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	})
}

const helpColor = 0x8b5cf6

// isSubcommand reports whether an option is a subcommand (or subcommand group).
func isSubcommand(o *discordgo.ApplicationCommandOption) bool {
	return o.Type == discordgo.ApplicationCommandOptionSubCommand ||
		o.Type == discordgo.ApplicationCommandOptionSubCommandGroup
}

// commandListEmbed builds the overview: one field per command, each listing its
// subcommands (if any) or a one-line option summary.
func commandListEmbed() *discordgo.MessageEmbed {
	e := &discordgo.MessageEmbed{
		Title:       "📖 DnD Bot — Commands",
		Color:       helpColor,
		Description: "Type `/help command:<name>` for a command's full options. Discord's slash-command picker also shows each option as you type.",
	}
	for _, c := range commandDefs() {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:   "/" + c.Name,
			Value:  clampField(commandSummary(c)),
			Inline: false,
		})
	}
	e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
		Name:  "Chat",
		Value: "**@mention** me or **DM** me to chat about your campaign (DMs are opt-in per deployment).",
	})
	return e
}

// commandSummary is the one-field body for a command in the overview.
func commandSummary(c *discordgo.ApplicationCommand) string {
	var b strings.Builder
	if c.Description != "" {
		b.WriteString(c.Description)
	}
	subs := make([]string, 0)
	opts := make([]string, 0)
	for _, o := range c.Options {
		if isSubcommand(o) {
			subs = append(subs, fmt.Sprintf("`%s` — %s", o.Name, o.Description))
		} else {
			opts = append(opts, formatOptionInline(o))
		}
	}
	if len(subs) > 0 {
		b.WriteString("\n")
		b.WriteString(strings.Join(subs, "\n"))
	}
	if len(opts) > 0 {
		b.WriteString("\nOptions: ")
		b.WriteString(strings.Join(opts, ", "))
	}
	return b.String()
}

// commandDetailEmbed builds the full breakdown for a single command.
func commandDetailEmbed(name string) *discordgo.MessageEmbed {
	var cmd *discordgo.ApplicationCommand
	for _, c := range commandDefs() {
		if c.Name == name {
			cmd = c
			break
		}
	}
	if cmd == nil {
		return &discordgo.MessageEmbed{
			Title:       "Unknown command",
			Color:       helpColor,
			Description: fmt.Sprintf("No command named `%s`. Run `/help` to see them all.", name),
		}
	}

	e := &discordgo.MessageEmbed{
		Title:       "/" + cmd.Name,
		Color:       helpColor,
		Description: cmd.Description,
	}

	// Subcommands each get their own field listing their options; flat commands
	// get a single Options field.
	hasSub := false
	for _, o := range cmd.Options {
		if isSubcommand(o) {
			hasSub = true
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
				Name:  fmt.Sprintf("/%s %s", cmd.Name, o.Name),
				Value: clampField(subcommandDetail(o)),
			})
		}
	}
	if !hasSub {
		if body := optionsDetail(cmd.Options); body != "" {
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Options", Value: clampField(body)})
		} else {
			e.Description += "\n\n_No options._"
		}
	}
	return e
}

// subcommandDetail renders a subcommand's description and its options.
func subcommandDetail(sub *discordgo.ApplicationCommandOption) string {
	var b strings.Builder
	if sub.Description != "" {
		b.WriteString(sub.Description)
		b.WriteString("\n")
	}
	if body := optionsDetail(sub.Options); body != "" {
		b.WriteString(body)
	} else {
		b.WriteString("_No options._")
	}
	return b.String()
}

// optionsDetail renders one bullet per (non-subcommand) option with its type,
// required marker, description, and any fixed choices.
func optionsDetail(opts []*discordgo.ApplicationCommandOption) string {
	var lines []string
	for _, o := range opts {
		if isSubcommand(o) {
			continue
		}
		req := "optional"
		if o.Required {
			req = "**required**"
		}
		line := fmt.Sprintf("• `%s` (%s, %s) — %s", o.Name, optionTypeName(o.Type), req, o.Description)
		if len(o.Choices) > 0 {
			names := make([]string, 0, len(o.Choices))
			for _, ch := range o.Choices {
				names = append(names, ch.Name)
			}
			line += " [" + strings.Join(names, ", ") + "]"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// formatOptionInline is a compact "name(required?)" for the overview.
func formatOptionInline(o *discordgo.ApplicationCommandOption) string {
	if o.Required {
		return fmt.Sprintf("`%s`*", o.Name)
	}
	return fmt.Sprintf("`%s`", o.Name)
}

// optionTypeName maps a Discord option type to a friendly label.
func optionTypeName(t discordgo.ApplicationCommandOptionType) string {
	switch t {
	case discordgo.ApplicationCommandOptionString:
		return "text"
	case discordgo.ApplicationCommandOptionInteger:
		return "number"
	case discordgo.ApplicationCommandOptionBoolean:
		return "yes/no"
	case discordgo.ApplicationCommandOptionChannel:
		return "channel"
	case discordgo.ApplicationCommandOptionUser:
		return "user"
	case discordgo.ApplicationCommandOptionRole:
		return "role"
	default:
		return "value"
	}
}

// clampField keeps a field value within Discord's 1024-char limit.
func clampField(s string) string {
	const max = 1024
	r := []rune(s)
	if len(r) <= max {
		if s == "" {
			return "—"
		}
		return s
	}
	return string(r[:max-1]) + "…"
}

// helpCommandNames returns the top-level command names, for /help autocomplete.
func helpCommandNames() []string {
	defs := commandDefs()
	names := make([]string, 0, len(defs))
	for _, c := range defs {
		names = append(names, c.Name)
	}
	return names
}
