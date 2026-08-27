package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/disgoorg/snowflake/v2"

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
