package db

import "testing"

func TestAppendDetail(t *testing.T) {
	cases := []struct {
		name             string
		existing, adding string
		want             string
	}{
		{"empty existing", "", "new fact", "new fact"},
		{"empty addition", "old fact", "", "old fact"},
		{"both empty", "", "", ""},
		{"append on new line", "old fact", "new fact", "old fact\nnew fact"},
		{"dedup exact", "the dragon is dead", "the dragon is dead", "the dragon is dead"},
		{"dedup case-insensitive", "The Dragon Is Dead", "the dragon is dead", "The Dragon Is Dead"},
		{"dedup substring", "The dragon is dead and gone", "dragon is dead", "The dragon is dead and gone"},
		{"trims whitespace", "  old  ", "  new  ", "old\nnew"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AppendDetail(c.existing, c.adding); got != c.want {
				t.Errorf("AppendDetail(%q,%q) = %q, want %q", c.existing, c.adding, got, c.want)
			}
		})
	}
}

func TestValidWorldKind(t *testing.T) {
	for _, k := range []WorldEntityKind{KindNPC, KindLocation, KindFaction, KindQuest, KindHook} {
		if !ValidWorldKind(k) {
			t.Errorf("%q should be a valid world kind", k)
		}
	}
	// KindCharacter is a proposal-target discriminator, NOT a world kind.
	if ValidWorldKind(KindCharacter) {
		t.Error("KindCharacter must not be a valid world kind")
	}
	if ValidWorldKind("item") {
		t.Error("dropped 'item' kind must not be valid")
	}
	if ValidWorldKind("dragon") {
		t.Error("unknown kind must not be valid")
	}
}
