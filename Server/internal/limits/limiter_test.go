package limits

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/testpg"
)

func TestDefaultPoliciesAreExact(t *testing.T) {
	want := map[string]Policy{
		ScopeLiveActivityAccount:     {Count: 120, Window: time.Minute},
		ScopeLiveActivityDevice:      {Count: 30, Window: time.Minute},
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
	payload, err := prepareCharges(charges, pepper, now)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) == "" || containsAny(string(payload), "192.0.2.9", "AAAAAAAA", "BBBBBBBB") {
		t.Fatalf("payload leaks raw bucket key: %s", payload)
	}
	var got []wireCharge
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Scope != ScopeAuthRefreshAccount || got[1].Scope != ScopeAuthRefreshDevice || got[2].Scope != ScopeAuthRefreshIP {
		t.Fatalf("charges not canonically sorted: %+v", got)
	}
	wantDigest := hmac.New(sha256.New, pepper[:])
	wantDigest.Write([]byte(ScopeAuthRefreshIP + "\x00" + "192.0.2.9"))
	if got[2].KeyDigest != hex.EncodeToString(wantDigest.Sum(nil)) {
		t.Fatalf("digest=%q", got[2].KeyDigest)
	}
	if got[0].WindowStart != "2026-09-20T12:00:00Z" || got[0].WindowEnd != "2026-09-20T12:10:00Z" {
		t.Fatalf("window=%s..%s", got[0].WindowStart, got[0].WindowEnd)
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
	repository := rateRepositoryFunc(func(context.Context, []byte, time.Time) (Decision, error) {
		return Decision{}, errors.New("postgres at secret-host.example: password=hunter2")
	})
	limiter := newLimiter(repository, testPepper(), time.Now)
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

func TestRepositoryEnforcesCapAcrossOneHundredCallers(t *testing.T) {
	fixture := testpg.New(t)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := newLimiter(repository, testPepper(), func() time.Time { return now })
	policy := Policy{Count: 25, Window: time.Minute}
	var allowed atomic.Int32
	var failures atomic.Int32
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			decision, err := limiter.Take(context.Background(), ScopeAuthChallengeIP, "192.0.2.99", policy)
			if err != nil {
				failures.Add(1)
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wait.Wait()
	if failures.Load() != 0 || allowed.Load() != 25 {
		t.Fatalf("allowed=%d failures=%d", allowed.Load(), failures.Load())
	}
}

func TestRepositoryWeightedChargesAreAllOrNone(t *testing.T) {
	fixture := testpg.New(t)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := newLimiter(repository, testPepper(), func() time.Time { return now })
	policy := Policy{Count: 3, Window: time.Minute}
	a := Charge{Scope: ScopeAuthRefreshAccount, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 2, Policy: policy}
	b := Charge{Scope: ScopeAuthRefreshDevice, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Cost: 2, Policy: policy}
	if decision, err := limiter.TakeMany(t.Context(), []Charge{a}); err != nil || !decision.Allowed {
		t.Fatalf("seed decision=%+v err=%v", decision, err)
	}
	if decision, err := limiter.TakeMany(t.Context(), []Charge{a, b}); err != nil || decision.Allowed || decision.RetryAfter <= 0 {
		t.Fatalf("combined decision=%+v err=%v", decision, err)
	}
	if decision, err := limiter.TakeMany(t.Context(), []Charge{b}); err != nil || !decision.Allowed || decision.Remaining != 1 {
		t.Fatalf("rollback decision=%+v err=%v", decision, err)
	}
}

func TestRepositoryFiveChargeChunksCommitOrRollbackAsOneTransaction(t *testing.T) {
	fixture := testpg.New(t)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := newLimiter(repository, testPepper(), func() time.Time { return now })
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
	if decision, err := limiter.TakeMany(t.Context(), denied); err != nil || decision.Allowed || decision.RetryAfter <= 0 {
		t.Fatalf("five-charge denial=%+v err=%v", decision, err)
	}
	for index := 0; index < 3; index++ {
		if decision, err := limiter.TakeMany(t.Context(), []Charge{denied[index]}); err != nil || !decision.Allowed {
			t.Fatalf("first chunk bucket %d was partially consumed: decision=%+v err=%v", index, decision, err)
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

func TestRepositoryLiveScopesUseIsolatedLegacyStorageNamespaces(t *testing.T) {
	fixture := testpg.New(t)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	pepper := testPepper()
	limiter := newLimiter(repository, pepper, func() time.Time { return now })
	policy := Policy{Count: 1, Window: time.Minute}
	charges := []Charge{
		{Scope: ScopeLiveAttemptIP, Key: "192.0.2.99", Cost: 1, Policy: policy},
		{Scope: ScopeLiveAttemptDevice, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 1, Policy: policy},
		{Scope: ScopeLiveAttemptAccount, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Cost: 1, Policy: policy},
		{Scope: ScopeLiveAttemptFamily, Key: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Cost: 1, Policy: policy},
	}
	if decision, err := limiter.TakeMany(t.Context(), charges); err != nil || !decision.Allowed {
		t.Fatalf("live decision=%+v err=%v", decision, err)
	}

	for _, charge := range charges {
		mac := hmac.New(sha256.New, pepper[:])
		_, _ = mac.Write([]byte(charge.Scope + "\x00" + charge.Key))
		var storedScope string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT scope FROM rate_limit_buckets WHERE key_digest=$1`, mac.Sum(nil)).Scan(&storedScope); err != nil {
			t.Fatalf("scope %q was not stored with its logical digest: %v", charge.Scope, err)
		}
		if storedScope == charge.Scope {
			t.Fatalf("scope %q bypassed the frozen storage registry", charge.Scope)
		}
	}

	// The live IP carrier is also a real legacy scope. The logical scope is part
	// of the HMAC input, so the same raw key must still receive independent quota.
	if decision, err := limiter.Take(t.Context(), ScopeAuthChallengeIP, charges[0].Key, policy); err != nil || !decision.Allowed {
		t.Fatalf("legacy carrier collided with live quota: decision=%+v err=%v", decision, err)
	}
	var buckets int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM rate_limit_buckets WHERE scope=$1`, ScopeAuthChallengeIP).Scan(&buckets); err != nil {
		t.Fatal(err)
	}
	if buckets != 2 {
		t.Fatalf("live and legacy isolated buckets=%d want=2", buckets)
	}
}

func TestRepositoryExportScopesUseIsolatedLegacyStorageNamespaces(t *testing.T) {
	fixture := testpg.New(t)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	limiter := newLimiter(repository, testPepper(), func() time.Time { return now })
	policies := DefaultPolicies()
	charges := []Charge{
		{Scope: ScopeExportIP, Key: "192.0.2.99", Cost: 1, Policy: policies[ScopeExportIP]},
		{Scope: ScopeExportAccount, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cost: 1, Policy: policies[ScopeExportAccount]},
		{Scope: ScopeExportDevice, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Cost: 1, Policy: policies[ScopeExportDevice]},
	}
	if decision, err := limiter.TakeMany(t.Context(), charges); err != nil || !decision.Allowed {
		t.Fatalf("export admission=%+v err=%v", decision, err)
	}
	pepper := testPepper()
	for _, charge := range charges {
		mac := hmac.New(sha256.New, pepper[:])
		_, _ = mac.Write([]byte(charge.Scope + "\x00" + charge.Key))
		var storedScope string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT scope FROM rate_limit_buckets WHERE key_digest=$1`, mac.Sum(nil)).Scan(&storedScope); err != nil {
			t.Fatalf("scope %q was not stored: %v", charge.Scope, err)
		}
		if storedScope == charge.Scope {
			t.Fatalf("scope %q bypassed the frozen storage registry", charge.Scope)
		}
	}
}

func TestRepositoryLiveScopesRollBackTogetherOnDenial(t *testing.T) {
	fixture := testpg.New(t)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := newLimiter(repository, testPepper(), func() time.Time { return now })
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

func TestRepositoryLiveScopeEnforcesConcurrentCapAndResetsAtNextWindow(t *testing.T) {
	fixture := testpg.New(t)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := newLimiter(repository, testPepper(), func() time.Time { return now })
	policy := Policy{Count: 25, Window: time.Minute}
	var allowed atomic.Int32
	var failures atomic.Int32
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			decision, takeErr := limiter.Take(context.Background(), ScopeLiveAttemptDevice, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", policy)
			if takeErr != nil {
				failures.Add(1)
			} else if decision.Allowed {
				allowed.Add(1)
			}
		}()
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

func TestRepositoryOversizedWeightedChargeIsDeniedWithoutPartialQuota(t *testing.T) {
	fixture := testpg.New(t)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 3, 4, 0, time.UTC)
	limiter := newLimiter(repository, testPepper(), func() time.Time { return now })
	policy := Policy{Count: 1, Window: time.Minute}
	charges := []Charge{
		{Scope: ScopeSyncMutationAccount, Key: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3", Cost: 1, Policy: policy},
		{Scope: ScopeSyncMutationDevice, Key: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3", Cost: 1, Policy: policy},
		{Scope: ScopeSyncMutationFamily, Key: "cccccccc-cccc-4ccc-8ccc-ccccccccccc3", Cost: 1, Policy: policy},
		{Scope: ScopeSyncPullAccount, Key: "dddddddd-dddd-4ddd-8ddd-ddddddddddd3", Cost: 2, Policy: policy},
		{Scope: ScopeSyncPullDevice, Key: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeee3", Cost: 1, Policy: policy},
	}
	decision, err := limiter.TakeMany(t.Context(), charges)
	if err != nil || decision.Allowed || decision.Remaining != 0 || decision.RetryAfter < time.Second || decision.RetryAfter > time.Minute {
		t.Fatalf("oversized weighted decision=%+v err=%v", decision, err)
	}
	for index, charge := range charges {
		charge.Cost = 1
		if decision, err := limiter.TakeMany(t.Context(), []Charge{charge}); err != nil || !decision.Allowed {
			t.Fatalf("bucket %d was charged by denied admission: decision=%+v err=%v", index, decision, err)
		}
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

type rateRepositoryFunc func(context.Context, []byte, time.Time) (Decision, error)

func (function rateRepositoryFunc) take(ctx context.Context, payload []byte, now time.Time) (Decision, error) {
	return function(ctx, payload, now)
}
