package gateway

import (
	"strings"
	"testing"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
)

// TestMergeAskPassagesOrdersByDistanceAndLabels checks that canon and transcript
// results are interleaved by ascending distance and tagged by source so the
// answer model can prefer curated canon.
func TestMergeAskPassagesOrdersByDistanceAndLabels(t *testing.T) {
	transcripts := []db.RetrievedChunk{
		{Content: "the party fought a dragon", Distance: 0.30},
		{Content: "they camped for the night", Distance: 0.50},
	}
	canon := []db.RetrievedChunk{
		{Content: "NPC: Captain Varek", Distance: 0.10},
		{Content: "Quest: Missing Caravan", Distance: 0.40},
	}
	got := mergeAskPassages(transcripts, canon)
	if len(got) != 4 {
		t.Fatalf("expected 4 passages, got %d", len(got))
	}
	// Closest first: canon 0.10, transcript 0.30, canon 0.40, transcript 0.50.
	if !strings.HasPrefix(got[0], "[Campaign canon] ") || !strings.Contains(got[0], "Captain Varek") {
		t.Errorf("passage[0] = %q, want closest canon first", got[0])
	}
	if !strings.HasPrefix(got[1], "[Session record] ") || !strings.Contains(got[1], "dragon") {
		t.Errorf("passage[1] = %q, want transcript at 0.30", got[1])
	}
	if !strings.HasPrefix(got[2], "[Campaign canon] ") || !strings.Contains(got[2], "Missing Caravan") {
		t.Errorf("passage[2] = %q, want canon at 0.40", got[2])
	}
	if !strings.HasPrefix(got[3], "[Session record] ") {
		t.Errorf("passage[3] = %q, want transcript last", got[3])
	}
}

// TestMergeAskPassagesCap bounds the total passages sent to the model.
func TestMergeAskPassagesCap(t *testing.T) {
	var transcripts []db.RetrievedChunk
	for i := 0; i < 20; i++ {
		transcripts = append(transcripts, db.RetrievedChunk{Content: "t", Distance: float64(i)})
	}
	var canon []db.RetrievedChunk
	for i := 0; i < 20; i++ {
		canon = append(canon, db.RetrievedChunk{Content: "c", Distance: float64(i) + 0.5})
	}
	if got := len(mergeAskPassages(transcripts, canon)); got > 14 {
		t.Errorf("merged passages = %d, want capped at 14", got)
	}
}

// TestMergeAskPassagesEmpty handles no results.
func TestMergeAskPassagesEmpty(t *testing.T) {
	if got := mergeAskPassages(nil, nil); len(got) != 0 {
		t.Errorf("empty merge = %v, want nil/empty", got)
	}
}

// TestIsActiveQuest covers status interpretation for /prep.
func TestIsActiveQuest(t *testing.T) {
	active := []string{"", "active", "in progress", "started", "ongoing"}
	for _, s := range active {
		e := db.WorldEntity{Kind: db.KindQuest, Metadata: map[string]any{"status": s}}
		if !isActiveQuest(e) {
			t.Errorf("status %q should be active", s)
		}
	}
	done := []string{"completed", "Complete", "DONE", "failed", "resolved", "closed", "abandoned"}
	for _, s := range done {
		e := db.WorldEntity{Kind: db.KindQuest, Metadata: map[string]any{"status": s}}
		if isActiveQuest(e) {
			t.Errorf("status %q should NOT be active", s)
		}
	}
	// No metadata at all -> active.
	if !isActiveQuest(db.WorldEntity{Kind: db.KindQuest}) {
		t.Error("quest with no status should be active")
	}
}

// TestEntityPrepLine renders name + description + metadata summary.
func TestEntityPrepLine(t *testing.T) {
	e := db.WorldEntity{
		Kind: db.KindQuest, Name: "Missing Caravan",
		Description: "Find the lost caravan.",
		Metadata:    map[string]any{"status": "active", "objective": "return survivors"},
	}
	line := entityPrepLine(e)
	if !strings.Contains(line, "Missing Caravan") || !strings.Contains(line, "Find the lost caravan.") {
		t.Errorf("prep line missing core fields: %q", line)
	}
	if !strings.Contains(line, "status: active") {
		t.Errorf("prep line missing metadata summary: %q", line)
	}
}

// TestPrepCommandRegistered ensures /prep exists and is DM-capable.
func TestPrepCommandRegistered(t *testing.T) {
	var found, dm bool
	for _, sp := range allCommandSpecs() {
		if sp.def.Name == "prep" {
			found = true
			dm = sp.dm
		}
	}
	if !found {
		t.Fatal("prep command not registered")
	}
	if !dm {
		t.Error("prep should be DM-capable (dm=true)")
	}
}
