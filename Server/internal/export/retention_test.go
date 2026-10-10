package export_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/export"
	"babytracker/server/internal/testkit"
)

// insertJob writes an export job of the harness family requested by the
// owner, in state, expiring at expires, with one chunk of attempt 1 when it
// has material (the chunk as the worker writes it, as the maintenance
// login).
func insertJob(t *testing.T, h *testkit.Harness, id, state string, expires time.Time, chunk bool) {
	t.Helper()
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, err := tx.Exec(t.Context(), `INSERT INTO public.export_jobs(id, family_id, requester_account_id, request_id, server_instance_id,
			recovery_epoch, security_revision, state, created_at, expires_at, next_attempt_at, attempt)
		VALUES ($1, $2, $3, $1, $4, 1, 1, $5, $6::timestamptz - interval '24 hours', $6, $6::timestamptz - interval '24 hours', 1)`,
		id, h.FamilyID, h.Owner.AccountID, testkit.InstanceID, state, expires); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if chunk {
		if _, err := h.Maintenance.Exec(t.Context(), `INSERT INTO public.export_chunks(family_id, export_id, attempt, chunk_index, plaintext_length, is_final, ciphertext)
			VALUES ($1, $2, 1, 0, 0, true, '\x00000000000000000000000000000000'::bytea)`, h.FamilyID, id); err != nil {
			t.Fatal(err)
		}
	}
}

// jobRows counts the export jobs and chunks of id as the maintenance login.
func jobRows(t *testing.T, h *testkit.Harness, id string) (jobs, chunks int) {
	t.Helper()
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM public.export_jobs WHERE id = $1),
		(SELECT count(*) FROM public.export_chunks WHERE export_id = $1)`, id).Scan(&jobs, &chunks); err != nil {
		t.Fatal(err)
	}
	return jobs, chunks
}

// An expired or failed export job is deleted, with its chunks, by the
// worker's expiry pass once it is more than 7 days past expires_at (the
// database clock that the export queue runs on); one less old, and a ready
// job, are kept.
func TestExpiredExportJobsAreDeletedAfterSevenDays(t *testing.T) {
	h := testkit.New(t)
	s, w := newExportServices(t, h)
	now := h.DatabaseNow()
	day := 24 * time.Hour
	jobs := []struct {
		id, state string
		age       time.Duration // past expires_at
		deleted   bool
	}{
		{"30000000-0000-4000-8000-000000002001", "expired", 8 * day, true},
		{"30000000-0000-4000-8000-000000002002", "failed", 8 * day, true},
		{"30000000-0000-4000-8000-000000002003", "expired", 7*day + time.Minute, true},
		{"30000000-0000-4000-8000-000000002004", "expired", 6 * day, false},
		{"30000000-0000-4000-8000-000000002005", "failed", 7*day - time.Minute, false},
	}
	for _, job := range jobs {
		insertJob(t, h, job.id, job.state, now.Add(-job.age), true)
	}
	created, fresh, err := s.Create(t.Context(), h.Owner, request("30000000-0000-4000-8000-000000002101"))
	if err != nil || !fresh {
		t.Fatalf("create=%+v fresh=%v err=%v", created, fresh, err)
	}
	if err := w.RunOnce(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		n, chunks := jobRows(t, h, job.id)
		if job.deleted && (n != 0 || chunks != 0) {
			t.Fatalf("%s job %v past expiry: %d rows, %d chunks left", job.state, job.age, n, chunks)
		}
		if !job.deleted && n != 1 {
			t.Fatalf("%s job %v past expiry was deleted", job.state, job.age)
		}
	}
	if ready, err := s.Status(t.Context(), h.Owner, created.ExportID); err != nil || ready.State != "ready" {
		t.Fatalf("the ready job: %+v %v", ready, err)
	}
	if n, _ := jobRows(t, h, created.ExportID); n != 1 {
		t.Fatal("the ready job was deleted")
	}
}

// The retired-job selection reads export_jobs_expiry_idx, with no
// sequential scan of export_jobs, on 20,000 jobs (1% retired), under custom
// and generic plans.
func TestRetiredExportJobSelectionUsesTheExpiryIndex(t *testing.T) {
	h := testkit.New(t)
	now := h.DatabaseNow()
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, err := tx.Exec(t.Context(), `INSERT INTO public.export_jobs(id, family_id, requester_account_id, request_id, server_instance_id,
			recovery_epoch, security_revision, state, created_at, expires_at, next_attempt_at, attempt)
		SELECT md5('j'||i)::uuid, $1, $2, md5('j'||i)::uuid, $3, 1, 1, CASE WHEN i % 2 = 0 THEN 'expired' ELSE 'failed' END,
			e - interval '24 hours', e, e - interval '24 hours', 1
		FROM generate_series(1, 20000) i,
			LATERAL (SELECT $4::timestamptz - CASE WHEN i % 100 = 0 THEN interval '8 days' ELSE interval '5 days' END - i * interval '1 second') x(e)`,
		h.FamilyID, h.Owner.AccountID, testkit.InstanceID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Migrator.Exec(t.Context(), `ANALYZE public.export_jobs`); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		t.Run(mode, func(t *testing.T) {
			plan, err := h.Maintenance.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = plan.Rollback(t.Context()) }()
			if _, err := plan.Exec(t.Context(), `SELECT set_config('plan_cache_mode', $1, true)`, mode); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := plan.QueryRow(t.Context(), `EXPLAIN (FORMAT JSON) `+export.SQLRetiredJobs).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct{ Plan exportPlanNode }
			if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
				t.Fatalf("plan %s: %v", raw, err)
			}
			usesIndex := false
			plans[0].Plan.walk(func(n exportPlanNode) {
				if n.RelationName == "export_jobs" && n.NodeType == "Seq Scan" {
					t.Fatalf("a sequential scan of export_jobs: %s", raw)
				}
				usesIndex = usesIndex || n.IndexName == "export_jobs_expiry_idx"
			})
			if !usesIndex {
				t.Fatalf("the plan does not read export_jobs_expiry_idx: %s", raw)
			}
		})
	}
	var retired int
	if err := h.Maintenance.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM (%s) r`, export.SQLRetiredJobs)).Scan(&retired); err != nil || retired != 100 {
		t.Fatalf("the selection takes %d (%v), want its bound of 100 of the 200 retired", retired, err)
	}
}

type exportPlanNode struct {
	NodeType     string           `json:"Node Type"`
	RelationName string           `json:"Relation Name"`
	IndexName    string           `json:"Index Name"`
	Plans        []exportPlanNode `json:"Plans"`
}

func (n exportPlanNode) walk(visit func(exportPlanNode)) {
	visit(n)
	for _, child := range n.Plans {
		child.walk(visit)
	}
}
