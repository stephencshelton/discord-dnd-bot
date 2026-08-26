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
	return fmt.Errorf("unknown session subcommand")
}

func (g *Gateway) sessionStart(ctx context.Context, ic *ictx, guildID string) error {
	// Reject if already recording. A non-not-found error means the DB is
	// unreachable — surface it rather than silently starting a second session.
	switch _, err := g.store.GetActiveSession(ctx, guildID); {
	case err == nil:
		return ic.reply("A session is already recording. Use `/session stop` first.", true)
	case !errors.Is(err, db.ErrNotFound):
		return err
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return ic.reply(err.Error(), true)
	}

	// Find the invoking user's voice channel.
	vs, err := g.userVoiceChannel(guildID, ic.userID())
	if err != nil {
		return ic.reply("Join a voice channel first, then run `/session start`.", true)
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
		return ic.reply(
			"⚠️ I couldn't join the voice channel to start recording. Please try again in a "+
				"moment. Text commands (chat, `/ask`, `/recap`, etc.) still work.",
			true)
	}
	return ic.reply("🔴 Recording started. Play on! Use `/session stop` when you're done.", false)
}

func (g *Gateway) sessionStop(ctx context.Context, ic *ictx, guildID string) error {
	sess, err := g.store.GetActiveSession(ctx, guildID)
	if errors.Is(err, db.ErrNotFound) {
		return ic.reply("No active session to stop.", true)
	}
	if err != nil {
		return err
	}
	if err := ic.ack(false); err != nil {
		return err
	}

	// Stop capture, flush the recording, and upload it.
	audioKey, duration, err := g.voice.stop(ctx, guildID)
	if err != nil {
		_ = g.store.SetSessionResult(ctx, sess.ID, "", "", "failed")
		return fmt.Errorf("finalize recording: %w", err)
	}

	if err := g.store.EndSession(ctx, sess.ID, audioKey, int(duration.Seconds())); err != nil {
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
