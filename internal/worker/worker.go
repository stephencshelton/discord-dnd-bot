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
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
	"github.com/stephencshelton/discord-dnd-bot/internal/storage"
)

// Worker holds the dependencies needed to process jobs.
type Worker struct {
	cfg     *config.Config
	log     *slog.Logger
	queue   *queue.Queue
	store   *db.Store
	ai      *litellm.Client
	storage *storage.Store
	discord *discordgo.Session // REST-only client for posting results
}

// New builds a worker. The discord session is used only for REST calls
// (posting messages / uploading files); it is never opened as a gateway.
func New(cfg *config.Config, log *slog.Logger, q *queue.Queue, store *db.Store, ai *litellm.Client, st *storage.Store) (*Worker, error) {
	dg, err := discordgo.New("Bot " + cfg.Discord.Token)
	if err != nil {
		return nil, fmt.Errorf("create discord rest client: %w", err)
	}
	return &Worker{
		cfg:     cfg,
		log:     log,
		queue:   q,
		store:   store,
		ai:      ai,
		storage: st,
		discord: dg,
	}, nil
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

// process dispatches a single job and records metrics.
func (w *Worker) process(ctx context.Context, job *queue.Job) {
	start := time.Now()
	jobCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	var err error
	switch job.Type {
	case queue.JobTranscribeSession:
		err = w.handleTranscribeSession(jobCtx, job.Payload)
	case queue.JobGenerateArt:
		err = w.handleGenerateArt(jobCtx, job.Payload)
	case queue.JobReindexCampaign:
		err = w.handleReindexCampaign(jobCtx, job.Payload)
	default:
		err = fmt.Errorf("unknown job type %q", job.Type)
	}

	status := "ok"
	if err != nil {
		status = "error"
		w.log.Error("job failed", "type", job.Type, "err", err)
	}
	metrics.JobsProcessed.WithLabelValues(string(job.Type), status).Inc()
	metrics.JobDuration.WithLabelValues(string(job.Type)).Observe(time.Since(start).Seconds())
}

// unmarshal is a tiny helper to decode a job payload.
func unmarshal[T any](raw json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(raw, &v)
	return v, err
}
