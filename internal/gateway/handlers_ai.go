package gateway

import (
	"context"
	"strings"

	"github.com/disgoorg/disgo/discord"

	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/prompts"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
)

// handleLore answers a free-form worldbuilding question with the chat model.
func (g *Gateway) handleLore(ctx context.Context, ic *ictx) error {
	guildID, ok := g.resolveGuild(ctx, ic.guildID(), ic.userID())
	if !ok {
		return ic.reply(dmGuildHelp, true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}
	prompt := ic.optString("prompt")

	if err := ic.ack(false); err != nil {
		return err
	}
	msgs := []litellm.Message{
		{Role: "system", Content: prompts.LoreSystem},
		{Role: "user", Content: prompts.LoreUser(camp.Name, camp.System, camp.Premise, prompt)},
	}
	answer, err := g.ai.Chat(ctx, g.cfg.LiteLLM.Lore(), msgs, 700)
	if err != nil {
		metrics.AIRequests.WithLabelValues("chat", "error").Inc()
		return err
	}
	metrics.AIRequests.WithLabelValues("chat", "ok").Inc()
	return ic.followup(truncateForDiscord(answer))
}

// handleRecap generates a "previously on" from recent session notes.
func (g *Gateway) handleRecap(ctx context.Context, ic *ictx) error {
	guildID, ok := g.resolveGuild(ctx, ic.guildID(), ic.userID())
	if !ok {
		return ic.reply(dmGuildHelp, true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}
	notes, err := g.store.RecentCompletedNotes(ctx, camp.ID, 3)
	if err != nil {
		return err
	}
	if len(notes) == 0 {
		return ic.reply("No completed sessions yet — record one with `/session start`.", true)
	}
	if err := ic.ack(false); err != nil {
		return err
	}
	msgs := []litellm.Message{
		{Role: "system", Content: prompts.LoreSystem},
		{Role: "user", Content: prompts.RecapUser(camp.Name, notes)},
	}
	recap, err := g.ai.Chat(ctx, g.cfg.LiteLLM.Recap(), msgs, 400)
	if err != nil {
		metrics.AIRequests.WithLabelValues("chat", "error").Inc()
		return err
	}
	metrics.AIRequests.WithLabelValues("chat", "ok").Inc()
	e := discord.Embed{
		Title:       "Previously, on " + camp.Name + "...",
		Description: truncateForEmbed(recap),
		Color:       0x8b5cf6,
	}
	return ic.followupEmbed(e)
}

// handleAsk answers a question grounded in the campaign's session notes using
// retrieval-augmented generation: embed the question, fetch the most similar
// note passages (pgvector), and constrain the chat model to those excerpts.
func (g *Gateway) handleAsk(ctx context.Context, ic *ictx) error {
	guildID, ok := g.resolveGuild(ctx, ic.guildID(), ic.userID())
	if !ok {
		return ic.reply(dmGuildHelp, true)
	}
	question := strings.TrimSpace(ic.optString("question"))
	if question == "" {
		return ic.reply("Ask me something about your campaign's history.", true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}

	// Nothing indexed yet? Guide the user rather than returning an empty answer.
	if has, herr := g.store.HasEmbeddings(ctx, camp.ID); herr == nil && !has {
		return ic.reply("I don't have any indexed session notes for this campaign yet. Record and complete a session with `/session start`, then try `/ask` again.", true)
	}

	if err := ic.ack(false); err != nil {
		return err
	}

	// 1) Embed the question with the same route used to embed notes.
	qvecs, err := g.ai.Embed(ctx, g.cfg.LiteLLM.EmbedModel, []string{question})
	if err != nil || len(qvecs) == 0 {
		metrics.AIRequests.WithLabelValues("embed", "error").Inc()
		return ic.followup("I couldn't process that question right now. Please try again later.")
	}
	metrics.AIRequests.WithLabelValues("embed", "ok").Inc()

	// 2) Retrieve the most relevant note passages for this campaign.
	chunks, err := g.store.SearchSimilarNotes(ctx, camp.ID, qvecs[0], 6)
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		return ic.followup("I couldn't find anything about that in this campaign's session notes.")
	}
	passages := make([]string, 0, len(chunks))
	for _, c := range chunks {
		passages = append(passages, c.Content)
	}

	// 3) Ask the chat model, constrained to the retrieved excerpts.
	msgs := []litellm.Message{
		{Role: "system", Content: prompts.AskSystem},
		{Role: "user", Content: prompts.AskUser(camp.Name, question, passages)},
	}
	answer, err := g.ai.Chat(ctx, g.cfg.LiteLLM.Ask(), msgs, 600)
	if err != nil {
		metrics.AIRequests.WithLabelValues("chat", "error").Inc()
		return err
	}
	metrics.AIRequests.WithLabelValues("chat", "ok").Inc()
	return ic.followup(truncateForDiscord(answer))
}

// handleReindex enqueues a job to (re)build the active campaign's /ask search
// memory from all completed session notes. Useful after enabling embeddings, or
// after changing the embedding model. Admin-gated (rebuilds can be expensive).
func (g *Gateway) handleReindex(ctx context.Context, ic *ictx) error {
	guildID := ic.guildID()
	if guildID == "" {
		return ic.reply("Use `/reindex` inside a server with an active campaign.", true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}
	if err := g.queue.Enqueue(ctx, queue.JobReindexCampaign, queue.ReindexCampaignPayload{
		CampaignID: camp.ID.String(),
		GuildID:    guildID,
		ChannelID:  ic.channelID(),
	}); err != nil {
		return err
	}
	metrics.JobsEnqueued.WithLabelValues(string(queue.JobReindexCampaign)).Inc()
	return ic.reply("🔎 Rebuilding this campaign's `/ask` memory from completed sessions. I'll post here when it's done.", true)
}

// handleArt enqueues an art-generation job (offloaded to the worker so the
// gateway isn't blocked on a slow image model).
func (g *Gateway) handleArt(ctx context.Context, ic *ictx) error {
	guildID := ic.guildID()
	if guildID == "" {
		return ic.reply("Use `/art` inside a server.", true)
	}

	scene := ic.optString("scene")

	if err := ic.ack(false); err != nil {
		return err
	}
	payload := queue.GenerateArtPayload{
		GuildID:   guildID,
		ChannelID: ic.channelID(),
		UserID:    ic.userID(),
		Prompt:    scene,
	}
	if err := g.queue.Enqueue(ctx, queue.JobGenerateArt, payload); err != nil {
		return err
	}
	metrics.JobsEnqueued.WithLabelValues(string(queue.JobGenerateArt)).Inc()
	return ic.followup("🎨 Painting your scene… I'll post it in this channel shortly.")
}

// truncateForDiscord trims content to Discord's 2000-char message limit.
func truncateForDiscord(s string) string {
	const max = 2000
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "\u2026"
}

// truncateForEmbed trims content to Discord's 4096-char embed-description limit.
func truncateForEmbed(s string) string {
	const max = 4096
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "\u2026"
}
