-- 0002: per-user preferences.
--
-- Adds the ability for a user who shares multiple of the bot's guilds to pick
-- which guild's campaign their DMs operate against (via /dm-server). Without a
-- preference the gateway falls back to the sole shared guild (legacy behavior).
--
-- Idempotent like 0001 (IF NOT EXISTS), applied on startup under the migration
-- advisory lock.

CREATE TABLE IF NOT EXISTS user_prefs (
    user_id     TEXT PRIMARY KEY,            -- Discord user ID
    -- Selected guild for DM interactions. Not a FK to guilds(id): a user may
    -- select a guild before any guild row exists, and we don't want a delete of
    -- the guild row to fail on this reference — the gateway validates membership
    -- and allowlist at use time anyway.
    dm_guild_id TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
