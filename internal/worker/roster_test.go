package worker

import (
	"testing"

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
