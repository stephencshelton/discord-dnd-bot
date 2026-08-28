// Package config loads all runtime configuration from the environment.
//
// Shared by every service (gateway, worker, api) for uniform, 12-factor
// config: everything comes from env vars, mapping cleanly to Kubernetes
// ConfigMaps/Secrets. Nothing is read from disk.
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// Config is the fully-resolved configuration for a discord-dnd-bot process.
type Config struct {
	// Service identifies which binary is running (gateway|worker|api). Used for
	// logging and metrics labels.
	Service string `envconfig:"SERVICE" default:"gateway"`

	// LogLevel controls slog verbosity: debug|info|warn|error.
	LogLevel string `envconfig:"LOG_LEVEL" default:"info"`

	// HTTPAddr is the address the health/metrics server binds to.
	HTTPAddr string `envconfig:"HTTP_ADDR" default:":8080"`

	Discord  DiscordConfig
	Database DatabaseConfig
	Redis    RedisConfig
	Storage  StorageConfig
	LiteLLM  LiteLLMConfig
	Audio    AudioConfig
	Worker   WorkerConfig
}

// DiscordConfig holds Discord bot credentials and behavior toggles.
type DiscordConfig struct {
	// Token is the bot token. Required for the gateway service.
	Token string `envconfig:"DISCORD_TOKEN"`
	// AppID is the application (client) ID, used for registering slash commands.
	AppID string `envconfig:"DISCORD_APP_ID"`
	// GuildID is kept for single-guild dev setups; prefer GuildIDs in production.
	GuildID string `envconfig:"DISCORD_GUILD_ID"`
	// GuildIDs registers commands only in the listed guilds. Empty disables
	// guild integrations.
	GuildIDs []string `envconfig:"DISCORD_GUILD_IDS"`
	// AllowDirectMessages is DEPRECATED and no longer consulted: DMs are always
	// enabled. Retained so existing env/values with DISCORD_ALLOW_DIRECT_MESSAGES
	// still parse. Remove after the setting is dropped from deployments.
	AllowDirectMessages bool `envconfig:"DISCORD_ALLOW_DIRECT_MESSAGES" default:"false"`
	// DisableDAVE controls Discord's DAVE end-to-end voice encryption. Default
	// false (DAVE ENABLED) via the pure-Go thomas-vilte/dave-go backend.
	//
	// Earlier every incoming packet failed to DAVE-decrypt; the root cause was a
	// disgo bug (RTP padding bit masked as 0x04 instead of 0x20 — disgo #593,
	// fixed in #594) that left RTP padding attached and pushed the DAVE frame
	// marker off the end of the buffer. Fixed by upgrading disgo past that PR.
	//
	// Set DISCORD_DISABLE_DAVE=true to fall back to the noop session (advertises
	// DAVE v0): voice is then only transport-encrypted. Kept as an escape hatch.
	DisableDAVE bool `envconfig:"DISCORD_DISABLE_DAVE" default:"false"`
	// FeedbackDMUserID is the Discord user ID that receives a DM whenever someone
	// submits /feedback. Feedback is always stored in the DB regardless; this is
	// just a real-time notification. Empty disables the DM (DB-only).
	FeedbackDMUserID string `envconfig:"DISCORD_FEEDBACK_DM_USER_ID" default:"396406966458515460"`
}

// AllowedGuildIDs returns the configured command allowlist with empty and
// duplicate values removed. The legacy single-guild setting is a fallback.
func (c DiscordConfig) AllowedGuildIDs() []string {
	ids := c.GuildIDs
	if len(ids) == 0 && c.GuildID != "" {
		ids = []string{c.GuildID}
	}

	seen := make(map[string]struct{}, len(ids))
	allowed := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		allowed = append(allowed, id)
	}
	return allowed
}

// DatabaseConfig holds PostgreSQL connection settings. Either provide a full
// URL, or the discrete parts (Host/Port/User/Password/Name/SSLMode) and let the
// bot assemble the DSN — the latter keeps the password out of a hand-built URL
// so only DATABASE_PASSWORD needs to be a secret.
type DatabaseConfig struct {
	// URL is a full libpq/pgx connection string. When set it takes precedence
	// over the discrete fields below.
	URL string `envconfig:"DATABASE_URL"`

	// Discrete connection parts, used to build the DSN when URL is empty.
	Host     string `envconfig:"DATABASE_HOST" default:"localhost"`
	Port     int    `envconfig:"DATABASE_PORT" default:"5432"`
	User     string `envconfig:"DATABASE_USER" default:"discord_dnd_bot"`
	Password string `envconfig:"DATABASE_PASSWORD" default:"discord_dnd_bot"`
	Name     string `envconfig:"DATABASE_NAME" default:"discord_dnd_bot"`
	SSLMode  string `envconfig:"DATABASE_SSLMODE" default:"disable"`

	MaxConns        int32         `envconfig:"DATABASE_MAX_CONNS" default:"10"`
	ConnMaxLifetime time.Duration `envconfig:"DATABASE_CONN_MAX_LIFETIME" default:"1h"`
}

// DSN returns the connection string: DATABASE_URL if provided, otherwise a URL
// assembled from the discrete parts (with the password percent-escaped).
func (c DatabaseConfig) DSN() string {
	if strings.TrimSpace(c.URL) != "" {
		return c.URL
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.User, c.Password),
		Host:     fmt.Sprintf("%s:%d", c.Host, c.Port),
		Path:     "/" + c.Name,
		RawQuery: "sslmode=" + url.QueryEscape(c.SSLMode),
	}
	return u.String()
}

// RedisConfig holds Redis connection settings used for the job queue and cache.
type RedisConfig struct {
	Addr     string `envconfig:"REDIS_ADDR" default:"localhost:6379"`
	Password string `envconfig:"REDIS_PASSWORD"`
	DB       int    `envconfig:"REDIS_DB" default:"0"`
}

// StorageConfig holds S3-compatible object storage settings (MinIO/S3/R2).
// Raw session audio and generated art are stored here rather than in the DB.
type StorageConfig struct {
	Endpoint        string `envconfig:"STORAGE_ENDPOINT"` // empty => real AWS S3
	Region          string `envconfig:"STORAGE_REGION" default:"us-east-1"`
	Bucket          string `envconfig:"STORAGE_BUCKET" default:"discord-dnd-bot"`
	AccessKeyID     string `envconfig:"STORAGE_ACCESS_KEY_ID"`
	SecretAccessKey string `envconfig:"STORAGE_SECRET_ACCESS_KEY"`
	// UsePathStyle is required for MinIO and most S3-compatible services.
	UsePathStyle bool `envconfig:"STORAGE_USE_PATH_STYLE" default:"true"`
}

// LiteLLMConfig points at the LiteLLM proxy, which presents an OpenAI-compatible
// API so one base URL + key fronts every model. Each endpoint (notes, recap,
// lore, transcribe, image, embeddings) has its own model setting so operators
// can pick the cheapest capable model per task without code changes.
type LiteLLMConfig struct {
	BaseURL string `envconfig:"LITELLM_BASE_URL" default:"http://litellm:4000"`
	APIKey  string `envconfig:"LITELLM_API_KEY"`

	// ChatModel is the default chat route used when a task-specific model below
	// is left empty. Retained for backward compatibility.
	ChatModel string `envconfig:"LITELLM_CHAT_MODEL" default:"dnd-chat"`

	// Task-specific chat routes. Empty values fall back to ChatModel via the
	// resolver methods, so existing single-model deployments keep working.
	NotesModel string `envconfig:"LITELLM_NOTES_MODEL"`
	RecapModel string `envconfig:"LITELLM_RECAP_MODEL"`
	LoreModel  string `envconfig:"LITELLM_LORE_MODEL"`
	AskModel   string `envconfig:"LITELLM_ASK_MODEL"`

	TranscribeModel string `envconfig:"LITELLM_TRANSCRIBE_MODEL" default:"voice-transcribe"`
	ImageModel      string `envconfig:"LITELLM_IMAGE_MODEL" default:"dnd-image"`
	// EmbedModel powers transcript retrieval for the grounded /ask command.
	EmbedModel string `envconfig:"LITELLM_EMBED_MODEL" default:"dnd-embed"`
	// EmbedDim is the vector dimensionality EmbedModel returns; it sizes the
	// pgvector column and must match the embed route (e.g. 1536 for OpenAI
	// text-embedding-3-small, 1024 for Titan Embed v2). Changing it requires
	// re-embedding existing rows.
	EmbedDim int `envconfig:"LITELLM_EMBED_DIM" default:"1536"`

	RequestTimeout time.Duration `envconfig:"LITELLM_REQUEST_TIMEOUT" default:"120s"`
	// UploadTimeout bounds large multipart uploads (audio transcription), kept
	// separate from RequestTimeout since audio chunks can take longer than a
	// chat completion.
	UploadTimeout time.Duration `envconfig:"LITELLM_UPLOAD_TIMEOUT" default:"300s"`
}

// Notes returns the model for session-note generation, falling back to ChatModel.
func (c LiteLLMConfig) Notes() string { return firstNonEmpty(c.NotesModel, c.ChatModel) }

// Recap returns the model for /recap, falling back to ChatModel.
func (c LiteLLMConfig) Recap() string { return firstNonEmpty(c.RecapModel, c.ChatModel) }

// Lore returns the model for /lore, falling back to ChatModel.
func (c LiteLLMConfig) Lore() string { return firstNonEmpty(c.LoreModel, c.ChatModel) }

// Ask returns the model for the grounded /ask command, falling back to ChatModel.
func (c LiteLLMConfig) Ask() string { return firstNonEmpty(c.AskModel, c.ChatModel) }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// AudioConfig tunes recording/transcription behavior, letting a deployment
// trade cost (silence trimming) against quality/latency without code changes.
type AudioConfig struct {
	// SilenceTrim enables dropping near-silent frames before upload to cut
	// billed transcription minutes.
	SilenceTrim bool `envconfig:"AUDIO_SILENCE_TRIM" default:"true"`
	// SilenceRMSThreshold is the per-frame RMS amplitude (0..32767) below which
	// a frame is considered silence when SilenceTrim is enabled.
	SilenceRMSThreshold int `envconfig:"AUDIO_SILENCE_RMS_THRESHOLD" default:"350"`
	// TranscribeSegmentMinutes bounds how much of a speaker's track the worker
	// buffers and transcribes at once. A long track is split into <= this many
	// minutes per WAV segment (with a few seconds of overlap so a word spanning
	// a boundary isn't lost), so BOTH the worker's peak RAM and the STT backend's
	// scale with the segment length, NOT the (possibly multi-hour) session length.
	// Segments are downmixed to mono before encoding, halving STT input size.
	// Keep this small enough that one mono segment fits the STT backend's memory
	// (faster-whisper-medium OOMs on ~10-min segments at 8Gi; 3 min is safe). 0
	// disables segmenting (whole track in one request — only safe for short sessions).
	TranscribeSegmentMinutes int `envconfig:"AUDIO_TRANSCRIBE_SEGMENT_MINUTES" default:"3"`
}

// WorkerConfig controls in-pod job concurrency. Combined with HPA on queue
// depth, this scales the worker fleet up under load and down when idle.
type WorkerConfig struct {
	// Concurrency is jobs processed in parallel per pod, bounded so memory
	// (each job buffers audio) stays predictable. Default 4.
	Concurrency int `envconfig:"WORKER_CONCURRENCY" default:"4"`
	// TranscribeJobTimeout bounds a single transcribe+summarize job. Whisper on
	// CPU runs well below realtime, so a multi-hour recording can take tens of
	// minutes; the default is generous so long sessions complete rather than
	// being cancelled mid-transcription. Other job types use JobTimeout.
	TranscribeJobTimeout time.Duration `envconfig:"WORKER_TRANSCRIBE_JOB_TIMEOUT" default:"4h"`
	// JobTimeout bounds non-transcribe jobs (art, reindex).
	JobTimeout time.Duration `envconfig:"WORKER_JOB_TIMEOUT" default:"15m"`
	// MaxRetries is how many times a job is requeued after a transient failure
	// before it is abandoned. Attempt 0 is the first try, so with MaxRetries=3 a
	// job runs at most 4 times total. Permanent failures (bad input, no audio)
	// are never retried regardless of this setting. 0 disables retries entirely
	// (a failed job is dropped immediately, the pre-retry behavior).
	MaxRetries int `envconfig:"WORKER_MAX_RETRIES" default:"3"`
}

// Load reads configuration from the environment. It returns an error only for
// malformed values; missing optional values fall back to defaults.
func Load() (*Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	// Normalize the Discord bot token. Secrets frequently arrive with a
	// trailing newline (e.g. from `echo`/`kubectl create secret`) or with an
	// accidental "Bot " prefix; both make Discord reject the identify with
	// close code 4004 ("Authentication failed") even though the raw token is
	// valid. The session code adds the "Bot " scheme itself, so strip it here.
	c.Discord.Token = strings.TrimSpace(c.Discord.Token)
	c.Discord.Token = strings.TrimSpace(strings.TrimPrefix(c.Discord.Token, "Bot "))
	return &c, nil
}
