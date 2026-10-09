package security

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func TestIdentityAuthorityResolutionTracksReplayHistory(t *testing.T) {
	const (
		oldAccount = "10000000-0000-4000-8000-000000000001"
		newAccount = "10000000-0000-4000-8000-000000000002"
		oldKey     = "hmac-sha256:identity.old:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		newKey     = "hmac-sha256:identity.new:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	state := replayIdentityHistory(t,
		identityHistoryEvent{kind: "account_registered", accountID: oldAccount, identityKey: oldKey, status: "active"},
		identityHistoryEvent{kind: "account_deleted", accountID: oldAccount, identityKey: oldKey, status: "deleted"},
		identityHistoryEvent{kind: "account_registered", accountID: newAccount, identityKey: newKey, status: "active"},
	)

	resolution, err := state.ResolveIdentityAuthority([]string{newKey, oldKey})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Status != "active" || resolution.AccountID != newAccount || resolution.IdentityKey != newKey ||
		resolution.LifecycleGeneration != 2 || resolution.NextActorGeneration != 3 {
		t.Fatalf("resolution=%+v", resolution)
	}

	deleted := replayIdentityHistory(t,
		identityHistoryEvent{kind: "account_registered", accountID: oldAccount, identityKey: oldKey, status: "active"},
		identityHistoryEvent{kind: "account_deleted", accountID: oldAccount, identityKey: oldKey, status: "deleted"},
	)
	resolution, err = deleted.ResolveIdentityAuthority([]string{oldKey})
	if err != nil || resolution.Status != "deleted" || resolution.LifecycleGeneration != 1 || resolution.NextActorGeneration != 2 {
		t.Fatalf("deleted resolution=%+v err=%v", resolution, err)
	}
}

func TestIdentityAuthorityResolutionFailsClosedOnMultipleLiveActors(t *testing.T) {
	state := NewState()
	state.accounts["10000000-0000-4000-8000-000000000001"] = accountState{IdentityKey: "hmac-sha256:identity.old:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "active", registeredRevision: 1}
	state.accounts["10000000-0000-4000-8000-000000000002"] = accountState{IdentityKey: "hmac-sha256:identity.new:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: "identity_unavailable", registeredRevision: 2}
	if _, err := state.ResolveIdentityAuthority([]string{
		"hmac-sha256:identity.new:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"hmac-sha256:identity.old:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}); Code(err) != "authorization_quarantine" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
}

func TestDeletedIdentityResolvesOnlyASeparateNextGenerationActor(t *testing.T) {
	const (
		oldAccount = "10000000-0000-4000-8000-000000000001"
		newAccount = "10000000-0000-4000-8000-000000000002"
		identity   = "hmac-sha256:identity.old:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	state := replayIdentityHistory(t,
		identityHistoryEvent{kind: "account_registered", accountID: oldAccount, identityKey: identity, status: "active"},
		identityHistoryEvent{kind: "account_deleted", accountID: oldAccount, identityKey: identity, status: "deleted"},
	)
	snapshot := newAuthoritySnapshot(state, state.Head(), 1, 1)
	actor, err := snapshot.ResolveIdentityActor([]string{identity}, newAccount, 2)
	if err != nil || actor.principal.AccountID != newAccount {
		t.Fatalf("actor=%+v err=%v", actor.principal, err)
	}
	if _, err := snapshot.ResolveIdentityActor([]string{identity}, oldAccount, 1); Code(err) != "invalid_command" {
		t.Fatalf("old continuity code=%q err=%v", Code(err), err)
	}
}

func TestReferencedCredentialKeyIDsAreSortedDefensiveCopies(t *testing.T) {
	state := NewState()
	state.accounts["10000000-0000-4000-8000-000000000001"] = accountState{IdentityKey: "hmac-sha256:z-key:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "deleted"}
	state.accounts["10000000-0000-4000-8000-000000000002"] = accountState{IdentityKey: "hmac-sha256:a-key:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: "active"}
	state.devices["40000000-0000-4000-8000-000000000001"] = deviceState{CredentialDigest: "hmac-sha256:device.z:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Status: "active"}
	state.devices["40000000-0000-4000-8000-000000000002"] = deviceState{CredentialDigest: "hmac-sha256:device.a:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Status: "revoked"}

	first := state.ReferencedCredentialKeyIDs()
	if len(first.Identity) != 2 || first.Identity[0] != "a-key" || first.Identity[1] != "z-key" {
		t.Fatalf("identity=%v", first.Identity)
	}
	if len(first.DeviceContinuity) != 1 || first.DeviceContinuity[0] != "device.z" {
		t.Fatalf("device=%v", first.DeviceContinuity)
	}
	first.Identity[0] = "mutated"
	second := state.ReferencedCredentialKeyIDs()
	if second.Identity[0] != "a-key" {
		t.Fatalf("inventory was not defensive: %v", second.Identity)
	}
}

type identityHistoryEvent struct {
	kind                           EventKind
	accountID, identityKey, status string
}

func replayIdentityHistory(t *testing.T, history ...identityHistoryEvent) State {
	t.Helper()
	prior := Head{Generation: 1}
	candidates := make([]Candidate, 0, len(history))
	for index, item := range history {
		commandID := "30000000-0000-4000-8000-" + []string{"000000000001", "000000000002", "000000000003", "000000000004"}[index]
		event := Event{Version: "v1", Generation: 1, Revision: uint64(index + 1), PreviousHash: hex.EncodeToString(prior.EventHash[:]),
			CommandID: commandID, CommandHash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			ActorAccountID: item.accountID, Kind: item.kind, At: time.Date(2026, 9, 20, 12, index, 0, 0, time.UTC).Format(time.RFC3339Nano),
			Effects: []Effect{{Kind: item.kind, AccountID: item.accountID, IdentityKey: item.identityKey, Status: item.status}}}
		encoded, err := EncodeEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		candidate := Candidate{CommandID: commandID, Prior: prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
		candidates = append(candidates, candidate)
		prior = Head{Generation: 1, Revision: event.Revision, EventHash: candidate.EventHash}
	}
	state, err := Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// Retained for package fixtures constructing a standalone preparation scope.
func newAuthoritySnapshot(state State, head Head, recoveryEpoch, id uint64) AuthoritySnapshot {
	lease := &fenceLease{}
	lease.active.Store(true)
	owned := state.Clone()
	owner := &validatedAuthority{state: owned, head: head, binding: AuthorityBinding{Epoch: recoveryEpoch}, keys: owned.ReferencedCredentialKeyIDs()}
	return newLeasedAuthoritySnapshot(owner, lease, id)
}
