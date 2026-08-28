package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
)

// maxChunkRunes bounds each note passage sent to the embedding model — generous
// enough to keep most sections intact while staying under model input limits.
const maxChunkRunes = 1200

// embedSessionNotes splits a session's text into passages, embeds them via the
// configured LiteLLM route, and stores the vectors for /ask retrieval. Replaces
// existing rows so re-processing is idempotent. The `text` is the full session
// transcript (not the summarized notes) so /ask can retrieve fine-grained
// details the summary may have omitted.
func (w *Worker) embedSessionNotes(ctx context.Context, sessionID, campaignID uuid.UUID, text string) error {
	passages := chunkNotes(text, maxChunkRunes)
	if len(passages) == 0 {
		return nil
	}
	vecs, err := w.ai.Embed(ctx, w.cfg.LiteLLM.EmbedModel, passages)
	if err != nil {
		return err
	}
	chunks := make([]db.NoteChunk, 0, len(passages))
	for i, p := range passages {
		if i >= len(vecs) {
			break
		}
		chunks = append(chunks, db.NoteChunk{Content: p, Embedding: vecs[i]})
	}
	return w.store.ReplaceSessionEmbeddings(ctx, sessionID, campaignID, chunks)
}

// handleReindexCampaign (re)embeds every completed session's notes in a
// campaign — e.g. sessions recorded before embeddings were enabled, or after a
// model change. Idempotent per session (ReplaceSessionEmbeddings clears and
// rewrites).
func (w *Worker) handleReindexCampaign(ctx context.Context, raw json.RawMessage) error {
	p, err := unmarshal[queue.ReindexCampaignPayload](raw)
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	campaignID, err := uuid.Parse(p.CampaignID)
	if err != nil {
		return fmt.Errorf("parse campaign id: %w", err)
	}
	sessions, err := w.store.CompletedSessionsWithNotes(ctx, campaignID)
	if err != nil {
		return fmt.Errorf("list completed sessions: %w", err)
	}
	var indexed, failed int
	for _, sess := range sessions {
		if err := w.embedSessionNotes(ctx, sess.ID, campaignID, sess.Transcript); err != nil {
			failed++
			metrics.AIRequests.WithLabelValues("embed", "error").Inc()
			w.log.Warn("reindex session", "session", sess.ID, "err", err)
			continue
		}
		indexed++
		metrics.AIRequests.WithLabelValues("embed", "ok").Inc()
	}

	// Also (re)embed curated canon — world entities and player characters — so
	// /ask can surface DM-authored and AI-approved facts alongside transcripts.
	canonIndexed, canonFailed := w.reindexCanon(ctx, campaignID)

	if indexed == 0 && canonIndexed == 0 && failed == 0 && canonFailed == 0 {
		w.notify(p.GuildID, p.ChannelID, "🔎 Nothing to index yet — record a session or add world/character entries, then try again.")
		return nil
	}

	msg := fmt.Sprintf("🔎 Reindexed campaign memory: %d session(s) and %d canon entr(ies) indexed for `/ask`.", indexed, canonIndexed)
	if failed > 0 || canonFailed > 0 {
		msg += fmt.Sprintf(" %d failed — check logs.", failed+canonFailed)
	}
	w.notify(p.GuildID, p.ChannelID, msg)
	return nil
}

// reindexCanon (re)embeds every world entity and player character in a campaign.
// Returns how many were indexed and how many failed. Failures are logged and
// counted but never abort the backfill.
func (w *Worker) reindexCanon(ctx context.Context, campaignID uuid.UUID) (indexed, failed int) {
	entities, err := w.store.ListAllWorldEntities(ctx, campaignID)
	if err != nil {
		w.log.Warn("reindex canon: list entities", "campaign", campaignID, "err", err)
	}
	for _, ent := range entities {
		if err := w.embedCanonRecord(ctx, campaignID, db.CanonSourceEntity, ent.ID, ent.CanonText()); err != nil {
			failed++
			metrics.AIRequests.WithLabelValues("embed", "error").Inc()
			w.log.Warn("reindex canon: entity", "entity", ent.ID, "err", err)
			continue
		}
		indexed++
		metrics.AIRequests.WithLabelValues("embed", "ok").Inc()
	}

	pcs, err := w.store.ListPCs(ctx, campaignID)
	if err != nil {
		w.log.Warn("reindex canon: list characters", "campaign", campaignID, "err", err)
	}
	for _, pc := range pcs {
		if err := w.embedCanonRecord(ctx, campaignID, db.CanonSourceCharacter, pc.ID, pc.CanonText()); err != nil {
			failed++
			metrics.AIRequests.WithLabelValues("embed", "error").Inc()
			w.log.Warn("reindex canon: character", "character", pc.ID, "err", err)
			continue
		}
		indexed++
		metrics.AIRequests.WithLabelValues("embed", "ok").Inc()
	}
	return indexed, failed
}

// embedCanonRecord embeds a single canon record (world entity or player
// character) and upserts its vector for /ask retrieval. Idempotent per record.
func (w *Worker) embedCanonRecord(ctx context.Context, campaignID uuid.UUID, sourceKind string, sourceID uuid.UUID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		// Nothing meaningful to index — ensure any stale row is removed.
		return w.store.DeleteCanonEmbedding(ctx, sourceKind, sourceID)
	}
	vecs, err := w.ai.Embed(ctx, w.cfg.LiteLLM.EmbedModel, []string{text})
	if err != nil {
		return err
	}
	if len(vecs) == 0 {
		return nil
	}
	return w.store.UpsertCanonEmbedding(ctx, campaignID, sourceKind, sourceID, text, vecs[0])
}

// handleEmbedCanon (re)embeds a single canon record so grounded /ask retrieval
// can surface curated campaign facts (NPCs, locations, factions, quests, player
// characters) alongside session transcripts. The record's current text is
// rendered via its CanonText() method; a record that no longer exists (deleted
// between enqueue and processing) has its embedding removed instead.
func (w *Worker) handleEmbedCanon(ctx context.Context, raw json.RawMessage) error {
	p, err := unmarshal[queue.EmbedCanonPayload](raw)
	if err != nil {
		return queue.Permanent(fmt.Errorf("decode payload: %w", err))
	}
	campaignID, err := uuid.Parse(p.CampaignID)
	if err != nil {
		return queue.Permanent(fmt.Errorf("parse campaign id: %w", err))
	}
	sourceID, err := uuid.Parse(p.SourceID)
	if err != nil {
		return queue.Permanent(fmt.Errorf("parse source id: %w", err))
	}

	var text string
	switch p.SourceKind {
	case db.CanonSourceEntity:
		e, gerr := w.store.GetWorldEntityByID(ctx, sourceID)
		if errors.Is(gerr, db.ErrNotFound) {
			return w.store.DeleteCanonEmbedding(ctx, p.SourceKind, sourceID)
		}
		if gerr != nil {
			return fmt.Errorf("load entity: %w", gerr)
		}
		text = e.CanonText()
	case db.CanonSourceCharacter:
		pc, gerr := w.store.GetPCByID(ctx, sourceID)
		if errors.Is(gerr, db.ErrNotFound) {
			return w.store.DeleteCanonEmbedding(ctx, p.SourceKind, sourceID)
		}
		if gerr != nil {
			return fmt.Errorf("load character: %w", gerr)
		}
		text = pc.CanonText()
	default:
		return queue.Permanent(fmt.Errorf("unknown canon source kind %q", p.SourceKind))
	}

	if err := w.embedCanonRecord(ctx, campaignID, p.SourceKind, sourceID, text); err != nil {
		metrics.AIRequests.WithLabelValues("embed", "error").Inc()
		return fmt.Errorf("embed canon: %w", err)
	}
	metrics.AIRequests.WithLabelValues("embed", "ok").Inc()
	return nil
}

// chunkNotes splits markdown notes into passages no longer than maxRunes,
// preferring to break on blank lines (section/paragraph boundaries) so a chunk
// stays semantically coherent. Oversized paragraphs are hard-split.
func chunkNotes(notes string, maxRunes int) []string {
	notes = strings.TrimSpace(notes)
	if notes == "" {
		return nil
	}
	if maxRunes <= 0 {
		maxRunes = maxChunkRunes
	}
	paragraphs := strings.Split(notes, "\n\n")
	var (
		out     []string
		current strings.Builder
	)
	flush := func() {
		if current.Len() > 0 {
			out = append(out, strings.TrimSpace(current.String()))
			current.Reset()
		}
	}
	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		// A single paragraph larger than the limit is hard-split by runes.
		if len([]rune(para)) > maxRunes {
			flush()
			for _, piece := range chunkString(para, maxRunes) {
				out = append(out, strings.TrimSpace(piece))
			}
			continue
		}
		// Flush first if appending would overflow the current chunk.
		if current.Len() > 0 && len([]rune(current.String()))+len([]rune(para))+2 > maxRunes {
			flush()
		}
		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
	}
	flush()
	return out
}
