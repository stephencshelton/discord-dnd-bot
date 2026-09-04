package gateway

import (
	"context"

	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
)

// chat runs an interactive chat completion for a slash command and reports
// whether the model's reply was cut off at the token limit.
//
// Every gateway command used litellm.Chat, which discards finish_reason — so a
// reply that ran out of output tokens came back chopped mid-sentence and looked
// to the user exactly like a broken generation, with nothing in the logs. This
// wrapper keeps that visible (log + metric) so an undersized budget shows up in
// monitoring rather than only in a complaint.
//
// Unlike the worker, this does NOT auto-continue a truncated reply: the user is
// waiting on an interaction, and extra round trips would push a slow model past
// the follow-up window. The budgets are instead sized to the Discord surface
// (see config.LiteLLMConfig token resolvers), and callers mark the cut so it
// reads as "trimmed", not "crashed".
//
// task labels the metric (e.g. "lore", "ask", "recap", "prep", "mention").
func (g *Gateway) chat(ctx context.Context, task, model string, msgs []litellm.Message, maxTokens int) (text string, truncated bool, err error) {
	res, err := g.ai.Chat(ctx, model, msgs, maxTokens)
	if err != nil {
		metrics.AIRequests.WithLabelValues("chat", "error").Inc()
		return "", false, err
	}
	metrics.AIRequests.WithLabelValues("chat", "ok").Inc()
	if res.Truncated {
		metrics.AIResponsesTruncated.WithLabelValues(task, "final").Inc()
		logging.FromContext(ctx, g.log).Warn("model reply hit the token limit; answer is incomplete",
			"task", task, "model", model, "max_tokens", maxTokens)
	}
	return res.Content, res.Truncated, nil
}
