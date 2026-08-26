package prompts

import (
	"strings"
	"testing"
)

func TestSessionNotesUser(t *testing.T) {
	got := SessionNotesUser("Rise of the Runelords", "Pathfinder", "goblins attack",
		"Monday, 1 January 2024 19:00 UTC", []string{"Alice", "Bob"}, "we fought goblins")

	for _, want := range []string{
		"Campaign: Rise of the Runelords",
		"Game system: Pathfinder",
		"Premise: goblins attack",
		"Session date: Monday, 1 January 2024 19:00 UTC",
		"Participants (Discord voice call): Alice, Bob",
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
	got := SessionNotesUser("", "  ", "", "", nil, "transcript")
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
	if !strings.Contains(got, `Previously, on Descent...`) {
		t.Errorf("missing recap header, got:\n%s", got)
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
	if !strings.Contains(got, "Previously, on our campaign...") {
		t.Errorf("expected default campaign name, got:\n%s", got)
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
