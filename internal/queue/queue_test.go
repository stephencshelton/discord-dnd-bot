package queue

import (
	"encoding/json"
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
