package gateway

import (
	"fmt"
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

// TestEmbedReplyKeepsTailAndMarker is the regression test for a /prep that was
// silently incomplete: the reply was cut at the embed limit AFTER the "cut off"
// marker had been appended, so the marker was thrown away with the tail. A long
// reply must keep every line (embed + overflow), the marker must survive at the
// true end, and a short reply must stay entirely in the embed.
func TestEmbedReplyKeepsTailAndMarker(t *testing.T) {
	var lines []string
	for i := 0; i < 120; i++ {
		lines = append(lines, fmt.Sprintf("- open thread %03d: the party still has to deal with this", i))
	}
	long := strings.Join(lines, "\n")

	desc, overflow := embedReply(long, true)
	if n := len([]rune(desc)); n > discordfmt.EmbedDescriptionLimit {
		t.Errorf("description = %d runes, want <= %d", n, discordfmt.EmbedDescriptionLimit)
	}
	if overflow == "" {
		t.Fatal("a reply longer than one embed must continue in overflow, not be cut")
	}
	all := desc + "\n" + overflow
	for _, l := range []string{lines[0], lines[len(lines)/2], lines[len(lines)-1]} {
		if !strings.Contains(all, l) {
			t.Errorf("line lost across the split: %q", l)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(overflow), "(cut off — the reply hit its length limit)_") {
		t.Errorf("truncation marker must end the overflow, got tail %q", overflow[max(0, len(overflow)-80):])
	}

	if d, o := embedReply("A brief answer.", false); d != "A brief answer." || o != "" {
		t.Errorf("short reply = (%q, %q), want it whole in the embed", d, o)
	}
}
