package config

import (
	"reflect"
	"testing"
)

func TestDiscordConfigAllowedGuildIDs(t *testing.T) {
	tests := []struct {
		name string
		cfg  DiscordConfig
		want []string
	}{
		{
			name: "empty disables guild integrations",
			want: []string{},
		},
		{
			name: "legacy single guild",
			cfg:  DiscordConfig{GuildID: "guild-1"},
			want: []string{"guild-1"},
		},
		{
			name: "multiple guilds are trimmed and deduplicated",
			cfg:  DiscordConfig{GuildIDs: []string{" guild-1 ", "guild-2", "guild-1", ""}},
			want: []string{"guild-1", "guild-2"},
		},
		{
			name: "list takes precedence over legacy setting",
			cfg:  DiscordConfig{GuildID: "legacy", GuildIDs: []string{"guild-1"}},
			want: []string{"guild-1"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.cfg.AllowedGuildIDs(); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("AllowedGuildIDs() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDatabaseDSN(t *testing.T) {
	t.Run("URL takes precedence when set", func(t *testing.T) {
		c := DatabaseConfig{URL: "postgres://u:p@example:5432/db?sslmode=require", Host: "ignored"}
		if got := c.DSN(); got != "postgres://u:p@example:5432/db?sslmode=require" {
			t.Fatalf("DSN() = %q, want the explicit URL", got)
		}
	})

	t.Run("assembles from parts when URL empty", func(t *testing.T) {
		c := DatabaseConfig{Host: "db-host", Port: 5432, User: "bot", Password: "s3cret", Name: "botdb", SSLMode: "disable"}
		want := "postgres://bot:s3cret@db-host:5432/botdb?sslmode=disable"
		if got := c.DSN(); got != want {
			t.Fatalf("DSN() = %q, want %q", got, want)
		}
	})

	t.Run("escapes special characters in the password", func(t *testing.T) {
		c := DatabaseConfig{Host: "h", Port: 5432, User: "u", Password: "p@ss:w/rd", Name: "d", SSLMode: "disable"}
		got := c.DSN()
		if got != "postgres://u:p%40ss%3Aw%2Frd@h:5432/d?sslmode=disable" {
			t.Fatalf("DSN() = %q, want percent-escaped password", got)
		}
	})
}

func TestLiteLLMModelResolvers(t *testing.T) {
	t.Run("falls back to ChatModel when task models are empty", func(t *testing.T) {
		c := LiteLLMConfig{ChatModel: "dnd-chat"}
		for name, got := range map[string]string{
			"Notes": c.Notes(),
			"Recap": c.Recap(),
			"Lore":  c.Lore(),
			"Ask":   c.Ask(),
			"State": c.State(),
		} {
			if got != "dnd-chat" {
				t.Errorf("%s() = %q, want fallback %q", name, got, "dnd-chat")
			}
		}
	})

	t.Run("task-specific models override the default", func(t *testing.T) {
		c := LiteLLMConfig{
			ChatModel:  "dnd-chat",
			NotesModel: "dnd-notes",
			RecapModel: "dnd-recap",
			LoreModel:  "dnd-lore",
			AskModel:   "dnd-ask",
			StateModel: "dnd-state",
		}
		cases := map[string]struct{ got, want string }{
			"Notes": {c.Notes(), "dnd-notes"},
			"Recap": {c.Recap(), "dnd-recap"},
			"Lore":  {c.Lore(), "dnd-lore"},
			"Ask":   {c.Ask(), "dnd-ask"},
			"State": {c.State(), "dnd-state"},
		}
		for name, tc := range cases {
			if tc.got != tc.want {
				t.Errorf("%s() = %q, want %q", name, tc.got, tc.want)
			}
		}
	})

	t.Run("whitespace-only task models fall back", func(t *testing.T) {
		c := LiteLLMConfig{ChatModel: "dnd-chat", NotesModel: "   "}
		if got := c.Notes(); got != "dnd-chat" {
			t.Errorf("Notes() = %q, want fallback %q", got, "dnd-chat")
		}
	})
}
