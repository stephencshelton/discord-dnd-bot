package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/logging"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
)

// handleSession starts/stops/reports voice recording sessions.
func (g *Gateway) handleSession(ctx context.Context, ic *ictx) error {
	guildID := ic.guildID()
	if guildID == "" {
		return ic.reply("Sessions must be run inside a server.", true)
	}

	switch ic.subcommand() {
	case "start":
		return g.sessionStart(ctx, ic, guildID)
	case "stop":
		return g.sessionStop(ctx, ic, guildID)
	case "status":
		return g.sessionStatus(ctx, ic, guildID)
	case "list":
		return g.sessionList(ctx, ic, guildID)
	case "requeue":
		return g.sessionRequeue(ctx, ic, guildID)
	}
	return fmt.Errorf("unknown session subcommand %q", ic.subcommand())
}

func (g *Gateway) sessionStart(ctx context.Context, ic *ictx, guildID string) error {
	log := logging.FromContext(ctx, g.log)

	// Ack immediately (ephemeral) BEFORE any slow work. Joining voice can take
	// many seconds (DAVE/UDP handshake), which blows Discord's 3s interaction
	// window and yields "Unknown interaction" (10062) if we reply late. With a
	// deferred ack we have ~15 minutes to follow up with the real result.
	if err := ic.ack(true); err != nil {
		return err
	}

	// Reject if already recording. A non-not-found error means the DB is
	// unreachable — surface it rather than silently starting a second session.
	switch existing, err := g.store.GetActiveSession(ctx, guildID); {
	case err == nil:
		// The DB says a session is recording. If this pod actually holds the
		// in-memory recording, it's a genuine duplicate. Otherwise the row is
		// stale (pod restart, or a prior start whose voice handshake half-
		// failed) — clear it so the user isn't wedged, then start fresh.
		if g.voice.has(guildID) {
			return ic.followup("A session is already recording. Use `/session stop` first.")
		}
		log.Warn("clearing stale recording session with no in-memory recording",
			"guild", guildID, "session", existing.ID)
		if serr := g.store.SetSessionResult(ctx, existing.ID, "", "", "failed"); serr != nil {
			return serr
		}
	case !errors.Is(err, db.ErrNotFound):
		return err
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.followup(err.Error())
	}

	// Find the invoking user's voice channel.
	vs, err := g.userVoiceChannel(guildID, ic.userID())
	if err != nil {
		return ic.followup("Join a voice channel first, then run `/session start`.")
	}

	sess, err := g.store.CreateSession(ctx, camp.ID, guildID, vs)
	if err != nil {
		return err
	}
	if err := g.voice.start(guildID, vs, sess.ID.String()); err != nil {
		// Roll back the DB row so state stays consistent.
		_ = g.store.SetSessionResult(ctx, sess.ID, "", "", "failed")
		// disgo negotiates Discord's mandatory DAVE (end-to-end voice
		// encryption) as part of the voice handshake, so a failure here is
		// usually a transient connect issue rather than a protocol rejection.
		logging.FromContext(ctx, g.log).Error("voice join failed",
			"guild", guildID, "channel", vs, "err", err)
		metrics.ComponentError("discord", "voice_join")
		return ic.followup(
			"⚠️ I couldn't join the voice channel to start recording. Please try again in a " +
				"moment. Text commands (chat, `/ask`, `/recap`, etc.) still work.")
	}
	return ic.followup("🔴 Recording started. Play on! Use `/session stop` when you're done.")
}

func (g *Gateway) sessionStop(ctx context.Context, ic *ictx, guildID string) error {
	log := logging.FromContext(ctx, g.log)

	// Ack immediately (ephemeral) before the slow finalize/flush work so we
	// never miss Discord's 3s window (see sessionStart).
	if err := ic.ack(true); err != nil {
		return err
	}

	sess, err := g.store.GetActiveSession(ctx, guildID)
	if errors.Is(err, db.ErrNotFound) {
		return ic.followup("No active session to stop.")
	}
	if err != nil {
		return err
	}

	// If the DB says a session is recording but this pod holds no in-memory
	// recording, the audio can't be finalized (pod restart, or a start whose
	// voice handshake never completed). Mark it failed and tell the user
	// clearly instead of erroring with a cryptic "no recording".
	if !g.voice.has(guildID) {
		log.Warn("stop with no in-memory recording; marking session failed",
			"guild", guildID, "session", sess.ID)
		_ = g.store.SetSessionResult(ctx, sess.ID, "", "", "failed")
		return ic.followup(
			"⚠️ I couldn't finalize that recording — I lost the live audio connection " +
				"(the bot may have restarted or never fully joined). The session has been " +
				"cleared; please run `/session start` again.")
	}

	// Stop capture and flush the final tail chunk. The audio is stored as PCM
	// chunks under the session's chunk_prefix; the worker reassembles them.
	_, duration, err := g.voice.stop(ctx, guildID)
	if err != nil {
		_ = g.store.SetSessionResult(ctx, sess.ID, "", "", "failed")
		return fmt.Errorf("finalize recording: %w", err)
	}

	if err := g.store.EndSession(ctx, sess.ID, int(duration.Seconds())); err != nil {
		return err
	}

	// Hand off the heavy transcribe+summarize work to the worker pool.
	if err := g.queue.Enqueue(ctx, queue.JobTranscribeSession, queue.TranscribeSessionPayload{
		SessionID: sess.ID.String(),
		GuildID:   guildID,
	}); err != nil {
		return err
	}
	metrics.JobsEnqueued.WithLabelValues(string(queue.JobTranscribeSession)).Inc()
	return ic.followup(fmt.Sprintf("⏹️ Recorded %s. Transcribing and writing your session notes now — I'll post them when ready.", duration.Round(time.Second)))
}

func (g *Gateway) sessionStatus(ctx context.Context, ic *ictx, guildID string) error {
	sess, err := g.store.GetActiveSession(ctx, guildID)
	if errors.Is(err, db.ErrNotFound) {
		return ic.reply("No session is currently recording. Start one with `/session start`.", true)
	}
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("\U0001F534 Recording since <t:%d:R>.", sess.StartedAt.Unix())
	if parts, perr := g.store.ListParticipants(ctx, sess.ID); perr == nil && len(parts) > 0 {
		names := make([]string, 0, len(parts))
		for _, p := range parts {
			names = append(names, p.DisplayName)
		}
		msg += "\n**Heard so far:** " + strings.Join(names, ", ")
	}
	return ic.reply(msg, true)
}

// sessionList shows the ACTIVE campaign's recent sessions in a given status
// (default "failed") so anyone can spot stuck/failed recordings and grab their
// IDs for `/session requeue`. Scoped to the active campaign so sessions from
// other campaigns in the same server aren't mixed in.
func (g *Gateway) sessionList(ctx context.Context, ic *ictx, guildID string) error {
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}
	status := ic.optString("status")
	if status == "" {
		status = "failed"
	}
	sessions, err := g.store.ListSessionsByStatusForCampaign(ctx, camp.ID, status, 15)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return ic.reply(fmt.Sprintf("No **%s** sessions in **%s**.", status, camp.Name), true)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**%s — sessions with status `%s`** (newest first):\n", camp.Name, status)
	for _, s := range sessions {
		fmt.Fprintf(&b, "• `%s` — started <t:%d:f>\n", s.ID, s.StartedAt.Unix())
	}
	if status == "failed" || status == "processing" {
		b.WriteString("\nRe-run one with `/session requeue session_id:<id>`.")
	}
	if status == "complete" {
		b.WriteString("\nMissing `/review-session` proposals for one? Re-derive them from its saved transcript with " +
			"`/session requeue session_id:<id> proposals_only:true` (no re-transcription).")
	}
	return ic.reply(b.String(), true)
}

// sessionRequeue re-enqueues work for an existing session, letting anyone
// recover a recording whose worker job failed or was lost (e.g. a crash before
// automatic retries were added, or after retries were exhausted). The audio
// chunks still live in object storage, so reprocessing rebuilds the transcript
// and notes from scratch.
//
// With proposals_only it re-runs just the campaign-state extraction step, which
// is the cheap recovery when the transcript and notes are fine but
// /review-session has nothing to show (e.g. the extraction step failed). A full
// requeue would re-transcribe hours of audio to reach the same place.
func (g *Gateway) sessionRequeue(ctx context.Context, ic *ictx, guildID string) error {
	log := logging.FromContext(ctx, g.log)

	id, err := uuid.Parse(strings.TrimSpace(ic.optString("session_id")))
	if err != nil {
		return ic.reply("That doesn't look like a valid session ID. Copy one from `/session list`.", true)
	}

	sess, err := g.store.GetSession(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return ic.reply("No session with that ID.", true)
	}
	if err != nil {
		return err
	}
	// Guard against cross-guild requeues: only act on this server's sessions.
	if sess.GuildID != guildID {
		return ic.reply("That session belongs to a different server.", true)
	}
	// A still-recording session hasn't been finalized, so there's nothing to
	// transcribe yet — don't let a requeue race the live recorder.
	if sess.Status == "recording" {
		return ic.reply("That session is still recording. Stop it with `/session stop` first.", true)
	}

	if ic.optBool("proposals_only") {
		// Extraction needs both a transcript and notes; without them only a full
		// requeue can help, so say that instead of enqueueing a doomed job.
		if strings.TrimSpace(sess.Transcript) == "" || strings.TrimSpace(sess.Notes) == "" {
			return ic.reply("That session has no transcript/notes yet, so there's nothing to derive proposals from. Re-run it without `proposals_only` first.", true)
		}
		if err := g.queue.Enqueue(ctx, queue.JobExtractState, queue.ExtractStatePayload{
			SessionID: sess.ID.String(),
			GuildID:   guildID,
			Notify:    true,
		}); err != nil {
			return err
		}
		metrics.JobsEnqueued.WithLabelValues(string(queue.JobExtractState)).Inc()
		log.Info("session state-extraction requeued", "session", sess.ID, "user", ic.userID())
		return ic.reply(fmt.Sprintf(
			"🧭 Re-deriving world-state proposals for session `%s` from the saved transcript (transcript and notes are untouched). I'll post when they're ready, then run `/review-session`.",
			sess.ID), true)
	}

	// Move it back to processing so `/session list` reflects the retry and a
	// concurrent requeue is less likely to double up.
	if err := g.store.SetSessionResult(ctx, sess.ID, sess.Transcript, sess.Notes, "processing"); err != nil {
		return err
	}
	if err := g.queue.Enqueue(ctx, queue.JobTranscribeSession, queue.TranscribeSessionPayload{
		SessionID: sess.ID.String(),
		GuildID:   guildID,
	}); err != nil {
		return err
	}
	metrics.JobsEnqueued.WithLabelValues(string(queue.JobTranscribeSession)).Inc()
	log.Info("session requeued by admin", "session", sess.ID, "user", ic.userID())
	return ic.reply(fmt.Sprintf("🔁 Requeued session `%s`. I'll post the notes when they're ready.", sess.ID), true)
}

// userVoiceChannel returns the voice channel ID the user is currently in.
func (g *Gateway) userVoiceChannel(guildID, userID string) (string, error) {
	gid, err1 := snowflake.Parse(guildID)
	uid, err2 := snowflake.Parse(userID)
	if err1 != nil {
		return "", err1
	}
	if err2 != nil {
		return "", err2
	}
	vs, ok := g.client.Caches.VoiceState(gid, uid)
	if !ok || vs.ChannelID == nil {
		return "", errors.New("user not in a voice channel")
	}
	return vs.ChannelID.String(), nil
}
