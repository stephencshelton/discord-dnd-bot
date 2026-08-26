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
