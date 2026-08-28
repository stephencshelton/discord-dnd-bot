package worker

import (
	"context"
	"encoding/json"
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
	if len(sessions) == 0 {
		w.notify(p.GuildID, p.ChannelID, "🔎 No completed sessions with transcripts to index yet.")
		return nil
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
	msg := fmt.Sprintf("🔎 Reindexed campaign memory: %d session(s) indexed for `/ask`.", indexed)
	if failed > 0 {
		msg += fmt.Sprintf(" %d failed — check logs.", failed)
	}
	w.notify(p.GuildID, p.ChannelID, msg)
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
