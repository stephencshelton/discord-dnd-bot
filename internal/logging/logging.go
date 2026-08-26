// Package logging provides a single slog-based logger factory shared by all
// services so log formatting and level handling are consistent, plus helpers
// for propagating request-scoped child loggers and correlation IDs through
// context.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
)

// ParseLevel maps a human level string to an slog.Level. Unknown values
// default to Info. Exposed so callers can reuse the same mapping.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New returns a JSON slog.Logger at the given level, tagged with the service
// name. JSON is used because logs are shipped to a cluster log pipeline.
// Source location is included so call sites are available when troubleshooting.
func New(service, level string) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     ParseLevel(level),
		AddSource: true,
	})
	return slog.New(h).With("service", service)
}

// ctxKey is the private context key type for the request-scoped logger.
type ctxKey struct{}

// correlationIDKey is the private context key for the correlation ID.
type correlationIDKey struct{}

// CorrelationIDField is the standard attribute key used for correlation IDs
// across logs and job payloads, enabling gateway -> queue -> worker tracing.
const CorrelationIDField = "correlation_id"

// NewCorrelationID returns a new random hex correlation ID (16 hex chars / 8
// bytes). Used to tie a gateway interaction to any queued job and its worker
// processing so a single request can be traced end to end.
func NewCorrelationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively impossible; fall back to a marker
		// rather than panicking in a hot path.
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// WithLogger returns a context carrying the given logger, so downstream code
// can retrieve a request-scoped child logger via FromContext.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, log)
}

// FromContext returns the logger stored in ctx, or fallback if none is present.
// fallback may be nil, in which case slog.Default() is returned so callers
// never receive a nil logger.
func FromContext(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	if fallback != nil {
		return fallback
	}
	return slog.Default()
}

// WithCorrelationID stores a correlation ID in ctx and derives a child logger
// tagged with it, so subsequent FromContext lookups carry the ID.
func WithCorrelationID(ctx context.Context, base *slog.Logger, id string) context.Context {
	ctx = context.WithValue(ctx, correlationIDKey{}, id)
	log := FromContext(ctx, base).With(CorrelationIDField, id)
	return WithLogger(ctx, log)
}

// CorrelationIDFromContext returns the correlation ID stored in ctx, or "".
func CorrelationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(correlationIDKey{}).(string); ok {
		return id
	}
	return ""
}
