package gateway

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// fakeRegistrar is a test double for commandRegistrar. It records the guild IDs
// it was called with and returns a per-guild scripted response so tests can
// simulate individual guilds failing (e.g. a 403 "Missing Access").
type fakeRegistrar struct {
	// globalErr, when non-nil, is returned for the initial global-clear call
	// (guildID == "").
	globalErr error
	// perGuild maps a guildID to the error it should return; a nil entry (or a
	// missing key) means success.
	perGuild map[string]error
	// calls records every (non-global) guildID passed, in order.
	calls []string
}

func (f *fakeRegistrar) ApplicationCommandBulkOverwrite(_, guildID string, cmds []*discordgo.ApplicationCommand, _ ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	if guildID == "" {
		return nil, f.globalErr
	}
	f.calls = append(f.calls, guildID)
	if err := f.perGuild[guildID]; err != nil {
		return nil, err
	}
	// Echo back the defs with synthetic IDs so the caller can collect regIDs.
	out := make([]*discordgo.ApplicationCommand, len(cmds))
	for i, c := range cmds {
		out[i] = &discordgo.ApplicationCommand{ID: guildID + "-cmd-" + c.Name, Name: c.Name}
	}
	return out, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testDefs() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{{Name: "alpha"}, {Name: "beta"}}
}

func TestRegisterGuildCommands_AllSucceed(t *testing.T) {
	r := &fakeRegistrar{}
	ids, err := registerGuildCommands(r, testLogger(), "app", []string{"g1", "g2"}, testDefs())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 guild calls, got %d (%v)", len(r.calls), r.calls)
	}
	// 2 guilds * 2 commands = 4 IDs collected.
	if len(ids) != 4 {
		t.Fatalf("expected 4 registered command IDs, got %d (%v)", len(ids), ids)
	}
}

func TestRegisterGuildCommands_PartialFailureStillSucceeds(t *testing.T) {
	// g1 fails (simulating HTTP 403 Missing Access) but g2 must still register,
	// and the overall call must NOT return an error so the bot stays up.
	forbidden := errors.New("HTTP 403 Forbidden: Missing Access")
	r := &fakeRegistrar{perGuild: map[string]error{"g1": forbidden}}

	ids, err := registerGuildCommands(r, testLogger(), "app", []string{"g1", "g2"}, testDefs())
	if err != nil {
		t.Fatalf("partial failure should not error, got: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("both guilds should be attempted, got calls %v", r.calls)
	}
	// Only g2 succeeded -> 2 command IDs, all prefixed with g2.
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs from the surviving guild, got %d (%v)", len(ids), ids)
	}
	for _, id := range ids {
		if got := id[:2]; got != "g2" {
			t.Fatalf("expected only g2 command IDs, got %q", id)
		}
	}
}

func TestRegisterGuildCommands_AllFailReturnsError(t *testing.T) {
	forbidden := errors.New("HTTP 403 Forbidden: Missing Access")
	r := &fakeRegistrar{perGuild: map[string]error{"g1": forbidden, "g2": forbidden}}

	ids, err := registerGuildCommands(r, testLogger(), "app", []string{"g1", "g2"}, testDefs())
	if err == nil {
		t.Fatal("expected an error when every guild fails")
	}
	if !errors.Is(err, forbidden) {
		t.Fatalf("error should wrap the last guild failure, got: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("no commands should be registered when all guilds fail, got %v", ids)
	}
}

func TestRegisterGuildCommands_NoGuildsIsNoError(t *testing.T) {
	r := &fakeRegistrar{}
	ids, err := registerGuildCommands(r, testLogger(), "app", nil, testDefs())
	if err != nil {
		t.Fatalf("no configured guilds should not error, got: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected no registered IDs, got %v", ids)
	}
	if len(r.calls) != 0 {
		t.Fatalf("no per-guild calls expected, got %v", r.calls)
	}
}

func TestRegisterGuildCommands_GlobalClearFailureAborts(t *testing.T) {
	// If clearing global commands fails, that's a systemic problem (bad app
	// ID/token) and should abort before touching any guild.
	r := &fakeRegistrar{globalErr: errors.New("boom")}
	ids, err := registerGuildCommands(r, testLogger(), "app", []string{"g1"}, testDefs())
	if err == nil {
		t.Fatal("expected an error when the global clear fails")
	}
	if len(r.calls) != 0 {
		t.Fatalf("no guild registration should occur after a global-clear failure, got %v", r.calls)
	}
	if len(ids) != 0 {
		t.Fatalf("expected no IDs, got %v", ids)
	}
}
