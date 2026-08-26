package litellm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if got != "hello there" {
		t.Errorf("Chat = %q, want %q", got, "hello there")
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
