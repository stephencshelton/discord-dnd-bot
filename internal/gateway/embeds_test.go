package gateway

import (
	"fmt"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"

	"github.com/stephencshelton/discord-dnd-bot/internal/discordfmt"
)

// TestListLineBoundsEachEntry is the regression test for the `/world list`
// failure (`50035 ... content BASE_TYPE_MAX_LENGTH: Must be 2000 or fewer`).
// Approving a /review-session proposal APPENDS to an entity's description, so a
// long-lived NPC's line grows without bound; each row must stay an index entry.
func TestListLineBoundsEachEntry(t *testing.T) {
	huge := "• _[npc]_ **Irovalin** — " + strings.Repeat("the demon was extracted and he recovered slowly. ", 200)
	got := listLine(huge)

	if n := len([]rune(got)); n > listLineLimit {
		t.Errorf("listLine = %d runes, want <= %d", n, listLineLimit)
	}
	if !strings.HasPrefix(got, "• _[npc]_ **Irovalin**") {
		t.Errorf("the identifying prefix must survive truncation, got %q", got)
	}
	// A short row is untouched.
	const short = "• _[npc]_ **Varek** — guard captain"
	if listLine(short) != short {
		t.Errorf("short row should be unchanged, got %q", listLine(short))
	}
}

// TestListLineKeepsWholeListingUnderMessageLimit models the reported scenario
// end-to-end: many entities with long, appended descriptions must chunk into
// valid messages rather than one over-length body.
func TestListLineKeepsWholeListingUnderMessageLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("**World of Kyonin** — 40 entries:\n")
	longDesc := strings.Repeat("appended detail from an approved proposal. ", 20)
	for i := 0; i < 40; i++ {
		line := fmt.Sprintf("• _[npc]_ **NPC %d** — %s", i, longDesc)
		b.WriteString(listLine(line) + "\n")
	}
	chunks := discordfmt.ChunkMarkdown(b.String(), discordfmt.ChunkLimit)
	if len(chunks) < 2 {
		t.Fatalf("expected the listing to span multiple messages, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > discordfmt.MessageLimit {
			t.Errorf("chunk %d = %d runes, exceeds Discord's %d limit", i, n, discordfmt.MessageLimit)
		}
	}
}

// TestFitEmbedFieldsRespectsFieldAndTotalLimits covers the /search bug class:
// eight snippets that each fit the 1024 field cap still breach the 6000-char
// embed total, and Discord rejects the whole interaction when they do.
func TestFitEmbedFieldsRespectsFieldAndTotalLimits(t *testing.T) {
	fields := make([]discord.EmbedField, 0, 8)
	for i := 0; i < 8; i++ {
		fields = append(fields, discord.EmbedField{
			Name:  fmt.Sprintf("Session %d", i),
			Value: strings.Repeat("x", 2000), // over the per-field cap on purpose
		})
	}
	got := fitEmbedFields(len("Campaign memory"), fields)

	total := len([]rune("Campaign memory"))
	for _, f := range got {
		if n := len([]rune(f.Value)); n > discordfmt.EmbedFieldValueLimit {
			t.Errorf("field %q value = %d runes, want <= %d", f.Name, n, discordfmt.EmbedFieldValueLimit)
		}
		if n := len([]rune(f.Name)); n > discordfmt.EmbedFieldNameLimit {
			t.Errorf("field name = %d runes, want <= %d", n, discordfmt.EmbedFieldNameLimit)
		}
		total += len([]rune(f.Name)) + len([]rune(f.Value))
	}
	if total > discordfmt.EmbedTotalLimit {
		t.Errorf("embed total = %d runes, want <= %d", total, discordfmt.EmbedTotalLimit)
	}
	if len(got) >= len(fields) {
		t.Error("expected some fields to be dropped to fit the budget")
	}
	// The user must be told the result was cut short.
	last := got[len(got)-1]
	if !strings.Contains(last.Name, "and more") {
		t.Errorf("expected a trailing 'and more' note, got %q", last.Name)
	}
}

// TestFitEmbedFieldsCapsFieldCount guards Discord's 25-field maximum.
func TestFitEmbedFieldsCapsFieldCount(t *testing.T) {
	fields := make([]discord.EmbedField, 0, 40)
	for i := 0; i < 40; i++ {
		fields = append(fields, discord.EmbedField{Name: fmt.Sprintf("f%d", i), Value: "short"})
	}
	got := fitEmbedFields(0, fields)
	if len(got) > discordfmt.MaxEmbedFields {
		t.Errorf("got %d fields, want <= %d", len(got), discordfmt.MaxEmbedFields)
	}
}

// TestFitEmbedFieldsPassesThroughSmallInput ensures the common case is untouched.
func TestFitEmbedFieldsPassesThroughSmallInput(t *testing.T) {
	in := []discord.EmbedField{{Name: "A", Value: "one"}, {Name: "B", Value: "two"}}
	got := fitEmbedFields(20, in)
	if len(got) != 2 || got[0].Value != "one" || got[1].Value != "two" {
		t.Errorf("small field set should pass through unchanged, got %+v", got)
	}
}

// TestTruncateForFieldRejectsEmpty documents that Discord refuses empty field
// values, so a placeholder is substituted.
func TestTruncateForFieldRejectsEmpty(t *testing.T) {
	if got := truncateForField(""); got == "" {
		t.Error("empty field value must be replaced with a placeholder")
	}
	if got := truncateForField(strings.Repeat("y", 5000)); len([]rune(got)) > discordfmt.EmbedFieldValueLimit {
		t.Errorf("got %d runes, want <= %d", len([]rune(got)), discordfmt.EmbedFieldValueLimit)
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1, "y", "ies"); got != "y" {
		t.Errorf("plural(1) = %q, want %q", got, "y")
	}
	if got := plural(2, "y", "ies"); got != "ies" {
		t.Errorf("plural(2) = %q, want %q", got, "ies")
	}
	if got := plural(0, "y", "ies"); got != "ies" {
		t.Errorf("plural(0) = %q, want %q", got, "ies")
	}
}
