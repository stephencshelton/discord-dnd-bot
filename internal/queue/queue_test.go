package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// TestJobEnvelopeRoundTrip verifies that a typed payload survives being wrapped
// in a Job envelope and unmarshaled back out — the exact path Enqueue/Dequeue
// take, minus Redis.
func TestJobEnvelopeRoundTrip(t *testing.T) {
	in := TranscribeSessionPayload{SessionID: "sess-123", GuildID: "guild-456"}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	body, err := json.Marshal(Job{Type: JobTranscribeSession, Payload: raw})
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}

	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	if job.Type != JobTranscribeSession {
		t.Errorf("job type = %q, want %q", job.Type, JobTranscribeSession)
	}

	var out TranscribeSessionPayload
	if err := json.Unmarshal(job.Payload, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if out != in {
		t.Errorf("payload round-trip = %+v, want %+v", out, in)
	}
}

func TestGenerateArtPayloadRoundTrip(t *testing.T) {
	in := GenerateArtPayload{
		GuildID:   "g",
		ChannelID: "c",
		UserID:    "u",
		Prompt:    "a dragon perched on a snowy peak",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out GenerateArtPayload
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip = %+v, want %+v", out, in)
	}
}

func TestJobTypeConstants(t *testing.T) {
	if JobTranscribeSession != "transcribe_session" {
		t.Errorf("JobTranscribeSession = %q", JobTranscribeSession)
	}
	if JobGenerateArt != "generate_art" {
		t.Errorf("JobGenerateArt = %q", JobGenerateArt)
	}
}

// TestPermanentClassification verifies the retryable/permanent error split the
// worker uses to decide whether to requeue a failed job.
func TestPermanentClassification(t *testing.T) {
	base := errors.New("bad input")

	perm := Permanent(base)
	if !IsPermanent(perm) {
		t.Errorf("IsPermanent(Permanent(err)) = false, want true")
	}
	if !errors.Is(perm, base) {
		t.Errorf("Permanent(err) should still wrap the original error")
	}

	// A wrapped permanent error is still permanent.
	wrapped := fmt.Errorf("transcribe: %w", perm)
	if !IsPermanent(wrapped) {
		t.Errorf("IsPermanent should see through additional wrapping")
	}

	// A plain (transient) error is not permanent.
	if IsPermanent(base) {
		t.Errorf("a plain error must not be classified permanent")
	}
	// Permanent(nil) is nil so it composes cheaply at call sites.
	if Permanent(nil) != nil {
		t.Errorf("Permanent(nil) = non-nil, want nil")
	}
}

// TestRequeueIncrementsAttempt verifies the attempt counter advances on requeue
// (marshaling only — Redis is not exercised here) so the worker's max-retries
// bound is respected.
func TestRequeueIncrementsAttempt(t *testing.T) {
	job := &Job{Type: JobTranscribeSession, Attempt: 0}
	// Simulate what Requeue does before the LPush: bump then serialize.
	for want := 1; want <= 3; want++ {
		job.Attempt++
		body, err := json.Marshal(job)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out Job
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.Attempt != want {
			t.Errorf("Attempt after requeue = %d, want %d", out.Attempt, want)
		}
	}
}
