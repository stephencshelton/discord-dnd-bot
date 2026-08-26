// Package gateway implements the Discord-facing service: owns the gateway
// connection, registers slash commands, handles interactions/mentions/DMs, and
// records voice. Slow AI work is pushed to the worker queue, keeping the
// gateway responsive (Discord requires an ack within 3 seconds).
package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"

	"github.com/stephencshelton/discord-dnd-bot/internal/config"
	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
	"github.com/stephencshelton/discord-dnd-bot/internal/storage"
)

// Gateway is the Discord service.
type Gateway struct {
	cfg           *config.Config
	log           *slog.Logger
	sess          *discordgo.Session
	store         *db.Store
	queue         *queue.Queue
	ai            *litellm.Client
	storage       *storage.Store
	voice         *voiceManager
	appID         string
	regIDs        []string // registered command IDs, for cleanup
	allowedGuilds map[string]struct{}
	allowDMs      bool
	isGuildMember func(guildID, userID string) bool
}

// New wires up the gateway. It does not open the connection yet.
func New(cfg *config.Config, log *slog.Logger, store *db.Store, q *queue.Queue, ai *litellm.Client, st *storage.Store) (*Gateway, error) {
	sess, err := discordgo.New("Bot " + cfg.Discord.Token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	// Guild + voice-state intents for recording; message content for mention/DM.
	sess.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildVoiceStates |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent

	allowedGuilds := make(map[string]struct{})
	for _, guildID := range cfg.Discord.AllowedGuildIDs() {
		allowedGuilds[guildID] = struct{}{}
	}

	g := &Gateway{
		cfg:           cfg,
		log:           log,
		sess:          sess,
		store:         store,
		queue:         q,
		ai:            ai,
		storage:       st,
		appID:         cfg.Discord.AppID,
		allowedGuilds: allowedGuilds,
		allowDMs:      cfg.Discord.AllowDirectMessages,
		isGuildMember: func(guildID, userID string) bool {
			_, err := sess.GuildMember(guildID, userID)
			return err == nil
		},
	}
	g.voice = newVoiceManager(g)

	sess.AddHandler(g.onReady)
	sess.AddHandler(g.onInteraction)
	sess.AddHandler(g.onMessageCreate)
	return g, nil
}

// Open connects to Discord and registers commands.
func (g *Gateway) Open(ctx context.Context) error {
	if err := g.sess.Open(); err != nil {
		return fmt.Errorf("open gateway: %w", err)
	}
	if g.appID == "" {
		g.appID = g.sess.State.User.ID
	}
	if err := g.registerCommands(); err != nil {
		return fmt.Errorf("register commands: %w", err)
	}
	return nil
}

// Close disconnects cleanly.
func (g *Gateway) Close() error {
	g.voice.stopAll()
	return g.sess.Close()
}

// Session exposes the underlying discordgo session for auxiliary loops (e.g. the
// reminder scheduler) that need to post messages.
func (g *Gateway) Session() *discordgo.Session { return g.sess }

// Ready reports readiness for the k8s probe: the gateway is ready once the
// session has an authenticated user.
func (g *Gateway) Ready(context.Context) error {
	if g.sess == nil || g.sess.State == nil || g.sess.State.User == nil {
		return fmt.Errorf("discord session not ready")
	}
	return nil
}

func (g *Gateway) allowsGuild(guildID string) bool {
	if guildID == "" {
		return false
	}
	_, allowed := g.allowedGuilds[guildID]
	return allowed
}

// directMessageGuildID resolves a DM to the sole allowlisted guild containing
// the user. Multiple memberships are rejected to avoid mixing campaign data.
func (g *Gateway) directMessageGuildID(userID string) (string, bool) {
	if !g.allowDMs || g.isGuildMember == nil {
		return "", false
	}

	matchedGuildID := ""
	for guildID := range g.allowedGuilds {
		if !g.isGuildMember(guildID, userID) {
			continue
		}
		if matchedGuildID != "" {
			return "", false
		}
		matchedGuildID = guildID
	}
	return matchedGuildID, matchedGuildID != ""
}

func (g *Gateway) onReady(s *discordgo.Session, r *discordgo.Ready) {
	g.log.Info("discord ready", "user", r.User.Username, "guilds", len(r.Guilds))
}
