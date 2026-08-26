// Package db provides the PostgreSQL connection pool, embedded migrations, and
// typed data-access methods (the Store) used by every service.
package db

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps a pgx pool.
type DB struct {
	Pool *pgxpool.Pool
}

// Connect opens a pgx pool using the provided database config.
func Connect(ctx context.Context, cfg config.DatabaseConfig) (*DB, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if cfg.MaxConns > 0 {
		pcfg.MaxConns = cfg.MaxConns
	}
	if cfg.ConnMaxLifetime > 0 {
		pcfg.MaxConnLifetime = cfg.ConnMaxLifetime
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// Close releases the pool.
func (d *DB) Close() {
	if d.Pool != nil {
		d.Pool.Close()
	}
}

// ReportPoolStats periodically publishes pgx pool statistics as gauges (and a
// debug log line) until the context is cancelled. Gives visibility into pool
// exhaustion / acquire contention. Best effort and panic-safe.
func (d *DB) ReportPoolStats(ctx context.Context, log *slog.Logger, every time.Duration) {
	if d.Pool == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	defer func() {
		if r := recover(); r != nil {
			metrics.PanicsRecovered.WithLabelValues("goroutine").Inc()
			log.Error("db pool-stats reporter panicked; recovered", "panic", r)
		}
	}()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := d.Pool.Stat()
			metrics.DBPoolConns.WithLabelValues("total").Set(float64(s.TotalConns()))
			metrics.DBPoolConns.WithLabelValues("acquired").Set(float64(s.AcquiredConns()))
			metrics.DBPoolConns.WithLabelValues("idle").Set(float64(s.IdleConns()))
			log.Debug("db pool stats",
				"total", s.TotalConns(),
				"acquired", s.AcquiredConns(),
				"idle", s.IdleConns(),
				"max", s.MaxConns(),
				"acquire_count", s.AcquireCount(),
				"empty_acquire_count", s.EmptyAcquireCount(),
			)
		}
	}
}

// Migrate applies all embedded .sql migrations in lexical order. Each file
// must be idempotent (CREATE TABLE IF NOT EXISTS ...) since there's no
// applied-version tracking; re-running is safe.
//
// embedDim sizes the pgvector column for /ask retrieval, substituted for the
// __EMBED_DIM__ placeholder in the embeddings migration.
func (d *DB) Migrate(ctx context.Context, embedDim int) error {
	if embedDim < 1 {
		embedDim = 1536
	}
	// Serialize migrations across concurrently-starting replicas with a session
	// advisory lock, so two gateways don't race on DDL.
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Release()
	const migrationLockKey = 0x646E6462 // "dndb"
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationLockKey) }()

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		stmt := strings.ReplaceAll(string(sqlBytes), "__EMBED_DIM__", strconv.Itoa(embedDim))
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}
