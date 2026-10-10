package liveactivity

import (
	"context"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/authority"
	"github.com/jackc/pgx/v5"
)

// main's authorization tests on the authority snapshot. main's membership
// revision is the session lineage (app.live_activity_authorized): a removal
// revokes the member's sessions, and a rejoin starts a new lineage. The
// guard's registration sweep is the worker's (Queue.SweepRegistrations).

func TestLiveActivityAuthorityLifecycle(t *testing.T) {
	for _, change := range []string{"refresh", "logout", "expiry", "removed_rejoined", "deleted", "viewer", "epoch"} {
		t.Run(change, func(t *testing.T) {
			h, _, reg := workerHarness(t)
			workerBaby(t, h)
			if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
				t.Fatal(err)
			}
			q := NewQueue(h.Maintenance)
			claims, err := q.Claim(t.Context(), h.DatabaseNow(), 10)
			if err != nil || len(claims) == 0 {
				t.Fatalf("claim %d %v", len(claims), err)
			}
			subject := SubjectFromClaim(claims[0])
			changed := false
			check := func() error {
				return h.Authority.WithLiveActivitySnapshot(t.Context(), subject, func(tx pgx.Tx, g access.Grant) error {
					if g.Role == "" {
						t.Error("missing member role")
					}
					if changed && change == "viewer" && access.RoleMayWrite(g.Role) {
						t.Error("viewer gained writes")
					}
					var n int
					return tx.QueryRow(t.Context(), `SELECT count(*) FROM care_entities WHERE family_id=$1 AND kind=1`, h.FamilyID).Scan(&n)
				})
			}
			if err = check(); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "refresh":
				h.Now = h.Now.Add(20 * time.Minute)
				h.Caregiver = h.Refresh("caregiver")
			case "logout":
				_, err = h.Auth.Logout(t.Context(), h.Caregiver, h.CommandID())
			case "expiry":
				// The registration's lineage deadline passes (the database
				// clock decides; main moved its service clock).
				tenantExec(t, h, h.Caregiver.AccountID, `UPDATE live_activity_devices SET authority_expires_at=clock_timestamp()-interval '1 second'`)
			case "removed_rejoined":
				_, err = h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID)
				if err != nil {
					t.Fatal(err)
				}
				h.Caregiver, err = h.Reauthenticate("caregiver")
				if err != nil {
					t.Fatal(err)
				}
				invite, e := h.Family.Invite(t.Context(), h.Owner, h.CommandID(), "caregiver")
				if e != nil {
					t.Fatal(e)
				}
				_, err = h.Family.Redeem(t.Context(), h.Caregiver, h.CommandID(), invite.Token, true)
			case "deleted":
				_, err = (&account.Service{Authority: h.Authority}).Delete(t.Context(), h.Caregiver, h.CommandID(), true)
			case "viewer":
				_, err = h.Family.ChangeRole(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer")
			case "epoch":
				h.AuthorityExec(t, `UPDATE server_state SET recovery_epoch=recovery_epoch+1`)
			}
			if err != nil {
				t.Fatal(err)
			}
			changed = true
			err = check()
			allowed := change == "refresh" || change == "viewer"
			if allowed && err != nil {
				t.Fatalf("valid authority rejected: %v", err)
			}
			if !allowed && !errors.Is(err, authority.ErrLiveActivityAuthorizationChanged) {
				t.Fatalf("stale authority accepted: %v", err)
			}
			if !allowed {
				// The worker's minute sweep (and, after an epoch change, the
				// claim's own retirement of the old realm).
				if _, err = q.SweepRegistrations(t.Context(), false); err != nil {
					t.Fatal(err)
				}
				if _, err = q.Claim(t.Context(), h.DatabaseNow(), 10); err != nil {
					t.Fatal(err)
				}
				if n := scalar(t, h, `SELECT count(*) FROM live_activity_registration_index WHERE NOT revoked`); n != 0 {
					t.Fatalf("sweep left %d live registrations", n)
				}
			}
		})
	}
}

func TestLiveActivitySnapshotLeaseAndRelease(t *testing.T) {
	h, _, reg := workerHarness(t)
	workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("owner"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	claims, err := NewQueue(h.Maintenance).Claim(t.Context(), h.DatabaseNow(), 10)
	if err != nil || len(claims) == 0 {
		t.Fatal(err)
	}
	subject := SubjectFromClaim(claims[0])
	var escaped pgx.Tx
	if err = h.Authority.WithLiveActivitySnapshot(t.Context(), subject, func(tx pgx.Tx, g access.Grant) error {
		escaped = tx
		var readonly string
		if err := tx.QueryRow(t.Context(), `SHOW transaction_read_only`).Scan(&readonly); err != nil {
			return err
		}
		if readonly != "on" {
			t.Error("mutable snapshot")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = escaped.Exec(t.Context(), `SELECT 1`); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("snapshot transaction retained: %v", err)
	}
	// A subsequent authority write must not wait for a leaked read fence.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err = h.Family.ChangeRole(ctx, h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer"); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"lease", "version", "account", "device", "family", "baby", "kind", "dirty", "deadline"} {
		t.Run(field, func(t *testing.T) {
			bad := subject
			switch field {
			case "lease":
				bad.LeaseID = otherInstallation
			case "version":
				bad.RegistrationVersion++
			case "account":
				bad.AccountID = h.Caregiver.AccountID
			case "device":
				bad.DeviceID = h.Caregiver.DeviceID
			case "family":
				bad.FamilyID = otherInstallation
			case "baby":
				bad.BabyID = otherInstallation
			case "kind":
				if bad.Kind == "feed" {
					bad.Kind = "sleep"
				} else {
					bad.Kind = "feed"
				}
			case "dirty":
				bad.DirtyVersion++
			case "deadline":
				bad.LeaseDeadline = bad.LeaseDeadline.Add(time.Second)
			}
			if err := h.Authority.WithLiveActivitySnapshot(t.Context(), bad, func(pgx.Tx, access.Grant) error { t.Error("forged claim authorized"); return nil }); err == nil {
				t.Fatal("forged claim accepted")
			}
		})
	}
	if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=clock_timestamp()-interval '1 second' WHERE job_id=$1`, subject.JobID); err != nil {
		t.Fatal(err)
	}
	if err = h.Authority.WithLiveActivitySnapshot(t.Context(), subject, func(pgx.Tx, access.Grant) error { t.Error("expired lease authorized"); return nil }); err == nil {
		t.Fatal("expired lease accepted")
	}
}
