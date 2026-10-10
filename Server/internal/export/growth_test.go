package export_test

import (
	"babytracker/server/internal/export"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestGrowthOwnerArchiveRawAndTombstones(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		deleted  bool
		deferred bool
	}{{name: "raw"}, {name: "tombstones", deleted: true}, {name: "delayed_raw", deferred: true}} {
		t.Run(scenario.name, func(t *testing.T) {
			h := testkit.New(t)
			for _, f := range []string{"baby.json", "growth-profile.json", "measurement.json"} {
				out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, f))
				if err != nil || out.Status != "applied" {
					t.Fatalf("seed %+v %v", out, err)
				}
			}
			if scenario.deleted {
				m := testkit.MustMutation(t, "baby.json")
				m.MutationID, m.Operation, m.Payload = h.CommandID(), "delete", protocol.DeletePayload{}
				if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil {
					t.Fatal(err)
				}
			}
			s, w := newExportServices(t, h)
			created, _, err := s.Create(t.Context(), h.Owner, request(h.CommandID()))
			if err != nil {
				t.Fatal(err)
			}
			if scenario.deferred {
				changed, err := h.Maintenance.Exec(t.Context(), `UPDATE public.export_jobs
					SET next_attempt_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, created.ExportID)
				if err != nil || changed.RowsAffected() != 1 {
					t.Fatalf("schedule rows=%d err=%v", changed.RowsAffected(), err)
				}
				if err := w.RunOnce(t.Context(), 1); err != nil {
					t.Fatal(err)
				}
				status, err := s.Status(t.Context(), h.Owner, created.ExportID)
				var attempt int
				if err != nil || status.State != "queued" {
					t.Fatalf("deferred status=%s err=%v", status.State, err)
				}
				if err := h.Maintenance.QueryRow(t.Context(), `SELECT attempt FROM public.export_jobs WHERE id=$1`, created.ExportID).Scan(&attempt); err != nil || attempt != 0 {
					t.Fatalf("deferred attempt=%d err=%v", attempt, err)
				}
				changed, err = h.Maintenance.Exec(t.Context(), `UPDATE public.export_jobs
					SET next_attempt_at=clock_timestamp()+interval '300 milliseconds' WHERE id=$1`, created.ExportID)
				if err != nil || changed.RowsAffected() != 1 {
					t.Fatalf("reschedule rows=%d err=%v", changed.RowsAffected(), err)
				}
				t.Log("growth archive delayed-queue proof: single worker pass kept job queued with attempt=0")
			}
			waitForGrowthExportReady(t, h, s, w, created.ExportID)
			var raw bytes.Buffer
			if err := s.Download(t.Context(), h.Owner, created.ExportID, &raw); err != nil {
				t.Fatal(err)
			}
			var archive map[string]json.RawMessage
			if err := json.Unmarshal(raw.Bytes(), &archive); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"growth_profiles", "measurements"} {
				var entities []protocol.CanonicalEntity
				if err := json.Unmarshal(archive[name], &entities); err != nil {
					t.Fatalf("missing %s: %v", name, err)
				}
				if len(entities) != 1 || (entities[0].Meta.TombstonedAt != nil) != scenario.deleted || (entities[0].Payload == nil) != scenario.deleted {
					t.Fatalf("%s %+v", name, entities)
				}
				if !scenario.deleted {
					want := testkit.MustMutation(t, map[string]string{"growth_profiles": "growth-profile.json", "measurements": "measurement.json"}[name])
					got, _ := json.Marshal(entities[0].Payload)
					expected, _ := json.Marshal(want.Payload)
					if string(got) != string(expected) {
						t.Fatalf("raw payload %s != %s", got, expected)
					}
				}
			}
			if strings.Contains(raw.String(), "percentile") {
				t.Fatal("derived percentile exported")
			}
			if err := s.Download(t.Context(), h.Caregiver, created.ExportID, &bytes.Buffer{}); err == nil {
				t.Fatal("caregiver downloaded growth archive")
			}
		})
	}
}

// A successful worker pass can claim no job before its due time. Wait for the
// requested export's durable ready state before exercising download semantics.
func waitForGrowthExportReady(t *testing.T, h *testkit.Harness, service *export.Service, worker *export.Worker, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	fail := func(cause error, status protocol.ExportStatus) {
		t.Helper()
		diagnosticCtx, diagnosticCancel := context.WithTimeout(t.Context(), time.Second)
		defer diagnosticCancel()
		var state string
		var attempt int
		var claimDelayMS float64
		var jobCode *string
		diagnosticErr := h.Maintenance.QueryRow(diagnosticCtx, `SELECT state,attempt,last_code,
			EXTRACT(EPOCH FROM(next_attempt_at-clock_timestamp()))::double precision*1000
			FROM public.export_jobs WHERE id=$1`, id).Scan(&state, &attempt, &jobCode, &claimDelayMS)
		code := ""
		if jobCode != nil {
			code = *jobCode
		}
		t.Fatalf("export readiness: err=%v status=%s job_state=%s code=%s attempt=%d claim_delay_ms=%f diagnosticErr=%v",
			cause, status.State, state, code, attempt, claimDelayMS, diagnosticErr)
	}
	var status protocol.ExportStatus
	for {
		if err := worker.RunOnce(ctx, 1); err != nil {
			fail(err, status)
		}
		var err error
		status, err = service.Status(ctx, h.Owner, id)
		if err != nil {
			fail(err, status)
		}
		switch status.State {
		case "ready":
			return
		case "queued", "running":
		default:
			fail(fmt.Errorf("unexpected export state %q", status.State), status)
		}
		select {
		case <-ctx.Done():
			fail(ctx.Err(), status)
		case <-ticker.C:
		}
	}
}
