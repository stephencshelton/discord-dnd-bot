package gateway

import (
	"strings"
	"testing"

	"github.com/stephencshelton/discord-dnd-bot/internal/discordfmt"
)

// TestMarkTruncatedOnlyWhenCut ensures a complete reply is never decorated (that
// would be alarming noise on every normal answer) and a cut one always is.
func TestMarkTruncatedOnlyWhenCut(t *testing.T) {
	answer := "The Spore Queen wants to weaponize souls."
	if got := markTruncated(answer, false); got != answer {
		t.Errorf("complete answer should pass through unchanged, got %q", got)
	}

	got := markTruncated(answer, true)
	if !strings.HasPrefix(got, answer) {
		t.Errorf("marker must be appended, not replace content: %q", got)
	}
	if !strings.Contains(got, "cut off") {
		t.Errorf("marker should explain the cut, got %q", got)
	}
}

// TestTruncateForDiscordFitsMessageLimit checks the display ceilings still hold
// after delegating to discordfmt, and that the cut is word-aligned rather than
// mid-word (a mid-word cut is what made a trimmed answer look like a crash).
func TestTruncateForDiscordFitsMessageLimit(t *testing.T) {
	long := strings.Repeat("the party travelled onward ", 200) // ~5400 chars
	msg := truncateForDiscord(long)
	if n := len([]rune(msg)); n > discordfmt.MessageLimit {
		t.Errorf("message = %d runes, want <= %d", n, discordfmt.MessageLimit)
	}
	if strings.HasSuffix(strings.TrimSuffix(msg, "…"), "travell") {
		t.Error("cut landed mid-word")
	}

	embed := truncateForEmbed(long)
	if n := len([]rune(embed)); n > discordfmt.EmbedDescriptionLimit {
		t.Errorf("embed = %d runes, want <= %d", n, discordfmt.EmbedDescriptionLimit)
	}
	// The embed surface is larger, so it must retain more than the message one.
	if len([]rune(embed)) <= len([]rune(msg)) {
		t.Error("embed limit should allow more content than the message limit")
	}

	// Short content is untouched on both surfaces.
	const short = "A brief answer."
	if truncateForDiscord(short) != short || truncateForEmbed(short) != short {
		t.Error("short content should not be modified")
	}
}
