package loadtest

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

// withHotFamily adds the seeder's hot family (`seed --hot-family`: an owner
// and four caregivers, two devices each) to cfg's fleet and names it in the
// header, as loadseed does. It returns the family's ID.
func withHotFamily(t *testing.T, cfg RunConfig) string {
	t.Helper()
	header, states, err := ReadFleet(cfg.Fleet)
	if err != nil {
		t.Fatal(err)
	}
	family := "e0000000-0000-4000-8000-00000000f00d"
	baby := "e0000000-0000-4000-8000-0000000000b1"
	for account := range 5 {
		role := "caregiver"
		if account == 0 {
			role = "owner"
		}
		for slot := range 2 {
			n := len(states)
			s := DeviceState{FamilyID: family, AccountID: fmt.Sprintf("e0000000-0000-4000-8000-%012d", 100+account),
				DeviceID: fmt.Sprintf("e0000000-0000-4000-8000-%012d", 200+2*account+slot), Role: role, BabyIDs: []string{baby},
				AccessToken: fmt.Sprintf("access-hot-%d", n), RefreshToken: fmt.Sprintf("h%042d", n), DeviceCredential: fmt.Sprintf("hc%041d", n),
				AccessExpiresAt: time.Now().Add(10 * time.Minute), ReauthenticateAt: time.Now().Add(29 * 24 * time.Hour),
				ClientIP: fmt.Sprintf("100.65.0.%d", 1+n%250)}
			bearerFamilies.Store(s.AccessToken, family)
			bearerFamilies.Store(s.RefreshToken, family)
			states = append(states, s)
		}
	}
	header.Families++
	header.Devices = len(states)
	header.HotFamilyID = family
	if err := WriteFleet(cfg.Fleet, header, states); err != nil {
		t.Fatal(err)
	}
	return family
}

// Task 20 Step 2: the seeder's hot family (5 accounts × 2 devices, named in
// the fleet header) is the cell's, whatever family has more writers, and it
// writes the plan's 50 mutations a second. Its sync budget at 80% of the
// limits is 16 requests a second (each account's 2 devices at 120/min meet
// the 240/min account limit), so the writes travel 4 to a request at 12.5
// requests a second; no device or account exceeds its limit, and the model
// is sized to the other families.
func TestSeededHotFamilyWritesFiftyMutationsASecondWithinTheLimits(t *testing.T) {
	f := newFakeV1(t)
	cfg := runConfig(t, f, 4, Model{RefreshPerSecond: 0.5, PullPerSecond: 10, WritePerSecond: 2, LiveSockets: 8})
	hot := withHotFamily(t, cfg)
	header, states, err := ReadFleet(cfg.Fleet)
	if err != nil {
		t.Fatal(err)
	}
	g := newGenerator(RunConfig{Seed: 7}, header, states)
	if want := Model10k().Scale(4.0 / modelFamilies); g.modelBase != want {
		t.Fatalf("model %+v, want the 4 families' %+v", g.modelBase, want)
	}
	h := planHotFamily(g)
	if h.family != hot || !h.seeded || len(h.accounts) != 5 || math.Abs(h.budget-16) > 1e-9 || math.Abs(h.perRequest-3.125) > 1e-9 ||
		math.Abs(h.rate-16) > 1e-9 {
		t.Fatalf("hot family %s seeded %v accounts %d budget %v: %v a request at %v/s", h.family, h.seeded, len(h.accounts), h.budget, h.perRequest, h.rate)
	}
	// 3 or 4 mutations a request, 3.125 on average.
	sum := 0
	for range 80000 {
		n := h.mutations(g.rng)
		if n != 3 && n != 4 {
			t.Fatalf("a request of %d mutations", n)
		}
		sum += n
	}
	if mean := float64(sum) / 80000; math.Abs(mean-3.125) > 0.01 {
		t.Fatalf("%v mutations a request on average, want 3.125", mean)
	}
	const picks = 100000
	perDevice, perAccount := map[string]int{}, map[string]int{}
	for range picks {
		d := h.pick(g.rng)
		perDevice[d.DeviceID]++
		perAccount[d.AccountID]++
	}
	perMinute := func(n int) float64 { return float64(n) / picks * h.rate * 60 }
	for id, n := range perDevice {
		if got := perMinute(n); got > 120*1.02 {
			t.Errorf("device %s syncs %.0f/min, over 120", id, got)
		}
	}
	for id, n := range perAccount {
		if got := perMinute(n); got > 240*1.02 {
			t.Errorf("account %s syncs %.0f/min, over 240", id, got)
		}
	}
	if len(perDevice) != 10 {
		t.Fatalf("%d of the 10 devices write", len(perDevice))
	}
	// An account's two devices take turns: their counts differ by one at
	// most, so neither drifts over its per-minute share in a window.
	byAccount := map[string][]int{}
	for _, a := range h.accounts {
		for _, d := range a {
			byAccount[d.AccountID] = append(byAccount[d.AccountID], perDevice[d.DeviceID])
		}
	}
	for account, counts := range byAccount {
		if len(counts) == 2 && (counts[0]-counts[1] > 1 || counts[1]-counts[0] > 1) {
			t.Errorf("account %s's devices took %v picks: not in turn", account, counts)
		}
	}

	cfg.Scenario, cfg.Warmup, cfg.Measure = "hot-family", 200*time.Millisecond, 2*time.Second
	if _, err := Run(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	r := readResult(t, cfg.Out)
	cell, ok := r.Cells["hot_family"].(map[string]any)
	if !ok || cell["family_id"] != hot || cell["seeded"] != true || cell["mutations_per_request"] != 3.125 ||
		cell["target_mutations_per_second"] != float64(50) || cell["target_requests_per_second"] != float64(16) {
		t.Fatalf("hot_family cell %v", r.Cells)
	}
	requests, mutations := cell["achieved_requests_per_second"].(float64), cell["achieved_mutations_per_second"].(float64)
	if requests < 8 || mutations < 25 || mutations < 3*requests || mutations > 4*requests {
		t.Fatalf("achieved %v requests and %v mutations a second, want about 16 and 50", requests, mutations)
	}
	written, batches := 0, map[int]int{}
	for _, req := range f.all("/v1/sync") {
		v, _ := bearerFamilies.Load(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		if v != hot || req.At.Before(r.WindowStart) || !req.At.Before(r.MeasureEnd) {
			continue
		}
		var body protocol.SyncRequest
		if err := json.Unmarshal(req.Body, &body); err != nil || len(body.Mutations) < 3 || len(body.Mutations) > 4 {
			t.Fatalf("a hot-family write carried %d mutations, want 3 or 4", len(body.Mutations))
		}
		written++
		batches[len(body.Mutations)]++
	}
	if batches[3] == 0 || batches[4] == 0 {
		t.Errorf("batches %v: want requests of 3 and of 4", batches)
	}
	if written == 0 {
		t.Fatal("the hot family wrote nothing")
	}
}
