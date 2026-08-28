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
	worldAddModalPrefix = "wa"
	characterAddModalID = "ca"
)

// --- /world add ---

// worldAddModal builds the kind-specific structured form for a world entity.
// Every kind collects a Name and a free Description/Notes; the middle fields are
// the structured, kind-appropriate metadata that make the entry useful later.
func worldAddModal(kind db.WorldEntityKind) discord.ModalCreate {
	name := discord.NewShortTextInput("name").WithRequired(true).WithPlaceholder("Name")

	m := discord.NewModalCreate(worldAddModalPrefix+":"+string(kind), "Add "+entityKindLabel(kind)).
		AddLabel("Name", name)

	// Kind-specific structured fields (all optional so a quick stub still works).
	switch kind {
	case db.KindNPC:
		m = m.
			AddLabel("Role / Title", discord.NewShortTextInput("role").WithRequired(false).WithPlaceholder("e.g. Commander of the Eastwatch guard")).
			AddLabel("Location", discord.NewShortTextInput("location").WithRequired(false).WithPlaceholder("Where are they usually found?")).
			AddLabel("Attitude / Status", discord.NewShortTextInput("status").WithRequired(false).WithPlaceholder("e.g. ally, hostile, missing")).
			AddLabel("Description", discord.NewParagraphTextInput("description").WithRequired(false).WithPlaceholder("Who are they? What should we remember?"))
	case db.KindLocation:
		m = m.
			AddLabel("Region", discord.NewShortTextInput("region").WithRequired(false).WithPlaceholder("What larger area is this in?")).
			AddLabel("Notable features", discord.NewShortTextInput("features").WithRequired(false).WithPlaceholder("e.g. fortified border town, gateway to the pass")).
			AddLabel("Description", discord.NewParagraphTextInput("description").WithRequired(false).WithPlaceholder("What is this place? What happened here?"))
	case db.KindFaction:
		m = m.
			AddLabel("Goals", discord.NewShortTextInput("goals").WithRequired(false).WithPlaceholder("What do they want?")).
			AddLabel("Notable members", discord.NewShortTextInput("members").WithRequired(false).WithPlaceholder("Leaders / key figures")).
			AddLabel("Attitude / Status", discord.NewShortTextInput("status").WithRequired(false).WithPlaceholder("e.g. allied, hostile, unknown")).
			AddLabel("Description", discord.NewParagraphTextInput("description").WithRequired(false).WithPlaceholder("Who are they? What should we remember?"))
	case db.KindQuest:
		m = m.
			AddLabel("Status", discord.NewShortTextInput("status").WithRequired(false).WithPlaceholder("e.g. active, completed, failed")).
			AddLabel("Objective", discord.NewShortTextInput("objective").WithRequired(false).WithPlaceholder("What must the party do?")).
			AddLabel("Quest giver", discord.NewShortTextInput("giver").WithRequired(false).WithPlaceholder("Who assigned it?")).
			AddLabel("Description", discord.NewParagraphTextInput("description").WithRequired(false).WithPlaceholder("Details, stakes, complications"))
	default:
		m = m.AddLabel("Description", discord.NewParagraphTextInput("description").WithRequired(false))
	}
	return m
}

// worldMetadataFields lists, per kind, the modal text-input custom IDs that map
// to structured metadata (everything except name/description). Keeping this in
// one place keeps the modal builder and submit handler in sync.
func worldMetadataFields(kind db.WorldEntityKind) []string {
	switch kind {
	case db.KindNPC:
		return []string{"role", "location", "status"}
	case db.KindLocation:
		return []string{"region", "features"}
	case db.KindFaction:
		return []string{"goals", "members", "status"}
	case db.KindQuest:
		return []string{"status", "objective", "giver"}
	default:
		return nil
	}
}

// handleWorldAddModalSubmit persists the world entity from a submitted modal. It
// upserts by (kind, case-insensitive name): a matching existing entity is
// updated (merging the structured metadata and refreshing the description),
// otherwise a new one is created. Structured fields go into metadata JSONB; a
// display description is stored in the description column.
func (g *Gateway) handleWorldAddModalSubmit(ctx context.Context, e *events.ModalSubmitInteractionCreate) {
	kind := db.WorldEntityKind(strings.TrimPrefix(e.Data.CustomID, worldAddModalPrefix+":"))
	switch kind {
	case db.KindNPC, db.KindLocation, db.KindFaction, db.KindQuest:
	default:
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

	// Upsert by case-insensitive name within kind (consistent with proposal apply
	// and the unique index), so re-adding an entity edits it instead of failing.
	existing, gerr := g.store.GetWorldEntityByName(ctx, camp.ID, kind, name)
	verb := "Added"
	var entityID uuid.UUID
	switch {
	case gerr == nil:
		// Merge metadata onto the existing entity; keep the old description when
		// the form left it blank so an edit doesn't wipe prior text.
		merged := map[string]any{}
		for k, v := range existing.Metadata {
			merged[k] = v
		}
		for k, v := range meta {
			merged[k] = v
		}
		desc := existing.Description
		if description != "" {
			desc = description
		}
		if err := g.store.UpdateWorldEntityFull(ctx, existing.ID, existing.Name, desc, merged); err != nil {
			g.log.Error("world add modal: update", "err", err)
			_ = e.CreateMessage(discord.MessageCreate{Content: "Couldn't save that. Please try again.", Flags: discord.MessageFlagEphemeral})
			return
		}
		entityID = existing.ID
		verb = "Updated"
	case errors.Is(gerr, db.ErrNotFound):
		created, err := g.store.CreateWorldEntity(ctx, db.WorldEntity{
			CampaignID: camp.ID, Kind: kind, Name: name, Description: description, Metadata: meta,
		})
		if err != nil {
			g.log.Error("world add modal: create", "err", err)
			_ = e.CreateMessage(discord.MessageCreate{Content: "Couldn't save that. Please try again.", Flags: discord.MessageFlagEphemeral})
			return
		}
		entityID = created.ID
	default:
		g.log.Error("world add modal: lookup", "err", gerr)
		_ = e.CreateMessage(discord.MessageCreate{Content: "Couldn't save that. Please try again.", Flags: discord.MessageFlagEphemeral})
		return
	}

	// Make it retrievable by /ask (async; best-effort).
	g.enqueueCanonEmbed(ctx, camp.ID, db.CanonSourceEntity, entityID)

	logging.FromContext(ctx, g.log).Info("world entity saved via modal",
		"campaign", camp.ID, "kind", kind, "name", name, "verb", verb, "user", e.User().ID.String())
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
