package liveactivity

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/testkit"
)

// Each minute's sweep logs one line at Info, numbers only: its duration,
// the registration pages and revocations, the work-sweep pages (one family:
// one short page), the rows retention removed, and
// the claim calls since the previous sweep (how many, how many claimed a
// job, their mean and longest latency), for the capacity runs.
func TestLiveActivitySweepLogsItsDurationAndClaims(t *testing.T) {
	h, dirty, reg := workerHarness(t)
	workerBaby(t, h)
	if _, err := reg.device(t, t.Context(), h.AccessToken("caregiver"), deviceRequest()); err != nil {
		t.Fatal(err)
	}
	h.StartFeed(t, h.Owner, testkit.TimerA)
	logs := &logCapture{}
	w := newTestWorker(t, h, dirty, h.Authority, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		return PushResult{HTTPStatus: 200, Classification: Accepted}, nil
	}))
	w.Logger = slog.New(slog.NewTextHandler(logs, nil))
	clock := newWorkerClock(h)
	w.Now = clock.Now
	// The first pass sweeps at once (no claim before it), then claims the
	// start push and finds nothing more.
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if err := w.RunOnce(t.Context(), 32); err != nil {
		t.Fatal(err)
	}
	var sweeps []map[string]int
	field := regexp.MustCompile(`(\w+)=(-?\d+)`)
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if !strings.Contains(line, `msg="live activity sweep"`) {
			continue
		}
		if !strings.Contains(line, "level=INFO") {
			t.Fatalf("sweep line %q, want Info", line)
		}
		values := map[string]int{}
		for _, match := range field.FindAllStringSubmatch(line, -1) {
			values[match[1]], _ = strconv.Atoi(match[2])
		}
		sweeps = append(sweeps, values)
	}
	if len(sweeps) != 2 {
		t.Fatalf("sweep lines %v in %s, want two", sweeps, logs.String())
	}
	for i, sweep := range sweeps {
		for _, key := range []string{"duration_ms", "pages", "revoked", "work_pages", "retained", "claim_calls", "claimed", "claim_mean_us", "claim_max_us"} {
			if _, ok := sweep[key]; !ok {
				t.Fatalf("sweep %d lacks %s: %v", i, key, sweep)
			}
		}
		if sweep["pages"] != 1 || sweep["revoked"] != 0 || sweep["work_pages"] != 1 {
			t.Fatalf("sweep %d: %v, want one registration page, nothing revoked and one work page", i, sweep)
		}
	}
	if first := sweeps[0]; first["claim_calls"] != 0 || first["claimed"] != 0 {
		t.Fatalf("the first sweep reports claims %v before any", first)
	}
	// The first pass's claims: each job it claimed, then one call that found
	// nothing (ending the pass).
	if second := sweeps[1]; second["claimed"] < 1 || second["claim_calls"] != second["claimed"]+1 || second["claim_max_us"] < second["claim_mean_us"] || second["claim_max_us"] <= 0 {
		t.Fatalf("the second sweep %v, want the first pass's claim calls, the start push among them", second)
	}
}
