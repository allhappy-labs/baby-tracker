package config

import (
	"testing"
	"time"

	"babytracker/server/internal/limits"
)

func TestLoadLimitsUsesDocumentedDefaultsAndBoundedOverrides(t *testing.T) {
	values := map[string]string{}
	got, err := LoadLimits(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	policy := got.Policies[limits.ScopeAuthChallengeIP]
	if len(got.Policies) != 31 || policy.Count != 30 || policy.Window != 10*time.Minute {
		t.Fatalf("policies=%d challenge=%+v", len(got.Policies), policy)
	}
	if got.Policies[limits.ScopeLiveActivityAccount] != (limits.Policy{Count: 120, Window: time.Minute}) ||
		got.Policies[limits.ScopeLiveActivityDevice] != (limits.Policy{Count: 30, Window: time.Minute}) {
		t.Fatalf("live activity policies=%+v %+v", got.Policies[limits.ScopeLiveActivityAccount], got.Policies[limits.ScopeLiveActivityDevice])
	}
	if got.Policies[limits.ScopePushTokenDevice] != (limits.Policy{Count: 10, Window: time.Hour}) ||
		got.Policies[limits.ScopePushTokenAccount] != (limits.Policy{Count: 30, Window: time.Hour}) {
		t.Fatalf("push-token policies=%+v %+v", got.Policies[limits.ScopePushTokenDevice], got.Policies[limits.ScopePushTokenAccount])
	}
	if got.Policies[limits.ScopeBearerFailureIP] != (limits.Policy{Count: 60, Window: 10 * time.Minute}) {
		t.Fatalf("failed-bearer policy=%+v", got.Policies[limits.ScopeBearerFailureIP])
	}
	if got.Policies[limits.ScopeSyncMutationDevice] != (limits.Policy{Count: 1200, Window: time.Minute}) ||
		got.Policies[limits.ScopeSyncMutationAccount] != (limits.Policy{Count: 2400, Window: time.Minute}) ||
		got.Policies[limits.ScopeSyncMutationFamily] != (limits.Policy{Count: 6000, Window: time.Minute}) ||
		got.Policies[limits.ScopeSyncPullDevice] != (limits.Policy{Count: 120, Window: time.Minute}) ||
		got.Policies[limits.ScopeSyncPullAccount] != (limits.Policy{Count: 240, Window: time.Minute}) {
		t.Fatalf("sync policies=%+v", got.Policies)
	}
	if got.MaxCaregivers != 10 || got.MaxInvitations != 10 || got.InvitationTTL != 7*24*time.Hour {
		t.Fatalf("family limits=%+v", got)
	}
	values["BT_MAX_CAREGIVERS"] = "12"
	values["BT_MAX_INVITATIONS"] = "15"
	values["BT_INVITATION_TTL"] = "96h"
	values["BT_RATE_LIMIT_AUTH_CHALLENGE_IP_COUNT"] = "12"
	values["BT_RATE_LIMIT_AUTH_CHALLENGE_IP_WINDOW"] = "3m"
	got, err = LoadLimits(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if got.Policies[limits.ScopeAuthChallengeIP] != (limits.Policy{Count: 12, Window: 3 * time.Minute}) {
		t.Fatalf("override=%+v", got.Policies[limits.ScopeAuthChallengeIP])
	}
	if got.MaxCaregivers != 12 || got.MaxInvitations != 15 || got.InvitationTTL != 96*time.Hour {
		t.Fatalf("family overrides=%+v", got)
	}
	for _, bad := range []string{"0", "1000001", "01", "not-an-integer"} {
		values["BT_RATE_LIMIT_AUTH_CHALLENGE_IP_COUNT"] = bad
		if _, err := LoadLimits(func(name string) string { return values[name] }); err == nil {
			t.Fatalf("count %q accepted", bad)
		}
	}
	values["BT_RATE_LIMIT_AUTH_CHALLENGE_IP_COUNT"] = "30"
	for _, bad := range []string{"0s", "1ns", "1001ns", "25h", " 1m", "60"} {
		values["BT_RATE_LIMIT_AUTH_CHALLENGE_IP_WINDOW"] = bad
		if _, err := LoadLimits(func(name string) string { return values[name] }); err == nil {
			t.Fatalf("window %q accepted", bad)
		}
	}
}

func TestLoadLimitsUsesScopeSpecificSyncMaximums(t *testing.T) {
	for name, value := range map[string]string{
		"BT_RATE_LIMIT_SYNC_MUTATION_DEVICE_COUNT": "100001",
		"BT_RATE_LIMIT_SYNC_PULL_DEVICE_COUNT":     "10001",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadLimits(func(key string) string {
				if key == name {
					return value
				}
				return ""
			}); err == nil {
				t.Fatalf("accepted %s=%s", name, value)
			}
		})
	}
}

func TestLoadLimitsKeepsSyncWindowsFixedPerMinute(t *testing.T) {
	for _, name := range []string{
		"BT_RATE_LIMIT_SYNC_MUTATION_DEVICE_WINDOW",
		"BT_RATE_LIMIT_SYNC_PULL_ACCOUNT_WINDOW",
	} {
		if _, err := LoadLimits(func(key string) string {
			if key == name {
				return "2m"
			}
			return ""
		}); err == nil {
			t.Fatalf("accepted %s override", name)
		}
	}
}

func TestLoadLimitsRejectsInvalidFamilyLimits(t *testing.T) {
	tests := map[string]map[string]string{
		"zero caregivers":       {"BT_MAX_CAREGIVERS": "0"},
		"leading zero":          {"BT_MAX_INVITATIONS": "010"},
		"too many caregivers":   {"BT_MAX_CAREGIVERS": "101"},
		"too many invitations":  {"BT_MAX_INVITATIONS": "1001"},
		"too short invitation":  {"BT_INVITATION_TTL": "59m"},
		"too long invitation":   {"BT_INVITATION_TTL": "721h"},
		"fractional invitation": {"BT_INVITATION_TTL": "1h0m0.5s"},
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadLimits(func(key string) string { return values[key] }); err == nil {
				t.Fatal("invalid family limit accepted")
			}
		})
	}
}

// MCP quota overrides may only lower the documented per-minute defaults.
func TestLoadLimitsMCPQuotasAreFixedPerMinuteAndOnlyLowerable(t *testing.T) {
	got, err := LoadLimits(func(string) string { return "" })
	if err != nil || got.Policies[limits.ScopeMCPIntegration] != (limits.Policy{Count: 30, Window: time.Minute}) ||
		got.Policies[limits.ScopeMCPAccount] != (limits.Policy{Count: 60, Window: time.Minute}) ||
		got.Policies[limits.ScopeMCPFamily] != (limits.Policy{Count: 120, Window: time.Minute}) {
		t.Fatalf("mcp defaults=%+v err=%v", got.Policies, err)
	}
	lowered, err := LoadLimits(func(key string) string {
		if key == "BT_RATE_LIMIT_MCP_FAMILY_COUNT" {
			return "5"
		}
		return ""
	})
	if err != nil || lowered.Policies[limits.ScopeMCPFamily].Count != 5 {
		t.Fatalf("lowered=%+v err=%v", lowered.Policies[limits.ScopeMCPFamily], err)
	}
	for name, value := range map[string]string{"BT_RATE_LIMIT_MCP_INTEGRATION_COUNT": "31", "BT_RATE_LIMIT_MCP_ACCOUNT_COUNT": "61",
		"BT_RATE_LIMIT_MCP_FAMILY_COUNT": "121", "BT_RATE_LIMIT_MCP_INTEGRATION_WINDOW": "2m"} {
		if _, err := LoadLimits(func(key string) string {
			if key == name {
				return value
			}
			return ""
		}); err == nil {
			t.Fatalf("accepted %s=%s", name, value)
		}
	}
}
