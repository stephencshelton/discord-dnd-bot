// Package gateway implements the Discord-facing service: owns the gateway
// connection, registers slash commands, handles interactions/mentions/DMs, and
// records voice. Slow AI work is pushed to the worker queue, keeping the
// gateway responsive (Discord requires an ack within 3 seconds).
//
// Uses disgo (github.com/disgoorg/disgo) which implements Discord's DAVE
// end-to-end voice encryption (required by Discord as of 2026), via the pure-Go
// dave-go backend. IDs are Discord snowflakes; disgo models them as
// snowflake.ID while the rest of the app (config, db, queue) uses strings, so we
// convert at the disgo boundary.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	davesession "github.com/thomas-vilte/dave-go/session"

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
	client        *bot.Client
	store         *db.Store
	queue         *queue.Queue
	ai            *litellm.Client
	storage       *storage.Store
	voice         *voiceManager
	appID         snowflake.ID
	allowedGuilds map[string]struct{}
	isGuildMember func(guildID, userID string) bool
}

// New wires up the gateway. It does not open the connection yet.
func New(cfg *config.Config, log *slog.Logger, store *db.Store, q *queue.Queue, ai *litellm.Client, st *storage.Store) (*Gateway, error) {
	allowedGuilds := make(map[string]struct{})
	for _, guildID := range cfg.Discord.AllowedGuildIDs() {
		allowedGuilds[guildID] = struct{}{}
	}

	g := &Gateway{
		cfg:           cfg,
		log:           log,
		store:         store,
		queue:         q,
		ai:            ai,
		storage:       st,
		allowedGuilds: allowedGuilds,
	}
	g.voice = newVoiceManager(g)

	client, err := disgo.New(cfg.Discord.Token,
		// Surface disgo's internal logs (incl. voice gateway / UDP / DAVE
		// handshake) through our structured logger for diagnosability.
		bot.WithLogger(log),
		// Guild + voice-state intents for recording; message content for mention/DM.
		// Disable gateway stream compression: disgo master defaults to
		// zstd-stream, whose decompressor can buffer events so the bot's own
		// VOICE_STATE_UPDATE/VOICE_SERVER_UPDATE arrive batched ~30s late,
		// starving conn.Open of the events it waits on (voice never connects).
		// Uncompressed delivers every event immediately.
		bot.WithGatewayConfigOpts(
			gateway.WithCompression(gateway.CompressionNone),
			gateway.WithIntents(
				gateway.IntentGuilds,
				gateway.IntentGuildVoiceStates,
				gateway.IntentGuildMessages,
				gateway.IntentDirectMessages,
				gateway.IntentMessageContent,
			),
		),
		// Cache guilds, members and voice states so /session can resolve the
		// caller's voice channel and voice recording can resolve display names.
		bot.WithCacheConfigOpts(cache.WithCaches(
			cache.FlagGuilds,
			cache.FlagMembers,
			cache.FlagVoiceStates,
			cache.FlagChannels,
		)),
		// DAVE (E2EE voice) via the pure-Go dave-go backend. Requires a disgo build
		// with the RTP-padding decrypt fix (disgo #594); older builds fail to
		// decrypt every incoming packet. Toggle off via DISCORD_DISABLE_DAVE (falls
		// back to the noop session, transport-only encryption) — see config.
		bot.WithVoiceManagerConfigOpts(
			voice.WithDaveSessionCreateFunc(daveSessionCreateFunc(cfg.Discord.DisableDAVE)),
		),
		bot.WithEventListenerFunc(g.onReady),
		bot.WithEventListenerFunc(g.onInteraction),
		bot.WithEventListenerFunc(g.onAutocomplete),
		bot.WithEventListenerFunc(g.onMessageCreate),
		// Diagnostics for the voice handshake: these fire only when Discord sends
		// the voice server/state for our bot, so their presence (or absence) in
		// logs pinpoints where a /session voice join stalls.
		bot.WithEventListenerFunc(g.onVoiceServerUpdate),
		bot.WithEventListenerFunc(g.onVoiceStateUpdate),
	)
	if err != nil {
		return nil, fmt.Errorf("create discord client: %w", err)
	}
	g.client = client
	g.isGuildMember = func(guildID, userID string) bool {
		gid, err1 := snowflake.Parse(guildID)
		uid, err2 := snowflake.Parse(userID)
		if err1 != nil || err2 != nil {
			return false
		}
		_, ok := client.Caches.Member(gid, uid)
		if ok {
			return true
		}
		// Fall back to REST if not cached.
		if _, rerr := client.Rest.GetMember(gid, uid); rerr == nil {
			return true
		}
		return false
	}
	if cfg.Discord.AppID != "" {
		if id, perr := snowflake.Parse(cfg.Discord.AppID); perr == nil {
			g.appID = id
		}
	}
	return g, nil
}

// daveSessionCreateFunc returns the DAVE (E2EE voice) session factory. Enabled
// (the default) it returns the real pure-Go dave-go session (protocol v1);
// disabled (DISCORD_DISABLE_DAVE=true) it returns the noop session (protocol
// v0), which yields transport-only encrypted voice. DAVE requires a disgo build
// with the RTP-padding decrypt fix (disgo #594).
func daveSessionCreateFunc(disabled bool) godave.SessionCreateFunc {
	if disabled {
		return godave.NewNoopSession
	}
	return davesession.CreateFunc()
}

// Open connects to Discord and registers commands.
func (g *Gateway) Open(ctx context.Context) error {
	if err := g.client.OpenGateway(ctx); err != nil {
		return fmt.Errorf("open gateway: %w", err)
	}
	if g.appID == 0 {
		g.appID = g.client.ApplicationID
	}
	if err := g.registerCommands(); err != nil {
		return fmt.Errorf("register commands: %w", err)
	}
	// Resume any sessions the previous pod was recording (crash/rollout). This
	// runs after the gateway is open so the guild/voice-state caches can populate
	// and the voice channel can be rejoined. Best-effort; it logs and moves on.
	// Derive from the caller's (process-lifetime, signal-scoped) context so the
	// goroutine is cancelled on shutdown rather than running detached.
	go func() {
		// Give the guild caches a moment to hydrate before rejoining voice.
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return
		}
		rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		g.voice.resumeActive(rctx)
	}()
	return nil
}

// Close disconnects cleanly.
func (g *Gateway) Close() error {
	g.voice.stopAll()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g.client.Close(ctx)
	return nil
}

// Client exposes the underlying disgo client for auxiliary loops (e.g. the
// reminder scheduler) that need to post messages.
func (g *Gateway) Client() *bot.Client { return g.client }

// Ready reports readiness for the k8s probe: the gateway is ready once the
// client has an application ID (assigned after the gateway opens).
func (g *Gateway) Ready(context.Context) error {
	if g.client == nil || g.client.ApplicationID == 0 {
		return fmt.Errorf("discord client not ready")
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
// guild it's simply the interaction's guild. In a DM there is no guild, so it
// resolves the user's selected/sole shared guild via directMessageGuildID.
// Returns ("", false) when a DM can't be mapped to a single guild (unset
// selection with multiple shared servers, or no shared servers) — the caller
// should tell the user to run /dm-server.
func (g *Gateway) resolveGuild(ctx context.Context, guildID, userID string) (string, bool) {
	if guildID != "" {
		return guildID, true
	}
	return g.directMessageGuildID(ctx, userID)
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

func (g *Gateway) onReady(_ *events.Ready) {
	g.log.Info("discord ready", "bot_id", g.client.ID().String())
}

// onVoiceServerUpdate logs the VOICE_SERVER_UPDATE that carries the voice
// endpoint+token. disgo needs this to open the voice gateway; if a /session
// join stalls and this never logs, Discord isn't sending it (or intents/routing
// are wrong). Purely diagnostic — disgo's voice manager handles it internally.
func (g *Gateway) onVoiceServerUpdate(e *events.VoiceServerUpdate) {
	endpoint := ""
	if e.Endpoint != nil {
		endpoint = *e.Endpoint
	}
	g.log.Info("voice server update received",
		"guild", e.GuildID.String(), "endpoint", endpoint, "has_token", e.Token != "")
}

// onVoiceStateUpdate logs our own bot's voice-state transitions (join/leave),
// which carry the session_id disgo needs for the voice handshake.
func (g *Gateway) onVoiceStateUpdate(e *events.GuildVoiceStateUpdate) {
	if e.VoiceState.UserID != g.client.ID() {
		return
	}
	ch := ""
	if e.VoiceState.ChannelID != nil {
		ch = e.VoiceState.ChannelID.String()
	}
	g.log.Info("bot voice state update",
		"guild", e.VoiceState.GuildID.String(), "channel", ch,
		"session_id_present", e.VoiceState.SessionID != "")
}

// displayName resolves a member's best display name (guild nick > global name >
// username), falling back to the raw user ID. It checks the cache first, then
// REST. Never returns an empty string.
func (g *Gateway) displayName(guildID, userID string) string {
	gid, err1 := snowflake.Parse(guildID)
	uid, err2 := snowflake.Parse(userID)
	if err1 != nil || err2 != nil {
		return userID
	}
	if m, ok := g.client.Caches.Member(gid, uid); ok {
		if name := m.EffectiveName(); name != "" {
			return name
		}
	}
	if m, err := g.client.Rest.GetMember(gid, uid); err == nil && m != nil {
		if name := m.EffectiveName(); name != "" {
			return name
		}
	}
	return userID
}
