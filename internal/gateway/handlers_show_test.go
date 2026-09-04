package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/discordfmt"
)

// findSubcommand returns the named subcommand of the named command.
func findSubcommand(t *testing.T, command, sub string) discord.ApplicationCommandOption {
	t.Helper()
	for _, c := range allCommandDefs() {
		if c.Name != command {
			continue
		}
		for _, o := range c.Options {
			if isSubcommand(o) && o.OptionName() == sub {
				return o
			}
		}
	}
	t.Fatalf("/%s %s is not registered", command, sub)
	return nil
}

// TestShowSubcommandsAreSelfDocumenting asserts the detail views expose the same
// discoverability affordances as the rest of the command surface: a description
// (so /help documents them automatically, since /help is generated from these
// defs) and an autocompleted name option (so the picker lists what's recorded
// instead of requiring the user to remember exact spelling).
func TestShowSubcommandsAreSelfDocumenting(t *testing.T) {
	for _, tc := range []struct{ command, wantOpt string }{
		{"world", "name"},
		{"character", "name"},
	} {
		sub := findSubcommand(t, tc.command, "show")
		if strings.TrimSpace(sub.OptionDescription()) == "" {
			t.Errorf("/%s show needs a description for /help to document it", tc.command)
		}

		var found bool
		for _, o := range subOptions(sub) {
			s, ok := o.(discord.ApplicationCommandOptionString)
			if !ok || s.Name != tc.wantOpt {
				continue
			}
			found = true
			if !s.Autocomplete {
				t.Errorf("/%s show %s must set Autocomplete so options populate like every other name option",
					tc.command, tc.wantOpt)
			}
			if strings.TrimSpace(s.Description) == "" {
				t.Errorf("/%s show %s needs a description", tc.command, tc.wantOpt)
			}
			// Discord rejects a command that both autocompletes and offers static
			// choices for the same option.
			if len(s.Choices) > 0 {
				t.Errorf("/%s show %s cannot have both Autocomplete and Choices", tc.command, tc.wantOpt)
			}
		}
		if !found {
			t.Errorf("/%s show is missing its %q option", tc.command, tc.wantOpt)
		}
	}
}

// TestWorldShowKindIsOptionalWithChoices documents the deliberate difference from
// edit/delete: you generally remember an entry's NAME, not its category, so kind
// is optional here and only disambiguates a name reused across kinds. It still
// uses the same static choice list as the other /world subcommands.
func TestWorldShowKindIsOptionalWithChoices(t *testing.T) {
	sub := findSubcommand(t, "world", "show")
	for _, o := range subOptions(sub) {
		s, ok := o.(discord.ApplicationCommandOptionString)
		if !ok || s.Name != "kind" {
			continue
		}
		if s.Required {
			t.Error("/world show kind must be optional (name alone should work)")
		}
		if len(s.Choices) != len(worldKindChoices()) {
			t.Errorf("kind should offer the standard %d world-kind choices, got %d",
				len(worldKindChoices()), len(s.Choices))
		}
		return
	}
	t.Error("/world show is missing its kind option")
}

// TestWorldShowNameIsRequired ensures the lookup key is mandatory (unlike
// /character show, which defaults to the caller's own character).
func TestWorldShowNameIsRequired(t *testing.T) {
	sub := findSubcommand(t, "world", "show")
	for _, o := range subOptions(sub) {
		if s, ok := o.(discord.ApplicationCommandOptionString); ok && s.Name == "name" {
			if !s.Required {
				t.Error("/world show name must be required")
			}
			return
		}
	}
	t.Error("missing name option")
}

// TestCharacterShowNameIsOptional ensures `/character show` with no arguments
// works and means "my character".
func TestCharacterShowNameIsOptional(t *testing.T) {
	sub := findSubcommand(t, "character", "show")
	for _, o := range subOptions(sub) {
		if s, ok := o.(discord.ApplicationCommandOptionString); ok && s.Name == "name" {
			if s.Required {
				t.Error("/character show name should be optional (defaults to your own character)")
			}
			return
		}
	}
	t.Error("missing name option")
}

// TestWorldEntityEmbedShowsFullRecord verifies the detail view surfaces the
// structured metadata using the SAME labels as the add/edit form, so the read and
// write views describe fields identically.
func TestWorldEntityEmbedShowsFullRecord(t *testing.T) {
	e := db.WorldEntity{
		ID:          uuid.New(),
		Kind:        db.KindQuest,
		Name:        "Hunt the Spore Queen",
		Description: "Track the fungal high priestess to Deathstalk Tower.",
		Metadata: map[string]any{
			"status":    "active",
			"objective": "Find the tablet",
			"giver":     "Prince Delamian",
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	embed, overflow := worldEntityEmbed(e)

	if overflow != "" {
		t.Errorf("a short description should not overflow, got %q", overflow)
	}
	if !strings.Contains(embed.Title, e.Name) {
		t.Errorf("title should name the entry, got %q", embed.Title)
	}
	if !strings.Contains(embed.Description, "Deathstalk Tower") {
		t.Errorf("description should be shown in full, got %q", embed.Description)
	}
	// Every populated metadata field must appear, labelled as in the form.
	want := map[string]string{"Status": "active", "Objective": "Find the tablet", "Quest giver": "Prince Delamian"}
	got := map[string]string{}
	for _, f := range embed.Fields {
		got[f.Name] = f.Value
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("field %q = %q, want %q (fields: %+v)", name, got[name], value, got)
		}
	}
}

// TestWorldEntityEmbedSurfacesUnknownMetadata ensures metadata an approved
// proposal merged in that isn't part of the kind's form is still displayed —
// otherwise approved data would be invisible in the UI.
func TestWorldEntityEmbedSurfacesUnknownMetadata(t *testing.T) {
	e := db.WorldEntity{
		Kind:        db.KindNPC,
		Name:        "Irovalin",
		Description: "Recovered researcher.",
		Metadata:    map[string]any{"role": "researcher", "allegiance": "Kyonin"},
	}
	embed, _ := worldEntityEmbed(e)
	var sawExtra bool
	for _, f := range embed.Fields {
		if f.Name == "allegiance" && f.Value == "Kyonin" {
			sawExtra = true
		}
	}
	if !sawExtra {
		t.Errorf("metadata outside the kind's form fields must still be shown, got %+v", embed.Fields)
	}
}

// TestWorldEntityEmbedEmptyDescription checks the placeholder, since Discord
// rejects an empty embed description.
func TestWorldEntityEmbedEmptyDescription(t *testing.T) {
	embed, _ := worldEntityEmbed(db.WorldEntity{Kind: db.KindNPC, Name: "Nobody"})
	if strings.TrimSpace(embed.Description) == "" {
		t.Error("empty description must be replaced with a placeholder")
	}
}

// TestShowOverflowsRatherThanTruncating is the key behavioural guarantee of a
// detail view: an entry whose description has been appended to many times (every
// approved proposal appends) must be shown IN FULL via overflow messages, not cut
// short the way a listing row is.
func TestShowOverflowsRatherThanTruncating(t *testing.T) {
	long := strings.Repeat("The party pressed deeper into the ruined amphitheater. ", 200) // ~10.8k chars
	e := db.WorldEntity{Kind: db.KindLocation, Name: "Chevaroth", Description: long}

	embed, overflow := worldEntityEmbed(e)
	if overflow == "" {
		t.Fatal("a long description must spill into overflow, not be discarded")
	}
	if n := len([]rune(embed.Description)); n > discordfmt.EmbedDescriptionLimit {
		t.Errorf("embed description = %d runes, exceeds %d", n, discordfmt.EmbedDescriptionLimit)
	}
	// Nothing may be lost: the embed body plus the overflow must reconstruct the
	// original text (ignoring the whitespace introduced at chunk joins).
	rejoined := strings.Join(strings.Fields(embed.Description+" "+overflow), " ")
	if rejoined != strings.Join(strings.Fields(long), " ") {
		t.Error("content was lost or reordered between the embed and its overflow")
	}
}

// TestSplitForShowKeepsShortTextInline guards the common case: a normal-length
// entry must not generate pointless follow-up messages.
func TestSplitForShowKeepsShortTextInline(t *testing.T) {
	head, rest := splitForShow("A short description.", showDescBudget)
	if head != "A short description." || rest != "" {
		t.Errorf("splitForShow = (%q, %q), want the text inline with no overflow", head, rest)
	}
}

// TestCharacterEmbedShowsSheet covers the character detail card, including the
// notes field where approved proposals record deeds.
func TestCharacterEmbedShowsSheet(t *testing.T) {
	pc := db.PlayerCharacter{
		Name:          "Throck",
		Class:         "Barbarian",
		Race:          "Orc",
		Level:         14,
		Notes:         "Cleaved a Tanglebriar Regent like a Beyblade.",
		DiscordUserID: "396406966458515460",
	}
	embed, overflow := characterEmbed(pc)
	if overflow != "" {
		t.Errorf("short notes should not overflow, got %q", overflow)
	}
	if !strings.Contains(embed.Description, "Beyblade") {
		t.Errorf("notes should be the card body, got %q", embed.Description)
	}
	got := map[string]string{}
	for _, f := range embed.Fields {
		got[f.Name] = f.Value
	}
	for name, value := range map[string]string{"Class": "Barbarian", "Race": "Orc", "Level": "14"} {
		if got[name] != value {
			t.Errorf("field %q = %q, want %q", name, got[name], value)
		}
	}
	if got["Player"] != "<@396406966458515460>" {
		t.Errorf("Player field should mention the owner, got %q", got["Player"])
	}
}

// TestCharacterEmbedOmitsEmptyFields ensures blank optional fields aren't
// rendered as empty rows (Discord rejects empty field values outright).
func TestCharacterEmbedOmitsEmptyFields(t *testing.T) {
	embed, _ := characterEmbed(db.PlayerCharacter{Name: "Mystery"})
	for _, f := range embed.Fields {
		if strings.TrimSpace(f.Value) == "" {
			t.Errorf("field %q has an empty value", f.Name)
		}
	}
	if strings.TrimSpace(embed.Description) == "" {
		t.Error("empty notes must be replaced with a placeholder")
	}
}

// TestWorldKindColorsAreDistinct keeps the cards visually distinguishable.
func TestWorldKindColorsAreDistinct(t *testing.T) {
	seen := map[int]db.WorldEntityKind{}
	for _, k := range db.AllWorldKinds {
		c := worldKindColor(k)
		if prev, dup := seen[c]; dup {
			t.Errorf("kinds %s and %s share color %#06x", prev, k, c)
		}
		seen[c] = k
	}
}
