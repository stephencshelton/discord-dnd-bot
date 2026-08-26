// Package queue is a minimal Redis-backed job queue handing work from the
// gateway (which must stay responsive to Discord) to the worker pool (slow AI
// work). Redis lists keep the two deployments decoupled, scaling
// independently in Kubernetes.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
)

// JobType enumerates the kinds of asynchronous work the worker performs.
type JobType string

const (
	// JobTranscribeSession: transcribe recorded audio then generate session notes.
	JobTranscribeSession JobType = "transcribe_session"
	// JobGenerateArt: produce AI scene art from a prompt.
	JobGenerateArt JobType = "generate_art"
	// JobReindexCampaign: (re)embed all completed session notes for a campaign
	// so grounded /ask retrieval works over historical sessions.
	JobReindexCampaign JobType = "reindex_campaign"
)

// Job is a unit of asynchronous work. Payload is type-specific JSON.
type Job struct {
	Type    JobType         `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// TranscribeSessionPayload carries the session to process.
type TranscribeSessionPayload struct {
	SessionID string `json:"session_id"`
	GuildID   string `json:"guild_id"`
}

// GenerateArtPayload carries an art request.
type GenerateArtPayload struct {
	GuildID   string `json:"guild_id"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	Prompt    string `json:"prompt"`
}

// ReindexCampaignPayload carries a request to (re)embed a campaign's completed
// session notes for grounded /ask retrieval.
type ReindexCampaignPayload struct {
	CampaignID string `json:"campaign_id"`
	GuildID    string `json:"guild_id"`
	ChannelID  string `json:"channel_id"` // where to post a completion notice
}

const queueKey = "discord-dnd-bot:jobs"

// Queue wraps a Redis client.
type Queue struct {
	rdb *redis.Client
}

// New connects to Redis.
func New(cfg config.RedisConfig) *Queue {
	return &Queue{rdb: redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})}
}

// Ping verifies connectivity (used by health checks).
func (q *Queue) Ping(ctx context.Context) error { return q.rdb.Ping(ctx).Err() }

// Close releases the client.
func (q *Queue) Close() error { return q.rdb.Close() }

// Enqueue pushes a typed job. The gateway calls this; it returns immediately.
func (q *Queue) Enqueue(ctx context.Context, jobType JobType, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	body, err := json.Marshal(Job{Type: jobType, Payload: raw})
	if err != nil {
		return err
	}
	return q.rdb.LPush(ctx, queueKey, body).Err()
}

// Dequeue blocks up to timeout for the next job. Returns (nil, nil) on timeout.
func (q *Queue) Dequeue(ctx context.Context, timeout time.Duration) (*Job, error) {
	res, err := q.rdb.BRPop(ctx, timeout, queueKey).Result()
	if err == redis.Nil {
		return nil, nil // timed out, no job
	}
	if err != nil {
		return nil, err
	}
	if len(res) != 2 {
		return nil, fmt.Errorf("unexpected BRPOP result length %d", len(res))
	}
	var job Job
	if err := json.Unmarshal([]byte(res[1]), &job); err != nil {
		return nil, err
	}
	return &job, nil
}
