package loadtest

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// --flood runs each attack alone or both, and the cell says which, with
// the IP scopes it must make evict.
func TestFloodModeRunsEachAttackAlone(t *testing.T) {
	for mode, want := range map[string][]string{"ip": {"flood_ip"}, "bearer": {"flood_bearer"}, "both": {"flood_ip", "flood_bearer"},
		"": {"flood_ip", "flood_bearer"}} {
		f := newFakeV1(t)
		g := testGenerator(t, f, 1)
		g.cfg.Flood = mode
		var got []string
		for _, c := range floodExtras(g) {
			got = append(got, c.class)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("--flood %q: classes %v, want %v", mode, got, want)
		}
	}
	cfg := RunConfig{Base: "http://x", Host: "h", Fleet: "f", Ledger: "l", Tracked: "t", Out: "o", Scenario: "flood", Load: 1, Measure: time.Minute,
		Flood: "ipv6"}
	if cfg.validate() == nil {
		t.Fatal("--flood ipv6 accepted")
	}
	cfg.Scenario, cfg.Flood = "soak", "ip"
	if cfg.validate() == nil {
		t.Fatal("--flood outside the flood scenario accepted")
	}
}

// An attack class's release lag and drops are its own (scheduler_lag_by_class)
// and never reach the run's gates: the cells are a deliberate overload of
// the generator's own network, and the legitimate classes' lag still
// judges whether the generator kept their schedule.
func TestAttackClassesStayOutOfTheSchedulerLagGate(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	g.phases = newPhases(time.Now())
	block := make(chan struct{})
	defer close(block)
	attack := extraClass{class: "flood_bearer", rate: 400, attack: true, op: func(ctx context.Context, _ time.Time) {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}}
	g.runBackground(t.Context(), 1e-9, time.Now(), 500*time.Millisecond, 0, []extraClass{attack})
	if n := g.phases.health.lag.Total; n != 0 {
		t.Fatalf("the gate saw %d attack arrivals", n)
	}
	if g.phases.health.dropped != 0 {
		t.Fatalf("the gate saw %d attack drops", g.phases.health.dropped)
	}
	if lags := g.lags.summary(); lags["flood_bearer"].Arrivals == 0 {
		t.Fatalf("the attack's own lag is not recorded: %v", lags)
	}
}

// The cell records what was offered and achieved per attack route, the
// bytes sent, the generator's CPU over the measurement, and the scopes
// whose evictions the runner checks.
func TestFloodCellRecordsOfferedAchievedBytesAndCPU(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	g.cfg.Measure = 2 * time.Second
	classes := floodExtras(g)
	for _, c := range classes {
		for range 4 {
			c.op(t.Context(), time.Now())
		}
	}
	for _, c := range classes {
		if c.report != nil {
			c.report()
		}
	}
	cell, _ := g.cells["flood"].(map[string]any)
	if cell == nil || cell["mode"] != "both" {
		t.Fatalf("flood cell %v", g.cells["flood"])
	}
	routes, _ := cell["routes"].(map[string]floodRouteFigures)
	challenge, bearer := routes[RouteFloodChallenge], routes[RouteFloodBadBearer]
	if challenge.Sent != 2 || math.Abs(challenge.OfferedPerSecond-floodIPRate/2) > 1e-9 || math.Abs(challenge.AchievedPerSecond-1) > 1e-9 ||
		challenge.BytesSent == 0 {
		t.Fatalf("challenge figures %+v", challenge)
	}
	if bearer.Sent != 4 || bearer.BytesSent != int64(4*floodBearerBodyBytes) || math.Abs(bearer.AchievedPerSecond-2) > 1e-9 {
		t.Fatalf("bearer figures %+v", bearer)
	}
	if _, ok := cell["generator_system_cpu_seconds"].(float64); !ok {
		t.Fatalf("no generator CPU: %v", cell)
	}
	if got, _ := cell["evictions_expected"].([]string); strings.Join(got, ",") != "auth-challenge-ip,auth-refresh-ip" {
		t.Fatalf("evictions expected %v", cell["evictions_expected"])
	}
}
