package gateway

import (
	"context"
	"sort"
	"strings"

	"github.com/disgoorg/disgo/discord"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
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
		return ic.reply("I don't have anything indexed for this campaign yet. Record a session with `/session start`, or add NPCs/locations/characters with `/world add` and `/character add`, then try `/ask` again.", true)
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

	// 2) Retrieve the most relevant passages for this campaign from BOTH the
	// session transcripts AND curated canon (world entities + player characters),
	// then merge by similarity. This lets /ask answer from what was said at the
	// table (transcripts) and from DM-authored / AI-approved facts (canon) — e.g.
	// an NPC added via /world add, or a fact from /remember — in one answer.
	transcriptChunks, err := g.store.SearchSimilarNotes(ctx, camp.ID, qvecs[0], 12)
	if err != nil {
		return err
	}
	canonChunks, err := g.store.SearchSimilarCanon(ctx, camp.ID, qvecs[0], 6)
	if err != nil {
		return err
	}
	passages := mergeAskPassages(transcriptChunks, canonChunks)
	if len(passages) == 0 {
		return ic.followup("I couldn't find anything about that in this campaign's records.")
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

// handlePrep assembles a concrete, actionable pre-session briefing for the
// table from the CURRENT campaign state — distinct from /recap's dramatic
// "previously on" narrative. It gathers the most recent completed session's
// notes, the campaign's active quests, key world entities, and player
// characters, then asks the (recap) model to turn that state into a "where we
// left off and what's next" prep sheet. It never invents — it works only from
// recorded state.
func (g *Gateway) handlePrep(ctx context.Context, ic *ictx) error {
	guildID, ok := g.resolveGuild(ctx, ic.guildID(), ic.userID())
	if !ok {
		return ic.reply(dmGuildHelp, true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}

	if err := ic.ack(false); err != nil {
		return err
	}

	// Most recent completed session (for "where we left off").
	lastNotes, lastDate := "", ""
	if sessions, serr := g.store.ListSessionsByStatusForCampaign(ctx, camp.ID, "complete", 1); serr == nil && len(sessions) > 0 {
		lastDate = sessions[0].StartedAt.UTC().Format("Monday, 2 January 2006")
		if full, gerr := g.store.GetSession(ctx, sessions[0].ID); gerr == nil {
			lastNotes = full.Notes
		}
	}

	// Current world state: active quests, key NPCs/locations/factions, and PCs.
	var activeQuests, keyEntities []string
	if entities, eerr := g.store.ListAllWorldEntities(ctx, camp.ID); eerr == nil {
		for _, ent := range entities {
			line := entityPrepLine(ent)
			if ent.Kind == db.KindQuest && isActiveQuest(ent) {
				activeQuests = append(activeQuests, line)
				continue
			}
			if ent.Kind != db.KindQuest {
				keyEntities = append(keyEntities, line)
			}
		}
	}
	var characters []string
	if pcs, perr := g.store.ListPCs(ctx, camp.ID); perr == nil {
		for _, pc := range pcs {
			line := pc.Name
			descr := strings.TrimSpace(strings.TrimSpace(pc.Race) + " " + strings.TrimSpace(pc.Class))
			if descr != "" {
				line += " — " + descr
			}
			characters = append(characters, line)
		}
	}

	// If there's genuinely nothing to work from, say so instead of an empty sheet.
	if lastNotes == "" && len(activeQuests) == 0 && len(keyEntities) == 0 && len(characters) == 0 {
		return ic.followup("There's nothing to prep from yet — record a session with `/session start`, or add quests/NPCs/characters with `/world add` and `/character add`.")
	}

	msgs := []litellm.Message{
		{Role: "system", Content: prompts.PrepSystem},
		{Role: "user", Content: prompts.PrepUser(camp.Name, camp.System, lastDate, lastNotes, activeQuests, keyEntities, characters)},
	}
	briefing, err := g.ai.Chat(ctx, g.cfg.LiteLLM.Recap(), msgs, 900)
	if err != nil {
		metrics.AIRequests.WithLabelValues("chat", "error").Inc()
		return err
	}
	metrics.AIRequests.WithLabelValues("chat", "ok").Inc()

	e := discord.Embed{
		Title:       "🗺️ Session prep — " + camp.Name,
		Description: truncateForEmbed(briefing),
		Color:       0x0ea5e9,
	}
	return ic.followupEmbed(e)
}

// entityPrepLine renders a world entity as a compact "Name — description (meta)"
// line for the prep briefing input.
func entityPrepLine(e db.WorldEntity) string {
	line := e.Name
	if d := strings.TrimSpace(e.Description); d != "" {
		line += " — " + d
	}
	if summary := worldMetaSummary(e); summary != "" {
		line += " (" + summary + ")"
	}
	return line
}

// isActiveQuest reports whether a quest entity's status metadata marks it as
// still in play. A quest with no status is treated as active; only an explicit
// terminal status (completed/failed/resolved/…) excludes it.
func isActiveQuest(e db.WorldEntity) bool {
	status, _ := e.Metadata["status"].(string)
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "done", "failed", "resolved", "closed", "abandoned":
		return false
	default:
		return true
	}
}

// mergeAskPassages combines transcript and canon retrieval results into a single
// ranked, labeled passage list for the /ask prompt. Passages are ordered by
// ascending cosine distance (closest first) so the most relevant context leads,
// regardless of source. Each passage is prefixed with its source so the answer
// model can weigh curated canon appropriately. Total is capped to keep the
// prompt bounded.
func mergeAskPassages(transcripts, canon []db.RetrievedChunk) []string {
	const maxPassages = 14
	type labeled struct {
		text     string
		distance float64
	}
	merged := make([]labeled, 0, len(transcripts)+len(canon))
	for _, c := range canon {
		merged = append(merged, labeled{"[Campaign canon] " + strings.TrimSpace(c.Content), c.Distance})
	}
	for _, c := range transcripts {
		merged = append(merged, labeled{"[Session record] " + strings.TrimSpace(c.Content), c.Distance})
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].distance < merged[j].distance })
	if len(merged) > maxPassages {
		merged = merged[:maxPassages]
	}
	out := make([]string, 0, len(merged))
	for _, m := range merged {
		out = append(out, m.text)
	}
	return out
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
