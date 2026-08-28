// Package queue is a minimal Redis-backed job queue handing work from the
// gateway (which must stay responsive to Discord) to the worker pool (slow AI
// work). Redis lists keep the two deployments decoupled, scaling
// independently in Kubernetes.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
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
	// JobExtractState: after a session's notes are generated, propose campaign
	// world-state changes for DM review. Runs as its own job (decoupled from
	// transcription) so a failure here never fails the session and it can be
	// retried/reprocessed independently.
	JobExtractState JobType = "extract_state"
	// JobEmbedCanon: (re)embed a single canon record (a world entity or player
	// character) so grounded /ask retrieval can surface curated campaign facts
	// alongside session transcripts. Enqueued whenever such a record is created
	// or updated; a delete removes the embedding directly (no AI call needed).
	JobEmbedCanon JobType = "embed_canon"
)

// Job is a unit of asynchronous work. Payload is type-specific JSON.
type Job struct {
	Type    JobType         `json:"type"`
	Payload json.RawMessage `json:"payload"`
	// CorrelationID ties this job back to the gateway interaction that enqueued
	// it, so logs across gateway -> queue -> worker can be joined. Optional.
	CorrelationID string `json:"correlation_id,omitempty"`
	// Attempt counts how many times this job has been dequeued for processing.
	// It starts at 0 on first enqueue and is incremented each time the worker
	// requeues the job after a retryable failure. It bounds retries so a job
	// that keeps failing can't loop forever.
	Attempt int `json:"attempt,omitempty"`
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

// ExtractStatePayload carries a request to extract proposed campaign-state
// changes from a completed session's transcript + notes.
type ExtractStatePayload struct {
	SessionID string `json:"session_id"`
	GuildID   string `json:"guild_id"`
	// Notify, when true, posts a short DM/channel nudge that proposals are ready
	// to review. Set for the automatic post-session run; a manual reprocess can
	// leave it false.
	Notify bool `json:"notify,omitempty"`
}

// EmbedCanonPayload carries a request to (re)embed a single canon record for
// grounded /ask retrieval.
type EmbedCanonPayload struct {
	CampaignID string `json:"campaign_id"`
	// SourceKind is "entity" (world_entities) or "character" (player_characters).
	SourceKind string `json:"source_kind"`
	SourceID   string `json:"source_id"`
}

const queueKey = "discord-dnd-bot:jobs"

// ErrPermanent wraps a job failure that must NOT be retried — the input is
// fundamentally bad (unparseable payload, missing session, no audio captured,
// empty transcript) so re-running the job would only fail again. Handlers wrap
// such errors with Permanent(); the worker checks with errors.Is and skips the
// requeue. Everything else (transient LiteLLM/storage/network errors) is treated
// as retryable.
var ErrPermanent = errors.New("permanent job failure")

// Permanent marks err as a non-retryable failure. It returns nil when err is nil
// so it can wrap a call site cheaply. Use it in a job handler for failures where
// retrying is pointless (bad input, missing data).
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrPermanent, err)
}

// IsPermanent reports whether err (or anything it wraps) is a permanent,
// non-retryable failure.
func IsPermanent(err error) bool { return errors.Is(err, ErrPermanent) }

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

// Depth returns the number of jobs currently waiting in the queue. Used to
// export a backlog gauge for dashboards and HPA.
func (q *Queue) Depth(ctx context.Context) (int64, error) {
	return q.rdb.LLen(ctx, queueKey).Result()
}

// Enqueue pushes a typed job. The gateway calls this; it returns immediately.
// A correlation ID (from ctx, if present) is stamped onto the job so the
// resulting worker processing can be traced back to the originating request.
func (q *Queue) Enqueue(ctx context.Context, jobType JobType, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	body, err := json.Marshal(Job{
		Type:          jobType,
		Payload:       raw,
		CorrelationID: logging.CorrelationIDFromContext(ctx),
	})
	if err != nil {
		return err
	}
	if err := q.rdb.LPush(ctx, queueKey, body).Err(); err != nil {
		metrics.ComponentError("redis", "enqueue")
		return err
	}
	metrics.JobsEnqueued.WithLabelValues(string(jobType)).Inc()
	return nil
}

// Requeue pushes a previously-dequeued job back onto the queue with its Attempt
// counter incremented, so a transient failure (a LiteLLM/storage blip, a pod
// crash mid-job) can be retried rather than lost. Because Dequeue uses BRPOP —
// which atomically removes the job from Redis — a failed job would otherwise
// vanish; Requeue is how the worker puts it back. The job goes to the head
// (LPush, same as Enqueue) so it will be picked up again promptly.
func (q *Queue) Requeue(ctx context.Context, job *Job) error {
	job.Attempt++
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if err := q.rdb.LPush(ctx, queueKey, body).Err(); err != nil {
		metrics.ComponentError("redis", "requeue")
		return err
	}
	metrics.JobsRetried.WithLabelValues(string(job.Type)).Inc()
	return nil
}

// Dequeue blocks up to timeout for the next job. Returns (nil, nil) on timeout.
func (q *Queue) Dequeue(ctx context.Context, timeout time.Duration) (*Job, error) {
	res, err := q.rdb.BRPop(ctx, timeout, queueKey).Result()
	if err == redis.Nil {
		return nil, nil // timed out, no job
	}
	if err != nil {
		// A cancelled context during shutdown is expected, not an error worth
		// counting as a component failure.
		if ctx.Err() == nil {
			metrics.ComponentError("redis", "dequeue")
		}
		return nil, err
	}
	if len(res) != 2 {
		metrics.ComponentError("redis", "dequeue_decode")
		return nil, fmt.Errorf("unexpected BRPOP result length %d", len(res))
	}
	var job Job
	if err := json.Unmarshal([]byte(res[1]), &job); err != nil {
		metrics.ComponentError("redis", "dequeue_decode")
		return nil, err
	}
	return &job, nil
}
