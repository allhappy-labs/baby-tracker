//go:build identitytest

package capacity

import (
	"babytracker/server/internal/protocol"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ConnectRoute is generator infrastructure, separate from the app/DB/TLS budgets.
// It cannot forward arbitrary targets and never terminates or bypasses TLS.
type ConnectRoute struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	clients  map[net.Conn]struct{}
	once     sync.Once
	stopped  chan struct{}
	workers  sync.WaitGroup
}

func StartConnectRoute(ctx context.Context, target string) (*ConnectRoute, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil || host != "127.0.0.1" || port == "" {
		return nil, ErrUnsafeTarget
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &ConnectRoute{listener: listener, target: target, clients: map[net.Conn]struct{}{}, stopped: make(chan struct{})}
	r.workers.Add(1)
	go func() {
		defer r.workers.Done()
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			r.mu.Lock()
			select {
			case <-r.stopped:
				r.mu.Unlock()
				c.Close()
				return
			default:
			}
			if len(r.clients) >= 16 {
				r.mu.Unlock()
				c.Close()
				continue
			}
			r.clients[c] = struct{}{}
			r.workers.Add(1)
			r.mu.Unlock()
			go r.serve(ctx, c)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			r.shutdown()
		case <-r.stopped:
		}
	}()
	return r, nil
}
func (r *ConnectRoute) Address() string { return r.listener.Addr().String() }
func (r *ConnectRoute) shutdown() {
	r.once.Do(func() {
		close(r.stopped)
		r.listener.Close()
		r.mu.Lock()
		defer r.mu.Unlock()
		for c := range r.clients {
			c.Close()
		}
	})
}
func (r *ConnectRoute) Close() { r.shutdown(); r.workers.Wait() }
func (r *ConnectRoute) serve(ctx context.Context, c net.Conn) {
	defer r.workers.Done()
	defer func() { c.Close(); r.mu.Lock(); delete(r.clients, c); r.mu.Unlock() }()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(io.LimitReader(c, 8192))
	request, err := http.ReadRequest(reader)
	if err != nil || request.Method != "CONNECT" || request.Host != "sync.example.test:443" || request.URL.Host != "sync.example.test:443" {
		io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	upstream, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", r.target)
	if err != nil {
		return
	}
	defer upstream.Close()
	if _, err = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	// No request body is allowed; TLS starts only after the CONNECT response.
	if reader.Buffered() != 0 {
		return
	}
	copied := make(chan struct{})
	go func() { defer close(copied); io.Copy(upstream, c); upstream.Close() }()
	io.Copy(c, upstream)
	c.Close()
	<-copied
}

var errSwiftProof = errors.New("capacity_swift_proof_incomplete")

func swiftWireSession(d FixtureDevice) protocol.SessionResponse {
	b := d.Session
	wire := protocol.SessionResponse{ProtocolVersion: "v1", AccessToken: b.AccessToken, RefreshToken: b.RefreshToken, ExpiresAt: b.ExpiresAt.UTC().Format(time.RFC3339Nano), ReauthenticateAt: b.ReauthenticateAt.UTC().Format(time.RFC3339Nano), RecoveryEpoch: b.RecoveryEpoch, DeviceID: b.DeviceID, DeviceCredential: d.DeviceCredential, Account: protocol.SessionAccount{ID: b.Account.ID, DisplayName: b.Account.DisplayName, ProfileRequired: b.Account.ProfileRequired}}
	if b.FamilySlot != nil {
		wire.FamilySlot = &protocol.SessionFamilySlot{FamilyID: b.FamilySlot.FamilyID, State: b.FamilySlot.State}
	}
	return wire
}

type SwiftPair struct {
	Distribution string  `json:"distribution"`
	Ordinal      int     `json:"ordinal"`
	Durable      float64 `json:"durable_seconds"`
	Observable   float64 `json:"observable_seconds"`
	Publication  float64 `json:"publication_seconds"`
}
type SwiftDiagnostic struct {
	Artifact       *SwiftArtifact `json:"swift_artifact,omitempty"`
	Coherent       bool           `json:"coherent"`
	Pairs          []SwiftPair    `json:"pairs"`
	CPUSeconds     float64        `json:"swift_launcher_cpu_seconds"`
	RSSPeakBytes   uint64         `json:"swift_launcher_rss_peak_bytes"`
	ResourceStatus string         `json:"resource_status"`
	Status         string         `json:"status"`
	Semantics      string         `json:"semantics"`
	ElapsedSeconds float64        `json:"elapsed_seconds"`
	Sample         map[string]any `json:"sample"`
	Proofs         []string       `json:"proofs"`
}

func (r *RestoredFixture) RunSwiftPeer(ctx context.Context, w Workload, artifact SwiftArtifact, output, mode string) (SwiftDiagnostic, error) {
	result := SwiftDiagnostic{Status: "incomplete", Semantics: "separate fresh-restored Swift durable-store and observable-model diagnostic; two devices on one account plus isolated second family; not rendered UI or mixed-load proof"}
	result.Artifact = &artifact
	if err := artifact.Validate(); err != nil {
		return result, err
	}

	if w.target == nil || len(r.Devices) < 4 || r.Devices[0].FamilyID != r.Devices[1].FamilyID || r.Devices[0].FamilyID == r.Devices[2].FamilyID {
		return result, ErrFixtureInvalid
	}
	route, err := StartConnectRoute(ctx, w.target.TLSAddress)
	if err != nil {
		return result, err
	}
	defer route.Close()
	state := filepath.Join(r.db.root, "swift-state.json")
	if err = writePrivateJSON(state, map[string]protocol.SessionResponse{"Owner": swiftWireSession(r.Devices[0]), "Caregiver": swiftWireSession(r.Devices[1]), "Isolation": swiftWireSession(r.Devices[2])}); err != nil {
		return result, err
	}
	defer os.Remove(state)
	// The entire private root also belongs to supervisor cleanup on hard kill.
	defer os.RemoveAll(filepath.Join(r.db.root, "foreground-latency-stores"))
	log, err := os.OpenFile(filepath.Join(output, "swift.private.log"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	defer log.Close()
	proofPath := filepath.Join(output, "swift-proof.private.log")
	proofFile, err := os.OpenFile(proofPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	proofFile.Close()
	filter := "ForegroundLatencyE2ETests/foregroundLiveHintAppliesBeforeFallback"
	if mode == "lifecycle" {
		filter = "ForegroundLatencyE2ETests/capacityLifecycleDiagnostic"
	}
	cmd := artifact.testCommand(ctx, filter)
	cmd.Dir = artifact.PackagePath
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = append(cmd.Env, "BABY_TRACKER_CAPACITY_PROOF="+proofPath, "BABY_TRACKER_FOREGROUND_LATENCY=1", "BABY_TRACKER_CAPACITY_CONNECT="+route.Address(), "BABY_TRACKER_E2E_BASE=https://sync.example.test", "BABY_TRACKER_E2E_STATE="+state, "BABY_TRACKER_E2E_CERT="+filepath.Join(r.db.root, "tls.crt"), "BABY_TRACKER_E2E_INSTANCE="+r.Config.ServerInstanceID)
	started := time.Now()
	runErr := cmd.Run()
	result.ElapsedSeconds = time.Since(started).Seconds()
	result.ResourceStatus = "unavailable"
	if cmd.ProcessState != nil {
		if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			result.CPUSeconds = float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
			result.RSSPeakBytes = uint64(usage.Maxrss)
			if runtime.GOOS != "darwin" {
				result.RSSPeakBytes *= 1024
			}
			result.ResourceStatus = "available: wait4 launcher and already-reaped children only; incomplete descendants excluded; outside container budgets"
		}
	}

	proofInput, err := os.Open(proofPath)
	if err != nil {
		return result, err
	}
	defer proofInput.Close()
	raw, err := io.ReadAll(io.LimitReader(proofInput, 16<<20))
	if err != nil {
		return result, err
	}
	if err := artifact.Validate(); err != nil {
		result.Artifact.Status = "rejected_after_execution"
		return result, err
	}
	proof := finishSwiftProof(raw, mode, runErr)
	result.Pairs, result.Sample, result.Proofs, result.Status = proof.Pairs, proof.Sample, proof.Proofs, proof.Status
	result.Coherent = proof.Coherent
	if result.Status == "incomplete" {
		return result, errSwiftProof
	}

	return result, nil
}

type SwiftCellResult struct {
	Version    int              `json:"version"`
	Kind       string           `json:"kind"`
	Completion CompletionReason `json:"completion"`
	Source     CellResult       `json:"source"`
	Diagnostic SwiftDiagnostic  `json:"diagnostic"`
}

func (r *RestoredFixture) SwiftCell(ctx context.Context, w Workload, artifact SwiftArtifact, output, mode string) error {
	diagnostic, err := r.RunSwiftPeer(ctx, w, artifact, output, mode)
	result := SwiftCellResult{Version: 1, Kind: "swift_foreground_diagnostic", Completion: SampleIncomplete, Source: w.target.Base, Diagnostic: diagnostic}
	r.CollectMetrics(ctx, w, &result.Source)
	if err == nil {
		result.Completion = Complete
	}
	if closeErr := r.Close(); closeErr != nil {
		result.Completion = CleanupFailed
		err = closeErr
	}
	if writeErr := writePrivateJSON(filepath.Join(output, "cell.json"), result); writeErr != nil {
		return writeErr
	}
	return err
}

// The summary is untrusted input. Required fields are typed and compared with
// nearest-rank percentiles derived from accepted pairs, never published verbatim.
type swiftSummary struct {
	Distribution  string   `json:"distribution"`
	Count         int      `json:"count"`
	Expected      int      `json:"expected"`
	Elapsed       float64  `json:"elapsed_seconds"`
	Durable50     *float64 `json:"durable_p50_seconds"`
	Durable95     *float64 `json:"durable_p95_seconds"`
	Durable99     *float64 `json:"durable_p99_seconds"`
	Observable95  *float64 `json:"observable_p95_seconds"`
	Publication95 *float64 `json:"publication_p95_seconds"`
}

func parseSwiftProof(raw []byte) SwiftDiagnostic {
	result := SwiftDiagnostic{Status: "incomplete", Coherent: true, Sample: map[string]any{}}
	scan := bufio.NewScanner(bytes.NewReader(raw))
	scan.Buffer(make([]byte, 4096), 65536)
	var summary swiftSummary
	summaries := 0
	for scan.Scan() {
		line := scan.Text()
		if strings.HasPrefix(line, "foreground-latency pair: ") {
			var pair SwiftPair
			valid := decodeSwiftRow([]byte(strings.TrimPrefix(line, "foreground-latency pair: ")), &pair) == nil && pair.Ordinal == len(result.Pairs)+1 && len(result.Pairs) < 200 && (pair.Distribution == "healthy" || pair.Distribution == "capacity_lifecycle" || pair.Distribution == "incomplete")
			for _, v := range []float64{pair.Durable, pair.Observable, pair.Publication} {
				valid = valid && v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
			}
			if !valid {
				result.Coherent = false
				continue
			}
			result.Pairs = append(result.Pairs, pair)
		}
		if strings.HasPrefix(line, "foreground-latency sample: ") {
			summaries++
			if summaries != 1 || decodeSwiftRow([]byte(strings.TrimPrefix(line, "foreground-latency sample: ")), &summary) != nil {
				result.Coherent = false
			}
		}
		if strings.HasPrefix(line, "foreground-latency proof: ") {
			if len(result.Proofs) >= 16 {
				result.Coherent = false
			} else {
				result.Proofs = append(result.Proofs, strings.TrimPrefix(line, "foreground-latency proof: "))
			}
		}
	}
	if scan.Err() != nil {
		result.Coherent = false
	}
	healthy, route, isolation, lifecycle := false, false, false, false
	for _, p := range result.Proofs {
		healthy = healthy || p == "healthy complete"
		route = route || p == "capacity HTTP native-WSS trust and cancellation controls complete"
		isolation = isolation || p == "isolation families=2 measured_sockets=2 cross_family_records=0"
		lifecycle = lifecycle || p == "capacity lifecycle complete"
	}
	durable, observable, publication := []float64{}, []float64{}, []float64{}
	for _, p := range result.Pairs {
		if p.Distribution != summary.Distribution {
			result.Coherent = false
		}
		durable = append(durable, p.Durable)
		observable = append(observable, p.Observable)
		publication = append(publication, p.Publication)
		if summary.Elapsed < math.Max(p.Durable, math.Max(p.Observable, p.Publication)) {
			result.Coherent = false
		}
	}
	percentile := func(values []float64, f float64) float64 {
		if len(values) == 0 {
			return 0
		}
		sort.Float64s(values)
		return values[int(math.Ceil(float64(len(values))*f))-1]
	}
	checks := []struct {
		key      string
		supplied *float64
		derived  float64
	}{
		{"durable_p50_seconds", summary.Durable50, percentile(durable, .5)},
		{"durable_p95_seconds", summary.Durable95, percentile(durable, .95)},
		{"durable_p99_seconds", summary.Durable99, percentile(durable, .99)},
		{"observable_p95_seconds", summary.Observable95, percentile(observable, .95)},
		{"publication_p95_seconds", summary.Publication95, percentile(publication, .95)},
	}
	for _, c := range checks {
		result.Sample[c.key] = nil
		if c.supplied == nil || math.IsNaN(*c.supplied) || math.IsInf(*c.supplied, 0) || math.Abs(*c.supplied-c.derived) > 1e-9 {
			result.Coherent = false
		}
	}
	if summaries != 1 || summary.Count != len(result.Pairs) || summary.Elapsed <= 0 || math.IsNaN(summary.Elapsed) || math.IsInf(summary.Elapsed, 0) {
		result.Coherent = false
	}
	result.Sample["distribution"] = summary.Distribution
	result.Sample["count"] = len(result.Pairs)
	result.Sample["expected"] = summary.Expected
	result.Sample["elapsed_seconds"] = summary.Elapsed
	if result.Coherent && route && isolation {
		if summary.Distribution == "healthy" && summary.Expected == 200 && len(result.Pairs) == 200 && summary.Elapsed >= 60 && healthy {
			result.Status = "complete"
			for _, c := range checks {
				result.Sample[c.key] = c.derived
			}
		} else if summary.Distribution == "capacity_lifecycle" && summary.Expected == 3 && len(result.Pairs) == 3 && lifecycle {
			result.Status = "lifecycle_complete"
		}
	}
	return result
}

// Reject duplicate JSON fields as well as type errors/unknown fields. A last-key
// wins decoder could otherwise conceal a contradictory count or percentile.
func decodeSwiftRow(raw []byte, target any) error {
	fields := json.NewDecoder(bytes.NewReader(raw))
	token, err := fields.Token()
	if err != nil || token != json.Delim('{') {
		return errSwiftProof
	}
	seen := map[string]bool{}
	for fields.More() {
		token, err = fields.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return errSwiftProof
		}
		seen[key] = true
		var value json.RawMessage
		if fields.Decode(&value) != nil {
			return errSwiftProof
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errSwiftProof
	}
	return nil
}

func finishSwiftProof(raw []byte, mode string, runErr error) SwiftDiagnostic {
	result := parseSwiftProof(raw)
	expected := "complete"
	if mode == "lifecycle" {
		expected = "lifecycle_complete"
	}
	if runErr != nil || result.Status != expected {
		result.Status = "incomplete"
		for key := range result.Sample {
			if strings.Contains(key, "_p50_") || strings.Contains(key, "_p95_") || strings.Contains(key, "_p99_") {
				result.Sample[key] = nil
			}
		}
	}
	return result
}
