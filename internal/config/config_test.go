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

// TestLiteLLMTokenBudgets guards the completeness knobs: a 0/negative max_tokens
// would be sent verbatim and let the provider pick a tiny default, which is how
// session notes ended up cut off mid-sentence and extraction JSON unparseable.
func TestLiteLLMTokenBudgets(t *testing.T) {
	// Every resolver must fall back to a positive default when unset or negative.
	var zero LiteLLMConfig
	neg := LiteLLMConfig{
		NotesMaxTokens:  -1,
		StateMaxTokens:  -1,
		CriticMaxTokens: -1,
		LoreMaxTokens:   -1,
		AskMaxTokens:    -1,
		RecapMaxTokens:  -1,
		PrepMaxTokens:   -1,
		ChatMaxTokens:   -1,
	}
	defaults := map[string]struct {
		got, want int
	}{
		"Notes":  {zero.NotesTokens(), defaultNotesMaxTokens},
		"State":  {zero.StateTokens(), defaultStateMaxTokens},
		"Critic": {zero.CriticTokens(), defaultCriticMaxTokens},
		"Lore":   {zero.LoreTokens(), defaultLoreMaxTokens},
		"Ask":    {zero.AskTokens(), defaultAskMaxTokens},
		"Recap":  {zero.RecapTokens(), defaultRecapMaxTokens},
		"Prep":   {zero.PrepTokens(), defaultPrepMaxTokens},
		"Chat":   {zero.ChatTokens(), defaultChatMaxTokens},
	}
	for name, tc := range defaults {
		if tc.got != tc.want {
			t.Errorf("%sTokens() unset = %d, want default %d", name, tc.got, tc.want)
		}
		if tc.got <= 0 {
			t.Errorf("%sTokens() default must be positive, got %d", name, tc.got)
		}
	}
	negatives := map[string]struct {
		got, want int
	}{
		"Notes":  {neg.NotesTokens(), defaultNotesMaxTokens},
		"State":  {neg.StateTokens(), defaultStateMaxTokens},
		"Critic": {neg.CriticTokens(), defaultCriticMaxTokens},
		"Lore":   {neg.LoreTokens(), defaultLoreMaxTokens},
		"Ask":    {neg.AskTokens(), defaultAskMaxTokens},
		"Recap":  {neg.RecapTokens(), defaultRecapMaxTokens},
		"Prep":   {neg.PrepTokens(), defaultPrepMaxTokens},
		"Chat":   {neg.ChatTokens(), defaultChatMaxTokens},
	}
	for name, tc := range negatives {
		if tc.got != tc.want {
			t.Errorf("%sTokens() negative = %d, want default %d", name, tc.got, tc.want)
		}
	}

	// A configured positive value must be honoured verbatim.
	set := LiteLLMConfig{
		NotesMaxTokens:  1,
		StateMaxTokens:  2,
		CriticMaxTokens: 3,
		LoreMaxTokens:   4,
		AskMaxTokens:    5,
		RecapMaxTokens:  6,
		PrepMaxTokens:   7,
		ChatMaxTokens:   8,
	}
	overrides := map[string]struct {
		got, want int
	}{
		"Notes":  {set.NotesTokens(), 1},
		"State":  {set.StateTokens(), 2},
		"Critic": {set.CriticTokens(), 3},
		"Lore":   {set.LoreTokens(), 4},
		"Ask":    {set.AskTokens(), 5},
		"Recap":  {set.RecapTokens(), 6},
		"Prep":   {set.PrepTokens(), 7},
		"Chat":   {set.ChatTokens(), 8},
	}
	for name, tc := range overrides {
		if tc.got != tc.want {
			t.Errorf("%sTokens() = %d, want configured %d", name, tc.got, tc.want)
		}
	}

	// The extraction budget must exceed the notes budget: extraction re-states
	// every proposal as JSON with evidence quotes, so it is the larger output.
	if defaultStateMaxTokens <= defaultNotesMaxTokens {
		t.Errorf("state budget (%d) should exceed notes budget (%d)", defaultStateMaxTokens, defaultNotesMaxTokens)
	}
	// /prep renders five sections into an embed, so it needs more room than the
	// single-answer interactive commands.
	if defaultPrepMaxTokens <= defaultChatMaxTokens {
		t.Errorf("prep budget (%d) should exceed the plain chat budget (%d)", defaultPrepMaxTokens, defaultChatMaxTokens)
	}
}
