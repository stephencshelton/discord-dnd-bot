package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// canonEmbeddingsAvailable reports whether the canon_embeddings table exists in
// the connected test database. It won't when the minimal-schema fallback was
// used (that path deliberately skips pgvector-dependent tables), so canon tests
// skip cleanly rather than fail.
func canonEmbeddingsAvailable(t *testing.T, s *Store) bool {
	t.Helper()
	var exists bool
	err := s.db.Pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name='canon_embeddings')`).
		Scan(&exists)
	return err == nil && exists
}

// TestCanonEmbeddingUpsertSearchDelete exercises the canon embedding lifecycle:
// upsert (create), re-upsert (replace, no duplicate), similarity search, and
// delete. Requires pgvector (skips under the minimal-schema fallback).
func TestCanonEmbeddingUpsertSearchDelete(t *testing.T) {
	store, campID := newTestStore(t)
	if !canonEmbeddingsAvailable(t, store) {
		t.Skip("canon_embeddings table not present (no pgvector); skipping")
	}
	ctx := context.Background()

	entityID := uuid.New()
	vec := make([]float32, 1536)
	for i := range vec {
		vec[i] = 0.01
	}

	// Upsert (create).
	if err := store.UpsertCanonEmbedding(ctx, campID, CanonSourceEntity, entityID, "NPC: Varek", vec); err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	// Re-upsert with new content must REPLACE, not duplicate (UNIQUE key).
	if err := store.UpsertCanonEmbedding(ctx, campID, CanonSourceEntity, entityID, "NPC: Varek (traitor)", vec); err != nil {
		t.Fatalf("upsert replace: %v", err)
	}

	got, err := store.SearchSimilarCanon(ctx, campID, vec, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 canon row (upsert should not duplicate), got %d", len(got))
	}
	if got[0].Content != "NPC: Varek (traitor)" {
		t.Errorf("content = %q, want the replaced text", got[0].Content)
	}

	// Empty content deletes the row (a record that lost all its text).
	if err := store.UpsertCanonEmbedding(ctx, campID, CanonSourceEntity, entityID, "   ", vec); err != nil {
		t.Fatalf("upsert empty: %v", err)
	}
	got, _ = store.SearchSimilarCanon(ctx, campID, vec, 5)
	if len(got) != 0 {
		t.Errorf("empty-content upsert should delete the row, still have %d", len(got))
	}

	// Explicit delete is idempotent.
	if err := store.DeleteCanonEmbedding(ctx, CanonSourceEntity, entityID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// TestHasEmbeddingsCountsCanon confirms /ask's "anything indexed?" guard sees
// canon embeddings, not just session transcripts.
func TestHasEmbeddingsCountsCanon(t *testing.T) {
	store, campID := newTestStore(t)
	if !canonEmbeddingsAvailable(t, store) {
		t.Skip("canon_embeddings table not present (no pgvector); skipping")
	}
	ctx := context.Background()

	has, err := store.HasEmbeddings(ctx, campID)
	if err != nil {
		t.Fatalf("has (empty): %v", err)
	}
	if has {
		t.Fatal("fresh campaign should have no embeddings")
	}

	vec := make([]float32, 1536)
	if err := store.UpsertCanonEmbedding(ctx, campID, CanonSourceCharacter, uuid.New(), "Player character: Ludo", vec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	has, err = store.HasEmbeddings(ctx, campID)
	if err != nil {
		t.Fatalf("has (canon): %v", err)
	}
	if !has {
		t.Error("HasEmbeddings should be true once canon is indexed")
	}
}
