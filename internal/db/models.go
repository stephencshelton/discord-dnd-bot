package db

import (
	"time"

	"github.com/google/uuid"
)

// Guild is a Discord server's bot settings.
type Guild struct {
	ID             string
	NotesChannelID string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Campaign is a single ongoing story within a guild.
type Campaign struct {
	ID        uuid.UUID
	GuildID   string
	Name      string
	System    string
	Premise   string
	Archived  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PlayerCharacter is a hero owned by a Discord user.
type PlayerCharacter struct {
	ID            uuid.UUID
	CampaignID    uuid.UUID
	DiscordUserID string
	Name          string
	Class         string
	Race          string
	Level         int
	Notes         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// WorldEntityKind enumerates worldbuilding entity types.
type WorldEntityKind string

const (
	KindNPC      WorldEntityKind = "npc"
	KindLocation WorldEntityKind = "location"
	KindFaction  WorldEntityKind = "faction"
	KindQuest    WorldEntityKind = "quest"
	// KindHook covers unresolved story hooks / plot threads the party could
	// pursue (a mysterious letter, an unanswered summons). Distinct from quests
	// (which have objectives/status) so /prep can list dangling threads.
	KindHook WorldEntityKind = "hook"
)

// AllWorldKinds is every valid world-entity kind, used for validation and to
// enumerate the extraction/template surface in one place.
var AllWorldKinds = []WorldEntityKind{KindNPC, KindLocation, KindFaction, KindQuest, KindHook}

// KindCharacter is a proposal target discriminator (NOT a world_entities kind):
// a proposal with EntityKind == KindCharacter targets an existing player
// character in player_characters rather than a world entity. It's stored in the
// same free-text entity_kind column so no schema change is needed, and the apply
// path branches on it. Extraction can only UPDATE characters (append recorded
// deeds/facts to their notes) — never create one, since a PC needs a Discord
// owner the transcript can't supply.
const KindCharacter WorldEntityKind = "character"

// ValidWorldKind reports whether k is a known world-entity kind (excludes the
// KindCharacter proposal-target discriminator).
func ValidWorldKind(k WorldEntityKind) bool {
	for _, v := range AllWorldKinds {
		if v == k {
			return true
		}
	}
	return false
}

// WorldEntity is a generic worldbuilding record (NPC/location/faction/quest).
type WorldEntity struct {
	ID          uuid.UUID
	CampaignID  uuid.UUID
	Kind        WorldEntityKind
	Name        string
	Description string
	// Metadata holds optional structured, kind-specific fields beyond the
	// freeform Description (e.g. a quest's status, an NPC's role). Stored as a
	// JSONB object; nil/empty means no extra metadata. Approved AI state
	// proposals merge their structured patch data here.
	Metadata  map[string]any
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Session is one recorded game session and its derived artifacts.
type Session struct {
	ID              uuid.UUID
	CampaignID      uuid.UUID
	GuildID         string
	VoiceChannelID  string
	Status          string
	AudioKey        string
	ChunkPrefix     string
	Transcript      string
	Notes           string
	DurationSeconds int
	StartedAt       time.Time
	EndedAt         *time.Time
}

// SessionSearchResult is a ranked snippet from a completed campaign session.
type SessionSearchResult struct {
	ID        uuid.UUID
	StartedAt time.Time
	Snippet   string
}

// SessionParticipant records a Discord user who spoke during a session.
type SessionParticipant struct {
	UserID       string
	DisplayName  string
	FirstSpokeAt time.Time
	LastSpokeAt  time.Time
}

// Reminder is a recurring session reminder.
type Reminder struct {
	ID         uuid.UUID
	CampaignID uuid.UUID
	GuildID    string
	ChannelID  string
	Schedule   string
	NextRun    time.Time
	CreatedAt  time.Time
}

// ProposalAction enumerates what an approved state proposal does to canon.
type ProposalAction string

const (
	// ActionCreateEntity creates a new world entity from the proposal.
	ActionCreateEntity ProposalAction = "create_entity"
	// ActionUpdateEntity updates an existing world entity from the proposal.
	ActionUpdateEntity ProposalAction = "update_entity"
)

// Proposal status values.
const (
	ProposalPending  = "pending"
	ProposalApproved = "approved"
	ProposalRejected = "rejected"
)

// StateProposal is an AI- or human-suggested change to the persistent campaign
// world, awaiting DM review. It is NEVER canon until approved: approving
// atomically applies Patch to world_entities and marks the row approved;
// rejecting leaves canon untouched. This is the guardrail that keeps AI-derived
// information from silently becoming campaign truth.
type StateProposal struct {
	ID          uuid.UUID
	CampaignID  uuid.UUID
	SessionID   *uuid.UUID // nil for proposals not tied to a specific recording
	Action      ProposalAction
	EntityKind  WorldEntityKind
	EntityID    *uuid.UUID // set for update_entity; nil for create_entity
	EntityName  string
	Patch       map[string]any // structured proposed data (description + metadata)
	Explanation string
	Evidence    string
	Confidence  float64
	Status      string
	ReviewedBy  *string
	CreatedAt   time.Time
	ReviewedAt  *time.Time
}

// Description extracts the proposed description string from Patch, if present.
func (p StateProposal) Description() string {
	if p.Patch == nil {
		return ""
	}
	if d, ok := p.Patch["description"].(string); ok {
		return d
	}
	return ""
}

// AppliedChange describes the canonical record an approved proposal created or
// updated, independent of whether it targeted a world entity or a player
// character. It lets callers (e.g. the review handler) react uniformly — post a
// message, enqueue the right embedding — without re-deriving the target type.
type AppliedChange struct {
	CampaignID  uuid.UUID
	SourceKind  string    // CanonSourceEntity | CanonSourceCharacter
	SourceID    uuid.UUID // the world_entities.id or player_characters.id affected
	DisplayName string    // entity/character name, for user-facing messages
}
