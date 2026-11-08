package loadtest

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/netip"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

// floodClass is floodExtras' class named class.
func floodClass(t *testing.T, g *generator, class string) extraClass {
	t.Helper()
	for _, c := range floodExtras(g) {
		if c.class == class {
			return c
		}
	}
	t.Fatalf("no %s class", class)
	return extraClass{}
}

// Task 19's IP flood fills the IP scope pools it reaches (auth-challenge-ip,
// auth-refresh-ip): more distinct addresses than a scope's pool holds,
// with margin (the pool is 64 shards of cap/64 buckets, so every shard
// must overflow), each sending one auth/challenge and one refresh with a
// garbage token a cycle, every address's bucket live within one window.
func TestFloodIPCellOverflowsTheIPScopePools(t *testing.T) {
	if floodIPAddresses < limits.DefaultMaxIPEntriesPerScope*5/4 {
		t.Fatalf("%d addresses against a pool of %d a scope", floodIPAddresses, limits.DefaultMaxIPEntriesPerScope)
	}
	policies := limits.DefaultPolicies()
	for _, scope := range floodEvictionScopes {
		if window := policies[scope].Window; floodIPCycle >= window {
			t.Fatalf("an address returns every %v, past %s's %v window", floodIPCycle, scope, window)
		}
	}
	if got := floodIPAddress(0); got != "198.18.0.0" {
		t.Fatalf("first address %s", got)
	}
	if got := floodIPAddress(floodIPAddresses - 1); got != "198.19.127.255" {
		t.Fatalf("last address %s", got)
	}
	if floodIPAddress(floodIPAddresses) != floodIPAddress(0) {
		t.Fatal("the addresses do not cycle")
	}
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	c := floodClass(t, g, "flood_ip")
	if !c.attack || math.Abs(c.rate-float64(floodIPAddresses)*2/floodIPCycle.Seconds()) > 1e-9 {
		t.Fatalf("rate %v attack %v", c.rate, c.attack)
	}
	for range 6 {
		c.op(t.Context(), time.Now())
	}
	challenges, refreshes := f.all("/v1/auth/challenge"), f.all("/v1/auth/refresh-recoverable")
	if len(challenges) != 3 || len(refreshes) != 3 {
		t.Fatalf("%d challenges, %d refreshes", len(challenges), len(refreshes))
	}
	prefix := netip.MustParsePrefix("198.18.0.0/15")
	seen := map[string]int{}
	for i, r := range append(challenges, refreshes...) {
		address := r.Header.Get("X-Forwarded-For")
		if a, err := netip.ParseAddr(address); err != nil || !prefix.Contains(a) {
			t.Fatalf("request %d from %q", i, address)
		}
		seen[address]++
	}
	if len(seen) != 3 {
		t.Fatalf("addresses %v: each sends one challenge and one refresh", seen)
	}
	for _, r := range challenges {
		var body protocol.ChallengeRequest
		if err := json.Unmarshal(r.Body, &body); err != nil || body.ProtocolVersion != "v1" ||
			strings.Join(body.Capabilities, ",") != protocol.CapabilityIdentityV1 || r.Header.Get("Authorization") != "" {
			t.Fatalf("challenge %s: %v", r.Body, err)
		}
	}
	for _, r := range refreshes {
		var body protocol.RecoverableRefreshRequest
		if err := json.Unmarshal(r.Body, &body); err != nil || body.RefreshToken == "" || validBearer(body.RefreshToken) {
			t.Fatalf("refresh %s: %v", r.Body, err)
		}
	}
	snapshot := g.attack.Snapshot()
	if snapshot[RouteFloodChallenge].Latency.Total != 3 || snapshot[RouteFloodRefresh].Latency.Total != 3 {
		t.Fatalf("attack routes %v", snapshot)
	}
}

// Task 19's bad bearers: 5,000 other addresses of 198.18.0.0/15 each send
// 1 MiB POST /v1/sync bodies with well-formed unknown bearers, at the
// bearer-failure-ip limit (60 per 10 minutes: 500 a second). A well-formed
// bearer passes the format check, so the server reads the body and charges
// the address's failed-bearer budget.
func TestFloodBadBearerCellSendsFullBodiesWithWellFormedUnknownBearers(t *testing.T) {
	if floodBearerBodyBytes != 1<<20 {
		t.Fatalf("body %d bytes", floodBearerBodyBytes)
	}
	prefix := netip.MustParsePrefix("198.18.0.0/15")
	ip := map[string]bool{}
	for i := range floodIPAddresses {
		ip[floodIPAddress(i)] = true
	}
	for i := range floodBearerAddresses {
		a := floodBearerAddress(i)
		if ip[a] || !prefix.Contains(netip.MustParseAddr(a)) {
			t.Fatalf("bearer address %d %s", i, a)
		}
	}
	if floodBearerAddress(floodBearerAddresses) != floodBearerAddress(0) {
		t.Fatal("the addresses do not cycle")
	}
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	c := floodClass(t, g, "flood_bearer")
	if !c.attack || math.Abs(c.rate-5000*60/600.0) > 1e-9 {
		t.Fatalf("rate %v attack %v", c.rate, c.attack)
	}
	for range 3 {
		c.op(t.Context(), time.Now())
	}
	syncs := f.all("/v1/sync")
	if len(syncs) != 3 {
		t.Fatalf("%d syncs", len(syncs))
	}
	bearers, addresses := map[string]bool{}, map[string]bool{}
	for _, r := range syncs {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !validBearer(bearer) {
			t.Fatalf("bearer %q is not well formed", bearer)
		}
		bearers[bearer], addresses[r.Header.Get("X-Forwarded-For")] = true, true
		if len(r.Body) != floodBearerBodyBytes || !json.Valid(r.Body) {
			t.Fatalf("body %d bytes, valid JSON %v", len(r.Body), json.Valid(r.Body))
		}
	}
	if len(bearers) != 3 || len(addresses) != 3 {
		t.Fatalf("%d bearers from %d addresses", len(bearers), len(addresses))
	}
	if got := g.attack.Snapshot()[RouteFloodBadBearer].Latency.Total; got != 3 {
		t.Fatalf("flood_bad_bearer %d", got)
	}
}

// validBearer is the server's format check (auth validBearerSecret): raw
// base64url of 32 bytes.
func validBearer(raw string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == raw
}
