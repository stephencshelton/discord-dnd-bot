package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"
)

// handleHelp renders usage help generated directly from the registered command
// definitions, so it always reflects the real command surface (no drift). With
// no argument it lists every command; with `command:<name>` it details one.
func (g *Gateway) handleHelp(_ context.Context, ic *ictx) error {
	want := strings.TrimSpace(strings.ToLower(ic.optString("command")))
	var e discord.Embed
	if want != "" {
		e = commandDetailEmbed(want)
	} else {
		e = commandListEmbed()
	}
	return ic.replyEmbedEphemeral(e)
}

const helpColor = 0x8b5cf6

// isSubcommand reports whether an option is a subcommand (or subcommand group).
func isSubcommand(o discord.ApplicationCommandOption) bool {
	return o.Type() == discord.ApplicationCommandOptionTypeSubCommand ||
		o.Type() == discord.ApplicationCommandOptionTypeSubCommandGroup
}

// subOptions returns the child options of a subcommand option, or nil.
func subOptions(o discord.ApplicationCommandOption) []discord.ApplicationCommandOption {
	switch s := o.(type) {
	case discord.ApplicationCommandOptionSubCommand:
		return s.Options
	case discord.ApplicationCommandOptionSubCommandGroup:
		out := make([]discord.ApplicationCommandOption, 0, len(s.Options))
		for _, sub := range s.Options {
			out = append(out, sub)
		}
		return out
	}
	return nil
}

// optionRequired reports whether a (non-subcommand) option is required.
func optionRequired(o discord.ApplicationCommandOption) bool {
	switch v := o.(type) {
	case discord.ApplicationCommandOptionString:
		return v.Required
	case discord.ApplicationCommandOptionInt:
		return v.Required
	case discord.ApplicationCommandOptionBool:
		return v.Required
	case discord.ApplicationCommandOptionChannel:
		return v.Required
	case discord.ApplicationCommandOptionUser:
		return v.Required
	case discord.ApplicationCommandOptionRole:
		return v.Required
	}
	return false
}

// optionChoiceNames returns the fixed choice labels for an option, if any.
func optionChoiceNames(o discord.ApplicationCommandOption) []string {
	switch v := o.(type) {
	case discord.ApplicationCommandOptionString:
		names := make([]string, 0, len(v.Choices))
		for _, c := range v.Choices {
			names = append(names, c.Name)
		}
		return names
	case discord.ApplicationCommandOptionInt:
		names := make([]string, 0, len(v.Choices))
		for _, c := range v.Choices {
			names = append(names, c.Name)
		}
		return names
	}
	return nil
}

// commandListEmbed builds the overview: one field per command, each listing its
// subcommands (if any) or a one-line option summary.
func commandListEmbed() discord.Embed {
	e := discord.Embed{
		Title:       "📖 DnD Bot — Commands",
		Color:       helpColor,
		Description: "Type `/help command:<name>` for a command's full options. Discord's slash-command picker also shows each option as you type.",
	}
	for _, c := range allCommandDefs() {
		e.Fields = append(e.Fields, discord.EmbedField{
			Name:   "/" + c.Name,
			Value:  clampField(commandSummary(c)),
			Inline: boolPtr(false),
		})
	}
	e.Fields = append(e.Fields, discord.EmbedField{
		Name:  "Chat",
		Value: "**@mention** me in a channel or **DM** me to chat about your campaign. If you're in more than one of my servers, use `/dm-server` to pick which one your DMs use.",
	})
	// The command set grows over time; budget the fields against the embed's
	// 6000-char total so adding commands can't silently break /help.
	e.Fields = fitEmbedFields(len([]rune(e.Title))+len([]rune(e.Description)), e.Fields)
	return e
}

// commandSummary is the one-field body for a command in the overview.
func commandSummary(c discord.SlashCommandCreate) string {
	var b strings.Builder
	if c.Description != "" {
		b.WriteString(c.Description)
	}
	subs := make([]string, 0)
	opts := make([]string, 0)
	for _, o := range c.Options {
		if isSubcommand(o) {
			subs = append(subs, fmt.Sprintf("`%s` — %s", o.OptionName(), o.OptionDescription()))
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
func commandDetailEmbed(name string) discord.Embed {
	var cmd *discord.SlashCommandCreate
	for _, c := range allCommandDefs() {
		if c.Name == name {
			cc := c
			cmd = &cc
			break
		}
	}
	if cmd == nil {
		return discord.Embed{
			Title:       "Unknown command",
			Color:       helpColor,
			Description: fmt.Sprintf("No command named `%s`. Run `/help` to see them all.", name),
		}
	}

	e := discord.Embed{
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
			e.Fields = append(e.Fields, discord.EmbedField{
				Name:  fmt.Sprintf("/%s %s", cmd.Name, o.OptionName()),
				Value: clampField(subcommandDetail(o)),
			})
		}
	}
	if !hasSub {
		if body := optionsDetail(cmd.Options); body != "" {
			e.Fields = append(e.Fields, discord.EmbedField{Name: "Options", Value: clampField(body)})
		} else {
			e.Description += "\n\n_No options._"
		}
	}
	return e
}

// subcommandDetail renders a subcommand's description and its options.
func subcommandDetail(sub discord.ApplicationCommandOption) string {
	var b strings.Builder
	if sub.OptionDescription() != "" {
		b.WriteString(sub.OptionDescription())
		b.WriteString("\n")
	}
	if body := optionsDetail(subOptions(sub)); body != "" {
		b.WriteString(body)
	} else {
		b.WriteString("_No options._")
	}
	return b.String()
}

// optionsDetail renders one bullet per (non-subcommand) option with its type,
// required marker, description, and any fixed choices.
func optionsDetail(opts []discord.ApplicationCommandOption) string {
	var lines []string
	for _, o := range opts {
		if isSubcommand(o) {
			continue
		}
		req := "optional"
		if optionRequired(o) {
			req = "**required**"
		}
		line := fmt.Sprintf("• `%s` (%s, %s) — %s", o.OptionName(), optionTypeName(o.Type()), req, o.OptionDescription())
		if names := optionChoiceNames(o); len(names) > 0 {
			line += " [" + strings.Join(names, ", ") + "]"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// formatOptionInline is a compact "name(required?)" for the overview.
func formatOptionInline(o discord.ApplicationCommandOption) string {
	if optionRequired(o) {
		return fmt.Sprintf("`%s`*", o.OptionName())
	}
	return fmt.Sprintf("`%s`", o.OptionName())
}

// optionTypeName maps a Discord option type to a friendly label.
func optionTypeName(t discord.ApplicationCommandOptionType) string {
	switch t {
	case discord.ApplicationCommandOptionTypeString:
		return "text"
	case discord.ApplicationCommandOptionTypeInt:
		return "number"
	case discord.ApplicationCommandOptionTypeBool:
		return "yes/no"
	case discord.ApplicationCommandOptionTypeChannel:
		return "channel"
	case discord.ApplicationCommandOptionTypeUser:
		return "user"
	case discord.ApplicationCommandOptionTypeRole:
		return "role"
	default:
		return "value"
	}
}

// clampField keeps a field value within Discord's 1024-char field limit,
// breaking on a word boundary. An empty value is replaced with a placeholder
// because Discord rejects empty field values.
func clampField(s string) string {
	return truncateForField(s)
}

// boolPtr returns a pointer to b (for discord.EmbedField.Inline).
func boolPtr(b bool) *bool { return &b }

// helpCommandNames returns the top-level command names, for /help autocomplete.
func helpCommandNames() []string {
	defs := allCommandDefs()
	names := make([]string, 0, len(defs))
	for _, c := range defs {
		names = append(names, c.Name)
	}
	return names
}
