package extract

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
)

// FilterByConfidence drops proposals whose confidence is below min. A min <= 0
// disables the floor (returns the input unchanged). This is the cheap,
// deterministic first line of noteworthiness gating — no AI call.
func FilterByConfidence(proposals []db.StateProposal, min float64) []db.StateProposal {
	if min <= 0 {
		return proposals
	}
	out := proposals[:0:0] // new backing array; don't mutate caller's slice
	for _, p := range proposals {
		if p.Confidence >= min {
			out = append(out, p)
		}
	}
	return out
}

// CriticCandidate renders a proposal into the compact one-block summary the
// critic pass scores. It's kept here so the summary the editor sees stays in
// sync with what's actually being proposed.
func CriticCandidate(p db.StateProposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", p.EntityKind, p.EntityName)
	if d := strings.TrimSpace(p.Description()); d != "" {
		b.WriteString("\n")
		b.WriteString(d)
	}
	if e := strings.TrimSpace(p.Explanation); e != "" && e != strings.TrimSpace(p.Description()) {
		b.WriteString("\nChange: ")
		b.WriteString(e)
	}
	if ev := strings.TrimSpace(p.Evidence); ev != "" {
		b.WriteString("\nEvidence: ")
		b.WriteString(ev)
	}
	fmt.Fprintf(&b, "\nConfidence: %.2f", p.Confidence)
	return b.String()
}

// criticResult mirrors the critic pass's strict-JSON contract: the indices
// (into the candidate list) worth keeping.
type criticResult struct {
	Keep []int `json:"keep"`
}

// ApplyCriticKeep filters proposals to those the critic chose to keep, given the
// critic's raw JSON response. Behavior is intentionally FAIL-OPEN on malformed
// output: if the response can't be parsed, the original proposals are returned
// unchanged (the confidence floor already applied, and it's better to over-
// surface for DM review than to silently drop everything on a critic hiccup).
// Out-of-range indices are ignored. Order follows the original slice.
func ApplyCriticKeep(proposals []db.StateProposal, rawCriticJSON string) []db.StateProposal {
	body, err := extractJSONObject(rawCriticJSON)
	if err != nil {
		return proposals // fail-open
	}
	var res criticResult
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return proposals // fail-open
	}
	keep := make(map[int]bool, len(res.Keep))
	for _, i := range res.Keep {
		if i >= 0 && i < len(proposals) {
			keep[i] = true
		}
	}
	out := proposals[:0:0]
	for i, p := range proposals {
		if keep[i] {
			out = append(out, p)
		}
	}
	return out
}
