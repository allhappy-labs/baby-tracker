package liveactivity

import (
	"context"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/family"
	"babytracker/server/internal/security"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

type familyResolver struct{ service *family.Service }

func (r familyResolver) Current(ctx context.Context, p access.Principal) (string, error) {
	v, e := r.service.Current(ctx, p)
	return v.ID, e
}

func TestLiveActivityAuthorityLifecycle(t *testing.T) {
	for _, change := range []string{"refresh", "logout", "expiry", "removed_rejoined", "deleted", "viewer", "epoch"} {
		t.Run(change, func(t *testing.T) {
			h := testkit.New(t)
			seedBaby(t, h)
			service := NewService(h.Security, familyResolver{h.Family}, topic)
			if _, err := service.RegisterDevice(t.Context(), h.Caregiver, deviceRequest()); err != nil {
				t.Fatal(err)
			}
			q := NewQueue(h.LiveActivity)
			claims, err := q.Claim(t.Context(), time.Now(), 10)
			if err != nil || len(claims) == 0 {
				t.Fatalf("claim %d %v", len(claims), err)
			}
			guard, err := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			subject := SubjectFromClaim(claims[0])
			changed := false
			check := func() error {
				return guard.WithLiveActivitySnapshot(t.Context(), subject, func(tx pgx.Tx, g access.Grant) error {
					if g.MembershipRevision == 0 {
						t.Error("missing membership lifetime")
					}
					if changed && change == "viewer" && access.RoleMayWrite(g.Role) {
						t.Error("viewer gained writes")
					}
					var n int
					return tx.QueryRow(t.Context(), `SELECT count(*) FROM babies WHERE family_id=$1`, h.FamilyID).Scan(&n)
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
				h.Now = h.Now.Add(400 * 24 * time.Hour)
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
				_, err = (&account.Service{Security: h.Security}).Delete(t.Context(), h.Caregiver, h.CommandID(), true)
				if err != nil {
					t.Fatal(err)
				}
			case "viewer":
				_, err = h.Family.ChangeRole(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer")
			case "epoch":
				_, err = h.Migrator.Exec(t.Context(), `UPDATE server_state SET recovery_epoch=recovery_epoch+1`)
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
			if !allowed && err == nil {
				t.Fatal("stale authority accepted")
			}
			if !allowed {
				if _, err = guard.SweepLiveActivityRegistrations(t.Context()); err != nil {
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
	h := testkit.New(t)
	seedBaby(t, h)
	service := NewService(h.Security, familyResolver{h.Family}, topic)
	if _, err := service.RegisterDevice(t.Context(), h.Owner, deviceRequest()); err != nil {
		t.Fatal(err)
	}
	claims, err := NewQueue(h.LiveActivity).Claim(t.Context(), time.Now(), 10)
	if err != nil || len(claims) == 0 {
		t.Fatal(err)
	}
	guard, err := security.NewLiveActivitySnapshotGuard(h.Security, h.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	subject := SubjectFromClaim(claims[0])
	var escaped pgx.Tx
	if err = guard.WithLiveActivitySnapshot(t.Context(), subject, func(tx pgx.Tx, g access.Grant) error {
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
	for _, field := range []string{"lease", "version", "account", "device", "family", "baby", "kind", "dirty", "deadline", "session"} {
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
			case "session":
				bad.SessionID = h.Caregiver.SessionID
			}
			if err := guard.WithLiveActivitySnapshot(t.Context(), bad, func(pgx.Tx, access.Grant) error { t.Error("forged claim authorized"); return nil }); err == nil {
				t.Fatal("forged claim accepted")
			}
		})
	}
	if _, err = h.Migrator.Exec(t.Context(), `UPDATE live_activity_delivery SET lease_deadline=clock_timestamp()-interval '1 second' WHERE job_id=$1`, subject.JobID); err != nil {
		t.Fatal(err)
	}
	if err = guard.WithLiveActivitySnapshot(t.Context(), subject, func(pgx.Tx, access.Grant) error { t.Error("expired lease authorized"); return nil }); err == nil {
		t.Fatal("expired lease accepted")
	}
}
