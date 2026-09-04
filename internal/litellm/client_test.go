package litellm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(handler http.HandlerFunc) (*Client, *httptest.Server) {
	srv := httptest.NewServer(handler)
	return New(srv.URL, "test-key", 5*time.Second), srv
}

func TestChatSuccess(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"dnd-chat"`) {
			t.Errorf("request body missing model: %s", body)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello there"}}]}`))
	})
	defer srv.Close()

	got, err := c.Chat(context.Background(), "dnd-chat", []Message{{Role: "user", Content: "hi"}}, 100)
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if got.Content != "hello there" {
		t.Errorf("Chat = %q, want %q", got.Content, "hello there")
	}
	// A response with no finish_reason must not be mistaken for a truncated one.
	if got.Truncated {
		t.Error("absent finish_reason should not report truncation")
	}
}

func TestChatAPIError(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`))
	})
	defer srv.Close()

	_, err := c.Chat(context.Background(), "dnd-chat", nil, 10)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected API error, got %v", err)
	}
}

func TestChatEmptyChoices(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	})
	defer srv.Close()

	_, err := c.Chat(context.Background(), "dnd-chat", nil, 10)
	if err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("expected empty response error, got %v", err)
	}
}

// TestChatReportsTruncation covers the silent failure mode behind
// cut-off session notes and unparseable extraction JSON: hitting max_tokens is
// NOT an HTTP error, so the caller only learns about it from finish_reason.
func TestChatReportsTruncation(t *testing.T) {
	for _, reason := range []string{"length", "max_tokens"} {
		c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"cut off mid-sen"},"finish_reason":"` + reason + `"}]}`))
		})
		res, err := c.Chat(context.Background(), "dnd-chat", nil, 10)
		srv.Close()
		if err != nil {
			t.Fatalf("finish_reason=%s: unexpected error %v", reason, err)
		}
		if !res.Truncated {
			t.Errorf("finish_reason=%s should report Truncated", reason)
		}
		if res.Content != "cut off mid-sen" {
			t.Errorf("finish_reason=%s: content = %q", reason, res.Content)
		}
	}

	// A normal completion must NOT be flagged (that would trigger pointless
	// continuation round trips on every call).
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"all done"},"finish_reason":"stop"}]}`))
	})
	defer srv.Close()
	res, err := c.Chat(context.Background(), "dnd-chat", nil, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Truncated || res.FinishReason != "stop" {
		t.Errorf("normal completion misreported: %+v", res)
	}
}

func TestEmbedSuccess(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"dnd-embed"`) {
			t.Errorf("request body missing model: %s", body)
		}
		if !strings.Contains(string(body), `"input":["a","b"]`) {
			t.Errorf("request body missing inputs: %s", body)
		}
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]},{"embedding":[0.3,0.4]}]}`))
	})
	defer srv.Close()

	got, err := c.Embed(context.Background(), "dnd-embed", []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed error: %v", err)
	}
	if len(got) != 2 || len(got[0]) != 2 || got[1][1] != 0.4 {
		t.Fatalf("Embed = %v, want 2 vectors of len 2", got)
	}
}

func TestEmbedCountMismatch(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
	})
	defer srv.Close()

	_, err := c.Embed(context.Background(), "dnd-embed", []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "expected 2 vectors") {
		t.Fatalf("expected count mismatch error, got %v", err)
	}
}

func TestEmbedEmptyInputNoCall(t *testing.T) {
	called := false
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	defer srv.Close()

	got, err := c.Embed(context.Background(), "dnd-embed", nil)
	if err != nil || got != nil {
		t.Fatalf("Embed(nil) = (%v, %v), want (nil, nil)", got, err)
	}
	if called {
		t.Error("Embed made an HTTP call for empty input")
	}
}

func TestTranscribeSuccess(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("not multipart: %v", err)
		}
		w.Write([]byte(`{"text":"the transcript"}`))
	})
	defer srv.Close()

	got, err := c.Transcribe(context.Background(), "voice-transcribe", "a.wav", strings.NewReader("audio-bytes"))
	if err != nil {
		t.Fatalf("Transcribe error: %v", err)
	}
	if got != "the transcript" {
		t.Errorf("Transcribe = %q", got)
	}
}

func TestGenerateImageSuccess(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"url":"https://img/1.png","b64_json":""}]}`))
	})
	defer srv.Close()

	url, b64, err := c.GenerateImage(context.Background(), "dnd-image", "a dragon", "1024x1024")
	if err != nil {
		t.Fatalf("GenerateImage error: %v", err)
	}
	if url != "https://img/1.png" || b64 != "" {
		t.Errorf("url=%q b64=%q", url, b64)
	}
}

func TestGenerateImageEmpty(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	})
	defer srv.Close()

	_, _, err := c.GenerateImage(context.Background(), "dnd-image", "x", "512x512")
	if err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("expected empty response error, got %v", err)
	}
}

func TestDoNoAuthHeaderWhenKeyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no auth header, got %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)

	if _, err := c.Chat(context.Background(), "m", nil, 1); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
}

func TestRetriesOnTransient5xxThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			// First two attempts fail transiently.
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("upstream unavailable"))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"eventually ok"}}]}`))
	}))
	defer srv.Close()

	// maxRetries=2 => up to 3 attempts total.
	c := New(srv.URL, "k", 2*time.Second, WithMaxRetries(2))
	got, err := c.Chat(context.Background(), "m", nil, 1)
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if got.Content != "eventually ok" {
		t.Errorf("Chat = %q", got.Content)
	}
	if n := atomic.LoadInt32(&attempts); n != 3 {
		t.Errorf("expected 3 attempts, got %d", n)
	}
}

func TestDoesNotRetryOn4xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", 2*time.Second, WithMaxRetries(3))
	_, err := c.Chat(context.Background(), "m", nil, 1)
	if err == nil {
		t.Fatal("expected an error on 400")
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Errorf("400 must not be retried; got %d attempts", n)
	}
}

func TestRetriesExhaustedReturnsError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "k", 2*time.Second, WithMaxRetries(1)) // 2 attempts
	_, err := c.Chat(context.Background(), "m", nil, 1)
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if n := atomic.LoadInt32(&attempts); n != 2 {
		t.Errorf("expected 2 attempts (1 retry), got %d", n)
	}
}
