package worker

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
)

// TestBuildRosterMapsPlayersToCharacters protects speaker attribution in the
// notes: the transcript is labeled by Discord name, so a PC line must say which
// speaker plays that character, or the notes model guesses (and guessed the
// wrong player for the cleric).
func TestBuildRosterMapsPlayersToCharacters(t *testing.T) {
	pcs := []db.PlayerCharacter{
		{Name: "Throk Bloodrend", Race: "Orc", Class: "Barbarian", Level: 15, DiscordUserID: "u1"},
		{Name: "Chak", Race: "Goblin", Class: "Rogue", Level: 14, DiscordUserID: "u-absent"},
	}
	r := buildRoster(pcs, nil, map[string]string{"u1": "xCptAmerica"})

	want := []string{
		"Throk Bloodrend — Lv 15 Orc Barbarian, played by xCptAmerica",
		"Chak — Lv 14 Goblin Rogue", // player not in the call: no speaker to map
	}
	if len(r.characters) != len(want) {
		t.Fatalf("characters = %q", r.characters)
	}
	for i := range want {
		if r.characters[i] != want[i] {
			t.Errorf("characters[%d] = %q, want %q", i, r.characters[i], want[i])
		}
	}
}

// TestPCNotesLineSparse covers a PC created with only a name: the line must not
// carry dangling separators like "Lv 0" or an empty "—".
func TestPCNotesLineSparse(t *testing.T) {
	if got := pcNotesLine(db.PlayerCharacter{Name: "Throb"}, ""); got != "Throb" {
		t.Errorf("bare PC = %q", got)
	}
	if got := pcNotesLine(db.PlayerCharacter{Name: "Throb"}, "Woodithebear540"); got != "Throb — played by Woodithebear540" {
		t.Errorf("PC with player only = %q", got)
	}
}

// TestBuildRosterVocabularyOrder protects what reaches Whisper's small prompt
// window: PC names first, then NPCs before factions and locations, and no quest
// or hook titles, which are not proper nouns and would crowd out real names.
func TestBuildRosterVocabularyOrder(t *testing.T) {
	pcs := []db.PlayerCharacter{{Name: "Chak"}}
	entities := []db.WorldEntity{ // ListAllWorldEntities orders by kind, then name
		{Kind: db.KindFaction, Name: "Treerazer's Army"},
		{Kind: db.KindHook, Name: "The screaming soul pods"},
		{Kind: db.KindLocation, Name: "Shevaroth"},
		{Kind: db.KindNPC, Name: "Zoldanox"},
		{Kind: db.KindQuest, Name: "Kill the Sworn Queen"},
	}
	r := buildRoster(pcs, entities, nil)

	if want := "Chak, Zoldanox, Treerazer's Army, Shevaroth."; r.vocabulary != want {
		t.Errorf("vocabulary = %q, want %q", r.vocabulary, want)
	}
	if len(r.entities) != len(entities) {
		t.Errorf("notes glossary should list every entity kind, got %q", r.entities)
	}
}

// TestEntityNotesLineBoundsDescription keeps a long canon description from
// bloating the notes prompt: only an identifying excerpt is sent.
func TestEntityNotesLineBoundsDescription(t *testing.T) {
	e := db.WorldEntity{Kind: db.KindNPC, Name: "Zoldanox", Description: strings.Repeat("fungus dragon lore ", 50)}
	got := entityNotesLine(e)
	prefix := "[npc] Zoldanox — "
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("line = %q", got)
	}
	if n := utf8.RuneCountInString(strings.TrimPrefix(got, prefix)); n > notesEntityDescRunes {
		t.Errorf("description excerpt is %d runes, bound is %d", n, notesEntityDescRunes)
	}
	if got := entityNotesLine(db.WorldEntity{Kind: db.KindLocation, Name: "Shevaroth"}); got != "[location] Shevaroth" {
		t.Errorf("no-description line = %q", got)
	}
}
