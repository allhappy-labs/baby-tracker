package authority_test

import (
	"errors"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// liveActivityHarness is the harness on the database clock: the
// registration's lineage is checked against clock_timestamp().
func liveActivityHarness(t *testing.T) *testkit.Harness {
	t.Helper()
	h := testkit.NewAtDatabaseClock(t)
	h.Now = h.DatabaseNow()
	return h
}

// liveActivityClaim is h with the caregiver's Live Activity device
// registered and a live lease on one of its jobs. The job's not_before is
// the database's now() at the registration, so it is claimed at the
// database's clock, not the host's: a host clock behind the database's
// reads the job as not yet due.
func liveActivityClaim(t *testing.T, h *testkit.Harness) authority.LiveActivitySubject {
	t.Helper()
	h.SeedBabies(t, 1)
	digests, err := h.Auth.AccessDigests(h.AccessToken("caregiver"))
	if err != nil {
		t.Fatal(err)
	}
	device := protocol.LiveActivityDeviceRegistration{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}},
		InstallationID: "70000000-0000-4000-8000-000000000001", RegistrationVersion: 1, APNsEnvironment: "sandbox", PayloadVersion: 1, PushToStartToken: "aabb", FeedBreakMinutes: 30}
	if _, err := liveactivity.NewService(h.Authority, "com.example.babytracker.push-type.liveactivity").RegisterDevice(t.Context(), authority.Request{Digests: digests}, device); err != nil {
		t.Fatal(err)
	}
	claims, err := liveactivity.NewQueue(h.Maintenance).Claim(t.Context(), h.DatabaseNow(), 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %d %v", len(claims), err)
	}
	return liveactivity.SubjectFromClaim(claims[0])
}

// A live lease on an authorized registration runs run with the member's
// role, after exactly one round trip (BEGIN, the tenant context and the
// check, pipelined), in a read-only transaction.
func TestLiveActivitySnapshotRunsAfterOneRoundTripReadOnly(t *testing.T) {
	h := liveActivityHarness(t)
	subject := liveActivityClaim(t, h)
	pool, wire := testkit.CountedPool(t, h.Runtime)
	service := h.AuthorityOn(t, pool)
	noop := func(pgx.Tx, access.Grant) error { return nil }
	if err := service.WithLiveActivitySnapshot(t.Context(), subject, noop); err != nil { // prepares
		t.Fatal(err)
	}
	wire.Reset()
	var before int
	var grant access.Grant
	var insert error
	err := service.WithLiveActivitySnapshot(t.Context(), subject, func(tx pgx.Tx, g access.Grant) error {
		before, grant = wire.Trips(), g
		_, insert = tx.Exec(t.Context(), `INSERT INTO live_activity_tokens(registration_id,account_id,family_id,activity_id,baby_id,kind,environment,topic,token_version,state)
			VALUES($1,$2,$3,'x',$4,'feed','sandbox','t',1,'active')`, subject.RegistrationID, subject.AccountID, subject.FamilyID, subject.BabyID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatalf("%d round trips before run, want 1 (statements %q)", before, wire.Statements())
	}
	if grant != (access.Grant{AccountID: h.Caregiver.AccountID, FamilyID: h.FamilyID, Role: "caregiver"}) {
		t.Fatalf("grant %+v", grant)
	}
	var pgErr *pgconn.PgError
	if !errors.As(insert, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("an INSERT in the snapshot: %v, want 25006", insert)
	}
	if _, err := h.Family.ChangeRole(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := service.WithLiveActivitySnapshot(t.Context(), subject, func(_ pgx.Tx, g access.Grant) error { grant = g; return nil }); err != nil || grant.Role != "viewer" {
		t.Fatalf("after a role change: %+v %v", grant, err)
	}
}

// A stale lease, a revoked session lineage, a lapsed hosted entitlement or a
// removed member is ErrLiveActivityAuthorizationChanged, and run never runs.
func TestLiveActivitySnapshotRefusesChangedAuthorization(t *testing.T) {
	for _, change := range []string{"stale_lease", "expired_lease", "revoked_lineage", "lapsed_entitlement", "removed_member", "other_family"} {
		t.Run(change, func(t *testing.T) {
			h := liveActivityHarness(t)
			subject := liveActivityClaim(t, h)
			service := h.Authority
			switch change {
			case "stale_lease":
				subject.LeaseID = "70000000-0000-4000-8000-0000000000ff"
			case "expired_lease":
				h.AuthorityExec(t, `UPDATE live_activity_delivery SET lease_deadline=clock_timestamp()-interval '1 second' WHERE job_id=$1`, subject.JobID)
			case "revoked_lineage":
				if _, err := h.Auth.Logout(t.Context(), h.Caregiver, h.CommandID()); err != nil {
					t.Fatal(err)
				}
			case "lapsed_entitlement":
				service = h.AuthorityOnWith(t, h.Runtime, func(o *authority.Options) { o.RequireFamilySyncEntitlement = true })
				h.AuthorityExec(t, `INSERT INTO family_entitlements(family_id,entitlement_key,source,status,expires_at,created_at,updated_at) VALUES ($1,'family_sync','test','active',clock_timestamp()+interval '1 hour',now(),now())`, h.FamilyID)
				if err := service.WithLiveActivitySnapshot(t.Context(), subject, func(pgx.Tx, access.Grant) error { return nil }); err != nil {
					t.Fatalf("an entitled hosted family refused: %v", err)
				}
				h.AuthorityExec(t, `UPDATE family_entitlements SET expires_at=clock_timestamp()-interval '1 second' WHERE family_id=$1`, h.FamilyID)
			case "removed_member":
				if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
					t.Fatal(err)
				}
			case "other_family":
				subject.FamilyID = "f0000000-0000-4000-8000-0000000000f9"
			}
			ran := false
			err := service.WithLiveActivitySnapshot(t.Context(), subject, func(pgx.Tx, access.Grant) error { ran = true; return nil })
			if !errors.Is(err, authority.ErrLiveActivityAuthorizationChanged) || ran {
				t.Fatalf("%s: err %v, ran %v", change, err, ran)
			}
		})
	}
	h := testkit.New(t)
	if err := h.Authority.WithLiveActivitySnapshot(t.Context(), authority.LiveActivitySubject{}, func(pgx.Tx, access.Grant) error { return nil }); !errors.Is(err, authority.ErrInvalidCommand) {
		t.Fatalf("an empty subject: %v", err)
	}
}
