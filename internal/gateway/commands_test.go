package gateway

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// fakeRegistrar is a test double for commandRegistrar. It records the guild IDs
// it was called with and returns a per-guild scripted response so tests can
// simulate individual guilds failing (e.g. a 403 "Missing Access").
type fakeRegistrar struct {
	// globalErr, when non-nil, is returned by SetGlobalCommands.
	globalErr error
	// perGuild maps a guildID (string form) to the error it should return; a
	// nil entry (or a missing key) means success.
	perGuild map[string]error
	// calls records every guild ID passed to SetGuildCommands, in order.
	calls []string
}

func (f *fakeRegistrar) SetGlobalCommands(_ snowflake.ID, _ []discord.ApplicationCommandCreate, _ ...rest.RequestOpt) ([]discord.ApplicationCommand, error) {
	if f.globalErr != nil {
		return nil, f.globalErr
	}
	return nil, nil
}

func (f *fakeRegistrar) SetGuildCommands(_ snowflake.ID, guildID snowflake.ID, cmds []discord.ApplicationCommandCreate, _ ...rest.RequestOpt) ([]discord.ApplicationCommand, error) {
	gid := guildID.String()
	f.calls = append(f.calls, gid)
	if err := f.perGuild[gid]; err != nil {
		return nil, err
	}
	// Echo back synthetic commands so the caller can log counts.
	out := make([]discord.ApplicationCommand, len(cmds))
	return out, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testDefs() []discord.ApplicationCommandCreate {
	return []discord.ApplicationCommandCreate{
		discord.SlashCommandCreate{Name: "alpha", Description: "a"},
		discord.SlashCommandCreate{Name: "beta", Description: "b"},
	}
}

// Valid snowflake IDs (numeric) for guilds used across tests.
const (
	g1 = "100000000000000001"
	g2 = "100000000000000002"
)

const appID = snowflake.ID(999999999999999999)

func TestRegisterGuildCommands_AllSucceed(t *testing.T) {
	r := &fakeRegistrar{}
	err := registerGuildCommands(r, testLogger(), appID, []string{g1, g2}, testDefs(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 guild calls, got %d (%v)", len(r.calls), r.calls)
	}
}

func TestRegisterGuildCommands_PartialFailureStillSucceeds(t *testing.T) {
	// g1 fails (simulating HTTP 403 Missing Access) but g2 must still register,
	// and the overall call must NOT return an error so the bot stays up.
	forbidden := errors.New("HTTP 403 Forbidden: Missing Access")
	r := &fakeRegistrar{perGuild: map[string]error{g1: forbidden}}

	err := registerGuildCommands(r, testLogger(), appID, []string{g1, g2}, testDefs(), nil)
	if err != nil {
		t.Fatalf("partial failure should not error, got: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("both guilds should be attempted, got calls %v", r.calls)
	}
}

func TestRegisterGuildCommands_AllFailReturnsError(t *testing.T) {
	forbidden := errors.New("HTTP 403 Forbidden: Missing Access")
	r := &fakeRegistrar{perGuild: map[string]error{g1: forbidden, g2: forbidden}}

	err := registerGuildCommands(r, testLogger(), appID, []string{g1, g2}, testDefs(), nil)
	if err == nil {
		t.Fatal("expected an error when every guild fails")
	}
	if !errors.Is(err, forbidden) {
		t.Fatalf("error should wrap the last guild failure, got: %v", err)
	}
}

func TestRegisterGuildCommands_NoGuildsIsNoError(t *testing.T) {
	r := &fakeRegistrar{}
	err := registerGuildCommands(r, testLogger(), appID, nil, testDefs(), nil)
	if err != nil {
		t.Fatalf("no configured guilds should not error, got: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("no per-guild calls expected, got %v", r.calls)
	}
}

func TestRegisterGuildCommands_GlobalClearFailureAborts(t *testing.T) {
	// If setting global commands fails, that's a systemic problem (bad app
	// ID/token) and should abort before touching any guild.
	r := &fakeRegistrar{globalErr: errors.New("boom")}
	err := registerGuildCommands(r, testLogger(), appID, []string{g1}, testDefs(), nil)
	if err == nil {
		t.Fatal("expected an error when the global set fails")
	}
	if len(r.calls) != 0 {
		t.Fatalf("no guild registration should occur after a global failure, got %v", r.calls)
	}
}
