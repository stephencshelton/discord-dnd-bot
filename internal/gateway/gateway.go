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

// resolveGuild determines which guild a command should operate against. In a
// guild it's simply i.GuildID. In a DM there is no guild, so it resolves the
// user's selected/sole shared guild via directMessageGuildID. Returns
// ("", false) when a DM can't be mapped to a single guild (unset selection with
// multiple shared servers, or no shared servers) — the caller should tell the
// user to run /dm-server.
func (g *Gateway) resolveGuild(ctx context.Context, i *discordgo.InteractionCreate) (string, bool) {
	if i.GuildID != "" {
		return i.GuildID, true
	}
	return g.directMessageGuildID(ctx, interactionUserID(i))
}

// dmGuildHelp is the message shown when a DM command can't resolve a guild.
const dmGuildHelp = "I couldn't tell which server's campaign to use. If you're in more than one of my servers, run `/dm-server` here to pick one (and to switch later)."

// directMessageGuildID resolves a DM to a single allowlisted guild for the
// user. Resolution order:
//  1. the user's explicitly selected DM guild (via /dm-server), if they're
//     still an allowlisted member of it;
//  2. otherwise, if they belong to exactly one allowlisted guild, that guild;
//  3. otherwise ambiguous — no guild (the caller explains why to the user).
//
// ctx is used to read the stored preference; a store error falls back to the
// membership-based resolution rather than failing the DM outright.
func (g *Gateway) directMessageGuildID(ctx context.Context, userID string) (string, bool) {
	if g.isGuildMember == nil {
		return "", false
	}

	// 1. Honor an explicit selection when the user is still a valid member.
	if g.store != nil {
		if pref, err := g.store.GetDMGuildID(ctx, userID); err == nil && pref != "" {
			if _, ok := g.allowedGuilds[pref]; ok && g.isGuildMember(pref, userID) {
				return pref, true
			}
		}
	}

	// 2. Fall back to the sole shared guild.
	matchedGuildID := ""
	for guildID := range g.allowedGuilds {
		if !g.isGuildMember(guildID, userID) {
			continue
		}
		if matchedGuildID != "" {
			return "", false // ambiguous: multiple guilds and no valid preference
		}
		matchedGuildID = guildID
	}
	return matchedGuildID, matchedGuildID != ""
}

// sharedGuildIDs returns the allowlisted guilds the user is a member of.
func (g *Gateway) sharedGuildIDs(userID string) []string {
	if g.isGuildMember == nil {
		return nil
	}
	var out []string
	for guildID := range g.allowedGuilds {
		if g.isGuildMember(guildID, userID) {
			out = append(out, guildID)
		}
	}
	return out
}

// dmRejectReason explains, for logging/troubleshooting, why a DM from userID is
// not actioned. It mirrors directMessageGuildID's logic without side effects.
func (g *Gateway) dmRejectReason(userID string) string {
	if g.isGuildMember == nil {
		return "membership lookup unavailable"
	}
	switch matches := len(g.sharedGuildIDs(userID)); {
	case matches == 0:
		return "user is not a member of any allowlisted guild"
	case matches > 1:
		return "user is a member of multiple allowlisted guilds and has no /dm-server selection"
	default:
		return "allowed"
	}
}

func (g *Gateway) onReady(s *discordgo.Session, r *discordgo.Ready) {
	g.log.Info("discord ready", "user", r.User.Username, "guilds", len(r.Guilds))
}
