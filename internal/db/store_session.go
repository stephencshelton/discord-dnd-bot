package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// --- Sessions ---

// CreateSession starts a recording session row.
func (s *Store) CreateSession(ctx context.Context, campaignID uuid.UUID, guildID, voiceChannelID string) (*Session, error) {
	var sess Session
	err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO sessions (campaign_id, guild_id, voice_channel_id, status)
		 VALUES ($1,$2,$3,'recording')
		 RETURNING id, campaign_id, guild_id, COALESCE(voice_channel_id,''), status, started_at`,
		campaignID, guildID, voiceChannelID).
		Scan(&sess.ID, &sess.CampaignID, &sess.GuildID, &sess.VoiceChannelID, &sess.Status, &sess.StartedAt)
	return &sess, err
}

// GetActiveSession returns the currently-recording session for a guild.
func (s *Store) GetActiveSession(ctx context.Context, guildID string) (*Session, error) {
	var sess Session
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, campaign_id, guild_id, COALESCE(voice_channel_id,''), status, started_at
		 FROM sessions WHERE guild_id=$1 AND status='recording'
		 ORDER BY started_at DESC LIMIT 1`, guildID).
		Scan(&sess.ID, &sess.CampaignID, &sess.GuildID, &sess.VoiceChannelID, &sess.Status, &sess.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &sess, err
}

// EndSession marks a session as processing and records duration + audio key.
func (s *Store) EndSession(ctx context.Context, id uuid.UUID, audioKey string, durationSeconds int) error {
	_, err := s.db.Pool.Exec(ctx,
		`UPDATE sessions SET status='processing', audio_key=$2, duration_seconds=$3, ended_at=now() WHERE id=$1`,
		id, audioKey, durationSeconds)
	return err
}

// SetSessionResult stores the transcript + notes and marks the session complete.
func (s *Store) SetSessionResult(ctx context.Context, id uuid.UUID, transcript, notes, status string) error {
	_, err := s.db.Pool.Exec(ctx,
		`UPDATE sessions SET transcript=$2, notes=$3, status=$4 WHERE id=$1`,
		id, transcript, notes, status)
	return err
}

// GetSession fetches a full session record.
func (s *Store) GetSession(ctx context.Context, id uuid.UUID) (*Session, error) {
	var sess Session
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, campaign_id, guild_id, COALESCE(voice_channel_id,''), status,
		        COALESCE(audio_key,''), COALESCE(transcript,''), COALESCE(notes,''),
		        duration_seconds, started_at, ended_at
		 FROM sessions WHERE id=$1`, id).
		Scan(&sess.ID, &sess.CampaignID, &sess.GuildID, &sess.VoiceChannelID, &sess.Status,
			&sess.AudioKey, &sess.Transcript, &sess.Notes, &sess.DurationSeconds, &sess.StartedAt, &sess.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &sess, err
}

// RecentCompletedNotes returns up to `limit` most-recent completed session notes
// for a campaign, oldest first (so a recap reads chronologically).
func (s *Store) RecentCompletedNotes(ctx context.Context, campaignID uuid.UUID, limit int) ([]string, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT COALESCE(notes,'') FROM sessions
		 WHERE campaign_id=$1 AND status='complete' AND notes <> ''
		 ORDER BY started_at DESC LIMIT $2`, campaignID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to chronological (oldest first).
	for i, j := 0, len(notes)-1; i < j; i, j = i+1, j-1 {
		notes[i], notes[j] = notes[j], notes[i]
	}
	return notes, nil
}

// SearchSessions finds completed sessions whose transcript or notes match a
// campaign-scoped query, returning a highlighted snippet rather than the full
// transcript.
func (s *Store) SearchSessions(ctx context.Context, campaignID uuid.UUID, query string, limit int) ([]SessionSearchResult, error) {
	if limit < 1 {
		limit = 10
	}
	if limit > 20 {
		limit = 20
	}
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, started_at,
		        ts_headline('simple', coalesce(notes, '') || ' ' || coalesce(transcript, ''),
		                    plainto_tsquery('simple', $2),
		                    'MaxWords=36,MinWords=16,StartSel=**,StopSel=**')
		 FROM sessions
		 WHERE campaign_id=$1 AND status='complete' AND search_vector @@ plainto_tsquery('simple', $2)
		 ORDER BY ts_rank(search_vector, plainto_tsquery('simple', $2)) DESC, started_at DESC
		 LIMIT $3`, campaignID, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []SessionSearchResult
	for rows.Next() {
		var result SessionSearchResult
		if err := rows.Scan(&result.ID, &result.StartedAt, &result.Snippet); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

// --- Session participants ---

// RecordParticipant upserts a speaker for a session: the first call sets
// first_spoke_at, later calls bump last_spoke_at and refresh the display name.
// Best-effort telemetry; callers typically ignore the error.
func (s *Store) RecordParticipant(ctx context.Context, sessionID uuid.UUID, userID, displayName string) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO session_participants (session_id, user_id, display_name, first_spoke_at, last_spoke_at)
		 VALUES ($1,$2,$3,now(),now())
		 ON CONFLICT (session_id, user_id) DO UPDATE
		   SET last_spoke_at = now(),
		       display_name = CASE WHEN excluded.display_name <> '' THEN excluded.display_name
		                           ELSE session_participants.display_name END`,
		sessionID, userID, displayName)
	return err
}

// ListParticipants returns everyone who spoke during a session, ordered by when
// they first spoke.
func (s *Store) ListParticipants(ctx context.Context, sessionID uuid.UUID) ([]SessionParticipant, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT user_id, display_name, first_spoke_at, last_spoke_at
		 FROM session_participants WHERE session_id=$1
		 ORDER BY first_spoke_at ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionParticipant
	for rows.Next() {
		var p SessionParticipant
		if err := rows.Scan(&p.UserID, &p.DisplayName, &p.FirstSpokeAt, &p.LastSpokeAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- Reminders ---

// CreateReminder upserts the reminder for a campaign (one active reminder each).
func (s *Store) CreateReminder(ctx context.Context, r Reminder) (*Reminder, error) {
	// Replace any existing reminder for the campaign to keep "one schedule" semantics.
	if _, err := s.db.Pool.Exec(ctx, `DELETE FROM reminders WHERE campaign_id=$1`, r.CampaignID); err != nil {
		return nil, err
	}
	err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO reminders (campaign_id, guild_id, channel_id, schedule, next_run)
		 VALUES ($1,$2,$3,$4,$5)
		 RETURNING id, created_at`,
		r.CampaignID, r.GuildID, r.ChannelID, r.Schedule, r.NextRun).
		Scan(&r.ID, &r.CreatedAt)
	return &r, err
}

// GetReminder returns the reminder for a campaign.
func (s *Store) GetReminder(ctx context.Context, campaignID uuid.UUID) (*Reminder, error) {
	var r Reminder
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, campaign_id, guild_id, channel_id, schedule, next_run, created_at
		 FROM reminders WHERE campaign_id=$1`, campaignID).
		Scan(&r.ID, &r.CampaignID, &r.GuildID, &r.ChannelID, &r.Schedule, &r.NextRun, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

// DeleteReminder removes the reminder for a campaign.
func (s *Store) DeleteReminder(ctx context.Context, campaignID uuid.UUID) error {
	_, err := s.db.Pool.Exec(ctx, `DELETE FROM reminders WHERE campaign_id=$1`, campaignID)
	return err
}

// DueReminders returns reminders whose next_run is at or before now.
func (s *Store) DueReminders(ctx context.Context, now time.Time) ([]Reminder, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, campaign_id, guild_id, channel_id, schedule, next_run, created_at
		 FROM reminders WHERE next_run <= $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		var r Reminder
		if err := rows.Scan(&r.ID, &r.CampaignID, &r.GuildID, &r.ChannelID, &r.Schedule, &r.NextRun, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimReminder atomically advances a reminder's next_run only if it still
// matches expected, returning true if this caller won the claim. Lets the
// reminder loop run across multiple gateway replicas without duplicate posts.
func (s *Store) ClaimReminder(ctx context.Context, id uuid.UUID, expected, next time.Time) (bool, error) {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE reminders SET next_run=$3 WHERE id=$1 AND next_run=$2`, id, expected, next)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// --- Feedback ---

// AddFeedback stores a feedback submission.
func (s *Store) AddFeedback(ctx context.Context, guildID, userID, body string) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO feedback (guild_id, discord_user_id, body) VALUES ($1,$2,$3)`,
		guildID, userID, body)
	return err
}
