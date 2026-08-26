package dice

import (
	"strconv"
	"strings"
	"testing"
)

func TestRollBounds(t *testing.T) {
	for _, tc := range []struct {
		expr string
		min  int
		max  int
	}{
		{"d20", 1, 20},
		{"1d20", 1, 20},
		{"2d6", 2, 12},
		{"2d6+3", 5, 15},
		{"1d20-1", 0, 19},
		{"3d8+2d4+1", 6, 33},
		{"4d6kh3", 3, 18}, // keep highest 3 of 4d6
		{"4d6dl1", 3, 18}, // drop lowest 1 of 4d6 (same range)
		{"2d20kl1", 1, 20},
	} {
		for i := 0; i < 200; i++ {
			r, err := Roll(tc.expr)
			if err != nil {
				t.Fatalf("Roll(%q) error: %v", tc.expr, err)
			}
			if r.Total < tc.min || r.Total > tc.max {
				t.Fatalf("Roll(%q) = %d, want in [%d,%d] (detail: %s)", tc.expr, r.Total, tc.min, tc.max, r.Detail)
			}
		}
	}
}

func TestRollDetailFormat(t *testing.T) {
	r, err := Roll("2d6+3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Detail, "2d6") || !strings.Contains(r.Detail, "+ 3") {
		t.Errorf("unexpected detail: %s", r.Detail)
	}
	if !strings.HasSuffix(r.Detail, "= "+strconv.Itoa(r.Total)) {
		t.Errorf("detail should end with total: %s", r.Detail)
	}
}

func TestKeepHighestActuallyKeepsHighest(t *testing.T) {
	// 4d6kh3 must never be less than 3d... minimum and the kept set is 3 dice.
	r, err := Roll("4d6kh3")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Terms) != 1 || len(r.Terms[0].Rolls) != 4 || len(r.Terms[0].Kept) != 3 {
		t.Fatalf("expected 4 rolls, 3 kept; got rolls=%v kept=%v", r.Terms[0].Rolls, r.Terms[0].Kept)
	}
}

func TestInvalidExpressions(t *testing.T) {
	for _, expr := range []string{"", "abc", "d1", "0d6", "1d1", "101d6", "1d1001", "2d6++3", "d"} {
		if _, err := Roll(expr); err == nil {
			t.Errorf("Roll(%q) expected error, got nil", expr)
		}
	}
}
