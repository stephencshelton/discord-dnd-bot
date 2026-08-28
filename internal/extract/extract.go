// Package extract turns a model's raw state-extraction JSON into validated,
// deduplicated campaign-state proposals.
//
// It is deliberately dependency-light (only the db types) and pure so the
// parsing/validation logic — the part most likely to meet hostile or malformed
// model output — is unit-testable without a database, Discord, or LiteLLM. The
// worker calls the model, hands the raw text here, and persists whatever survives
// validation. Nothing here ever touches canon: the output is always proposals.
package extract

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
)

// ErrNoJSON is returned when no JSON object can be found in the model output.
var ErrNoJSON = errors.New("no JSON object found in model output")

// maxProposals caps how many proposals a single extraction can yield, bounding
// abuse/runaway output. Extra proposals are dropped.
const maxProposals = 40

// rawResult mirrors the JSON contract in prompts.StateExtractionSchema.
type rawResult struct {
	Proposals []rawProposal `json:"proposals"`
}

type rawProposal struct {
	Action           string         `json:"action"`
	EntityKind       string         `json:"entity_kind"`
	ExistingEntityID string         `json:"existing_entity_id"`
	EntityName       string         `json:"entity_name"`
	Patch            map[string]any `json:"patch"`
	Explanation      string         `json:"explanation"`
	Evidence         string         `json:"evidence"`
	Confidence       float64        `json:"confidence"`
}

// ExistingEntity is the minimal existing-entity context the extractor needs to
// resolve update targets and avoid duplicate creates.
type ExistingEntity struct {
	ID   uuid.UUID
	Kind db.WorldEntityKind
	Name string
}

// ExistingCharacter is the minimal player-character context the extractor needs
// to attach recorded deeds/facts to a known PC (extraction can only UPDATE
// characters, never create them — a PC needs a Discord owner).
type ExistingCharacter struct {
	ID   uuid.UUID
	Name string
}

// validKinds is the set of world-entity kinds a proposal may target. Player
// characters (db.KindCharacter) are handled separately in normalize since they
// resolve against existing PCs and are update-only.
var validKinds = map[db.WorldEntityKind]bool{
	db.KindNPC:      true,
	db.KindLocation: true,
	db.KindFaction:  true,
	db.KindQuest:    true,
	db.KindHook:     true,
}

// entityKey identifies an entity for dedup/resolution: kind + case-folded name.
type entityKey struct {
	kind db.WorldEntityKind
	name string
}

// Parse extracts the JSON object from raw model output and unmarshals it into
// validated, deduplicated proposals for the given campaign/session.
//
// Behavior:
//   - Tolerates surrounding prose / code fences by slicing to the outer-most
//     JSON object (some models wrap JSON despite instructions).
//   - Rejects malformed JSON with an error (the caller treats extraction as
//     best-effort and logs/metrics the failure without failing the session).
//   - Drops individual proposals that are invalid (unknown action/kind, no
//     entity name, no evidence) rather than failing the whole batch — a single
//     bad item shouldn't discard good ones.
//   - Normalizes create-vs-update: matches entity_name case-insensitively
//     against existing entities of the same kind. A "create" for a name that
//     already exists becomes an "update" (dedup); an "update" with no resolvable
//     target becomes a "create". Duplicate proposals within one batch (same
//     kind+name) are merged, keeping the highest confidence.
//
// sessionID may be uuid.Nil for manually-authored proposals; callers pass a
// pointer through to the DB. `characters` lets the extractor attach recorded
// deeds/facts to an existing player character (update-only; a character
// proposal that names no known PC is dropped).
func Parse(raw string, campaignID uuid.UUID, sessionID *uuid.UUID, existing []ExistingEntity, characters []ExistingCharacter) ([]db.StateProposal, error) {
	body, err := extractJSONObject(raw)
	if err != nil {
		return nil, err
	}
	var res rawResult
	dec := json.NewDecoder(strings.NewReader(body))
	if err := dec.Decode(&res); err != nil {
		return nil, fmt.Errorf("parse extraction JSON: %w", err)
	}

	// Index existing entities by (kind, lower(name)) for O(1) dedup/resolution.
	byName := make(map[entityKey]ExistingEntity, len(existing))
	for _, e := range existing {
		byName[entityKey{e.Kind, strings.ToLower(strings.TrimSpace(e.Name))}] = e
	}
	// Index existing player characters by case-folded name.
	byCharName := make(map[string]ExistingCharacter, len(characters))
	for _, c := range characters {
		byCharName[strings.ToLower(strings.TrimSpace(c.Name))] = c
	}

	// Merge duplicate proposals within the batch by (kind, lower(name)).
	merged := map[entityKey]db.StateProposal{}
	var order []entityKey

	for _, rp := range res.Proposals {
		if len(merged) >= maxProposals {
			break
		}
		p, ok := normalize(rp, campaignID, sessionID, byName, byCharName)
		if !ok {
			continue
		}
		k := entityKey{p.EntityKind, strings.ToLower(p.EntityName)}
		if prev, exists := merged[k]; exists {
			// Merge: prefer the higher-confidence text, union the patch/evidence.
			merged[k] = mergeProposals(prev, p)
			continue
		}
		merged[k] = p
		order = append(order, k)
	}
	out := make([]db.StateProposal, 0, len(order))
	for _, k := range order {
		out = append(out, merged[k])
	}
	return out, nil
}

// normalize validates and canonicalizes a single raw proposal. It returns
// (proposal, true) if the proposal is usable, or (_, false) to drop it.
func normalize(rp rawProposal, campaignID uuid.UUID, sessionID *uuid.UUID, byName map[entityKey]ExistingEntity, byCharName map[string]ExistingCharacter) (db.StateProposal, bool) {
	name := strings.TrimSpace(rp.EntityName)
	kind := db.WorldEntityKind(strings.ToLower(strings.TrimSpace(rp.EntityKind)))
	evidence := strings.TrimSpace(rp.Evidence)

	// A name and supporting evidence are always required (the evidence rule
	// enforces the "conservative, justified" contract). The kind must be a valid
	// world kind OR the special character target.
	isCharacter := kind == db.KindCharacter
	if (!validKinds[kind] && !isCharacter) || name == "" || evidence == "" {
		return db.StateProposal{}, false
	}

	patch := map[string]any{}
	for kk, vv := range rp.Patch {
		patch[kk] = vv
	}
	// If the model didn't put a description in the patch but did explain the
	// change, seed the description from the explanation so the entity isn't blank.
	if _, ok := patch["description"].(string); !ok {
		if expl := strings.TrimSpace(rp.Explanation); expl != "" {
			patch["description"] = expl
		}
	}

	conf := rp.Confidence
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}

	p := db.StateProposal{
		CampaignID:  campaignID,
		SessionID:   sessionID,
		EntityKind:  kind,
		EntityName:  name,
		Patch:       patch,
		Explanation: strings.TrimSpace(rp.Explanation),
		Evidence:    evidence,
		Confidence:  conf,
		Status:      db.ProposalPending,
	}

	// Player-character target: attach recorded deeds/facts to an EXISTING PC.
	// Extraction never creates a character (needs a Discord owner), so drop the
	// proposal if it names no known PC. Always an update action.
	if isCharacter {
		pc, ok := byCharName[strings.ToLower(name)]
		if !ok {
			return db.StateProposal{}, false
		}
		p.Action = db.ActionUpdateEntity
		idCopy := pc.ID
		p.EntityID = &idCopy
		p.EntityName = pc.Name // canonical casing
		return p, true
	}

	// Resolve create-vs-update against existing entities (case-insensitive).
	lookup := entityKey{kind, strings.ToLower(name)}
	existing, exists := byName[lookup]

	// Also honor an explicit existing_entity_id the model provided.
	if id, err := uuid.Parse(strings.TrimSpace(rp.ExistingEntityID)); err == nil && id != uuid.Nil {
		p.Action = db.ActionUpdateEntity
		idCopy := id
		p.EntityID = &idCopy
		// Keep the canonical name from the known entity if we can match the id.
		return p, true
	}

	if exists {
		// Name already exists -> this is an update, regardless of what the model
		// labeled it (dedup: never create a duplicate).
		p.Action = db.ActionUpdateEntity
		idCopy := existing.ID
		p.EntityID = &idCopy
		p.EntityName = existing.Name // canonical casing
		return p, true
	}

	// No existing match -> create. (If the model said "update" but we can't find
	// a target, treating it as create ensures the DM's later approval isn't lost.)
	p.Action = db.ActionCreateEntity
	p.EntityID = nil
	return p, true
}

// mergeProposals combines two proposals for the same entity within one batch.
// It keeps the higher-confidence one as the base and unions evidence/patch so
// no supporting detail is lost.
func mergeProposals(a, b db.StateProposal) db.StateProposal {
	base, other := a, b
	if b.Confidence > a.Confidence {
		base, other = b, a
	}
	// Union patch fields (base wins on conflict).
	if base.Patch == nil {
		base.Patch = map[string]any{}
	}
	for k, v := range other.Patch {
		if _, ok := base.Patch[k]; !ok {
			base.Patch[k] = v
		}
	}
	// Concatenate distinct evidence.
	if other.Evidence != "" && !strings.Contains(base.Evidence, other.Evidence) {
		base.Evidence = strings.TrimSpace(base.Evidence + " " + other.Evidence)
	}
	// An update action is stronger than create (we know the target exists).
	if other.Action == db.ActionUpdateEntity && base.Action == db.ActionCreateEntity {
		base.Action = db.ActionUpdateEntity
		base.EntityID = other.EntityID
		base.EntityName = other.EntityName
	}
	return base
}

// extractJSONObject returns the substring from the first '{' to its matching
// closing '}', so a model that wraps its JSON in prose or ```json fences still
// parses. It respects strings and escapes so a brace inside a string value
// doesn't fool the matcher. Returns ErrNoJSON when there's no object.
func extractJSONObject(s string) (string, error) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", ErrNoJSON
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", ErrNoJSON
}
