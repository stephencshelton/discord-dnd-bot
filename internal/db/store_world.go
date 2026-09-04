package db

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// marshalMetadata encodes an entity metadata map to JSON bytes for a JSONB
// column. A nil/empty map becomes "{}" so the column is never NULL.
func marshalMetadata(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// unmarshalMetadata decodes JSONB bytes to a metadata map. Invalid/empty input
// yields nil (treated as "no metadata") rather than an error, so a malformed
// legacy row can't break reads.
func unmarshalMetadata(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

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

// GetPCByID fetches a single player character by its primary key.
func (s *Store) GetPCByID(ctx context.Context, id uuid.UUID) (*PlayerCharacter, error) {
	var p PlayerCharacter
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, campaign_id, discord_user_id, name, COALESCE(class,''), COALESCE(race,''), level, COALESCE(notes,''), created_at, updated_at
		 FROM player_characters WHERE id=$1`, id).
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
	meta, err := marshalMetadata(e.Metadata)
	if err != nil {
		return nil, err
	}
	err = s.db.Pool.QueryRow(ctx,
		`INSERT INTO world_entities (campaign_id, kind, name, description, metadata)
		 VALUES ($1,$2,$3,$4,$5)
		 RETURNING id, created_at, updated_at`,
		e.CampaignID, string(e.Kind), e.Name, e.Description, meta).
		Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
	return &e, err
}

// ListWorldEntities returns entities of a kind for a campaign.
func (s *Store) ListWorldEntities(ctx context.Context, campaignID uuid.UUID, kind WorldEntityKind) ([]WorldEntity, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, campaign_id, kind, name, COALESCE(description,''), COALESCE(metadata,'{}'::jsonb), created_at, updated_at
		 FROM world_entities WHERE campaign_id=$1 AND kind=$2 ORDER BY name`, campaignID, string(kind))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorldEntity
	for rows.Next() {
		var e WorldEntity
		var k string
		var meta []byte
		if err := rows.Scan(&e.ID, &e.CampaignID, &k, &e.Name, &e.Description, &meta, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.Kind = WorldEntityKind(k)
		e.Metadata = unmarshalMetadata(meta)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetWorldEntityByName looks up an entity by kind+name in a campaign.
func (s *Store) GetWorldEntityByName(ctx context.Context, campaignID uuid.UUID, kind WorldEntityKind, name string) (*WorldEntity, error) {
	var e WorldEntity
	var k string
	var meta []byte
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, campaign_id, kind, name, COALESCE(description,''), COALESCE(metadata,'{}'::jsonb), created_at, updated_at
		 FROM world_entities WHERE campaign_id=$1 AND kind=$2 AND lower(name)=lower($3)`,
		campaignID, string(kind), name).
		Scan(&e.ID, &e.CampaignID, &k, &e.Name, &e.Description, &meta, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	e.Kind = WorldEntityKind(k)
	e.Metadata = unmarshalMetadata(meta)
	return &e, err
}

// FindWorldEntitiesByName returns every entity in a campaign whose name matches
// (case-insensitively), across ALL kinds, ordered by kind.
//
// The kind+name pair is what's unique, so the same name can legitimately exist as
// both an NPC and a location ("Eastwatch" the captain, "Eastwatch" the town).
// This backs a kind-optional lookup: callers show the single hit directly, or ask
// the user which kind they meant when there's more than one.
func (s *Store) FindWorldEntitiesByName(ctx context.Context, campaignID uuid.UUID, name string) ([]WorldEntity, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, campaign_id, kind, name, COALESCE(description,''), COALESCE(metadata,'{}'::jsonb), created_at, updated_at
		 FROM world_entities WHERE campaign_id=$1 AND lower(name)=lower($2) ORDER BY kind`,
		campaignID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorldEntity
	for rows.Next() {
		var e WorldEntity
		var k string
		var meta []byte
		if err := rows.Scan(&e.ID, &e.CampaignID, &k, &e.Name, &e.Description, &meta, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.Kind = WorldEntityKind(k)
		e.Metadata = unmarshalMetadata(meta)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetWorldEntityByID fetches a single world entity by its primary key.
func (s *Store) GetWorldEntityByID(ctx context.Context, id uuid.UUID) (*WorldEntity, error) {
	var e WorldEntity
	var k string
	var meta []byte
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, campaign_id, kind, name, COALESCE(description,''), COALESCE(metadata,'{}'::jsonb), created_at, updated_at
		 FROM world_entities WHERE id=$1`, id).
		Scan(&e.ID, &e.CampaignID, &k, &e.Name, &e.Description, &meta, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	e.Kind = WorldEntityKind(k)
	e.Metadata = unmarshalMetadata(meta)
	return &e, err
}

// UpdateWorldEntity edits an entity's name/description.
func (s *Store) UpdateWorldEntity(ctx context.Context, id uuid.UUID, name, description string) error {
	_, err := s.db.Pool.Exec(ctx,
		`UPDATE world_entities SET name=$2, description=$3, updated_at=now() WHERE id=$1`,
		id, name, description)
	return err
}

// UpdateWorldEntityFull edits an entity's name, description, and metadata (the
// structured-input path). metadata replaces the stored object wholesale — the
// caller is responsible for merging with any existing metadata it wants to keep.
func (s *Store) UpdateWorldEntityFull(ctx context.Context, id uuid.UUID, name, description string, metadata map[string]any) error {
	meta, err := marshalMetadata(metadata)
	if err != nil {
		return err
	}
	_, err = s.db.Pool.Exec(ctx,
		`UPDATE world_entities SET name=$2, description=$3, metadata=$4, updated_at=now() WHERE id=$1`,
		id, name, description, meta)
	return err
}

// DeleteWorldEntity removes an entity.
func (s *Store) DeleteWorldEntity(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Pool.Exec(ctx, `DELETE FROM world_entities WHERE id=$1`, id)
	return err
}

// ListAllWorldEntities returns every world entity for a campaign across all
// kinds, ordered by kind then name. Used to give the state extractor the full
// existing-world context so it can update rather than duplicate entities.
func (s *Store) ListAllWorldEntities(ctx context.Context, campaignID uuid.UUID) ([]WorldEntity, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, campaign_id, kind, name, COALESCE(description,''), COALESCE(metadata,'{}'::jsonb), created_at, updated_at
		 FROM world_entities WHERE campaign_id=$1 ORDER BY kind, name`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorldEntity
	for rows.Next() {
		var e WorldEntity
		var k string
		var meta []byte
		if err := rows.Scan(&e.ID, &e.CampaignID, &k, &e.Name, &e.Description, &meta, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.Kind = WorldEntityKind(k)
		e.Metadata = unmarshalMetadata(meta)
		out = append(out, e)
	}
	return out, rows.Err()
}
