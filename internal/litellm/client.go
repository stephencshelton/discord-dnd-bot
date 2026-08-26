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
	"mime/multipart"
	"net/http"
	"time"
)

// Client talks to a LiteLLM (OpenAI-compatible) endpoint.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New constructs a Client. timeout bounds each request.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
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
		Message Message `json:"message"`
	} `json:"choices"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// Chat sends a chat completion and returns the assistant's text reply. It powers
// bot mentions, DM chat, and session-note generation.
func (c *Client) Chat(ctx context.Context, model string, msgs []Message, maxTokens int) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       model,
		Messages:    msgs,
		Temperature: 0.7,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		return "", err
	}
	var out chatResponse
	if err := c.do(ctx, http.MethodPost, "/v1/chat/completions", "application/json", bytes.NewReader(body), &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("litellm chat error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("litellm chat: empty response")
	}
	return out.Choices[0].Message.Content, nil
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
	if err := c.do(ctx, http.MethodPost, "/v1/embeddings", "application/json", bytes.NewReader(body), &out); err != nil {
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
	if err := c.do(ctx, http.MethodPost, "/v1/audio/transcriptions", w.FormDataContentType(), &buf, &out); err != nil {
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
	if err := c.do(ctx, http.MethodPost, "/v1/images/generations", "application/json", bytes.NewReader(body), &out); err != nil {
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

// do performs the HTTP request and decodes JSON into out.
func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("litellm request %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("litellm %s %s: status %d: %s", method, path, resp.StatusCode, string(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("litellm decode %s: %w", path, err)
	}
	return nil
}
