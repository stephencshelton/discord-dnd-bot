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
)

// WorldEntity is a generic worldbuilding record (NPC/location/faction/quest).
type WorldEntity struct {
	ID          uuid.UUID
	CampaignID  uuid.UUID
	Kind        WorldEntityKind
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
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
