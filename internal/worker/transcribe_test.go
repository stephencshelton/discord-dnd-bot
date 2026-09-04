package worker

import (
	"strings"
	"testing"
)

// TestChunkMarkdownPreservesLines is the regression test for unreadable session
// notes: the notes are posted as chat messages (Discord soft-wraps those; its
// inline preview of an attached .md file does NOT wrap), and a chunk boundary
// must never land mid-line or the Markdown on either side of the split stops
// rendering (a heading becomes plain text, a bullet loses its bullet).
func TestChunkMarkdownPreservesLines(t *testing.T) {
	notes := strings.Join([]string{
		"## Recap",
		"The party returned from behind enemy lines and spent their triumph points.",
		"",
		"## Key Events",
		"- They returned Irovalin alive to friendly territory.",
		"- They spent four triumph points to resolve all four war interludes.",
		"- Prince Delamian assigned the hunt for the Spore Queen.",
		"",
		"## Open Threads",
		"- Combat with the Tanglebriar Regents is still underway.",
	}, "\n")

	chunks := chunkMarkdown(notes, 120)
	if len(chunks) < 2 {
		t.Fatalf("expected the notes to split across messages, got %d chunk(s)", len(chunks))
	}

	// Every original line must appear intact in exactly one chunk (no line split
	// across a boundary), and no chunk may exceed the limit.
	for _, c := range chunks {
		if n := len([]rune(c)); n > 120 {
			t.Errorf("chunk exceeds limit: %d runes", n)
		}
	}
	rejoined := strings.Join(chunks, "\n")
	for _, line := range strings.Split(notes, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.Contains(rejoined, line) {
			t.Errorf("line was split or lost across chunks: %q", line)
		}
	}
}

// TestChunkMarkdownEdgeCases covers empty input and a single line longer than the
// limit (which has no wrap opportunity and must be hard-split rather than dropped).
func TestChunkMarkdownEdgeCases(t *testing.T) {
	if got := chunkMarkdown("   \n\n  ", 100); got != nil {
		t.Errorf("blank notes should produce no messages, got %q", got)
	}
	if got := chunkMarkdown("short notes", 100); len(got) != 1 || got[0] != "short notes" {
		t.Errorf("short notes should be one chunk, got %q", got)
	}

	long := strings.Repeat("a", 250)
	chunks := chunkMarkdown(long, 100)
	if len(chunks) != 3 {
		t.Fatalf("expected an over-long line to be hard-split into 3, got %d", len(chunks))
	}
	if strings.Join(chunks, "") != long {
		t.Error("hard-split lost or reordered content")
	}
}
