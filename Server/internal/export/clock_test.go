package export_test

import (
	"errors"
	"io"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/export"
	"babytracker/server/internal/testkit"
)

// The worker claims a queued job whose next_attempt_at<=now() by the
// database clock, so the job is stamped by that clock too: a service clock
// an hour ahead of the database (any skew between the node and PostgreSQL)
// must not leave a new export unclaimed for an hour.
func TestExportJobIsClaimableByTheDatabaseClock(t *testing.T) {
	h := testkit.New(t)
	s, w := newExportServicesAt(t, h, func() time.Time { return h.DatabaseNow().Add(time.Hour) })
	created, fresh, err := s.Create(t.Context(), h.Owner, request("30000000-0000-4000-8000-000000001a01"))
	if err != nil || !fresh || created.State != "queued" {
		t.Fatalf("create=%+v fresh=%v err=%v", created, fresh, err)
	}
	if err := w.RunOnce(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(t.Context(), h.Owner, created.ExportID)
	if err != nil || status.State == "queued" {
		t.Fatalf("one worker pass left the job unclaimed: status=%+v err=%v", status, err)
	}
	if status.State != "ready" {
		t.Fatalf("status=%+v, want ready", status)
	}
}

// A job's created_at, expires_at, next_attempt_at and its rate reservation's
// accepted_at come from one clock, the database's, whatever the service
// clock says (the worker's claim and expiry pass, the six-hour rate window
// and the expiry checks of Status, Create's replay and Download all read
// that clock). A service clock two days off in either direction must move
// none of them, and must not make a fresh job look expired or a lapsed job
// look fresh.
func TestExportJobTimesShareOneClock(t *testing.T) {
	for name, skew := range map[string]time.Duration{"ahead": 48 * time.Hour, "behind": -48 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			h := testkit.New(t)
			s, _ := newExportServicesAt(t, h, func() time.Time { return h.DatabaseNow().Add(skew) })
			const requestID = "30000000-0000-4000-8000-000000001c01"
			created, fresh, err := s.Create(t.Context(), h.Owner, request(requestID))
			if err != nil || !fresh {
				t.Fatalf("create=%+v fresh=%v err=%v", created, fresh, err)
			}
			var createdAt, expiresAt, nextAttempt, accepted time.Time
			var reservations int
			if err := h.Maintenance.QueryRow(t.Context(), `SELECT created_at,expires_at,next_attempt_at,
				(SELECT min(accepted_at) FROM public.export_rate_reservations WHERE request_id=$1),
				(SELECT count(*) FROM public.export_rate_reservations WHERE request_id=$1 AND accepted_at=export_jobs.created_at)
				FROM public.export_jobs WHERE id=$2`, requestID, created.ExportID).
				Scan(&createdAt, &expiresAt, &nextAttempt, &accepted, &reservations); err != nil {
				t.Fatal(err)
			}
			if !createdAt.Equal(accepted) || !createdAt.Equal(nextAttempt) || expiresAt.Sub(createdAt) != 24*time.Hour || reservations != 2 {
				t.Fatalf("created %v accepted %v next attempt %v expires %v reservations %d", createdAt, accepted, nextAttempt, expiresAt, reservations)
			}
			if drift := h.DatabaseNow().Sub(createdAt); drift < 0 || drift > time.Minute {
				t.Fatalf("created_at %v is not the database's clock (now %v)", createdAt, h.DatabaseNow())
			}
			if created.CreatedAt != createdAt.UTC().Format("2006-01-02T15:04:05.000000Z") ||
				created.ExpiresAt != expiresAt.UTC().Format("2006-01-02T15:04:05.000000Z") {
				t.Fatalf("answer %+v disagrees with the row", created)
			}
			// Expiry is the database's judgement too.
			if status, err := s.Status(t.Context(), h.Owner, created.ExportID); err != nil || status.State != "queued" {
				t.Fatalf("a fresh job under a skewed service clock: %+v %v", status, err)
			}
			if replay, fresh, err := s.Create(t.Context(), h.Owner, request(requestID)); err != nil || fresh || replay.State != "queued" {
				t.Fatalf("replay under a skewed service clock: %+v fresh=%v %v", replay, fresh, err)
			}
			if err := s.Download(t.Context(), h.Owner, created.ExportID, io.Discard); !errors.Is(err, export.ErrNotReady) {
				t.Fatalf("download of a fresh job under a skewed service clock: %v", err)
			}
			if _, err := h.Maintenance.Exec(t.Context(), `UPDATE public.export_jobs SET created_at=now()-interval '25 hours',expires_at=now()-interval '1 hour' WHERE id=$1`, created.ExportID); err != nil {
				t.Fatal(err)
			}
			if status, err := s.Status(t.Context(), h.Owner, created.ExportID); err != nil || status.State != "expired" {
				t.Fatalf("a lapsed job under a skewed service clock: %+v %v", status, err)
			}
			if err := s.Download(t.Context(), h.Owner, created.ExportID, io.Discard); !errors.Is(err, export.ErrExpired) {
				t.Fatalf("download of a lapsed job under a skewed service clock: %v", err)
			}
		})
	}
}

// The export service serves the principal's session family with no family
// read: a principal that is not a refreshed family session is
// owner_required, and one naming another family than its session's row is
// refused by the guarded transaction.
func TestExportServesTheSessionFamilyWithoutReadingIt(t *testing.T) {
	h := testkit.New(t)
	s, _ := newExportServices(t, h)
	account := h.Owner
	account.Scope = "account"
	unrefreshed := h.Owner
	unrefreshed.SessionScope, unrefreshed.SessionFamilyID = "account", ""
	for _, p := range []access.Principal{account, unrefreshed} {
		if _, _, err := s.Create(t.Context(), p, request("30000000-0000-4000-8000-000000001b01")); !errors.Is(err, export.ErrOwnerRequired) {
			t.Fatalf("scope %s/%s: %v", p.Scope, p.SessionScope, err)
		}
	}
	forged := h.Owner
	forged.SessionFamilyID = "8b000000-0000-4000-8000-000000000001"
	if _, _, err := s.Create(t.Context(), forged, request("30000000-0000-4000-8000-000000001b02")); authority.CodeOf(err) != "membership_revoked" {
		t.Fatalf("another family: %v", err)
	}
	if _, err := s.Status(t.Context(), forged, "30000000-0000-4000-8000-000000001b02"); authority.CodeOf(err) != "membership_revoked" {
		t.Fatalf("another family's status: %v", err)
	}
}
