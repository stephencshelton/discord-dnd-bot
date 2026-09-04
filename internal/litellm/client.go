// Package litellm is a thin OpenAI-compatible client for a LiteLLM proxy.
//
// LiteLLM fronts every model behind one base URL + key, exposing OpenAI REST
// shapes for chat, transcription, and image generation. We send a logical
// model name; routing to a specific provider is LiteLLM's concern, so the bot
// has zero provider-specific code and providers can swap without a redeploy.
package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
)

// Client talks to a LiteLLM (OpenAI-compatible) endpoint.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	log     *slog.Logger
	// maxRetries is the number of additional attempts on transient failures
	// (network errors, HTTP 429/5xx). 0 means a single attempt.
	maxRetries int
}

// Option configures a Client.
type Option func(*Client)

// WithLogger attaches a logger used for request/latency/error logging.
func WithLogger(log *slog.Logger) Option {
	return func(c *Client) { c.log = log }
}

// WithMaxRetries sets how many times a transient failure is retried.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// New constructs a Client. timeout bounds each request attempt.
func New(baseURL, apiKey string, timeout time.Duration, opts ...Option) *Client {
	c := &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		http:       &http.Client{Timeout: timeout},
		log:        slog.Default(),
		maxRetries: 2,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// usage mirrors the OpenAI usage block so token accounting is available.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"` // system|user|assistant
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage    `json:"usage,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// ChatResult is a chat completion plus why generation stopped. Callers that
// generate long output (session notes, state-extraction JSON) need this: when
// the model hits max_tokens the reply is silently CUT MID-SENTENCE, which
// produced half-written recaps and unparseable JSON. Truncated lets a caller
// continue the generation instead of shipping a fragment.
type ChatResult struct {
	Content string
	// FinishReason is the provider's stop reason ("stop", "length", ...). Empty
	// when the provider omits it.
	FinishReason string
	// Truncated is true when generation stopped because it ran out of tokens.
	Truncated bool
}

// Chat sends a chat completion and returns the assistant's text reply. It powers
// bot mentions, DM chat, and session-note generation. Output truncation is not
// visible through this call — use ChatWithResult when completeness matters.
func (c *Client) Chat(ctx context.Context, model string, msgs []Message, maxTokens int) (string, error) {
	res, err := c.ChatWithResult(ctx, model, msgs, maxTokens)
	return res.Content, err
}

// ChatWithResult is Chat plus the stop reason, so callers can detect and repair
// a reply that was cut off at max_tokens.
func (c *Client) ChatWithResult(ctx context.Context, model string, msgs []Message, maxTokens int) (ChatResult, error) {
	body, err := json.Marshal(chatRequest{
		Model:       model,
		Messages:    msgs,
		Temperature: 0.7,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		return ChatResult{}, err
	}
	var out chatResponse
	if err := c.do(ctx, "chat", http.MethodPost, "/v1/chat/completions", "application/json", bytes.NewReader(body), &out); err != nil {
		return ChatResult{}, err
	}
	if out.Error != nil {
		return ChatResult{}, fmt.Errorf("litellm chat error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return ChatResult{}, fmt.Errorf("litellm chat: empty response")
	}
	recordTokens("chat", out.Usage)
	reason := out.Choices[0].FinishReason
	res := ChatResult{
		Content:      out.Choices[0].Message.Content,
		FinishReason: reason,
		// Providers spell the token-limit stop differently: OpenAI/LiteLLM use
		// "length", Anthropic-style routes surface "max_tokens".
		Truncated: reason == "length" || reason == "max_tokens",
	}
	if res.Truncated {
		c.log.Warn("model output hit the token limit and was cut off",
			"model", model, "max_tokens", maxTokens, "finish_reason", reason)
	}
	return res, nil
}

type transcriptionResponse struct {
	Text  string    `json:"text"`
	Error *apiError `json:"error,omitempty"`
}

type embeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *apiError `json:"error,omitempty"`
}

// Embed returns an embedding vector for each input string, in order. Powers
// grounded /ask retrieval: notes are embedded on write, the question on read,
// compared by vector distance in Postgres (pgvector).
func (c *Client) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embeddingRequest{Model: model, Input: inputs})
	if err != nil {
		return nil, err
	}
	var out embeddingResponse
	if err := c.do(ctx, "embed", http.MethodPost, "/v1/embeddings", "application/json", bytes.NewReader(body), &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("litellm embed error: %s", out.Error.Message)
	}
	if len(out.Data) != len(inputs) {
		return nil, fmt.Errorf("litellm embed: expected %d vectors, got %d", len(inputs), len(out.Data))
	}
	vecs := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

// Transcribe uploads audio bytes and returns the transcript text. audioName is
// used only for the multipart filename (its extension hints at the format).
func (c *Client) Transcribe(ctx context.Context, model, audioName string, audio io.Reader) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("model", model); err != nil {
		return "", err
	}
	fw, err := w.CreateFormFile("file", audioName)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, audio); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	var out transcriptionResponse
	if err := c.do(ctx, "transcribe", http.MethodPost, "/v1/audio/transcriptions", w.FormDataContentType(), &buf, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("litellm transcribe error: %s", out.Error.Message)
	}
	return out.Text, nil
}

type imageRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	N      int    `json:"n"`
	Size   string `json:"size"`
}

type imageResponse struct {
	Data []struct {
		URL     string `json:"url"`
		B64JSON string `json:"b64_json"`
	} `json:"data"`
	Error *apiError `json:"error,omitempty"`
}

// GenerateImage requests AI scene art and returns the first result's URL (or an
// empty string if the provider returned base64 only, in which case B64 is set).
func (c *Client) GenerateImage(ctx context.Context, model, prompt, size string) (url, b64 string, err error) {
	body, err := json.Marshal(imageRequest{Model: model, Prompt: prompt, N: 1, Size: size})
	if err != nil {
		return "", "", err
	}
	var out imageResponse
	if err := c.do(ctx, "image", http.MethodPost, "/v1/images/generations", "application/json", bytes.NewReader(body), &out); err != nil {
		return "", "", err
	}
	if out.Error != nil {
		return "", "", fmt.Errorf("litellm image error: %s", out.Error.Message)
	}
	if len(out.Data) == 0 {
		return "", "", fmt.Errorf("litellm image: empty response")
	}
	return out.Data[0].URL, out.Data[0].B64JSON, nil
}

// do performs the HTTP request and decodes JSON into out. kind labels the call
// (chat|embed|transcribe|image) for logging and metrics. Transient failures
// (network errors, HTTP 429/5xx) are retried with exponential backoff up to
// c.maxRetries; the request body is buffered so it can be replayed per attempt.
func (c *Client) do(ctx context.Context, kind, method, path, contentType string, body io.Reader, out any) error {
	var buf []byte
	if body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("litellm read body: %w", err)
		}
		buf = b
	}

	start := time.Now()
	log := logging.FromContext(ctx, c.log).With("ai_kind", kind, "path", path)

	var lastErr error
	attempts := c.maxRetries + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		var reader io.Reader
		if buf != nil {
			reader = bytes.NewReader(buf)
		}
		status, retryable, err := c.attempt(ctx, method, path, contentType, reader, out)
		if err == nil {
			dur := time.Since(start)
			metrics.AIRequests.WithLabelValues(kind, "ok").Inc()
			metrics.AIRequestDuration.WithLabelValues(kind).Observe(dur.Seconds())
			log.Debug("litellm request ok",
				"status", status, "attempt", attempt, "duration_ms", dur.Milliseconds())
			return nil
		}
		lastErr = err
		if !retryable || attempt == attempts || ctx.Err() != nil {
			break
		}
		backoff := time.Duration(attempt) * 250 * time.Millisecond
		log.Warn("litellm request failed; retrying",
			"status", status, "attempt", attempt, "err", err, "backoff_ms", backoff.Milliseconds())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			lastErr = ctx.Err()
		}
	}

	metrics.AIRequests.WithLabelValues(kind, "error").Inc()
	metrics.AIRequestDuration.WithLabelValues(kind).Observe(time.Since(start).Seconds())
	metrics.ComponentError("litellm", kind)
	log.Error("litellm request failed", "err", lastErr)
	return lastErr
}

// attempt performs a single HTTP attempt. It returns the HTTP status (0 on a
// transport error), whether the failure is retryable, and any error.
func (c *Client) attempt(ctx context.Context, method, path, contentType string, body io.Reader, out any) (int, bool, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Content-Type", contentType)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Transport-level errors (timeouts, connection resets) are retryable.
		return 0, true, fmt.Errorf("litellm request %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, true, err
	}
	if resp.StatusCode >= 400 {
		// 429 and 5xx are transient; 4xx (except 429) are permanent.
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return resp.StatusCode, retryable, fmt.Errorf("litellm %s %s: status %d: %s", method, path, resp.StatusCode, string(data))
	}
	if out == nil {
		return resp.StatusCode, false, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp.StatusCode, false, fmt.Errorf("litellm decode %s: %w", path, err)
	}
	return resp.StatusCode, false, nil
}

// recordTokens publishes token-usage metrics for a chat response, if present.
func recordTokens(kind string, u *usage) {
	if u == nil {
		return
	}
	if u.PromptTokens > 0 {
		metrics.AITokens.WithLabelValues(kind, "prompt").Add(float64(u.PromptTokens))
	}
	if u.CompletionTokens > 0 {
		metrics.AITokens.WithLabelValues(kind, "completion").Add(float64(u.CompletionTokens))
	}
}
