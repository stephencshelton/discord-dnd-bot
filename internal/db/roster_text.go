package db

import (
	"fmt"
	"strings"

	"github.com/stephencshelton/discord-dnd-bot/internal/discordfmt"
)

// rosterDescRunes bounds each entity's description in a name roster. A roster
// is there to identify WHO a name is ("fungus dragon whose lair is in
// Shevaroth") so a model can match a misheard name by role, not to restate
// canon. One clause is enough, and it keeps a campaign with a hundred entities
// to a few thousand tokens beside the records the prompt is really about.
const rosterDescRunes = 160

// RosterLine renders a PC for a name roster: "Throk Bloodrend — Lv 15 Orc
// Barbarian, played by xCptAmerica". player is the Discord name that labels the
// player's lines in a transcript; pass "" where there is no transcript to map
// (or the player wasn't in the call) and the clause is omitted.
func (p PlayerCharacter) RosterLine(player string) string {
	var desc []string
	if p.Level > 0 {
		desc = append(desc, fmt.Sprintf("Lv %d", p.Level))
	}
	if s := strings.TrimSpace(strings.TrimSpace(p.Race) + " " + strings.TrimSpace(p.Class)); s != "" {
		desc = append(desc, s)
	}
	line := strings.TrimSpace(p.Name)
	if len(desc) > 0 {
		line += " — " + strings.Join(desc, " ")
	}
	if player != "" {
		if len(desc) > 0 {
			line += ","
		} else {
			line += " —"
		}
		line += " played by " + player
	}
	return line
}

// RosterLine renders a world entity for a name roster: "[npc] Zoldanox — <short
// description>".
func (e WorldEntity) RosterLine() string {
	line := fmt.Sprintf("[%s] %s", e.Kind, strings.TrimSpace(e.Name))
	if d := strings.Join(strings.Fields(e.Description), " "); d != "" {
		line += " — " + discordfmt.Truncate(d, rosterDescRunes)
	}
	return line
}
