package capacity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Matched MCP cells. A sync-only cell leaves MCPMode empty (MCP disabled).
const (
	MCPIdle    = "idle"
	MCPRead    = "read"
	MCPAbusive = "abusive"

	// mcpMaxClients bounds load-generator credentials, not a capacity claim.
	mcpMaxClients = 64
	// mcpAbuseSpacing keeps one abusive worker below 50 requests/second so the
	// host load generator itself is not the bottleneck being measured.
	mcpAbuseSpacing = 20 * time.Millisecond
	mcpProtocol     = "2025-11-25"
)

// Outcomes of one MCP request. Refusals and errors stay visible: nothing is
// dropped or retried to improve the admitted rate.
const (
	mcpAdmitted           = "admitted"
	mcpToolError          = "tool_error"
	mcpRateLimited        = "rate_limited"
	mcpConcurrencyLimited = "concurrency_limited"
	mcpServerBusy         = "server_busy"
	mcpTimeout            = "timeout"
	mcpUnavailable        = "unavailable"
	mcpUnauthorized       = "unauthorized"
	mcpTransport          = "transport"
	mcpOther              = "other"
)

// mcpCredential is private harness state, never written to results.
type mcpCredential struct {
	Credential string
	BabyID     string
}

// MCPEvidence records measured-window MCP traffic separately from phone sync
// counts, so sync acceptance is judged on sync alone and MCP admitted and
// refused rates are reported as they are.
type MCPEvidence struct {
	Mode                   string         `json:"mode"`
	Clients                int            `json:"clients"`
	SetupProofs            int            `json:"setup_proofs"`
	Attempts               int            `json:"attempts"`
	Admitted               int            `json:"admitted"`
	Refused                int            `json:"refused"` // rate_limited + concurrency_limited + server_busy
	Errors                 int            `json:"errors"`  // timeouts, unavailable, unauthorized, tool and other errors
	Outcomes               map[string]int `json:"outcomes"`
	AdmittedLatencySeconds []float64      `json:"admitted_latency_seconds"`
	MeasurementSeconds     float64        `json:"measurement_seconds"`
	AdmittedP50Seconds     float64        `json:"admitted_p50_seconds"`
	AdmittedP95Seconds     float64        `json:"admitted_p95_seconds"`
	AdmittedPerMinute      float64        `json:"admitted_per_minute"`
	RefusedPerMinute       float64        `json:"refused_per_minute"`
}

func mcpRefusal(outcome string) bool {
	return outcome == mcpRateLimited || outcome == mcpConcurrencyLimited || outcome == mcpServerBusy
}

func (e *MCPEvidence) record(outcome string, latency time.Duration) {
	if e.Outcomes == nil {
		e.Outcomes = map[string]int{}
	}
	e.Attempts++
	e.Outcomes[outcome]++
	switch {
	case outcome == mcpAdmitted:
		e.Admitted++
		e.AdmittedLatencySeconds = append(e.AdmittedLatencySeconds, latency.Seconds())
	case mcpRefusal(outcome):
		e.Refused++
	default:
		e.Errors++
	}
}

func (e *MCPEvidence) finalize() {
	e.AdmittedP50Seconds, e.AdmittedP95Seconds, e.AdmittedPerMinute, e.RefusedPerMinute = 0, 0, 0, 0
	if n := len(e.AdmittedLatencySeconds); n > 0 {
		values := append([]float64(nil), e.AdmittedLatencySeconds...)
		sort.Float64s(values)
		e.AdmittedP50Seconds = values[int(math.Ceil(float64(n)*.50))-1]
		e.AdmittedP95Seconds = values[int(math.Ceil(float64(n)*.95))-1]
	}
	if e.MeasurementSeconds > 0 {
		e.AdmittedPerMinute = float64(e.Admitted) * 60 / e.MeasurementSeconds
		e.RefusedPerMinute = float64(e.Refused) * 60 / e.MeasurementSeconds
	}
}

func (e *MCPEvidence) coherent() bool {
	sum, refused, errs := 0, 0, 0
	for outcome, count := range e.Outcomes {
		if count < 0 {
			return false
		}
		sum += count
		switch {
		case outcome == mcpAdmitted:
		case mcpRefusal(outcome):
			refused += count
		default:
			errs += count
		}
	}
	for _, v := range append([]float64{e.MeasurementSeconds}, e.AdmittedLatencySeconds...) {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return e.Attempts == sum && e.Admitted+e.Refused+e.Errors == e.Attempts && e.Refused == refused && e.Errors == errs &&
		e.Outcomes[mcpAdmitted] == e.Admitted && len(e.AdmittedLatencySeconds) == e.Admitted && e.SetupProofs >= 0
}

// classifyMCPResponse reads only the bounded safe machine code of a reply.
func classifyMCPResponse(status int, body []byte) string {
	var envelope struct {
		Result *struct {
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return mcpOther
	}
	if status == http.StatusOK && envelope.Error == nil && envelope.Result != nil {
		if envelope.Result.IsError {
			return mcpToolError
		}
		return mcpAdmitted
	}
	code := ""
	if envelope.Error != nil {
		code = envelope.Error.Data.Code
	}
	switch {
	case status == http.StatusTooManyRequests && code == "rate_limited":
		return mcpRateLimited
	case status == http.StatusTooManyRequests && code == "concurrency_limited":
		return mcpConcurrencyLimited
	case status == http.StatusServiceUnavailable && code == "server_busy":
		return mcpServerBusy
	case status == http.StatusServiceUnavailable && code == "timeout":
		return mcpTimeout
	case status == http.StatusServiceUnavailable:
		return mcpUnavailable
	case status == http.StatusUnauthorized:
		return mcpUnauthorized
	}
	return mcpOther
}

// mcpToolCall cycles the three read tools against the client's own baby.
func mcpToolCall(call int, babyID string, now time.Time) string {
	var name string
	var arguments any
	switch call % 3 {
	case 0:
		name, arguments = "list_babies", map[string]any{}
	case 1:
		name, arguments = "get_history", map[string]any{"baby_id": babyID, "start": now.Add(-24 * time.Hour).UTC().Format(time.RFC3339), "end": now.UTC().Format(time.RFC3339), "limit": 50}
	default:
		name, arguments = "get_summary", map[string]any{"baby_id": babyID, "start": now.Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339), "end": now.UTC().Format(time.RFC3339), "timezone": "UTC"}
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call + 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments}})
	return string(raw)
}

// mcpCall sends one stateless authenticated tool call (no session, no SSE)
// and classifies the reply. Latency covers the full request.
func mcpCall(ctx context.Context, t *runTarget, c mcpCredential, call int, now time.Time) (string, time.Duration) {
	base := t.mcpBase
	if base == "" {
		base = "https://sync.example.test"
	}
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp", bytes.NewReader([]byte(mcpToolCall(call, c.BabyID, now))))
	if err != nil {
		return mcpOther, time.Since(started)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", mcpProtocol)
	request.Header.Set("Authorization", "Bearer "+c.Credential)
	response, err := t.mcpHTTP().Do(request)
	if err != nil {
		var timeout interface{ Timeout() bool }
		if ctx.Err() != nil || (errors.As(err, &timeout) && timeout.Timeout()) {
			return mcpTimeout, time.Since(started)
		}
		return mcpTransport, time.Since(started)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 300<<10))
	if err != nil {
		return mcpTimeout, time.Since(started)
	}
	return classifyMCPResponse(response.StatusCode, body), time.Since(started)
}

func (t *runTarget) mcpHTTP() *http.Client {
	if t.MCPHTTP != nil {
		return t.MCPHTTP
	}
	return t.HTTP
}

// mcpRouteStatus probes /mcp without credentials: 401 proves the route is
// mounted (enabled cells), 404 that it is absent (sync-only cells). The
// probe is refused before admission, authentication or any database work.
func mcpRouteStatus(ctx context.Context, t *runTarget) (int, error) {
	base := t.mcpBase
	if base == "" {
		base = "https://sync.example.test"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return 0, err
	}
	response, err := t.mcpHTTP().Do(request)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	return response.StatusCode, nil
}

// mcpSetup checks the cell's MCP shape before offered load: the route exists
// exactly when MCP is enabled, and each read/abusive client is admitted once.
func mcpSetup(ctx context.Context, w Workload, t *runTarget, evidence *MCPEvidence) error {
	status, err := mcpRouteStatus(ctx, t)
	want := http.StatusUnauthorized
	if w.MCPMode == "" {
		want = http.StatusNotFound
	}
	if err != nil || status != want {
		return fmt.Errorf("capacity_mcp_route_mismatch")
	}
	if w.MCPMode != MCPRead && w.MCPMode != MCPAbusive {
		return nil
	}
	if len(t.MCPCredentials) < w.MCPClients {
		return errors.New("capacity_mcp_credentials_missing")
	}
	for i := range w.MCPClients {
		if outcome, _ := mcpCall(ctx, t, t.MCPCredentials[i], 0, time.Now()); outcome != mcpAdmitted {
			return errors.New("capacity_mcp_setup_failed")
		}
		evidence.SetupProofs++
	}
	return nil
}

// runMCPClients offers MCP load for one window. Read clients are paced on a
// fixed schedule; abusive clients run concurrent unpaced workers per
// credential. Only measured-window outcomes are recorded.
func runMCPClients(w Workload, t *runTarget, window *measurementWindow, measured bool, evidence *MCPEvidence, mu *sync.Mutex, wg *sync.WaitGroup) {
	if w.MCPMode != MCPRead && w.MCPMode != MCPAbusive {
		return
	}
	send := func(client, call int) {
		ctx, cancel := context.WithTimeout(window.RequestContext, 10*time.Second)
		outcome, latency := mcpCall(ctx, t, t.MCPCredentials[client], call, time.Now())
		cancel()
		if measured {
			mu.Lock()
			evidence.record(outcome, latency)
			mu.Unlock()
		}
	}
	for client := range w.MCPClients {
		if w.MCPMode == MCPRead {
			interval := time.Minute / time.Duration(w.MCPCallsPerClientPerMinute)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := 0; ; n++ {
					when := window.Start.Add(time.Duration(n)*interval + time.Duration(client)*interval/time.Duration(w.MCPClients))
					if !when.Before(window.End) {
						return
					}
					timer := time.NewTimer(time.Until(when))
					select {
					case <-window.ScheduleContext.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
					if !window.CanStart() {
						return
					}
					send(client, n)
				}
			}()
			continue
		}
		for worker := range w.MCPAbuseConcurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := worker; window.CanStart(); n++ {
					next := time.Now().Add(mcpAbuseSpacing)
					send(client, n)
					select {
					case <-window.ScheduleContext.Done():
						return
					case <-time.After(time.Until(next)):
					}
				}
			}()
		}
	}
}
