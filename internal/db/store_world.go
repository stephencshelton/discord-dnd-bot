package db

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// --- Player characters ---

// CreatePC inserts a player character.
func (s *Store) CreatePC(ctx context.Context, pc PlayerCharacter) (*PlayerCharacter, error) {
	err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO player_characters (campaign_id, discord_user_id, name, class, race, level, notes)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 RETURNING id, created_at, updated_at`,
		pc.CampaignID, pc.DiscordUserID, pc.Name, pc.Class, pc.Race, pc.Level, pc.Notes).
		Scan(&pc.ID, &pc.CreatedAt, &pc.UpdatedAt)
	return &pc, err
}

// ListPCs returns all characters in a campaign.
func (s *Store) ListPCs(ctx context.Context, campaignID uuid.UUID) ([]PlayerCharacter, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, campaign_id, discord_user_id, name, COALESCE(class,''), COALESCE(race,''), level, COALESCE(notes,''), created_at, updated_at
		 FROM player_characters WHERE campaign_id=$1 ORDER BY name`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlayerCharacter
	for rows.Next() {
		var p PlayerCharacter
		if err := rows.Scan(&p.ID, &p.CampaignID, &p.DiscordUserID, &p.Name, &p.Class, &p.Race, &p.Level, &p.Notes, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPCByName looks up a character by name within a campaign.
func (s *Store) GetPCByName(ctx context.Context, campaignID uuid.UUID, name string) (*PlayerCharacter, error) {
	var p PlayerCharacter
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, campaign_id, discord_user_id, name, COALESCE(class,''), COALESCE(race,''), level, COALESCE(notes,''), created_at, updated_at
		 FROM player_characters WHERE campaign_id=$1 AND lower(name)=lower($2)`, campaignID, name).
		Scan(&p.ID, &p.CampaignID, &p.DiscordUserID, &p.Name, &p.Class, &p.Race, &p.Level, &p.Notes, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

// UpdatePC edits character fields.
func (s *Store) UpdatePC(ctx context.Context, id uuid.UUID, name, class, race string, level int, notes string) error {
	_, err := s.db.Pool.Exec(ctx,
		`UPDATE player_characters SET name=$2, class=$3, race=$4, level=$5, notes=$6, updated_at=now() WHERE id=$1`,
		id, name, class, race, level, notes)
	return err
}

// DeletePC removes a character.
func (s *Store) DeletePC(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Pool.Exec(ctx, `DELETE FROM player_characters WHERE id=$1`, id)
	return err
}

// --- World entities (npc/location/faction/quest) ---

// CreateWorldEntity inserts a worldbuilding record.
func (s *Store) CreateWorldEntity(ctx context.Context, e WorldEntity) (*WorldEntity, error) {
	err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO world_entities (campaign_id, kind, name, description)
		 VALUES ($1,$2,$3,$4)
		 RETURNING id, created_at, updated_at`,
		e.CampaignID, string(e.Kind), e.Name, e.Description).
		Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
	return &e, err
}

// ListWorldEntities returns entities of a kind for a campaign.
func (s *Store) ListWorldEntities(ctx context.Context, campaignID uuid.UUID, kind WorldEntityKind) ([]WorldEntity, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, campaign_id, kind, name, COALESCE(description,''), created_at, updated_at
		 FROM world_entities WHERE campaign_id=$1 AND kind=$2 ORDER BY name`, campaignID, string(kind))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorldEntity
	for rows.Next() {
		var e WorldEntity
		var k string
		if err := rows.Scan(&e.ID, &e.CampaignID, &k, &e.Name, &e.Description, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.Kind = WorldEntityKind(k)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetWorldEntityByName looks up an entity by kind+name in a campaign.
func (s *Store) GetWorldEntityByName(ctx context.Context, campaignID uuid.UUID, kind WorldEntityKind, name string) (*WorldEntity, error) {
	var e WorldEntity
	var k string
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, campaign_id, kind, name, COALESCE(description,''), created_at, updated_at
		 FROM world_entities WHERE campaign_id=$1 AND kind=$2 AND lower(name)=lower($3)`,
		campaignID, string(kind), name).
		Scan(&e.ID, &e.CampaignID, &k, &e.Name, &e.Description, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	e.Kind = WorldEntityKind(k)
	return &e, err
}

// UpdateWorldEntity edits an entity's name/description.
func (s *Store) UpdateWorldEntity(ctx context.Context, id uuid.UUID, name, description string) error {
	_, err := s.db.Pool.Exec(ctx,
		`UPDATE world_entities SET name=$2, description=$3, updated_at=now() WHERE id=$1`,
		id, name, description)
	return err
}

// DeleteWorldEntity removes an entity.
func (s *Store) DeleteWorldEntity(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Pool.Exec(ctx, `DELETE FROM world_entities WHERE id=$1`, id)
	return err
}
