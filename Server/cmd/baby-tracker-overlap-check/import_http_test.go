package main

import (
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDrainSyncHTTPConsumesEveryFixedHighWaterImportPageBeforeNextPhase(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t)
	repo, err := limits.NewRepository(h.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	rate, err := limits.New(limits.Options{Repository: repo, Pepper: [32]byte{1}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	server := httptest.NewServer(httpapi.New(httpapi.Options{Gate: h.Security, SessionService: h.Auth, FamilyService: h.Family, SyncService: h.Service, ImportService: h.Importer, SyncLimits: &h.Limits, InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, Limiter: rate, SyncPullDevicePolicy: policies[limits.ScopeSyncPullDevice], SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount], SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount], SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily]}))
	defer server.Close()
	directory := t.TempDir()
	owner, caregiver := filepath.Join(directory, "owner.curl"), filepath.Join(directory, "caregiver.curl")
	for subject, path := range map[string]string{"owner": owner, "caregiver": caregiver} {
		if err = os.WriteFile(path, []byte("header = \"Authorization: Bearer "+h.AccessToken(subject)+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	initial := testkit.MustMutation(t, "baby.json")
	initial.MutationID = h.CommandID()
	first, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, initial)
	if err != nil || first.Cursor == nil {
		t.Fatalf("initial=%+v err=%v", first, err)
	}
	cursorPath := filepath.Join(directory, "cursor.json")
	if err = writeOverlapCursor(cursorPath, *first.Cursor); err != nil {
		t.Fatal(err)
	}
	if err = probeImportHTTP(t.Context(), server.URL, owner, caregiver, "prior_at_n_minus_1", func(protocol.Cursor) error { return nil }); err != nil {
		t.Fatal(err)
	}
	pages, changes, err := drainSyncHTTP(t.Context(), server.URL, owner, cursorPath)
	if err != nil || pages < 2 || changes < 2 {
		t.Fatalf("pages=%d changes=%d err=%v", pages, changes, err)
	}
	raw, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatal(err)
	}
	var drained protocol.Cursor
	if json.Unmarshal(raw, &drained) != nil || drained.Sequence != h.FamilySequence(t) {
		t.Fatalf("drained=%+v family_sequence=%d", drained, h.FamilySequence(t))
	}
	next := testkit.MustMutation(t, "baby.json")
	next.MutationID, next.EntityID = h.CommandID(), "10000000-0000-4000-8000-000000009999"
	payload := next.Payload.(protocol.BabyPayload)
	payload.Name = "Next phase"
	next.Payload = payload
	accepted, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, next)
	if err != nil || accepted.Cursor == nil {
		t.Fatalf("next=%+v err=%v", accepted, err)
	}
	pulled := h.Pull(t, h.Owner, &drained, nil)
	if len(pulled.Changes) != 1 || pulled.Changes[0].EntityID != next.EntityID || pulled.HighWater != *accepted.Cursor {
		t.Fatalf("next phase pull=%+v", pulled)
	}
}

func TestImportFeatureProbeUsesDistinctDataAcrossThreeRealHTTPPhases(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t)
	repo, e := limits.NewRepository(h.Runtime)
	if e != nil {
		t.Fatal(e)
	}
	rate, e := limits.New(limits.Options{Repository: repo, Pepper: [32]byte{1}, Now: func() time.Time { return h.Now }})
	if e != nil {
		t.Fatal(e)
	}
	policies := limits.DefaultPolicies()
	server := httptest.NewServer(httpapi.New(httpapi.Options{Gate: h.Security, SessionService: h.Auth, FamilyService: h.Family, SyncService: h.Service, ImportService: h.Importer, SyncLimits: &h.Limits, InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, Limiter: rate, SyncPullDevicePolicy: policies[limits.ScopeSyncPullDevice], SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount], SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount], SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily]}))
	defer server.Close()
	owner, caregiver := filepath.Join(t.TempDir(), "owner.curl"), filepath.Join(t.TempDir(), "caregiver.curl")
	for subject, path := range map[string]string{"owner": owner, "caregiver": caregiver} {
		if e = os.WriteFile(path, []byte("header = \"Authorization: Bearer "+h.AccessToken(subject)+"\"\n"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	var previous uint64
	for _, phase := range []string{"prior_at_n_minus_1", "prior_at_n", "current_at_n"} {
		var highWater protocol.Cursor
		if e = probeImportHTTP(t.Context(), server.URL, owner, caregiver, phase, func(cursor protocol.Cursor) error {
			highWater = cursor
			return nil
		}); e != nil {
			t.Fatalf("phase %s: %v", phase, e)
		}
		if highWater.Sequence <= previous {
			t.Fatalf("phase %s high water did not advance: previous=%d current=%d", phase, previous, highWater.Sequence)
		}
		previous = highWater.Sequence
	}
}
