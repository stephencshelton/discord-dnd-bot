package gateway

import (
	"context"
	"testing"
)

func TestAllowsGuild(t *testing.T) {
	gateway := &Gateway{allowedGuilds: map[string]struct{}{"guild-1": {}}}

	if !gateway.allowsGuild("guild-1") {
		t.Fatal("configured guild should be allowed")
	}
	if gateway.allowsGuild("guild-2") {
		t.Fatal("unconfigured guild should be rejected")
	}
	if gateway.allowsGuild("") {
		t.Fatal("guild authorization should reject direct messages")
	}
	if (&Gateway{allowedGuilds: map[string]struct{}{}}).allowsGuild("guild-1") {
		t.Fatal("empty allowlist should reject guilds")
	}

	ctx := context.Background()
	directMessages := &Gateway{
		allowedGuilds: map[string]struct{}{"guild-1": {}, "guild-2": {}},
		isGuildMember: func(guildID, userID string) bool {
			return userID == "user-1" && guildID == "guild-1" || userID == "user-2"
		},
	}
	// store is nil here, so resolution falls back to membership (no preference).
	if guildID, allowed := directMessages.directMessageGuildID(ctx, "user-1"); !allowed || guildID != "guild-1" {
		t.Fatalf("directMessageGuildID(user-1) = %q, %v; want guild-1, true", guildID, allowed)
	}
	if _, allowed := directMessages.directMessageGuildID(ctx, "user-2"); allowed {
		t.Fatal("users in multiple allowlisted guilds should not receive DM access without a selection")
	}
	if _, allowed := directMessages.directMessageGuildID(ctx, "user-3"); allowed {
		t.Fatal("users outside the allowlist should not receive DM access")
	}
}
