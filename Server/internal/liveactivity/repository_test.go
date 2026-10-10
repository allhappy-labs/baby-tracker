package liveactivity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// main's repository tests (main:Server/internal/liveactivity/repository_test.go)
// on the branch harness: every registration runs inside the authority's
// request seam (h.Authority.WithRequestTx with the caller's access token),
// and the worker's queue functions are called on the maintenance pool (the
// Go Queue arrives with the worker). Membership-revision assertions are
// lineage assertions (TestLiveActivityRegistrationFollowsTheLineage).

const installation = "70000000-0000-4000-8000-000000000001"
const otherInstallation = "70000000-0000-4000-8000-000000000002"
const topic = "com.example.babytracker.push-type.liveactivity"

func deviceRequest() protocol.LiveActivityDeviceRegistration {
	return protocol.LiveActivityDeviceRegistration{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}}, InstallationID: installation, RegistrationVersion: 1, APNsEnvironment: "sandbox", PayloadVersion: 1, PushToStartToken: "aabb", FeedBreakMinutes: 30}
}

// harness is the harness on the database clock: the session binding and
// app.live_activity_authorized check sessions, and the leases and retries
// their stamps, against clock_timestamp().
func harness(t *testing.T) *testkit.Harness {
	t.Helper()
	h := testkit.NewAtDatabaseClock(t)
	h.Now = h.DatabaseNow()
	return h
}

// inRequest runs run in a request transaction for the session of token, as
// a registration request does (action "read").
func inRequest(t *testing.T, h *testkit.Harness, token string, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	t.Helper()
	digests, err := h.Auth.AccessDigests(token)
	if err != nil {
		t.Fatal(err)
	}
	return h.Authority.WithRequestTx(t.Context(), authority.Request{Digests: digests}, "read", run)
}

func registerAs(t *testing.T, h *testkit.Harness, token, topic string, r protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error) {
	t.Helper()
	var receipt protocol.LiveActivityRegistrationReceipt
	err := inRequest(t, h, token, func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		var err error
		receipt, err = NewRepository(topic).RegisterDevice(t.Context(), tx, p, g, r)
		return err
	})
	return receipt, err
}

func register(t *testing.T, h *testkit.Harness, subject string, r protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error) {
	t.Helper()
	return registerAs(t, h, h.AccessToken(subject), topic, r)
}

func activityAs(t *testing.T, h *testkit.Harness, token, topic string, r protocol.LiveActivityTokenRegistration) (protocol.LiveActivityRegistrationReceipt, error) {
	t.Helper()
	var receipt protocol.LiveActivityRegistrationReceipt
	err := inRequest(t, h, token, func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		var err error
		receipt, err = NewRepository(topic).RegisterActivity(t.Context(), tx, p, g, r)
		return err
	})
	return receipt, err
}

func unregister(t *testing.T, h *testkit.Harness, subject, installationID string, version uint64) error {
	t.Helper()
	return inRequest(t, h, h.AccessToken(subject), func(tx pgx.Tx, p access.Principal, _ access.Grant) error {
		return NewRepository("").UnregisterDevice(t.Context(), tx, p, installationID, version)
	})
}

func seedBaby(t *testing.T, h *testkit.Harness) string {
	t.Helper()
	return h.SeedBabies(t, 1)[0].CanonicalEntity.Meta.ID
}

func scalar(t *testing.T, h *testkit.Harness, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := h.Migrator.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// claim is main's queue Claim: a lease-bound scheduling coordinate.
type claim struct {
	JobID               string    `json:"job_id"`
	LeaseID             string    `json:"lease_id"`
	RegistrationID      string    `json:"registration_id"`
	RegistrationVersion uint64    `json:"registration_version"`
	AccountID           string    `json:"account_id"`
	DeviceID            string    `json:"device_id"`
	FamilyID            string    `json:"family_id"`
	BabyID              string    `json:"baby_id"`
	Kind                string    `json:"kind"`
	InstanceID          string    `json:"instance_id"`
	RecoveryEpoch       uint64    `json:"recovery_epoch"`
	DirtyVersion        uint64    `json:"dirty_version"`
	LeaseDeadline       time.Time `json:"lease_deadline"`
}

// claimWork is main's Queue.Claim at the database clock.
func claimWork(t *testing.T, h *testkit.Harness, limit int) []claim {
	t.Helper()
	rows, err := h.Maintenance.Query(t.Context(), `SELECT * FROM app.claim_live_activity_work(clock_timestamp(),$1)`, limit)
	if err != nil {
		t.Fatal(err)
	}
	raws, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatal(err)
	}
	claims := []claim{}
	for _, raw := range raws {
		var c claim
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		claims = append(claims, c)
	}
	return claims
}

// finishWork is main's Queue.Complete (retryAt nil) or Queue.Retry: whether
// the lease was current.
func finishWork(t *testing.T, h *testkit.Harness, c claim, retryAt *time.Time, reason *string) bool {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var accepted bool
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT app.finish_live_activity_work($1,$2,$3)`, raw, retryAt, reason).Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	return accepted
}

func retainWork(t *testing.T, h *testkit.Harness) int64 {
	t.Helper()
	var n int64
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT app.retain_live_activity_work()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// flushDirty is the worker's change-feed flush (app.dirty_live_activity_families):
// main's journal trigger is not ported, so a committed care change dirties
// the family when the worker flushes it.
func flushDirty(t *testing.T, h *testkit.Harness, families []string, sequences []int64) {
	t.Helper()
	if _, err := h.Maintenance.Exec(t.Context(), `SELECT app.dirty_live_activity_families($1::uuid[],$2::bigint[])`, families, sequences); err != nil {
		t.Fatal(err)
	}
}

func TestLiveActivityRegistrationOwnership(t *testing.T) {
	h := harness(t)
	seedBaby(t, h)
	req := deviceRequest()
	receipt, err := register(t, h, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	if receipt.ServerInstanceID != testkit.InstanceID || receipt.RecoveryEpoch != testkit.RecoveryEpoch || receipt.AcceptedRegistrationVersion != 1 {
		t.Fatalf("receipt %+v", receipt)
	}
	if _, err := register(t, h, "caregiver", req); err == nil {
		t.Fatal("foreign installation takeover accepted")
	}
	_, otherToken, err := h.NewDeviceSession("owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registerAs(t, h, otherToken, topic, req); err == nil {
		t.Fatal("same-account foreign device takeover accepted")
	}
	// main refused a grant without a membership lifetime; the binding's
	// remaining guard is a grant of another account.
	err = inRequest(t, h, h.AccessToken("owner"), func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		g.AccountID = h.Caregiver.AccountID
		_, err := NewRepository(topic).RegisterDevice(t.Context(), tx, p, g, req)
		return err
	})
	if !errors.Is(err, ErrRegistrationDenied) {
		t.Fatalf("another account's grant accepted: %v", err)
	}
	// The unregistration-only repository registers nothing.
	err = inRequest(t, h, h.AccessToken("owner"), func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		_, err := NewRepository("").RegisterDevice(t.Context(), tx, p, g, req)
		return err
	})
	if !errors.Is(err, ErrRegistrationDenied) {
		t.Fatalf("the unregistration-only repository registered: %v", err)
	}

	req.InstallationID = otherInstallation
	if _, err := register(t, h, "caregiver", req); err == nil {
		t.Fatal("foreign live token takeover accepted")
	}
	req.PushToStartToken = "ccdd"
	if _, err := register(t, h, "caregiver", req); err != nil {
		t.Fatal(err)
	}
	err = inRequest(t, h, h.AccessToken("owner"), func(tx pgx.Tx, _ access.Principal, _ access.Grant) error {
		var count int
		if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM live_activity_devices").Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Errorf("runtime enumerated %d accounts' registrations", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The registration is bound to the requesting session's lineage, which
// replaces main's membership revision: a refresh keeps the lineage, so the
// registration stays valid; removal and rejoin start a new lineage, so the
// old registration is denied until the device registers again at a newer
// version.
func TestLiveActivityRegistrationFollowsTheLineage(t *testing.T) {
	h := harness(t)
	baby := seedBaby(t, h)
	req := deviceRequest()
	if _, err := register(t, h, "caregiver", req); err != nil {
		t.Fatal(err)
	}
	token := protocol.LiveActivityTokenRegistration{RequestMeta: req.RequestMeta, InstallationID: installation, ActivityID: "lineage-activity", Token: "eeff", TokenVersion: 1,
		Scope: protocol.LiveActivityScope{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyID: h.FamilyID, BabyID: baby, KindRaw: "feed"}, State: "active"}

	refreshed := h.Refresh("caregiver")
	if refreshed.SessionID == h.Caregiver.SessionID {
		t.Fatal("the refresh kept the session")
	}
	if _, err := register(t, h, "caregiver", req); err != nil {
		t.Fatalf("identical retry after a refresh (same lineage): %v", err)
	}
	if _, err := activityAs(t, h, h.AccessToken("caregiver"), topic, token); err != nil {
		t.Fatalf("card token after a refresh (same lineage): %v", err)
	}

	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Reauthenticate("caregiver"); err != nil {
		t.Fatal(err)
	}
	invite, err := h.Family.Invite(t.Context(), h.Owner, h.CommandID(), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	accountOnly, err := h.Auth.Authenticate(t.Context(), h.AccessToken("caregiver"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Family.Redeem(t.Context(), accountOnly, h.CommandID(), invite.Token, true); err != nil {
		t.Fatal(err)
	}
	rejoined := h.Refresh("caregiver")
	if rejoined.SessionScope != "family" || rejoined.DeviceID != h.Caregiver.DeviceID {
		t.Fatalf("rejoined session %q on device %s", rejoined.SessionScope, rejoined.DeviceID)
	}
	token.TokenVersion = 2
	if _, err := activityAs(t, h, h.AccessToken("caregiver"), topic, token); !errors.Is(err, ErrRegistrationDenied) {
		t.Fatalf("card token on the old lineage's registration after rejoin: %v", err)
	}
	if _, err := register(t, h, "caregiver", req); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("same-version registration after rejoin (another lineage): %v", err)
	}
	req.RegistrationVersion = 2
	if _, err := register(t, h, "caregiver", req); err != nil {
		t.Fatalf("newer registration after rejoin: %v", err)
	}
	if _, err := activityAs(t, h, h.AccessToken("caregiver"), topic, token); err != nil {
		t.Fatalf("card token after re-registration: %v", err)
	}
}

// The receipt's wire is main's (api/fixtures/live-activity-v1/registration-receipt.json):
// a device registration reporting an unconfirmed start, and a card token
// joining that generation, with the fixture's IDs in place of the harness's.
func TestLiveActivityRegistrationReceiptMatchesTheFixture(t *testing.T) {
	h := harness(t)
	baby := seedBaby(t, h)
	const generation, attempt = "5a5a5a5a-5a5a-4a5a-8a5a-5a5a5a5a5a5a", "6b6b6b6b-6b6b-4b6b-8b6b-6b6b6b6b6b6b"
	req := deviceRequest()
	if _, err := register(t, h, "owner", req); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET target_state=jsonb_build_object('generation_id',$2::text,'start_attempt_id',$3::text,'status','startUnconfirmed','kind','feed')
		WHERE device_id=$1 AND kind='feed'`, h.Owner.DeviceID, generation, attempt); err != nil {
		t.Fatal(err)
	}
	device, err := register(t, h, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	known := generation
	card, err := activityAs(t, h, h.AccessToken("owner"), topic, protocol.LiveActivityTokenRegistration{RequestMeta: req.RequestMeta, InstallationID: installation,
		ActivityID: "activity-1", Token: "ccdd", TokenVersion: 2, KnownGenerationID: &known,
		Scope: protocol.LiveActivityScope{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyID: h.FamilyID, BabyID: baby, KindRaw: "feed"}, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(device.Generations) != 0 || device.AcceptedTokenVersion != nil || len(card.UnconfirmedStarts) != 0 {
		t.Fatalf("device receipt %+v, card receipt %+v", device, card)
	}
	merged := card
	merged.UnconfirmedStarts = device.UnconfirmedStarts
	if merged.AcceptedRegistrationVersion != device.AcceptedRegistrationVersion {
		t.Fatalf("card receipt registration version %d, device %d", merged.AcceptedRegistrationVersion, device.AcceptedRegistrationVersion)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.NewReplacer(h.Info.InstanceID, "22222222-2222-4222-8222-222222222222", h.FamilyID, "33333333-3333-4333-8333-333333333333",
		baby, "44444444-4444-4444-8444-444444444444", generation, "55555555-5555-4555-8555-555555555555", attempt, "66666666-6666-4666-8666-666666666666").Replace(string(encoded))
	fixture, err := os.ReadFile("../../api/fixtures/live-activity-v1/registration-receipt.json")
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal([]byte(normalized), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("receipt wire:\n%s\nwant the fixture:\n%s", normalized, fixture)
	}
	var decoded protocol.LiveActivityRegistrationReceipt
	if err := json.Unmarshal(fixture, &decoded); err != nil || decoded.Validate() != nil {
		t.Fatalf("fixture decode %v / validate %v", err, decoded.Validate())
	}
}

func TestLiveActivityVersionCAS(t *testing.T) {
	h := harness(t)
	seedBaby(t, h)
	req := deviceRequest()
	if _, err := register(t, h, "owner", req); err != nil {
		t.Fatal(err)
	}
	n := scalar(t, h, "SELECT sum(dirty_version) FROM live_activity_family_dirty")
	if _, err := register(t, h, "owner", req); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, h, "SELECT sum(dirty_version) FROM live_activity_family_dirty"); got != n {
		t.Fatal("identical retry dirtied work")
	}
	req.FeedBreakMinutes = 60
	if _, err := register(t, h, "owner", req); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("equal-version changed retry: %v", err)
	}
	req.RegistrationVersion = 2
	if _, err := register(t, h, "owner", req); err != nil {
		t.Fatal(err)
	}
	if err := unregister(t, h, "owner", installation, 3); err != nil {
		t.Fatal(err)
	}
	for _, version := range []uint64{1, 2, 3} {
		req.RegistrationVersion = version
		if _, err := register(t, h, "owner", req); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("tombstone resurrected at %d: %v", version, err)
		}
	}
	req.RegistrationVersion = 4
	if _, err := register(t, h, "owner", req); err != nil {
		t.Fatal(err)
	}
}

// main's journal trigger is not ported: a care write dirties the family
// when the worker flushes the committed change feed
// (app.dirty_live_activity_families), so a rolled-back flush leaves no dirty
// work, and a write by an unregistered actor dirties the registered
// caregiver's family once flushed.
func TestLiveActivityJournalRollback(t *testing.T) {
	h := harness(t)
	baby := seedBaby(t, h)
	if got := scalar(t, h, "SELECT count(*) FROM live_activity_family_dirty"); got != 0 {
		t.Fatal("unregistered family enqueued")
	}
	if _, err := register(t, h, "caregiver", deviceRequest()); err != nil {
		t.Fatal(err)
	}
	before := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID)
	tx, err := h.Maintenance.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `SELECT app.dirty_live_activity_families($1::uuid[],$2::bigint[])`, []string{h.FamilyID}, []int64{100}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); got != before {
		t.Fatal("rollback left dirty work")
	}
	h.RenameBaby(t, baby, "Updated by unregistered owner")
	if got := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); got != before {
		t.Fatal("a care write dirtied the family in SQL (main's journal trigger)")
	}
	flushDirty(t, h, []string{h.FamilyID}, []int64{int64(h.FamilySequence(t))})
	if got := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); got != before+1 {
		t.Fatalf("unregistered actor failed to dirty registered caregiver: %d -> %d", before, got)
	}
}

func TestLiveActivityLeaseFence(t *testing.T) {
	h := harness(t)
	baby := seedBaby(t, h)
	if _, err := register(t, h, "owner", deviceRequest()); err != nil {
		t.Fatal(err)
	}
	claims := claimWork(t, h, 10)
	if len(claims) != 2 {
		t.Fatalf("claims=%v", claims)
	}
	old := claims[0]
	h.RenameBaby(t, baby, "New revision")
	flushDirty(t, h, []string{h.FamilyID}, []int64{int64(h.FamilySequence(t))})
	if finishWork(t, h, old, nil, nil) {
		t.Fatal("old dirty claim completed")
	}
	if _, err := h.Migrator.Exec(t.Context(), "UPDATE live_activity_delivery SET lease_deadline=now()-interval '1 second' WHERE job_id=$1", old.JobID); err != nil {
		t.Fatal(err)
	}
	claims = claimWork(t, h, 10)
	if len(claims) != 1 {
		t.Fatalf("reclaim=%v", claims)
	}
	if finishWork(t, h, old, nil, nil) {
		t.Fatal("expired completion accepted")
	}
	current := claims[0]
	mutated := current
	mutated.RegistrationVersion++
	minute, hour, reason := h.DatabaseNow().Add(time.Minute), h.DatabaseNow().Add(time.Hour), "unavailable"
	if finishWork(t, h, mutated, &minute, &reason) {
		t.Fatal("forged retry accepted")
	}
	if !finishWork(t, h, current, &hour, &reason) {
		t.Fatal("retry refused")
	}
	if finishWork(t, h, current, nil, nil) {
		t.Fatal("released claim completed")
	}
}

func TestLiveActivityTokenCAS(t *testing.T) {
	h := harness(t)
	baby := seedBaby(t, h)
	if _, err := register(t, h, "owner", deviceRequest()); err != nil {
		t.Fatal(err)
	}
	req := protocol.LiveActivityTokenRegistration{RequestMeta: deviceRequest().RequestMeta, InstallationID: installation, ActivityID: "activity-1", Token: "ccdd", TokenVersion: 1, Scope: protocol.LiveActivityScope{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyID: h.FamilyID, BabyID: baby, KindRaw: "feed"}, State: "active"}
	call := func() (protocol.LiveActivityRegistrationReceipt, error) {
		return activityAs(t, h, h.AccessToken("owner"), topic, req)
	}
	other := deviceRequest()
	other.InstallationID = otherInstallation
	other.PushToStartToken = "ccdd"
	if _, err := register(t, h, "caregiver", other); err != nil {
		t.Fatal(err)
	}
	if _, err := call(); err == nil {
		t.Fatal("update token stole another device push token")
	}
	req.Token = "eeff"
	first, err := call()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Generations) != 1 {
		t.Fatal("missing generation")
	}
	next, err := call()
	if err != nil || next.Generations[0] != first.Generations[0] {
		t.Fatal("retry changed generation", err)
	}
	req.State = "dismissed"
	if _, err = call(); !errors.Is(err, ErrVersionConflict) {
		t.Fatal("equal version mutation accepted", err)
	}
	req.TokenVersion = 2
	if _, err = call(); err != nil {
		t.Fatal(err)
	}
	req.State = "active"
	req.TokenVersion = 1
	if _, err = call(); !errors.Is(err, ErrVersionConflict) {
		t.Fatal("stale activity resurrected", err)
	}
	// The card's baby is any baby entity of the family, deleted ones
	// included (main's babies kept tombstoned rows): a card still running
	// for a baby deleted on another phone registers, so the phone does not
	// fall into its refused-card re-registration loop. An unknown ID or
	// another kind's entity is refused.
	deleted := "10000000-0000-4000-8000-0000000000d1"
	caretest.Seed(t, h, caretest.Baby(deleted))
	caretest.Seed(t, h, caretest.DeleteBaby(deleted))
	feed := caretest.Feed(baby, 1)
	caretest.Seed(t, h, feed)
	for _, c := range []struct {
		activity, token, baby string
		want                  error
	}{
		{"activity-deleted-baby", "abab", deleted, nil},
		{"activity-unknown", "acac", "10000000-0000-4000-8000-0000000000ff", ErrRegistrationDenied},
		{"activity-feed", "adad", feed.EntityID, ErrRegistrationDenied},
	} {
		req.ActivityID, req.Token, req.Scope.BabyID, req.TokenVersion = c.activity, c.token, c.baby, 1
		if _, err = call(); !errors.Is(err, c.want) {
			t.Fatalf("card %s for %s: %v, want %v", c.activity, c.baby, err, c.want)
		}
	}
}

func TestLiveActivityFamilySwitchFence(t *testing.T) {
	h := harness(t)
	seedBaby(t, h)
	req := deviceRequest()
	if _, err := register(t, h, "caregiver", req); err != nil {
		t.Fatal(err)
	}
	oldFamily := h.FamilyID
	if _, err := h.Family.Leave(t.Context(), h.Caregiver, h.CommandID()); err != nil {
		t.Fatal(err)
	}
	var err error
	h.Caregiver, err = h.Reauthenticate("caregiver")
	if err != nil {
		t.Fatal(err)
	}
	created, err := h.Family.Create(t.Context(), h.Caregiver, h.CommandID(), "Second family")
	if err != nil {
		t.Fatal(err)
	}
	h.Caregiver, err = h.Reauthenticate("caregiver")
	if err != nil {
		t.Fatal(err)
	}
	if h.Caregiver.SessionFamilyID != created.ID {
		t.Fatalf("caregiver session family %s, want %s", h.Caregiver.SessionFamilyID, created.ID)
	}
	req.RegistrationVersion = 2
	if _, err = register(t, h, "caregiver", req); err != nil {
		t.Fatal(err)
	}
	tx, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Caregiver.AccountID, FamilyID: oldFamily}); err != nil {
		t.Fatal(err)
	}
	var revoked bool
	if err = tx.QueryRow(t.Context(), "SELECT revoked FROM live_activity_devices WHERE installation_id=$1", installation).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("old family not revoked: %v", err)
	}
	for _, version := range []int{1, 2} {
		var accepted bool
		if err = tx.QueryRow(t.Context(), `SELECT app.fence_live_activity_installation($1,1,$2,$3,$4,$5,$6,now()+interval '1 hour',false)`, h.Info.InstanceID, installation, h.Caregiver.AccountID, h.Caregiver.DeviceID, oldFamily, version).Scan(&accepted); err != nil {
			t.Fatal(err)
		}
		if accepted {
			t.Fatalf("old family accepted delayed version %d", version)
		}
	}
}

// main's claim swept the cursor itself; here the worker sweeps
// (app.sweep_live_activity_work) and flushes its change feed in batches.
func TestLiveActivityBatchAndSweep(t *testing.T) {
	h := harness(t)
	seedBaby(t, h)
	if _, err := register(t, h, "caregiver", deviceRequest()); err != nil {
		t.Fatal(err)
	}
	before := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID)
	flushDirty(t, h, []string{h.FamilyID, h.FamilyID}, []int64{100, 101})
	if got := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); got != before+1 {
		t.Fatalf("batch dirtied %d times", got-before)
	}
	if got := scalar(t, h, "SELECT journal_sequence FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); got != 101 {
		t.Fatalf("batch cursor=%d", got)
	}
	// Simulate a missed change-feed flush; the sweep repairs it without one.
	h.ExecFamily(t, "UPDATE families SET next_sequence=102 WHERE id=$1", h.FamilyID)
	if _, err := h.Maintenance.Exec(t.Context(), `SELECT app.sweep_live_activity_work()`); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, h, "SELECT journal_sequence FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); got != 102 {
		t.Fatalf("sweep cursor=%d", got)
	}
	claims := claimWork(t, h, 10)
	if len(claims) != 2 {
		t.Fatalf("sweep claims=%v", claims)
	}
	for _, c := range claims {
		if !finishWork(t, h, c, nil, nil) {
			t.Fatal("completion refused")
		}
	}
	if claims = claimWork(t, h, 10); len(claims) != 0 {
		t.Fatalf("completed work reclaimed: %v", claims)
	}
}

func TestLiveActivityRetentionKeepsAuthorityTombstones(t *testing.T) {
	h := harness(t)
	seedBaby(t, h)
	if _, err := register(t, h, "owner", deviceRequest()); err != nil {
		t.Fatal(err)
	}
	if err := unregister(t, h, "owner", installation, 2); err != nil {
		t.Fatal(err)
	}
	if n := retainWork(t, h); n != 0 {
		t.Fatalf("live authority retained=%d", n)
	}
	h.ExecFamily(t, "UPDATE live_activity_devices SET authority_expires_at=now()-interval '1 second' WHERE installation_id=$1", installation)
	if _, err := h.Migrator.Exec(t.Context(), "UPDATE live_activity_installations SET authority_expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if n := retainWork(t, h); n != 1 {
		t.Fatalf("expired retention=%d", n)
	}
	if n := scalar(t, h, "SELECT count(*) FROM live_activity_installations"); n != 0 {
		t.Fatal("expired installation fence retained")
	}
}

func TestLiveActivityFullClaimFence(t *testing.T) {
	h := harness(t)
	seedBaby(t, h)
	if _, err := register(t, h, "owner", deviceRequest()); err != nil {
		t.Fatal(err)
	}
	claims := claimWork(t, h, 1)
	if len(claims) != 1 {
		t.Fatalf("claim %v", claims)
	}
	id := "70000000-0000-4000-8000-000000000099"
	changes := []func(*claim){func(c *claim) { c.JobID = id }, func(c *claim) { c.LeaseID = id }, func(c *claim) { c.RegistrationID = id }, func(c *claim) { c.RegistrationVersion++ }, func(c *claim) { c.AccountID = id }, func(c *claim) { c.DeviceID = id }, func(c *claim) { c.FamilyID = id }, func(c *claim) { c.BabyID = id }, func(c *claim) { c.Kind = "other" }, func(c *claim) { c.InstanceID = id }, func(c *claim) { c.RecoveryEpoch++ }, func(c *claim) { c.DirtyVersion++ }, func(c *claim) { c.LeaseDeadline = c.LeaseDeadline.Add(time.Second) }}
	for i, change := range changes {
		c := claims[0]
		change(&c)
		if finishWork(t, h, c, nil, nil) {
			t.Fatalf("coordinate %d accepted", i)
		}
	}
	expired := claims[0]
	expired.LeaseDeadline = h.DatabaseNow().Add(-time.Second).UTC().Truncate(time.Microsecond)
	if _, err := h.Migrator.Exec(t.Context(), "UPDATE live_activity_delivery SET lease_deadline=$2 WHERE job_id=$1", expired.JobID, expired.LeaseDeadline); err != nil {
		t.Fatal(err)
	}
	if finishWork(t, h, expired, nil, nil) {
		t.Fatal("matching expired lease accepted")
	}
	if _, err := h.Migrator.Exec(t.Context(), "UPDATE live_activity_delivery SET lease_deadline=$2 WHERE job_id=$1", claims[0].JobID, claims[0].LeaseDeadline); err != nil {
		t.Fatal(err)
	}
	if !finishWork(t, h, claims[0], nil, nil) {
		t.Fatal("current lease refused")
	}
}

func TestLiveActivityUnregisterBeforeRegister(t *testing.T) {
	h := harness(t)
	seedBaby(t, h)
	if err := unregister(t, h, "owner", installation, 2); err != nil {
		t.Fatal(err)
	}
	req := deviceRequest()
	for _, v := range []uint64{1, 2} {
		req.RegistrationVersion = v
		if _, err := register(t, h, "owner", req); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("delayed registration %d: %v", v, err)
		}
	}
	req.RegistrationVersion = 3
	if _, err := register(t, h, "owner", req); err != nil {
		t.Fatal(err)
	}
}

func TestLiveActivityRecoveryEpochResetsCursorAndRetiresTokens(t *testing.T) {
	h := harness(t)
	seedBaby(t, h)
	req := deviceRequest()
	if _, err := register(t, h, "owner", req); err != nil {
		t.Fatal(err)
	}
	other := deviceRequest()
	other.InstallationID = otherInstallation
	other.PushToStartToken = "ccdd"
	if _, err := register(t, h, "caregiver", other); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Migrator.Exec(t.Context(), "UPDATE live_activity_family_dirty SET journal_sequence=100"); err != nil {
		t.Fatal(err)
	}
	h.AuthorityExec(t, "UPDATE server_state SET recovery_epoch=2")
	h.AuthorityExec(t, "UPDATE sessions SET recovery_epoch=2 WHERE id=$1 OR id=$2", h.Owner.SessionID, h.Caregiver.SessionID)
	// Storage tests use the principal backed by the updated durable session
	// (the authority itself is bound to epoch 1, as main's storage test
	// bypassed its guard).
	call := func(p access.Principal, r protocol.LiveActivityDeviceRegistration) error {
		p.RecoveryEpoch = 2
		tx, err := h.Runtime.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: p.AccountID, FamilyID: h.FamilyID}); err != nil {
			return err
		}
		_, err = NewRepository(topic).RegisterDevice(t.Context(), tx, p, access.Grant{AccountID: p.AccountID, FamilyID: h.FamilyID}, r)
		if err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	if err := call(h.Owner, req); err != nil {
		t.Fatalf("same-owner token blocked after recovery: %v", err)
	}
	// Prior-epoch authority is objectively dead even when its token owner differs.
	req.InstallationID = "70000000-0000-4000-8000-000000000003"
	req.PushToStartToken = "ccdd"
	if err := call(h.Owner, req); err != nil {
		t.Fatalf("prior-epoch other-owner token blocked: %v", err)
	}
	claims := claimWork(t, h, 10)
	if len(claims) != 4 {
		t.Fatalf("epoch claims=%v", claims)
	}
	for _, c := range claims {
		if c.RecoveryEpoch != 2 {
			t.Fatal("prior epoch job reclaimed")
		}
	}
	// The worker's sweep (main's claim swept) moves the reset cursor to the family's sequence.
	if _, err := h.Maintenance.Exec(t.Context(), `SELECT app.sweep_live_activity_work()`); err != nil {
		t.Fatal(err)
	}
	if seq := scalar(t, h, "SELECT journal_sequence FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); seq != int64(h.FamilySequence(t)) {
		t.Fatalf("old epoch cursor retained: %d", seq)
	}
}

// The revoke trigger takes the family's dirty row before delivery, as the
// worker's change-feed enqueue (app.dirty_live_activity_family) does. Force
// both transactions to meet at that seam.
func TestLiveActivityUnregisterAndJournalUseSameLockOrder(t *testing.T) {
	h := harness(t)
	seedBaby(t, h)
	req := deviceRequest()
	if _, err := register(t, h, "owner", req); err != nil {
		t.Fatal(err)
	}
	req.InstallationID = otherInstallation
	req.PushToStartToken = "ccdd"
	if _, err := register(t, h, "caregiver", req); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	care, err := h.Migrator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer care.Rollback(context.Background())
	var carePID int
	if err = care.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&carePID); err != nil {
		t.Fatal(err)
	}
	if _, err = care.Exec(ctx, "SELECT 1 FROM live_activity_family_dirty WHERE family_id=$1 FOR UPDATE", h.FamilyID); err != nil {
		t.Fatal(err)
	}
	digests, err := h.Auth.AccessDigests(h.AccessToken("caregiver"))
	if err != nil {
		t.Fatal(err)
	}
	revoked := make(chan error, 1)
	go func() {
		revoked <- h.Authority.WithRequestTx(ctx, authority.Request{Digests: digests}, "read", func(tx pgx.Tx, p access.Principal, _ access.Grant) error {
			return NewRepository("").UnregisterDevice(ctx, tx, p, otherInstallation, 2)
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked bool
		if err = h.Admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::integer=ANY(pg_blocking_pids(pid)))", carePID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unregister never reached the held scheduling lock")
		}
		select {
		case err := <-revoked:
			t.Fatalf("unregister finished before scheduling lock was released: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = care.Exec(ctx, `SELECT app.dirty_live_activity_families($1::uuid[],$2::bigint[])`, []string{h.FamilyID}, []int64{100}); err != nil {
		t.Fatalf("change-feed enqueue aborted during concurrent unregister: %v", err)
	}
	if err = care.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-revoked; err != nil {
		t.Fatalf("unregister aborted during concurrent enqueue: %v", err)
	}
	if n := scalar(t, h, "SELECT count(*) FROM live_activity_delivery WHERE device_id=$1", h.Caregiver.DeviceID); n != 0 {
		t.Fatal("revoked device kept deliveries")
	}
	if n := scalar(t, h, "SELECT count(*) FROM live_activity_delivery WHERE device_id=$1", h.Owner.DeviceID); n != 2 {
		t.Fatal("care write lost other device deliveries")
	}
}

func TestLiveActivityAPNsCoordinateTransition(t *testing.T) {
	for _, change := range []string{"environment", "topic"} {
		t.Run(change, func(t *testing.T) {
			h := harness(t)
			baby := seedBaby(t, h)
			device := deviceRequest()
			currentTopic := topic
			if _, err := register(t, h, "owner", device); err != nil {
				t.Fatal(err)
			}
			token := protocol.LiveActivityTokenRegistration{RequestMeta: device.RequestMeta, InstallationID: installation, ActivityID: "coordinate-activity", Token: "eeff", TokenVersion: 1, Scope: protocol.LiveActivityScope{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyID: h.FamilyID, BabyID: baby, KindRaw: "feed"}, State: "active"}
			activity := func(subject string, r protocol.LiveActivityTokenRegistration) error {
				_, err := activityAs(t, h, h.AccessToken(subject), currentTopic, r)
				return err
			}
			registerCurrent := func(subject string, r protocol.LiveActivityDeviceRegistration) error {
				_, err := registerAs(t, h, h.AccessToken(subject), currentTopic, r)
				return err
			}
			if err := activity("owner", token); err != nil {
				t.Fatal(err)
			}
			device.RegistrationVersion = 2
			if change == "environment" {
				device.APNsEnvironment = "production"
			} else {
				currentTopic = topic + ".new"
			}
			if err := registerCurrent("owner", device); err != nil {
				t.Fatal(err)
			}
			err := inRequest(t, h, h.AccessToken("owner"), func(tx pgx.Tx, _ access.Principal, _ access.Grant) error {
				var state string
				if err := tx.QueryRow(t.Context(), "SELECT state FROM live_activity_tokens WHERE activity_id=$1", token.ActivityID).Scan(&state); err != nil {
					return err
				}
				if state != "ended" {
					t.Errorf("incompatible activity token remains %s", state)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = activity("owner", token); !errors.Is(err, ErrVersionConflict) {
				t.Fatalf("stale activity retry after coordinate change: %v", err)
			}
			// The old environment/topic claim must be released, but the new claim must
			// still reject live ownership conflicts after the update-token transition.
			token.TokenVersion = 2
			if err = activity("owner", token); err != nil {
				t.Fatal(err)
			}
			if err = activity("owner", token); err != nil {
				t.Fatalf("identical retry: %v", err)
			}
			var gotEnvironment, gotTopic string
			err = inRequest(t, h, h.AccessToken("owner"), func(tx pgx.Tx, _ access.Principal, _ access.Grant) error {
				return tx.QueryRow(t.Context(), "SELECT environment,topic FROM live_activity_tokens WHERE activity_id=$1", token.ActivityID).Scan(&gotEnvironment, &gotTopic)
			})
			if err != nil || gotEnvironment != device.APNsEnvironment || gotTopic != currentTopic {
				t.Fatalf("coordinates=%s/%s err=%v", gotEnvironment, gotTopic, err)
			}
			other := device
			other.InstallationID = otherInstallation
			other.RegistrationVersion = 1
			other.PushToStartToken = token.Token
			if err = registerCurrent("caregiver", other); !errors.Is(err, ErrRegistrationDenied) {
				t.Fatalf("new-coordinate token takeover: %v", err)
			}
			old := deviceRequest()
			old.InstallationID = otherInstallation
			old.PushToStartToken = token.Token
			if _, err = register(t, h, "caregiver", old); err != nil {
				t.Fatalf("old coordinates still owned: %v", err)
			}
		})
	}
}
