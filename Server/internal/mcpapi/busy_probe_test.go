package mcpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/familyread"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
)

// probeQuota is the production integration/account/family charge shape on
// the real limiter (cmd/baby-tracker-server mcpQuota).
type probeQuota struct {
	limiter  limits.Limiter
	policies map[string]limits.Policy
}

func (q probeQuota) Charge(ctx context.Context, identity authority.IntegrationIdentity) (limits.Decision, error) {
	return q.limiter.TakeMany(ctx, []limits.Charge{
		{Scope: limits.ScopeMCPIntegration, Key: identity.GrantID, Cost: 1, Policy: q.policies[limits.ScopeMCPIntegration]},
		{Scope: limits.ScopeMCPAccount, Key: identity.AccountID, Cost: 1, Policy: q.policies[limits.ScopeMCPAccount]},
		{Scope: limits.ScopeMCPFamily, Key: identity.FamilyID, Cost: 1, Policy: q.policies[limits.ScopeMCPFamily]},
	})
}

func processCPU() time.Duration {
	var usage syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// TestSyntheticBusyMCPClientProbe is an opt-in diagnostic, not an acceptance
// test: one valid credential with two unpaced workers against the real
// router (request logging), real handler, real guard (file fence, ledger
// inventory verification, projection, family policy), real keyed gate and
// the real database limiter. The authority ledger is grown to a fixture-like
// head first. Connections are not reused, like the lab proxy's upstream.
// BT_PERF_PROFILE writes a CPU profile of the measured window.
func TestSyntheticBusyMCPClientProbe(t *testing.T) {
	if os.Getenv("BT_MCP_PERF_PROBE") != "1" {
		t.Skip("opt-in synthetic MCP performance probe")
	}
	f := newDBFixture(t, nil)
	// Grow the family's audit chain to a fixture-like length first.
	for i := 0; ; i++ {
		revision, err := f.h.Authority.FamilyRevision(t.Context(), f.h.FamilyID)
		if err != nil {
			t.Fatal(err)
		}
		if revision >= 280 {
			break
		}
		grantID := fmt.Sprintf("61000000-0000-4000-8000-%012x", i+1)
		f.grant(t, grantID)
		f.revoke(t, grantID)
	}
	head, err := f.h.Authority.FamilyRevision(t.Context(), f.h.FamilyID)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{1}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	reader := f.h.IntegrationReader(t)
	var key [32]byte
	key[0] = 1
	controller, _ := admission.New(32, 4)
	gate, _ := admission.NewKeyed(4)
	listenerURL := httptest.NewUnstartedServer(nil)
	origin := "http://" + listenerURL.Listener.Addr().String()
	options := Options{Reader: familyread.New(reader, key, func() time.Time { return f.h.Now }), Authenticator: reader, Admission: controller,
		Gate: gate, Quota: probeQuota{limiter: limiter, policies: limits.DefaultPolicies()}, CanonicalOrigin: origin}
	mcpHandler, err := NewHandler(options)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	listenerURL.Config.Handler = httpapi.New(httpapi.Options{Logger: slog.New(slog.NewJSONHandler(logFile, nil)), Admission: controller, MCPHandler: mcpHandler})
	listenerURL.Start()
	defer listenerURL.Close()
	duration := 15 * time.Second
	if raw := os.Getenv("BT_MCP_PERF_SECONDS"); raw != "" {
		if parsed, err := time.ParseDuration(raw + "s"); err == nil {
			duration = parsed
		}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	body := toolCall(1, "list_babies", `{}`)
	var mu sync.Mutex
	outcomes := map[string]int{}
	var latencies []time.Duration
	if path := os.Getenv("BT_PERF_PROFILE"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.StartCPUProfile(file); err != nil {
			t.Fatal(err)
		}
		defer func() { pprof.StopCPUProfile(); file.Close() }()
	}
	cpuStart, started := processCPU(), time.Now()
	deadline := started.Add(duration)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				request, _ := http.NewRequest(http.MethodPost, listenerURL.URL+"/mcp", strings.NewReader(body))
				for name, value := range legacyHeaders() {
					request.Header.Set(name, value)
				}
				request.Header.Set("Authorization", "Bearer "+f.credential)
				begin := time.Now()
				response, err := client.Do(request)
				outcome := "transport"
				if err == nil {
					raw, _ := io.ReadAll(response.Body)
					response.Body.Close()
					outcome = fmt.Sprintf("%d", response.StatusCode)
					for _, code := range []string{"rate_limited", "concurrency_limited", "server_busy", "unauthorized", "unavailable", "timeout"} {
						if strings.Contains(string(raw), `"code":"`+code+`"`) {
							outcome += " " + code
						}
					}
				}
				mu.Lock()
				outcomes[outcome]++
				latencies = append(latencies, time.Since(begin))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	wall, cpu := time.Since(started), processCPU()-cpuStart
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	total := len(latencies)
	t.Logf("PROBE family_revision=%d seconds=%.1f requests=%d rps=%.1f process_cpu_s=%.2f cpu_ms_per_request=%.3f p50_ms=%.3f p95_ms=%.3f outcomes=%v",
		head, wall.Seconds(), total, float64(total)/wall.Seconds(), cpu.Seconds(), float64(cpu.Microseconds())/1000/float64(total),
		float64(latencies[total/2].Microseconds())/1000, float64(latencies[(total*95-1)/100].Microseconds())/1000, outcomes)
}
