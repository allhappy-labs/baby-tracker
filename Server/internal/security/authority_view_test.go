package security

import (
	"context"
	"errors"
	"testing"
)

func TestAuthorityViewExpiresWithLease(t *testing.T) {
	h, _, _ := cacheFixture(t)
	var held AuthorityView
	var snapshot AuthoritySnapshot
	if err := h.Service.WithAuthorityView(t.Context(), func(v AuthorityView) error {
		held = v
		if _, ok := v.Account("10000000-0000-4000-8000-000000000001"); !ok {
			t.Error("live account missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := held.Account("10000000-0000-4000-8000-000000000001"); ok {
		t.Fatal("expired account usable")
	}
	if _, err := held.ResolveIdentityAuthority(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expired error=%v", err)
	}
	if err := h.Service.WithAuthoritySnapshot(t.Context(), func(s AuthoritySnapshot) error { snapshot = s; return nil }); err != nil {
		t.Fatal(err)
	}
	if snapshot.valid() {
		t.Fatal("expired snapshot valid")
	}
	if _, err := snapshot.CommandHash(PreparedCommand{}); err == nil {
		t.Fatal("expired preparation accepted")
	}
}
func TestAuthorityViewDetachedResults(t *testing.T) {
	state := NewState()
	state.accounts["account"] = accountState{IdentityKey: "hmac-sha256:key:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "active"}
	state.memberships["family/account"] = membershipState{AccountID: "account", FamilyID: "family", Role: "owner"}
	state.commandReceipts["account/command"] = receipt{familyIDs: []string{"family"}, Result: CommandResult{Code: "ok"}}
	owner := &validatedAuthority{state: state, head: state.Head(), keys: state.ReferencedCredentialKeyIDs()}
	lease := &fenceLease{}
	lease.active.Store(true)
	defer lease.invalidate()
	v := AuthorityView{owner: owner, lease: lease}
	keys := v.ReferencedCredentialKeyIDs()
	keys.Identity[0] = "changed"
	members := v.Memberships("family")
	members[0].Role = "changed"
	receipt, _ := v.Receipt("account", "command")
	receipt.FamilyIDs[0] = "changed"
	receipt.Result.Code = "changed"
	again, _ := v.Receipt("account", "command")
	if v.ReferencedCredentialKeyIDs().Identity[0] != "key" || v.Memberships("family")[0].Role != "owner" || again.FamilyIDs[0] != "family" || again.Result.Code != "ok" {
		t.Fatal("selected result aliases immutable state")
	}
	// Every value selector must fail closed once the capability's scope ends.
	lease.invalidate()
	if _, ok := v.Account("account"); ok {
		t.Error("account")
	}
	if _, ok := v.Device("x"); ok {
		t.Error("device")
	}
	if _, ok := v.Session("x"); ok {
		t.Error("session")
	}
	if _, ok := v.Family("x"); ok {
		t.Error("family")
	}
	if _, ok := v.FamilySlot("x"); ok {
		t.Error("slot")
	}
	if _, ok := v.Membership("family", "account"); ok {
		t.Error("membership")
	}
	if _, ok := v.Invitation("x"); ok {
		t.Error("invitation")
	}
	if _, ok := v.Entitlement("x", "x"); ok {
		t.Error("entitlement")
	}
	if _, ok := v.RecoveryClaim("x", "x"); ok {
		t.Error("claim")
	}
	if _, ok := v.Receipt("account", "command"); ok {
		t.Error("receipt")
	}
	if len(v.Memberships("family")) != 0 || len(v.ReferencedCredentialKeyIDs().Identity) != 0 {
		t.Error("expired slices")
	}
}

func TestNestedAuthorityReadReusesOnlyCurrentLease(t *testing.T) {
	h, l, _ := cacheFixture(t)
	ctx, lease := h.Service.withFenceLease(context.Background())
	release, err := h.Service.fence.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Service.WithAuthorityView(ctx, func(v AuthorityView) error {
		return h.Service.WithAuthorityView(ctx, func(second AuthorityView) error {
			if v.owner != second.owner {
				t.Error("nested read cloned state")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if l.passes != 1 {
		t.Fatalf("passes=%d", l.passes)
	}
	lease.invalidate()
	release()
	done := make(chan error, 1)
	go func() { done <- h.Service.WithAuthorityView(ctx, func(AuthorityView) error { return nil }) }()
	if err := <-done; err == nil {
		t.Fatal("late borrowed context accepted")
	}
	readView(t, h)
	if l.passes != 2 {
		t.Fatalf("new lease passes=%d", l.passes)
	}
}

func TestAuthorityViewWarmReadDoesNotCloneState(t *testing.T) {
	h, _, _ := cacheFixture(t)
	var owner *validatedAuthority
	if err := h.Service.WithAuthorityView(t.Context(), func(v AuthorityView) error { owner = v.owner; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := h.Service.WithAuthorityView(t.Context(), func(v AuthorityView) error {
		if owner != v.owner {
			t.Fatal("unchanged read copied owner/state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestAuthorityViewDetachedValueCannotChangePreparation(t *testing.T) {
	h := newAuthority(t)
	if err := h.Service.WithAuthoritySnapshot(t.Context(), func(snapshot AuthoritySnapshot) error {
		account, _ := snapshot.View.Account(preparedAccount)
		account.Status = "active"
		_ = account
		resolution, err := snapshot.View.ResolveIdentityAuthority([]string{preparedIdentity})
		if err != nil {
			return err
		}
		if resolution.Status != "absent" {
			t.Fatal("detached value mutated owner")
		}
		if _, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1); err != nil {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityViewMetadataExpires(t *testing.T) {
	h, _, _ := cacheFixture(t)
	var retained AuthorityView
	if err := h.Service.WithAuthorityView(t.Context(), func(view AuthorityView) error {
		retained = view
		if view.Head() != h.Head() || view.RecoveryEpoch() != 1 {
			t.Fatal("wrong validated metadata")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, view := range []AuthorityView{retained, {}} {
		if view.Head() != (Head{}) || view.RecoveryEpoch() != 0 {
			t.Fatal("expired metadata escaped")
		}
	}
}
