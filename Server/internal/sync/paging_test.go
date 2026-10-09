package sync_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/family"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
)

type resolverFunc func(context.Context, access.Principal) (string, error)

func (function resolverFunc) Current(ctx context.Context, principal access.Principal) (string, error) {
	return function(ctx, principal)
}

func TestOutcomeCursorDoesNotSkipJournal(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t)
	first := testkit.MustMutation(t, "baby.json")
	second := testkit.MustMutation(t, "baby.json")
	second.MutationID = "01000000-0000-4000-8000-000000000902"
	second.EntityID = "10000000-0000-4000-8000-000000000902"
	secondPayload := second.Payload.(protocol.BabyPayload)
	secondPayload.Name = "Second"
	second.Payload = secondPayload
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, first); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, second); err != nil {
		t.Fatal(err)
	}
	update := first
	update.MutationID = "01000000-0000-4000-8000-000000000903"
	update.Operation = "update"
	update.ObservedEntityVersion = 1
	updatePayload := update.Payload.(protocol.BabyPayload)
	updatePayload.Name = "Updated"
	update.Payload = updatePayload
	response, err := h.Service.ApplyAndPull(t.Context(), h.Owner, protocol.SyncRequest{
		RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}},
		Cursor:      h.Cursor(0), Mutations: []protocol.Mutation{update},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Outcomes) != 1 || response.Outcomes[0].Cursor == nil || response.Outcomes[0].Cursor.Sequence != 3 {
		t.Fatalf("outcomes=%+v", response.Outcomes)
	}
	if len(response.Changes) != 1 || response.Changes[0].Cursor.Sequence != 1 || response.HighWater.Sequence != 3 || response.NextPageToken == nil {
		t.Fatalf("first page=%+v", response)
	}
	secondPage, err := h.Service.ApplyAndPull(t.Context(), h.Owner, protocol.SyncRequest{
		RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}},
		Mutations:   []protocol.Mutation{}, PageToken: response.NextPageToken,
	})
	if err != nil || len(secondPage.Changes) != 1 || secondPage.Changes[0].Cursor.Sequence != 2 || secondPage.HighWater.Sequence != 3 {
		t.Fatalf("second page=%+v err=%v", secondPage, err)
	}
}

func TestCreateOrJoinSessionMustRefreshBeforeSync(t *testing.T) {
	for _, test := range []struct {
		name, subject string
		new           func(*testing.T) *testkit.Harness
		who           func(*testkit.Harness) access.Principal
	}{
		{"owner_create", "owner", testkit.NewWithUnrefreshedOwner, func(h *testkit.Harness) access.Principal { return h.Owner }},
		{"caregiver_join", "caregiver", testkit.NewWithUnrefreshedCaregiver, func(h *testkit.Harness) access.Principal { return h.Caregiver }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := test.new(t)
			h.RebuildSyncService(t)
			principal := test.who(h)
			if principal.Scope != "family" || principal.SessionScope != "account" || principal.SessionFamilyID != "" {
				t.Fatalf("unrefreshed principal=%+v", principal)
			}
			mutation := testkit.MustMutation(t, "baby.json")
			request := h.SyncRequest(nil, nil)
			request.Mutations = []protocol.Mutation{mutation}
			if _, err := h.Service.ApplyAndPull(t.Context(), principal, request); !errors.Is(err, familysync.ErrSessionRefreshRequired) {
				t.Fatalf("unrefreshed sync err=%v", err)
			}
			if _, err := h.Service.FullSnapshot(t.Context(), principal, protocol.SnapshotRequest{}); !errors.Is(err, familysync.ErrSessionRefreshRequired) {
				t.Fatalf("unrefreshed snapshot err=%v", err)
			}
			for _, table := range []string{"babies", "mutation_journal", "mutation_receipts", "sync_snapshots", "sync_snapshot_admissions", "sync_snapshot_entities", "sync_page_sessions"} {
				if got := h.CountAs(t, table, h.FamilyID, principal.AccountID); got != 0 {
					t.Fatalf("%s rows after rejected unrefreshed sync=%d", table, got)
				}
			}
			refreshed := h.Refresh(test.subject)
			if refreshed.SessionScope != "family" || refreshed.SessionFamilyID != h.FamilyID {
				t.Fatalf("refreshed principal=%+v", refreshed)
			}
			if response, err := h.Service.ApplyAndPull(t.Context(), refreshed, request); err != nil || len(response.Outcomes) != 1 || response.Outcomes[0].Status != "applied" {
				t.Fatalf("refreshed response=%+v err=%v", response, err)
			}
		})
	}
}

func TestContinuationAuthenticatesBindingBeforeExpiryAndKindUse(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.Limits.SyncPageTTLSeconds = 60
	h.RebuildSyncService(t)
	h.SeedBabies(t, 2)
	first, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	otherDevice, err := h.NewDevice("owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Service.ApplyAndPull(t.Context(), otherDevice, h.SyncRequest(nil, first.NextPageToken)); !errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("wrong session/device err=%v", err)
	}
	if _, err = h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{PageToken: first.NextPageToken}); !errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("wrong token kind err=%v", err)
	}
	h.Now = h.Now.Add(61 * time.Second)
	if _, err = h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, first.NextPageToken)); !errors.Is(err, familysync.ErrPageExpired) {
		t.Fatalf("expired token err=%v", err)
	}
}

func TestContinuationFamilyResolutionWinsBeforeUntrustedTokenLookup(t *testing.T) {
	h := testkit.New(t)
	h.RebuildSyncService(t)
	service := familysync.NewServiceWithPaging(h.Security, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now },
		resolverFunc(func(context.Context, access.Principal) (string, error) { return "", family.ErrAuthorizationQuarantine }), nil,
		testkit.PageKeyring("page.v1", 0x77))
	token := "forged-and-untrusted"
	_, err := service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, &token))
	if !errors.Is(err, family.ErrAuthorizationQuarantine) || errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("quarantine did not win: %v", err)
	}
	stale := h.Owner
	stale.RecoveryEpoch++
	_, err = h.Service.ApplyAndPull(t.Context(), stale, h.SyncRequest(nil, &token))
	if family.Code(err) != "epoch_changed" || errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("epoch did not win: code=%q err=%v", family.Code(err), err)
	}
}

func TestContinuationRevocationCommittedBeforeSQLFenceDeniesBeforeTokenUse(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t)
	h.SeedBabies(t, 2)
	first, err := h.Service.ApplyAndPull(t.Context(), h.Caregiver, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	before := h.CountAs(t, "sync_page_sessions", h.FamilyID, h.Caregiver.AccountID)
	guard := &preSQLGuard{inner: h.Security, entered: make(chan struct{}), release: make(chan struct{})}
	service := familysync.NewServiceWithPaging(guard, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now },
		harnessResolver{h}, nil, testkit.PageKeyring("page.v1", 0x77))
	done := make(chan error, 1)
	go func() {
		_, err := service.ApplyAndPull(context.Background(), h.Caregiver, h.SyncRequest(nil, first.NextPageToken))
		done <- err
	}()
	<-guard.entered
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	close(guard.release)
	if err := <-done; err == nil || errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("revocation fence err=%v", err)
	}
	if got := h.CountAs(t, "sync_page_sessions", h.FamilyID, h.Caregiver.AccountID); got != before {
		t.Fatalf("revoked continuation changed page rows=%d before=%d", got, before)
	}
}

func TestContinuousWriterDoesNotStarvePull(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t)
	h.SeedBabies(t, 2)
	first, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil || first.HighWater.Sequence != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	h.SeedBabies(t, 1)
	last, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || last.HighWater.Sequence != 2 || len(last.Changes) != 1 || last.Changes[0].Cursor.Sequence != 2 || last.NextPageToken != nil {
		t.Fatalf("last=%+v err=%v", last, err)
	}
}

func TestPageTokenIsBoundDurableAndReplayStable(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t)
	h.SeedBabies(t, 3)
	first, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	page, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || page.NextPageToken == nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	replay, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || replay.NextPageToken == nil || *replay.NextPageToken != *page.NextPageToken || replay.Changes[0].Cursor != page.Changes[0].Cursor {
		t.Fatalf("replay=%+v page=%+v err=%v", replay, page, err)
	}
	h.RebuildSyncService(t)
	restarted, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || restarted.NextPageToken == nil || *restarted.NextPageToken != *page.NextPageToken {
		t.Fatalf("restart=%+v err=%v", restarted, err)
	}
	other, err := h.Service.ApplyAndPull(t.Context(), h.Caregiver, h.SyncRequest(nil, first.NextPageToken))
	if !errors.Is(err, familysync.ErrInvalidPageToken) || other.NextPageToken != nil {
		t.Fatalf("wrong-device response=%+v err=%v", other, err)
	}
	forged := *first.NextPageToken
	if forged[len(forged)-1] == 'A' {
		forged = forged[:len(forged)-1] + "B"
	} else {
		forged = forged[:len(forged)-1] + "A"
	}
	_, err = h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, &forged))
	if !errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("forged err=%v", err)
	}
}

func TestPageReplayAndSuccessorStayStableAcrossRetainedKeyRotationAndRestart(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	oldKeys := testkit.PageKeyring("old-page-token-key-with-a-much-longer-id", 0x71)
	h.RebuildSyncServiceWithPageKeys(t, oldKeys)
	h.SeedBabies(t, 3)
	first, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	page, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || page.NextPageToken == nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}

	h.Limits.SyncPageSize = 100 // persisted policy, not restart config, controls replay
	rotated := testkit.PageKeyring("n", 0x72, oldKeys.Current)
	h.RebuildSyncServiceWithPageKeys(t, rotated)
	replay, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || replay.NextPageToken == nil || *replay.NextPageToken != *page.NextPageToken || !reflect.DeepEqual(replay.Changes, page.Changes) {
		t.Fatalf("rotated replay=%+v original=%+v err=%v", replay, page, err)
	}
	last, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, page.NextPageToken))
	if err != nil || len(last.Changes) != 1 || last.NextPageToken != nil {
		t.Fatalf("retained successor=%+v err=%v", last, err)
	}

	h.RebuildSyncServiceWithPageKeys(t, testkit.PageKeyring("n", 0x72))
	if _, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, first.NextPageToken)); !errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("removed key err=%v", err)
	}
}
