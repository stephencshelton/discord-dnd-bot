package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
)

// Structured input modals for /world add and /character add.
//
// Rather than a single freeform "description" option, these commands pop a
// fill-in form (a Discord modal) whose fields are tailored to what's being
// recorded. The structured answers land in the entity's metadata JSONB (with a
// human-readable "description" assembled for display and, later, retrieval), so
// data enters the database consistently regardless of who's adding it.
//
// Custom-ID scheme:
//   - world:    "wa:<kind>"  (kind = npc|location|faction|quest)
//   - character: "ca"        (single form)

const (
	worldAddModalPrefix  = "wa"
	worldEditModalPrefix = "we"
	characterAddModalID  = "ca"
)

// appendDetail returns existing with addition appended on its own line, so an
// entity that "comes back up" accumulates detail rather than overwriting it.
// Delegates to db.AppendDetail so the ADD/accrue semantics match the proposal
// apply path exactly (dedup-aware; deliberate replacement is done by /world edit).
func appendDetail(existing, addition string) string {
	return db.AppendDetail(existing, addition)
}

// worldFieldLabels maps a metadata field custom-ID to its human label +
// placeholder, per kind. Order matters (it's the modal layout). The special
// "description" field is always appended last by the builder.
type worldField struct{ id, label, placeholder string }

func worldFields(kind db.WorldEntityKind) []worldField {
	switch kind {
	case db.KindNPC:
		return []worldField{
			{"role", "Role / Title", "e.g. Commander of the Eastwatch guard"},
			{"location", "Location", "Where are they usually found?"},
			{"status", "Attitude / Status", "e.g. ally, hostile, missing"},
		}
	case db.KindLocation:
		return []worldField{
			{"region", "Region", "What larger area is this in?"},
			{"features", "Notable features", "e.g. fortified border town, gateway to the pass"},
		}
	case db.KindFaction:
		return []worldField{
			{"goals", "Goals", "What do they want?"},
			{"members", "Notable members", "Leaders / key figures"},
			{"status", "Attitude / Status", "e.g. allied, hostile, unknown"},
		}
	case db.KindQuest:
		return []worldField{
			{"status", "Status", "e.g. active, completed, failed"},
			{"objective", "Objective", "What must the party do?"},
			{"giver", "Quest giver", "Who assigned it?"},
		}
	case db.KindHook:
		return []worldField{
			{"status", "Status", "e.g. open, being pursued, dropped"},
			{"related", "Tied to", "NPC/location/faction it connects to"},
		}
	default:
		return nil
	}
}

// --- /world add & /world edit ---

// worldEntityModal builds the kind-specific structured form. When edit is false
// (the /world add flow) the form is blank and the submit APPENDS to any existing
// entity of the same name. When edit is true (/world edit) it is prefilled from
// `existing` and the submit REPLACES that entity's fields. The custom ID encodes
// which flow it is (wa:<kind> vs we:<kind>) and, for edit, the entity name.
func worldEntityModal(kind db.WorldEntityKind, existing *db.WorldEntity, edit bool) discord.ModalCreate {
	name := discord.NewShortTextInput("name").WithRequired(true).WithPlaceholder("Name")
	desc := discord.NewParagraphTextInput("description").WithRequired(false)

	customID := worldAddModalPrefix + ":" + string(kind)
	title := "Add " + entityKindLabel(kind)
	if edit {
		customID = worldEditModalPrefix + ":" + string(kind)
		title = "Edit " + entityKindLabel(kind)
		desc = desc.WithPlaceholder("Replaces the current description")
	} else {
		desc = desc.WithPlaceholder("What should we remember? (added to any existing notes)")
	}

	fields := worldFields(kind)
	// Prefill from the existing entity on edit.
	if edit && existing != nil {
		name = name.WithValue(existing.Name)
		desc = desc.WithValue(existing.Description)
	}

	m := discord.NewModalCreate(customID, title).AddLabel("Name", name)
	for _, f := range fields {
		in := discord.NewShortTextInput(f.id).WithRequired(false).WithPlaceholder(f.placeholder)
		if edit && existing != nil {
			if v, ok := existing.Metadata[f.id].(string); ok {
				in = in.WithValue(v)
			}
		}
		m = m.AddLabel(f.label, in)
	}
	m = m.AddLabel("Description", desc)
	return m
}

// worldAddModal is the blank add form (append semantics).
func worldAddModal(kind db.WorldEntityKind) discord.ModalCreate {
	return worldEntityModal(kind, nil, false)
}

// worldMetadataFields lists, per kind, the modal text-input custom IDs that map
// to structured metadata (everything except name/description). Derived from
// worldFields so the builder and submit handler can't drift.
func worldMetadataFields(kind db.WorldEntityKind) []string {
	fs := worldFields(kind)
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.id)
	}
	return out
}

// handleWorldEntityModalSubmit persists a world entity from either the add form
// (append to an existing same-named entity, or create) or the edit form (replace
// the targeted entity's fields). Structured fields go into metadata JSONB; the
// description column holds display/searchable prose.
func (g *Gateway) handleWorldEntityModalSubmit(ctx context.Context, e *events.ModalSubmitInteractionCreate, isEdit bool) {
	prefix := worldAddModalPrefix
	if isEdit {
		prefix = worldEditModalPrefix
	}
	kind := db.WorldEntityKind(strings.TrimPrefix(e.Data.CustomID, prefix+":"))
	if !db.ValidWorldKind(kind) {
		_ = e.CreateMessage(discord.MessageCreate{Content: "Unknown world entry type.", Flags: discord.MessageFlagEphemeral})
		return
	}

	guildID, ok := g.resolveGuild(ctx, guildIDString(e), e.User().ID.String())
	if !ok {
		_ = e.CreateMessage(discord.MessageCreate{Content: dmGuildHelp, Flags: discord.MessageFlagEphemeral})
		return
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		_ = e.CreateMessage(discord.MessageCreate{Content: err.Error(), Flags: discord.MessageFlagEphemeral})
		return
	}

	name := strings.TrimSpace(e.Data.Text("name"))
	if name == "" {
		_ = e.CreateMessage(discord.MessageCreate{Content: "A name is required.", Flags: discord.MessageFlagEphemeral})
		return
	}
	description := strings.TrimSpace(e.Data.Text("description"))

	// Collect structured metadata (skip empty fields).
	meta := map[string]any{}
	for _, field := range worldMetadataFields(kind) {
		if v := strings.TrimSpace(e.Data.Text(field)); v != "" {
			meta[field] = v
		}
	}

	existing, gerr := g.store.GetWorldEntityByName(ctx, camp.ID, kind, name)
	verb := "Added"
	var entityID uuid.UUID
	switch {
	case gerr == nil:
		verb = "Updated"
		var (
			desc   string
			merged = map[string]any{}
		)
		if isEdit {
			// EDIT: replace description and metadata with the submitted values
			// (the form was prefilled, so the user saw and chose the full text).
			desc = description
			merged = meta
		} else {
			// ADD: accumulate — merge metadata and APPEND new description so prior
			// hand-written detail is never lost.
			for k, v := range existing.Metadata {
				merged[k] = v
			}
			for k, v := range meta {
				merged[k] = v
			}
			desc = appendDetail(existing.Description, description)
		}
		if err := g.store.UpdateWorldEntityFull(ctx, existing.ID, name, desc, merged); err != nil {
			g.log.Error("world modal: update", "err", err, "edit", isEdit)
			_ = e.CreateMessage(discord.MessageCreate{Content: "Couldn't save that. Please try again.", Flags: discord.MessageFlagEphemeral})
			return
		}
		entityID = existing.ID
	case errors.Is(gerr, db.ErrNotFound):
		// Edit of a non-existent entity falls through to create (nothing lost).
		created, cerr := g.store.CreateWorldEntity(ctx, db.WorldEntity{
			CampaignID: camp.ID, Kind: kind, Name: name, Description: description, Metadata: meta,
		})
		if cerr != nil {
			g.log.Error("world modal: create", "err", cerr, "edit", isEdit)
			_ = e.CreateMessage(discord.MessageCreate{Content: "Couldn't save that. Please try again.", Flags: discord.MessageFlagEphemeral})
			return
		}
		entityID = created.ID
	default:
		g.log.Error("world modal: lookup", "err", gerr)
		_ = e.CreateMessage(discord.MessageCreate{Content: "Couldn't save that. Please try again.", Flags: discord.MessageFlagEphemeral})
		return
	}

	// Make it retrievable by /ask (async; best-effort).
	g.enqueueCanonEmbed(ctx, camp.ID, db.CanonSourceEntity, entityID)

	logging.FromContext(ctx, g.log).Info("world entity saved via modal",
		"campaign", camp.ID, "kind", kind, "name", name, "verb", verb, "edit", isEdit, "user", e.User().ID.String())
	_ = e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("🌍 %s %s **%s**.", verb, entityKindLabel(kind), name),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// --- /character add ---

// characterAddModal builds the structured player-character form.
func characterAddModal(existing *db.PlayerCharacter) discord.ModalCreate {
	name := discord.NewShortTextInput("name").WithRequired(true).WithPlaceholder("Character name")
	class := discord.NewShortTextInput("class").WithRequired(false).WithPlaceholder("e.g. Wizard")
	race := discord.NewShortTextInput("race").WithRequired(false).WithPlaceholder("e.g. Half-elf")
	level := discord.NewShortTextInput("level").WithRequired(false).WithPlaceholder("e.g. 3")
	notes := discord.NewParagraphTextInput("notes").WithRequired(false).WithPlaceholder("Short bio, goals, quirks")

	if existing != nil {
		name = name.WithValue(existing.Name)
		class = class.WithValue(existing.Class)
		race = race.WithValue(existing.Race)
		if existing.Level > 0 {
			level = level.WithValue(strconv.Itoa(existing.Level))
		}
		notes = notes.WithValue(existing.Notes)
	}

	return discord.NewModalCreate(characterAddModalID, "Your character").
		AddLabel("Name", name).
		AddLabel("Class", class).
		AddLabel("Race / Ancestry", race).
		AddLabel("Level", level).
		AddLabel("Bio / Notes", notes)
}

// handleCharacterAddModalSubmit persists the player's character from the modal.
// A character is keyed to the invoking Discord user within the campaign: if they
// already have one, it's updated; otherwise a new one is created.
func (g *Gateway) handleCharacterAddModalSubmit(ctx context.Context, e *events.ModalSubmitInteractionCreate) {
	userID := e.User().ID.String()
	guildID, ok := g.resolveGuild(ctx, guildIDString(e), userID)
	if !ok {
		_ = e.CreateMessage(discord.MessageCreate{Content: dmGuildHelp, Flags: discord.MessageFlagEphemeral})
		return
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		_ = e.CreateMessage(discord.MessageCreate{Content: err.Error(), Flags: discord.MessageFlagEphemeral})
		return
	}

	name := strings.TrimSpace(e.Data.Text("name"))
	if name == "" {
		_ = e.CreateMessage(discord.MessageCreate{Content: "A name is required.", Flags: discord.MessageFlagEphemeral})
		return
	}
	level := 1
	if lv := strings.TrimSpace(e.Data.Text("level")); lv != "" {
		if n, perr := strconv.Atoi(lv); perr == nil && n > 0 {
			level = n
		}
	}
	pc := db.PlayerCharacter{
		CampaignID:    camp.ID,
		DiscordUserID: userID,
		Name:          name,
		Class:         strings.TrimSpace(e.Data.Text("class")),
		Race:          strings.TrimSpace(e.Data.Text("race")),
		Level:         level,
		Notes:         strings.TrimSpace(e.Data.Text("notes")),
	}

	// Update this user's existing character in the campaign if present.
	verb := "Saved"
	var pcID uuid.UUID
	if existing, gerr := g.userCharacter(ctx, camp.ID, userID); gerr == nil && existing != nil {
		if err := g.store.UpdatePC(ctx, existing.ID, pc.Name, pc.Class, pc.Race, pc.Level, pc.Notes); err != nil {
			g.log.Error("character add modal: update", "err", err)
			_ = e.CreateMessage(discord.MessageCreate{Content: "Couldn't save that. Please try again.", Flags: discord.MessageFlagEphemeral})
			return
		}
		pcID = existing.ID
		verb = "Updated"
	} else {
		created, err := g.store.CreatePC(ctx, pc)
		if err != nil {
			g.log.Error("character add modal: create", "err", err)
			_ = e.CreateMessage(discord.MessageCreate{Content: "Couldn't save that. Please try again.", Flags: discord.MessageFlagEphemeral})
			return
		}
		pcID = created.ID
	}

	// Make it retrievable by /ask (async; best-effort).
	g.enqueueCanonEmbed(ctx, camp.ID, db.CanonSourceCharacter, pcID)

	logging.FromContext(ctx, g.log).Info("player character saved via modal",
		"campaign", camp.ID, "name", pc.Name, "verb", verb, "user", userID)
	_ = e.CreateMessage(discord.MessageCreate{
		Content: fmt.Sprintf("🗡️ %s **%s** (Lv %d %s %s).", verb, pc.Name, pc.Level, pc.Race, pc.Class),
		Flags:   discord.MessageFlagEphemeral,
	})
}

// userCharacter returns the invoking user's character in a campaign, if any.
func (g *Gateway) userCharacter(ctx context.Context, campaignID uuid.UUID, userID string) (*db.PlayerCharacter, error) {
	pcs, err := g.store.ListPCs(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	for i := range pcs {
		if pcs[i].DiscordUserID == userID {
			return &pcs[i], nil
		}
	}
	return nil, db.ErrNotFound
}

// guildIDString extracts the guild ID (or "") from a modal-submit interaction.
func guildIDString(e *events.ModalSubmitInteractionCreate) string {
	if gid := e.GuildID(); gid != nil {
		return gid.String()
	}
	return ""
}

// enqueueCanonEmbed schedules a (re)embedding of a canon record (world entity or
// player character) so grounded /ask retrieval picks it up. Best-effort: a
// failure to enqueue is logged, not surfaced — the record is already saved and a
// later /reindex will catch it up. The embedding itself runs in the worker so it
// never blocks the interaction.
func (g *Gateway) enqueueCanonEmbed(ctx context.Context, campaignID uuid.UUID, sourceKind string, sourceID uuid.UUID) {
	if g.queue == nil {
		return
	}
	if err := g.queue.Enqueue(ctx, queue.JobEmbedCanon, queue.EmbedCanonPayload{
		CampaignID: campaignID.String(),
		SourceKind: sourceKind,
		SourceID:   sourceID.String(),
	}); err != nil {
		logging.FromContext(ctx, g.log).Warn("enqueue canon embed",
			"kind", sourceKind, "source", sourceID, "err", err)
	}
}
