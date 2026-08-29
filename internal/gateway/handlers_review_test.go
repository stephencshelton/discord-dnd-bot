package gateway

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
)

// TestReviewCustomIDRoundTrip verifies review button/modal custom IDs encode and
// decode losslessly, and that foreign IDs are rejected.
func TestReviewCustomIDRoundTrip(t *testing.T) {
	id := uuid.New()
	for _, verb := range []string{reviewApprove, reviewReject, reviewEdit, reviewSkip, reviewModal} {
		cid := reviewCustomID(verb, id)
		gotVerb, gotID, ok := parseReviewCustomID(cid)
		if !ok {
			t.Fatalf("parseReviewCustomID(%q) not ok", cid)
		}
		if gotVerb != verb {
			t.Errorf("verb = %q, want %q", gotVerb, verb)
		}
		if gotID != id {
			t.Errorf("id = %v, want %v", gotID, id)
		}
	}

	for _, bad := range []string{
		"other:approve:" + id.String(),         // wrong prefix
		reviewIDPrefix + ":approve:not-a-uuid", // bad uuid
		reviewIDPrefix + ":approve",            // missing id
		"campaign_autocomplete",                // unrelated component id
	} {
		if _, _, ok := parseReviewCustomID(bad); ok {
			t.Errorf("parseReviewCustomID(%q) should be rejected", bad)
		}
	}
}

// TestReviewViewRendersActions confirms a proposal renders an embed with the
// expected fields and the four action buttons.
func TestReviewViewRendersActions(t *testing.T) {
	p := db.StateProposal{
		ID:          uuid.New(),
		Action:      db.ActionCreateEntity,
		EntityKind:  db.KindNPC,
		EntityName:  "Captain Varek",
		Patch:       map[string]any{"description": "Commander of the Eastwatch guard.", "role": "commander"},
		Explanation: "New NPC introduced.",
		Evidence:    "Dana introduced him as the commander.",
		Confidence:  0.9,
	}
	embed, components := reviewView(p, 3)
	if embed.Title == "" || len(embed.Fields) == 0 {
		t.Fatalf("embed not populated: %+v", embed)
	}
	if len(components) != 1 {
		t.Fatalf("expected 1 action row, got %d", len(components))
	}
	row, ok := components[0].(discord.ActionRowComponent)
	if !ok {
		t.Fatalf("component is not an action row: %T", components[0])
	}
	if len(row.Components) != 4 {
		t.Errorf("expected 4 buttons (approve/reject/edit/skip), got %d", len(row.Components))
	}
	// The extra structured patch field should be surfaced in a Details field.
	var hasDetails bool
	for _, f := range embed.Fields {
		if f.Name == "Details" {
			hasDetails = true
		}
	}
	if !hasDetails {
		t.Error("expected a Details field surfacing the non-description patch data")
	}
}

// TestEntityKindLabel spot-checks the human labels.
func TestEntityKindLabel(t *testing.T) {
	cases := map[db.WorldEntityKind]string{
		db.KindNPC:      "NPC",
		db.KindLocation: "Location",
		db.KindFaction:  "Faction",
		db.KindQuest:    "Quest",
	}
	for k, want := range cases {
		if got := entityKindLabel(k); got != want {
			t.Errorf("entityKindLabel(%s) = %q, want %q", k, got, want)
		}
	}
}

// TestReviewSessionCommandRegistered ensures the command exists, is guild-only,
// and is NOT permission-gated (open to everyone in the server).
func TestReviewSessionCommandRegistered(t *testing.T) {
	var found bool
	for _, sp := range allCommandSpecs() {
		if sp.def.Name != "review-session" {
			continue
		}
		found = true
		if sp.dm {
			t.Error("review-session should be guild-only (dm=false)")
		}
		if sp.def.DefaultMemberPermissions.OK {
			t.Errorf("review-session should NOT be permission-gated, got %+v", sp.def.DefaultMemberPermissions)
		}
	}
	if !found {
		t.Fatal("review-session command not registered")
	}
}
