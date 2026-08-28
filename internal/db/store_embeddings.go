package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// --- Session embeddings (grounded /ask retrieval) ---

// NoteChunk is one embedded passage of a session's notes plus its vector.
type NoteChunk struct {
	Content   string
	Embedding []float32
}

// RetrievedChunk is a passage returned by similarity search, with its cosine
// distance (0 = identical, 2 = opposite) and the session it came from.
type RetrievedChunk struct {
	SessionID uuid.UUID
	Content   string
	Distance  float64
}

// vectorLiteral renders a []float32 as a pgvector text literal: [0.1,0.2,...].
// pgvector accepts this form for both inserts and query parameters.
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*8 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// ReplaceSessionEmbeddings atomically replaces all embedding rows for a session
// with the provided chunks, making re-running the embedding job idempotent.
func (s *Store) ReplaceSessionEmbeddings(ctx context.Context, sessionID, campaignID uuid.UUID, chunks []NoteChunk) error {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM session_embeddings WHERE session_id=$1`, sessionID); err != nil {
		return fmt.Errorf("clear embeddings: %w", err)
	}
	for i, c := range chunks {
		if strings.TrimSpace(c.Content) == "" || len(c.Embedding) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO session_embeddings (session_id, campaign_id, chunk_index, content, embedding)
			 VALUES ($1,$2,$3,$4,$5)`,
			sessionID, campaignID, i, c.Content, vectorLiteral(c.Embedding)); err != nil {
			return fmt.Errorf("insert embedding %d: %w", i, err)
		}
	}
	return tx.Commit(ctx)
}

// SearchSimilarNotes returns the top-k note chunks in a campaign most similar to
// the query embedding, ordered by ascending cosine distance (closest first).
func (s *Store) SearchSimilarNotes(ctx context.Context, campaignID uuid.UUID, query []float32, k int) ([]RetrievedChunk, error) {
	if k < 1 {
		k = 5
	}
	if k > 20 {
		k = 20
	}
	if len(query) == 0 {
		return nil, nil
	}
	rows, err := s.db.Pool.Query(ctx,
		`SELECT session_id, content, embedding <=> $2 AS distance
		   FROM session_embeddings
		  WHERE campaign_id=$1
		  ORDER BY embedding <=> $2
		  LIMIT $3`,
		campaignID, vectorLiteral(query), k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetrievedChunk
	for rows.Next() {
		var rc RetrievedChunk
		if err := rows.Scan(&rc.SessionID, &rc.Content, &rc.Distance); err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// HasEmbeddings reports whether a campaign has any indexed note chunks yet, so
// callers can give a helpful message instead of an empty /ask answer.
func (s *Store) HasEmbeddings(ctx context.Context, campaignID uuid.UUID) (bool, error) {
	var exists bool
	err := s.db.Pool.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM session_embeddings WHERE campaign_id=$1
		    UNION ALL
		    SELECT 1 FROM canon_embeddings WHERE campaign_id=$1
		 )`, campaignID).
		Scan(&exists)
	if err != nil && err != pgx.ErrNoRows {
		return false, err
	}
	return exists, nil
}

// --- Canon embeddings (world entities + player characters, for /ask) ---

// CanonSourceKind distinguishes what a canon embedding describes.
const (
	CanonSourceEntity    = "entity"
	CanonSourceCharacter = "character"
)

// UpsertCanonEmbedding stores (or replaces) the embedding for a single canon
// record (world entity or player character), keyed by (source_kind, source_id)
// so re-embedding an updated record replaces its one row rather than
// duplicating. An empty vector or content is treated as a delete so a record
// that loses all its text stops polluting /ask.
func (s *Store) UpsertCanonEmbedding(ctx context.Context, campaignID uuid.UUID, sourceKind string, sourceID uuid.UUID, content string, embedding []float32) error {
	if strings.TrimSpace(content) == "" || len(embedding) == 0 {
		return s.DeleteCanonEmbedding(ctx, sourceKind, sourceID)
	}
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO canon_embeddings (campaign_id, source_kind, source_id, content, embedding, updated_at)
		 VALUES ($1,$2,$3,$4,$5, now())
		 ON CONFLICT (source_kind, source_id) DO UPDATE
		   SET campaign_id = EXCLUDED.campaign_id,
		       content     = EXCLUDED.content,
		       embedding   = EXCLUDED.embedding,
		       updated_at  = now()`,
		campaignID, sourceKind, sourceID, content, vectorLiteral(embedding))
	return err
}

// DeleteCanonEmbedding removes a canon record's embedding (e.g. when the entity
// or character is deleted). Idempotent.
func (s *Store) DeleteCanonEmbedding(ctx context.Context, sourceKind string, sourceID uuid.UUID) error {
	_, err := s.db.Pool.Exec(ctx,
		`DELETE FROM canon_embeddings WHERE source_kind=$1 AND source_id=$2`, sourceKind, sourceID)
	return err
}

// SearchSimilarCanon returns the top-k canon passages (entities/PCs) in a
// campaign most similar to the query embedding, closest first.
func (s *Store) SearchSimilarCanon(ctx context.Context, campaignID uuid.UUID, query []float32, k int) ([]RetrievedChunk, error) {
	if k < 1 {
		k = 5
	}
	if k > 20 {
		k = 20
	}
	if len(query) == 0 {
		return nil, nil
	}
	rows, err := s.db.Pool.Query(ctx,
		`SELECT content, embedding <=> $2 AS distance
		   FROM canon_embeddings
		  WHERE campaign_id=$1
		  ORDER BY embedding <=> $2
		  LIMIT $3`,
		campaignID, vectorLiteral(query), k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetrievedChunk
	for rows.Next() {
		var rc RetrievedChunk
		if err := rows.Scan(&rc.Content, &rc.Distance); err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// CompletedSessionNote is a completed session's text, used for backfilling
// embeddings via /reindex. Both the full transcript and the summarized notes
// are returned; /ask indexing uses the transcript for maximum recall.
type CompletedSessionNote struct {
	ID         uuid.UUID
	Notes      string
	Transcript string
}

// CompletedSessionsWithNotes returns completed sessions in a campaign that have
// a non-empty transcript, oldest first. Used to (re)build /ask embeddings from
// the full transcript.
func (s *Store) CompletedSessionsWithNotes(ctx context.Context, campaignID uuid.UUID) ([]CompletedSessionNote, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, COALESCE(notes,''), COALESCE(transcript,'') FROM sessions
		  WHERE campaign_id=$1 AND status='complete' AND COALESCE(transcript,'') <> ''
		  ORDER BY started_at ASC`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompletedSessionNote
	for rows.Next() {
		var c CompletedSessionNote
		if err := rows.Scan(&c.ID, &c.Notes, &c.Transcript); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
