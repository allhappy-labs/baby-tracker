package export_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/export"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

func newExportServices(t *testing.T, h *testkit.Harness) (*export.Service, *export.Worker) {
	t.Helper()
	return newExportServicesAt(t, h, h.DatabaseNow)
}

// newExportServicesAt is newExportServices on the service clock now.
func newExportServicesAt(t *testing.T, h *testkit.Harness, now func() time.Time) (*export.Service, *export.Worker) {
	t.Helper()
	var key [32]byte
	for i := range key {
		key[i] = 0x5a
	}
	s, w, err := export.New(export.Options{Authority: h.Authority, Identity: h.Info,
		Runtime: h.Runtime, WorkerPool: h.Maintenance, Keys: map[string][32]byte{"test.v1": key}, ActiveKeyID: "test.v1", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return s, w
}

func request(id string) protocol.CreateExportRequest {
	return protocol.CreateExportRequest{RequestMeta: protocol.RequestMeta{
		ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityExportV1}}, RequestID: id}
}

func TestOwnerExportCreateWorkerDownloadAndExpiry(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "feed-bottle.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "supplement.json")); err != nil {
		t.Fatal(err)
	}
	s, w := newExportServices(t, h)
	id := "30000000-0000-4000-8000-000000001901"
	created, fresh, err := s.Create(t.Context(), h.Owner, request(id))
	if err != nil || !fresh || created.State != "queued" {
		t.Fatalf("create=%+v fresh=%v err=%v", created, fresh, err)
	}
	replayed, fresh, err := s.Create(t.Context(), h.Owner, request(id))
	if err != nil || fresh || replayed.ExportID != created.ExportID {
		t.Fatalf("replay=%+v fresh=%v err=%v", replayed, fresh, err)
	}
	if _, _, err := s.Create(t.Context(), h.Caregiver, request("30000000-0000-4000-8000-000000001902")); err == nil {
		t.Fatal("caregiver started export")
	}
	if _, _, err := s.Create(t.Context(), h.Owner, request("30000000-0000-4000-8000-000000001903")); !errors.Is(err, export.ErrInProgress) {
		t.Fatalf("second export error=%v", err)
	}
	if err := w.RunOnce(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	ready, err := s.Status(t.Context(), h.Owner, created.ExportID)
	if err != nil || ready.State != "ready" || ready.SHA256 == nil {
		code := ""
		if ready.Code != nil {
			code = *ready.Code
		}
		t.Fatalf("status=%+v code=%s err=%v", ready, code, err)
	}
	var raw bytes.Buffer
	if err := s.Download(t.Context(), h.Owner, created.ExportID, &raw); err != nil {
		t.Fatal(err)
	}
	var archive export.ArchiveV1
	if err := json.Unmarshal(raw.Bytes(), &archive); err != nil {
		t.Fatal(err)
	}
	if archive.FamilyID != h.FamilyID || len(archive.Babies) != 1 || len(archive.Feeds) != 1 || len(archive.Supplements) != 1 || len(archive.Journal) < 3 {
		t.Fatalf("archive family=%s babies=%d feeds=%d supplements=%d journal=%d", archive.FamilyID, len(archive.Babies), len(archive.Feeds), len(archive.Supplements), len(archive.Journal))
	}
	for _, event := range archive.Audit {
		at, err := time.Parse("2006-01-02T15:04:05.000000Z", event.At)
		if err != nil || at.Format("2006-01-02T15:04:05.000000Z") != event.At {
			t.Fatalf("noncanonical audit time %q: %v", event.At, err)
		}
	}
	// The audit section is the family's own chain, seq 1 through the family
	// audit seq the archive was captured at, with no gap: family_created,
	// then the caregiver's invitation and its redemption.
	revision, err := h.Authority.FamilyRevision(t.Context(), h.FamilyID)
	if err != nil || archive.SecurityRevision != revision || uint64(len(archive.Audit)) != revision || len(archive.Audit) < 3 {
		t.Fatalf("archive revision=%d audit=%d family revision=%d err=%v", archive.SecurityRevision, len(archive.Audit), revision, err)
	}
	for index, event := range archive.Audit {
		if event.Revision != uint64(index+1) || event.Kind == "" || event.ActorAccountID == "" {
			t.Fatalf("audit[%d]=%+v", index, event)
		}
	}
	if archive.Audit[0].Kind != "family_created" || archive.Audit[0].ActorAccountID != h.Owner.AccountID {
		t.Fatalf("first audit event=%+v", archive.Audit[0])
	}
	if err := s.Download(t.Context(), h.Caregiver, created.ExportID, &bytes.Buffer{}); err == nil {
		t.Fatal("caregiver downloaded export")
	}
	if _, err := h.Maintenance.Exec(t.Context(), `UPDATE public.export_jobs SET recovery_epoch=recovery_epoch+1 WHERE id=$1`, created.ExportID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Status(t.Context(), h.Owner, created.ExportID); !errors.Is(err, export.ErrAuthorizationChanged) {
		t.Fatalf("stale epoch status=%v", err)
	}
	if _, _, err := s.Create(t.Context(), h.Owner, request(id)); !errors.Is(err, export.ErrAuthorizationChanged) {
		t.Fatalf("stale epoch replay=%v", err)
	}
	if err := s.Download(t.Context(), h.Owner, created.ExportID, &bytes.Buffer{}); !errors.Is(err, export.ErrAuthorizationChanged) {
		t.Fatalf("stale epoch download=%v", err)
	}
	if _, err := h.Maintenance.Exec(t.Context(), `UPDATE public.export_jobs SET recovery_epoch=recovery_epoch-1 WHERE id=$1`, created.ExportID); err != nil {
		t.Fatal(err)
	}
	// Expiry erases chunks and wrapped key; the idempotency receipt survives.
	if _, err := h.Maintenance.Exec(t.Context(), `UPDATE public.export_jobs SET created_at=now()-interval '25 hours',expires_at=now()-interval '1 hour' WHERE id=$1`, created.ExportID); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	expired, err := s.Status(t.Context(), h.Owner, created.ExportID)
	if err != nil || expired.State != "expired" {
		t.Fatalf("expired=%+v err=%v", expired, err)
	}
	if err := s.Download(t.Context(), h.Owner, created.ExportID, &bytes.Buffer{}); !errors.Is(err, export.ErrExpired) {
		t.Fatalf("expired download=%v", err)
	}
	var chunks int
	var header []byte
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM public.export_chunks WHERE export_id=$1),header FROM public.export_jobs WHERE id=$1`, created.ExportID).Scan(&chunks, &header); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 || header != nil {
		t.Fatalf("expired material remains: chunks=%d header=%v", chunks, header != nil)
	}
}

func TestWorkerInvalidatesReadyArchiveWhenRequesterIsNoLongerOwner(t *testing.T) {
	h := testkit.NewHosted(t)
	s, w := newExportServices(t, h)
	created, _, err := s.Create(t.Context(), h.Owner, request("30000000-0000-4000-8000-000000001904"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale job left after an authority change. Request access is
	// immediately scoped to its original requester; reconciliation erases it.
	if _, err := h.Maintenance.Exec(t.Context(), `UPDATE public.export_jobs SET requester_account_id=$2 WHERE id=$1`,
		created.ExportID, h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := s.Download(t.Context(), h.Owner, created.ExportID, &bytes.Buffer{}); !errors.Is(err, export.ErrNotFound) {
		t.Fatalf("old requester download=%v", err)
	}
	if err := w.RunOnce(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	var state string
	var header []byte
	var chunks int
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT state,header,(SELECT count(*) FROM public.export_chunks WHERE export_id=$1)
		FROM public.export_jobs WHERE id=$1`, created.ExportID).Scan(&state, &header, &chunks); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || header != nil || chunks != 0 {
		t.Fatalf("material survives transfer: state=%s header=%v chunks=%d", state, header != nil, chunks)
	}
}

// An ownership change committed after the export was requested fails the
// job before anything is read: the worker rechecks the job's subject against
// the authority, not the request's principal.
func TestWorkerFailsQueuedExportAfterOwnershipTransfer(t *testing.T) {
	h := testkit.New(t)
	s, w := newExportServices(t, h)
	created, _, err := s.Create(t.Context(), h.Owner, request("30000000-0000-4000-8000-000000001905"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Family.Transfer(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	var state, code string
	var header []byte
	var chunks int
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT state,coalesce(last_code,''),header,(SELECT count(*) FROM public.export_chunks WHERE export_id=$1)
		FROM public.export_jobs WHERE id=$1`, created.ExportID).Scan(&state, &code, &header, &chunks); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code != "export_authorization_changed" || header != nil || chunks != 0 {
		t.Fatalf("job after transfer: state=%s code=%s header=%v chunks=%d", state, code, header != nil, chunks)
	}
}
