package capacity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"github.com/coder/websocket"
)

// measurementWindow separates the offered schedule from in-flight requests.
// A completed request keeps its complete latency even when it drains after
// the offered window. Parent cancellation always ends both contexts.
type measurementWindow struct {
	Start, End                      time.Time
	ScheduleContext, RequestContext context.Context
	cancelSchedule, cancelRequests  context.CancelFunc
}

func newMeasurementWindow(parent context.Context, duration, drain time.Duration) *measurementWindow {
	start := time.Now()
	schedule, cancelSchedule := context.WithDeadline(parent, start.Add(duration))
	requests, cancelRequests := context.WithDeadline(parent, start.Add(duration+drain))
	return &measurementWindow{Start: start, End: start.Add(duration), ScheduleContext: schedule, RequestContext: requests, cancelSchedule: cancelSchedule, cancelRequests: cancelRequests}
}
func (w *measurementWindow) CanStart() bool {
	return w.ScheduleContext.Err() == nil && time.Now().Before(w.End)
}
func (w *measurementWindow) Close() { w.cancelSchedule(); w.cancelRequests() }
func (w *measurementWindow) Durations() (time.Duration, time.Duration) {
	elapsed := time.Since(w.Start)
	measurement := elapsed
	duration := w.End.Sub(w.Start)
	if measurement > duration {
		measurement = duration
	}
	drain := elapsed - duration
	if drain < 0 {
		drain = 0
	}
	return measurement, drain
}

// Writer admission must be cancelable even while token renewal owns a family.
func lockWriter(ctx context.Context, lock *sync.RWMutex) error {
	return lockAdmission(ctx, lock.TryRLock)
}
func lockExclusive(ctx context.Context, lock *sync.RWMutex) error {
	return lockAdmission(ctx, lock.TryLock)
}
func lockAdmission(ctx context.Context, try func() bool) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if try() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type LiveEvidence struct {
	Observed         bool `json:"observed"`
	Established      int  `json:"established"`
	Connected        int  `json:"connected"`
	UnexpectedLosses int  `json:"unexpected_losses"`
	PlannedRenewals  int  `json:"planned_renewals"`
	PlannedShutdowns int  `json:"planned_shutdowns"`
}
type livePopulation struct {
	mu      sync.Mutex
	stats   LiveEvidence
	planned map[*websocket.Conn]bool
	wg      sync.WaitGroup
}

func newLivePopulation() *livePopulation {
	return &livePopulation{stats: LiveEvidence{Observed: true}, planned: map[*websocket.Conn]bool{}}
}
func (p *livePopulation) watch(ctx context.Context, ws *websocket.Conn) {
	p.mu.Lock()
	p.planned[ws] = false
	p.stats.Established++
	p.stats.Connected++
	p.wg.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				p.mu.Lock()
				planned := p.planned[ws]
				delete(p.planned, ws)
				p.stats.Connected--
				if !planned && ctx.Err() == nil {
					p.stats.UnexpectedLosses++
				} else if !planned {
					p.stats.PlannedShutdowns++
				}
				p.mu.Unlock()
				return
			}
		}
	}()
}
func (p *livePopulation) retire(ws *websocket.Conn) {
	p.mu.Lock()
	if planned, ok := p.planned[ws]; ok && !planned {
		p.planned[ws] = true
		p.stats.PlannedRenewals++
	}
	p.mu.Unlock()
	_ = ws.CloseNow()
}
func (p *livePopulation) shutdown() {
	p.mu.Lock()
	connections := make([]*websocket.Conn, 0, len(p.planned))
	for ws, planned := range p.planned {
		if !planned {
			p.planned[ws] = true
			p.stats.PlannedShutdowns++
		}
		connections = append(connections, ws)
	}
	p.mu.Unlock()
	for _, ws := range connections {
		_ = ws.CloseNow()
	}
	p.wg.Wait()
}
func (p *livePopulation) snapshot() LiveEvidence { p.mu.Lock(); defer p.mu.Unlock(); return p.stats }

// Batch callbacks count only operations actually admitted for dispatch.
func runScheduledBatch(window *measurementWindow, parent context.Context, count int, dispatch func(context.Context, int) error) (int, error) {
	started := 0
	for i := 0; i < count; i++ {
		requestParent := parent
		if window != nil {
			if !window.CanStart() {
				break
			}
			requestParent = window.RequestContext
		} else if parent.Err() != nil {
			break
		}
		callCtx, cancel := context.WithTimeout(requestParent, 10*time.Second)
		started++
		err := dispatch(callCtx, i)
		cancel()
		if err != nil {
			return started, err
		}
	}
	return started, nil
}

func runLockedBatch(window *measurementWindow, parent context.Context, count int, lockFor func(int) *sync.RWMutex, dispatch func(context.Context, int) error) (int, error) {
	started := 0
	for i := 0; i < count; i++ {
		lock := lockFor(i)
		if err := lockExclusive(window.ScheduleContext, lock); err != nil {
			return started, nil
		}
		n, err := runScheduledBatch(window, parent, 1, func(ctx context.Context, _ int) error { return dispatch(ctx, i) })
		lock.Unlock()
		started += n
		if err != nil {
			return started, err
		}
		if n == 0 {
			return started, nil
		}
	}
	return started, nil
}

type Workload struct {
	DatabaseProfile          string        `json:"database_profile,omitempty"`
	Diagnostics              bool          `json:"diagnostics"`
	Devices                  int           `json:"devices"`
	ActiveWriters            int           `json:"active_writers"`
	EmptyPullInterval        time.Duration `json:"empty_pull_interval_ns"`
	WritesPerWriterPerMinute int           `json:"writes_per_writer_per_minute"`
	BacklogChanges           int           `json:"backlog_changes"`
	Warmup                   time.Duration `json:"warmup_ns"`
	Measure                  time.Duration `json:"measure_ns"`
	Contention               bool          `json:"contention"`
	SlowConsumer             bool          `json:"slow_consumer"`
	Reconnect                bool          `json:"reconnect"`
	// MCPMode selects the matched MCP cell: "" (MCP disabled, sync-only),
	// "idle" (enabled, no MCP traffic), "read" (paced reads) or "abusive"
	// (unpaced concurrent reads that exceed the per-credential limits).
	MCPMode                    string `json:"mcp_mode,omitempty"`
	MCPClients                 int    `json:"mcp_clients,omitempty"`
	MCPCallsPerClientPerMinute int    `json:"mcp_calls_per_client_per_minute,omitempty"`
	MCPAbuseConcurrency        int    `json:"mcp_abuse_concurrency,omitempty"`
	target                     *runTarget
}
type workloadDevice struct {
	Token, FamilyID, BabyID string
	RefreshToken            string
	ExpiresAt               time.Time
}
type runTarget struct {
	// mcpBase overrides the MCP origin in unit tests only.
	mcpBase string
	// mcpRouteChecked makes a sync-only cell prove /mcp is absent (404).
	mcpRouteChecked bool
	MCPCredentials  []mcpCredential
	TLSAddress      string
	HTTP            *http.Client
	// MCPHTTP is the MCP load generator's own client (a separate desktop
	// client from the phones). Nil falls back to HTTP in unit tests.
	MCPHTTP    *http.Client
	Devices    []workloadDevice
	Checkpoint func(CellResult) error
	Base       CellResult
}

func (w Workload) Validate(m FixtureManifest) error {
	if w.Devices < 2 || w.Devices > 512 || w.Devices%2 != 0 || w.Devices > m.Devices || w.ActiveWriters < 1 || w.ActiveWriters > w.Devices/2 || w.Warmup < 30*time.Second || w.Measure < 60*time.Second || w.Measure > 15*time.Minute || w.EmptyPullInterval < 300*time.Second || w.WritesPerWriterPerMinute < 1 || w.WritesPerWriterPerMinute > 60 || w.BacklogChanges < 0 || w.BacklogChanges > 100 {
		return errors.New("capacity_workload_invalid")
	}
	p := limits.DefaultPolicies()
	rate := w.WritesPerWriterPerMinute
	// One writer and one peer per account/family, plus initial pulls, proof and
	// empty pulls. Warmup crosses the same rolling quota windows as measurement.
	writers := 1
	if w.Contention {
		writers = w.ActiveWriters
	}
	if rate*writers+4 > p[limits.ScopeSyncPullDevice].Count || 2*rate*writers+8 > p[limits.ScopeSyncPullAccount].Count || rate*writers+w.BacklogChanges+1 > p[limits.ScopeSyncMutationDevice].Count || rate*writers+w.BacklogChanges+1 > p[limits.ScopeSyncMutationAccount].Count || rate*writers+w.BacklogChanges+1 > p[limits.ScopeSyncMutationFamily].Count {
		return errors.New("capacity_schedule_exceeds_default_quotas")
	}
	if w.Reconnect && 2 > w.Devices {
		return errors.New("capacity_workload_invalid")
	}
	return w.validateMCP()
}

// validateMCP keeps matched cells comparable: every MCP-enabled cell uses
// an explicit-mcp pool profile (sync budgets identical to explicit), and a
// read cell stays within the per-integration quota so admitted traffic is
// not shaped by refusals.
func (w Workload) validateMCP() error {
	invalid := errors.New("capacity_mcp_workload_invalid")
	if (w.MCPMode != "") != mcpProfile(w.DatabaseProfile) {
		return invalid
	}
	switch w.MCPMode {
	case "", MCPIdle:
		if w.MCPClients != 0 || w.MCPCallsPerClientPerMinute != 0 || w.MCPAbuseConcurrency != 0 {
			return invalid
		}
	case MCPRead:
		if w.MCPClients < 1 || w.MCPClients > mcpMaxClients || w.MCPCallsPerClientPerMinute < 1 ||
			w.MCPCallsPerClientPerMinute > limits.DefaultPolicies()[limits.ScopeMCPIntegration].Count || w.MCPAbuseConcurrency != 0 {
			return invalid
		}
	case MCPAbusive:
		if w.MCPClients < 1 || w.MCPClients > mcpMaxClients || w.MCPCallsPerClientPerMinute != 0 || w.MCPAbuseConcurrency < 2 || w.MCPAbuseConcurrency > 16 {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}

var admissionRefused = errors.New("capacity_server_busy")

const refusalClassification = "quota_429_and_typed_server_busy_v1"

type statusError int

func (e statusError) Error() string { return "capacity_http_status" }
func request(ctx context.Context, c *http.Client, path, token string, input, output any) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return errors.New("capacity_encode_failed")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://sync.example.test"+path, bytes.NewReader(raw))
	if err != nil {
		return errors.New("capacity_request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Baby-Tracker-Protocol", "v1")
	capabilities := "records-v1"
	if path == "/v1/auth/refresh" {
		capabilities = "identity-v1"
	}
	req.Header.Set("X-Baby-Tracker-Capabilities", capabilities)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return errors.New("capacity_client_timeout")
		}
		return errors.New("capacity_transport_failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return responseError(response)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<20))
		return err
	}
	if json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(output) != nil {
		return errors.New("capacity_protocol_failed")
	}
	return nil
}
func responseError(response *http.Response) error {
	if response.StatusCode == http.StatusServiceUnavailable && response.Body != nil {
		// Decode only the bounded machine code; never retain message/token data.
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if readErr == nil && len(raw) <= 4096 && json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code == "server_busy" {
			return admissionRefused
		}
	}
	return statusError(response.StatusCode)
}

func errorCause(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, admissionRefused) {
		return "server_busy"
	}
	var status statusError
	if errors.As(err, &status) {
		switch status {
		case 401:
			return "http_401"
		case 403:
			return "http_403"
		case 409:
			return "http_409"
		case 429:
			return "http_429"
		case 500:
			return "http_500"
		case 503:
			return "http_503"
		default:
			return "http_other"
		}
	}
	switch err.Error() {
	case "capacity_client_timeout":
		return "client_timeout"
	case "capacity_protocol_failed":
		return "protocol_decode"
	case "capacity_transport_failed":
		return "transport"
	case "capacity_mutation_not_applied":
		return "mutation_not_applied"
	case "capacity_cross_family_or_unknown_change":
		return "identity_or_unknown_change"
	case "capacity_peer_effect_missing_or_duplicate":
		return "peer_effect_mismatch"
	case "capacity_refresh_identity_failed":
		return "refresh_identity"
	default:
		return "other"
	}
}
func (r *CellResult) recordPhase(c *Counts, err error, phase string) {
	record(c, err)
	if err != nil {
		if r.ErrorCauses == nil {
			r.ErrorCauses = map[string]int{}
		}
		r.ErrorCauses[phase+"_"+errorCause(err)]++
	}
}
func plannedSlots(writers, rate int, duration time.Duration) int {
	interval := time.Minute / time.Duration(rate)
	count := 0
	for writer := 0; writer < writers; writer++ {
		available := duration - time.Duration(writer)*interval/time.Duration(writers)
		if available > 0 {
			count += int((available + interval - 1) / interval)
		}
	}
	return count
}
func record(c *Counts, err error) {
	c.Attempts++
	if err == nil {
		c.Successes++
	} else if errors.Is(err, admissionRefused) {
		c.Refusals++
		c.GlobalAdmissionRefusals++
	} else if err == statusError(429) {
		c.Refusals++
	} else {
		c.UnexpectedErrors++
	}
}
func syncCall(ctx context.Context, t *runTarget, d workloadDevice, cursor *protocol.Cursor, mutations []protocol.Mutation) (protocol.SyncResponse, error) {
	var out protocol.SyncResponse
	err := request(ctx, t.HTTP, "/v1/sync", d.Token, protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}}, Cursor: cursor, Mutations: mutations}, &out)
	if err == nil && (out.HighWater.FamilyID != d.FamilyID || len(out.SkippedOptionalCursors) > 0) {
		err = errors.New("capacity_cross_family_or_unknown_change")
	}
	return out, err
}
func scheduleEvidence(w Workload, t *runTarget, now time.Time) (map[string]float64, time.Time, bool) {
	admission := time.Duration((w.Devices-1)/50) * 61 * time.Second
	earliest, latest := t.Devices[0].ExpiresAt, t.Devices[0].ExpiresAt
	needed := 0
	end := now.Add(admission + w.Warmup + w.Measure)
	for _, d := range t.Devices[:w.Devices] {
		if d.ExpiresAt.Before(earliest) {
			earliest = d.ExpiresAt
		}
		if d.ExpiresAt.After(latest) {
			latest = d.ExpiresAt
		}
		if !d.ExpiresAt.IsZero() && !d.ExpiresAt.After(end) {
			needed++
		}
	}
	evidence := map[string]float64{"single_source_ip": 1, "socket_attempts_per_minute": 60, "socket_batch_size": 50, "socket_admission_delay_seconds": admission.Seconds(), "access_lifetime_seconds": 600, "initial_access_min_remaining_seconds": earliest.Sub(now).Seconds(), "initial_access_max_remaining_seconds": latest.Sub(now).Seconds(), "required_refreshes_by_window_end": float64(needed), "refresh_ip_per_600_seconds": 120, "theoretical_max_refreshed_tokens_simultaneously_valid": 240}
	return evidence, earliest, needed > 240 && !latest.After(now.Add(admission+w.Warmup))
}
func Run(ctx context.Context, fixture FixtureManifest, w Workload) (CellResult, error) {
	r := CellResult{Version: 1, Requested: w, Completion: SetupFailed, SealedAuthorityHead: fixture.ActualAuthorityEvents, FixtureInputSHA256: fixture.InputSHA256, PeerSemantics: "writer operation start through distinct-device HTTP journal decode; no Swift durable or observable apply", FixtureProfile: "one account, two devices, one baby per family; diaper history"}
	if w.target != nil {
		r = w.target.Base
		r.Requested = w
		r.Completion = SetupFailed
	}
	r.RefusalClassification = refusalClassification
	if w.MCPMode != "" {
		r.MCP = &MCPEvidence{Mode: w.MCPMode, Clients: w.MCPClients}
	}
	if err := w.Validate(fixture); err != nil {
		return r, err
	}
	t := w.target
	if t == nil || len(t.Devices) < w.Devices {
		return r, errors.New("capacity_private_target_required")
	}
	r.CountsSemantics = "top counts: measurement writer HTTP only; measurement_http sums writer, peer, empty pull and auth refresh HTTP; measured_socket_reconnect separate; setup/warmup include socket and HTTP calls"
	r.PlannedWrites = plannedSlots(w.ActiveWriters, w.WritesPerWriterPerMinute, w.Measure)
	evidence, earliest, incompatible := scheduleEvidence(w, t, time.Now())
	r.ScheduleEvidence = evidence
	if incompatible {
		r.FailureCode = "capacity_token_window_incompatible"
		if t.Checkpoint != nil {
			_ = t.Checkpoint(r)
		}
		return r, errors.New(r.FailureCode)
	}
	if w.Measure > 10*time.Minute && w.Devices > 32 {
		r.FailureCode = "capacity_refresh_schedule_unverified"
		if t.Checkpoint != nil {
			_ = t.Checkpoint(r)
		}
		return r, errors.New(r.FailureCode)
	}
	began := time.Now()
	var mu sync.Mutex
	population := newLivePopulation()
	checkpoint := func() error {
		r.Live = population.snapshot()
		if w.SlowConsumer {
			r.Live.Observed = false
		}
		r.Finalize()
		if t.Checkpoint != nil {
			return t.Checkpoint(r)
		}
		return nil
	}
	defer func() { t.HTTP.CloseIdleConnections() }()
	sockets := make([]*websocket.Conn, 0, w.Devices)
	defer func() {
		population.shutdown()
		for _, s := range sockets {
			_ = s.CloseNow()
		}
	}()
	cursors := make([]protocol.Cursor, w.Devices)
	familyLocks := make([]sync.RWMutex, w.Devices/2)
	for i, d := range t.Devices[:w.Devices] {
		// The single source IP socket-attempt quota is 60/minute. Larger cells
		// admit batches on separate full windows; no quota settings are changed.
		if i > 0 && i%50 == 0 {
			select {
			case <-ctx.Done():
				r.Completion = Deadline
				return r, ctx.Err()
			case <-time.After(61 * time.Second):
			}
		}
		ws, response, err := websocket.Dial(ctx, "https://sync.example.test/v1/live", &websocket.DialOptions{HTTPClient: t.HTTP, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + d.Token}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}, Subprotocols: []string{realtime.Subprotocol}})
		if err != nil && response != nil {
			err = responseError(response)
		}
		r.recordPhase(&r.Setup, err, "setup")
		if err != nil {
			_ = checkpoint()
			return r, errors.New("capacity_socket_setup_failed")
		}
		sockets = append(sockets, ws)
		r.ActualDevices++
		if !w.SlowConsumer {
			population.watch(ctx, ws)
		}
		out, err := syncCall(ctx, t, d, nil, []protocol.Mutation{})
		r.recordPhase(&r.Setup, err, "setup")
		if err != nil || out.NextPageToken != nil {
			_ = checkpoint()
			return r, errors.New("capacity_initial_pull_failed")
		}
		cursors[i] = out.HighWater
	}
	if w.SlowConsumer {
		r.SlowConsumerDevices = w.Devices
	}
	renewFamily := func(callCtx context.Context, index int, measured bool, window *measurementWindow) error {
		admissionCtx := callCtx
		if window != nil {
			admissionCtx = window.ScheduleContext
		}
		if err := lockExclusive(admissionCtx, &familyLocks[index/2]); err != nil {
			return nil
		}
		defer familyLocks[index/2].Unlock()
		_, err := runScheduledBatch(window, callCtx, 4, func(requestCtx context.Context, stage int) error {
			i := index + stage/2
			d := t.Devices[i]
			if stage%2 == 0 {
				var bundle protocol.SessionResponse
				err := request(requestCtx, t.HTTP, "/v1/auth/refresh", "", protocol.RefreshRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityIdentityV1}}, RefreshToken: d.RefreshToken}, &bundle)
				expiry, parseErr := time.Parse(time.RFC3339Nano, bundle.ExpiresAt)
				if err == nil && (parseErr != nil || bundle.AccessToken == "" || bundle.RefreshToken == "" || bundle.FamilySlot == nil || bundle.FamilySlot.FamilyID != d.FamilyID) {
					err = errors.New("capacity_refresh_identity_failed")
				}
				mu.Lock()
				r.recordPhase(&r.Refresh, err, "refresh")
				if measured {
					record(&r.MeasuredRefresh, err)
				}
				mu.Unlock()
				if err != nil {
					return err
				}
				d.Token = bundle.AccessToken
				d.RefreshToken = bundle.RefreshToken
				d.ExpiresAt = expiry
				t.Devices[i] = d
				return nil
			}
			population.retire(sockets[i])
			ws, response, err := websocket.Dial(requestCtx, "https://sync.example.test/v1/live", &websocket.DialOptions{HTTPClient: t.HTTP, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + d.Token}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}, Subprotocols: []string{realtime.Subprotocol}})
			if err != nil && response != nil {
				err = responseError(response)
			}
			mu.Lock()
			r.ReconnectAttempts++
			r.recordPhase(&r.RefreshReconnect, err, "refresh_reconnect")
			if measured {
				record(&r.MeasuredReconnect, err)
			}
			mu.Unlock()
			if err != nil {
				return err
			}
			sockets[i] = ws
			if !w.SlowConsumer {
				population.watch(ctx, ws)
			}
			return nil
		})
		return err
	}

	if w.Reconnect {
		if err := renewFamily(ctx, 0, false, nil); err != nil {
			_ = checkpoint()
			return r, errors.New("capacity_reconnect_setup_failed")
		}
	}
	if w.BacklogChanges > 0 {
		d := t.Devices[0]
		mutations := make([]protocol.Mutation, w.BacklogChanges)
		for i := range mutations {
			mutations[i] = protocol.Mutation{MutationID: newUUID(), EntityID: newUUID(), EntityKind: "diaper", Operation: "create", PayloadVersion: 1, ClientMutationTime: time.Now().UTC().Format(time.RFC3339Nano), Payload: protocol.DiaperPayload{BabyID: d.BabyID, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), TimeZoneID: "UTC", Contents: "wet"}}
		}
		out, err := syncCall(ctx, t, d, &cursors[0], mutations)
		r.recordPhase(&r.Setup, err, "setup")
		if err != nil || len(out.Outcomes) != w.BacklogChanges {
			_ = checkpoint()
			return r, errors.New("capacity_backlog_setup_failed")
		}
		for _, o := range out.Outcomes {
			if o.Status != "applied" {
				_ = checkpoint()
				return r, errors.New("capacity_backlog_not_acknowledged")
			}
			r.BacklogAcknowledged++
		}
		cursors[0] = out.HighWater
		start := time.Now()
		pull, err := syncCall(ctx, t, t.Devices[1], &cursors[1], []protocol.Mutation{})
		r.recordPhase(&r.Setup, err, "setup")
		if err != nil || len(pull.Changes) != w.BacklogChanges || pull.NextPageToken != nil {
			_ = checkpoint()
			return r, errors.New("capacity_catchup_failed")
		}
		r.CatchupObserved = len(pull.Changes)
		r.CatchupSeconds = append(r.CatchupSeconds, time.Since(start).Seconds())
		cursors[1] = pull.HighWater
	}
	operation := func(callCtx context.Context, writer int, measured bool, counter *Counts, window *measurementWindow) error {
		start := time.Now()
		index := 2 * writer
		if w.Contention {
			index = 0
		}
		if err := lockWriter(callCtx, &familyLocks[index/2]); err != nil {
			if measured {
				mu.Lock()
				r.ScheduledWrites--
				mu.Unlock()
			}
			return err
		}
		defer familyLocks[index/2].RUnlock()
		peer := index + 1
		d := t.Devices[index]
		entity := newUUID()
		mutation := protocol.Mutation{MutationID: newUUID(), EntityID: entity, EntityKind: "diaper", Operation: "create", PayloadVersion: 1, ClientMutationTime: time.Now().UTC().Format(time.RFC3339Nano), Payload: protocol.DiaperPayload{BabyID: d.BabyID, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), TimeZoneID: "UTC", Contents: "wet"}}
		mu.Lock()
		cursor := cursors[index]
		mu.Unlock()
		// A token renewal may have held the family lock across the boundary.
		// Such a slot never dispatches a mutation and stays deadline-unfired.
		if window != nil && !window.CanStart() {
			if measured {
				mu.Lock()
				r.ScheduledWrites--
				mu.Unlock()
			}
			return errors.New("capacity_schedule_closed")
		}
		out, err := syncCall(callCtx, t, d, &cursor, []protocol.Mutation{mutation})
		if err == nil && (len(out.Outcomes) != 1 || out.Outcomes[0].Status != "applied") {
			err = errors.New("capacity_mutation_not_applied")
		}
		mu.Lock()
		phase := "setup"
		if counter == &r.Warmup {
			phase = "warmup"
		}
		if measured {
			phase = "measurement"
		}
		r.recordPhase(counter, err, phase+"_mutation")
		if err == nil {
			cursors[index] = out.HighWater
			if measured {
				r.Acknowledged++
				r.LatencySeconds = append(r.LatencySeconds, time.Since(start).Seconds())
			}
		}
		mu.Unlock()
		if err != nil {
			return err
		}
		mu.Lock()
		peerCursor := cursors[peer]
		mu.Unlock()
		pull, pullErr := syncCall(callCtx, t, t.Devices[peer], &peerCursor, []protocol.Mutation{})
		found := 0
		for _, change := range pull.Changes {
			if change.EntityID == entity && change.Cursor.FamilyID == d.FamilyID {
				found++
			}
		}
		if pullErr == nil && (found != 1 || pull.NextPageToken != nil) {
			pullErr = errors.New("capacity_peer_effect_missing_or_duplicate")
		}
		mu.Lock()
		if measured {
			r.recordPhase(&r.PeerPulls, pullErr, "measurement_peer")
		} else {
			phase := "setup"
			if counter == &r.Warmup {
				phase = "warmup"
			}
			r.recordPhase(counter, pullErr, phase+"_peer")
		}
		if pullErr == nil {
			cursors[peer] = pull.HighWater
			if measured {
				r.PeerApplied++
				r.PeerHTTPSeconds = append(r.PeerHTTPSeconds, time.Since(start).Seconds())
			}
		}
		mu.Unlock()
		return pullErr
	}
	// A real authenticated mutation and distinct-device pull must succeed before
	// admitting steady offered load.
	if err := operation(ctx, 0, false, &r.Setup, nil); err != nil {
		_ = checkpoint()
		return r, errors.New("capacity_proof_pair_failed")
	}
	r.ProofAcknowledged = 1
	r.ProofPeerObserved = 1
	if w.Contention {
		proofCtx, cancelProof := context.WithCancel(ctx)
		barrier := make(chan struct{})
		outcomes := make(chan error, 2)
		for writer := 0; writer < 2; writer++ {
			go func(writer int) { <-barrier; outcomes <- operation(proofCtx, writer, false, &r.Setup, nil) }(writer)
		}
		close(barrier)
		failed, pairs := false, 0
		for i := 0; i < 2; i++ {
			if err := <-outcomes; err != nil {
				failed = true
				cancelProof()
			} else {
				pairs++
			}
		}
		cancelProof()
		// Every started operation has returned before reading/checkpointing r.
		r.ContentionProofPairs = pairs
		if failed {
			_ = checkpoint()
			return r, errors.New("capacity_contention_proof_failed")
		}
	}
	// The MCP shape is proven before offered load: the route is present only
	// in MCP-enabled cells, and each read/abusive client is admitted once.
	if w.MCPMode != "" || t.mcpRouteChecked {
		proof := r.MCP
		if proof == nil {
			proof = &MCPEvidence{}
		}
		if err := mcpSetup(ctx, w, t, proof); err != nil {
			_ = checkpoint()
			return r, err
		}
	}
	if err := checkpoint(); err != nil {
		return r, err
	}
	r.SetupSeconds += time.Since(began).Seconds()
	r.ActualWriters = w.ActiveWriters
	runWindow := func(duration time.Duration, measured bool) error {
		window := newMeasurementWindow(ctx, duration, 10*time.Second)
		defer window.Close()
		start := window.Start
		windowCtx := window.ScheduleContext
		if measured {
			r.PlannedEmptyPulls = w.Devices * int((duration-1)/w.EmptyPullInterval)
			if w.Measure > 10*time.Minute {
				r.PlannedRefresh = w.Devices
				r.PlannedReconnect = w.Devices
			}
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(w.EmptyPullInterval)
			defer ticker.Stop()
			for {
				select {
				case <-windowCtx.Done():
					return
				case <-ticker.C:
					_, _ = runLockedBatch(window, ctx, w.Devices, func(i int) *sync.RWMutex { return &familyLocks[i/2] }, func(requestCtx context.Context, i int) error {
						d := t.Devices[i]
						mu.Lock()
						cursor := cursors[i]
						mu.Unlock()
						_, err := syncCall(requestCtx, t, d, &cursor, []protocol.Mutation{})
						mu.Lock()
						if measured {
							r.recordPhase(&r.EmptyPulls, err, "measurement_empty_pull")
						} else {
							r.recordPhase(&r.Warmup, err, "warmup_empty_pull")
						}
						mu.Unlock()
						return nil // Every admitted HTTP outcome is recorded; continue other devices while open.
					})
				}
			}
		}()
		if measured && w.Measure > 10*time.Minute {
			wg.Add(1)
			go func() {
				defer wg.Done()
				timer := time.NewTimer(time.Until(earliest.Add(-2 * time.Minute)))
				defer timer.Stop()
				select {
				case <-windowCtx.Done():
					return
				case <-timer.C:
				}
				for i := 0; i < w.Devices; i += 2 {
					if err := renewFamily(ctx, i, true, window); err != nil {
						return
					}
				}
			}()
		}
		// MCP load shares the window schedule; it never takes family locks.
		runMCPClients(w, t, window, measured, r.MCP, &mu, &wg)
		interval := time.Minute / time.Duration(w.WritesPerWriterPerMinute)
		for writer := 0; writer < w.ActiveWriters; writer++ {
			wg.Add(1)
			go func(writer int) {
				defer wg.Done()
				for n := 0; ; n++ {
					// Stagger across the interval, and use a monotonic fixed offered schedule.
					when := start.Add(time.Duration(n)*interval + time.Duration(writer)*interval/time.Duration(w.ActiveWriters))
					if !when.Before(start.Add(duration)) {
						return
					}
					timer := time.NewTimer(time.Until(when))
					select {
					case <-windowCtx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
					if !window.CanStart() {
						return
					}
					mu.Lock()
					if measured {
						r.ScheduledWrites++
					}
					lag := time.Since(when)
					if lag > interval {
						if measured {
							r.SkippedWrites++
						}
						mu.Unlock()
						continue
					}
					mu.Unlock()
					requestCtx, cancelRequest := context.WithTimeout(window.RequestContext, 10*time.Second)
					counter := &r.Warmup
					if measured {
						counter = &r.Counts
					}
					_ = operation(requestCtx, writer, measured, counter, window)
					cancelRequest()
					mu.Lock()
					if measured {
						measurement, drain := window.Durations()
						r.MeasurementSeconds = measurement.Seconds()
						r.DrainSeconds = drain.Seconds()
						_ = checkpoint()
					}
					mu.Unlock()
				}
			}(writer)
		}
		wg.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-windowCtx.Done():
		case <-time.After(time.Until(start.Add(duration))):
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if measured {
			measurement, drain := window.Durations()
			r.MeasurementSeconds = measurement.Seconds()
			r.DrainSeconds = drain.Seconds()
			if r.MCP != nil {
				r.MCP.MeasurementSeconds = r.MeasurementSeconds
			}
		}
		return nil
	}
	if err := runWindow(w.Warmup, false); err != nil {
		r.Completion = Interrupted
		_ = checkpoint()
		return r, err
	}
	r.Completion = Interrupted
	if err := checkpoint(); err != nil {
		return r, err
	}
	if err := runWindow(w.Measure, true); err != nil {
		r.Completion = Deadline
		if !errors.Is(err, context.DeadlineExceeded) {
			r.Completion = Interrupted
		}
		_ = checkpoint()
		return r, err
	}
	r.Completion = Complete
	if r.Successes < 200 {
		r.Completion = SampleIncomplete
	}
	r.Live = population.snapshot()
	if w.SlowConsumer {
		r.Live.Observed = false
	}
	r.Finalize()
	err := checkpoint()
	return r, err
}
