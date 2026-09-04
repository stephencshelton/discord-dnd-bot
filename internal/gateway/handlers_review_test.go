package gateway

import (
	"strings"
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
	embed, components := reviewView(p, 1, 3)
	if embed.Title == "" || len(embed.Fields) == 0 {
		t.Fatalf("embed not populated: %+v", embed)
	}
	if embed.Footer == nil || !strings.Contains(embed.Footer.Text, "2 of 3") {
		t.Errorf("footer should show the queue position, got %+v", embed.Footer)
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

// TestNextPendingIndex covers the Skip navigation: skipping must move to the
// proposal AFTER the current one (a skipped proposal stays pending, so "show the
// first pending" would redisplay the same card and look like a no-op), wrapping
// to the top past the end.
func TestNextPendingIndex(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	ps := []db.StateProposal{{ID: ids[0]}, {ID: ids[1]}, {ID: ids[2]}}

	if idx, wrapped := nextPendingIndex(ps, ids[0]); idx != 1 || wrapped {
		t.Errorf("skip first: got (%d,%v), want (1,false)", idx, wrapped)
	}
	if idx, wrapped := nextPendingIndex(ps, ids[1]); idx != 2 || wrapped {
		t.Errorf("skip middle: got (%d,%v), want (2,false)", idx, wrapped)
	}
	if idx, wrapped := nextPendingIndex(ps, ids[2]); idx != 0 || !wrapped {
		t.Errorf("skip last: got (%d,%v), want (0,true)", idx, wrapped)
	}
	// Unknown / already-decided proposal: fall back to the head, no wrap note.
	if idx, wrapped := nextPendingIndex(ps, uuid.New()); idx != 0 || wrapped {
		t.Errorf("unknown id: got (%d,%v), want (0,false)", idx, wrapped)
	}
	// A single pending proposal has nowhere to go; it must not claim a wrap.
	if idx, wrapped := nextPendingIndex(ps[:1], ids[0]); idx != 0 || wrapped {
		t.Errorf("single proposal: got (%d,%v), want (0,false)", idx, wrapped)
	}
	if idx, wrapped := nextPendingIndex(nil, ids[0]); idx != 0 || wrapped {
		t.Errorf("empty list: got (%d,%v), want (0,false)", idx, wrapped)
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

// TestSessionRequeueHasProposalsOnlyOption guards the cheap recovery path for a
// session whose transcript/notes are fine but whose world-state extraction
// produced nothing: re-deriving proposals must not require re-transcribing hours
// of audio.
func TestSessionRequeueHasProposalsOnlyOption(t *testing.T) {
	var found bool
	for _, sp := range allCommandSpecs() {
		if sp.def.Name != "session" {
			continue
		}
		for _, opt := range sp.def.Options {
			sub, ok := opt.(discord.ApplicationCommandOptionSubCommand)
			if !ok || sub.Name != "requeue" {
				continue
			}
			for _, o := range sub.Options {
				if b, ok := o.(discord.ApplicationCommandOptionBool); ok && b.Name == "proposals_only" {
					found = true
					if b.Required {
						t.Error("proposals_only must stay optional so a plain requeue still works")
					}
				}
			}
		}
	}
	if !found {
		t.Error("/session requeue is missing the proposals_only option")
	}
}
