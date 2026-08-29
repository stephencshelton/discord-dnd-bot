package gateway

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
)

// The review workflow lets a DM step through AI-proposed campaign-state changes
// one at a time using ephemeral message buttons, approving/rejecting/editing
// each without command spam. Approving atomically applies the change to canon
// (world_entities) and marks the proposal approved; rejecting leaves canon
// untouched. Every action is idempotent (see db.ApproveProposal/RejectProposal),
// so a double-click or retried interaction can't create duplicates.

// Custom-ID scheme for the review buttons/modals. Format: "rs:<verb>:<uuid>".
// Keeping the proposal ID in the custom ID makes each button self-describing and
// stateless — the gateway needs no per-message session state (which wouldn't
// survive a pod restart anyway).
const (
	reviewIDPrefix = "rs"
	reviewApprove  = "approve"
	reviewReject   = "reject"
	reviewEdit     = "edit"
	reviewSkip     = "skip"
	reviewModal    = "editmodal"
)

// reviewCustomID builds a button/modal custom ID for a proposal action.
func reviewCustomID(verb string, proposalID uuid.UUID) string {
	return fmt.Sprintf("%s:%s:%s", reviewIDPrefix, verb, proposalID)
}

// parseReviewCustomID splits a review custom ID back into its verb + proposal
// ID. ok is false if it's not one of ours.
func parseReviewCustomID(id string) (verb string, proposalID uuid.UUID, ok bool) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 || parts[0] != reviewIDPrefix {
		return "", uuid.Nil, false
	}
	pid, err := uuid.Parse(parts[2])
	if err != nil {
		return "", uuid.Nil, false
	}
	return parts[1], pid, true
}

// handleReviewSession is the /review-session command: it finds the pending
// proposals for the active campaign (optionally a specific session) and shows
// the first one with Approve/Reject/Edit/Skip buttons. Open to anyone in the
// server — proposals are only suggestions and applying one is reversible via the
// /world commands, so gating isn't warranted.
func (g *Gateway) handleReviewSession(ctx context.Context, ic *ictx) error {
	guildID := ic.guildID()
	if guildID == "" {
		return ic.reply("Use `/review-session` inside a server.", true)
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}

	// Resolve which proposals to review. An optional session_id narrows it to a
	// single session; otherwise all pending proposals for the campaign.
	var proposals []db.StateProposal
	if raw := strings.TrimSpace(ic.optString("session_id")); raw != "" {
		sid, perr := uuid.Parse(raw)
		if perr != nil {
			return ic.reply("That doesn't look like a valid session ID. Pick one from the suggestions.", true)
		}
		sess, serr := g.store.GetSession(ctx, sid)
		if errors.Is(serr, db.ErrNotFound) || (serr == nil && sess.CampaignID != camp.ID) {
			return ic.reply("No session with that ID in this campaign.", true)
		}
		if serr != nil {
			return serr
		}
		proposals, err = g.store.ListPendingProposalsForSession(ctx, sid)
	} else {
		proposals, err = g.store.ListPendingProposalsForCampaign(ctx, camp.ID)
	}
	if err != nil {
		return err
	}
	if len(proposals) == 0 {
		return ic.reply("✅ No pending campaign-state proposals to review. I'll suggest some after your next recorded session.", true)
	}

	// Show the first proposal, ephemerally, with action buttons. Remaining count
	// drives the "N more" hint so the DM knows how far they are.
	embed, components := reviewView(proposals[0], len(proposals))
	return ic.e.CreateMessage(discord.MessageCreate{
		Embeds:     []discord.Embed{embed},
		Components: components,
		Flags:      discord.MessageFlagEphemeral,
	})
}

// reviewView renders one proposal as an embed + action buttons. remaining is
// how many pending proposals are left (including this one), for the footer.
func reviewView(p db.StateProposal, remaining int) (discord.Embed, []discord.LayoutComponent) {
	title := "🆕 New " + entityKindLabel(p.EntityKind)
	color := 0x22c55e // green for create
	if p.Action == db.ActionUpdateEntity {
		title = "✏️ Update " + entityKindLabel(p.EntityKind)
		color = 0x3b82f6 // blue for update
	}

	desc := p.Description()
	if desc == "" {
		desc = "_(no description proposed)_"
	}

	fields := []discord.EmbedField{
		{Name: p.EntityName, Value: truncate(desc, 1000)},
	}
	if p.Explanation != "" {
		fields = append(fields, discord.EmbedField{Name: "Change", Value: truncate(p.Explanation, 1000)})
	}
	if p.Evidence != "" {
		fields = append(fields, discord.EmbedField{Name: "Evidence", Value: truncate(p.Evidence, 1000)})
	}
	// Surface any extra structured patch fields (e.g. quest status) so the DM
	// sees exactly what will be merged into the entity's metadata.
	if extra := patchExtras(p); extra != "" {
		fields = append(fields, discord.EmbedField{Name: "Details", Value: truncate(extra, 1000)})
	}

	footer := fmt.Sprintf("Confidence %.0f%% · %d proposal(s) pending · nothing changes until you approve", p.Confidence*100, remaining)
	embed := discord.Embed{
		Title:       title,
		Description: fmt.Sprintf("Proposed change to **%s**", p.EntityName),
		Color:       color,
		Fields:      fields,
		Footer:      &discord.EmbedFooter{Text: footer},
	}

	buttons := discord.NewActionRow(
		discord.NewSuccessButton("Approve", reviewCustomID(reviewApprove, p.ID)),
		discord.NewDangerButton("Reject", reviewCustomID(reviewReject, p.ID)),
		discord.NewSecondaryButton("Edit", reviewCustomID(reviewEdit, p.ID)),
		discord.NewSecondaryButton("Skip", reviewCustomID(reviewSkip, p.ID)),
	)
	return embed, []discord.LayoutComponent{buttons}
}

// patchExtras renders non-description patch fields as a short "key: value" list.
func patchExtras(p db.StateProposal) string {
	if len(p.Patch) == 0 {
		return ""
	}
	var b strings.Builder
	for k, v := range p.Patch {
		if k == "description" {
			continue
		}
		fmt.Fprintf(&b, "• %s: %v\n", k, v)
	}
	return strings.TrimSpace(b.String())
}

func entityKindLabel(k db.WorldEntityKind) string {
	switch k {
	case db.KindNPC:
		return "NPC"
	case db.KindLocation:
		return "Location"
	case db.KindFaction:
		return "Faction"
	case db.KindQuest:
		return "Quest"
	case db.KindHook:
		return "Story hook"
	case db.KindCharacter:
		return "Player character"
	default:
		return string(k)
	}
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// onComponentInteraction dispatches button clicks. Runs on its own goroutine for
// the same reason as onInteraction (disgo's synchronous, mutex-held dispatch).
func (g *Gateway) onComponentInteraction(e *events.ComponentInteractionCreate) {
	go g.routeComponent(e)
}

func (g *Gateway) routeComponent(e *events.ComponentInteractionCreate) {
	defer func() {
		if r := recover(); r != nil {
			metrics.PanicsRecovered.WithLabelValues("interaction").Inc()
			g.log.Error("component handler panicked; recovered",
				"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
		}
	}()

	data, ok := e.Data.(discord.ButtonInteractionData)
	if !ok {
		return // only buttons are used by this feature
	}
	verb, proposalID, ok := parseReviewCustomID(data.CustomID())
	if !ok {
		return // not one of ours
	}

	ctx, cancel := context.WithTimeout(context.Background(), interactionTimeout)
	defer cancel()
	corrID := logging.NewCorrelationID()
	log := g.log.With(logging.CorrelationIDField, corrID, "component", "review-session", "verb", verb, "proposal", proposalID.String())
	ctx = logging.WithLogger(logging.WithCorrelationID(ctx, g.log, corrID), log)

	if err := g.handleReviewButton(ctx, e, verb, proposalID); err != nil {
		log.Error("review button failed", "err", err)
		_ = e.UpdateMessage(discord.MessageUpdate{
			Content:    ptr("Something went wrong handling that. Please run `/review-session` again."),
			Embeds:     &[]discord.Embed{},
			Components: &[]discord.LayoutComponent{},
		})
	}
}

// handleReviewButton applies one review action and advances to the next pending
// proposal in the SAME ephemeral message (edit-in-place), so the DM moves
// through the queue without re-running the command.
func (g *Gateway) handleReviewButton(ctx context.Context, e *events.ComponentInteractionCreate, verb string, proposalID uuid.UUID) error {
	reviewerID := e.User().ID.String()

	prop, err := g.store.GetStateProposal(ctx, proposalID)
	if errors.Is(err, db.ErrNotFound) {
		return g.advanceReview(ctx, e, uuid.Nil, "That proposal is no longer available.")
	}
	if err != nil {
		return err
	}

	switch verb {
	case reviewEdit:
		// Open a modal pre-filled with the current name/description. The modal's
		// submit (onModalSubmit) saves the edit; the queue isn't advanced here.
		return e.Modal(editModal(*prop))

	case reviewApprove:
		change, applied, aerr := g.store.ApproveProposal(ctx, proposalID, reviewerID)
		if aerr != nil {
			return aerr
		}
		msg := ""
		if applied {
			metrics.StateProposalsReviewed.WithLabelValues("approved").Inc()
			logging.FromContext(ctx, g.log).Info("proposal approved",
				"source_kind", change.SourceKind, "source", change.SourceID, "name", change.DisplayName, "reviewer", reviewerID)
			// Make the newly-canon record retrievable by /ask (async, best-effort).
			// A character proposal with no matching PC has a zero SourceID — skip.
			if change.SourceID != uuid.Nil {
				g.enqueueCanonEmbed(ctx, change.CampaignID, change.SourceKind, change.SourceID)
			}
			if change.SourceKind == db.CanonSourceCharacter && change.SourceID == uuid.Nil {
				msg = fmt.Sprintf("✅ Approved — but I couldn't find a player character named **%s** to attach it to.", prop.EntityName)
			} else {
				msg = fmt.Sprintf("✅ Approved — **%s** is now part of your campaign records.", prop.EntityName)
			}
		} else {
			// Idempotent no-op (already decided / double-click).
			msg = "That proposal was already decided."
		}
		return g.advanceReviewForCampaign(ctx, e, prop.CampaignID, msg)

	case reviewReject:
		changed, rerr := g.store.RejectProposal(ctx, proposalID, reviewerID)
		if rerr != nil {
			return rerr
		}
		msg := "That proposal was already decided."
		if changed {
			metrics.StateProposalsReviewed.WithLabelValues("rejected").Inc()
			msg = fmt.Sprintf("🚫 Rejected — **%s** was not added. Your canon is unchanged.", prop.EntityName)
		}
		return g.advanceReviewForCampaign(ctx, e, prop.CampaignID, msg)

	case reviewSkip:
		return g.advanceReviewForCampaign(ctx, e, prop.CampaignID, fmt.Sprintf("⏭️ Skipped **%s** (still pending).", prop.EntityName))

	default:
		return fmt.Errorf("unknown review verb %q", verb)
	}
}

// advanceReviewForCampaign re-queries the campaign's remaining pending proposals
// and edits the ephemeral message to show the next one (or a done note),
// prefixing the given status line.
func (g *Gateway) advanceReviewForCampaign(ctx context.Context, e *events.ComponentInteractionCreate, campaignID uuid.UUID, status string) error {
	proposals, err := g.store.ListPendingProposalsForCampaign(ctx, campaignID)
	if err != nil {
		return err
	}
	return g.renderAdvance(e, proposals, status)
}

// advanceReview is used when we couldn't resolve the campaign (proposal gone);
// it just clears the message with a status note.
func (g *Gateway) advanceReview(_ context.Context, e *events.ComponentInteractionCreate, _ uuid.UUID, status string) error {
	return g.renderAdvance(e, nil, status)
}

func (g *Gateway) renderAdvance(e *events.ComponentInteractionCreate, proposals []db.StateProposal, status string) error {
	if len(proposals) == 0 {
		content := status + "\n\n🎉 All caught up — no more pending proposals."
		return e.UpdateMessage(discord.MessageUpdate{
			Content:    &content,
			Embeds:     &[]discord.Embed{},
			Components: &[]discord.LayoutComponent{},
		})
	}
	embed, components := reviewView(proposals[0], len(proposals))
	return e.UpdateMessage(discord.MessageUpdate{
		Content:    &status,
		Embeds:     &[]discord.Embed{embed},
		Components: &components,
	})
}

// editModal builds the modal for editing a proposal's name/description before
// approval. Field custom IDs are fixed; the modal's own custom ID carries the
// proposal ID so the submit handler knows which proposal to update.
func editModal(p db.StateProposal) discord.ModalCreate {
	name := discord.NewShortTextInput("name").
		WithValue(p.EntityName).
		WithRequired(true).
		WithPlaceholder("Entity name")
	desc := discord.NewParagraphTextInput("description").
		WithValue(p.Description()).
		WithRequired(false).
		WithPlaceholder("What should the campaign remember about this?")
	return discord.NewModalCreate(reviewCustomID(reviewModal, p.ID), "Edit proposal").
		AddLabel("Name", name).
		AddLabel("Description", desc)
}

// onModalSubmit handles modal submissions (review-proposal edits, and the
// structured /world add and /character add forms).
func (g *Gateway) onModalSubmit(e *events.ModalSubmitInteractionCreate) {
	go g.routeModal(e)
}

func (g *Gateway) routeModal(e *events.ModalSubmitInteractionCreate) {
	defer func() {
		if r := recover(); r != nil {
			metrics.PanicsRecovered.WithLabelValues("interaction").Inc()
			g.log.Error("modal handler panicked; recovered",
				"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), interactionTimeout)
	defer cancel()

	// Dispatch by custom-ID prefix using the modal registry. Each modal family
	// registers a prefix + handler in modalRoutes(), so adding a new modal is a
	// single entry there — no edits to this dispatcher.
	customID := e.Data.CustomID
	for _, r := range g.modalRoutes() {
		if r.match(customID) {
			r.handle(ctx, e)
			return
		}
	}
	// Fallback: the review-proposal edit modal (custom ID carries a UUID, so it
	// isn't a fixed prefix like the others).
	g.handleReviewModalSubmit(ctx, e)
}

// modalRoute binds a custom-ID matcher to a submit handler.
type modalRoute struct {
	match  func(customID string) bool
	handle func(ctx context.Context, e *events.ModalSubmitInteractionCreate)
}

// modalRoutes is the registry of modal-submit handlers. To add a new modal
// family, add one entry here; the dispatcher needs no changes. Order matters
// only if prefixes overlap (they don't).
func (g *Gateway) modalRoutes() []modalRoute {
	hasPrefix := func(p string) func(string) bool {
		return func(id string) bool { return strings.HasPrefix(id, p+":") }
	}
	return []modalRoute{
		{match: hasPrefix(worldAddModalPrefix), handle: func(ctx context.Context, e *events.ModalSubmitInteractionCreate) {
			g.handleWorldEntityModalSubmit(ctx, e, false)
		}},
		{match: hasPrefix(worldEditModalPrefix), handle: func(ctx context.Context, e *events.ModalSubmitInteractionCreate) {
			g.handleWorldEntityModalSubmit(ctx, e, true)
		}},
		{match: func(id string) bool { return id == characterAddModalID }, handle: g.handleCharacterAddModalSubmit},
	}
}

// handleReviewModalSubmit handles the edit-proposal modal submission.
func (g *Gateway) handleReviewModalSubmit(ctx context.Context, e *events.ModalSubmitInteractionCreate) {
	verb, proposalID, ok := parseReviewCustomID(e.Data.CustomID)
	if !ok || verb != reviewModal {
		return
	}

	name := strings.TrimSpace(e.Data.Text("name"))
	desc := strings.TrimSpace(e.Data.Text("description"))
	if name == "" {
		_ = e.CreateMessage(discord.MessageCreate{Content: "A name is required.", Flags: discord.MessageFlagEphemeral})
		return
	}

	if err := g.store.UpdateProposalPatch(ctx, proposalID, name, desc); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			_ = e.CreateMessage(discord.MessageCreate{Content: "That proposal is no longer pending.", Flags: discord.MessageFlagEphemeral})
			return
		}
		g.log.Error("update proposal patch", "err", err, "proposal", proposalID)
		_ = e.CreateMessage(discord.MessageCreate{Content: "Couldn't save that edit. Please try again.", Flags: discord.MessageFlagEphemeral})
		return
	}

	// Re-show the (now edited) proposal so the DM can approve it.
	prop, err := g.store.GetStateProposal(ctx, proposalID)
	if err != nil {
		_ = e.CreateMessage(discord.MessageCreate{Content: "Saved your edit. Run `/review-session` to continue.", Flags: discord.MessageFlagEphemeral})
		return
	}
	remaining := 1
	if n, cerr := g.store.CountPendingProposalsForCampaign(ctx, prop.CampaignID); cerr == nil && n > 0 {
		remaining = n
	}
	embed, components := reviewView(*prop, remaining)
	_ = e.CreateMessage(discord.MessageCreate{
		Content:    "✏️ Saved your edit — review it below.",
		Embeds:     []discord.Embed{embed},
		Components: components,
		Flags:      discord.MessageFlagEphemeral,
	})
}

// ptr returns a pointer to v (helper for MessageUpdate pointer fields).
func ptr[T any](v T) *T { return &v }

// reviewSessionAutocomplete suggests recent sessions that still have pending
// proposals, for the /review-session session_id option.
func (g *Gateway) reviewSessionAutocomplete(ctx context.Context, guildID, userID string, add func(name, value string) bool) {
	gid, ok := g.resolveGuild(ctx, guildID, userID)
	if !ok {
		return
	}
	camp, err := g.store.GetActiveCampaign(ctx, gid)
	if err != nil {
		return
	}
	sessionIDs, err := g.store.SessionIDsWithPendingProposals(ctx, camp.ID)
	if err != nil {
		return
	}
	for _, sid := range sessionIDs {
		label := sid.String()
		if sess, serr := g.store.GetSession(ctx, sid); serr == nil {
			label = fmt.Sprintf("%s — %s", sess.StartedAt.UTC().Format("2006-01-02 15:04"), shortID(sid))
		}
		if !add(label, sid.String()) {
			break
		}
	}
}

func shortID(id uuid.UUID) string {
	s := id.String()
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}
