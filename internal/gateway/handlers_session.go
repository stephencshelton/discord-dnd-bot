package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
)

// handleSession starts/stops/reports voice recording sessions.
func (g *Gateway) handleSession(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	guildID := i.GuildID
	if guildID == "" {
		return g.reply(s, i, "Sessions must be run inside a server.", true)
	}
	sub := i.ApplicationCommandData().Options[0]

	switch sub.Name {
	case "start":
		return g.sessionStart(ctx, s, i, guildID)
	case "stop":
		return g.sessionStop(ctx, s, i, guildID)
	case "status":
		return g.sessionStatus(ctx, s, i, guildID)
	}
	return fmt.Errorf("unknown session subcommand")
}

func (g *Gateway) sessionStart(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, guildID string) error {
	// Reject if already recording. A non-not-found error means the DB is
	// unreachable — surface it rather than silently starting a second session.
	switch _, err := g.store.GetActiveSession(ctx, guildID); {
	case err == nil:
		return g.reply(s, i, "A session is already recording. Use `/session stop` first.", true)
	case !errors.Is(err, db.ErrNotFound):
		return err
	}
	camp, err := g.activeCampaign(ctx, guildID)
	if err != nil {
		return g.reply(s, i, err.Error(), true)
	}

	// Find the invoking user's voice channel.
	vs, err := g.userVoiceChannel(guildID, interactionUser(i))
	if err != nil {
		return g.reply(s, i, "Join a voice channel first, then run `/session start`.", true)
	}

	sess, err := g.store.CreateSession(ctx, camp.ID, guildID, vs)
	if err != nil {
		return err
	}
	if err := g.voice.start(guildID, vs, sess.ID.String()); err != nil {
		// Roll back the DB row so state stays consistent.
		_ = g.store.SetSessionResult(ctx, sess.ID, "", "", "failed")
		return fmt.Errorf("join voice: %w", err)
	}
	return g.reply(s, i, "🔴 Recording started. Play on! Use `/session stop` when you're done.", false)
}

func (g *Gateway) sessionStop(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, guildID string) error {
	sess, err := g.store.GetActiveSession(ctx, guildID)
	if errors.Is(err, db.ErrNotFound) {
		return g.reply(s, i, "No active session to stop.", true)
	}
	if err != nil {
		return err
	}
	if err := g.ack(s, i, false); err != nil {
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
	return g.followup(s, i, fmt.Sprintf("⏹️ Recorded %s. Transcribing and writing your session notes now — I'll post them when ready.", duration.Round(time.Second)))
}

func (g *Gateway) sessionStatus(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, guildID string) error {
	sess, err := g.store.GetActiveSession(ctx, guildID)
	if errors.Is(err, db.ErrNotFound) {
		return g.reply(s, i, "No session is currently recording. Start one with `/session start`.", true)
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
	return g.reply(s, i, msg, true)
}

// userVoiceChannel returns the voice channel ID the user is currently in.
func (g *Gateway) userVoiceChannel(guildID, userID string) (string, error) {
	guild, err := g.sess.State.Guild(guildID)
	if err != nil {
		return "", err
	}
	for _, vs := range guild.VoiceStates {
		if vs.UserID == userID {
			return vs.ChannelID, nil
		}
	}
	return "", errors.New("user not in a voice channel")
}
