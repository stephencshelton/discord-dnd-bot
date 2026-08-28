package extract

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
)

func props(confidences ...float64) []db.StateProposal {
	out := make([]db.StateProposal, len(confidences))
	for i, c := range confidences {
		out[i] = db.StateProposal{
			CampaignID: uuid.New(),
			EntityKind: db.KindNPC,
			EntityName: string(rune('A' + i)),
			Confidence: c,
			Patch:      map[string]any{"description": "d"},
		}
	}
	return out
}

func TestFilterByConfidence(t *testing.T) {
	in := props(0.1, 0.4, 0.39, 0.9)

	// Floor of 0.4 keeps >= 0.4 (0.4 and 0.9).
	got := FilterByConfidence(in, 0.4)
	if len(got) != 2 {
		t.Fatalf("expected 2 kept, got %d", len(got))
	}
	for _, p := range got {
		if p.Confidence < 0.4 {
			t.Errorf("kept below-floor proposal: %v", p.Confidence)
		}
	}

	// Floor of 0 disables filtering.
	if got := FilterByConfidence(in, 0); len(got) != len(in) {
		t.Errorf("min<=0 should keep all, got %d", len(got))
	}

	// Does not mutate the caller's slice.
	_ = FilterByConfidence(in, 0.99)
	if len(in) != 4 {
		t.Errorf("input slice was mutated: len=%d", len(in))
	}
}

func TestApplyCriticKeep(t *testing.T) {
	in := props(0.9, 0.9, 0.9)

	// Keep indices 0 and 2.
	got := ApplyCriticKeep(in, `{"keep":[0,2]}`)
	if len(got) != 2 || got[0].EntityName != "A" || got[1].EntityName != "C" {
		t.Fatalf("keep [0,2] wrong: %+v", got)
	}

	// Empty keep => drop everything.
	if got := ApplyCriticKeep(in, `{"keep":[]}`); len(got) != 0 {
		t.Errorf("empty keep should drop all, got %d", len(got))
	}

	// Out-of-range indices ignored.
	if got := ApplyCriticKeep(in, `{"keep":[0,99,-1]}`); len(got) != 1 || got[0].EntityName != "A" {
		t.Errorf("out-of-range indices not ignored: %+v", got)
	}

	// Tolerates prose/fence wrapping.
	if got := ApplyCriticKeep(in, "Sure!\n```json\n{\"keep\":[1]}\n```"); len(got) != 1 || got[0].EntityName != "B" {
		t.Errorf("wrapped critic JSON not parsed: %+v", got)
	}
}

// TestApplyCriticKeepFailsOpen keeps all proposals when the critic output is
// unparseable — better to over-surface for review than silently drop everything.
func TestApplyCriticKeepFailsOpen(t *testing.T) {
	in := props(0.9, 0.9)
	for _, bad := range []string{"not json", "", "{broken", "{\"other\":true}"} {
		got := ApplyCriticKeep(in, bad)
		// "{\"other\":true}" parses as JSON with no "keep" => drops all (valid),
		// so only assert fail-open for the truly-unparseable cases.
		if bad == "{\"other\":true}" {
			continue
		}
		if len(got) != len(in) {
			t.Errorf("ApplyCriticKeep(%q) should fail open (keep all), got %d", bad, len(got))
		}
	}
}

func TestCriticCandidateSummary(t *testing.T) {
	p := db.StateProposal{
		EntityKind:  db.KindQuest,
		EntityName:  "Missing Caravan",
		Patch:       map[string]any{"description": "Find the caravan."},
		Explanation: "New quest.",
		Evidence:    "Dana asked for help.",
		Confidence:  0.8,
	}
	s := CriticCandidate(p)
	for _, want := range []string{"quest", "Missing Caravan", "Find the caravan.", "Dana asked for help.", "0.80"} {
		if !strings.Contains(s, want) {
			t.Errorf("candidate summary missing %q:\n%s", want, s)
		}
	}
}
