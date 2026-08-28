package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/extract"
	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/prompts"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
)

// handleExtractState runs the post-session campaign-state extraction: it asks
// the configured state model to PROPOSE (never apply) discrete, evidence-backed
// world-state changes from the session's transcript + notes, validates the
// model's strict-JSON output, and persists the survivors as pending proposals
// for the DM to review via /review-session.
//
// This runs as its own job, decoupled from transcription, precisely so that a
// failure here can be retried independently and NEVER marks the session failed
// — the transcript and notes are already saved. It is idempotent-friendly:
// re-running clears the session's prior pending (un-reviewed) proposals and
// re-derives them, so a reprocess doesn't pile up duplicates, while already
// approved/rejected proposals are preserved.
func (w *Worker) handleExtractState(ctx context.Context, raw json.RawMessage) error {
	p, err := unmarshal[queue.ExtractStatePayload](raw)
	if err != nil {
		return queue.Permanent(fmt.Errorf("decode payload: %w", err))
	}
	sessionID, err := uuid.Parse(p.SessionID)
	if err != nil {
		return queue.Permanent(fmt.Errorf("parse session id: %w", err))
	}

	sess, err := w.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	// Nothing to extract from without both a transcript and notes. This is a
	// permanent condition for this job (a retry can't conjure them); the session
	// itself remains valid/complete regardless.
	if strings.TrimSpace(sess.Transcript) == "" || strings.TrimSpace(sess.Notes) == "" {
		return queue.Permanent(fmt.Errorf("session %s has no transcript/notes to extract from", sessionID))
	}

	camp, err := w.store.GetCampaign(ctx, sess.CampaignID)
	if err != nil {
		return fmt.Errorf("load campaign: %w", err)
	}

	// Gather existing world entities (for update-vs-create resolution / dedup)
	// and player characters (context so the model attributes facts correctly).
	entities, err := w.store.ListAllWorldEntities(ctx, camp.ID)
	if err != nil {
		return fmt.Errorf("list world entities: %w", err)
	}
	existing := make([]extract.ExistingEntity, 0, len(entities))
	entityLines := make([]string, 0, len(entities))
	for _, e := range entities {
		existing = append(existing, extract.ExistingEntity{ID: e.ID, Kind: e.Kind, Name: e.Name})
		line := fmt.Sprintf("[%s] %s (id: %s)", e.Kind, e.Name, e.ID)
		if e.Description != "" {
			line += " — " + e.Description
		}
		entityLines = append(entityLines, line)
	}

	var charLines []string
	if pcs, perr := w.store.ListPCs(ctx, camp.ID); perr == nil {
		for _, pc := range pcs {
			line := pc.Name
			if pc.Class != "" || pc.Race != "" {
				line += fmt.Sprintf(" (%s %s)", strings.TrimSpace(pc.Race), strings.TrimSpace(pc.Class))
			}
			charLines = append(charLines, line)
		}
	}

	// Ask the model for strict JSON. Use the state route (falls back to chat).
	rawJSON, err := w.ai.Chat(ctx, w.cfg.LiteLLM.State(), []litellm.Message{
		{Role: "system", Content: prompts.StateExtractionSystem},
		{Role: "user", Content: prompts.StateExtractionUser(camp.Name, camp.System, camp.Premise, charLines, entityLines, sess.Notes, sess.Transcript)},
	}, 3000)
	if err != nil {
		metrics.AIRequests.WithLabelValues("extract", "error").Inc()
		return fmt.Errorf("state extraction chat: %w", err)
	}

	// Validate + normalize the model output. A parse failure is PERMANENT for
	// this job (retrying the same saved transcript yields the same bad output);
	// it's logged + metered but must not fail the session.
	sid := sessionID
	proposals, err := extract.Parse(rawJSON, camp.ID, &sid, existing)
	if err != nil {
		metrics.AIRequests.WithLabelValues("extract", "error").Inc()
		w.log.Warn("state extraction produced unparseable output",
			"session", sessionID, "err", err)
		return queue.Permanent(fmt.Errorf("parse extraction: %w", err))
	}
	metrics.AIRequests.WithLabelValues("extract", "ok").Inc()

	// Replace this session's prior PENDING proposals so a reprocess is idempotent
	// (approved/rejected ones are preserved by the store method).
	if err := w.store.ReplacePendingSessionProposals(ctx, camp.ID, sessionID, proposals); err != nil {
		return fmt.Errorf("persist proposals: %w", err)
	}
	metrics.StateProposalsCreated.Add(float64(len(proposals)))

	w.log.Info("state extraction complete",
		"session", sessionID, "campaign", camp.ID, "proposals", len(proposals))

	// Optionally nudge the notes channel that proposals are ready to review.
	if p.Notify && len(proposals) > 0 {
		ch := w.notesChannel(ctx, p.GuildID, sess.VoiceChannelID)
		w.notify(p.GuildID, ch, fmt.Sprintf(
			"🧭 I found **%d** proposed update(s) to your campaign world from the last session. A DM can review them with `/review-session` (nothing changes until approved).",
			len(proposals)))
	}
	return nil
}
