package worker

import (
	"context"
	"strings"

	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
)

// continuePrompt asks the model to resume a reply that was cut off at the token
// limit. It is deliberately blunt about not repeating or re-introducing text:
// the pieces are concatenated verbatim, so any preamble ("Continuing...") or
// restated heading would corrupt the result (or, for JSON, make it unparseable).
const continuePrompt = `Your previous message was cut off because it hit the output token limit.
Continue from EXACTLY where it stopped — resume mid-word if that is where it ended.
Do NOT repeat anything you already wrote, do NOT restart, do NOT summarize what came before,
and do NOT add any preamble, apology, or closing remark. Output only the remaining content.`

// maxContinuationRounds bounds how many follow-up calls chatComplete will make.
// Each round costs a full prompt re-send, so this is a safety net for a long
// session's notes/extraction — not a licence for unbounded generation.
const maxContinuationRounds = 3

// chatComplete is Chat plus automatic recovery from output truncation.
//
// Why this exists: a chat completion that hits max_tokens comes back CUT OFF
// MID-SENTENCE with no error at all. That silently produced two user-visible
// bugs — session notes whose last section stopped mid-word, and state-extraction
// JSON that no longer parsed (so a whole session yielded zero review proposals
// and /review-session looked broken). Both were invisible in logs because the
// HTTP call itself succeeded.
//
// On truncation it re-asks the model to continue from where it stopped, feeding
// back the partial reply as assistant context, and concatenates the pieces. It
// gives up after maxContinuationRounds and returns what it has (a long-but-
// complete-ish answer beats an error), recording the outcome so a too-small
// token budget is visible in metrics rather than only in a user complaint.
//
// task labels the metric (e.g. "notes", "extract").
func (w *Worker) chatComplete(ctx context.Context, task, model string, msgs []litellm.Message, maxTokens int) (string, error) {
	convo := make([]litellm.Message, 0, len(msgs)+2*maxContinuationRounds)
	convo = append(convo, msgs...)

	var full strings.Builder
	for round := 0; ; round++ {
		res, err := w.ai.Chat(ctx, model, convo, maxTokens)
		if err != nil {
			return "", err
		}
		full.WriteString(res.Content)

		if !res.Truncated {
			if round > 0 {
				w.log.Info("recovered truncated model output via continuation",
					"task", task, "model", model, "rounds", round, "chars", full.Len())
			}
			return full.String(), nil
		}
		if round >= maxContinuationRounds {
			metrics.AIResponsesTruncated.WithLabelValues(task, "final").Inc()
			w.log.Warn("model output still truncated after continuation rounds; returning partial",
				"task", task, "model", model, "max_tokens", maxTokens, "rounds", round, "chars", full.Len())
			return full.String(), nil
		}
		metrics.AIResponsesTruncated.WithLabelValues(task, "continued").Inc()
		convo = append(convo,
			litellm.Message{Role: "assistant", Content: res.Content},
			litellm.Message{Role: "user", Content: continuePrompt},
		)
	}
}
