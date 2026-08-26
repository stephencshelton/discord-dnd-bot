// Command worker runs the asynchronous job processor. It consumes jobs from
// Redis (enqueued by the gateway) and performs transcription, summarization, and
// image generation via LiteLLM. No Discord gateway connection — only a REST
// client for posting results — so it scales purely on job throughput.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/httpserver"
	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
	"github.com/stephencshelton/discord-dnd-bot/internal/storage"
	"github.com/stephencshelton/discord-dnd-bot/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	cfg.Service = "worker"
	log := logging.New(cfg.Service, cfg.LogLevel)

	if cfg.Discord.Token == "" {
		log.Error("DISCORD_TOKEN is required (for posting results)")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := db.Connect(ctx, cfg.Database)
	if err != nil {
		log.Error("connect database", "err", err)
		os.Exit(1)
	}
	defer database.Close()
	store := db.NewStore(database)

	q := queue.New(cfg.Redis)
	defer q.Close()

	st, err := storage.New(ctx, cfg.Storage)
	if err != nil {
		log.Error("init storage", "err", err)
		os.Exit(1)
	}

	ai := litellm.New(cfg.LiteLLM.BaseURL, cfg.LiteLLM.APIKey, cfg.LiteLLM.RequestTimeout)

	w, err := worker.New(cfg, log, q, store, ai, st)
	if err != nil {
		log.Error("build worker", "err", err)
		os.Exit(1)
	}

	// Health/metrics endpoint. Readiness = Redis reachable.
	health := httpserver.New(cfg.HTTPAddr, func(hctx context.Context) error {
		return q.Ping(hctx)
	})
	go func() {
		if err := health.Start(ctx); err != nil {
			log.Error("health server", "err", err)
		}
	}()

	log.Info("worker running", "http", cfg.HTTPAddr)
	w.Run(ctx) // blocks until ctx is cancelled

	log.Info("worker exited")
}
