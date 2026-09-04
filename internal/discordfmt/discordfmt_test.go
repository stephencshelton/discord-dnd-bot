package discordfmt

import (
	"strings"
	"testing"
)

// TestTruncateBreaksOnWordBoundary is the regression test for trimmed AI replies
// that looked like crashed ones: the old helper cut at an exact rune count, so a
// reply ended mid-word (`…and why Irovalin's soul "wasn't`).
func TestTruncateBreaksOnWordBoundary(t *testing.T) {
	s := "The party returned from behind enemy lines and spent their triumph points wisely."
	got := Truncate(s, 40)

	if !strings.HasSuffix(got, ellipsis) {
		t.Errorf("truncated text should be marked with an ellipsis, got %q", got)
	}
	if len([]rune(got)) > 40 {
		t.Errorf("result exceeds the limit: %d runes (%q)", len([]rune(got)), got)
	}
	// The cut must land between words: the character before the ellipsis should
	// not be mid-word, i.e. the kept text must be a whole-word prefix of s.
	kept := strings.TrimSuffix(got, ellipsis)
	if !strings.HasPrefix(s, kept) {
		t.Fatalf("kept text is not a prefix of the input: %q", kept)
	}
	if rest := s[len(kept):]; rest != "" && !strings.HasPrefix(rest, " ") {
		t.Errorf("cut landed mid-word: kept %q, next %q", kept, rest)
	}
}

func TestTruncateShortInputUnchanged(t *testing.T) {
	s := "Short enough."
	if got := Truncate(s, 100); got != s {
		t.Errorf("Truncate(%q, 100) = %q, want unchanged", s, got)
	}
	// Exactly at the limit is also unchanged (no gratuitous ellipsis).
	if got := Truncate(s, len([]rune(s))); got != s {
		t.Errorf("Truncate at exact limit = %q, want unchanged", got)
	}
}

func TestTruncatePrefersSentenceEnd(t *testing.T) {
	s := "The party arrived at Chevaroth. They descended into the ruined amphitheater immediately."
	got := Truncate(s, 60)
	if !strings.HasPrefix(got, "The party arrived at Chevaroth.") {
		t.Errorf("should keep the complete first sentence, got %q", got)
	}
}

// TestTruncateNoBreakOpportunity covers a single enormous token: there's nothing
// to break on, so a hard cut is correct — but it must still be bounded.
func TestTruncateNoBreakOpportunity(t *testing.T) {
	s := strings.Repeat("a", 100)
	got := Truncate(s, 10)
	if n := len([]rune(got)); n != 10 {
		t.Errorf("got %d runes, want exactly 10 (%q)", n, got)
	}
	if !strings.HasSuffix(got, ellipsis) {
		t.Errorf("expected ellipsis marker, got %q", got)
	}
	if got := Truncate(s, 0); got != "" {
		t.Errorf("Truncate(_, 0) = %q, want empty", got)
	}
}

// TestChunkMarkdownNeverSplitsALine guards Markdown rendering: a heading or
// bullet cut in half stops rendering and reads as garbage.
func TestChunkMarkdownNeverSplitsALine(t *testing.T) {
	lines := []string{
		"## Recap",
		"The party returned from behind enemy lines.",
		"",
		"## Key Events",
		"- They returned Irovalin alive to friendly territory.",
		"- They spent four triumph points on the war interludes.",
		"- Prince Delamian assigned the hunt for the Spore Queen.",
	}
	chunks := ChunkMarkdown(strings.Join(lines, "\n"), 100)
	if len(chunks) < 2 {
		t.Fatalf("expected a split, got %d chunk(s)", len(chunks))
	}
	for _, c := range chunks {
		if n := len([]rune(c)); n > 100 {
			t.Errorf("chunk exceeds size: %d runes", n)
		}
	}
	joined := strings.Join(chunks, "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.Contains(joined, line) {
			t.Errorf("line was split or lost: %q", line)
		}
	}
}

func TestChunkMarkdownEdgeCases(t *testing.T) {
	if got := ChunkMarkdown("  \n\n ", 100); got != nil {
		t.Errorf("blank input should yield no chunks, got %q", got)
	}
	if got := ChunkMarkdown("one line", 100); len(got) != 1 || got[0] != "one line" {
		t.Errorf("short input should be a single chunk, got %q", got)
	}
	// An over-long single line has no break opportunity: hard-split, nothing lost.
	long := strings.Repeat("x", 250)
	chunks := ChunkMarkdown(long, 100)
	if strings.Join(chunks, "") != long {
		t.Error("hard-split lost or reordered content")
	}
	if len(chunks) != 3 {
		t.Errorf("expected 3 pieces, got %d", len(chunks))
	}
}

// TestChunkMarkdownSplitsLongProseOnWords covers the realistic case: a long PROSE
// paragraph is a single line, so it must be broken between words. Slicing it at an
// exact rune count splits a word across two messages, which reads as corrupted
// text — the same false "it crashed" signal as a mid-word truncation.
func TestChunkMarkdownSplitsLongProseOnWords(t *testing.T) {
	para := strings.Repeat("the party pressed deeper into the ruined amphitheater. ", 100)
	chunks := ChunkMarkdown(para, 300)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > 300 {
			t.Errorf("chunk %d = %d runes, over the limit", i, n)
		}
		// No chunk may start or end mid-word.
		if strings.HasPrefix(c, " ") || strings.HasSuffix(c, " ") {
			t.Errorf("chunk %d has ragged whitespace: %q", i, c)
		}
	}
	// Nothing lost: normalizing whitespace must reconstruct the original.
	got := strings.Join(strings.Fields(strings.Join(chunks, " ")), " ")
	want := strings.Join(strings.Fields(para), " ")
	if got != want {
		t.Error("content was lost or reordered when splitting prose")
	}
	// Each boundary must fall between words, i.e. every chunk is whole words.
	for _, c := range chunks {
		for _, w := range strings.Fields(c) {
			if w != "the" && w != "party" && w != "pressed" && w != "deeper" &&
				w != "into" && w != "ruined" && w != "amphitheater." {
				t.Errorf("found a split word %q", w)
			}
		}
	}
}

// TestSplitWordsHardSplitsUnbreakableToken ensures a single token longer than the
// limit (a URL, say) still gets emitted rather than looping forever.
func TestSplitWordsHardSplitsUnbreakableToken(t *testing.T) {
	tok := strings.Repeat("z", 250)
	got := SplitWords(tok, 100)
	if strings.Join(got, "") != tok {
		t.Error("unbreakable token lost content")
	}
	for _, p := range got {
		if len([]rune(p)) > 100 {
			t.Errorf("piece = %d runes, over the limit", len([]rune(p)))
		}
	}
	// Short input passes through untouched.
	if got := SplitWords("small", 100); len(got) != 1 || got[0] != "small" {
		t.Errorf("SplitWords should pass short input through, got %q", got)
	}
}

func TestHardSplit(t *testing.T) {
	if got := HardSplit("abcdefg", 3); strings.Join(got, "") != "abcdefg" || len(got) != 3 {
		t.Errorf("HardSplit = %q", got)
	}
	if got := HardSplit("abc", 0); len(got) != 1 || got[0] != "abc" {
		t.Errorf("HardSplit with size 0 should passthrough, got %q", got)
	}
}

// TestLimitsMatchDiscord pins the documented Discord limits; ChunkLimit must stay
// under the message limit to leave room for newline joins and prefixes.
func TestLimitsMatchDiscord(t *testing.T) {
	if MessageLimit != 2000 {
		t.Errorf("MessageLimit = %d, want 2000", MessageLimit)
	}
	if EmbedDescriptionLimit != 4096 {
		t.Errorf("EmbedDescriptionLimit = %d, want 4096", EmbedDescriptionLimit)
	}
	if ChunkLimit >= MessageLimit {
		t.Errorf("ChunkLimit (%d) must be below MessageLimit (%d)", ChunkLimit, MessageLimit)
	}
}
