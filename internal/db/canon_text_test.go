package db

import (
	"strings"
	"testing"
)

func TestWorldEntityCanonText(t *testing.T) {
	e := WorldEntity{
		Kind:        KindNPC,
		Name:        "Captain Varek",
		Description: "Commander of the Eastwatch guard.",
		Metadata:    map[string]any{"role": "commander", "location": "Eastwatch", "status": "ally"},
	}
	got := e.CanonText()

	if !strings.HasPrefix(got, "NPC: Captain Varek") {
		t.Errorf("canon text should lead with kind+name, got:\n%s", got)
	}
	for _, want := range []string{"Commander of the Eastwatch guard.", "Role: commander", "Location: Eastwatch", "Status: ally"} {
		if !strings.Contains(got, want) {
			t.Errorf("canon text missing %q, got:\n%s", want, got)
		}
	}
}

func TestWorldEntityCanonTextNameOnly(t *testing.T) {
	e := WorldEntity{Kind: KindLocation, Name: "Eastwatch"}
	got := e.CanonText()
	if got != "Location: Eastwatch" {
		t.Errorf("name-only canon text = %q, want %q", got, "Location: Eastwatch")
	}
}

func TestWorldEntityCanonTextDeterministic(t *testing.T) {
	// Metadata key order must be stable so re-embedding an unchanged entity
	// produces identical text (avoids needless re-embeds / churn).
	e := WorldEntity{Kind: KindQuest, Name: "Q", Metadata: map[string]any{"z": "1", "a": "2", "m": "3"}}
	first := e.CanonText()
	for i := 0; i < 20; i++ {
		if e.CanonText() != first {
			t.Fatalf("canon text not deterministic across renders")
		}
	}
	// Sorted: A before M before Z.
	ai, mi, zi := strings.Index(first, "A:"), strings.Index(first, "M:"), strings.Index(first, "Z:")
	if ai >= mi || mi >= zi {
		t.Errorf("metadata keys not sorted: %q", first)
	}
}

func TestPlayerCharacterCanonText(t *testing.T) {
	pc := PlayerCharacter{Name: "Ludo", Class: "Wizard", Race: "Half-elf", Level: 3, Notes: "Curious and reckless."}
	got := pc.CanonText()
	for _, want := range []string{"Player character: Ludo", "Level 3 Half-elf Wizard", "Curious and reckless."} {
		if !strings.Contains(got, want) {
			t.Errorf("PC canon text missing %q, got:\n%s", want, got)
		}
	}

	// Name-only PC still renders something useful.
	bare := PlayerCharacter{Name: "Nameless"}
	if got := bare.CanonText(); got != "Player character: Nameless" {
		t.Errorf("bare PC canon text = %q", got)
	}
}
