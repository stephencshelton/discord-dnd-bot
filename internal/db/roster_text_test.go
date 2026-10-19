package db

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPCRosterLine protects speaker attribution: transcripts are labeled by
// Discord name, so the roster must say which player plays which character, and
// a sparse PC must not render dangling separators like "Lv 0" or an empty "—".
func TestPCRosterLine(t *testing.T) {
	for _, tc := range []struct {
		pc     PlayerCharacter
		player string
		want   string
	}{
		{PlayerCharacter{Name: "Throk Bloodrend", Race: "Orc", Class: "Barbarian", Level: 15}, "PlayerOne",
			"Throk Bloodrend — Lv 15 Orc Barbarian, played by PlayerOne"},
		{PlayerCharacter{Name: "Chak", Race: "Goblin", Class: "Rogue", Level: 14}, "", "Chak — Lv 14 Goblin Rogue"},
		{PlayerCharacter{Name: "Throb"}, "", "Throb"},
		{PlayerCharacter{Name: "Throb"}, "PlayerTwo", "Throb — played by PlayerTwo"},
	} {
		if got := tc.pc.RosterLine(tc.player); got != tc.want {
			t.Errorf("RosterLine = %q, want %q", got, tc.want)
		}
	}
}

// TestEntityRosterLineBoundsDescription keeps a long canon description from
// bloating every prompt that carries the roster: only an identifying excerpt
// is sent.
func TestEntityRosterLineBoundsDescription(t *testing.T) {
	e := WorldEntity{Kind: KindNPC, Name: "Zoldanox", Description: strings.Repeat("fungus dragon lore ", 50)}
	got := e.RosterLine()
	prefix := "[npc] Zoldanox — "
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("line = %q", got)
	}
	if n := utf8.RuneCountInString(strings.TrimPrefix(got, prefix)); n > rosterDescRunes {
		t.Errorf("description excerpt is %d runes, bound is %d", n, rosterDescRunes)
	}
	if got := (WorldEntity{Kind: KindLocation, Name: "Shevaroth"}).RosterLine(); got != "[location] Shevaroth" {
		t.Errorf("no-description line = %q", got)
	}
}
