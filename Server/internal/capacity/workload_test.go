package capacity

import (
	"babytracker/server/internal/protocol"
	"context"
	"encoding/json"
	"errors"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pilotWorkload() Workload {
	return Workload{Devices: 32, ActiveWriters: 16, EmptyPullInterval: 300 * time.Second, WritesPerWriterPerMinute: 15, Warmup: 30 * time.Second, Measure: 60 * time.Second}
}
func TestWorkloadQuotaAccounting(t *testing.T) {
	m := FixtureManifest{Devices: 512}
	w := pilotWorkload()
	for _, devices := range []int{32, 128, 512} {
		for _, rate := range []int{1, 15, 60} {
			w.Devices = devices
			w.ActiveWriters = devices / 2
			w.WritesPerWriterPerMinute = rate
			if err := w.Validate(m); err != nil {
				t.Fatalf("admissible %d/%d: %v", devices, rate, err)
			}
		}
	}
	w = pilotWorkload()
	w.Contention = true
	w.ActiveWriters = 2
	w.WritesPerWriterPerMinute = 60
	if w.Validate(m) == nil {
		t.Fatal("same device's combined writer and peer/setup pull demand exceeds quota")
	}
	w = pilotWorkload()
	w.BacklogChanges = 100
	if err := w.Validate(m); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Workload){func(w *Workload) { w.Measure = 59 * time.Second }, func(w *Workload) { w.Warmup = 29 * time.Second }, func(w *Workload) { w.Devices = 33 }, func(w *Workload) { w.EmptyPullInterval = time.Second }, func(w *Workload) { w.ActiveWriters = 32 }} {
		w = pilotWorkload()
		change(&w)
		if w.Validate(m) == nil {
			t.Fatal("invalid schedule accepted")
		}
	}
}

type rewriteTransport struct{ base *url.URL }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = t.base.Scheme
	req.URL.Host = t.base.Host
	return http.DefaultTransport.RoundTrip(req)
}
func TestSetup429PreservesRefusalAndNeverMeasures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/live" {
			ws, err := websocket.Accept(out, req, &websocket.AcceptOptions{Subprotocols: []string{"baby-tracker.v1"}})
			if err != nil {
				return
			}
			defer ws.CloseNow()
			for {
				if _, _, err := ws.Read(req.Context()); err != nil {
					return
				}
			}
		}
		out.WriteHeader(429)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	w := pilotWorkload()
	target := &runTarget{HTTP: &http.Client{Transport: rewriteTransport{base}}}
	for i := 0; i < 32; i++ {
		target.Devices = append(target.Devices, workloadDevice{Token: "private-test-token", FamilyID: "family", BabyID: "baby"})
	}
	w.target = target
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := Run(ctx, FixtureManifest{Devices: 32}, w)
	if err == nil || r.Setup.Refusals != 1 || r.MeasurementSeconds != 0 || r.Percentiles != nil || r.Passing || r.ActualDevices != 1 {
		t.Fatalf("setup failure lost/refuted: %#v / %v", r, err)
	}
}

func TestTokenScheduleUsesObservedExpirationDistribution(t *testing.T) {
	now := time.Now()
	w := pilotWorkload()
	w.Devices = 512
	w.ActiveWriters = 256
	tgt := &runTarget{}
	for i := 0; i < 512; i++ {
		tgt.Devices = append(tgt.Devices, workloadDevice{ExpiresAt: now.Add(10*time.Minute - time.Duration(i)*time.Millisecond)})
	}
	evidence, _, incompatible := scheduleEvidence(w, tgt, now)
	if !incompatible || evidence["required_refreshes_by_window_end"] != 512 || evidence["socket_admission_delay_seconds"] != 610 || evidence["theoretical_max_refreshed_tokens_simultaneously_valid"] != 240 {
		t.Fatalf("selected schedule not proven incompatible: %#v", evidence)
	}
	// The decision derives from the actual expiration distribution, never an
	// unconditional 512-device prohibition. This alternate distribution is a
	// control input, not a claim that normal sessions live longer than 10min.
	for i := range tgt.Devices {
		tgt.Devices[i].ExpiresAt = now.Add(time.Hour)
	}
	if _, _, incompatible = scheduleEvidence(w, tgt, now); incompatible {
		t.Fatal("blanket device-count rejection")
	}
	w = pilotWorkload()
	for i := range tgt.Devices {
		tgt.Devices[i].ExpiresAt = now.Add(10 * time.Minute)
	}
	if _, _, incompatible = scheduleEvidence(w, tgt, now); incompatible {
		t.Fatal("pilot schedule rejected")
	}
}

func TestSocketSetup429IsRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) { out.WriteHeader(429) }))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	w := pilotWorkload()
	tgt := &runTarget{HTTP: &http.Client{Transport: rewriteTransport{base}}}
	for i := 0; i < 32; i++ {
		tgt.Devices = append(tgt.Devices, workloadDevice{Token: "private-test-token"})
	}
	w.target = tgt
	r, err := Run(t.Context(), FixtureManifest{Devices: 32}, w)
	if err == nil || r.Setup.Refusals != 1 || r.Setup.UnexpectedErrors != 0 || r.ActualDevices != 0 {
		t.Fatalf("socket429 classification lost: %#v / %v", r.Setup, err)
	}
}

func TestMeasurementBoundaryDrainsStartedRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	window := newMeasurementWindow(ctx, 20*time.Millisecond, 100*time.Millisecond)
	defer window.Close()
	if !window.CanStart() {
		t.Fatal("initial operation rejected")
	}
	<-window.ScheduleContext.Done()
	if window.CanStart() {
		t.Fatal("write starts after measurement boundary")
	}
	if window.RequestContext.Err() != nil {
		t.Fatal("in-flight request truncated at measurement boundary instead of bounded drain")
	}
	time.Sleep(20 * time.Millisecond)
	measurement, drain := window.Durations()
	if measurement != 20*time.Millisecond || drain < 20*time.Millisecond {
		t.Fatalf("measurement/drain conflated: %v/%v", measurement, drain)
	}
	cancel()
	select {
	case <-window.RequestContext.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation ignored while draining")
	}
}
func TestMeasurementDrainHasHardBound(t *testing.T) {
	window := newMeasurementWindow(t.Context(), 10*time.Millisecond, 20*time.Millisecond)
	defer window.Close()
	<-window.RequestContext.Done()
	if time.Since(window.Start) < 30*time.Millisecond || time.Since(window.Start) > time.Second {
		t.Fatal("request drain deadline missing")
	}
}

func TestEveryPlannedSlotHasOutcomeClass(t *testing.T) {
	r := CellResult{PlannedWrites: 240, ScheduledWrites: 231, SkippedWrites: 94, Counts: Counts{Attempts: 137}, Acknowledged: 123, PeerApplied: 115}
	r.Finalize()
	if r.DeadlineUnfiredWrites != 9 || r.Attempts+r.SkippedWrites+r.DeadlineUnfiredWrites != r.PlannedWrites || r.AcknowledgedWithoutPeer != 8 {
		t.Fatalf("slot/ack accounting lost: %#v", r)
	}
	if plannedSlots(16, 15, 60*time.Second) != 240 || plannedSlots(1, 1, 61*time.Second) != 2 {
		t.Fatal("offered geometry incorrect")
	}
}
func TestFixedCardinalityErrorCauses(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{{statusError(503), "http_503"}, {statusError(429), "http_429"}, {context.DeadlineExceeded, "deadline_exceeded"}, {context.Canceled, "canceled"}, {errors.New("capacity_protocol_failed"), "protocol_decode"}, {errors.New("private token must never survive"), "other"}}
	r := CellResult{}
	for _, c := range cases {
		if got := errorCause(c.err); got != c.want {
			t.Fatalf("cause %s expected %s", got, c.want)
		}
		r.recordPhase(&r.Counts, c.err, "measurement")
	}
	if len(r.ErrorCauses) != len(cases) || r.Refusals != 1 || r.UnexpectedErrors != 5 {
		t.Fatal("refusal/cause counts not retained")
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "private token") {
		t.Fatal("private error text leaked")
	}
}

func TestMeasurementHTTPDenominatorSeparatesWriterAndPeer(t *testing.T) {
	r := CellResult{Counts: Counts{Attempts: 137, Successes: 123, UnexpectedErrors: 14}, PeerPulls: Counts{Attempts: 123, Successes: 115, UnexpectedErrors: 8}, EmptyPulls: Counts{Attempts: 2, Successes: 1, UnexpectedErrors: 1}, MeasuredRefresh: Counts{Attempts: 2, Successes: 2}}
	r.Finalize()
	if r.MeasurementHTTP.Attempts != 264 || r.MeasurementHTTP.Successes != 241 || r.MeasurementHTTP.UnexpectedErrors != 23 || r.UnexpectedErrors != 14 {
		t.Fatalf("measurement denominator lost: %#v", r)
	}
}

func TestBlockedWriterAdmissionHonorsDrainCancellation(t *testing.T) {
	var lock sync.RWMutex
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := lockWriter(ctx, &lock); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked writer ignored deadline: %v", err)
	}
	if time.Since(began) > time.Second {
		t.Fatal("writer admission exceeded bounded drain")
	}
}

func TestEstablishedSocketLossPersistsWhileHTTPHealthy(t *testing.T) {
	closed := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/live" {
			w.WriteHeader(200)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		closed <- ws
	}))
	defer server.Close()
	ws, _, err := websocket.Dial(t.Context(), server.URL+"/live", nil)
	if err != nil {
		t.Fatal(err)
	}
	population := newLivePopulation()
	population.watch(t.Context(), ws)
	_ = (<-closed).CloseNow()
	deadline := time.Now().Add(time.Second)
	for population.snapshot().UnexpectedLosses == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stats := population.snapshot()
	if stats.Connected != 0 || stats.UnexpectedLosses != 1 {
		t.Fatalf("silent live loss: %#v", stats)
	}
	response, err := http.Get(server.URL + "/sync")
	if err != nil || response.StatusCode != 200 {
		t.Fatal("HTTP control failed")
	}
	response.Body.Close()
}

func TestEmptyAndRenewalBatchesDrainWithoutSyntheticAttempts(t *testing.T) {
	for _, name := range []string{"empty_pull", "refresh_reconnect"} {
		t.Run(name, func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(40 * time.Millisecond); w.WriteHeader(200) }))
			defer server.Close()
			window := newMeasurementWindow(t.Context(), 20*time.Millisecond, 100*time.Millisecond)
			defer window.Close()
			started, err := runScheduledBatch(window, t.Context(), 2, func(ctx context.Context, i int) error {
				attempts++
				req, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					resp.Body.Close()
				}
				return err
			})
			if err != nil || started != 1 || attempts != 1 || window.RequestContext.Err() != nil {
				t.Fatalf("batch truncated/synthetic: %d/%d %v", started, attempts, err)
			}
		})
	}
}

func TestPlannedSocketRenewalDoesNotBecomeUnexpectedLoss(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err == nil {
			accepted <- ws
		}
	}))
	defer server.Close()
	ws, _, err := websocket.Dial(t.Context(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote := <-accepted
	defer remote.CloseNow()
	p := newLivePopulation()
	p.watch(t.Context(), ws)
	p.retire(ws)
	p.shutdown()
	stats := p.snapshot()
	if stats.UnexpectedLosses != 0 || stats.PlannedRenewals != 1 || stats.Connected != 0 {
		t.Fatalf("renewal misclassified: %+v", stats)
	}
}

func TestRefreshAndReconnectEachRequireTheirOwnAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		refreshDelay, reconnectDelay time.Duration
		wantStarted                  int
	}{{"refresh_crosses_end", 40 * time.Millisecond, 0, 1}, {"reconnect_crosses_end", 0, 40 * time.Millisecond, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			var refreshes, reconnects atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/refresh" {
					refreshes.Add(1)
					time.Sleep(tc.refreshDelay)
					w.WriteHeader(200)
					return
				}
				reconnects.Add(1)
				time.Sleep(tc.reconnectDelay)
				ws, err := websocket.Accept(w, r, nil)
				if err == nil {
					defer ws.CloseNow()
				}
			}))
			defer server.Close()
			window := newMeasurementWindow(t.Context(), 20*time.Millisecond, 100*time.Millisecond)
			defer window.Close()
			started, err := runScheduledBatch(window, t.Context(), 4, func(ctx context.Context, stage int) error {
				if stage%2 == 0 {
					req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/refresh", nil)
					resp, err := http.DefaultClient.Do(req)
					if err == nil {
						resp.Body.Close()
					}
					return err
				}
				ws, _, err := websocket.Dial(ctx, server.URL+"/reconnect", nil)
				if err == nil {
					ws.CloseNow()
				}
				return err
			})
			if err != nil || started != tc.wantStarted || refreshes.Load() != 1 || int(reconnects.Load()) != tc.wantStarted-1 {
				t.Fatalf("synthetic/truncated stages: started%d auth%d reconnect%d err%v", started, refreshes.Load(), reconnects.Load(), err)
			}
		})
	}
}

func TestEmptyPullRequestTimerStartsAfterCancelableAdmission(t *testing.T) {
	var lock sync.RWMutex
	lock.Lock()
	window := newMeasurementWindow(t.Context(), time.Second, 20*time.Second)
	defer window.Close()
	released := make(chan struct{})
	go func() { time.Sleep(100 * time.Millisecond); lock.Unlock(); close(released) }()
	started, err := runLockedBatch(window, t.Context(), 1, func(int) *sync.RWMutex { return &lock }, func(ctx context.Context, _ int) error {
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) < 9950*time.Millisecond {
			t.Fatal("request timer consumed by family admission wait")
		}
		return nil
	})
	<-released
	if err != nil || started != 1 {
		t.Fatalf("admission failed: %d %v", started, err)
	}
	lock.Lock()
	window2 := newMeasurementWindow(t.Context(), 20*time.Millisecond, 100*time.Millisecond)
	defer window2.Close()
	calls := 0
	started, err = runLockedBatch(window2, t.Context(), 2, func(int) *sync.RWMutex { return &lock }, func(context.Context, int) error { calls++; return nil })
	lock.Unlock()
	if err != nil || started != 0 || calls != 0 {
		t.Fatalf("synthetic closed-window batch attempt: %d/%d %v", started, calls, err)
	}
}

func TestAdmissionRefusalBelowErrorThresholdNeverPasses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		passing    bool
		refusals   int
	}{
		{"healthy", `{}`, 200, true, 0},
		{"global admission", `{"error":{"code":"server_busy"}}`, 503, false, 1},
		{"ordinary unavailable", `{"error":{"code":"not_ready"}}`, 503, true, 0},
		{"quota", `{"error":{"code":"quota_exceeded"}}`, 429, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			err := request(t.Context(), &http.Client{Transport: rewriteTransport{base}}, "/v1/sync", "", nil, nil)
			r := coherentCell()
			r.Requested.Measure = 5 * time.Minute
			r.MeasurementSeconds = 300
			r.PlannedWrites = 1200
			r.ScheduledWrites = 1200
			successes := 1200
			if err != nil {
				successes--
			}
			r.Counts = Counts{Attempts: 1199, Successes: 1199}
			r.recordPhase(&r.Counts, err, "measurement_mutation")
			r.Acknowledged = successes
			r.PeerApplied = successes
			r.PeerPulls = Counts{Attempts: successes, Successes: successes}
			r.LatencySeconds = make([]float64, successes)
			r.PeerHTTPSeconds = make([]float64, successes)
			r.Finalize()
			if r.Passing != tc.passing || r.Refusals != tc.refusals {
				t.Fatalf("acceptance/refusal classification: passing=%v refusals=%d unexpected=%d completion=%s", r.Passing, r.Refusals, r.UnexpectedErrors, r.Completion)
			}
			if tc.name == "global admission" && r.ErrorCauses["measurement_mutation_server_busy"] != 1 {
				t.Fatal("global refusal not separately typed")
			}
		})
	}
}

type contentionTransport struct {
	base                            rewriteTransport
	mutations                       atomic.Int32
	held, canceled, release, joined chan struct{}
	entity                          string
}

func (t *contentionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/v1/live" {
		return t.base.RoundTrip(req)
	}
	var input protocol.SyncRequest
	if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
		return nil, err
	}
	if len(input.Mutations) > 0 {
		switch t.mutations.Add(1) {
		case 1:
			t.entity = input.Mutations[0].EntityID
		case 2:
			<-t.held
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"server_busy"}}`)), Header: make(http.Header)}, nil
		case 3:
			close(t.held)
			<-req.Context().Done()
			close(t.canceled)
			<-t.release
			close(t.joined)
			return nil, req.Context().Err()
		}
	}
	cursor := protocol.Cursor{ServerInstanceID: "11111111-1111-4111-8111-111111111111", RecoveryEpoch: 1, FamilyID: "22222222-2222-4222-8222-222222222222", Sequence: 1}
	response := protocol.SyncResponse{ServerInstanceID: cursor.ServerInstanceID, RecoveryEpoch: 1, HighWater: cursor, Outcomes: []protocol.Outcome{{Status: "applied"}}, Changes: []protocol.JournalChange{}}
	if len(input.Mutations) == 0 && t.entity != "" {
		response.Changes = append(response.Changes, protocol.JournalChange{EntityID: t.entity, EntityVersion: 1, EntityKind: "diaper", Cursor: cursor, SnapshotOrTombstone: protocol.CanonicalEntity{EntityKind: "diaper", Meta: protocol.EntityMeta{EntityVersion: 1}, Payload: protocol.DiaperPayload{}}})
	}
	raw, _ := json.Marshal(response)
	body := string(raw)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}
func TestFailedContentionJoinsHeldWriterBeforeCheckpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"baby-tracker.v1"}})
		if err == nil {
			defer ws.CloseNow()
			for {
				if _, _, err = ws.Read(r.Context()); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	transport := &contentionTransport{base: rewriteTransport{base}, held: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), joined: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	w := pilotWorkload()
	w.Devices = 4
	w.ActiveWriters = 2
	w.Contention = true
	checkpoints := make(chan struct{}, 10)
	w.target = &runTarget{HTTP: &http.Client{Transport: transport}, Checkpoint: func(CellResult) error { checkpoints <- struct{}{}; return nil }}
	for i := 0; i < 4; i++ {
		w.target.Devices = append(w.target.Devices, workloadDevice{Token: "private", FamilyID: "22222222-2222-4222-8222-222222222222", BabyID: "baby", ExpiresAt: time.Now().Add(time.Hour)})
	}
	type outcome struct {
		r   CellResult
		err error
	}
	done := make(chan outcome, 1)
	go func() { r, err := Run(ctx, FixtureManifest{Devices: 4}, w); done <- outcome{r, err} }()
	defer func() {
		cancel()
		close(transport.release)
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()
	select {
	case <-transport.canceled:
	case result := <-done:
		t.Fatalf("Run returned with the second contention worker still owned: %v %+v", result.err, result.r.Setup)
	case <-time.After(time.Second):
		t.Fatal("failed probe did not cancel held worker")
	}
	select {
	case <-checkpoints:
		t.Fatal("checkpoint before worker join")
	case <-done:
		t.Fatal("return before worker join")
	case <-time.After(20 * time.Millisecond):
	}
	// Release separately from the deferred safety path.
	transport.release <- struct{}{}
	result := <-done
	if result.err == nil || result.r.Setup.Attempts != 12 || result.r.Setup.Refusals != 1 || result.r.Setup.UnexpectedErrors != 1 || result.r.ContentionProofPairs != 0 || result.r.ProofAcknowledged != 1 || result.r.ProofPeerObserved != 1 {
		t.Fatalf("joined outcomes lost: %+v %v", result.r.Setup, result.err)
	}
	select {
	case <-transport.joined:
	default:
		t.Fatal("second worker not joined")
	}
	done <- result
}

func TestAdmissionClassificationBoundsAndReceiptRoundTrip(t *testing.T) {
	for _, body := range []string{`{"error":{"code":"server_busy"}} trailing`, strings.Repeat(" ", 4096) + `{"error":{"code":"server_busy"}}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503); io.WriteString(w, body) }))
		base, _ := url.Parse(server.URL)
		err := request(t.Context(), &http.Client{Transport: rewriteTransport{base}}, "/v1/sync", "", nil, nil)
		server.Close()
		if errorCause(err) != "http_503" {
			t.Fatal("malformed or oversized error was promoted to typed admission refusal")
		}
	}
	r := coherentCell()
	r.Counts = Counts{Attempts: 240, Successes: 239, Refusals: 1, GlobalAdmissionRefusals: 1}
	r.Acknowledged = 239
	r.PeerApplied = 239
	r.PeerPulls = Counts{Attempts: 239, Successes: 239}
	r.LatencySeconds = r.LatencySeconds[:239]
	r.PeerHTTPSeconds = r.PeerHTTPSeconds[:239]
	r.ErrorCauses = map[string]int{"measurement_mutation_server_busy": 1}
	r.Finalize()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(parent, "cell.json")
	raw, _ := json.Marshal(r)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResult(path)
	if err != nil || got.Passing || got.MeasurementHTTP.GlobalAdmissionRefusals != 1 || got.UnexpectedErrors != 0 || got.Refusals != 1 {
		t.Fatalf("typed refusal receipt lost: %+v %v", got.MeasurementHTTP, err)
	}
}

func TestSocketSetupAdmissionIsTypedRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		out.WriteHeader(503)
		io.WriteString(out, `{"error":{"code":"server_busy"}}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	w := pilotWorkload()
	w.target = &runTarget{HTTP: &http.Client{Transport: rewriteTransport{base}}}
	for i := 0; i < 32; i++ {
		w.target.Devices = append(w.target.Devices, workloadDevice{Token: "private"})
	}
	r, err := Run(t.Context(), FixtureManifest{Devices: 32}, w)
	if err == nil || r.Setup.GlobalAdmissionRefusals != 1 || r.Setup.Refusals != 1 || r.Setup.UnexpectedErrors != 0 {
		t.Fatalf("socket admission refusal lost: %+v %v", r.Setup, err)
	}
}
