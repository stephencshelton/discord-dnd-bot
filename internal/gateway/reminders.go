package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
)

// RunReminderLoop polls for due reminders, posts them to Discord, and
// reschedules each to its next weekly occurrence. Runs in the gateway process
// since that's where the authenticated Discord session lives.
//
// Safe across multiple gateway replicas: each reminder is claimed with a
// conditional UPDATE (ClaimReminder) before posting, so only the winning
// replica posts, and a crash mid-claim drops one reminder rather than
// double-posting.
func RunReminderLoop(ctx context.Context, log *slog.Logger, store *db.Store, client *bot.Client, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Info("reminder loop started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			log.Info("reminder loop stopped")
			return
		case <-ticker.C:
			// Recover per tick so a panic in one cycle doesn't kill the loop.
			func() {
				defer func() {
					if r := recover(); r != nil {
						metrics.PanicsRecovered.WithLabelValues("goroutine").Inc()
						log.Error("reminder tick panicked; recovered",
							"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
					}
				}()
				tickReminders(ctx, log, store, client)
			}()
		}
	}
}

func tickReminders(ctx context.Context, log *slog.Logger, store *db.Store, client *bot.Client) {
	due, err := store.DueReminders(ctx, time.Now().UTC())
	if err != nil {
		log.Error("query due reminders", "err", err)
		return
	}
	for _, r := range due {
		next, err := nextRunFromSchedule(r.Schedule, time.Now().UTC())
		if err != nil {
			log.Error("parse reminder schedule", "schedule", r.Schedule, "err", err)
			// Bump a week ahead as a fallback so we don't hot-loop on it.
			next = time.Now().UTC().AddDate(0, 0, 7)
		}
		// Only the replica whose conditional UPDATE wins proceeds to post.
		claimed, err := store.ClaimReminder(ctx, r.ID, r.NextRun, next)
		if err != nil {
			log.Error("claim reminder", "id", r.ID, "err", err)
			continue
		}
		if !claimed {
			continue // another replica already claimed and will post it
		}

		channelID := r.ChannelID
		// Prefer the guild's configured notes channel; fall back to the reminder's own.
		if g, gerr := store.GetGuild(ctx, r.GuildID); gerr == nil && g.NotesChannelID != "" {
			channelID = g.NotesChannelID
		}

		msg := "🎲 **Session reminder!** Your next game is coming up. See you at the table!"
		if camp, cerr := store.GetCampaign(ctx, r.CampaignID); cerr == nil {
			msg = fmt.Sprintf("🎲 **%s** — session reminder! Your next game is coming up. See you at the table!", camp.Name)
		}
		cid, perr := snowflake.Parse(channelID)
		if perr != nil {
			log.Error("parse reminder channel", "channel", channelID, "err", perr)
			continue
		}
		if _, err := client.Rest.CreateMessage(cid, discord.MessageCreate{Content: msg}); err != nil {
			log.Error("post reminder", "channel", channelID, "err", err)
		}
	}
}

// nextRunFromSchedule parses a stored schedule string ("weekly:<weekday>:<HH:MM>")
// and returns the next occurrence after `from`.
func nextRunFromSchedule(schedule string, from time.Time) (time.Time, error) {
	// Format weekly:<weekday>:<HH:MM> e.g. "weekly:3:18:30"; split into 3 parts
	// so the clock's colon stays intact.
	parts := strings.SplitN(schedule, ":", 3)
	if len(parts) != 3 || parts[0] != "weekly" {
		return time.Time{}, fmt.Errorf("unsupported schedule %q", schedule)
	}
	weekday, err := strconv.Atoi(parts[1])
	if err != nil || weekday < 0 || weekday > 6 {
		return time.Time{}, fmt.Errorf("invalid weekday in schedule %q", schedule)
	}
	return nextWeekly(from, time.Weekday(weekday), parts[2])
}
