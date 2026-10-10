package export_test

import (
	"errors"
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
