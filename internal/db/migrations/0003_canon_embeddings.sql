-- 0003_canon_embeddings.sql
--
-- Make /ask aware of curated campaign canon (world entities + player
-- characters), not just session transcripts. Runs AFTER 0001/0002 on every
-- startup; strictly additive and idempotent (the migration runner has no
-- applied-version tracking and re-executes each file under an advisory lock).
--
-- Vector embeddings over campaign canon for grounded /ask retrieval. One row per
-- source record (a world entity or a player character). The embedding dimension
-- is templated at migration time from LITELLM_EMBED_DIM (see internal/db.Migrate)
-- so it matches the same embedding model that powers session_embeddings.
--
-- Kept SEPARATE from session_embeddings because canon has no session_id (its FK
-- is NOT NULL there) and canon rows are keyed by their own source record, so a
-- single entity/PC maps to exactly one embedding row that can be upserted or
-- deleted as the record changes. /ask retrieves from both tables and merges by
-- distance.
CREATE TABLE IF NOT EXISTS canon_embeddings (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id  UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    -- source_kind: 'entity' (world_entities) | 'character' (player_characters).
    source_kind  TEXT NOT NULL,
    -- source_id is the world_entities.id or player_characters.id this row
    -- describes. No FK (it's polymorphic across two tables); the app deletes the
    -- embedding when the source is removed, and re-embeds on change. A campaign
    -- delete cascades via campaign_id.
    source_id    UUID NOT NULL,
    -- content is the human-readable text that was embedded (entity/PC rendered
    -- to prose), surfaced to /ask as a context excerpt.
    content      TEXT NOT NULL,
    embedding    vector(__EMBED_DIM__) NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One embedding row per source record; re-embedding upserts on this key.
    UNIQUE (source_kind, source_id)
);
CREATE INDEX IF NOT EXISTS idx_canon_embeddings_campaign
    ON canon_embeddings (campaign_id);
CREATE INDEX IF NOT EXISTS idx_canon_embeddings_vec
    ON canon_embeddings USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);
