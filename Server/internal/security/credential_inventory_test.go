package security

import (
	"babytracker/server/internal/access"
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestReceiptInventoryCannotRetireRequiredKeys(t *testing.T) {
	h, _, _ := cacheFixture(t)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	deadline := now.Add(time.Hour)
	h.Service.now = func() time.Time { return now }
	calls := 0
	fail := false
	uncertain := false
	h.Service.recoveryKeyReferences = func(_ context.Context, v AuthorityView) (RecoveryKeyReferencesResult, error) {
		calls++
		if v.Head() != h.Head() || v.RecoveryEpoch() != 1 {
			t.Fatal("invalid provider capability")
		}
		if !h.Service.cacheMu.TryLock() {
			t.Fatal("provider called under global cache lock")
		}
		h.Service.cacheMu.Unlock()
		if fail {
			return RecoveryKeyReferencesResult{}, errors.New("candidate query unavailable")
		}
		keys := CredentialKeyInventory{SessionToken: []string{"parent", "output"}, DeviceContinuity: []string{"receipt"}}
		boundary := deadline
		if !now.Before(deadline) && !uncertain {
			keys = CredentialKeyInventory{}
			boundary = time.Time{}
		}
		return RecoveryKeyReferencesResult{Inventory: keys, Cacheable: !uncertain, RevalidateAt: boundary}, nil
	}
	read := func() CredentialKeyInventory {
		t.Helper()
		k, err := h.Service.ReferencedCredentialKeyIDs(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	keys := read()
	if !reflect.DeepEqual(keys.SessionToken, []string{"output", "parent"}) {
		t.Fatalf("keys=%+v", keys)
	}
	keys.SessionToken[0] = "tamper"
	if read().SessionToken[0] != "output" || calls != 1 {
		t.Fatalf("not detached/cached: calls=%d", calls)
	}
	now = deadline
	fail = true
	if _, err := h.Service.ReferencedCredentialKeyIDs(t.Context()); err == nil {
		t.Fatal("expired cache authorized failed required query")
	}
	if calls != 2 {
		t.Fatalf("queries=%d", calls)
	}
	fail = false
	uncertain = true
	if len(read().SessionToken) != 2 || calls != 3 {
		t.Fatal("uncertainty dropped keys")
	}
	if len(read().SessionToken) != 2 || calls != 4 {
		t.Fatal("uncertainty cached across lease")
	}
	uncertain = false
	if len(read().SessionToken) != 0 || calls != 5 {
		t.Fatal("resolved expiry retained keys")
	}
	read()
	if calls != 5 {
		t.Fatal("resolved cache queried again")
	}
}

func TestReceiptInventoryOncePerLeaseIncludingFailures(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncertain", true: "error"}[fail], func(t *testing.T) {
			h, _, _ := cacheFixture(t)
			calls := 0
			h.Service.recoveryKeyReferences = func(context.Context, AuthorityView) (RecoveryKeyReferencesResult, error) {
				calls++
				if fail {
					return RecoveryKeyReferencesResult{}, ErrUnavailable
				}
				return RecoveryKeyReferencesResult{Inventory: CredentialKeyInventory{SessionToken: []string{"retained"}}}, nil
			}
			for outer := 0; outer < 2; outer++ {
				release, err := h.Service.fence.Read(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				ctx, lease := h.Service.withFenceLease(t.Context())
				for nested := 0; nested < 3; nested++ {
					_, err := h.Service.ReferencedCredentialKeyIDs(ctx)
					if (err != nil) != fail {
						t.Fatalf("err=%v", err)
					}
				}
				lease.invalidate()
				release()
				if calls != outer+1 {
					t.Fatalf("calls=%d", calls)
				}
			}
		})
	}
}

func TestReceiptInventoryNewHeadRequiresFreshUnion(t *testing.T) {
	for _, failure := range []string{"none", "inventory", "projection"} {
		t.Run(failure, func(t *testing.T) {
			h, _, _ := cacheFixture(t)
			prior := h.Head()
			calls := 0
			h.Service.recoveryKeyReferences = func(_ context.Context, v AuthorityView) (RecoveryKeyReferencesResult, error) {
				calls++
				if h.Projection.head != v.Head() {
					t.Fatal("inventory preceded projection commit")
				}
				id := "old"
				if v.Head() != prior {
					if failure == "inventory" {
						return RecoveryKeyReferencesResult{}, ErrUnavailable
					}
					id = "new-receipt"
				}
				return RecoveryKeyReferencesResult{Cacheable: true, Inventory: CredentialKeyInventory{SessionToken: []string{id}}}, nil
			}
			if _, err := h.Service.ReferencedCredentialKeyIDs(t.Context()); err != nil {
				t.Fatal(err)
			}
			h.Projection.failOnce = failure == "projection"
			p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000002", Scope: "account", RecoveryEpoch: 1}
			_, err := h.Service.Execute(t.Context(), p, newVerifiedIdentityRegistration(p.AccountID, "sha256:next-identity", "30000000-0000-4000-8000-000000000002"))
			if (err != nil) != (failure != "none") {
				t.Fatalf("write err=%v", err)
			}
			k, err := h.Service.ReferencedCredentialKeyIDs(t.Context())
			if failure != "none" {
				if err == nil {
					t.Fatalf("failed publication returned old inventory %+v", k)
				}
			} else if err != nil || !reflect.DeepEqual(k.SessionToken, []string{"new-receipt"}) || calls != 2 {
				t.Fatalf("new union=%+v err=%v calls=%d", k, err, calls)
			}
			if failure == "projection" && calls != 1 {
				t.Fatal("queried before successful projection")
			}
		})
	}
}

func TestReceiptInventoryConcurrentColdQueriesCoalesce(t *testing.T) {
	h, _, _ := cacheFixture(t)
	var calls atomic.Int32
	h.Service.recoveryKeyReferences = func(context.Context, AuthorityView) (RecoveryKeyReferencesResult, error) {
		calls.Add(1)
		return RecoveryKeyReferencesResult{Cacheable: true, Inventory: CredentialKeyInventory{SessionToken: []string{"receipt"}}}, nil
	}
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() {
			k, err := h.Service.ReferencedCredentialKeyIDs(t.Context())
			if err == nil && !reflect.DeepEqual(k.SessionToken, []string{"receipt"}) {
				err = errors.New("incomplete inventory")
			}
			results <- err
		}()
	}
	for i := 0; i < 16; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("queries=%d", calls.Load())
	}
}

func TestReceiptInventoryProviderCannotReenterAuthority(t *testing.T) {
	h, _, _ := cacheFixture(t)
	h.Service.recoveryKeyReferences = func(ctx context.Context, v AuthorityView) (RecoveryKeyReferencesResult, error) {
		if _, err := h.Service.ReferencedCredentialKeyIDs(ctx); err == nil {
			t.Fatal("provider reentered authority")
		}
		if got := v.ReferencedCredentialKeyIDs(); len(got.SessionToken) != 0 {
			t.Fatal("provider exposed incomplete recursive inventory")
		}
		return RecoveryKeyReferencesResult{Cacheable: true}, nil
	}
	if _, err := h.Service.ReferencedCredentialKeyIDs(t.Context()); err != nil {
		t.Fatal(err)
	}
}
