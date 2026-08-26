package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// --- User preferences ---

// GetDMGuildID returns the guild ID a user selected for DM interactions, or ""
// (with no error) if the user has no preference set.
func (s *Store) GetDMGuildID(ctx context.Context, userID string) (string, error) {
	var guildID string
	err := s.db.Pool.QueryRow(ctx,
		`SELECT COALESCE(dm_guild_id,'') FROM user_prefs WHERE user_id=$1`, userID).
		Scan(&guildID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return guildID, err
}

// SetDMGuildID upserts a user's selected DM guild. Passing "" clears it.
func (s *Store) SetDMGuildID(ctx context.Context, userID, guildID string) error {
	var val any
	if guildID != "" {
		val = guildID
	} // else nil -> stored as NULL
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO user_prefs (user_id, dm_guild_id) VALUES ($1,$2)
		 ON CONFLICT (user_id) DO UPDATE SET dm_guild_id=$2, updated_at=now()`,
		userID, val)
	return err
}
