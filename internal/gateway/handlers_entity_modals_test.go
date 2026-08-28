package gateway

import (
	"sort"
	"testing"

	"github.com/disgoorg/disgo/discord"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
)

// modalFieldIDs returns the text-input custom IDs in a modal, in order, so tests
// can assert the structured form has the expected fields.
func modalFieldIDs(m discord.ModalCreate) []string {
	var ids []string
	for _, c := range m.Components {
		label, ok := c.(discord.LabelComponent)
		if !ok {
			continue
		}
		if ti, ok := label.Component.(discord.TextInputComponent); ok {
			ids = append(ids, ti.GetCustomID())
		}
	}
	return ids
}

// modalFieldValues maps each text-input custom ID to its prefilled value.
func modalFieldValues(m discord.ModalCreate) map[string]string {
	out := map[string]string{}
	for _, c := range m.Components {
		if label, ok := c.(discord.LabelComponent); ok {
			if ti, ok := label.Component.(discord.TextInputComponent); ok {
				out[ti.GetCustomID()] = ti.Value
			}
		}
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestWorldAddModalKindFields ensures each world kind's form exposes Name +
// Description plus its kind-specific structured metadata fields, and that the
// modal custom ID round-trips the kind.
func TestWorldAddModalKindFields(t *testing.T) {
	cases := map[db.WorldEntityKind][]string{
		db.KindNPC:      {"name", "role", "location", "status", "description"},
		db.KindLocation: {"name", "region", "features", "description"},
		db.KindFaction:  {"name", "goals", "members", "status", "description"},
		db.KindQuest:    {"name", "status", "objective", "giver", "description"},
		db.KindHook:     {"name", "status", "related", "description"},
	}
	for kind, want := range cases {
		m := worldAddModal(kind)
		if m.CustomID != worldAddModalPrefix+":"+string(kind) {
			t.Errorf("%s: modal custom id = %q", kind, m.CustomID)
		}
		got := modalFieldIDs(m)
		for _, f := range want {
			if !contains(got, f) {
				t.Errorf("%s modal missing field %q (have %v)", kind, f, got)
			}
		}
		// The metadata field list must be a subset of the modal's fields (minus
		// name/description) so the submit handler reads exactly what's shown.
		for _, f := range worldMetadataFields(kind) {
			if !contains(got, f) {
				t.Errorf("%s: worldMetadataFields has %q but modal doesn't show it", kind, f)
			}
			if f == "name" || f == "description" {
				t.Errorf("%s: metadata field %q must not include name/description", kind, f)
			}
		}
	}
}

// TestWorldEditModalPrefillsAndReplacePrefix verifies the edit form uses the
// we: prefix (replace semantics) and prefills name/description/metadata.
func TestWorldEditModalPrefillsAndReplacePrefix(t *testing.T) {
	existing := &db.WorldEntity{
		Kind: db.KindNPC, Name: "Captain Varek",
		Description: "Commander of the Eastwatch guard.",
		Metadata:    map[string]any{"role": "commander", "status": "ally"},
	}
	m := worldEntityModal(db.KindNPC, existing, true)
	if m.CustomID != worldEditModalPrefix+":"+string(db.KindNPC) {
		t.Errorf("edit modal custom id = %q, want we: prefix", m.CustomID)
	}
	vals := modalFieldValues(m)
	if vals["name"] != "Captain Varek" {
		t.Errorf("name not prefilled: %q", vals["name"])
	}
	if vals["description"] != "Commander of the Eastwatch guard." {
		t.Errorf("description not prefilled: %q", vals["description"])
	}
	if vals["role"] != "commander" || vals["status"] != "ally" {
		t.Errorf("metadata not prefilled: %+v", vals)
	}
}

// TestWorldAddModalBlankNoPrefill verifies the add form is blank (append flow).
func TestWorldAddModalBlankNoPrefill(t *testing.T) {
	m := worldAddModal(db.KindNPC)
	for _, v := range modalFieldValues(m) {
		if v != "" {
			t.Errorf("add modal should have no prefilled values, got %q", v)
		}
	}
}

// TestWorldCommandSubcommands verifies /world exposes add, edit, list, and
// delete — so every world kind (incl. story hooks) can be created, edited,
// listed, and deleted.
func TestWorldCommandSubcommands(t *testing.T) {
	subs := subcommandNames(t, "world")
	for _, want := range []string{"add", "edit", "list", "delete"} {
		if !contains(subs, want) {
			t.Errorf("/world missing %q subcommand (have %v)", want, subs)
		}
	}
}

// TestCharacterCommandSubcommands verifies /character exposes add, edit, list,
// and delete.
func TestCharacterCommandSubcommands(t *testing.T) {
	subs := subcommandNames(t, "character")
	for _, want := range []string{"add", "edit", "list", "delete"} {
		if !contains(subs, want) {
			t.Errorf("/character missing %q subcommand (have %v)", want, subs)
		}
	}
}

// subcommandNames returns the subcommand names of a registered top-level command.
func subcommandNames(t *testing.T, command string) []string {
	t.Helper()
	for _, sp := range allCommandSpecs() {
		if sp.def.Name != command {
			continue
		}
		var out []string
		for _, o := range sp.def.Options {
			if sub, ok := o.(discord.ApplicationCommandOptionSubCommand); ok {
				out = append(out, sub.Name)
			}
		}
		return out
	}
	t.Fatalf("command %q not registered", command)
	return nil
}

// TestWorldAddModalUnknownKind falls back to a plain description-only form.
func TestWorldAddModalUnknownKind(t *testing.T) {
	m := worldAddModal(db.WorldEntityKind("dragon"))
	got := modalFieldIDs(m)
	if !contains(got, "name") || !contains(got, "description") {
		t.Errorf("fallback modal should still have name+description, got %v", got)
	}
}

// TestCharacterAddModalFields checks the character form fields and prefill.
func TestCharacterAddModalFields(t *testing.T) {
	m := characterAddModal(nil)
	if m.CustomID != characterAddModalID {
		t.Errorf("character modal custom id = %q", m.CustomID)
	}
	got := modalFieldIDs(m)
	sort.Strings(got)
	for _, f := range []string{"class", "level", "name", "notes", "race"} {
		if !contains(got, f) {
			t.Errorf("character modal missing field %q (have %v)", f, got)
		}
	}

	// Prefill: an existing character's values seed the inputs (so add == edit).
	existing := &db.PlayerCharacter{Name: "Ludo", Class: "Wizard", Race: "Half-elf", Level: 3, Notes: "Curious"}
	pm := characterAddModal(existing)
	var nameVal string
	for _, c := range pm.Components {
		if label, ok := c.(discord.LabelComponent); ok {
			if ti, ok := label.Component.(discord.TextInputComponent); ok && ti.GetCustomID() == "name" {
				nameVal = ti.Value
			}
		}
	}
	if nameVal != "Ludo" {
		t.Errorf("prefilled name = %q, want Ludo", nameVal)
	}
}

// TestWorldMetadataFieldsExcludeNameDescription guards the invariant that the
// structured-metadata field list never captures name/description (those are
// dedicated columns, not metadata).
func TestWorldMetadataFieldsExcludeNameDescription(t *testing.T) {
	for _, kind := range []db.WorldEntityKind{db.KindNPC, db.KindLocation, db.KindFaction, db.KindQuest} {
		for _, f := range worldMetadataFields(kind) {
			if f == "name" || f == "description" {
				t.Errorf("%s metadata field list must not contain %q", kind, f)
			}
		}
	}
}
