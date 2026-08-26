package gateway

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestCommandListEmbedCoversEveryCommand ensures the auto-generated overview has
// a field for every registered command, so /help can never silently drift.
func TestCommandListEmbedCoversEveryCommand(t *testing.T) {
	e := commandListEmbed()
	got := map[string]bool{}
	for _, f := range e.Fields {
		got[f.Name] = true
	}
	for _, c := range commandDefs() {
		if !got["/"+c.Name] {
			t.Errorf("/help overview missing command %q", c.Name)
		}
	}
}

// TestHelpRespectsDiscordEmbedLimits checks the overview and every per-command
// detail embed stay within Discord's field-count and field-length limits.
func TestHelpRespectsDiscordEmbedLimits(t *testing.T) {
	check := func(name string, e *discordgo.MessageEmbed) {
		if len(e.Fields) > 25 {
			t.Errorf("%s: %d fields, Discord allows max 25", name, len(e.Fields))
		}
		for _, f := range e.Fields {
			if n := len([]rune(f.Value)); n > 1024 {
				t.Errorf("%s: field %q value %d runes, Discord allows max 1024", name, f.Name, n)
			}
			if f.Value == "" {
				t.Errorf("%s: field %q has empty value", name, f.Name)
			}
		}
	}
	check("overview", commandListEmbed())
	for _, c := range commandDefs() {
		check("detail:"+c.Name, commandDetailEmbed(c.Name))
	}
}

// TestCommandDetailListsOptions verifies a known command's options surface in
// its detail embed (guards the option-rendering path).
func TestCommandDetailListsOptions(t *testing.T) {
	e := commandDetailEmbed("roll")
	joined := e.Description
	for _, f := range e.Fields {
		joined += "\n" + f.Name + "\n" + f.Value
	}
	for _, want := range []string{"dice", "reason"} {
		if !strings.Contains(joined, want) {
			t.Errorf("/help command:roll detail missing option %q; got:\n%s", want, joined)
		}
	}
}

// TestCommandDetailUnknown returns a helpful message rather than nil.
func TestCommandDetailUnknown(t *testing.T) {
	e := commandDetailEmbed("does-not-exist")
	if e == nil || e.Description == "" {
		t.Fatal("expected a non-empty embed for an unknown command")
	}
}
