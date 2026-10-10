package sync_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// applyAndPull is service.ApplyAndPull as the client decodes it.
func applyAndPull(t *testing.T, service *familysync.Service, p access.Principal, request protocol.SyncRequest) (protocol.SyncResponse, error) {
	t.Helper()
	response, err := service.ApplyAndPull(t.Context(), p, request)
	if err != nil {
		return response, err
	}
	return testkit.Decoded(t, response), nil
}

// fullSnapshot is service.FullSnapshot as the client decodes it.
func fullSnapshot(t *testing.T, service *familysync.Service, p access.Principal, request protocol.SnapshotRequest) (protocol.SnapshotResponse, error) {
	t.Helper()
	response, err := service.FullSnapshot(t.Context(), p, request)
	if err != nil {
		return response, err
	}
	return testkit.DecodedSnapshot(t, response), nil
}

// serviceGuarded is the harness sync service over guard.
func serviceGuarded(h *testkit.Harness, guard authority.RequestGuard) *familysync.Service {
	service := familysync.New(familysync.Options{Guard: guard, Server: h.Info, Limits: h.Limits, Now: func() time.Time { return h.Now },
		PageKeys: testkit.PageKeyring("page.v1", 0x77), Allowed: family.Allowed})
	service.SetImportAdmission(h.Importer)
	return service
}

// refusingGuard refuses every principal transaction with err before it
// begins: run never runs.
type refusingGuard struct {
	authority.RequestGuard
	err error
}

func (g refusingGuard) WithFamilyTx(context.Context, access.Principal, string, func(pgx.Tx, access.Grant) error) error {
	return g.err
}

func TestOutcomeCursorDoesNotSkipChanges(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
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
	response, err := applyAndPull(t, h.Service, h.Owner, protocol.SyncRequest{
		RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}},
		Cursor:      h.Cursor(0), Mutations: []protocol.Mutation{update},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Outcomes) != 1 || response.Outcomes[0].Cursor == nil || response.Outcomes[0].Cursor.Sequence != 3 {
		t.Fatalf("outcomes=%+v", response.Outcomes)
	}
	// Latest-state pull: baby 1 changed at 1 and again at 3, so it is
	// served once, at 3, after baby 2 at 2.
	if len(response.Changes) != 1 || response.Changes[0].Cursor.Sequence != 2 || response.HighWater.Sequence != 3 || response.NextPageToken == nil {
		t.Fatalf("first page=%+v", response)
	}
	secondPage, err := applyAndPull(t, h.Service, h.Owner, protocol.SyncRequest{
		RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}},
		Mutations:   []protocol.Mutation{}, PageToken: response.NextPageToken,
	})
	if err != nil || len(secondPage.Changes) != 1 || secondPage.Changes[0].Cursor.Sequence != 3 || secondPage.Changes[0].EntityID != first.EntityID ||
		secondPage.HighWater.Sequence != 3 || secondPage.NextPageToken != nil {
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
			h.RebuildSyncService(t, h.Limits)
			principal := test.who(h)
			if principal.Scope != "family" || principal.SessionScope != "account" || principal.SessionFamilyID != "" {
				t.Fatalf("unrefreshed principal=%+v", principal)
			}
			mutation := testkit.MustMutation(t, "baby.json")
			request := h.SyncRequest(nil, nil)
			request.Mutations = []protocol.Mutation{mutation}
			if _, err := applyAndPull(t, h.Service, principal, request); !errors.Is(err, familysync.ErrSessionRefreshRequired) {
				t.Fatalf("unrefreshed sync err=%v", err)
			}
			if _, err := fullSnapshot(t, h.Service, principal, protocol.SnapshotRequest{}); !errors.Is(err, familysync.ErrSessionRefreshRequired) {
				t.Fatalf("unrefreshed snapshot err=%v", err)
			}
			for _, table := range []string{"care_entities", "care_receipts", "sync_snapshot_sessions", "sync_page_bindings"} {
				if got := h.CountAs(t, table, h.FamilyID, principal.AccountID); got != 0 {
					t.Fatalf("%s rows after rejected unrefreshed sync=%d", table, got)
				}
			}
			refreshed := h.Refresh(test.subject)
			if refreshed.SessionScope != "family" || refreshed.SessionFamilyID != h.FamilyID {
				t.Fatalf("refreshed principal=%+v", refreshed)
			}
			if response, err := applyAndPull(t, h.Service, refreshed, request); err != nil || len(response.Outcomes) != 1 || response.Outcomes[0].Status != "applied" {
				t.Fatalf("refreshed response=%+v err=%v", response, err)
			}
		})
	}
}

func TestContinuationAuthenticatesBindingBeforeExpiryAndKindUse(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.Limits.SyncPageTTLSeconds = 60
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 2)
	first, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	otherDevice, err := h.NewDevice("owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = applyAndPull(t, h.Service, otherDevice, h.SyncRequest(nil, first.NextPageToken)); !errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("wrong session/device err=%v", err)
	}
	if _, err = fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{PageToken: first.NextPageToken}); !errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("wrong token kind err=%v", err)
	}
	h.Now = h.Now.Add(61 * time.Second)
	if _, err = applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken)); !errors.Is(err, familysync.ErrPageExpired) {
		t.Fatalf("expired token err=%v", err)
	}
}

// The guard's refusal (here a family whose audit chain failed, scope_failed)
// wins before an untrusted page token is ever decoded: the token is read
// only inside the authorized transaction.
func TestContinuationFamilyResolutionWinsBeforeUntrustedTokenLookup(t *testing.T) {
	h := testkit.New(t)
	h.RebuildSyncService(t, h.Limits)
	service := serviceGuarded(h, refusingGuard{RequestGuard: h.Authority, err: authority.ErrScopeFailed})
	token := "forged-and-untrusted"
	_, err := applyAndPull(t, service, h.Owner, h.SyncRequest(nil, &token))
	if !errors.Is(err, authority.ErrScopeFailed) || errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("the guard's refusal did not win: %v", err)
	}
	stale := h.Owner
	stale.RecoveryEpoch++
	_, err = applyAndPull(t, h.Service, stale, h.SyncRequest(nil, &token))
	if family.Code(err) != "epoch_changed" || errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("epoch did not win: code=%q err=%v", family.Code(err), err)
	}
}

func TestContinuationRevocationCommittedBeforeSQLFenceDeniesBeforeTokenUse(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 2)
	first, err := applyAndPull(t, h.Service, h.Caregiver, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	before := h.CountAs(t, "sync_page_bindings", h.FamilyID, h.Caregiver.AccountID)
	guard := &preSQLGuard{RequestGuard: h.Authority, entered: make(chan struct{}), release: make(chan struct{})}
	service := serviceGuarded(h, guard)
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
	if got := h.CountAs(t, "sync_page_bindings", h.FamilyID, h.Caregiver.AccountID); got != before {
		t.Fatalf("revoked continuation changed page rows=%d before=%d", got, before)
	}
}

func TestContinuousWriterDoesNotStarvePull(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 2)
	first, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil || first.HighWater.Sequence != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	h.SeedBabies(t, 1)
	last, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || last.HighWater.Sequence != 2 || len(last.Changes) != 1 || last.Changes[0].Cursor.Sequence != 2 || last.NextPageToken != nil {
		t.Fatalf("last=%+v err=%v", last, err)
	}
}

func TestPageTokenIsBoundDurableAndReplayStable(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 3)
	first, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	page, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || page.NextPageToken == nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	replay, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || replay.NextPageToken == nil || *replay.NextPageToken != *page.NextPageToken || replay.Changes[0].Cursor != page.Changes[0].Cursor {
		t.Fatalf("replay=%+v page=%+v err=%v", replay, page, err)
	}
	h.RebuildSyncService(t, h.Limits)
	restarted, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || restarted.NextPageToken == nil || *restarted.NextPageToken != *page.NextPageToken {
		t.Fatalf("restart=%+v err=%v", restarted, err)
	}
	other, err := applyAndPull(t, h.Service, h.Caregiver, h.SyncRequest(nil, first.NextPageToken))
	if !errors.Is(err, familysync.ErrInvalidPageToken) || other.NextPageToken != nil {
		t.Fatalf("wrong-device response=%+v err=%v", other, err)
	}
	forged := *first.NextPageToken
	if forged[len(forged)-1] == 'A' {
		forged = forged[:len(forged)-1] + "B"
	} else {
		forged = forged[:len(forged)-1] + "A"
	}
	_, err = applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, &forged))
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
	first, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	page, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || page.NextPageToken == nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}

	h.Limits.SyncPageSize = 100 // persisted policy, not restart config, controls replay
	rotated := testkit.PageKeyring("n", 0x72, oldKeys.Current)
	h.RebuildSyncServiceWithPageKeys(t, rotated)
	replay, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || replay.NextPageToken == nil || *replay.NextPageToken != *page.NextPageToken || !reflect.DeepEqual(replay.Changes, page.Changes) {
		t.Fatalf("rotated replay=%+v original=%+v err=%v", replay, page, err)
	}
	last, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, page.NextPageToken))
	if err != nil || len(last.Changes) != 1 || last.NextPageToken != nil {
		t.Fatalf("retained successor=%+v err=%v", last, err)
	}

	h.RebuildSyncServiceWithPageKeys(t, testkit.PageKeyring("n", 0x72))
	if _, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(nil, first.NextPageToken)); !errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("removed key err=%v", err)
	}
}
