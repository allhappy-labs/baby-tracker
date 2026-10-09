package security_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/security"
)

func appendReplayEvent(t *testing.T, prior *security.Head, sequence int, kind security.EventKind, effects []security.Effect) security.Candidate {
	t.Helper()
	commandID := fmt.Sprintf("30000000-0000-4000-8000-%012d", sequence)
	event := security.Event{Version: "v1", Generation: 1, Revision: prior.Revision + 1, PreviousHash: hex.EncodeToString(prior.EventHash[:]), CommandID: commandID,
		CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ActorAccountID: "10000000-0000-4000-8000-000000000001", ActorDeviceID: "", Kind: kind,
		At: time.Date(2026, 9, 20, 12, 0, sequence, 0, time.UTC).Format(time.RFC3339Nano), Effects: effects}
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	candidate := security.Candidate{CommandID: commandID, Prior: *prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
	*prior = security.Head{Generation: 1, Revision: event.Revision, EventHash: candidate.EventHash}
	return candidate
}

func TestCanonicalEventBytesAndBounds(t *testing.T) {
	event := security.Event{
		Version: "v1", Generation: 1, Revision: 1,
		PreviousHash: "0000000000000000000000000000000000000000000000000000000000000000",
		CommandID:    "30000000-0000-4000-8000-000000000001", CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: "10000000-0000-4000-8000-000000000001", ActorDeviceID: "", Kind: "account_registered", At: "2026-09-20T12:00:00Z",
		Effects: []security.Effect{{Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000001", IdentityKey: "opaque", Status: "active"}},
	}
	want := `{"version":"v1","generation":1,"revision":1,"previous_hash":"0000000000000000000000000000000000000000000000000000000000000000","command_id":"30000000-0000-4000-8000-000000000001","command_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","actor_account_id":"10000000-0000-4000-8000-000000000001","actor_device_id":"","kind":"account_registered","at":"2026-09-20T12:00:00Z","effects":[{"kind":"account_registered","account_id":"10000000-0000-4000-8000-000000000001","identity_key":"opaque","status":"active"}]}`
	got, err := security.EncodeEvent(event)
	if err != nil || string(got) != want {
		t.Fatalf("bytes=%s err=%v", got, err)
	}
	event.Generation = 9007199254740992
	if _, err := security.EncodeEvent(event); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("unsafe integer code=%s", security.Code(err))
	}
	event.Generation = 1
	event.Effects[0].IdentityKey = string(bytes.Repeat([]byte("x"), 64*1024))
	if _, err := security.EncodeEvent(event); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("oversize code=%s", security.Code(err))
	}
	for name, identity := range map[string]string{
		"nul":          "opaque\x00identity",
		"invalid utf8": string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			event.Effects[0].IdentityKey = identity
			if _, err := security.EncodeEvent(event); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%s", security.Code(err))
			}
		})
	}
}

func TestCanonicalCommandHashRejectsMalformedUTF8Payload(t *testing.T) {
	principal := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account"}
	payload := append([]byte(`{"version":"v1","account_id":"10000000-0000-4000-8000-000000000001","identity_lookup_digest":"`), 0xff)
	payload = append(payload, []byte(`"}`)...)
	command := security.Command{ID: "30000000-0000-4000-8000-000000000001", Kind: "account_registered", AffectedAccountIDs: []string{principal.AccountID}, Payload: payload}
	if _, err := security.CanonicalCommandHash(principal, command); security.Code(err) != "invalid_command" {
		t.Fatalf("code=%s err=%v", security.Code(err), err)
	}
}

func TestReplayRejectsNoncanonicalEnvelopeBytes(t *testing.T) {
	event := security.Event{
		Version: "v1", Generation: 1, Revision: 1,
		PreviousHash: "0000000000000000000000000000000000000000000000000000000000000000",
		CommandID:    "30000000-0000-4000-8000-000000000001", CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: "10000000-0000-4000-8000-000000000001", Kind: "account_registered", At: "2026-09-20T12:00:00Z",
		Effects: []security.Effect{{Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000001", IdentityKey: "opaque", Status: "active"}},
	}
	canonical, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	noncanonical := append([]byte(" "), canonical...)
	candidate := security.Candidate{CommandID: event.CommandID, Prior: security.Head{Generation: 1}, EventJSON: noncanonical, EventHash: sha256.Sum256(noncanonical)}
	if _, err := security.Replay([]security.Candidate{candidate}); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("code=%s", security.Code(err))
	}
}

func TestReplayRejectsMalformedKnownEffects(t *testing.T) {
	base := security.Event{Version: "v1", Generation: 1, Revision: 1, PreviousHash: strings.Repeat("0", 64), CommandID: "30000000-0000-4000-8000-000000000001",
		CommandHash: strings.Repeat("a", 64), ActorAccountID: "10000000-0000-4000-8000-000000000001", ActorDeviceID: "", Kind: "account_registered", At: "2026-09-20T12:00:00Z",
		Effects: []security.Effect{{Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000001", IdentityKey: "opaque", Status: "active"}}}
	for name, mutate := range map[string]func(*security.Event){
		"missing digest":    func(event *security.Event) { event.Effects[0].IdentityKey = "" },
		"wrong known field": func(event *security.Event) { event.Effects[0].DeviceID = "40000000-0000-4000-8000-000000000001" },
		"invalid status":    func(event *security.Event) { event.Effects[0].Status = "pending" },
		"duplicate effect":  func(event *security.Event) { event.Effects = append(event.Effects, event.Effects[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			event := base
			event.Effects = append([]security.Effect(nil), base.Effects...)
			mutate(&event)
			encoded, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			candidate := security.Candidate{CommandID: event.CommandID, Prior: security.Head{Generation: 1}, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
			if _, err := security.Replay([]security.Candidate{candidate}); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%s", security.Code(err))
			}
		})
	}
}

func TestLifecycleEffectsRequireCompleteAuthorityEvidence(t *testing.T) {
	const (
		owner     = "10000000-0000-4000-8000-000000000001"
		successor = "10000000-0000-4000-8000-000000000002"
		family    = "20000000-0000-4000-8000-000000000001"
	)
	base := security.Event{Version: "v1", Generation: 1, Revision: 1, PreviousHash: strings.Repeat("0", 64),
		CommandID: "30000000-0000-4000-8000-000000000001", CommandHash: strings.Repeat("a", 64),
		ActorAccountID: owner, ActorDeviceID: "", At: "2026-09-20T12:00:00Z"}
	tests := map[string]security.Event{
		"deletion missing previous role": func() security.Event {
			event := base
			event.Kind = "family_deletion_scheduled"
			event.Effects = []security.Effect{{Kind: event.Kind, AccountID: owner, FamilyID: family, Status: "deletion_pending", PurgeAfter: "2026-09-21T12:00:00Z"}}
			return event
		}(),
		"owner recovery missing operator evidence": func() security.Event {
			event := base
			event.Kind = "owner_recovered"
			event.Effects = []security.Effect{
				{Kind: event.Kind, AccountID: owner, FamilyID: family, Role: "caregiver", Status: "active"},
				{Kind: event.Kind, AccountID: successor, FamilyID: family, Role: "owner", Status: "active"},
			}
			return event
		}(),
	}
	for name, event := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := security.EncodeEvent(event); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%s", security.Code(err))
			}
		})
	}
}

func TestLifecycleEffectsBindActorAndAuthoritativeDeletionDeadline(t *testing.T) {
	const (
		owner  = "10000000-0000-4000-8000-000000000001"
		family = "20000000-0000-4000-8000-000000000001"
	)
	base := security.Event{Version: "v1", Generation: 1, Revision: 1, PreviousHash: strings.Repeat("0", 64),
		CommandID: "30000000-0000-4000-8000-000000000001", CommandHash: strings.Repeat("a", 64),
		ActorAccountID: owner, At: "2026-09-20T12:00:00Z", Kind: "family_deletion_scheduled",
		Effects: []security.Effect{{Kind: "family_deletion_scheduled", AccountID: owner, FamilyID: family, Role: "owner", Status: "deletion_pending", PurgeAfter: "2026-10-20T12:00:00Z"}}}
	if _, err := security.EncodeEvent(base); err != nil {
		t.Fatalf("valid lifecycle event: %v", err)
	}
	for name, mutate := range map[string]func(*security.Event){
		"missing effects": func(event *security.Event) { event.Effects = nil },
		"wrong actor":     func(event *security.Event) { event.ActorAccountID = "10000000-0000-4000-8000-000000000002" },
		"wrong deadline":  func(event *security.Event) { event.Effects[0].PurgeAfter = "2026-10-20T12:00:01Z" },
	} {
		t.Run(name, func(t *testing.T) {
			event := base
			event.Effects = append([]security.Effect(nil), base.Effects...)
			mutate(&event)
			if _, err := security.EncodeEvent(event); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%q err=%v", security.Code(err), err)
			}
		})
	}
}

func TestReplayAppliesDeviceAndRecoveryClaimEffects(t *testing.T) {
	const account = "10000000-0000-4000-8000-000000000001"
	const family = "20000000-0000-4000-8000-000000000001"
	const device = "40000000-0000-4000-8000-000000000001"
	prior := security.Head{Generation: 1}
	candidates := []security.Candidate{}
	candidates = append(candidates, appendReplayEvent(t, &prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: account, IdentityKey: "opaque", Status: "active"}}))
	candidates = append(candidates, appendReplayEvent(t, &prior, 2, "device_registered", []security.Effect{{Kind: "device_registered", AccountID: account, DeviceID: device, ServerInstanceID: "50000000-0000-4000-8000-000000000001", CanonicalOrigin: "https://family.example", DeviceCredentialDigest: "opaque-device-digest", Status: "active"}}))
	candidates = append(candidates, appendReplayEvent(t, &prior, 3, "family_created", []security.Effect{{Kind: "family_created", AccountID: account, FamilyID: family, Role: "owner", Status: "active", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}))
	candidates = append(candidates, appendReplayEvent(t, &prior, 4, "family_deletion_scheduled", []security.Effect{{Kind: "family_deletion_scheduled", AccountID: account, FamilyID: family, Role: "owner", Status: "deletion_pending", PurgeAfter: "2026-10-20T12:00:04Z"}}))
	candidates = append(candidates, appendReplayEvent(t, &prior, 5, "recovery_forfeited", []security.Effect{{Kind: "recovery_forfeited", AccountID: account, FamilyID: family, Status: "forfeited"}}))
	state, err := security.Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := state.Device(device); !ok || got.Status != "active" || got.AccountID != account {
		t.Fatalf("device=%+v present=%v", got, ok)
	}
	if got, ok := state.RecoveryClaim(family, account); !ok || got.Status != "forfeited" {
		t.Fatalf("claim=%+v present=%v", got, ok)
	}
	if _, ok := state.FamilySlot(account); ok {
		t.Fatal("deleted family slot remained active")
	}
	if got, ok := state.Membership(family, account); !ok || got.Status != "former" {
		t.Fatalf("membership=%+v present=%v", got, ok)
	}
}

func TestRecoveryReservationBlocksNewFamilyUntilReleased(t *testing.T) {
	const (
		ownerA     = "10000000-0000-4000-8000-000000000001"
		ownerB     = "10000000-0000-4000-8000-000000000002"
		familyA    = "20000000-0000-4000-8000-000000000001"
		familyB    = "20000000-0000-4000-8000-000000000002"
		familyC    = "20000000-0000-4000-8000-000000000003"
		invitation = "60000000-0000-4000-8000-000000000001"
	)
	prior := security.Head{Generation: 1}
	candidates := []security.Candidate{
		appendReplayEvent(t, &prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: ownerA, IdentityKey: "opaque-a", Status: "active"}}),
		appendReplayEvent(t, &prior, 2, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: ownerB, IdentityKey: "opaque-b", Status: "active"}}),
		appendReplayEvent(t, &prior, 3, "family_created", []security.Effect{{Kind: "family_created", AccountID: ownerA, FamilyID: familyA, Role: "owner", Status: "active", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}),
		appendReplayEvent(t, &prior, 4, "family_created", []security.Effect{{Kind: "family_created", AccountID: ownerB, FamilyID: familyB, Role: "owner", Status: "active", ProjectionMaterialDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}}),
		appendReplayEvent(t, &prior, 5, "invitation_created", []security.Effect{{Kind: "invitation_created", AccountID: ownerB, FamilyID: familyB, InvitationID: invitation, TokenDigest: "opaque-token", Role: "caregiver", Status: "active", ExpiresAt: "2026-09-21T12:00:00Z"}}),
		appendReplayEvent(t, &prior, 6, "family_deletion_scheduled", []security.Effect{{Kind: "family_deletion_scheduled", AccountID: ownerA, FamilyID: familyA, Role: "owner", Status: "deletion_pending", PurgeAfter: "2026-10-20T12:00:06Z"}}),
	}
	state, err := security.Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if slot, ok := state.FamilySlot(ownerA); !ok || slot.FamilyID != familyA || slot.State != "family_recovery" {
		t.Fatalf("recovery reservation slot=%+v present=%v", slot, ok)
	}

	assertDenied := func(kind security.EventKind, effects []security.Effect) {
		t.Helper()
		attemptPrior := prior
		attempt := appendReplayEvent(t, &attemptPrior, 7, kind, effects)
		if _, err := security.Replay(append(append([]security.Candidate{}, candidates...), attempt)); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("%s code=%s err=%v", kind, security.Code(err), err)
		}
	}
	assertDenied("family_created", []security.Effect{{Kind: "family_created", AccountID: ownerA, FamilyID: familyC, Role: "owner", Status: "active", ProjectionMaterialDigest: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}})
	assertDenied("invitation_redeemed", []security.Effect{{Kind: "invitation_redeemed", AccountID: ownerA, FamilyID: familyB, InvitationID: invitation, TokenDigest: "opaque-token", Role: "caregiver", Status: "redeemed", ExpiresAt: "2026-09-21T12:00:00Z"}})

	candidates = append(candidates, appendReplayEvent(t, &prior, 8, "recovery_forfeited", []security.Effect{{Kind: "recovery_forfeited", AccountID: ownerA, FamilyID: familyA, Status: "forfeited"}}))
	for name, effects := range map[string][]security.Effect{
		"create": {{Kind: "family_created", AccountID: ownerA, FamilyID: familyC, Role: "owner", Status: "active", ProjectionMaterialDigest: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}},
		"join":   {{Kind: "invitation_redeemed", AccountID: ownerA, FamilyID: familyB, InvitationID: invitation, TokenDigest: "opaque-token", Role: "caregiver", Status: "redeemed", ExpiresAt: "2026-09-21T12:00:00Z"}},
	} {
		t.Run(name+" after forfeiture", func(t *testing.T) {
			branchPrior := prior
			kind := security.EventKind("family_created")
			if name == "join" {
				kind = "invitation_redeemed"
			}
			branch := appendReplayEvent(t, &branchPrior, 9, kind, effects)
			branchState, err := security.Replay(append(append([]security.Candidate{}, candidates...), branch))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := branchState.FamilySlot(ownerA); !ok {
				t.Fatal("released recovery reservation was not claimable")
			}
		})
	}
}

func TestReservedPurgeEventsNeverRestoreRecovery(t *testing.T) {
	const (
		actor  = "10000000-0000-4000-8000-000000000001"
		family = "20000000-0000-4000-8000-000000000001"
	)
	makeEvent := func(prior security.Head, commandID string, kind security.EventKind, at time.Time, effect security.Effect) security.Candidate {
		t.Helper()
		eventActor := actor
		if kind == "family_purge_started" || kind == "family_purge_completed" {
			var err error
			eventActor, err = security.RetentionWorkerActorID(effect.ServerInstanceID)
			if err != nil {
				t.Fatal(err)
			}
		}
		event := security.Event{
			Version: "v1", Generation: 1, Revision: prior.Revision + 1,
			PreviousHash: hex.EncodeToString(prior.EventHash[:]), CommandID: commandID,
			CommandHash:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ActorAccountID: eventActor, ActorDeviceID: "", Kind: kind, At: at.Format(time.RFC3339Nano),
			Effects: []security.Effect{effect},
		}
		encoded, err := security.EncodeEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		candidate := security.Candidate{CommandID: commandID, Prior: prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
		return candidate
	}
	prior := security.Head{Generation: 1}
	appendEvent := func(commandID string, kind security.EventKind, at time.Time, effect security.Effect) security.Candidate {
		candidate := makeEvent(prior, commandID, kind, at, effect)
		prior = security.Head{Generation: 1, Revision: prior.Revision + 1, EventHash: candidate.EventHash}
		return candidate
	}
	candidates := []security.Candidate{}
	candidates = append(candidates, appendEvent("30000000-0000-4000-8000-000000000001", "account_registered", time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC), security.Effect{Kind: "account_registered", AccountID: actor, IdentityKey: "opaque", Status: "active"}))
	candidates = append(candidates, appendEvent("30000000-0000-4000-8000-000000000002", "family_created", time.Date(2026, 9, 20, 10, 1, 0, 0, time.UTC), security.Effect{Kind: "family_created", AccountID: actor, FamilyID: family, Role: "owner", Status: "active", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}))
	candidates = append(candidates, appendEvent("30000000-0000-4000-8000-000000000003", "family_deletion_scheduled", time.Date(2026, 9, 20, 10, 2, 0, 0, time.UTC), security.Effect{Kind: "family_deletion_scheduled", FamilyID: family, AccountID: actor, Role: "owner", Status: "deletion_pending", PurgeAfter: "2026-10-20T10:02:00Z"}))
	startRevision := uint64(4)
	const purgeInstance = "50000000-0000-4000-8000-000000000001"
	candidates = append(candidates, appendEvent("30000000-0000-4000-8000-000000000004", "family_purge_started", time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), security.Effect{Kind: "family_purge_started", FamilyID: family, ServerInstanceID: purgeInstance, Status: "purge_started", PurgeAfter: "2026-10-20T10:02:00Z", PurgeStartCommandID: "30000000-0000-4000-8000-000000000004", PurgeStartRevision: &startRevision}))
	recovery := security.Effect{Kind: "family_recovered", AccountID: actor, FamilyID: family, Role: "owner", Status: "active"}
	if _, err := security.Replay(append(append([]security.Candidate{}, candidates...), makeEvent(prior, "30000000-0000-4000-8000-000000000005", "family_recovered", time.Date(2026, 10, 20, 12, 1, 0, 0, time.UTC), recovery))); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("recovery after start code=%s", security.Code(err))
	}
	candidates = append(candidates, appendEvent("30000000-0000-4000-8000-000000000006", "family_purge_completed", time.Date(2026, 10, 20, 12, 2, 0, 0, time.UTC), security.Effect{Kind: "family_purge_completed", FamilyID: family, ServerInstanceID: purgeInstance, Status: "purge_completed", PhysicalPurgeReceipt: "80000000-0000-4000-8000-000000000001", PurgedAt: "2026-10-20T12:02:00Z", ExpectedStartRevision: &startRevision}))
	if _, err := security.Replay(append(append([]security.Candidate{}, candidates...), makeEvent(prior, "30000000-0000-4000-8000-000000000007", "family_recovered", time.Date(2026, 10, 20, 12, 3, 0, 0, time.UTC), recovery))); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("recovery after completion code=%s", security.Code(err))
	}
	for range 2 {
		state, err := security.Replay(candidates)
		if err != nil {
			t.Fatal(err)
		}
		familyState, ok := state.Family(family)
		if !ok || familyState.Status != "purge_completed" || familyState.PurgeStartCommandID != "30000000-0000-4000-8000-000000000004" ||
			familyState.PurgeStartRevision != 4 || familyState.PurgeServerInstanceID != purgeInstance || familyState.PhysicalPurgeReceipt != "80000000-0000-4000-8000-000000000001" || familyState.PurgedAt != "2026-10-20T12:02:00Z" {
			t.Fatalf("family state = %+v present=%v", familyState, ok)
		}
		if _, ok := state.FamilySlot(actor); ok {
			t.Fatal("terminal purge retained owner recovery reservation")
		}
	}
}

func TestEventDecoderRejectsUnknownAndDuplicateFields(t *testing.T) {
	base := []byte(`{"version":"v1","generation":1,"revision":1,"previous_hash":"0000000000000000000000000000000000000000000000000000000000000000","command_id":"30000000-0000-4000-8000-000000000001","command_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","actor_account_id":"10000000-0000-4000-8000-000000000001","actor_device_id":"","kind":"account_registered","at":"2026-09-20T12:00:00Z","effects":[{"kind":"account_registered","account_id":"10000000-0000-4000-8000-000000000001","identity_key":"opaque","status":"active"}]}`)
	hash := sha256.Sum256(base)
	valid := security.Candidate{CommandID: "30000000-0000-4000-8000-000000000001", Prior: security.Head{Generation: 1}, EventJSON: base, EventHash: hash}
	if _, err := security.Replay([]security.Candidate{valid}); err != nil {
		t.Fatalf("valid fixture: %v", err)
	}
	for name, data := range map[string][]byte{
		"duplicate":         bytes.Replace(base, []byte(`"version":"v1"`), []byte(`"version":"v1","version":"v1"`), 1),
		"unknown":           append(base[:len(base)-1], []byte(`,"future":true}`)...),
		"trailing object":   append(append([]byte{}, base...), []byte(`{}`)...),
		"noncanonical uuid": bytes.Replace(base, []byte(`10000000-0000-4000-8000-000000000001`), []byte(`10000000-0000-4000-8000-00000000000A`), 1),
		"noncanonical time": bytes.Replace(base, []byte(`2026-09-20T12:00:00Z`), []byte(`2026-09-20T12:00:00+00:00`), 1),
		"noncanonical hash": bytes.Replace(base, []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("A", 64)), 1),
		"oversize":          bytes.Replace(base, []byte(`"identity_key":"opaque"`), []byte(`"identity_key":"`+strings.Repeat("x", 70*1024)+`"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.EventJSON = data
			candidate.EventHash = sha256.Sum256(data)
			if _, err := security.Replay([]security.Candidate{candidate}); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%s", security.Code(err))
			}
		})
	}
}
