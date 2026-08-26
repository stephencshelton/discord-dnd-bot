package db

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a lookup matches no rows.
var ErrNotFound = errors.New("not found")

// Store exposes typed data access over the pool. All services share it.
type Store struct{ db *DB }

// NewStore wraps a DB in a Store.
func NewStore(d *DB) *Store { return &Store{db: d} }

// --- Guilds ---

// EnsureGuild inserts a guild row if missing and returns the current record.
func (s *Store) EnsureGuild(ctx context.Context, id string) (*Guild, error) {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO guilds (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`, id)
	if err != nil {
		return nil, err
	}
	return s.GetGuild(ctx, id)
}

// GetGuild fetches a guild by ID.
func (s *Store) GetGuild(ctx context.Context, id string) (*Guild, error) {
	var g Guild
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, COALESCE(notes_channel_id,''), created_at, updated_at
		 FROM guilds WHERE id=$1`, id).
		Scan(&g.ID, &g.NotesChannelID, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &g, err
}

// SetNotesChannel records the channel where session notes are posted (/setnotes).
func (s *Store) SetNotesChannel(ctx context.Context, guildID, channelID string) error {
	_, err := s.db.Pool.Exec(ctx,
		`UPDATE guilds SET notes_channel_id=$2, updated_at=now() WHERE id=$1`, guildID, channelID)
	return err
}

// --- Campaigns ---

// CreateCampaign inserts a campaign and returns it.
func (s *Store) CreateCampaign(ctx context.Context, guildID, name, system, premise string) (*Campaign, error) {
	var c Campaign
	err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO campaigns (guild_id, name, system, premise)
		 VALUES ($1,$2,$3,$4)
		 RETURNING id, guild_id, name, COALESCE(system,''), COALESCE(premise,''), archived, created_at, updated_at`,
		guildID, name, system, premise).
		Scan(&c.ID, &c.GuildID, &c.Name, &c.System, &c.Premise, &c.Archived, &c.CreatedAt, &c.UpdatedAt)
	return &c, err
}

// ListCampaigns returns campaigns for a guild, optionally including archived.
func (s *Store) ListCampaigns(ctx context.Context, guildID string, includeArchived bool) ([]Campaign, error) {
	q := `SELECT id, guild_id, name, COALESCE(system,''), COALESCE(premise,''), archived, created_at, updated_at
	      FROM campaigns WHERE guild_id=$1`
	if !includeArchived {
		q += ` AND NOT archived`
	}
	q += ` ORDER BY created_at`
	rows, err := s.db.Pool.Query(ctx, q, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Campaign
	for rows.Next() {
		var c Campaign
		if err := rows.Scan(&c.ID, &c.GuildID, &c.Name, &c.System, &c.Premise, &c.Archived, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCampaign fetches a campaign by ID.
func (s *Store) GetCampaign(ctx context.Context, id uuid.UUID) (*Campaign, error) {
	var c Campaign
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, guild_id, name, COALESCE(system,''), COALESCE(premise,''), archived, created_at, updated_at
		 FROM campaigns WHERE id=$1`, id).
		Scan(&c.ID, &c.GuildID, &c.Name, &c.System, &c.Premise, &c.Archived, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// UpdateCampaign edits mutable campaign fields.
func (s *Store) UpdateCampaign(ctx context.Context, id uuid.UUID, name, system, premise string) error {
	_, err := s.db.Pool.Exec(ctx,
		`UPDATE campaigns SET name=$2, system=$3, premise=$4, updated_at=now() WHERE id=$1`,
		id, name, system, premise)
	return err
}

// SetCampaignArchived toggles the archived flag.
func (s *Store) SetCampaignArchived(ctx context.Context, id uuid.UUID, archived bool) error {
	_, err := s.db.Pool.Exec(ctx,
		`UPDATE campaigns SET archived=$2, updated_at=now() WHERE id=$1`, id, archived)
	return err
}

// DeleteCampaign permanently removes a campaign and its cascaded data.
func (s *Store) DeleteCampaign(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Pool.Exec(ctx, `DELETE FROM campaigns WHERE id=$1`, id)
	return err
}

// SetActiveCampaign marks the campaign used for recording/worldbuilding.
func (s *Store) SetActiveCampaign(ctx context.Context, guildID string, campaignID uuid.UUID) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO active_campaign (guild_id, campaign_id) VALUES ($1,$2)
		 ON CONFLICT (guild_id) DO UPDATE SET campaign_id=EXCLUDED.campaign_id`,
		guildID, campaignID)
	return err
}

// GetActiveCampaign returns the active campaign for a guild, or ErrNotFound.
func (s *Store) GetActiveCampaign(ctx context.Context, guildID string) (*Campaign, error) {
	var c Campaign
	err := s.db.Pool.QueryRow(ctx,
		`SELECT c.id, c.guild_id, c.name, COALESCE(c.system,''), COALESCE(c.premise,''), c.archived, c.created_at, c.updated_at
		 FROM active_campaign a JOIN campaigns c ON c.id=a.campaign_id
		 WHERE a.guild_id=$1`, guildID).
		Scan(&c.ID, &c.GuildID, &c.Name, &c.System, &c.Premise, &c.Archived, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}
