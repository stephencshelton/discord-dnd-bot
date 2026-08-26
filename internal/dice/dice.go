// Package dice parses and evaluates standard tabletop dice notation such as
// "2d6+3", "d20", "4d6kh3" (keep highest 3), or "1d20+1d4+2". No I/O or AI
// calls, so /roll is instant and free. Rolls use crypto/rand for fair,
// unpredictable results.
package dice

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Max limits to keep a single roll bounded (anti-abuse / anti-spam): no more
// than this many dice per term and this many sides per die.
const (
	MaxDice  = 100
	MaxSides = 1000
	MaxTerms = 20
)

// Result is the outcome of evaluating a dice expression.
type Result struct {
	Total  int    // final total including modifiers
	Detail string // human-readable breakdown, e.g. "2d6 (4, 5) + 3 = 12"
	Terms  []Term // per-term breakdown
}

// Term is one component of an expression: a dice group or a flat modifier.
type Term struct {
	Text  string // the term as written, e.g. "2d6kh1"
	Rolls []int  // individual die results (nil for a flat modifier)
	Kept  []int  // the rolls that counted (after keep/drop)
	Value int    // signed contribution to the total
}

// term matches an optional sign, then either NdX(kh|kl|dh|dl)N or a flat integer.
var termRE = regexp.MustCompile(`^([+-]?)\s*(?:(\d*)d(\d+)((?:kh|kl|dh|dl)\d+)?|(\d+))$`)

// Roll parses and evaluates a dice expression like "2d6+3" or "d20-1". It
// returns an error for malformed or out-of-bounds input.
func Roll(expr string) (*Result, error) {
	expr = strings.TrimSpace(strings.ToLower(expr))
	if expr == "" {
		return nil, fmt.Errorf("empty expression")
	}
	// Insert spaces around +/- so Fields splits into "[+-]?term" tokens.
	spaced := strings.NewReplacer("+", " +", "-", " -").Replace(expr)
	tokens := strings.Fields(spaced)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("no dice terms in %q", expr)
	}
	if len(tokens) > MaxTerms {
		return nil, fmt.Errorf("too many terms (max %d)", MaxTerms)
	}

	res := &Result{}
	var parts []string
	for _, tok := range tokens {
		t, err := evalTerm(tok)
		if err != nil {
			return nil, err
		}
		res.Total += t.Value
		res.Terms = append(res.Terms, t)
		parts = append(parts, formatTerm(t))
	}
	res.Detail = strings.Join(parts, " ")
	// Collapse a leading "+ " for readability.
	res.Detail = strings.TrimPrefix(res.Detail, "+ ")
	res.Detail = fmt.Sprintf("%s = %d", res.Detail, res.Total)
	return res, nil
}

func evalTerm(tok string) (Term, error) {
	m := termRE.FindStringSubmatch(tok)
	if m == nil {
		return Term{}, fmt.Errorf("invalid term %q (try e.g. 2d6+3)", tok)
	}
	sign := 1
	if m[1] == "-" {
		sign = -1
	}

	// Flat modifier.
	if m[5] != "" {
		v, err := strconv.Atoi(m[5])
		if err != nil {
			return Term{}, fmt.Errorf("invalid modifier %q", tok)
		}
		return Term{Text: tok, Value: sign * v}, nil
	}

	// Dice group.
	count := 1
	if m[2] != "" {
		c, err := strconv.Atoi(m[2])
		if err != nil {
			return Term{}, err
		}
		count = c
	}
	sides, err := strconv.Atoi(m[3])
	if err != nil {
		return Term{}, err
	}
	if count < 1 || count > MaxDice {
		return Term{}, fmt.Errorf("dice count must be 1..%d", MaxDice)
	}
	if sides < 2 || sides > MaxSides {
		return Term{}, fmt.Errorf("die sides must be 2..%d", MaxSides)
	}

	rolls := make([]int, count)
	for i := range rolls {
		rolls[i] = rollDie(sides)
	}

	kept := rolls
	if kh := m[4]; kh != "" {
		kept, err = applyKeepDrop(rolls, kh)
		if err != nil {
			return Term{}, err
		}
	}
	sum := 0
	for _, v := range kept {
		sum += v
	}
	return Term{Text: tok, Rolls: rolls, Kept: kept, Value: sign * sum}, nil
}

// applyKeepDrop applies a keep/drop directive (kh/kl/dh/dl + N) to rolls and
// returns the surviving dice. The input slice is not mutated.
func applyKeepDrop(rolls []int, directive string) ([]int, error) {
	mode := directive[:2]
	n, err := strconv.Atoi(directive[2:])
	if err != nil || n < 0 {
		return nil, fmt.Errorf("invalid keep/drop %q", directive)
	}
	if n > len(rolls) {
		n = len(rolls)
	}
	sorted := append([]int(nil), rolls...)
	// ascending sort (small n; insertion sort avoids importing sort for clarity)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	switch mode {
	case "kh": // keep highest n
		return sorted[len(sorted)-n:], nil
	case "kl": // keep lowest n
		return sorted[:n], nil
	case "dh": // drop highest n
		return sorted[:len(sorted)-n], nil
	case "dl": // drop lowest n
		return sorted[n:], nil
	default:
		return nil, fmt.Errorf("unknown keep/drop mode %q", mode)
	}
}

func formatTerm(t Term) string {
	sign := "+"
	if t.Value < 0 {
		sign = "-"
	}
	if t.Rolls == nil {
		return fmt.Sprintf("%s %d", sign, abs(t.Value))
	}
	nums := make([]string, len(t.Rolls))
	for i, v := range t.Rolls {
		nums[i] = strconv.Itoa(v)
	}
	return fmt.Sprintf("%s %s (%s)", sign, t.Text, strings.Join(nums, ", "))
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// rollDie returns a uniformly-random result in [1, sides] using crypto/rand,
// falling back to 1 on the (practically impossible) rand error.
func rollDie(sides int) int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(sides)))
	if err != nil {
		return 1
	}
	return int(n.Int64()) + 1
}
