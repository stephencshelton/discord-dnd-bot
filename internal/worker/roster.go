package worker

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/discordfmt"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/prompts"
)

// notesEntityDescRunes bounds each entity's description in the session-notes
// glossary. The glossary is there to identify WHO a name is ("fungus dragon
// whose lair is in Shevaroth"), not to restate canon. One clause is enough for
// the notes model to match a misheard name by role, and it keeps a campaign
// with a hundred entities to a few thousand tokens beside the transcript.
const notesEntityDescRunes = 160

// vocabularyKinds are the world-entity kinds whose names are spoken aloud at the
// table, in the order they are hinted to speech-to-text. Quests and hooks are
// left out: their names are titles ("Find the Sworn Queen"), not proper nouns,
// and would spend the hint's small budget on words Whisper already spells.
// NPCs come first because they are named most often.
var vocabularyKinds = []db.WorldEntityKind{db.KindNPC, db.KindFaction, db.KindLocation}

// campaignRoster is the campaign's canonical names, loaded once per session so
// the same lookup steers Whisper's spelling and then corrects the notes.
type campaignRoster struct {
	characters []string // session-notes lines for each player character
	entities   []string // session-notes lines for each world entity
	vocabulary string   // speech-to-text prompt
}

// loadRoster reads the campaign's player characters and world entities. It is
// best-effort: a lookup failure costs name accuracy, not the session, so it is
// logged and the job carries on with whatever loaded.
func (w *Worker) loadRoster(ctx context.Context, campaignID uuid.UUID, names map[string]string) campaignRoster {
	log := logging.FromContext(ctx, w.log)
	pcs, err := w.store.ListPCs(ctx, campaignID)
	if err != nil {
		log.Warn("load player characters for name roster", "campaign", campaignID, "err", err)
	}
	entities, err := w.store.ListAllWorldEntities(ctx, campaignID)
	if err != nil {
		log.Warn("load world entities for name roster", "campaign", campaignID, "err", err)
	}
	return buildRoster(pcs, entities, names)
}

// buildRoster formats the roster from already-loaded rows. names maps a Discord
// user ID to the display name used as that speaker's transcript label; it is
// what lets the notes say which character a speaker is playing.
func buildRoster(pcs []db.PlayerCharacter, entities []db.WorldEntity, names map[string]string) campaignRoster {
	var r campaignRoster
	var vocab []string
	for _, pc := range pcs {
		r.characters = append(r.characters, pcNotesLine(pc, names[pc.DiscordUserID]))
		vocab = append(vocab, pc.Name)
	}
	for _, e := range entities {
		r.entities = append(r.entities, entityNotesLine(e))
	}
	for _, kind := range vocabularyKinds {
		for _, e := range entities {
			if e.Kind == kind {
				vocab = append(vocab, e.Name)
			}
		}
	}
	r.vocabulary = prompts.TranscriptionVocabulary(vocab)
	return r
}

// pcNotesLine renders a PC as "Throk Bloodrend — Lv 15 Orc Barbarian, played by
// PlayerOne". player is the speaker label from this session's call; it is
// empty when that player was not in the call, and the clause is then omitted.
func pcNotesLine(pc db.PlayerCharacter, player string) string {
	var desc []string
	if pc.Level > 0 {
		desc = append(desc, fmt.Sprintf("Lv %d", pc.Level))
	}
	if s := strings.TrimSpace(strings.TrimSpace(pc.Race) + " " + strings.TrimSpace(pc.Class)); s != "" {
		desc = append(desc, s)
	}
	line := pc.Name
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

// entityNotesLine renders a world entity as "[npc] Zoldanox — <short
// description>" for the session-notes glossary.
func entityNotesLine(e db.WorldEntity) string {
	line := fmt.Sprintf("[%s] %s", e.Kind, e.Name)
	if d := strings.Join(strings.Fields(e.Description), " "); d != "" {
		line += " — " + discordfmt.Truncate(d, notesEntityDescRunes)
	}
	return line
}
