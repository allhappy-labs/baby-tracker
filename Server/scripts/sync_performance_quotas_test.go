package scripts

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/limits"
)

// matrixQuotaOverrides is what sync-performance.sh's matrix composition sets
// on its disposable server, as KEY=VALUE lines.
func matrixQuotaOverrides(t *testing.T) map[string]string {
	t.Helper()
	out, err := exec.Command("bash", "-c", `source "$1"; matrix_quota_overrides`, "quotas", "sync-performance.sh").Output()
	if err != nil {
		t.Fatalf("matrix_quota_overrides: %v", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(key, "BT_RATE_LIMIT_") {
			t.Fatalf("not a quota override: %q", line)
		}
		env[key] = value
	}
	return env
}

// The capacity matrix provisions 2,898 phones (2 per family over 9 x 161
// families) through one source address in about an hour, and ages history
// with app renewals. Its disposable server states the quotas that cannot hold
// for that synthetic population; every other quota stays the default, and
// the production defaults are unchanged.
func TestMatrixDisposableQuotasAreExplicitAndValid(t *testing.T) {
	env := matrixQuotaOverrides(t)
	loaded, err := config.LoadLimits(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("the server refuses the matrix quotas: %v", err)
	}
	// Why each scope may be raised on the disposable server only.
	allowed := map[string]string{
		limits.ScopeAuthChallengeIP:    "every phone signs in from the one harness address",
		limits.ScopeAuthExchangeIP:     "every phone signs in from the one harness address",
		limits.ScopeAuthRefreshIP:      "every renewal comes from the one harness address",
		limits.ScopeInvitationCreateIP: "one invitation per family, all from the one address",
		limits.ScopeInvitationRedeemIP: "one redemption per family, all from the one address",
		limits.ScopeLiveAttemptIP:      "256 sockets dial within a 128-family cell from one address",
		limits.ScopeAuthRefreshDevice:  "aging compresses hours of renewals into a cell",
		limits.ScopeAuthRefreshAccount: "aging compresses hours of renewals into a cell",
		limits.ScopeSyncPullDevice:     "one owner token carries a cell's whole pull load",
		limits.ScopeSyncPullAccount:    "one owner token carries a cell's whole pull load",
	}
	defaults := limits.DefaultPolicies()
	for scope, policy := range loaded.Policies {
		if policy == defaults[scope] {
			continue
		}
		if _, ok := allowed[scope]; !ok {
			t.Errorf("%s is changed (%v) without a stated reason", scope, policy)
		}
		if policy.Window != defaults[scope].Window {
			t.Errorf("%s window changed: %v", scope, policy.Window)
		}
	}
	need := map[string]int{ // per window, the whole run's demand at most
		limits.ScopeAuthChallengeIP: 2898, limits.ScopeAuthExchangeIP: 2898,
		limits.ScopeInvitationCreateIP: 1449, limits.ScopeInvitationRedeemIP: 1449,
		limits.ScopeLiveAttemptIP: 2 * 2 * 128,
	}
	for scope, minimum := range need {
		if got := loaded.Policies[scope].Count; got < minimum {
			t.Errorf("%s allows %d, the matrix needs %d", scope, got, minimum)
		}
	}
	if got := limits.DefaultPolicies()[limits.ScopeAuthChallengeIP]; got != (limits.Policy{Count: 30, Window: 10 * time.Minute}) {
		t.Fatalf("production sign-in default changed: %v", got)
	}
}

// The Swift aging loop paces renewals to the quotas the server was given:
// the spacing the runner passes is the refresh window over its count.
func TestMatrixRotationSpacingFollowsTheConfiguredRefreshQuotas(t *testing.T) {
	env := matrixQuotaOverrides(t)
	loaded, err := config.LoadLimits(func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", "-c", `source "$1"; matrix_rotation_spacing`, "spacing", "sync-performance.sh").Output()
	if err != nil {
		t.Fatalf("matrix_rotation_spacing: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		t.Fatalf("want \"<ip seconds> <device seconds>\", got %q", out)
	}
	for i, scope := range []string{limits.ScopeAuthRefreshIP, limits.ScopeAuthRefreshDevice} {
		policy := loaded.Policies[scope]
		got, err := strconv.ParseFloat(fields[i], 64)
		want := policy.Window.Seconds() / float64(policy.Count)
		if err != nil || got < want || got > want*1.01 {
			t.Errorf("%s spacing %s, want %s", scope, fields[i], fmt.Sprint(want))
		}
	}
}
