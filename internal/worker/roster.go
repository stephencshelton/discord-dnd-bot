package worker

import (
	"context"

	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/prompts"
)

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
		r.characters = append(r.characters, pc.RosterLine(names[pc.DiscordUserID]))
		vocab = append(vocab, pc.Name)
	}
	for _, e := range entities {
		r.entities = append(r.entities, e.RosterLine())
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
