package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
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
//
// lastAttempt is true when the worker will not retry this job again; it gates the
// user-visible "extraction didn't work" message so an intermittent blip that
// succeeds on retry stays invisible, while a real dead end is never silent (a
// silent dead end is indistinguishable from /review-session being broken).
func (w *Worker) handleExtractState(ctx context.Context, raw json.RawMessage, lastAttempt bool) error {
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
	var existingChars []extract.ExistingCharacter
	if pcs, perr := w.store.ListPCs(ctx, camp.ID); perr == nil {
		for _, pc := range pcs {
			existingChars = append(existingChars, extract.ExistingCharacter{ID: pc.ID, Name: pc.Name})
			line := pc.Name
			if pc.Class != "" || pc.Race != "" {
				line += fmt.Sprintf(" (%s %s)", strings.TrimSpace(pc.Race), strings.TrimSpace(pc.Class))
			}
			charLines = append(charLines, line)
		}
	}

	// Ask the model for strict JSON. Use the state route (falls back to chat).
	// chatComplete (not Chat) because the reply is JSON: if the model runs out of
	// output tokens the JSON is cut mid-structure and NOTHING parses, which threw
	// away every proposal from long sessions.
	rawJSON, err := w.chatComplete(ctx, "extract", w.cfg.LiteLLM.State(), []litellm.Message{
		{Role: "system", Content: prompts.StateExtractionSystem},
		{Role: "user", Content: prompts.StateExtractionUser(camp.Name, camp.System, camp.Premise, charLines, entityLines, sess.Notes, sess.Transcript)},
	}, w.cfg.LiteLLM.StateTokens())
	if err != nil {
		metrics.AIRequests.WithLabelValues("extract", "error").Inc()
		if lastAttempt {
			w.notifyExtractionFailed(ctx, p, sess)
		}
		return fmt.Errorf("state extraction chat: %w", err)
	}

	// Validate + normalize the model output. A parse failure is PERMANENT for
	// this job (retrying the same saved transcript yields the same bad output);
	// it's logged + metered but must not fail the session. Tell the channel too:
	// staying silent here is what made a failed extraction look like the whole
	// /review-session feature was broken.
	sid := sessionID
	proposals, err := extract.Parse(rawJSON, camp.ID, &sid, existing, existingChars)
	if err != nil {
		metrics.AIRequests.WithLabelValues("extract", "error").Inc()
		w.log.Warn("state extraction produced unparseable output",
			"session", sessionID, "err", err, "raw_chars", len(rawJSON))
		w.notifyExtractionFailed(ctx, p, sess)
		return queue.Permanent(fmt.Errorf("parse extraction: %w", err))
	}
	metrics.AIRequests.WithLabelValues("extract", "ok").Inc()

	// Noteworthiness gating so a DM isn't bugged with trivia:
	//   1) Confidence floor — cheap, deterministic drop of low-confidence items.
	//   2) Critic pass — a second AI review that keeps only genuinely significant
	//      proposals. Fail-open: a critic error/parse-failure leaves the
	//      confidence-filtered set intact rather than dropping everything.
	rawCount := len(proposals)
	proposals = extract.FilterByConfidence(proposals, w.cfg.Extraction.MinConfidence)
	if w.cfg.Extraction.Critic && len(proposals) > 0 {
		proposals = w.criticFilter(ctx, proposals)
	}
	if dropped := rawCount - len(proposals); dropped > 0 {
		w.log.Info("state extraction filtered low-value proposals",
			"session", sessionID, "dropped", dropped, "kept", len(proposals))
	}

	// Replace this session's prior PENDING proposals so a reprocess is idempotent
	// (approved/rejected ones are preserved by the store method).
	if err := w.store.ReplacePendingSessionProposals(ctx, camp.ID, sessionID, proposals); err != nil {
		return fmt.Errorf("persist proposals: %w", err)
	}
	metrics.StateProposalsCreated.Add(float64(len(proposals)))

	w.log.Info("state extraction complete",
		"session", sessionID, "campaign", camp.ID, "proposals", len(proposals))

	// Optionally nudge the notes channel. Zero proposals gets a message too:
	// silence is indistinguishable from a broken feature, and after a long
	// session "I found nothing" is itself surprising information worth showing.
	if p.Notify {
		ch := w.notesChannel(ctx, p.GuildID, sess.VoiceChannelID)
		msg := "🧭 I didn't find any campaign-world updates worth proposing from that session. Nothing to review."
		if len(proposals) > 0 {
			msg = fmt.Sprintf(
				"🧭 I found **%d** proposed update(s) to your campaign world from the last session. A DM can review them with `/review-session` (nothing changes until approved).",
				len(proposals))
		}
		w.notify(p.GuildID, ch, msg)
	}
	return nil
}

// notifyExtractionFailed tells the notes channel that the world-state extraction
// step gave up, so a DM knows there's nothing to review because the step failed
// — not because /review-session is broken. Best-effort and gated on the job's
// Notify flag, exactly like the success message.
func (w *Worker) notifyExtractionFailed(ctx context.Context, p queue.ExtractStatePayload, sess *db.Session) {
	if !p.Notify {
		return
	}
	ch := w.notesChannel(ctx, p.GuildID, sess.VoiceChannelID)
	w.notify(p.GuildID, ch,
		"⚠️ I couldn't work out any campaign-world updates from that session — the extraction step failed, so `/review-session` has nothing to show. "+
			"Your transcript and notes are saved; a DM can record anything important with `/world add`.")
}

// criticFilter runs the second-pass "editor" review: it summarizes each
// candidate proposal, asks the recap model which are genuinely noteworthy, and
// returns only those. It FAILS OPEN — any error (AI call or parse) returns the
// input unchanged, since it's better to surface a borderline proposal for the DM
// than to silently drop everything on a critic hiccup.
//
// Note the failure mode this shares with extraction: the critic replies in JSON,
// so a reply cut off at the token limit doesn't error — it fails open and every
// candidate survives, silently disabling the filtering. chatComplete repairs the
// truncation instead, and the budget is configurable.
func (w *Worker) criticFilter(ctx context.Context, proposals []db.StateProposal) []db.StateProposal {
	candidates := make([]string, len(proposals))
	for i, p := range proposals {
		candidates[i] = extract.CriticCandidate(p)
	}
	raw, err := w.chatComplete(ctx, "extract_critic", w.cfg.LiteLLM.Recap(), []litellm.Message{
		{Role: "system", Content: prompts.CriticSystem},
		{Role: "user", Content: prompts.CriticUser(candidates)},
	}, w.cfg.LiteLLM.CriticTokens())
	if err != nil {
		metrics.AIRequests.WithLabelValues("extract_critic", "error").Inc()
		w.log.Warn("state extraction critic pass failed; keeping confidence-filtered set", "err", err)
		return proposals // fail-open
	}
	metrics.AIRequests.WithLabelValues("extract_critic", "ok").Inc()
	return extract.ApplyCriticKeep(proposals, raw)
}
