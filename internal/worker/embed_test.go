package worker

import (
	"strings"
	"testing"
)

func TestChunkNotesEmpty(t *testing.T) {
	if got := chunkNotes("", 100); got != nil {
		t.Errorf("chunkNotes(\"\") = %v, want nil", got)
	}
	if got := chunkNotes("   \n\n  ", 100); got != nil {
		t.Errorf("chunkNotes(whitespace) = %v, want nil", got)
	}
}

func TestChunkNotesBreaksOnParagraphs(t *testing.T) {
	notes := "## Recap\nThe party fought a dragon.\n\n## Key Events\n- Found the amulet.\n- Escaped the keep."
	chunks := chunkNotes(notes, 40)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d: %v", len(chunks), chunks)
	}
	for _, c := range chunks {
		if strings.TrimSpace(c) == "" {
			t.Error("chunk is empty/whitespace")
		}
	}
}

func TestChunkNotesHardSplitsOversizedParagraph(t *testing.T) {
	big := strings.Repeat("word ", 500) // ~2500 runes, no blank lines
	chunks := chunkNotes(big, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected oversized paragraph to be split, got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > 100 {
			t.Errorf("chunk %d has %d runes, want <= 100", i, n)
		}
	}
}

func TestChunkNotesKeepsSmallNotesWhole(t *testing.T) {
	notes := "A short single paragraph of notes."
	chunks := chunkNotes(notes, 1200)
	if len(chunks) != 1 || chunks[0] != notes {
		t.Fatalf("chunkNotes(small) = %v, want [%q]", chunks, notes)
	}
}
