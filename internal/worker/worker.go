// Package worker consumes jobs from the queue and performs the slow AI work
// (transcription, summarization, image generation) the gateway offloads.
// Stateless and horizontally scalable: Redis BRPOP distributes jobs across
// as many replicas as throughput requires.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
	"github.com/stephencshelton/discord-dnd-bot/internal/storage"
)

// Worker holds the dependencies needed to process jobs.
type Worker struct {
	cfg   *config.Config
	log   *slog.Logger
	queue *queue.Queue
	store *db.Store
	ai    *litellm.Client
	// transcribeAI is a litellm client with a long HTTP timeout for audio
	// transcription. The shared `ai` client uses the short RequestTimeout that's
	// right for chat/embed but would abort a multi-hour Whisper transcription.
	transcribeAI *litellm.Client
	storage      *storage.Store
	discord      rest.Rest // REST-only client for posting results (never a gateway)
}

// New builds a worker. The discord client is used only for REST calls
// (posting messages / uploading files); it is never opened as a gateway.
func New(cfg *config.Config, log *slog.Logger, q *queue.Queue, store *db.Store, ai *litellm.Client, st *storage.Store) (*Worker, error) {
	// A separate client for transcription: its HTTP timeout must cover a whole
	// (possibly multi-hour) recording, so it tracks the transcribe job timeout.
	transcribeAI := litellm.New(cfg.LiteLLM.BaseURL, cfg.LiteLLM.APIKey, cfg.Worker.TranscribeJobTimeout, litellm.WithLogger(log))
	return &Worker{
		cfg:          cfg,
		log:          log,
		queue:        q,
		store:        store,
		ai:           ai,
		transcribeAI: transcribeAI,
		storage:      st,
		discord:      rest.New(rest.NewClient(cfg.Discord.Token)),
	}, nil
}

// sendMessage posts a message (optionally with files) to a channel, converting
// the string channel ID to a snowflake at the disgo boundary.
func (w *Worker) sendMessage(channelID string, m discord.MessageCreate) error {
	cid, err := snowflake.Parse(channelID)
	if err != nil {
		return fmt.Errorf("invalid channel id %q: %w", channelID, err)
	}
	_, err = w.discord.CreateMessage(cid, m)
	return err
}

// Run loops until the context is cancelled, dispatching jobs to a bounded pool
// of in-pod goroutines (capped by cfg.Worker.Concurrency so memory stays
// predictable; HPA on queue depth scales beyond one pod). On shutdown it stops
// accepting new jobs and drains in-flight work.
func (w *Worker) Run(ctx context.Context) {
	concurrency := w.cfg.Worker.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	w.log.Info("worker started", "concurrency", concurrency)

	// Recover any jobs orphaned on the processing list by a previous worker that
	// was hard-killed mid-job (the reliable-queue safety net). Best-effort: a
	// Redis blip here just means the next worker start reaps them instead.
	if n, err := w.queue.ReapOrphans(ctx); err != nil {
		w.log.Warn("reap orphaned jobs on startup failed", "err", err)
	} else if n > 0 {
		w.log.Info("recovered orphaned jobs from processing list", "count", n)
	}

	// Periodically export queue depth so dashboards/HPA see backlog directly.
	go w.reportQueueDepth(ctx, 15*time.Second)

	// sem bounds how many jobs run in parallel within this pod.
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker stopping; draining in-flight jobs")
			wg.Wait()
			return
		default:
		}

		job, err := w.queue.Dequeue(ctx, 5*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return
			}
			w.log.Error("dequeue", "err", err)
			time.Sleep(time.Second)
			continue
		}
		if job == nil {
			continue // timeout, poll again
		}

		// Acquire a slot; blocks at capacity, applying backpressure.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// Already popped this job — process it synchronously rather than drop it.
			w.process(ctx, job)
			wg.Wait()
			return
		}

		wg.Add(1)
		go func(j *queue.Job) {
			defer wg.Done()
			defer func() { <-sem }()
			w.process(ctx, j)
		}(job)
	}
}

// process dispatches a single job and records metrics. It recovers from panics
// in job handlers so a single bad job can never crash the worker process (and
// take all other in-flight jobs down with it).
func (w *Worker) process(ctx context.Context, job *queue.Job) {
	start := time.Now()

	// Derive a correlation ID for this job (reuse one carried on the payload if
	// present) and a child logger tagged with job context, so every log line
	// for this job — and the originating gateway interaction — can be joined.
	corrID := job.CorrelationID
	if corrID == "" {
		corrID = logging.NewCorrelationID()
	}
	log := w.log.With(
		logging.CorrelationIDField, corrID,
		"job_type", string(job.Type),
	)
	ctx = logging.WithLogger(logging.WithCorrelationID(ctx, w.log, corrID), log)

	// Transcription of a long recording can take much longer than other jobs
	// (CPU Whisper runs below realtime), so give it its own generous timeout.
	timeout := w.cfg.Worker.JobTimeout
	if job.Type == queue.JobTranscribeSession {
		timeout = w.cfg.Worker.TranscribeJobTimeout
	}
	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	status := "ok"
	defer func() {
		if r := recover(); r != nil {
			status = "panic"
			metrics.PanicsRecovered.WithLabelValues("job").Inc()
			log.Error("job panicked; recovered",
				"panic", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()))
		}
		metrics.JobsProcessed.WithLabelValues(string(job.Type), status).Inc()
		dur := time.Since(start)
		metrics.JobDuration.WithLabelValues(string(job.Type)).Observe(dur.Seconds())
		log.Info("job finished", "status", status, "duration_ms", dur.Milliseconds())
	}()

	log.Info("job started")

	var err error
	switch job.Type {
	case queue.JobTranscribeSession:
		err = w.handleTranscribeSession(jobCtx, job.Payload, w.isLastAttempt(job))
	case queue.JobGenerateArt:
		err = w.handleGenerateArt(jobCtx, job.Payload)
	case queue.JobReindexCampaign:
		err = w.handleReindexCampaign(jobCtx, job.Payload)
	case queue.JobExtractState:
		err = w.handleExtractState(jobCtx, job.Payload)
	case queue.JobEmbedCanon:
		err = w.handleEmbedCanon(jobCtx, job.Payload)
	default:
		err = fmt.Errorf("unknown job type %q", job.Type)
	}

	if err != nil {
		status = "error"
		log.Error("job failed", "err", err)
		w.maybeRequeue(ctx, job, log, err)
		return
	}

	// Success: remove the job's in-flight copy from the processing list so the
	// reliable queue considers it done and the reaper won't re-run it. Uses a
	// short independent deadline so a slow Redis can't wedge the worker loop.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if aerr := w.queue.Ack(actx, job); aerr != nil {
		log.Error("failed to ack completed job; reaper may re-run it", "err", aerr)
	}
}

// isLastAttempt reports whether a retryable failure on this job would be its
// final one (retries already exhausted). Handlers use it to decide whether to
// surface a user-visible "failed" message now versus staying quiet while a
// retry is still coming.
func (w *Worker) isLastAttempt(job *queue.Job) bool {
	return job.Attempt >= w.cfg.Worker.MaxRetries
}

// maybeRequeue decides what to do with a failed job. Permanent failures (bad
// input, missing data — flagged with queue.Permanent) are dropped immediately
// since a retry would only fail again. Transient failures are requeued with an
// incremented attempt count until cfg.Worker.MaxRetries is exhausted, after
// which the job is dropped. Requeuing uses the worker's run context (not the
// per-job timeout context, which may already be cancelled).
func (w *Worker) maybeRequeue(ctx context.Context, job *queue.Job, log *slog.Logger, jobErr error) {
	// ackDrop removes a terminally-dropped job's in-flight copy so the reaper
	// won't resurrect it. Used for permanent failures and exhausted retries —
	// NOT for the shutdown case, where we deliberately leave it on the processing
	// list so a restarted worker's reaper picks it back up.
	ackDrop := func() {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := w.queue.Ack(actx, job); err != nil {
			log.Error("failed to ack dropped job", "err", err)
		}
	}

	if queue.IsPermanent(jobErr) {
		log.Warn("job failed permanently; not retrying", "attempt", job.Attempt)
		metrics.JobsDropped.WithLabelValues(string(job.Type), "permanent").Inc()
		ackDrop()
		return
	}
	if job.Attempt >= w.cfg.Worker.MaxRetries {
		log.Error("job exhausted retries; dropping",
			"attempt", job.Attempt, "max_retries", w.cfg.Worker.MaxRetries)
		metrics.JobsDropped.WithLabelValues(string(job.Type), "max_retries_exceeded").Inc()
		ackDrop()
		return
	}
	// Don't try to requeue while shutting down — leave the job on the processing
	// list so the restarted worker's reaper recovers it (no longer "lost").
	if ctx.Err() != nil {
		log.Warn("worker shutting down; leaving failed job for reaper", "attempt", job.Attempt)
		metrics.JobsDropped.WithLabelValues(string(job.Type), "shutdown").Inc()
		return
	}
	// Give the requeue a short, independent deadline so a slow/broken Redis
	// can't wedge the worker loop. Requeue also removes the in-flight copy.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := w.queue.Requeue(rctx, job); err != nil {
		// Requeue failed — but the job is still on the processing list, so the
		// reaper will recover it on the next worker start. Not lost.
		log.Error("failed to requeue job; left on processing list for reaper", "attempt", job.Attempt, "err", err)
		metrics.JobsDropped.WithLabelValues(string(job.Type), "requeue_failed").Inc()
		return
	}
	log.Warn("requeued failed job for retry",
		"next_attempt", job.Attempt, "max_retries", w.cfg.Worker.MaxRetries)
}

// unmarshal is a tiny helper to decode a job payload.
func unmarshal[T any](raw json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(raw, &v)
	return v, err
}

// reportQueueDepth polls the queue length on an interval and publishes it as a
// gauge until the context is cancelled. Best effort: a transient error is logged
// at debug and retried on the next tick.
func (w *Worker) reportQueueDepth(ctx context.Context, every time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			metrics.PanicsRecovered.WithLabelValues("goroutine").Inc()
			w.log.Error("queue-depth reporter panicked; recovered",
				"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
		}
	}()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			depth, err := w.queue.Depth(ctx)
			if err != nil {
				if ctx.Err() == nil {
					w.log.Debug("queue depth poll failed", "err", err)
				}
				continue
			}
			metrics.QueueDepth.Set(float64(depth))
		}
	}
}
