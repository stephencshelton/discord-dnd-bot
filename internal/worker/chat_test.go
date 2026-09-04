package worker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
)

// TestChatCompleteContinuesTruncatedOutput is the regression test for output that
// stops at the token limit: the reply comes back cut off with NO error, which
// produced session notes that ended mid-sentence and extraction JSON that no
// longer parsed. chatComplete must resume and stitch the pieces together.
func TestChatCompleteContinuesTruncatedOutput(t *testing.T) {
	var calls int
	var sawContinue bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls++
		switch calls {
		case 1:
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"## Recap\nThe party retur"},"finish_reason":"length"}]}`))
		default:
			// The continuation must carry the partial reply back as context.
			if strings.Contains(string(body), "cut off") && strings.Contains(string(body), "The party retur") {
				sawContinue = true
			}
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ned home."},"finish_reason":"stop"}]}`))
		}
	}))
	defer srv.Close()

	w := &Worker{log: slog.New(slog.DiscardHandler), ai: litellm.New(srv.URL, "k", 5*time.Second)}
	got, err := w.chatComplete(context.Background(), "notes", "dnd-notes",
		[]litellm.Message{{Role: "user", Content: "summarize"}}, 100)
	if err != nil {
		t.Fatalf("chatComplete error: %v", err)
	}
	if want := "## Recap\nThe party returned home."; got != want {
		t.Errorf("stitched output = %q, want %q", got, want)
	}
	if calls != 2 {
		t.Errorf("expected exactly one continuation round, got %d calls", calls)
	}
	if !sawContinue {
		t.Error("continuation request did not feed back the partial reply as context")
	}
}

// TestChatCompleteGivesUpAfterMaxRounds ensures a model that keeps hitting the
// limit can't loop forever — we return the partial text rather than spinning.
func TestChatCompleteGivesUpAfterMaxRounds(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"length"}]}`))
	}))
	defer srv.Close()

	w := &Worker{log: slog.New(slog.DiscardHandler), ai: litellm.New(srv.URL, "k", 5*time.Second)}
	got, err := w.chatComplete(context.Background(), "extract", "dnd-state", nil, 100)
	if err != nil {
		t.Fatalf("chatComplete error: %v", err)
	}
	if calls != maxContinuationRounds+1 {
		t.Errorf("made %d calls, want %d (initial + %d continuations)", calls, maxContinuationRounds+1, maxContinuationRounds)
	}
	if got != strings.Repeat("x", maxContinuationRounds+1) {
		t.Errorf("partial output = %q", got)
	}
}

// TestChatCompleteSingleCallWhenComplete guards against needless extra round
// trips on the overwhelmingly common case.
func TestChatCompleteSingleCallWhenComplete(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	w := &Worker{log: slog.New(slog.DiscardHandler), ai: litellm.New(srv.URL, "k", 5*time.Second)}
	got, err := w.chatComplete(context.Background(), "notes", "dnd-notes", nil, 100)
	if err != nil {
		t.Fatalf("chatComplete error: %v", err)
	}
	if got != "done" || calls != 1 {
		t.Errorf("got %q in %d call(s), want %q in 1", got, calls, "done")
	}
}
