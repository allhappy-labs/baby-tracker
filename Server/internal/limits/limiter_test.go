package limits

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultPoliciesAreExact(t *testing.T) {
	want := map[string]Policy{
		ScopeAuthChallengeIP:         {Count: 30, Window: 10 * time.Minute},
		ScopeAuthExchangeIP:          {Count: 30, Window: 10 * time.Minute},
		ScopeAuthRefreshIP:           {Count: 120, Window: 10 * time.Minute},
		ScopeAuthRefreshAccount:      {Count: 60, Window: 10 * time.Minute},
		ScopeAuthRefreshDevice:       {Count: 30, Window: 10 * time.Minute},
		ScopeFamilyStructuralAccount: {Count: 30, Window: time.Minute},
		ScopeFamilyStructuralDevice:  {Count: 60, Window: time.Minute},
		ScopeInvitationCreateAccount: {Count: 20, Window: time.Hour},
		ScopeInvitationCreateIP:      {Count: 60, Window: time.Hour},
		ScopeInvitationRedeemAccount: {Count: 20, Window: time.Hour},
		ScopeInvitationRedeemIP:      {Count: 60, Window: time.Hour},
		ScopeSyncMutationDevice:      {Count: 1200, Window: time.Minute},
		ScopeSyncMutationAccount:     {Count: 2400, Window: time.Minute},
		ScopeSyncMutationFamily:      {Count: 6000, Window: time.Minute},
		ScopeSyncPullDevice:          {Count: 120, Window: time.Minute},
		ScopeSyncPullAccount:         {Count: 240, Window: time.Minute},
		ScopeLiveAttemptIP:           {Count: 60, Window: time.Minute},
		ScopeLiveAttemptDevice:       {Count: 12, Window: time.Minute},
		ScopeLiveAttemptAccount:      {Count: 36, Window: time.Minute},
		ScopeLiveAttemptFamily:       {Count: 120, Window: time.Minute},
		ScopeExportIP:                {Count: 10, Window: time.Hour},
		ScopeExportAccount:           {Count: 60, Window: time.Minute},
		ScopeExportDevice:            {Count: 30, Window: time.Minute},
		ScopePushTokenDevice:         {Count: 10, Window: time.Hour},
		ScopePushTokenAccount:        {Count: 30, Window: time.Hour},
		ScopeBearerFailureIP:         {Count: 60, Window: 10 * time.Minute},
		ScopeMCPIntegration:          {Count: 30, Window: time.Minute},
		ScopeMCPAccount:              {Count: 60, Window: time.Minute},
		ScopeMCPFamily:               {Count: 120, Window: time.Minute},
		ScopeLiveActivityAccount:     {Count: 120, Window: time.Minute},
		ScopeLiveActivityDevice:      {Count: 30, Window: time.Minute},
	}
	got := DefaultPolicies()
	if len(got) != len(want) {
		t.Fatalf("policy count=%d want=%d", len(got), len(want))
	}
	for scope, policy := range want {
		if got[scope] != policy {
			t.Errorf("policy %q=%+v want=%+v", scope, got[scope], policy)
		}
	}
	got[ScopeAuthChallengeIP] = Policy{}
	if DefaultPolicies()[ScopeAuthChallengeIP] != want[ScopeAuthChallengeIP] {
		t.Fatal("DefaultPolicies returned mutable package state")
	}
}

func TestChargePayloadIsKeyedCanonicalAndContainsNoRawKey(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	pepper := testPepper()
	charges := []Charge{
		{Scope: ScopeAuthRefreshDevice, Key: "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", Cost: 2, Policy: Policy{Count: 30, Window: 10 * time.Minute}},
		{Scope: ScopeAuthRefreshIP, Key: "::ffff:192.0.2.9", Cost: 1, Policy: Policy{Count: 120, Window: 10 * time.Minute}},
		{Scope: ScopeAuthRefreshAccount, Key: "BBBBBBBB-BBBB-4BBB-8BBB-BBBBBBBBBBBB", Cost: 1, Policy: Policy{Count: 60, Window: 10 * time.Minute}},
	}
	got, err := prepareCharges(charges, pepper, now)
	if err != nil {
		t.Fatal(err)
	}
	if rendered := fmt.Sprintf("%+v", got); containsAny(strings.ToLower(rendered), "192.0.2.9", "aaaaaaaa", "bbbbbbbb") {
		t.Fatalf("prepared charges leak a raw bucket key: %s", rendered)
	}
	if len(got) != 3 || got[0].scope != ScopeAuthRefreshAccount || got[1].scope != ScopeAuthRefreshDevice || got[2].scope != ScopeAuthRefreshIP {
		t.Fatalf("charges not canonically sorted: %+v", got)
	}
	digest := func(scope, normalized string) [32]byte {
		mac := hmac.New(sha256.New, pepper[:])
		mac.Write([]byte(scope + "\x00" + normalized))
		var sum [32]byte
		copy(sum[:], mac.Sum(nil))
		return sum
	}
	if got[2].digest != digest(ScopeAuthRefreshIP, "192.0.2.9") {
		t.Fatalf("ip digest=%x", got[2].digest)
	}
	if got[1].digest != digest(ScopeAuthRefreshDevice, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa") {
		t.Fatalf("device digest=%x", got[1].digest)
	}
	windowStart, windowEnd := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), time.Date(2026, 9, 20, 12, 10, 0, 0, time.UTC)
	for _, charge := range got {
		if !charge.windowStart.Equal(windowStart) || !charge.windowEnd.Equal(windowEnd) || charge.limit != map[string]int{
			ScopeAuthRefreshAccount: 60, ScopeAuthRefreshDevice: 30, ScopeAuthRefreshIP: 120}[charge.scope] {
			t.Fatalf("charge=%+v", charge)
		}
	}
	if got[1].cost != 2 || got[0].cost != 1 || got[2].cost != 1 {
		t.Fatalf("costs=%d,%d,%d", got[0].cost, got[1].cost, got[2].cost)
	}

	// Same-scope charges sort by digest.
	pair, err := prepareCharges([]Charge{
		{Scope: ScopeAuthRefreshDevice, Key: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Cost: 1, Policy: Policy{Count: 30, Window: 10 * time.Minute}},
		{Scope: ScopeAuthRefreshDevice, Key: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Cost: 1, Policy: Policy{Count: 30, Window: 10 * time.Minute}},
	}, pepper, now)
	if err != nil || bytes.Compare(pair[0].digest[:], pair[1].digest[:]) >= 0 {
		t.Fatalf("same-scope charges not sorted by digest: %x %x err=%v", pair[0].digest, pair[1].digest, err)
	}
}

func TestChargeValidationRejectsUnsafeOrNonCanonicalInput(t *testing.T) {
	valid := Charge{Scope: ScopeAuthChallengeIP, Key: "192.0.2.1", Cost: 1, Policy: Policy{Count: 2, Window: time.Minute}}
	cases := map[string][]Charge{
		"empty":                    nil,
		"too many":                 {valid, {Scope: ScopeAuthExchangeIP, Key: "192.0.2.1", Cost: 1, Policy: valid.Policy}, {Scope: ScopeInvitationCreateIP, Key: "192.0.2.1", Cost: 1, Policy: valid.Policy}, {Scope: ScopeInvitationRedeemIP, Key: "192.0.2.1", Cost: 1, Policy: valid.Policy}, {Scope: ScopeAuthChallengeIP, Key: "192.0.2.2", Cost: 1, Policy: valid.Policy}, {Scope: ScopeAuthRefreshIP, Key: "192.0.2.3", Cost: 1, Policy: valid.Policy}},
		"unknown scope":            {{Scope: "made-up", Key: valid.Key, Cost: 1, Policy: valid.Policy}},
		"empty key":                {{Scope: valid.Scope, Key: "", Cost: 1, Policy: valid.Policy}},
		"zero cost":                {{Scope: valid.Scope, Key: valid.Key, Cost: 0, Policy: valid.Policy}},
		"huge limit":               {{Scope: valid.Scope, Key: valid.Key, Cost: 1, Policy: Policy{Count: 1_000_001, Window: time.Minute}}},
		"mutation scope too broad": {{Scope: ScopeSyncMutationDevice, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 1, Policy: Policy{Count: 100_001, Window: time.Minute}}},
		"pull scope too broad":     {{Scope: ScopeSyncPullDevice, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 1, Policy: Policy{Count: 10_001, Window: time.Minute}}},
		"mutation non-minute":      {{Scope: ScopeSyncMutationDevice, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 1, Policy: Policy{Count: 100, Window: 2 * time.Minute}}},
		"pull non-minute":          {{Scope: ScopeSyncPullDevice, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 1, Policy: Policy{Count: 100, Window: 30 * time.Second}}},
		"zero window":              {{Scope: valid.Scope, Key: valid.Key, Cost: 1, Policy: Policy{Count: 2}}},
		"submicro window":          {{Scope: valid.Scope, Key: valid.Key, Cost: 1, Policy: Policy{Count: 2, Window: time.Nanosecond}}},
		"fractional micro window":  {{Scope: valid.Scope, Key: valid.Key, Cost: 1, Policy: Policy{Count: 2, Window: time.Microsecond + time.Nanosecond}}},
		"long window":              {{Scope: valid.Scope, Key: valid.Key, Cost: 1, Policy: Policy{Count: 2, Window: 24*time.Hour + time.Nanosecond}}},
		"duplicate":                {valid, valid},
		"bad ip":                   {{Scope: valid.Scope, Key: "not-an-ip", Cost: 1, Policy: valid.Policy}},
		"bad account":              {{Scope: ScopeAuthRefreshAccount, Key: "not-a-uuid", Cost: 1, Policy: valid.Policy}},
	}
	for name, charges := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := prepareCharges(charges, testPepper(), time.Now())
			if ErrorCodeOf(err) != CodeInvalidLimit {
				t.Fatalf("code=%q err=%v", ErrorCodeOf(err), err)
			}
		})
	}
}

func TestClientIPResolverTrustsOnlyConfiguredRightToLeftChain(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:1::/48")}
	resolver, err := NewClientIPResolver(trusted)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("untrusted peer ignores spoofed forwarding", func(t *testing.T) {
		header := http.Header{"X-Forwarded-For": {"198.51.100.1"}}
		got, err := resolver.Resolve("192.0.2.44:1234", header)
		if err != nil || got.String() != "192.0.2.44" {
			t.Fatalf("address=%v err=%v", got, err)
		}
	})

	t.Run("trusted peer walks every header value right to left", func(t *testing.T) {
		header := http.Header{"X-Forwarded-For": {"198.51.100.7, 10.1.0.4", "10.2.0.5"}}
		got, err := resolver.Resolve("10.3.0.6:443", header)
		if err != nil || got.String() != "198.51.100.7" {
			t.Fatalf("address=%v err=%v", got, err)
		}
	})

	t.Run("ipv4 mapped address is canonical", func(t *testing.T) {
		got, err := resolver.Resolve("[::ffff:192.0.2.8]:443", nil)
		if err != nil || got.String() != "192.0.2.8" {
			t.Fatalf("address=%v err=%v", got, err)
		}
	})

	for name, header := range map[string]http.Header{
		"malformed":   {"X-Forwarded-For": {"198.51.100.7, nope"}},
		"empty hop":   {"X-Forwarded-For": {"198.51.100.7,,10.1.0.4"}},
		"all trusted": {"X-Forwarded-For": {"10.1.0.4,10.2.0.5"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolver.Resolve("10.3.0.6:443", header); ErrorCodeOf(err) != CodeInvalidForwardedFor {
				t.Fatalf("code=%q err=%v", ErrorCodeOf(err), err)
			}
		})
	}
}

func TestDependencyFailureIsSafeRetryableAndNeverBypasses(t *testing.T) {
	store := rateStoreFunc(func(context.Context, []preparedCharge, time.Time) (Decision, error) {
		return Decision{}, errors.New("postgres at secret-host.example: password=hunter2")
	})
	limiter := newLimiter(store, testPepper(), time.Now)
	decision, err := limiter.Take(t.Context(), ScopeAuthChallengeIP, "192.0.2.1", Policy{Count: 30, Window: 10 * time.Minute})
	if decision.Allowed {
		t.Fatal("dependency failure bypassed rate limiting")
	}
	if ErrorCodeOf(err) != CodeLimitUnavailable || !IsRetryable(err) {
		t.Fatalf("code=%q retryable=%v err=%v", ErrorCodeOf(err), IsRetryable(err), err)
	}
	if containsAny(err.Error(), "secret-host", "hunter2", "password") {
		t.Fatalf("unsafe error=%q", err)
	}
}

// TestRepositoryEnforcesCapAcrossOneHundredCallers, TestRepositoryWeightedChargesAreAllOrNone,
// TestRepositoryOversizedWeightedChargeIsDeniedWithoutPartialQuota and the four
// ...UseIsolatedLegacyStorageNamespaces tests are ported in memory_test.go.

func TestMemoryFiveChargesCommitOrRollbackTogether(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	policy := Policy{Count: 1, Window: time.Minute}
	charges := func(suffix string) []Charge {
		return []Charge{
			{Scope: ScopeSyncMutationAccount, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa" + suffix, Cost: 1, Policy: policy},
			{Scope: ScopeSyncMutationDevice, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb" + suffix, Cost: 1, Policy: policy},
			{Scope: ScopeSyncMutationFamily, Key: "cccccccc-cccc-4ccc-8ccc-ccccccccccc" + suffix, Cost: 1, Policy: policy},
			{Scope: ScopeSyncPullAccount, Key: "dddddddd-dddd-4ddd-8ddd-ddddddddddd" + suffix, Cost: 1, Policy: policy},
			{Scope: ScopeSyncPullDevice, Key: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeee" + suffix, Cost: 1, Policy: policy},
		}
	}
	denied := charges("1")
	if decision, err := limiter.TakeMany(t.Context(), []Charge{denied[3]}); err != nil || !decision.Allowed {
		t.Fatalf("seed decision=%+v err=%v", decision, err)
	}
	if decision, err := limiter.TakeMany(t.Context(), denied); err != nil || decision.Allowed || decision.Remaining != 0 || decision.RetryAfter != 56*time.Second {
		t.Fatalf("five-charge denial=%+v err=%v", decision, err)
	}
	for index, charge := range denied {
		if index == 3 {
			continue
		}
		if decision, err := limiter.TakeMany(t.Context(), []Charge{charge}); err != nil || !decision.Allowed {
			t.Fatalf("bucket %d was partially consumed: decision=%+v err=%v", index, decision, err)
		}
	}

	accepted := charges("2")
	if decision, err := limiter.TakeMany(t.Context(), accepted); err != nil || !decision.Allowed || decision.Remaining != 0 {
		t.Fatalf("five-charge success=%+v err=%v", decision, err)
	}
	for index, charge := range accepted {
		if decision, err := limiter.TakeMany(t.Context(), []Charge{charge}); err != nil || decision.Allowed {
			t.Fatalf("committed bucket %d was not consumed: decision=%+v err=%v", index, decision, err)
		}
	}
}

// MCP quota scopes are fixed one-minute windows whose counts may only be
// lowered: an operator cannot widen the private read surface by configuration.
func TestMCPQuotaScopesAreFixedWindowsThatCanOnlyBeLowered(t *testing.T) {
	for scope, count := range map[string]int{ScopeMCPIntegration: 30, ScopeMCPAccount: 60, ScopeMCPFamily: 120} {
		if !PolicyWindowIsFixed(scope) || PolicyMaximum(scope) != count {
			t.Errorf("%s fixed=%v maximum=%d want fixed and %d", scope, PolicyWindowIsFixed(scope), PolicyMaximum(scope), count)
		}
	}
}

func TestMemoryMCPQuotaRefusalChargesNothing(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore(0, 0)
	limiter := newLimiter(store, testPepper(), func() time.Time { return now })
	policies := DefaultPolicies()
	charges := []Charge{
		{Scope: ScopeMCPIntegration, Key: "60000000-0000-4000-8000-0000000000d1", Cost: 1, Policy: policies[ScopeMCPIntegration]},
		{Scope: ScopeMCPAccount, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 1, Policy: policies[ScopeMCPAccount]},
		{Scope: ScopeMCPFamily, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Cost: 1, Policy: policies[ScopeMCPFamily]},
	}
	for i := 0; i < 30; i++ {
		if decision, err := limiter.TakeMany(t.Context(), charges); err != nil || !decision.Allowed || decision.Remaining != 29-i {
			t.Fatalf("mcp charge %d=%+v err=%v", i, decision, err)
		}
	}
	if decision, err := limiter.TakeMany(t.Context(), charges); err != nil || decision.Allowed || decision.Remaining != 0 || decision.RetryAfter != time.Minute {
		t.Fatalf("31st integration charge=%+v err=%v", decision, err)
	}
	prepared, err := prepareCharges(charges, testPepper(), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, charge := range prepared {
		if count := store.countOf(charge); count != 30 {
			t.Fatalf("scope %q count=%d (all-or-none refusal must not charge)", charge.scope, count)
		}
	}
}

func TestMemoryLiveScopesRollBackTogetherOnDenial(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	policy := Policy{Count: 1, Window: time.Minute}
	device := Charge{Scope: ScopeLiveAttemptDevice, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 1, Policy: policy}
	account := Charge{Scope: ScopeLiveAttemptAccount, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Cost: 1, Policy: policy}
	family := Charge{Scope: ScopeLiveAttemptFamily, Key: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Cost: 1, Policy: policy}
	if decision, err := limiter.TakeMany(t.Context(), []Charge{family}); err != nil || !decision.Allowed {
		t.Fatalf("seed decision=%+v err=%v", decision, err)
	}
	if decision, err := limiter.TakeMany(t.Context(), []Charge{device, account, family}); err != nil || decision.Allowed {
		t.Fatalf("combined decision=%+v err=%v", decision, err)
	}
	for _, charge := range []Charge{device, account} {
		if decision, err := limiter.TakeMany(t.Context(), []Charge{charge}); err != nil || !decision.Allowed {
			t.Fatalf("denied admission consumed %q: decision=%+v err=%v", charge.Scope, decision, err)
		}
	}
}

func TestMemoryLiveScopeEnforcesConcurrentCapAndResetsAtNextWindow(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	policy := Policy{Count: 25, Window: time.Minute}
	var allowed, failures atomic.Int32
	var wait sync.WaitGroup
	for range 100 {
		wait.Go(func() {
			decision, takeErr := limiter.Take(context.Background(), ScopeLiveAttemptDevice, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", policy)
			if takeErr != nil {
				failures.Add(1)
			} else if decision.Allowed {
				allowed.Add(1)
			}
		})
	}
	wait.Wait()
	if failures.Load() != 0 || allowed.Load() != 25 {
		t.Fatalf("allowed=%d failures=%d", allowed.Load(), failures.Load())
	}
	now = now.Add(time.Minute)
	if decision, err := limiter.Take(t.Context(), ScopeLiveAttemptDevice, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", policy); err != nil || !decision.Allowed || decision.Remaining != 24 {
		t.Fatalf("next-window decision=%+v err=%v", decision, err)
	}
}

func TestMemoryFiveChargeOversizedWeightedDenialChargesNothing(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	policy := Policy{Count: 1, Window: time.Minute}
	charges := []Charge{
		{Scope: ScopeSyncMutationAccount, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3", Cost: 1, Policy: policy},
		{Scope: ScopeSyncMutationDevice, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3", Cost: 1, Policy: policy},
		{Scope: ScopeSyncMutationFamily, Key: "cccccccc-cccc-4ccc-8ccc-ccccccccccc3", Cost: 1, Policy: policy},
		{Scope: ScopeSyncPullAccount, Key: "dddddddd-dddd-4ddd-8ddd-ddddddddddd3", Cost: 2, Policy: policy},
		{Scope: ScopeSyncPullDevice, Key: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeee3", Cost: 1, Policy: policy},
	}
	decision, err := limiter.TakeMany(t.Context(), charges)
	if err != nil || decision.Allowed || decision.Remaining != 0 || decision.RetryAfter != 56*time.Second {
		t.Fatalf("oversized weighted decision=%+v err=%v", decision, err)
	}
	for index, charge := range charges {
		charge.Cost = 1
		if decision, err := limiter.TakeMany(t.Context(), []Charge{charge}); err != nil || !decision.Allowed {
			t.Fatalf("bucket %d was charged by denied admission: decision=%+v err=%v", index, decision, err)
		}
	}
}

func TestNewBuildsTheMemoryLimiterAndRefusesUnsafeOptions(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC) }
	for name, options := range map[string]Options{
		"zero pepper":               {Now: now},
		"nil clock":                 {Pepper: testPepper()},
		"negative verified entries": {Pepper: testPepper(), Now: now, MaxVerifiedEntries: -1},
		"negative ip entries":       {Pepper: testPepper(), Now: now, MaxIPEntriesPerScope: -1},
	} {
		if limiter, err := New(options); limiter != nil || ErrorCodeOf(err) != CodeInvalidLimit {
			t.Fatalf("%s: limiter=%v err=%v", name, limiter, err)
		}
	}
	limiter, err := New(Options{Pepper: testPepper(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	store := limiter.(*Service).store.(*memoryStore)
	if store.caps[0] != DefaultMaxVerifiedEntries/memoryShards || store.caps[1] != DefaultMaxIPEntriesPerScope/memoryShards || store.logger != nil {
		t.Fatalf("default caps=%v", store.caps)
	}
	var buffer lockedLog
	logger := slog.New(slog.NewTextHandler(&buffer, nil))
	bounded, err := New(Options{Pepper: testPepper(), Now: now, MaxVerifiedEntries: 128, MaxIPEntriesPerScope: 192, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	store = bounded.(*Service).store.(*memoryStore)
	if store.caps[0] != 2 || store.caps[len(store.caps)-1] != 3 || store.logger != logger {
		t.Fatalf("bounded caps=%v", store.caps)
	}
	if decision, err := limiter.Take(t.Context(), ScopeAuthChallengeIP, "192.0.2.1", Policy{Count: 2, Window: time.Minute}); err != nil || !decision.Allowed || decision.Remaining != 1 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if (&Service{}).Stats().IPEvictions == nil {
		t.Fatal("Stats of a store without counters must still be usable")
	}
}

// Every scope is classified once, by who vouches for its key. IP scopes are
// exactly the "-ip" scopes, whose keys normalizeKey parses as addresses.
func TestEveryScopeHasExactlyOneKeyClass(t *testing.T) {
	if len(scopeKeyClasses) != len(defaultPolicies) {
		t.Fatalf("classified %d scopes, policies %d", len(scopeKeyClasses), len(defaultPolicies))
	}
	for scope := range defaultPolicies {
		class := scopeKeyClasses[scope]
		if class != keyVerified && class != keyIP {
			t.Fatalf("scope %q is not classified", scope)
		}
		if (class == keyIP) != strings.HasSuffix(scope, "-ip") {
			t.Fatalf("scope %q classified %d", scope, class)
		}
		if _, ok := poolOf[scope]; !ok {
			t.Fatalf("scope %q has no pool", scope)
		}
	}
	if _, err := normalizeKey("made-up-ip", "192.0.2.1"); ErrorCodeOf(err) != CodeInvalidLimit {
		t.Fatalf("an unclassified scope was keyed: %v", err)
	}
}

// IPv6 client keys are their /64; IPv4 (and IPv4-mapped IPv6) keys are the
// address itself.
// The push-token scopes are keyed by the authenticated session's device and
// account (verified keys) and their window may be configured.
func TestPushTokenScopesAreVerifiedAndConfigurable(t *testing.T) {
	for _, scope := range []string{ScopePushTokenDevice, ScopePushTokenAccount} {
		if scopeKeyClasses[scope] != keyVerified {
			t.Fatalf("%s is not a verified-key scope", scope)
		}
		if PolicyWindowIsFixed(scope) || PolicyMaximum(scope) != maximumValue {
			t.Fatalf("%s: fixed=%v maximum=%d", scope, PolicyWindowIsFixed(scope), PolicyMaximum(scope))
		}
	}
	if ScopePushTokenDevice != "push-token-device" || ScopePushTokenAccount != "push-token-account" {
		t.Fatalf("scope names %q %q", ScopePushTokenDevice, ScopePushTokenAccount)
	}
}

// The Live Activity registration scopes are keyed by the authenticated
// session's account and device (verified keys), with main's names.
func TestLiveActivityScopesAreVerified(t *testing.T) {
	for _, scope := range []string{ScopeLiveActivityAccount, ScopeLiveActivityDevice} {
		if scopeKeyClasses[scope] != keyVerified {
			t.Fatalf("%s is not a verified-key scope", scope)
		}
	}
	if ScopeLiveActivityAccount != "live-activity-account" || ScopeLiveActivityDevice != "live-activity-device" {
		t.Fatalf("scope names %q %q", ScopeLiveActivityAccount, ScopeLiveActivityDevice)
	}
}

// The failed-bearer scope is keyed by client address (an IP pool of its
// own, evicting within itself) and its policy may be configured.
func TestBearerFailureScopeIsAnIPScope(t *testing.T) {
	if ScopeBearerFailureIP != "bearer-failure-ip" || scopeKeyClasses[ScopeBearerFailureIP] != keyIP {
		t.Fatalf("%q classified %d", ScopeBearerFailureIP, scopeKeyClasses[ScopeBearerFailureIP])
	}
	if pool := poolOf[ScopeBearerFailureIP]; pool == verifiedPool || poolScopes[pool] != ScopeBearerFailureIP {
		t.Fatalf("pool %d", pool)
	}
	if PolicyWindowIsFixed(ScopeBearerFailureIP) || PolicyMaximum(ScopeBearerFailureIP) != maximumValue {
		t.Fatal("the failed-bearer policy is not configurable")
	}
	if got := DefaultPolicies()[ScopeBearerFailureIP]; got != (Policy{Count: 60, Window: 10 * time.Minute}) {
		t.Fatalf("default %+v", got)
	}
}

// Peek answers what TakeMany would, and charges, creates and evicts nothing.
func TestPeekReportsWithoutCharging(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	store := newMemoryStore(0, memoryShards) // one bucket per IP shard
	limiter := newLimiter(store, testPepper(), func() time.Time { return now })
	two := Policy{Count: 2, Window: 10 * time.Minute}
	charge := []Charge{{Scope: ScopeBearerFailureIP, Key: "192.0.2.7", Cost: 1, Policy: two}}
	for range 3 {
		if d, err := limiter.Peek(t.Context(), charge); err != nil || !d.Allowed || d.Remaining != 1 {
			t.Fatalf("peek of an unspent key: %+v %v", d, err)
		}
	}
	if store.len() != 0 {
		t.Fatalf("peek created %d buckets", store.len())
	}
	if d, err := limiter.TakeMany(t.Context(), charge); err != nil || !d.Allowed {
		t.Fatalf("first charge %+v %v", d, err)
	}
	if d, _ := limiter.Peek(t.Context(), charge); !d.Allowed || d.Remaining != 0 {
		t.Fatalf("peek after one charge: %+v", d)
	}
	if d, _ := limiter.TakeMany(t.Context(), charge); !d.Allowed {
		t.Fatal("peeks spent the budget")
	}
	d, err := limiter.Peek(t.Context(), charge)
	if err != nil || d.Allowed || d.RetryAfter < time.Second {
		t.Fatalf("peek of a spent key: %+v %v", d, err)
	}
	// A peek at a full IP pool evicts nothing: the spent key keeps its count.
	for i := range 64 {
		other := []Charge{{Scope: ScopeBearerFailureIP, Key: fmt.Sprintf("198.51.100.%d", i), Cost: 1, Policy: two}}
		if _, err := limiter.Peek(t.Context(), other); err != nil {
			t.Fatal(err)
		}
	}
	if d, _ := limiter.Peek(t.Context(), charge); d.Allowed {
		t.Fatal("peeks evicted a spent key")
	}
	if _, err := limiter.Peek(t.Context(), []Charge{{Scope: ScopeBearerFailureIP, Key: "not-an-address", Cost: 1, Policy: two}}); ErrorCodeOf(err) != CodeInvalidLimit {
		t.Fatalf("peek accepted an invalid key: %v", err)
	}
}

func TestIPKeysAreTheAddressForIPv4AndTheSlash64ForIPv6(t *testing.T) {
	for input, want := range map[string]string{
		"192.0.2.9":                     "192.0.2.9",
		"::ffff:192.0.2.9":              "192.0.2.9",
		"2001:db8:1:2:aaaa:bbbb:cccc:1": "2001:db8:1:2::",
		"2001:DB8:1:2::ffff":            "2001:db8:1:2::",
		"2001:db8:1:3::1":               "2001:db8:1:3::",
	} {
		if got, err := normalizeKey(ScopeAuthChallengeIP, input); err != nil || got != want {
			t.Fatalf("%s keyed %q err=%v, want %q", input, got, err, want)
		}
	}
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := memoryLimiter(t, &now)
	one := Policy{Count: 1, Window: time.Minute}
	if d, err := limiter.Take(t.Context(), ScopeLiveAttemptIP, "2001:db8:1:2::1", one); err != nil || !d.Allowed {
		t.Fatalf("first address %+v %v", d, err)
	}
	if d, _ := limiter.Take(t.Context(), ScopeLiveAttemptIP, "2001:db8:1:2:ffff::9", one); d.Allowed {
		t.Fatal("another address in the same /64 has its own quota")
	}
	if d, _ := limiter.Take(t.Context(), ScopeLiveAttemptIP, "2001:db8:1:3::1", one); !d.Allowed {
		t.Fatal("the next /64 shares the quota")
	}
	if d, _ := limiter.Take(t.Context(), ScopeLiveAttemptIP, "192.0.2.10", one); !d.Allowed {
		t.Fatal("IPv4 address refused")
	}
	if d, _ := limiter.Take(t.Context(), ScopeLiveAttemptIP, "192.0.2.11", one); !d.Allowed {
		t.Fatal("a neighbouring IPv4 address shares the quota")
	}
}

func testPepper() [32]byte {
	var pepper [32]byte
	for index := range pepper {
		pepper[index] = byte(index + 1)
	}
	return pepper
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if len(candidate) <= len(value) {
			for index := 0; index+len(candidate) <= len(value); index++ {
				if value[index:index+len(candidate)] == candidate {
					return true
				}
			}
		}
	}
	return false
}

type rateStoreFunc func(context.Context, []preparedCharge, time.Time) (Decision, error)

func (function rateStoreFunc) take(ctx context.Context, charges []preparedCharge, now time.Time) (Decision, error) {
	return function(ctx, charges, now)
}

func (function rateStoreFunc) peek(ctx context.Context, charges []preparedCharge, now time.Time) (Decision, error) {
	return function(ctx, charges, now)
}
