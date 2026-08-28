-- 0002_state_proposals.sql
--
-- Automatic campaign-state extraction & DM review (Feature: /review-session).
--
-- This runs AFTER 0001_init.sql on every startup. Unlike 0001 (which was a
-- pre-launch consolidated schema), this is a post-launch migration against a
-- live database, so it must be strictly additive and idempotent — every
-- statement uses IF NOT EXISTS / ADD COLUMN IF NOT EXISTS so re-applying it on
-- an already-migrated database is a harmless no-op (the migration runner has no
-- applied-version tracking; it re-executes each file under an advisory lock).

-- 1) Optional structured metadata on world entities.
--    Holds kind-specific fields beyond the freeform description (e.g. a quest's
--    status, an NPC's role/location, a faction's goals). JSONB so new fields can
--    be added without a schema change; existing rows default to an empty object.
--    This is where approved AI state proposals deposit their structured patch data.
ALTER TABLE world_entities
    ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Case-insensitive uniqueness per (campaign, kind, name) prevents duplicate
-- entities when an approved proposal races or is retried. lower(name) is the
-- canonical key used for dedupe both here and in the app.
--
-- NOTE: if the live table already contains case-insensitive duplicates for a
-- (campaign_id, kind) pair, creating this unique index will fail. That should be
-- vanishingly rare (entities are hand-authored today), but if it happens,
-- deduplicate those rows before this migration can complete.
CREATE UNIQUE INDEX IF NOT EXISTS uq_world_entities_campaign_kind_name
    ON world_entities(campaign_id, kind, lower(name));

-- 2) Session-derived campaign-state proposals. After a session's transcript and
--    notes are generated, an AI extraction step proposes discrete, evidence-backed
--    changes to the persistent world (new/updated NPCs, locations, factions,
--    quests, relationships, facts, story hooks). These are NOT canon: they stay
--    pending until a DM approves them via /review-session, at which point the
--    change is atomically applied to world_entities and the row marked approved.
--    Rejecting leaves canon untouched. AI never writes world_entities directly.
CREATE TABLE IF NOT EXISTS state_proposals (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id   UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    -- The session this proposal was derived from. Nullable so manually-authored
    -- proposals (via /remember) that aren't tied to a recording are allowed.
    session_id    UUID REFERENCES sessions(id) ON DELETE SET NULL,
    -- action: create_entity | update_entity. (Relationships/facts/hooks are
    -- modeled as entities of the appropriate kind, keeping the apply path simple.)
    action        TEXT NOT NULL,
    -- kind: the world_entities kind this proposal concerns
    -- (npc|location|faction|quest). Matches world_entities.kind.
    entity_kind   TEXT NOT NULL,
    -- For update_entity, the existing world entity being changed (nullable for
    -- create_entity). ON DELETE SET NULL so deleting an entity doesn't orphan
    -- the FK; the apply path re-resolves by name if this is null.
    entity_id     UUID REFERENCES world_entities(id) ON DELETE SET NULL,
    entity_name   TEXT NOT NULL,
    -- patch is the structured proposed data (JSONB): a "description" string
    -- and/or arbitrary metadata fields to merge into world_entities.metadata.
    patch         JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- explanation is a short human-readable summary of the change.
    explanation   TEXT NOT NULL DEFAULT '',
    -- evidence is the supporting quote/summary from the transcript that justifies
    -- the proposal, so the DM can judge it without re-reading the whole session.
    evidence      TEXT NOT NULL DEFAULT '',
    -- confidence in [0,1] as reported by the model; low-confidence proposals can
    -- be surfaced with a warning but are never auto-applied.
    confidence    DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- status: pending | approved | rejected.
    status        TEXT NOT NULL DEFAULT 'pending',
    reviewed_by   TEXT,                        -- Discord user ID of the reviewing DM
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_state_proposals_campaign_status
    ON state_proposals(campaign_id, status);
CREATE INDEX IF NOT EXISTS idx_state_proposals_session
    ON state_proposals(session_id);
