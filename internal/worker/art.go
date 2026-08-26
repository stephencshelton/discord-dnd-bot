package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/prompts"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
)

// imageFetchClient bounds how long we wait on a provider-hosted image URL so a
// slow host can't tie up a worker slot for the whole job budget.
var imageFetchClient = &http.Client{Timeout: 30 * time.Second}

// handleGenerateArt generates scene art via LiteLLM and posts it back to the
// requesting channel. LiteLLM may return either a hosted URL or inline base64;
// we handle both and always upload the bytes to Discord so the image persists.
func (w *Worker) handleGenerateArt(ctx context.Context, raw json.RawMessage) error {
	p, err := unmarshal[queue.GenerateArtPayload](raw)
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}

	// Enrich the prompt with campaign-appropriate style if we can resolve one.
	system := ""
	if camp, cerr := w.store.GetActiveCampaign(ctx, p.GuildID); cerr == nil {
		system = camp.System
	}
	fullPrompt := prompts.ArtPrompt(system, p.Prompt)

	url, b64, err := w.ai.GenerateImage(ctx, w.cfg.LiteLLM.ImageModel, fullPrompt, "1024x1024")
	if err != nil {
		metrics.AIRequests.WithLabelValues("image", "error").Inc()
		w.notify(p.GuildID, p.ChannelID, "🎨 Art generation failed. Please try again later.")
		return fmt.Errorf("generate image: %w", err)
	}
	metrics.AIRequests.WithLabelValues("image", "ok").Inc()

	imgBytes, err := w.resolveImageBytes(ctx, url, b64)
	if err != nil {
		w.notify(p.GuildID, p.ChannelID, "🎨 The art was generated but I couldn't retrieve it.")
		return fmt.Errorf("resolve image: %w", err)
	}

	// Store a copy in object storage for durability, keyed by time.
	key := fmt.Sprintf("art/%s/%d.png", p.GuildID, time.Now().UnixNano())
	if _, serr := w.storage.Put(ctx, key, "image/png", bytes.NewReader(imgBytes)); serr != nil {
		w.log.Warn("persist art to storage failed", "err", serr)
	}

	caption := fmt.Sprintf("🎨 Art for <@%s>: %s", p.UserID, truncate(p.Prompt, 180))
	_, err = w.discord.ChannelMessageSendComplex(p.ChannelID, &discordgo.MessageSend{
		Content: caption,
		Files: []*discordgo.File{{
			Name:        "scene.png",
			ContentType: "image/png",
			Reader:      bytes.NewReader(imgBytes),
		}},
	})
	if err != nil {
		return fmt.Errorf("post art: %w", err)
	}
	return nil
}

// resolveImageBytes returns the raw image bytes from either a base64 payload or
// by fetching the hosted URL.
func (w *Worker) resolveImageBytes(ctx context.Context, url, b64 string) ([]byte, error) {
	if b64 != "" {
		return base64.StdEncoding.DecodeString(b64)
	}
	if url == "" {
		return nil, fmt.Errorf("no image url or data returned")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := imageFetchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch image: status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 25<<20)) // cap at 25 MiB
}

// truncate shortens s to at most n runes, adding an ellipsis if trimmed.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
