package liveactivity

import (
	"context"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

const installation = "70000000-0000-4000-8000-000000000001"
const otherInstallation = "70000000-0000-4000-8000-000000000002"
const topic = "com.example.babytracker.push-type.liveactivity"

func deviceRequest() protocol.LiveActivityDeviceRegistration {
	return protocol.LiveActivityDeviceRegistration{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}}, InstallationID: installation, RegistrationVersion: 1, APNsEnvironment: "sandbox", PayloadVersion: 1, PushToStartToken: "aabb", FeedBreakMinutes: 30}
}
func register(t *testing.T, h *testkit.Harness, p access.Principal, r protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error) {
	t.Helper()
	var receipt protocol.LiveActivityRegistrationReceipt
	err := h.Security.WithAuthorizedTransaction(t.Context(), p, h.FamilyID, "read", func(tx pgx.Tx, g access.Grant) error {
		g.MembershipRevision = 1
		var err error
		receipt, err = NewRepository(topic).RegisterDevice(t.Context(), tx, p, g, r)
		return err
	})
	return receipt, err
}
func seedBaby(t *testing.T, h *testkit.Harness) string {
	t.Helper()
	head, err := h.Security.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Migrator.Exec(t.Context(), `SELECT app.update_server_state($1,$2,$3,$4,$5)`, h.Info.InstanceID, h.Info.RecoveryEpoch, head.Generation, head.Revision, head.EventHash[:]); err != nil {
		t.Fatal(err)
	}
	m := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil {
		t.Fatal(err)
	}
	return m.EntityID
}
func scalar(t *testing.T, h *testkit.Harness, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := h.Migrator.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestLiveActivityRegistrationOwnership(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	req := deviceRequest()
	receipt, err := register(t, h, h.Owner, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := register(t, h, h.Caregiver, req); err == nil {
		t.Fatal("foreign installation takeover accepted")
	}
	otherDevice, err := h.NewDevice("owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = register(t, h, otherDevice, req); err == nil {
		t.Fatal("same-account foreign device takeover accepted")
	}
	err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, g access.Grant) error {
		g.MembershipRevision = 0
		_, err := NewRepository(topic).RegisterDevice(t.Context(), tx, h.Owner, g, req)
		return err
	})
	if !errors.Is(err, ErrRegistrationDenied) {
		t.Fatalf("missing membership lifetime accepted: %v", err)
	}

	req.InstallationID = otherInstallation
	if _, err := register(t, h, h.Caregiver, req); err == nil {
		t.Fatal("foreign live token takeover accepted")
	}
	req.PushToStartToken = "ccdd"
	if _, err := register(t, h, h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, g access.Grant) error {
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
func TestLiveActivityVersionCAS(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	req := deviceRequest()
	if _, err := register(t, h, h.Owner, req); err != nil {
		t.Fatal(err)
	}
	n := scalar(t, h, "SELECT sum(dirty_version) FROM live_activity_family_dirty")
	if _, err := register(t, h, h.Owner, req); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, h, "SELECT sum(dirty_version) FROM live_activity_family_dirty"); got != n {
		t.Fatal("identical retry dirtied work")
	}
	req.FeedBreakMinutes = 60
	if _, err := register(t, h, h.Owner, req); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("equal-version changed retry: %v", err)
	}
	req.RegistrationVersion = 2
	if _, err := register(t, h, h.Owner, req); err != nil {
		t.Fatal(err)
	}
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		return NewRepository(topic).UnregisterDevice(t.Context(), tx, h.Owner, installation, 3)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []uint64{1, 2, 3} {
		req.RegistrationVersion = version
		if _, err := register(t, h, h.Owner, req); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("tombstone resurrected at %d: %v", version, err)
		}
	}
	req.RegistrationVersion = 4
	if _, err := register(t, h, h.Owner, req); err != nil {
		t.Fatal(err)
	}
}
func TestLiveActivityJournalRollback(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	if got := scalar(t, h, "SELECT count(*) FROM live_activity_family_dirty"); got != 0 {
		t.Fatal("unregistered family enqueued")
	}
	if _, err := register(t, h, h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	before := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID)
	tx, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(t.Context(), `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
 VALUES($1,100,'baby',$2,100,$3,$4,'update','{}',now()),($1,101,'baby',$2,101,$3,$4,'update','{}',now())`, h.FamilyID, baby, h.Owner.AccountID, h.Owner.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); got != before {
		t.Fatal("rollback left dirty work")
	}
	h.RenameBaby(t, baby, "Updated by unregistered owner")
	if got := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); got != before+1 {
		t.Fatalf("unregistered actor failed to dirty registered caregiver: %d -> %d", before, got)
	}
}
func TestLiveActivityLeaseFence(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	req := deviceRequest()
	if _, err := register(t, h, h.Owner, req); err != nil {
		t.Fatal(err)
	}
	q := NewQueue(h.LiveActivity)
	claims, err := q.Claim(t.Context(), time.Now(), 10)
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims=%v err=%v", claims, err)
	}
	old := claims[0]
	h.RenameBaby(t, baby, "New revision")
	if err := q.Complete(t.Context(), old); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("old dirty claim completion: %v", err)
	}
	if _, err := h.Migrator.Exec(t.Context(), "UPDATE live_activity_delivery SET lease_deadline=now()-interval '1 second' WHERE job_id=$1", old.JobID); err != nil {
		t.Fatal(err)
	}
	claims, err = q.Claim(t.Context(), time.Now(), 10)
	if err != nil || len(claims) != 1 {
		t.Fatalf("reclaim=%v err=%v", claims, err)
	}
	if err = q.Complete(t.Context(), old); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("expired completion: %v", err)
	}
	current := claims[0]
	mutated := current
	mutated.RegistrationVersion++
	if err = q.Retry(t.Context(), mutated, time.Now().Add(time.Minute), "unavailable"); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("forged retry: %v", err)
	}
	if err = q.Retry(t.Context(), current, time.Now().Add(time.Hour), "unavailable"); err != nil {
		t.Fatal(err)
	}
	if err = q.Complete(t.Context(), current); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("released claim completed: %v", err)
	}
}
func TestLiveActivityTokenCAS(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	if _, err := register(t, h, h.Owner, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	req := protocol.LiveActivityTokenRegistration{RequestMeta: deviceRequest().RequestMeta, InstallationID: installation, ActivityID: "activity-1", Token: "ccdd", TokenVersion: 1, Scope: protocol.LiveActivityScope{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyID: h.FamilyID, BabyID: baby, KindRaw: "feed"}, State: "active"}
	call := func() (protocol.LiveActivityRegistrationReceipt, error) {
		var result protocol.LiveActivityRegistrationReceipt
		err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, g access.Grant) error {
			g.MembershipRevision = 1
			var err error
			result, err = NewRepository(topic).RegisterActivity(t.Context(), tx, h.Owner, g, req)
			return err
		})
		return result, err
	}
	other := deviceRequest()
	other.InstallationID = otherInstallation
	other.PushToStartToken = "ccdd"
	if _, err := register(t, h, h.Caregiver, other); err != nil {
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
}

func TestLiveActivityFamilySwitchFence(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	req := deviceRequest()
	if _, err := register(t, h, h.Caregiver, req); err != nil {
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
	h.FamilyID = created.ID
	req.RegistrationVersion = 2
	if _, err = register(t, h, h.Caregiver, req); err != nil {
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
func TestLiveActivityBatchAndSweep(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	if _, err := register(t, h, h.Caregiver, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	before := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID)
	h.ExecFamily(t, `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
 VALUES($1,100,'baby',$2,100,$3,$4,'update','{}',now()),($1,101,'baby',$2,101,$3,$4,'update','{}',now())`, h.FamilyID, baby, h.Owner.AccountID, h.Owner.DeviceID)
	if got := scalar(t, h, "SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); got != before+1 {
		t.Fatalf("batch dirtied %d times", got-before)
	}
	// Simulate a missed cursor wake; the next durable claim repairs it without NOTIFY.
	h.ExecFamily(t, "UPDATE families SET next_sequence=102 WHERE id=$1", h.FamilyID)
	q := NewQueue(h.LiveActivity)
	claims, err := q.Claim(t.Context(), time.Now(), 10)
	if err != nil || len(claims) != 2 {
		t.Fatalf("sweep claims=%v err=%v", claims, err)
	}
	if got := scalar(t, h, "SELECT journal_sequence FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); got != 102 {
		t.Fatalf("sweep cursor=%d", got)
	}
	for _, c := range claims {
		if err = q.Complete(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	claims, err = q.Claim(t.Context(), time.Now(), 10)
	if err != nil || len(claims) != 0 {
		t.Fatalf("completed work reclaimed: %v %v", claims, err)
	}
}
func TestLiveActivityRetentionKeepsAuthorityTombstones(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	if _, err := register(t, h, h.Owner, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		return NewRepository(topic).UnregisterDevice(t.Context(), tx, h.Owner, installation, 2)
	})
	if err != nil {
		t.Fatal(err)
	}
	q := NewQueue(h.LiveActivity)
	if n, err := q.Retain(t.Context()); err != nil || n != 0 {
		t.Fatalf("live authority retained=%d err=%v", n, err)
	}
	h.ExecFamily(t, "UPDATE live_activity_devices SET authority_expires_at=now()-interval '1 second' WHERE installation_id=$1", installation)
	if _, err = h.Migrator.Exec(t.Context(), "UPDATE live_activity_installations SET authority_expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if n, err := q.Retain(t.Context()); err != nil || n != 1 {
		t.Fatalf("expired retention=%d err=%v", n, err)
	}
	if n := scalar(t, h, "SELECT count(*) FROM live_activity_installations"); n != 0 {
		t.Fatal("expired installation fence retained")
	}
}
func TestLiveActivityFullClaimFence(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	if _, err := register(t, h, h.Owner, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	q := NewQueue(h.LiveActivity)
	claims, err := q.Claim(t.Context(), time.Now(), 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %v %v", claims, err)
	}
	id := "70000000-0000-4000-8000-000000000099"
	changes := []func(*Claim){func(c *Claim) { c.JobID = id }, func(c *Claim) { c.LeaseID = id }, func(c *Claim) { c.RegistrationID = id }, func(c *Claim) { c.RegistrationVersion++ }, func(c *Claim) { c.AccountID = id }, func(c *Claim) { c.DeviceID = id }, func(c *Claim) { c.FamilyID = id }, func(c *Claim) { c.BabyID = id }, func(c *Claim) { c.Kind = "other" }, func(c *Claim) { c.InstanceID = id }, func(c *Claim) { c.RecoveryEpoch++ }, func(c *Claim) { c.DirtyVersion++ }, func(c *Claim) { c.LeaseDeadline = c.LeaseDeadline.Add(time.Second) }}
	for i, change := range changes {
		c := claims[0]
		change(&c)
		if err = q.Complete(t.Context(), c); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("coordinate %d accepted: %v", i, err)
		}
	}
	expired := claims[0]
	expired.LeaseDeadline = time.Now().Add(-time.Second).UTC().Truncate(time.Microsecond)
	if _, err = h.Migrator.Exec(t.Context(), "UPDATE live_activity_delivery SET lease_deadline=$2 WHERE job_id=$1", expired.JobID, expired.LeaseDeadline); err != nil {
		t.Fatal(err)
	}
	if err = q.Complete(t.Context(), expired); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("matching expired lease accepted: %v", err)
	}
	if _, err = h.Migrator.Exec(t.Context(), "UPDATE live_activity_delivery SET lease_deadline=$2 WHERE job_id=$1", claims[0].JobID, claims[0].LeaseDeadline); err != nil {
		t.Fatal(err)
	}
	if err = q.Complete(t.Context(), claims[0]); err != nil {
		t.Fatal(err)
	}
}

func TestLiveActivityUnregisterBeforeRegister(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		return NewRepository(topic).UnregisterDevice(t.Context(), tx, h.Owner, installation, 2)
	})
	if err != nil {
		t.Fatal(err)
	}
	req := deviceRequest()
	for _, v := range []uint64{1, 2} {
		req.RegistrationVersion = v
		if _, err = register(t, h, h.Owner, req); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("delayed registration %d: %v", v, err)
		}
	}
	req.RegistrationVersion = 3
	if _, err = register(t, h, h.Owner, req); err != nil {
		t.Fatal(err)
	}
}

func TestLiveActivityRecoveryEpochResetsCursorAndRetiresTokens(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	req := deviceRequest()
	if _, err := register(t, h, h.Owner, req); err != nil {
		t.Fatal(err)
	}
	other := deviceRequest()
	other.InstallationID = otherInstallation
	other.PushToStartToken = "ccdd"
	if _, err := register(t, h, h.Caregiver, other); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Migrator.Exec(t.Context(), "UPDATE live_activity_family_dirty SET journal_sequence=100"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Migrator.Exec(t.Context(), "UPDATE server_state SET recovery_epoch=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Migrator.Exec(t.Context(), "UPDATE sessions SET recovery_epoch=2 WHERE session_id=$1 OR session_id=$2", h.Owner.SessionID, h.Caregiver.SessionID); err != nil {
		t.Fatal(err)
	}
	// Storage tests use the principal backed by the updated durable session. The
	// independent security-authority recovery/guard integration belongs to Task 3.
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
		_, err = NewRepository(topic).RegisterDevice(t.Context(), tx, p, access.Grant{AccountID: p.AccountID, FamilyID: h.FamilyID, MembershipRevision: 1}, r)
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
	q := NewQueue(h.LiveActivity)
	claims, err := q.Claim(t.Context(), time.Now(), 10)
	if err != nil || len(claims) != 4 {
		t.Fatalf("epoch claims=%v err=%v", claims, err)
	}
	for _, claim := range claims {
		if claim.RecoveryEpoch != 2 {
			t.Fatal("prior epoch job reclaimed")
		}
	}
	if seq := scalar(t, h, "SELECT journal_sequence FROM live_activity_family_dirty WHERE family_id=$1", h.FamilyID); seq != int64(h.FamilySequence(t)) {
		t.Fatalf("old epoch cursor retained: %d", seq)
	}
}

// The old revoke trigger acquired delivery before dirty while journal enqueue
// acquired dirty before delivery. Force both transactions to meet at that seam.
func TestLiveActivityUnregisterAndJournalUseSameLockOrder(t *testing.T) {
	h := testkit.New(t)
	baby := seedBaby(t, h)
	req := deviceRequest()
	if _, err := register(t, h, h.Owner, req); err != nil {
		t.Fatal(err)
	}
	req.InstallationID = otherInstallation
	req.PushToStartToken = "ccdd"
	if _, err := register(t, h, h.Caregiver, req); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	care, err := h.Migrator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer care.Rollback(context.Background())
	if err = tenant.SetLocal(ctx, care, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	var carePID int
	if err = care.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&carePID); err != nil {
		t.Fatal(err)
	}
	if _, err = care.Exec(ctx, "SELECT 1 FROM live_activity_family_dirty WHERE family_id=$1 FOR UPDATE", h.FamilyID); err != nil {
		t.Fatal(err)
	}
	revoke, err := h.Runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer revoke.Rollback(context.Background())
	if err = tenant.SetLocal(ctx, revoke, tenant.Context{AccountID: h.Caregiver.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	var revokePID int
	if err = revoke.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&revokePID); err != nil {
		t.Fatal(err)
	}
	revoked := make(chan error, 1)
	go func() {
		err := NewRepository(topic).UnregisterDevice(ctx, revoke, h.Caregiver, otherInstallation, 2)
		if err == nil {
			err = revoke.Commit(ctx)
		}
		revoked <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked bool
		if err = h.Migrator.QueryRow(ctx, "SELECT $1::integer=ANY(pg_blocking_pids($2::integer))", carePID, revokePID).Scan(&blocked); err != nil {
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
	_, err = care.Exec(ctx, `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
 VALUES($1,100,'baby',$2,100,$3,$4,'update','{}',now())`, h.FamilyID, baby, h.Owner.AccountID, h.Owner.DeviceID)
	if err != nil {
		t.Fatalf("care journal aborted during concurrent unregister: %v", err)
	}
	if err = care.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-revoked; err != nil {
		t.Fatalf("unregister aborted during concurrent journal: %v", err)
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
			h := testkit.New(t)
			baby := seedBaby(t, h)
			device := deviceRequest()
			currentTopic := topic
			if _, err := register(t, h, h.Owner, device); err != nil {
				t.Fatal(err)
			}
			token := protocol.LiveActivityTokenRegistration{RequestMeta: device.RequestMeta, InstallationID: installation, ActivityID: "coordinate-activity", Token: "eeff", TokenVersion: 1, Scope: protocol.LiveActivityScope{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyID: h.FamilyID, BabyID: baby, KindRaw: "feed"}, State: "active"}
			activity := func(p access.Principal, r protocol.LiveActivityTokenRegistration) error {
				return h.Security.WithAuthorizedTransaction(t.Context(), p, h.FamilyID, "read", func(tx pgx.Tx, g access.Grant) error {
					g.MembershipRevision = 1
					_, err := NewRepository(currentTopic).RegisterActivity(t.Context(), tx, p, g, r)
					return err
				})
			}
			registerCurrent := func(p access.Principal, r protocol.LiveActivityDeviceRegistration) error {
				return h.Security.WithAuthorizedTransaction(t.Context(), p, h.FamilyID, "read", func(tx pgx.Tx, g access.Grant) error {
					g.MembershipRevision = 1
					_, err := NewRepository(currentTopic).RegisterDevice(t.Context(), tx, p, g, r)
					return err
				})
			}
			if err := activity(h.Owner, token); err != nil {
				t.Fatal(err)
			}
			device.RegistrationVersion = 2
			if change == "environment" {
				device.APNsEnvironment = "production"
			} else {
				currentTopic = topic + ".new"
			}
			if err := registerCurrent(h.Owner, device); err != nil {
				t.Fatal(err)
			}
			err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
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
			if err = activity(h.Owner, token); !errors.Is(err, ErrVersionConflict) {
				t.Fatalf("stale activity retry after coordinate change: %v", err)
			}
			// The old environment/topic claim must be released, but the new claim must
			// still reject live ownership conflicts after the update-token transition.
			token.TokenVersion = 2
			if err = activity(h.Owner, token); err != nil {
				t.Fatal(err)
			}
			if err = activity(h.Owner, token); err != nil {
				t.Fatalf("identical retry: %v", err)
			}
			var gotEnvironment, gotTopic string
			err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
				return tx.QueryRow(t.Context(), "SELECT environment,topic FROM live_activity_tokens WHERE activity_id=$1", token.ActivityID).Scan(&gotEnvironment, &gotTopic)
			})
			if err != nil || gotEnvironment != device.APNsEnvironment || gotTopic != currentTopic {
				t.Fatalf("coordinates=%s/%s err=%v", gotEnvironment, gotTopic, err)
			}
			other := device
			other.InstallationID = otherInstallation
			other.RegistrationVersion = 1
			other.PushToStartToken = token.Token
			if err = registerCurrent(h.Caregiver, other); !errors.Is(err, ErrRegistrationDenied) {
				t.Fatalf("new-coordinate token takeover: %v", err)
			}
			old := deviceRequest()
			old.InstallationID = otherInstallation
			old.PushToStartToken = token.Token
			if _, err = register(t, h, h.Caregiver, old); err != nil {
				t.Fatalf("old coordinates still owned: %v", err)
			}
		})
	}
}
