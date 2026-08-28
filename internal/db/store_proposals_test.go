package db

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
)

// newTestStore connects to the Postgres instance in TEST_DATABASE_URL and
// applies migrations, returning a Store scoped to a freshly-created guild +
// campaign for isolation. It SKIPS the test (rather than failing) when no test
// database is configured or the schema can't be created (e.g. pgvector missing),
// so the suite still passes in CI environments without a database — the pure
// logic is covered elsewhere (internal/extract) and these add DB-backed
// coverage when a database is available.
//
// Set TEST_DATABASE_URL to run these, e.g.:
//
//	TEST_DATABASE_URL=postgres://user:pass@localhost:5432/dnd_test?sslmode=disable go test ./internal/db/...
func newTestStore(t *testing.T) (*Store, uuid.UUID) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	ctx := context.Background()
	dbc, err := Connect(ctx, config.DatabaseConfig{URL: url})
	if err != nil {
		t.Skipf("cannot connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(dbc.Close)
	// Prefer the real migrations. If they fail (most commonly because the
	// pgvector `vector` extension isn't installed on the test server), fall back
	// to a minimal schema covering just the tables these proposal tests touch, so
	// the DB-backed coverage still runs against a plain Postgres.
	if err := dbc.Migrate(ctx, 1536); err != nil {
		t.Logf("full migrate failed (%v); falling back to minimal proposal schema", err)
		if serr := applyMinimalProposalSchema(ctx, dbc); serr != nil {
			t.Skipf("cannot create minimal schema: %v", serr)
		}
	}
	store := NewStore(dbc)

	guildID := "test-guild-" + uuid.NewString()
	if _, err := store.EnsureGuild(ctx, guildID); err != nil {
		t.Fatalf("EnsureGuild: %v", err)
	}
	camp, err := store.CreateCampaign(ctx, guildID, "Test Campaign", "D&D 5e", "")
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteCampaign(ctx, camp.ID) })
	return store, camp.ID
}

// applyMinimalProposalSchema creates just the tables the proposal tests need,
// without the pgvector-dependent embeddings table, so these DB tests can run
// against a plain Postgres that lacks the `vector` extension. It mirrors the
// relevant DDL in migrations/0001_init.sql (including the case-insensitive
// unique index that prevents duplicate entities).
func applyMinimalProposalSchema(ctx context.Context, dbc *DB) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		`CREATE TABLE IF NOT EXISTS guilds (
			id TEXT PRIMARY KEY, notes_channel_id TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS campaigns (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			guild_id TEXT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
			name TEXT NOT NULL, system TEXT, premise TEXT,
			archived BOOLEAN NOT NULL DEFAULT false,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS active_campaign (
			guild_id TEXT PRIMARY KEY REFERENCES guilds(id) ON DELETE CASCADE,
			campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE)`,
		`CREATE TABLE IF NOT EXISTS player_characters (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
			discord_user_id TEXT NOT NULL, name TEXT NOT NULL, class TEXT, race TEXT,
			level INT NOT NULL DEFAULT 1, notes TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS world_entities (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
			kind TEXT NOT NULL, name TEXT NOT NULL, description TEXT,
			metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_world_entities_campaign_kind_name
			ON world_entities(campaign_id, kind, lower(name))`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
			guild_id TEXT NOT NULL, voice_channel_id TEXT,
			status TEXT NOT NULL DEFAULT 'recording', audio_key TEXT, chunk_prefix TEXT,
			transcript TEXT, notes TEXT, duration_seconds INT NOT NULL DEFAULT 0,
			started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(), ended_at TIMESTAMPTZ)`,
		`CREATE TABLE IF NOT EXISTS state_proposals (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
			session_id UUID REFERENCES sessions(id) ON DELETE SET NULL,
			action TEXT NOT NULL, entity_kind TEXT NOT NULL,
			entity_id UUID REFERENCES world_entities(id) ON DELETE SET NULL,
			entity_name TEXT NOT NULL, patch JSONB NOT NULL DEFAULT '{}'::jsonb,
			explanation TEXT NOT NULL DEFAULT '', evidence TEXT NOT NULL DEFAULT '',
			confidence DOUBLE PRECISION NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending', reviewed_by TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), reviewed_at TIMESTAMPTZ)`,
	}
	for _, s := range stmts {
		if _, err := dbc.Pool.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// TestProposalPersistenceAndListing covers create + list + count round-trips.
func TestProposalPersistenceAndListing(t *testing.T) {
	store, campID := newTestStore(t)
	ctx := context.Background()

	p := StateProposal{
		CampaignID:  campID,
		Action:      ActionCreateEntity,
		EntityKind:  KindNPC,
		EntityName:  "Captain Varek",
		Patch:       map[string]any{"description": "Commander of the Eastwatch guard.", "role": "commander"},
		Explanation: "New NPC.",
		Evidence:    "Introduced at Eastwatch.",
		Confidence:  0.9,
	}
	created, err := store.CreateStateProposal(ctx, p)
	if err != nil {
		t.Fatalf("CreateStateProposal: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("created proposal has nil ID")
	}

	got, err := store.GetStateProposal(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetStateProposal: %v", err)
	}
	if got.EntityName != "Captain Varek" || got.Description() != "Commander of the Eastwatch guard." {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if role, _ := got.Patch["role"].(string); role != "commander" {
		t.Errorf("patch metadata lost: %+v", got.Patch)
	}

	n, err := store.CountPendingProposalsForCampaign(ctx, campID)
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n != 1 {
		t.Errorf("pending count = %d, want 1", n)
	}
}

// TestApproveNewEntity approves a create proposal and confirms the world entity
// is created and the proposal marked approved.
func TestApproveNewEntity(t *testing.T) {
	store, campID := newTestStore(t)
	ctx := context.Background()

	created, err := store.CreateStateProposal(ctx, StateProposal{
		CampaignID: campID, Action: ActionCreateEntity, EntityKind: KindLocation,
		EntityName: "Eastwatch", Patch: map[string]any{"description": "A border town."},
		Evidence: "They arrived.", Confidence: 0.8,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ch, applied, err := store.ApproveProposal(ctx, created.ID, "dm-1")
	if err != nil {
		t.Fatalf("ApproveProposal: %v", err)
	}
	if !applied {
		t.Fatal("first approve should apply")
	}
	if ch == nil || ch.DisplayName != "Eastwatch" || ch.SourceKind != CanonSourceEntity {
		t.Fatalf("applied change wrong: %+v", ch)
	}

	// Proposal is now approved.
	got, _ := store.GetStateProposal(ctx, created.ID)
	if got.Status != ProposalApproved {
		t.Errorf("status = %q, want approved", got.Status)
	}
	// And it's canon.
	canon, err := store.GetWorldEntityByName(ctx, campID, KindLocation, "eastwatch")
	if err != nil {
		t.Fatalf("entity not in canon: %v", err)
	}
	if canon.ID != ch.SourceID {
		t.Errorf("canon id mismatch")
	}
	if canon.Description != "A border town." {
		t.Errorf("entity description = %q", canon.Description)
	}
}

// TestApproveExistingEntityUpdate approves an update proposal and confirms the
// existing entity is merged (description updated, hand-written metadata kept).
func TestApproveExistingEntityUpdate(t *testing.T) {
	store, campID := newTestStore(t)
	ctx := context.Background()

	// Seed a hand-written entity with metadata a DM authored.
	existing, err := store.CreateWorldEntity(ctx, WorldEntity{
		CampaignID: campID, Kind: KindQuest, Name: "The Missing Caravan",
		Description: "Find the caravan.", Metadata: map[string]any{"reward": "500gp"},
	})
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	eid := existing.ID
	created, err := store.CreateStateProposal(ctx, StateProposal{
		CampaignID: campID, Action: ActionUpdateEntity, EntityKind: KindQuest,
		EntityID: &eid, EntityName: "The Missing Caravan",
		Patch:    map[string]any{"description": "Completed — survivors returned.", "status": "completed"},
		Evidence: "They returned the survivors.", Confidence: 0.95,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ch, applied, err := store.ApproveProposal(ctx, created.ID, "dm-1")
	if err != nil || !applied {
		t.Fatalf("approve: applied=%v err=%v", applied, err)
	}
	if ch.SourceID != existing.ID {
		t.Errorf("update created a new entity instead of updating")
	}
	// Re-fetch to verify the merge (append description + merged metadata).
	ent, gerr := store.GetWorldEntityByID(ctx, existing.ID)
	if gerr != nil {
		t.Fatalf("refetch entity: %v", gerr)
	}
	if !strings.Contains(ent.Description, "Completed — survivors returned.") {
		t.Errorf("description not appended: %q", ent.Description)
	}
	if !strings.Contains(ent.Description, "Find the caravan.") {
		t.Errorf("prior description lost (should append, not overwrite): %q", ent.Description)
	}
	if status, _ := ent.Metadata["status"].(string); status != "completed" {
		t.Errorf("status metadata not merged: %+v", ent.Metadata)
	}
	// Hand-written reward metadata must survive the merge (no clobber).
	if reward, _ := ent.Metadata["reward"].(string); reward != "500gp" {
		t.Errorf("hand-written metadata clobbered: %+v", ent.Metadata)
	}
}

// TestRejectLeavesCanonUntouched rejects a proposal and confirms no entity is
// created.
func TestRejectLeavesCanonUntouched(t *testing.T) {
	store, campID := newTestStore(t)
	ctx := context.Background()

	created, err := store.CreateStateProposal(ctx, StateProposal{
		CampaignID: campID, Action: ActionCreateEntity, EntityKind: KindFaction,
		EntityName: "The Ashen Circle", Patch: map[string]any{"description": "A cult."},
		Evidence: "Rumored.", Confidence: 0.5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	changed, err := store.RejectProposal(ctx, created.ID, "dm-1")
	if err != nil || !changed {
		t.Fatalf("reject: changed=%v err=%v", changed, err)
	}
	if _, err := store.GetWorldEntityByName(ctx, campID, KindFaction, "The Ashen Circle"); err == nil {
		t.Error("rejected proposal must not create canon")
	}
	// Rejecting again is an idempotent no-op.
	changed, err = store.RejectProposal(ctx, created.ID, "dm-2")
	if err != nil {
		t.Fatalf("second reject: %v", err)
	}
	if changed {
		t.Error("second reject should be a no-op (changed=false)")
	}
}

// TestIdempotentApprove clicking approve twice must not create a duplicate
// entity nor double-apply.
func TestIdempotentApprove(t *testing.T) {
	store, campID := newTestStore(t)
	ctx := context.Background()

	created, err := store.CreateStateProposal(ctx, StateProposal{
		CampaignID: campID, Action: ActionCreateEntity, EntityKind: KindNPC,
		EntityName: "Dana", Patch: map[string]any{"description": "A guide."},
		Evidence: "She led them.", Confidence: 0.8,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ch1, applied1, err := store.ApproveProposal(ctx, created.ID, "dm-1")
	if err != nil || !applied1 {
		t.Fatalf("first approve: applied=%v err=%v", applied1, err)
	}
	ch2, applied2, err := store.ApproveProposal(ctx, created.ID, "dm-1")
	if err != nil {
		t.Fatalf("second approve: %v", err)
	}
	if applied2 {
		t.Error("second approve should be an idempotent no-op (applied=false)")
	}
	if ch2 != nil {
		t.Error("second approve should not return a change")
	}

	// Exactly one NPC named Dana exists.
	npcs, err := store.ListWorldEntities(ctx, campID, KindNPC)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	count := 0
	for _, n := range npcs {
		if n.Name == "Dana" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 Dana, got %d (duplicate created!)", count)
	}
	_ = ch1
}

// TestDuplicateEntityPreventedByUniqueIndex ensures two create proposals for the
// same (kind, name) — even different casing — resolve to a single entity when
// both are approved, never a duplicate.
func TestDuplicateEntityPreventedByUniqueIndex(t *testing.T) {
	store, campID := newTestStore(t)
	ctx := context.Background()

	mk := func(name string) uuid.UUID {
		p, err := store.CreateStateProposal(ctx, StateProposal{
			CampaignID: campID, Action: ActionCreateEntity, EntityKind: KindNPC,
			EntityName: name, Patch: map[string]any{"description": "desc " + name},
			Evidence: "e", Confidence: 0.7,
		})
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		return p.ID
	}
	id1 := mk("Grix")
	id2 := mk("grix") // different casing, same entity

	if _, _, err := store.ApproveProposal(ctx, id1, "dm"); err != nil {
		t.Fatalf("approve 1: %v", err)
	}
	if _, _, err := store.ApproveProposal(ctx, id2, "dm"); err != nil {
		t.Fatalf("approve 2 (should upsert, not duplicate): %v", err)
	}

	npcs, _ := store.ListWorldEntities(ctx, campID, KindNPC)
	count := 0
	for _, n := range npcs {
		if n.Name == "Grix" || n.Name == "grix" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 entity after two same-name approves, got %d", count)
	}
}

// TestReplacePendingSessionProposalsIsIdempotent confirms re-extraction replaces
// pending proposals for a session without duplicating, preserving decided ones.
func TestReplacePendingSessionProposalsIsIdempotent(t *testing.T) {
	store, campID := newTestStore(t)
	ctx := context.Background()

	sess, err := store.CreateSession(ctx, campID, "test-guild", "vc-1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	first := []StateProposal{
		{CampaignID: campID, Action: ActionCreateEntity, EntityKind: KindNPC, EntityName: "A", Patch: map[string]any{"description": "a"}, Evidence: "e", Confidence: 0.5},
		{CampaignID: campID, Action: ActionCreateEntity, EntityKind: KindNPC, EntityName: "B", Patch: map[string]any{"description": "b"}, Evidence: "e", Confidence: 0.5},
	}
	if err := store.ReplacePendingSessionProposals(ctx, campID, sess.ID, first); err != nil {
		t.Fatalf("first replace: %v", err)
	}

	// Re-running extraction produces a different set; pending ones are replaced.
	second := []StateProposal{
		{CampaignID: campID, Action: ActionCreateEntity, EntityKind: KindNPC, EntityName: "C", Patch: map[string]any{"description": "c"}, Evidence: "e", Confidence: 0.5},
	}
	if err := store.ReplacePendingSessionProposals(ctx, campID, sess.ID, second); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	pending, err := store.ListPendingProposalsForSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].EntityName != "C" {
		t.Fatalf("expected only [C] pending after replace, got %+v", pending)
	}
}

// TestApproveAppendsDescription verifies an approved update APPENDS to an
// entity's description rather than overwriting it (details accumulate).
func TestApproveAppendsDescription(t *testing.T) {
	store, campID := newTestStore(t)
	ctx := context.Background()

	seed, err := store.CreateWorldEntity(ctx, WorldEntity{
		CampaignID: campID, Kind: KindNPC, Name: "Varek", Description: "A stern guard captain.",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	created, err := store.CreateStateProposal(ctx, StateProposal{
		CampaignID: campID, Action: ActionUpdateEntity, EntityKind: KindNPC,
		EntityName: "Varek", Patch: map[string]any{"description": "Secretly working for the cult."},
		Evidence: "Overheard him plotting.", Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	if _, applied, aerr := store.ApproveProposal(ctx, created.ID, "dm"); aerr != nil || !applied {
		t.Fatalf("approve: applied=%v err=%v", applied, aerr)
	}
	ent, _ := store.GetWorldEntityByID(ctx, seed.ID)
	if !strings.Contains(ent.Description, "stern guard captain") {
		t.Errorf("prior detail lost: %q", ent.Description)
	}
	if !strings.Contains(ent.Description, "cult") {
		t.Errorf("new detail not appended: %q", ent.Description)
	}
}

// TestApproveCharacterProposalAppendsNotes verifies a character-target proposal
// appends to an existing PC's notes and never creates a PC.
func TestApproveCharacterProposalAppendsNotes(t *testing.T) {
	store, campID := newTestStore(t)
	ctx := context.Background()

	pc, err := store.CreatePC(ctx, PlayerCharacter{
		CampaignID: campID, DiscordUserID: "user-1", Name: "Ludo", Notes: "Loves shiny things.",
	})
	if err != nil {
		t.Fatalf("create pc: %v", err)
	}
	created, err := store.CreateStateProposal(ctx, StateProposal{
		CampaignID: campID, Action: ActionUpdateEntity, EntityKind: KindCharacter,
		EntityName: "Ludo", Patch: map[string]any{"description": "Slew the dragon of Eastwatch."},
		Evidence: "Ludo landed the killing blow.", Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	ch, applied, aerr := store.ApproveProposal(ctx, created.ID, "dm")
	if aerr != nil || !applied {
		t.Fatalf("approve: applied=%v err=%v", applied, aerr)
	}
	if ch.SourceKind != CanonSourceCharacter || ch.SourceID != pc.ID {
		t.Errorf("applied change wrong for PC: %+v", ch)
	}
	got, _ := store.GetPCByID(ctx, pc.ID)
	if !strings.Contains(got.Notes, "Loves shiny things.") {
		t.Errorf("prior PC notes lost: %q", got.Notes)
	}
	if !strings.Contains(got.Notes, "dragon of Eastwatch") {
		t.Errorf("new PC deed not appended: %q", got.Notes)
	}

	// A character proposal naming no known PC is a no-op change (no PC created).
	before, _ := store.ListPCs(ctx, campID)
	orphan, _ := store.CreateStateProposal(ctx, StateProposal{
		CampaignID: campID, Action: ActionUpdateEntity, EntityKind: KindCharacter,
		EntityName: "Ghost", Patch: map[string]any{"description": "Did something."},
		Evidence: "e", Confidence: 0.5,
	})
	ch2, applied2, _ := store.ApproveProposal(ctx, orphan.ID, "dm")
	if !applied2 {
		t.Error("orphan character proposal should still be marked applied (approved)")
	}
	if ch2.SourceID != uuid.Nil {
		t.Errorf("orphan character proposal should have no source id, got %v", ch2.SourceID)
	}
	after, _ := store.ListPCs(ctx, campID)
	if len(after) != len(before) {
		t.Errorf("character proposal must never create a PC: before=%d after=%d", len(before), len(after))
	}
}
