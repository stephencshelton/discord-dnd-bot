package prompts

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSessionNotesUser(t *testing.T) {
	got := SessionNotesUser("Rise of the Runelords", "Pathfinder", "goblins attack",
		"Monday, 1 January 2024 19:00 UTC", []string{"Alice", "Bob"},
		[]string{"Kessa — Lv 3 Elf Rogue, played by Alice"}, []string{"[npc] Ameiko — innkeeper"}, "we fought goblins")

	for _, want := range []string{
		"Campaign: Rise of the Runelords",
		"Game system: Pathfinder",
		"Premise: goblins attack",
		"Session date: Monday, 1 January 2024 19:00 UTC",
		"Participants (Discord voice call): Alice, Bob",
		"Player characters (correct spellings):\n- Kessa — Lv 3 Elf Rogue, played by Alice",
		"Known people, places and factions (correct spellings):\n- [npc] Ameiko — innkeeper",
		"## Recap",
		"## Key Events",
		"## Open Threads / Cliffhangers",
		"<<<TRANSCRIPT",
		"we fought goblins",
		"TRANSCRIPT>>>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestSessionNotesUserDefaults(t *testing.T) {
	got := SessionNotesUser("", "  ", "", "", nil, nil, nil, "transcript")
	if !strings.Contains(got, "Campaign: Untitled Campaign") {
		t.Errorf("expected default campaign name, got:\n%s", got)
	}
	if !strings.Contains(got, "Game system: unspecified") {
		t.Errorf("expected default system, got:\n%s", got)
	}
	if strings.Contains(got, "Premise:") {
		t.Errorf("empty premise should be omitted, got:\n%s", got)
	}
	if strings.Contains(got, "Session date:") {
		t.Errorf("empty session date should be omitted, got:\n%s", got)
	}
	if strings.Contains(got, "Participants") {
		t.Errorf("empty participants should be omitted, got:\n%s", got)
	}
	if strings.Contains(got, "correct spellings") {
		t.Errorf("empty rosters should be omitted, got:\n%s", got)
	}
}

// TestSessionNotesSystemCorrectsNames pins the rule that makes the roster
// useful: without it the model treats the list as background and keeps the
// transcript's misheard spellings ("Brock" for Throk).
func TestSessionNotesSystemCorrectsNames(t *testing.T) {
	for _, want := range []string{"speech-to-text", "always use them", "played by", "Do not merge"} {
		if !strings.Contains(SessionNotesSystem, want) {
			t.Errorf("SessionNotesSystem missing %q", want)
		}
	}
}

// TestTranscriptionVocabulary covers the Whisper spelling hint: order kept,
// duplicates dropped case-insensitively, blanks skipped, empty input sends no
// prompt at all.
func TestTranscriptionVocabulary(t *testing.T) {
	got := TranscriptionVocabulary([]string{"Throk Bloodrend", " Chak ", "", "chak", "Zoldanox"})
	if want := "Throk Bloodrend, Chak, Zoldanox."; got != want {
		t.Errorf("TranscriptionVocabulary = %q, want %q", got, want)
	}
	if got := TranscriptionVocabulary(nil); got != "" {
		t.Errorf("empty input = %q, want no prompt", got)
	}
}

// TestTranscriptionVocabularyBound protects the PC names at the front of the
// hint: faster-whisper keeps only the TAIL of an over-long prompt, so the list
// must fit the window itself, dropping whole names from the end.
func TestTranscriptionVocabularyBound(t *testing.T) {
	names := []string{"Throk Bloodrend"}
	for i := 0; i < 200; i++ {
		names = append(names, fmt.Sprintf("Npcname%03d", i))
	}
	got := TranscriptionVocabulary(names)
	if n := utf8.RuneCountInString(got); n > transcriptionVocabMaxRunes {
		t.Errorf("vocabulary is %d runes, bound is %d", n, transcriptionVocabMaxRunes)
	}
	if !strings.HasPrefix(got, "Throk Bloodrend, ") || !strings.HasSuffix(got, ".") {
		t.Errorf("vocabulary = %q", got)
	}
	for _, part := range strings.Split(strings.TrimSuffix(got, "."), ", ") {
		if part != "Throk Bloodrend" && len(part) != len("Npcname000") {
			t.Errorf("name was cut mid-word: %q", part)
		}
	}
}

func TestLoreUser(t *testing.T) {
	got := LoreUser("Waterdeep", "5e", "heist in progress", "Who guards the vault?")
	want := "Campaign: Waterdeep | System: 5e | Premise: heist in progress\n\nRequest: Who guards the vault?"
	if got != want {
		t.Errorf("LoreUser = %q, want %q", got, want)
	}
}

func TestLoreUserDefaultsAndNoPremise(t *testing.T) {
	got := LoreUser("", "", "", "Anything?")
	if strings.Contains(got, "Premise:") {
		t.Errorf("empty premise should be omitted, got %q", got)
	}
	if !strings.Contains(got, "Campaign: Untitled | System: unspecified") {
		t.Errorf("expected defaults, got %q", got)
	}
}

func TestRecapUser(t *testing.T) {
	got := RecapUser("Descent", []string{"first note", "second note"})
	if !strings.Contains(got, "next session of Descent") {
		t.Errorf("missing campaign name, got:\n%s", got)
	}
	if !strings.Contains(got, "--- Session 1 ---\nfirst note") {
		t.Errorf("missing session 1, got:\n%s", got)
	}
	if !strings.Contains(got, "--- Session 2 ---\nsecond note") {
		t.Errorf("missing session 2, got:\n%s", got)
	}
}

func TestRecapUserDefaultName(t *testing.T) {
	got := RecapUser("", nil)
	if !strings.Contains(got, "next session of our campaign") {
		t.Errorf("expected default campaign name, got:\n%s", got)
	}
}

// TestRecapSystemForbidsInvention protects the rule that /recap restates the
// record rather than extending it. /recap once ran under LoreSystem, which
// instructs the model to invent NPCs, locations and plot hooks; its output is
// read aloud at the table as the canonical account of last session, so anything
// invented there becomes campaign fact (AGENTS.md §8).
func TestRecapSystemForbidsInvention(t *testing.T) {
	sys := strings.ToLower(RecapSystem)
	for _, want := range []string{
		"only from the session notes",
		"never invent",
		"say less", // thin notes produce less, not filler
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("recap system prompt missing grounding rule mentioning %q", want)
		}
	}
}

// TestRecapSystemDemandsDetailNotProse is the regression test for a recap that
// was grounded but useless: told only not to invent, while still cast as a
// "storyteller" free to "dramatize the telling", the model returned atmosphere
// with almost nothing a player could act on. The prompt must ask for concrete
// recall and must not re-acquire the flourish vocabulary that caused this.
func TestRecapSystemDemandsDetailNotProse(t *testing.T) {
	sys := strings.ToLower(RecapSystem)

	// It must positively ask for the things a player needs to act on.
	for _, want := range []string{
		"recall, not storytelling",
		"unresolved",
		"name names",
		"specifics",
		"bullets",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("recap system prompt should demand concrete detail, missing %q", want)
		}
	}

	// And it must not invite the narration that made the output unusable. These
	// words may appear only as part of an explicit prohibition.
	for _, banned := range []string{"dramatize", "storyteller,", "evocative", "in-genre"} {
		if strings.Contains(sys, banned) {
			t.Errorf("recap system prompt re-acquired flourish instruction %q", banned)
		}
	}
	for _, mustForbid := range []string{"no dramatic narration", "skip scene-setting"} {
		if !strings.Contains(sys, mustForbid) {
			t.Errorf("recap system prompt should explicitly forbid prose, missing %q", mustForbid)
		}
	}
}

// TestRecapLengthBoundStatedOnce guards against the conflicting-instruction bug
// this replaced: LoreSystem said "under 250 words" while RecapUser said "max 150
// words", so the model was handed two different limits in one request. The bound
// belongs in the system prompt only, and it is tight on purpose — a small budget
// is what forces the model to spend it on facts rather than framing.
func TestRecapLengthBoundStatedOnce(t *testing.T) {
	if !strings.Contains(RecapSystem, "120 words") {
		t.Error("RecapSystem should carry the length bound")
	}
	if strings.Contains(RecapUser("Descent", []string{"a note"}), "words") {
		t.Error("RecapUser should not restate a word limit; it belongs in RecapSystem")
	}
}

// TestRecapUserStaysNeutral pins the other half of the fix: the user prompt used
// to ask for a "dramatic" recap, which pulled the output back toward prose even
// after the system prompt was tightened. Framing belongs in RecapSystem.
func TestRecapUserStaysNeutral(t *testing.T) {
	got := strings.ToLower(RecapUser("Descent", []string{"a note"}))
	for _, banned := range []string{"dramatic", "previously, on"} {
		if strings.Contains(got, banned) {
			t.Errorf("RecapUser should not carry narrative framing, found %q in:\n%s", banned, got)
		}
	}
}

func TestArtPromptStyleSwitch(t *testing.T) {
	cases := []struct {
		name       string
		system     string
		scene      string
		wantStyle  string
		otherStyle string
	}{
		{"fantasy default", "D&D 5e", "a dragon", "digital fantasy illustration", "cyberpunk"},
		{"cyberpunk", "Cyberpunk RED", "a street", "neon cyberpunk concept art", "digital fantasy illustration"},
		{"shadowrun", "Shadowrun 6e", "a bar", "neon cyberpunk concept art", "digital fantasy illustration"},
		{"case insensitive", "CYBERPUNK", "x", "neon cyberpunk concept art", "digital fantasy illustration"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ArtPrompt(c.system, c.scene)
			if !strings.Contains(got, c.scene) {
				t.Errorf("prompt missing scene %q: %s", c.scene, got)
			}
			if !strings.Contains(got, c.wantStyle) {
				t.Errorf("prompt missing style %q: %s", c.wantStyle, got)
			}
			if strings.Contains(got, c.otherStyle) {
				t.Errorf("prompt should not contain style %q: %s", c.otherStyle, got)
			}
			if !strings.Contains(got, "No text or watermarks") {
				t.Errorf("prompt missing watermark guard: %s", got)
			}
		})
	}
}

func TestNonEmpty(t *testing.T) {
	if got := nonEmpty("value", "fallback"); got != "value" {
		t.Errorf("nonEmpty(value) = %q, want value", got)
	}
	if got := nonEmpty("   ", "fallback"); got != "fallback" {
		t.Errorf("nonEmpty(blank) = %q, want fallback", got)
	}
	if got := nonEmpty("", "fallback"); got != "fallback" {
		t.Errorf("nonEmpty(empty) = %q, want fallback", got)
	}
}
