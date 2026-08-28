package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
)

// rememberTemplate is the fill-in template returned by /remember with no
// arguments. It shows a DM exactly how to record something the automatic
// post-session extraction may have missed, and mirrors the proposal fields so
// what they capture flows through the same review pipeline as AI proposals.
const rememberTemplate = "📝 **Add to campaign memory**\n" +
	"Something the AI missed? Capture it and it'll be proposed for your review " +
	"(nothing becomes canon until you approve it in `/review-session`).\n\n" +
	"Run it like this:\n" +
	"```\n" +
	"/remember kind:<npc|location|faction|quest> name:<name> note:<what to remember>\n" +
	"```\n" +
	"**Examples**\n" +
	"```\n" +
	"/remember kind:npc name:Captain Varek note:Commander of the Eastwatch guard; owes the party a favor.\n" +
	"/remember kind:quest name:The Missing Caravan note:Completed — survivors returned to town.\n" +
	"/remember kind:location name:Eastwatch note:Fortified border town; gateway to the northern pass.\n" +
	"/remember kind:faction name:The Ashen Circle note:Secretive cult rumored to be behind the disappearances.\n" +
	"```\n" +
	"Tips:\n" +
	"• Use the same **name** as an existing entry to *update* it instead of creating a duplicate.\n" +
	"• For a quest status change, say so in the note (e.g. \"Active → Completed\").\n" +
	"• Prefer facts the table has actually established over speculation."

// handleRemember either returns the fill-in template (no arguments) or records a
// DM-authored campaign-memory item as a PENDING proposal, so it goes through the
// same /review-session approval path as AI proposals. This keeps the safety
// invariant intact — even a human note is confirmed before it becomes canon —
// while giving DMs a quick way to capture things the extraction missed.
func (g *Gateway) handleRemember(ctx context.Context, ic *ictx) error {
	guildID, ok := g.resolveGuild(ctx, ic.guildID(), ic.userID())
	if !ok {
		return ic.reply(dmGuildHelp, true)
	}

	kindRaw := strings.TrimSpace(strings.ToLower(ic.optString("kind")))
	name := strings.TrimSpace(ic.optString("name"))
	note := strings.TrimSpace(ic.optString("note"))

	// No arguments -> return the template so the DM can see the exact format.
	if kindRaw == "" && name == "" && note == "" {
		return ic.reply(rememberTemplate, true)
	}

	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}

	kind := db.WorldEntityKind(kindRaw)
	switch kind {
	case db.KindNPC, db.KindLocation, db.KindFaction, db.KindQuest:
	default:
		return ic.reply("Pick a `kind` of npc, location, faction, or quest. Run `/remember` with no options to see the template.", true)
	}
	if name == "" {
		return ic.reply("Please include a `name`. Run `/remember` with no options to see the template.", true)
	}
	if note == "" {
		return ic.reply("Please include a `note` describing what to remember.", true)
	}

	// Decide create vs update by case-insensitive name match against existing
	// entities of this kind (same dedup rule the AI extractor uses).
	action := db.ActionCreateEntity
	var entityID *uuid.UUID
	if existing, gerr := g.store.GetWorldEntityByName(ctx, camp.ID, kind, name); gerr == nil {
		action = db.ActionUpdateEntity
		id := existing.ID
		entityID = &id
		name = existing.Name // canonical casing
	}

	prop := db.StateProposal{
		CampaignID:  camp.ID,
		SessionID:   nil, // manually authored, not tied to a recording
		Action:      action,
		EntityKind:  kind,
		EntityID:    entityID,
		EntityName:  name,
		Patch:       map[string]any{"description": note},
		Explanation: "Manually added by a DM via /remember.",
		Evidence:    fmt.Sprintf("Recorded by <@%s>.", ic.userID()),
		Confidence:  1, // human-authored
		Status:      db.ProposalPending,
	}
	if _, err := g.store.CreateStateProposal(ctx, prop); err != nil {
		return err
	}
	logging.FromContext(ctx, g.log).Info("remember proposal created",
		"campaign", camp.ID, "kind", kind, "name", name, "action", action, "user", ic.userID())

	verb := "add"
	if action == db.ActionUpdateEntity {
		verb = "update"
	}
	return ic.reply(fmt.Sprintf(
		"📝 Noted a proposal to %s %s **%s**. It's pending — approve it in `/review-session` to make it canon.",
		verb, entityKindLabel(kind), name), true)
}
