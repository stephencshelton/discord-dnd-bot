package gateway

import (
	"testing"
	"time"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
)

// TestDeferredTimeoutOutlastsAIRequest guards the bug that made /prep fail with
// "context deadline exceeded" at exactly 15s: the handler deadline must outlast
// the litellm client's own per-request timeout, so a slow model reply is bounded
// by the AI client (which errors cleanly and retries) rather than cut off
// mid-request by the interaction context.
func TestDeferredTimeoutOutlastsAIRequest(t *testing.T) {
	cfg := &config.Config{}
	cfg.LiteLLM.RequestTimeout = 120 * time.Second
	g := &Gateway{cfg: cfg}

	got := g.deferredTimeout()
	if got <= cfg.LiteLLM.RequestTimeout {
		t.Fatalf("deferredTimeout() = %s, want more than the LiteLLM request timeout %s",
			got, cfg.LiteLLM.RequestTimeout)
	}
	if got <= interactionTimeout {
		t.Fatalf("deferredTimeout() = %s, want more than interactionTimeout %s", got, interactionTimeout)
	}
	// Discord allows ~15 minutes to follow up on a deferred interaction; staying
	// well inside that keeps the followup edit from being rejected.
	if got >= 15*time.Minute {
		t.Fatalf("deferredTimeout() = %s, want well under Discord's 15m followup window", got)
	}
}

// TestDeferredTimeoutUnconfigured falls back to the short bound rather than
// zero (an immediately-expired context) when no LiteLLM timeout is configured.
func TestDeferredTimeoutUnconfigured(t *testing.T) {
	if got := (&Gateway{}).deferredTimeout(); got != interactionTimeout {
		t.Fatalf("deferredTimeout() with no config = %s, want %s", got, interactionTimeout)
	}
	if got := (&Gateway{cfg: &config.Config{}}).deferredTimeout(); got != interactionTimeout {
		t.Fatalf("deferredTimeout() with zero timeout = %s, want %s", got, interactionTimeout)
	}
}

// TestDeferredBudgetsFitDiscordFollowupWindow checks the non-AI post-defer
// budgets: each must outlast the 15s initial-response bound (the work they wait
// on — a DAVE/UDP handshake, an S3 purge of every session's chunks — routinely
// does) while staying well inside Discord's ~15 minute followup window, after
// which the followup edit would be rejected and the user left with a silent
// "thinking..." message.
func TestDeferredBudgetsFitDiscordFollowupWindow(t *testing.T) {
	budgets := map[string]time.Duration{
		"deferredVoiceTimeout": deferredVoiceTimeout,
		"deferredPurgeTimeout": deferredPurgeTimeout,
	}
	for name, d := range budgets {
		if d <= interactionTimeout {
			t.Errorf("%s = %s, want more than interactionTimeout %s", name, d, interactionTimeout)
		}
		if d >= 15*time.Minute {
			t.Errorf("%s = %s, want well under Discord's 15m followup window", name, d)
		}
	}
	// Purging every session's audio is the slowest of the two and must not be
	// cut short: a partial purge orphans objects in storage.
	if deferredPurgeTimeout <= deferredVoiceTimeout {
		t.Errorf("deferredPurgeTimeout = %s, want more than deferredVoiceTimeout %s",
			deferredPurgeTimeout, deferredVoiceTimeout)
	}
}
