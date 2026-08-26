// Command gateway runs the Discord-facing service: it maintains the gateway
// connection, serves slash commands, records voice, and enqueues heavy work to
// the worker pool. Horizontally scalable, though Discord sharding caps useful
// replica count (one shard set per bot).
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/gateway"
	"github.com/stephencshelton/discord-dnd-bot/internal/httpserver"
	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
	"github.com/stephencshelton/discord-dnd-bot/internal/storage"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	cfg.Service = "gateway"
	log := logging.New(cfg.Service, cfg.LogLevel)

	if cfg.Discord.Token == "" {
		log.Error("DISCORD_TOKEN is required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- dependencies ---
	database, err := db.Connect(ctx, cfg.Database)
	if err != nil {
		log.Error("connect database", "err", err)
		os.Exit(1)
	}
	defer database.Close()
	if err := database.Migrate(ctx, cfg.LiteLLM.EmbedDim); err != nil {
		log.Error("migrate database", "err", err)
		os.Exit(1)
	}
	store := db.NewStore(database)
	go database.ReportPoolStats(ctx, log, 30*time.Second)

	q := queue.New(cfg.Redis)
	defer func() { _ = q.Close() }()

	st, err := storage.New(ctx, cfg.Storage)
	if err != nil {
		log.Error("init storage", "err", err)
		os.Exit(1)
	}

	ai := litellm.New(cfg.LiteLLM.BaseURL, cfg.LiteLLM.APIKey, cfg.LiteLLM.RequestTimeout, litellm.WithLogger(log))

	gw, err := gateway.New(cfg, log, store, q, ai, st)
	if err != nil {
		log.Error("build gateway", "err", err)
		os.Exit(1)
	}

	// --- health/metrics server ---
	health := httpserver.New(cfg.HTTPAddr, log, func(hctx context.Context) error {
		if err := q.Ping(hctx); err != nil {
			return err
		}
		return gw.Ready(hctx)
	})
	go func() {
		if err := health.Start(ctx); err != nil {
			log.Error("health server", "err", err)
		}
	}()

	if err := gw.Open(ctx); err != nil {
		log.Error("open gateway", "err", err)
		os.Exit(1)
	}
	log.Info("gateway running", "http", cfg.HTTPAddr)

	// --- reminder scheduler (runs in the gateway since it has the Discord session) ---
	go gateway.RunReminderLoop(ctx, log, store, gw.Session(), 60*time.Second)

	<-ctx.Done()
	log.Info("shutting down")
	if err := gw.Close(); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("close gateway", "err", err)
	}
}
