-- discord-dnd-bot schema (PostgreSQL).
-- Applied automatically on startup by internal/db.Migrate under an advisory
-- lock. Idempotent (IF NOT EXISTS everywhere), so re-running is safe.
--
-- This is a single consolidated migration: the bot is not yet deployed, so
-- there is no migration history to preserve. Keep it that way until the first
-- production deploy; after that, add new numbered files rather than editing this.

-- Required extensions.
CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_uuid()
CREATE EXTENSION IF NOT EXISTS vector;    -- pgvector, powers grounded /ask (RAG)

CREATE TABLE IF NOT EXISTS guilds (
    id          TEXT PRIMARY KEY,           -- Discord guild ID
    notes_channel_id TEXT,                   -- where session notes are posted (/notes-channel)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS campaigns (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    guild_id    TEXT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    system      TEXT,                        -- game system, e.g. "D&D 5e"
    premise     TEXT,
    archived    BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_campaigns_guild ON campaigns(guild_id) WHERE NOT archived;

-- The single active campaign per guild used for recording & worldbuilding.
CREATE TABLE IF NOT EXISTS active_campaign (
    guild_id    TEXT PRIMARY KEY REFERENCES guilds(id) ON DELETE CASCADE,
    campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS player_characters (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,           -- owner, for dialogue attribution
    name        TEXT NOT NULL,
    class       TEXT,
    race        TEXT,
    level       INT NOT NULL DEFAULT 1,
    notes       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_pc_campaign ON player_characters(campaign_id);

-- Generic worldbuilding entities: npc | location | faction | quest.
CREATE TABLE IF NOT EXISTS world_entities (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,               -- npc|location|faction|quest
    name        TEXT NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_world_campaign_kind ON world_entities(campaign_id, kind);

CREATE TABLE IF NOT EXISTS sessions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    guild_id    TEXT NOT NULL,
    voice_channel_id TEXT,
    status      TEXT NOT NULL DEFAULT 'recording', -- recording|processing|complete|failed
    audio_key   TEXT,                        -- object-storage key of raw mix
    -- chunk_prefix is the object-storage prefix under which the live recorder
    -- periodically checkpoints raw PCM chunks (chunk-000001.pcm, ...). It lets a
    -- crashed/restarted session be reassembled from S3 so a pod death only loses
    -- roughly the downtime window rather than the whole recording.
    chunk_prefix TEXT,
    transcript  TEXT,
    notes       TEXT,                        -- final AI-generated writeup
    duration_seconds INT NOT NULL DEFAULT 0,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- heartbeat_at is bumped by the live recorder on each checkpoint; a reaper
    -- finalizes 'recording' sessions whose heartbeat has gone stale (the owning
    -- pod died and nothing resumed them).
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_sessions_campaign ON sessions(campaign_id);

-- At most one recording session per guild at a time. Enforces the "one active
-- session" rule in the database rather than relying on an app-level check
-- (which is racy under concurrent /session start).
CREATE UNIQUE INDEX IF NOT EXISTS uq_sessions_one_recording
    ON sessions(guild_id) WHERE status = 'recording';

-- Full-text search over completed session memory. The generated tsvector keeps
-- the write path simple; the GIN index keeps campaign search fast as
-- transcripts accumulate.
ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS search_vector tsvector
    GENERATED ALWAYS AS (
        to_tsvector('simple', coalesce(transcript, '') || ' ' || coalesce(notes, ''))
    ) STORED;
CREATE INDEX IF NOT EXISTS idx_sessions_search
    ON sessions USING GIN (search_vector);

-- Records which Discord users spoke during a recorded session. Gives a factual
-- "who was in the call" answer and lets session notes attribute context to real
-- participants instead of guessing from the transcript.
CREATE TABLE IF NOT EXISTS session_participants (
    session_id    UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    user_id       TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    first_spoke_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_spoke_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_session_participants_session
    ON session_participants (session_id);

-- Vector embeddings over completed session notes for grounded /ask retrieval
-- (RAG). One row per chunk. The embedding dimension is templated at migration
-- time from LITELLM_EMBED_DIM (see internal/db.Migrate) so the schema matches
-- whichever embedding model LiteLLM is configured to use.
CREATE TABLE IF NOT EXISTS session_embeddings (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id  UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    chunk_index INT  NOT NULL,
    content     TEXT NOT NULL,
    embedding   vector(__EMBED_DIM__) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, chunk_index)
);
CREATE INDEX IF NOT EXISTS idx_session_embeddings_campaign
    ON session_embeddings (campaign_id);
CREATE INDEX IF NOT EXISTS idx_session_embeddings_vec
    ON session_embeddings USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);

CREATE TABLE IF NOT EXISTS reminders (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    guild_id    TEXT NOT NULL,
    channel_id  TEXT NOT NULL,
    schedule    TEXT NOT NULL,               -- natural-language schedule as entered
    next_run    TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_reminders_next ON reminders(next_run);

CREATE TABLE IF NOT EXISTS feedback (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    guild_id    TEXT,
    discord_user_id TEXT,
    body        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-user preferences. Lets a user who shares multiple of the bot's guilds
-- pick which guild's campaign their DMs operate against (via /dm-server).
-- Without a preference the gateway falls back to the sole shared guild.
CREATE TABLE IF NOT EXISTS user_prefs (
    user_id     TEXT PRIMARY KEY,           -- Discord user ID
    -- Selected guild for DM interactions. Not a FK to guilds(id): a user may
    -- select a guild before any guild row exists, and the gateway validates
    -- membership + allowlist at use time anyway.
    dm_guild_id TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

