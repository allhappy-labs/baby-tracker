//go:build identitytest

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/loadseed"
	"babytracker/server/internal/loadtest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
)

// passingSummary is a run summary every latency, error and generator gate
// passes; the audit and host are what the audit command folds in.
func passingSummary() loadtest.RunSummary {
	fast := func(d time.Duration) loadtest.Histogram {
		var h loadtest.Histogram
		for i := 0; i < 1000; i++ {
			h.Record(d)
		}
		return h
	}
	routes := map[string]loadtest.RouteSummary{}
	for _, r := range []string{loadtest.RouteSyncPull, loadtest.RouteSyncWrite, loadtest.RouteRefresh, loadtest.RouteRefreshAck} {
		routes[r] = loadtest.RouteSummary{Latency: fast(40 * time.Millisecond), Outcomes: map[string]uint64{loadtest.OutcomeOK: 1000}}
	}
	return loadtest.RunSummary{Routes: routes, SchedulerLag: fast(2 * time.Millisecond)}
}

// resultJSON is result.json as the generator writes it: the summary at the
// top level plus its own keys (per-step summaries).
func resultJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(passingSummary())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["steps"] = json.RawMessage(`[{"load":1}]`)
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func reasons(v loadtest.Verdict) string { return strings.Join(v.Reasons, "; ") }

func TestFoldAuditSetsTheAuditAndHostAndKeepsTheRest(t *testing.T) {
	clean := loadtest.HostVerdict{MaxLoad1: 1.5}
	audit := loadtest.AuditResult{Acknowledged: 10, TrackedDevices: 2}
	folded, v, err := foldAudit(resultJSON(t), audit, clean, loadtest.SpecThresholds())
	if err != nil || !v.Pass {
		t.Fatalf("clean run: %+v %v", v, err)
	}
	var back struct {
		loadtest.RunSummary
		Steps []map[string]float64 `json:"steps"`
	}
	if err := json.Unmarshal(folded, &back); err != nil {
		t.Fatal(err)
	}
	if back.Audit != audit || back.Host.MaxLoad1 != 1.5 || len(back.Steps) != 1 || back.Steps[0]["load"] != 1 || len(back.Routes) != 4 {
		t.Fatalf("folded result %s", folded)
	}
	// Task 1 carry: a contaminated host fails the run.
	dirty := loadtest.HostVerdict{Contaminated: true, Offenders: []string{"swift-frontend"}}
	if _, v, err := foldAudit(resultJSON(t), audit, dirty, loadtest.SpecThresholds()); err != nil || v.Pass || !strings.Contains(reasons(v), "host contaminated: swift-frontend") {
		t.Fatalf("contaminated run: %+v %v", v, err)
	}
	for _, bad := range []loadtest.AuditResult{
		{TrackedDevices: 2},
		{Acknowledged: 10},
		{Acknowledged: 10, TrackedDevices: 2, Lost: 1},
		{Acknowledged: 10, TrackedDevices: 2, CrossFamily: 1},
		{Acknowledged: 10, TrackedDevices: 2, Diverged: 1},
		{Acknowledged: 10, TrackedDevices: 2, TrackedDiverged: 1},
	} {
		if _, v, err := foldAudit(resultJSON(t), bad, clean, loadtest.SpecThresholds()); err != nil || v.Pass {
			t.Errorf("audit %+v passed: %v", bad, err)
		}
	}
	for _, raw := range []string{`[]`, `null`, `{"fake":true}`, `{"routes":null}`, `not json`} {
		if _, _, err := foldAudit([]byte(raw), audit, clean, loadtest.SpecThresholds()); err == nil {
			t.Errorf("result %s was folded", raw)
		}
	}
}

func TestReadHostRequiresAVerdict(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	host, err := readHost(write("host.json", `{"contaminated":true,"offenders":["go test"],"details":["x"],"max_load1":4.2,"samples":6}`))
	if err != nil || !host.Contaminated || host.MaxLoad1 != 4.2 || len(host.Offenders) != 1 {
		t.Fatalf("host %+v %v", host, err)
	}
	for name, body := range map[string]string{"none.json": `{"max_load1":0.5}`, "null.json": `{"contaminated":null}`, "bad.json": `{`} {
		if _, err := readHost(write(name, body)); err == nil {
			t.Errorf("%s: read as a verdict", body)
		}
	}
	if _, err := readHost(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing host.json was read")
	}
}

func TestAuditFlags(t *testing.T) {
	full := []string{"--superuser-url-file", "u", "--ledger", "l", "--tracked", "t", "--out", "/run/load/audit.json", "--result", "r", "--host", "h"}
	f, err := parseAuditFlags(full)
	if err != nil || f.verdict != "/run/load/verdict.json" || f.thresholds != "" {
		t.Fatalf("flags %+v %v", f, err)
	}
	for i := 0; i < len(full); i += 2 {
		without := append(append([]string{}, full[:i]...), full[i+2:]...)
		if _, err := parseAuditFlags(without); !errors.Is(err, errUsage) {
			t.Errorf("without %s: %v", full[i], err)
		}
	}
}

// harnessSuperuserURL is the bootstrap superuser's URL on the harness's
// database.
func harnessSuperuserURL(t *testing.T, h *testkit.Harness) string {
	t.Helper()
	var database string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(testpg.RequireDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	return u.String()
}

func entityHash(t *testing.T, e protocol.CanonicalEntity) string {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestAuditCommandWritesTheReportTheFoldedResultAndTheVerdict(t *testing.T) {
	h := testkit.New(t)
	dir := t.TempDir()
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	path := func(name string) string { return filepath.Join(dir, name) }
	if err := os.WriteFile(filepath.Join(private, "superuser_database_url"), []byte(harnessSuperuserURL(t, h)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger, err := loadtest.CreateLedger(filepath.Join(private, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	outcomes := h.SeedBabies(t, 2)
	for _, o := range append(outcomes, outcomes[0]) { // a retried acknowledgement: 3 writes, 2 entities
		e := o.CanonicalEntity
		if err := ledger.Append(loadtest.LedgerEntry{FamilyID: h.FamilyID, EntityID: e.Meta.ID, Kind: e.EntityKind, Version: e.Meta.EntityVersion,
			WireSHA256: entityHash(t, *e)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	page := h.Pull(t, h.Owner, h.Cursor(0), nil)
	device := loadtest.TrackedDevice{FamilyID: h.FamilyID, DeviceID: h.Owner.DeviceID, Cursor: page.HighWater.Sequence, Entities: map[string]loadtest.TrackedEntity{}}
	for _, c := range page.Changes {
		e := c.SnapshotOrTombstone
		device.Entities[e.Meta.ID] = loadtest.TrackedEntity{Version: e.Meta.EntityVersion, WireSHA256: entityHash(t, e)}
	}
	if page.NextPageToken != nil || len(device.Entities) != 2 {
		t.Fatalf("pulled %d entities, more pages: %v", len(device.Entities), page.NextPageToken != nil)
	}
	if err := loadtest.WriteTracked(filepath.Join(private, "tracked.jsonl"), []loadtest.TrackedDevice{device}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path("result.json"), resultJSON(t), 0o644); err != nil {
		t.Fatal(err)
	}
	host := func(body string) {
		if err := os.WriteFile(path("host.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"--superuser-url-file", filepath.Join(private, "superuser_database_url"), "--ledger", filepath.Join(private, "ledger.jsonl"),
		"--tracked", filepath.Join(private, "tracked.jsonl"), "--out", path("audit.json"), "--result", path("result.json"), "--host", path("host.json")}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	run := func() (auditReport, loadtest.Verdict) {
		t.Helper()
		if err := auditCommand(t.Context(), args, logger); err != nil {
			t.Fatal(err)
		}
		var report auditReport
		readJSONFile(t, path("audit.json"), &report)
		var verdict loadtest.Verdict
		readJSONFile(t, path("verdict.json"), &verdict)
		return report, verdict
	}

	host(`{"contaminated":false,"offenders":[],"details":[],"max_load1":0.8,"samples":6}`)
	// M3: a database not marked as a load stack is refused before the
	// audit reads it, and nothing is written.
	if err := auditCommand(t.Context(), args, logger); err == nil || !strings.Contains(err.Error(), "not marked as a disposable load stack") {
		t.Fatalf("audited an unmarked database: %v", err)
	}
	if _, err := os.Stat(path("verdict.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused audit wrote a verdict: %v", err)
	}
	su, err := pgx.Connect(t.Context(), harnessSuperuserURL(t, h))
	if err != nil {
		t.Fatal(err)
	}
	defer su.Close(t.Context())
	var database string
	if err := su.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := su.Exec(t.Context(), `COMMENT ON DATABASE `+pgx.Identifier{database}.Sanitize()+` IS '`+loadseed.StackMarker+`'`); err != nil {
		t.Fatal(err)
	}

	report, verdict := run()
	if !verdict.Pass || report.Audit.Acknowledged != 2 || report.Audit.AcknowledgedWrites != 3 || report.Audit.TrackedDevices != 1 ||
		!report.Verdict.Pass || len(report.Findings) != 0 {
		t.Fatalf("clean run: report %+v verdict %+v", report, verdict)
	}
	if raw, err := os.ReadFile(path("result.json")); err != nil || !strings.Contains(string(raw), `"acknowledged_entities": 2`) ||
		!strings.Contains(string(raw), `"acknowledged_writes": 3`) || !strings.Contains(string(raw), `"tracked_skipped": 0`) {
		t.Fatalf("result.json does not name the audit's counts: %s %v", raw, err)
	}
	var folded struct {
		loadtest.RunSummary
		Steps []json.RawMessage `json:"steps"`
	}
	readJSONFile(t, path("result.json"), &folded)
	if folded.Audit != report.Audit || folded.Host.MaxLoad1 != 0.8 || len(folded.Steps) != 1 {
		t.Fatalf("result.json after the audit: %+v", folded)
	}

	host(`{"contaminated":true,"offenders":["xcodebuild"],"details":[],"max_load1":9,"samples":6}`)
	if _, verdict := run(); verdict.Pass || !strings.Contains(reasons(verdict), "host contaminated: xcodebuild") {
		t.Fatalf("contaminated run: %+v", verdict)
	}

	host(`{"contaminated":false,"offenders":[],"details":[],"max_load1":0.8,"samples":6}`)
	if _, err := su.Exec(t.Context(), `DELETE FROM care_entities WHERE family_id=$1 AND id=$2`, h.FamilyID, outcomes[1].CanonicalEntity.Meta.ID); err != nil {
		t.Fatal(err)
	}
	report, verdict = run()
	if verdict.Pass || report.Audit.Lost != 1 || report.Audit.TrackedDiverged != 1 || !strings.Contains(reasons(verdict), "lost 1") ||
		len(report.Findings) != 2 {
		t.Fatalf("a lost write: report %+v verdict %+v", report, verdict)
	}

	// A missing host verdict is an error, not a pass.
	if err := os.Remove(path("host.json")); err != nil {
		t.Fatal(err)
	}
	if err := auditCommand(t.Context(), args, logger); err == nil {
		t.Fatal("audited without a host verdict")
	}
}

func readJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, value); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
