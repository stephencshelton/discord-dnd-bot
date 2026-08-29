package queue

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
)

// newTestQueue connects to the Redis in TEST_REDIS_ADDR (using a high DB index
// to avoid clobbering real data) and flushes the queue keys for isolation. It
// SKIPS the test when no test Redis is configured or reachable, so the suite
// still passes in CI without Redis — the reliable-queue logic is validated here
// when a Redis is available.
//
//	TEST_REDIS_ADDR=localhost:6379 go test ./internal/queue/...
func newTestQueue(t *testing.T) *Queue {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set; skipping Redis integration test")
	}
	q := New(config.RedisConfig{Addr: addr, DB: 15})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := q.Ping(ctx); err != nil {
		t.Skipf("cannot reach TEST_REDIS_ADDR: %v", err)
	}
	// Clean slate.
	if err := q.rdb.Del(ctx, queueKey, processingKey).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	t.Cleanup(func() {
		_ = q.rdb.Del(context.Background(), queueKey, processingKey).Err()
		_ = q.Close()
	})
	return q
}

// TestReliableQueueAckRemovesInflight: a dequeued job sits on the processing
// list until Ack removes it; Depth counts it as outstanding meanwhile.
func TestReliableQueueAckRemovesInflight(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	if err := q.Enqueue(ctx, JobTranscribeSession, TranscribeSessionPayload{SessionID: "s1"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if d, _ := q.Depth(ctx); d != 1 {
		t.Fatalf("depth after enqueue = %d, want 1", d)
	}

	job, err := q.Dequeue(ctx, time.Second)
	if err != nil || job == nil {
		t.Fatalf("dequeue: job=%v err=%v", job, err)
	}
	// The job left the pending list but is still OUTSTANDING (in flight).
	if n, _ := q.rdb.LLen(ctx, queueKey).Result(); n != 0 {
		t.Errorf("pending list should be empty after dequeue, got %d", n)
	}
	if n, _ := q.rdb.LLen(ctx, processingKey).Result(); n != 1 {
		t.Errorf("processing list should hold the in-flight job, got %d", n)
	}
	if d, _ := q.Depth(ctx); d != 1 {
		t.Errorf("Depth should still be 1 while the job is in flight, got %d", d)
	}

	if err := q.Ack(ctx, job); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if d, _ := q.Depth(ctx); d != 0 {
		t.Errorf("Depth after ack = %d, want 0", d)
	}
}

// TestReliableQueueRequeueMovesBack: Requeue removes the in-flight copy and
// returns the job to pending with an incremented attempt — no duplicate.
func TestReliableQueueRequeueMovesBack(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	if err := q.Enqueue(ctx, JobGenerateArt, GenerateArtPayload{Prompt: "x"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, _ := q.Dequeue(ctx, time.Second)
	if job == nil {
		t.Fatal("expected a job")
	}
	if err := q.Requeue(ctx, job); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n, _ := q.rdb.LLen(ctx, processingKey).Result(); n != 0 {
		t.Errorf("processing list should be empty after requeue, got %d", n)
	}
	// Exactly one job pending again, attempt incremented.
	if d, _ := q.Depth(ctx); d != 1 {
		t.Errorf("Depth after requeue = %d, want 1 (no duplicate)", d)
	}
	got, _ := q.Dequeue(ctx, time.Second)
	if got == nil || got.Attempt != 1 {
		t.Errorf("requeued job attempt = %v, want 1", got)
	}
}

// TestReliableQueueReapRecoversOrphans: jobs left on the processing list (a
// hard-killed worker) are returned to pending by ReapOrphans.
func TestReliableQueueReapRecoversOrphans(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	// Two jobs dequeued but never acked (simulating a crashed worker).
	for i := 0; i < 2; i++ {
		if err := q.Enqueue(ctx, JobEmbedCanon, EmbedCanonPayload{SourceID: "e"}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := q.Dequeue(ctx, time.Second); err != nil {
			t.Fatalf("dequeue: %v", err)
		}
	}
	if n, _ := q.rdb.LLen(ctx, processingKey).Result(); n != 2 {
		t.Fatalf("processing list should hold 2 orphans, got %d", n)
	}

	recovered, err := q.ReapOrphans(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if recovered != 2 {
		t.Errorf("recovered = %d, want 2", recovered)
	}
	if n, _ := q.rdb.LLen(ctx, processingKey).Result(); n != 0 {
		t.Errorf("processing list should be empty after reap, got %d", n)
	}
	if n, _ := q.rdb.LLen(ctx, queueKey).Result(); n != 2 {
		t.Errorf("pending list should hold 2 recovered jobs, got %d", n)
	}
}
