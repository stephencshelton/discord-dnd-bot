package db

import (
	"fmt"
	"sort"
	"strings"
)

// CanonText renders a world entity into a compact, self-describing prose block
// suitable for embedding and for surfacing to /ask as a context excerpt. It
// leads with the kind + name (so retrieval and the answer model have a clear
// subject), then the description, then any structured metadata as "Key: value"
// lines. Metadata keys are emitted in sorted order for stable output.
//
// The result is deterministic and safe to embed repeatedly (re-embedding an
// unchanged entity yields identical text). An entity with only a name still
// produces useful text ("NPC: Captain Varek").
func (e WorldEntity) CanonText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", canonKindLabel(e.Kind), strings.TrimSpace(e.Name))
	if d := strings.TrimSpace(e.Description); d != "" {
		b.WriteString("\n")
		b.WriteString(d)
	}
	for _, line := range metadataLines(e.Metadata) {
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}

// CanonText renders a player character into an embeddable prose block: name,
// the "Level N Race Class" line when those are set, and any freeform notes.
func (p PlayerCharacter) CanonText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Player character: %s", strings.TrimSpace(p.Name))

	var descriptors []string
	if p.Level > 0 {
		descriptors = append(descriptors, fmt.Sprintf("Level %d", p.Level))
	}
	if r := strings.TrimSpace(p.Race); r != "" {
		descriptors = append(descriptors, r)
	}
	if c := strings.TrimSpace(p.Class); c != "" {
		descriptors = append(descriptors, c)
	}
	if len(descriptors) > 0 {
		b.WriteString("\n")
		b.WriteString(strings.Join(descriptors, " "))
	}
	if n := strings.TrimSpace(p.Notes); n != "" {
		b.WriteString("\n")
		b.WriteString(n)
	}
	return b.String()
}

// canonKindLabel is the display label for a world-entity kind used in canon text
// (mirrors the gateway's entityKindLabel but lives here to avoid an import cycle).
func canonKindLabel(k WorldEntityKind) string {
	switch k {
	case KindNPC:
		return "NPC"
	case KindLocation:
		return "Location"
	case KindFaction:
		return "Faction"
	case KindQuest:
		return "Quest"
	default:
		return string(k)
	}
}

// metadataLines renders a metadata map as sorted "Key: value" lines, skipping
// empty values. Non-string values are formatted with %v.
func metadataLines(meta map[string]any) []string {
	if len(meta) == 0 {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		v := meta[k]
		if v == nil {
			continue
		}
		s := strings.TrimSpace(fmt.Sprintf("%v", v))
		if s == "" {
			continue
		}
		// Title-case the key for readability (role -> Role).
		label := k
		if len(label) > 0 {
			label = strings.ToUpper(label[:1]) + label[1:]
		}
		out = append(out, fmt.Sprintf("%s: %s", label, s))
	}
	return out
}
