package capacity

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCellRequiresMeasurementAndSamples(t *testing.T) {
	for _, c := range []struct {
		name       string
		seconds    float64
		successes  int
		completion string
	}{
		{"zero measurement", 0, 240, "complete"}, {"few samples", 60, 199, "complete"}, {"incomplete success", 60, 240, "interrupted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			parent, _ := filepath.EvalSymlinks(t.TempDir())
			path := filepath.Join(parent, "cell.json")
			samples := make([]float64, c.successes)
			for i := range samples {
				samples[i] = .1
			}
			raw, _ := json.Marshal(map[string]any{"measurement_seconds": c.seconds, "successes": c.successes, "refusals": 2, "unexpected_errors": 3, "completion": c.completion, "latency_seconds": samples})
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("go", "run", "-tags", "identitytest", "../../cmd/sync-capacity-client", "report", "--input", path)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("report phase missing: %v; stderr=%s", err, func() []byte {
					if e, ok := err.(*exec.ExitError); ok {
						return e.Stderr
					}
					return nil
				}())
			}
			var result map[string]any
			if json.Unmarshal(out, &result) != nil {
				t.Fatal("invalid report")
			}
			if result["percentiles"] != nil || result["passing"] != false || result["refusals"] != float64(2) || result["unexpected_errors"] != float64(3) {
				t.Fatalf("dishonest report: %s", out)
			}
		})
	}
}

func coherentCell() CellResult {
	samples := make([]float64, 240)
	for i := range samples {
		samples[i] = .01
	}
	return CellResult{RefusalClassification: refusalClassification, Version: 1, Requested: Workload{Devices: 32, ActiveWriters: 16, WritesPerWriterPerMinute: 15, Warmup: 30 * time.Second, Measure: 60 * time.Second, EmptyPullInterval: 300 * time.Second}, ActualDevices: 32, ActualWriters: 16, PlannedWrites: 240, ScheduledWrites: 240, Counts: Counts{Attempts: 240, Successes: 240}, PeerPulls: Counts{Attempts: 240, Successes: 240}, MeasurementSeconds: 60, Acknowledged: 240, PeerApplied: 240, LatencySeconds: samples, PeerHTTPSeconds: samples, Completion: Complete, ResourcesEnforced: true, Live: LiveEvidence{Observed: true, Established: 32, Connected: 32}, ProductSHA: strings.Repeat("a", 40), ProductInputSHA256: strings.Repeat("a", 64), ProductBinarySHA256: strings.Repeat("b", 64), FixtureInputSHA256: strings.Repeat("c", 64), FixtureSourceInputSHA256: strings.Repeat("d", 64), AppImageID: "sha256:" + strings.Repeat("a", 64), DatabaseImageID: "sha256:" + strings.Repeat("b", 64), ProxyImageID: "sha256:" + strings.Repeat("c", 64), Profile: "standard", EnforcedLimits: map[string]ResourceBudget{"app": {CPUs: 1, MemoryBytes: 536870912, MemorySwapBytes: 536870912}, "database": {CPUs: 2.5, MemoryBytes: 3221225472, MemorySwapBytes: 3221225472}, "proxy": {CPUs: .5, MemoryBytes: 536870912, MemorySwapBytes: 536870912}}, SealedAuthorityHead: 248, RestoredAuthorityHead: 280}
}
func TestResultPublicationEligibility(t *testing.T) {
	r := coherentCell()
	r.Finalize()
	if r.Percentiles == nil || !r.Passing {
		t.Fatal("coherent cell refused")
	}
	for _, mutate := range []func(*CellResult){
		func(r *CellResult) { r.ActualDevices = 0 }, func(r *CellResult) { r.ActualWriters = 0 }, func(r *CellResult) { r.PlannedWrites = 0 }, func(r *CellResult) { r.ScheduledWrites = 239 }, func(r *CellResult) { r.Attempts = 10000 }, func(r *CellResult) { r.PeerPulls = Counts{} }, func(r *CellResult) { r.Acknowledged = 239 }, func(r *CellResult) { r.PeerApplied = 239 }, func(r *CellResult) { r.LatencySeconds = []float64{math.NaN()} }, func(r *CellResult) { r.Setup = Counts{Attempts: 10} }, func(r *CellResult) { r.Live.UnexpectedLosses = 1; r.Live.Connected = 31 }, func(r *CellResult) { r.AppImageID = "unknown" }, func(r *CellResult) { r.EnforcedLimits["app"] = ResourceBudget{} },
	} {
		r = coherentCell()
		mutate(&r)
		r.Finalize()
		if r.Passing {
			t.Fatalf("inconsistent cell passing: %#v", r)
		}
	}
	r = coherentCell()
	r.Completion = Interrupted
	r.Finalize()
	if r.Passing || r.Percentiles != nil {
		t.Fatal("interrupted cell published")
	}
}
func TestMalformedTornAndSymlinkResultsRefused(t *testing.T) {
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(parent, "cell.json")
	for _, raw := range []string{`{"completion":`, `{"completion":"complete","successes":-1}`, `{"completion":"complete","payload":"secret"}`, `{"completion":"complete"} {}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadResult(path); err == nil {
			t.Fatalf("malformed accepted: %s", raw)
		}
	}
	if err := os.WriteFile(path, []byte(`{"completion":"complete"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResult(path); err == nil {
		t.Fatal("non-private result accepted")
	}
	link := filepath.Join(parent, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResult(link); err == nil {
		t.Fatal("symlink result accepted")
	}
}

func TestPersistedDerivedAccountingCannotDisguiseErrors(t *testing.T) {
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(parent, "cell.json")
	r := coherentCell()
	r.Finalize()
	r.MeasurementHTTP.Attempts = 99999
	raw, _ := json.Marshal(r)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResult(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Completion != InvalidResult || got.Passing || got.Percentiles != nil {
		t.Fatal("forged denominator published")
	}
	r = coherentCell()
	r.Completion = Interrupted
	r.ScheduledWrites = 10
	r.Counts = Counts{Attempts: 9, Successes: 9}
	r.Acknowledged = 9
	r.PeerPulls = Counts{Attempts: 8, Successes: 8}
	r.PeerApplied = 8
	r.LatencySeconds = r.LatencySeconds[:9]
	r.PeerHTTPSeconds = r.PeerHTTPSeconds[:8]
	r.MeasurementSeconds = 3
	r.Finalize()
	if r.Completion != Interrupted || r.UnresolvedWrites != 1 || r.DeadlineUnfiredWrites != 230 || r.AcknowledgedWithoutPeer != 1 || r.Passing {
		t.Fatal("honest partial checkpoint destroyed")
	}
}

func TestHistoricalRefusalClassificationIsUnavailable(t *testing.T) {
	r := coherentCell()
	r.RefusalClassification = ""
	r.Finalize()
	if r.Passing {
		t.Fatal("legacy receipt promoted to typed admission evidence")
	}
}

func TestUnmeasuredGatedReceiptRemainsTyped(t *testing.T) {
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(parent, "cell.json")
	for _, reason := range []CompletionReason{Gated, Unmeasured, Interrupted} {
		r := CellResult{Version: 1, Completion: reason, MeasurementState: "unmeasured", PlannedCell: &CellPlan{Devices: 128, ActiveWriters: 64, Rate: 15, Seconds: 60}, RequiredAnchor: &CellPlan{Devices: 32, ActiveWriters: 16, Rate: 15, Seconds: 60, Shape: "steady"}}
		raw, _ := json.Marshal(r)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadResult(path)
		if err != nil || got.Completion != reason || got.Passing || got.Percentiles != nil || got.RefusalClassification != "unavailable_legacy" {
			t.Fatalf("unmeasured receipt misread: %+v %v", got, err)
		}
	}
}
