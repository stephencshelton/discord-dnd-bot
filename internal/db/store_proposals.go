package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// --- Session-derived campaign-state proposals ---
//
// Proposals are the guardrail that keeps AI-derived information from silently
// becoming campaign canon. Extraction writes rows here in 'pending' status; a
// DM approves (atomically applying the change to world_entities) or rejects
// (leaving canon untouched) via /review-session.

// CreateStateProposal inserts a pending proposal and returns it with its
// generated ID/timestamp populated.
func (s *Store) CreateStateProposal(ctx context.Context, p StateProposal) (*StateProposal, error) {
	patch, err := marshalMetadata(p.Patch)
	if err != nil {
		return nil, fmt.Errorf("marshal patch: %w", err)
	}
	if p.Status == "" {
		p.Status = ProposalPending
	}
	err = s.db.Pool.QueryRow(ctx,
		`INSERT INTO state_proposals
		   (campaign_id, session_id, action, entity_kind, entity_id, entity_name,
		    patch, explanation, evidence, confidence, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING id, created_at`,
		p.CampaignID, p.SessionID, string(p.Action), string(p.EntityKind), p.EntityID,
		p.EntityName, patch, p.Explanation, p.Evidence, p.Confidence, p.Status).
		Scan(&p.ID, &p.CreatedAt)
	return &p, err
}

// ReplacePendingSessionProposals atomically clears a session's existing PENDING
// proposals and inserts the freshly-extracted set, all in one transaction. This
// makes re-running extraction idempotent: a reprocess replaces stale pending
// suggestions rather than duplicating them, while proposals the DM has already
// approved or rejected are left untouched (only status='pending' rows are
// deleted). An empty `proposals` slice simply clears the pending set.
func (s *Store) ReplacePendingSessionProposals(ctx context.Context, campaignID, sessionID uuid.UUID, proposals []StateProposal) error {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM state_proposals WHERE session_id=$1 AND status=$2`,
		sessionID, ProposalPending); err != nil {
		return fmt.Errorf("clear pending proposals: %w", err)
	}

	for i := range proposals {
		p := proposals[i]
		patch, err := marshalMetadata(p.Patch)
		if err != nil {
			return fmt.Errorf("marshal patch: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO state_proposals
			   (campaign_id, session_id, action, entity_kind, entity_id, entity_name,
			    patch, explanation, evidence, confidence, status)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			campaignID, sessionID, string(p.Action), string(p.EntityKind), p.EntityID,
			p.EntityName, patch, p.Explanation, p.Evidence, p.Confidence, ProposalPending); err != nil {
			return fmt.Errorf("insert proposal: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// scanProposal reads one proposal row (column order must match the SELECTs below).
func scanProposal(row pgx.Row) (*StateProposal, error) {
	var (
		p       StateProposal
		action  string
		kind    string
		patch   []byte
		session *uuid.UUID
		entity  *uuid.UUID
	)
	if err := row.Scan(&p.ID, &p.CampaignID, &session, &action, &kind, &entity,
		&p.EntityName, &patch, &p.Explanation, &p.Evidence, &p.Confidence,
		&p.Status, &p.ReviewedBy, &p.CreatedAt, &p.ReviewedAt); err != nil {
		return nil, err
	}
	p.SessionID = session
	p.Action = ProposalAction(action)
	p.EntityKind = WorldEntityKind(kind)
	p.EntityID = entity
	if len(patch) > 0 {
		_ = json.Unmarshal(patch, &p.Patch)
	}
	return &p, nil
}

const proposalCols = `id, campaign_id, session_id, action, entity_kind, entity_id,
	entity_name, patch, COALESCE(explanation,''), COALESCE(evidence,''), confidence,
	status, reviewed_by, created_at, reviewed_at`

// GetStateProposal fetches a single proposal by ID.
func (s *Store) GetStateProposal(ctx context.Context, id uuid.UUID) (*StateProposal, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT `+proposalCols+` FROM state_proposals WHERE id=$1`, id)
	p, err := scanProposal(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ListPendingProposalsForSession returns the pending proposals for a session,
// oldest first (stable review order).
func (s *Store) ListPendingProposalsForSession(ctx context.Context, sessionID uuid.UUID) ([]StateProposal, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT `+proposalCols+` FROM state_proposals
		 WHERE session_id=$1 AND status=$2 ORDER BY created_at`, sessionID, ProposalPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProposals(rows)
}

// ListPendingProposalsForCampaign returns the pending proposals for a campaign,
// oldest first. Used when reviewing without a specific session (e.g. proposals
// not tied to a particular recording).
func (s *Store) ListPendingProposalsForCampaign(ctx context.Context, campaignID uuid.UUID) ([]StateProposal, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT `+proposalCols+` FROM state_proposals
		 WHERE campaign_id=$1 AND status=$2 ORDER BY created_at`, campaignID, ProposalPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProposals(rows)
}

func collectProposals(rows pgx.Rows) ([]StateProposal, error) {
	var out []StateProposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// CountPendingProposalsForCampaign returns how many proposals are awaiting
// review across a campaign (used to nudge the DM).
func (s *Store) CountPendingProposalsForCampaign(ctx context.Context, campaignID uuid.UUID) (int, error) {
	var n int
	err := s.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM state_proposals WHERE campaign_id=$1 AND status=$2`,
		campaignID, ProposalPending).Scan(&n)
	return n, err
}

// SessionIDsWithPendingProposals returns, for a campaign, the distinct session
// IDs that still have pending proposals, newest session first. Powers the
// /review-session autocomplete/selection.
func (s *Store) SessionIDsWithPendingProposals(ctx context.Context, campaignID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT DISTINCT sp.session_id
		 FROM state_proposals sp
		 JOIN sessions se ON se.id = sp.session_id
		 WHERE sp.campaign_id=$1 AND sp.status=$2 AND sp.session_id IS NOT NULL
		 ORDER BY sp.session_id`, campaignID, ProposalPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RejectProposal marks a pending proposal rejected without touching canon. It
// is idempotent: rejecting an already-decided proposal is a no-op that reports
// whether this call was the one that changed it.
func (s *Store) RejectProposal(ctx context.Context, id uuid.UUID, reviewerID string) (changed bool, err error) {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE state_proposals
		 SET status=$2, reviewed_by=$3, reviewed_at=now()
		 WHERE id=$1 AND status=$4`,
		id, ProposalRejected, reviewerID, ProposalPending)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateProposalPatch edits a pending proposal's proposed name/description
// before approval (the DM's "Edit" action). Rejected/approved proposals are not
// editable. Returns ErrNotFound if the proposal isn't pending.
func (s *Store) UpdateProposalPatch(ctx context.Context, id uuid.UUID, entityName, description string) error {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE state_proposals
		 SET entity_name=$2,
		     patch = jsonb_set(COALESCE(patch,'{}'::jsonb), '{description}', to_jsonb($3::text), true)
		 WHERE id=$1 AND status=$4`,
		id, entityName, description, ProposalPending)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// ApproveProposal atomically applies a pending proposal to canon and marks it
// approved, all in one transaction. It is IDEMPOTENT and safe under retries /
// double-clicks:
//
//   - It first conditionally claims the proposal (status pending -> approved)
//     with an UPDATE ... WHERE status='pending' RETURNING. If no row is claimed,
//     the proposal was already decided, so it returns (nil, false, nil) and does
//     NOT touch canon a second time (no duplicate entity, no double update).
//   - Only the caller that wins the claim applies the world change, keying
//     entity creation on the case-insensitive (campaign, kind, name) unique
//     index via ON CONFLICT so a concurrent create can't duplicate an entity.
//
// It returns the resulting canonical change and whether this call performed the
// application.
func (s *Store) ApproveProposal(ctx context.Context, id uuid.UUID, reviewerID string) (change *AppliedChange, applied bool, err error) {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1) Claim the proposal. The WHERE status='pending' makes this the single
	// point of serialization: exactly one concurrent approve wins.
	row := tx.QueryRow(ctx,
		`UPDATE state_proposals
		 SET status=$2, reviewed_by=$3, reviewed_at=now()
		 WHERE id=$1 AND status=$4
		 RETURNING `+proposalCols,
		id, ProposalApproved, reviewerID, ProposalPending)
	p, err := scanProposal(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already approved or rejected — idempotent no-op.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	// 2) Apply the change to canon (world entity or player character).
	ch, err := applyProposalTx(ctx, tx, p)
	if err != nil {
		return nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return ch, true, nil
}

// applyProposalTx performs the canonical change for an approved proposal inside
// the given transaction, dispatching on the proposal's target:
//   - KindCharacter -> append the proposed detail to an existing player
//     character's notes (never creates one; a PC needs a Discord owner).
//   - any world kind -> create/append a world_entities row (see applyEntityTx).
func applyProposalTx(ctx context.Context, tx pgx.Tx, p *StateProposal) (*AppliedChange, error) {
	if p.EntityKind == KindCharacter {
		return applyCharacterProposalTx(ctx, tx, p)
	}
	return applyEntityProposalTx(ctx, tx, p)
}

// applyCharacterProposalTx appends an approved proposal's detail to an existing
// player character's notes (matched case-insensitively by name within the
// campaign). It never creates a character. If no match exists, it's a no-op
// change (returns an AppliedChange with a zero SourceID) so approval doesn't
// error — the fact is simply not attachable to a known PC.
func applyCharacterProposalTx(ctx context.Context, tx pgx.Tx, p *StateProposal) (*AppliedChange, error) {
	var (
		id    uuid.UUID
		notes string
	)
	err := tx.QueryRow(ctx,
		`SELECT id, COALESCE(notes,'') FROM player_characters
		 WHERE campaign_id=$1 AND lower(name)=lower($2)`,
		p.CampaignID, p.EntityName).Scan(&id, &notes)
	if errors.Is(err, pgx.ErrNoRows) {
		// No such player character — nothing to attach the fact to.
		return &AppliedChange{CampaignID: p.CampaignID, SourceKind: CanonSourceCharacter, DisplayName: p.EntityName}, nil
	}
	if err != nil {
		return nil, err
	}
	newNotes := AppendDetail(notes, p.Description())
	if _, err := tx.Exec(ctx,
		`UPDATE player_characters SET notes=$2, updated_at=now() WHERE id=$1`, id, newNotes); err != nil {
		return nil, fmt.Errorf("update character notes: %w", err)
	}
	return &AppliedChange{CampaignID: p.CampaignID, SourceKind: CanonSourceCharacter, SourceID: id, DisplayName: p.EntityName}, nil
}

// applyEntityProposalTx creates or appends-to a world entity for an approved
// proposal. Creating is upserted on the case-insensitive (campaign, kind, name)
// unique index so a retry or a concurrent identical create merges rather than
// duplicates. Updating an entity APPENDS the proposal's description to the
// existing one (never overwriting hand-written detail — accumulate as the entity
// recurs) and deep-merges metadata so structured fields survive.
func applyEntityProposalTx(ctx context.Context, tx pgx.Tx, p *StateProposal) (*AppliedChange, error) {
	desc := p.Description()
	meta := map[string]any{}
	for k, v := range p.Patch {
		if k == "description" {
			continue
		}
		meta[k] = v
	}

	// Resolve any existing entity so we can APPEND to (not replace) its
	// description. For a create action we match on the unique key; for update we
	// prefer the explicit id, then fall back to the name.
	var (
		existingID   uuid.UUID
		existingDesc string
		found        bool
	)
	if p.Action == ActionUpdateEntity && p.EntityID != nil {
		if err := tx.QueryRow(ctx,
			`SELECT id, COALESCE(description,'') FROM world_entities WHERE id=$1 AND campaign_id=$2`,
			*p.EntityID, p.CampaignID).Scan(&existingID, &existingDesc); err == nil {
			found = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	if !found {
		if err := tx.QueryRow(ctx,
			`SELECT id, COALESCE(description,'') FROM world_entities
			 WHERE campaign_id=$1 AND kind=$2 AND lower(name)=lower($3)`,
			p.CampaignID, string(p.EntityKind), p.EntityName).Scan(&existingID, &existingDesc); err == nil {
			found = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	metaJSON, err := marshalMetadata(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal patch metadata: %w", err)
	}

	if found {
		// Update in place: APPEND the new description (dedup-aware) and merge
		// metadata. Existing hand-written detail is preserved.
		newDesc := AppendDetail(existingDesc, desc)
		var e WorldEntity
		var k string
		var gotMeta []byte
		err := tx.QueryRow(ctx,
			`UPDATE world_entities
			 SET description = $2,
			     metadata = COALESCE(metadata,'{}'::jsonb) || $3::jsonb,
			     updated_at = now()
			 WHERE id=$1
			 RETURNING id, campaign_id, kind, name, COALESCE(description,''), COALESCE(metadata,'{}'::jsonb), created_at, updated_at`,
			existingID, newDesc, metaJSON).
			Scan(&e.ID, &e.CampaignID, &k, &e.Name, &e.Description, &gotMeta, &e.CreatedAt, &e.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("update entity: %w", err)
		}
		return &AppliedChange{CampaignID: e.CampaignID, SourceKind: CanonSourceEntity, SourceID: e.ID, DisplayName: e.Name}, nil
	}

	// No existing entity — create it. (Applies to both create actions and
	// updates whose target no longer exists, so an approval is never lost.)
	// ON CONFLICT guards against a concurrent create racing this one.
	var e WorldEntity
	var k string
	var gotMeta []byte
	err = tx.QueryRow(ctx,
		`INSERT INTO world_entities (campaign_id, kind, name, description, metadata)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (campaign_id, kind, lower(name)) DO UPDATE
		   SET description = CASE WHEN EXCLUDED.description <> ''
		                         THEN EXCLUDED.description
		                         ELSE world_entities.description END,
		       metadata = COALESCE(world_entities.metadata,'{}'::jsonb) || EXCLUDED.metadata,
		       updated_at = now()
		 RETURNING id, campaign_id, kind, name, COALESCE(description,''), COALESCE(metadata,'{}'::jsonb), created_at, updated_at`,
		p.CampaignID, string(p.EntityKind), p.EntityName, desc, metaJSON).
		Scan(&e.ID, &e.CampaignID, &k, &e.Name, &e.Description, &gotMeta, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create entity: %w", err)
	}
	return &AppliedChange{CampaignID: e.CampaignID, SourceKind: CanonSourceEntity, SourceID: e.ID, DisplayName: e.Name}, nil
}
