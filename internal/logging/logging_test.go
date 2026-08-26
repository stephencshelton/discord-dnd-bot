package logging

import (
	"context"
	"log/slog"
	"testing"
)

func TestNewLevels(t *testing.T) {
	cases := []struct {
		level   string
		enabled slog.Level // a level that MUST be enabled
		blocked slog.Level // a level that MUST be filtered out (or -1 for none)
	}{
		{"debug", slog.LevelDebug, -100},
		{"info", slog.LevelInfo, slog.LevelDebug},
		{"warn", slog.LevelWarn, slog.LevelInfo},
		{"error", slog.LevelError, slog.LevelWarn},
		{"INFO", slog.LevelInfo, slog.LevelDebug},     // case-insensitive
		{"nonsense", slog.LevelInfo, slog.LevelDebug}, // unknown => info
	}
	for _, c := range cases {
		t.Run(c.level, func(t *testing.T) {
			l := New("gateway", c.level)
			if l == nil {
				t.Fatal("New returned nil logger")
			}
			if !l.Enabled(context.Background(), c.enabled) {
				t.Errorf("level %q: expected %v to be enabled", c.level, c.enabled)
			}
			if c.blocked != -100 && l.Enabled(context.Background(), c.blocked) {
				t.Errorf("level %q: expected %v to be filtered out", c.level, c.blocked)
			}
		})
	}
}

func TestNewReturnsUsableLogger(t *testing.T) {
	l := New("worker", "info")
	// Should not panic when logging with attributes.
	l.Info("hello", "k", "v")
}

func TestNewCorrelationID(t *testing.T) {
	a := NewCorrelationID()
	b := NewCorrelationID()
	if len(a) != 16 {
		t.Fatalf("correlation ID length = %d, want 16", len(a))
	}
	if a == b {
		t.Fatal("two correlation IDs should differ")
	}
}

func TestFromContextFallback(t *testing.T) {
	base := New("gateway", "info")
	// No logger in context => fallback returned.
	if got := FromContext(context.Background(), base); got != base {
		t.Fatal("expected fallback logger when none in context")
	}
	// nil context and nil fallback => never nil.
	if got := FromContext(context.TODO(), nil); got == nil { //nolint:staticcheck // deliberately testing nil-safety
		t.Fatal("FromContext must never return nil")
	}
}

func TestWithLoggerRoundTrip(t *testing.T) {
	base := New("gateway", "info")
	child := base.With("command", "roll")
	ctx := WithLogger(context.Background(), child)
	if got := FromContext(ctx, base); got != child {
		t.Fatal("FromContext should return the logger stored by WithLogger")
	}
}

func TestWithCorrelationID(t *testing.T) {
	base := New("gateway", "info")
	ctx := WithCorrelationID(context.Background(), base, "abc123")
	if got := CorrelationIDFromContext(ctx); got != "abc123" {
		t.Fatalf("CorrelationIDFromContext = %q, want abc123", got)
	}
	// A child logger tagged with the correlation ID must be retrievable.
	if FromContext(ctx, base) == base {
		t.Fatal("WithCorrelationID should install a derived child logger")
	}
	// Empty context has no correlation ID.
	if got := CorrelationIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty correlation ID, got %q", got)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "DEBUG": slog.LevelDebug,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn,
		"error": slog.LevelError, "info": slog.LevelInfo,
		"": slog.LevelInfo, "bogus": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
