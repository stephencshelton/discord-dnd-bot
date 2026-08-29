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

	// raw is the exact serialized bytes of this job as they live on the Redis
	// processing list (set by Dequeue). Ack/Requeue use it to LREM the precise
	// element. Not serialized — it's transient in-process bookkeeping.
	raw string `json:"-"`
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

const (
	// queueKey is the main pending-jobs list (LPUSH to enqueue, the tail is
	// consumed oldest-first).
	queueKey = "discord-dnd-bot:jobs"
	// processingKey holds jobs a worker has dequeued but not yet finished. A job
	// is atomically moved here by Dequeue (BRPOPLPUSH) and removed by Ack once it
	// completes (success or terminal drop). This is the "reliable queue" pattern:
	// if a worker is hard-killed mid-job, the job survives here and the reaper
	// returns it to the main queue, so a job is never silently lost.
	processingKey = "discord-dnd-bot:jobs:processing"
)

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

// Depth returns the number of jobs currently OUTSTANDING — pending plus
// in-flight (on the processing list). Used to export a backlog gauge for
// dashboards and autoscaling. Counting in-flight jobs matters: a long-running
// job leaves the pending list the instant it's dequeued, so pending-only depth
// would read zero while work is still happening and mislead scale-down.
func (q *Queue) Depth(ctx context.Context) (int64, error) {
	pending, err := q.rdb.LLen(ctx, queueKey).Result()
	if err != nil {
		return 0, err
	}
	inflight, err := q.rdb.LLen(ctx, processingKey).Result()
	if err != nil {
		return 0, err
	}
	return pending + inflight, nil
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

// Requeue returns a previously-dequeued job to the pending queue with its
// Attempt counter incremented, so a transient failure can be retried. It also
// removes the job's in-flight copy from the processing list (the reliable-queue
// bookkeeping), so the job exists in exactly one place afterward. The job goes
// to the head (LPush) so it's picked up again promptly.
func (q *Queue) Requeue(ctx context.Context, job *Job) error {
	// Remove the in-flight copy first (best-effort; if this fails the reaper
	// would eventually re-queue the same job, which retry logic tolerates).
	if job.raw != "" {
		if err := q.rdb.LRem(ctx, processingKey, 1, job.raw).Err(); err != nil {
			metrics.ComponentError("redis", "requeue_ack")
		}
	}
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

// Ack removes a finished job's in-flight copy from the processing list. It MUST
// be called once a job reaches a terminal state (success, or a permanent/
// exhausted-retries drop) so the reliable queue doesn't later re-run it via the
// reaper. Idempotent and best-effort: a job with no recorded raw body (e.g. an
// externally-injected job) is a no-op.
func (q *Queue) Ack(ctx context.Context, job *Job) error {
	if job == nil || job.raw == "" {
		return nil
	}
	if err := q.rdb.LRem(ctx, processingKey, 1, job.raw).Err(); err != nil {
		metrics.ComponentError("redis", "ack")
		return err
	}
	return nil
}

// Dequeue blocks up to timeout for the next job, atomically moving it from the
// pending queue to the processing list (BRPOPLPUSH). The job stays on the
// processing list — visible to Depth and recoverable by the reaper — until the
// worker calls Ack (done) or Requeue (retry). Returns (nil, nil) on timeout.
func (q *Queue) Dequeue(ctx context.Context, timeout time.Duration) (*Job, error) {
	body, err := q.rdb.BRPopLPush(ctx, queueKey, processingKey, timeout).Result()
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
	var job Job
	if err := json.Unmarshal([]byte(body), &job); err != nil {
		metrics.ComponentError("redis", "dequeue_decode")
		// Remove the un-decodable element so it doesn't wedge the processing list.
		_ = q.rdb.LRem(ctx, processingKey, 1, body).Err()
		return nil, err
	}
	job.raw = body // remember the exact bytes so Ack/Requeue can LREM this element
	return &job, nil
}

// ReapOrphans returns every job left on the processing list to the pending
// queue. A job lands there when a worker dies mid-processing (hard kill, node
// loss) without Ack/Requeue. Because the worker fleet is small (default max 1
// replica) and drains in-flight jobs on graceful shutdown, anything still on the
// processing list at reap time is genuinely orphaned, so moving it all back to
// pending is safe and recovers otherwise-lost work. Call on worker startup (and
// optionally periodically). Returns how many jobs were recovered.
//
// NOTE: with multiple concurrently-running workers this would also re-queue
// jobs another worker is actively processing; that's acceptable only because
// the worker runs at low/one replica and every handler is idempotent (dedup on
// transcribe/extract/embed). If the worker fleet is ever scaled to many always-
// on replicas, switch to a per-job visibility-timeout reaper instead.
func (q *Queue) ReapOrphans(ctx context.Context) (int, error) {
	recovered := 0
	for {
		// Atomically move one element from processing back to the pending head.
		body, err := q.rdb.RPopLPush(ctx, processingKey, queueKey).Result()
		if err == redis.Nil {
			return recovered, nil // processing list drained
		}
		if err != nil {
			metrics.ComponentError("redis", "reap")
			return recovered, err
		}
		recovered++
		metrics.JobsRetried.WithLabelValues("reaped").Inc()
		_ = body
	}
}
