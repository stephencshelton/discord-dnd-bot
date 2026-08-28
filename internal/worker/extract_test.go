package worker

import (
	"encoding/json"
	"testing"

	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
)

// TestExtractStatePayloadRoundTrip confirms the extract-state job payload
// serializes/deserializes cleanly (the worker decodes it in handleExtractState).
func TestExtractStatePayloadRoundTrip(t *testing.T) {
	in := queue.ExtractStatePayload{SessionID: "abc", GuildID: "g1", Notify: true}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := unmarshal[queue.ExtractStatePayload](json.RawMessage(raw))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// TestExtractStateIsSeparateJobType documents the key safety invariant: state
// extraction runs as its OWN job type, decoupled from transcription. Because
// transcription and extraction are different jobs, an extraction failure is
// handled by the worker's retry/drop logic for the extract job alone and can
// never mark the (already-complete) session failed. This test guards against a
// future refactor accidentally collapsing them back together.
func TestExtractStateIsSeparateJobType(t *testing.T) {
	if queue.JobExtractState == queue.JobTranscribeSession {
		t.Fatal("extract-state must be a distinct job type from transcription so a failure can't fail the session")
	}
	if queue.JobExtractState == "" {
		t.Fatal("JobExtractState must have a non-empty type identifier")
	}
}
