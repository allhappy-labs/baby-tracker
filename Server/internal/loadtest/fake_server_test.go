package loadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"babytracker/server/internal/protocol"
)

// fakeV1 is an httptest fake of the v1 routes the generator calls. It
// answers instantly and correctly by default; tests replace a route's
// behaviour with a hook. It records every request.
type fakeV1 struct {
	*httptest.Server
	t        testing.TB
	instance string

	mu       sync.Mutex
	requests []recorded
	counts   map[string]int
	seq      map[string]uint64           // family -> head
	children map[string]refreshChild     // attempt -> committed child
	sockets  map[*websocket.Conn]string  // open sockets -> bearer
	attempts map[string][]string         // parent refresh token -> attempt IDs seen
	mutIDs   map[string]protocol.Outcome // mutation ID -> first outcome (replayed as already_applied)
	hooks    map[string]func(w http.ResponseWriter, r *http.Request, body []byte) bool
	issued   atomic.Int64
	// hintDelay, when set, makes the fake a hub: a write's hint reaches the
	// family's open sockets this long after the write was answered.
	hintDelay time.Duration
	// lapsed holds access tokens the fake refuses 401 access_expired on
	// every route, as the server does once a token's lifetime has passed.
	lapsed sync.Map
}

type recorded struct {
	Method, Path, Host string
	Header             http.Header
	Body               []byte
	At                 time.Time
}

type refreshChild struct {
	access, refresh string
}

func newFakeV1(t testing.TB) *fakeV1 {
	f := &fakeV1{t: t, instance: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", counts: map[string]int{}, seq: map[string]uint64{},
		children: map[string]refreshChild{}, sockets: map[*websocket.Conn]string{}, attempts: map[string][]string{},
		mutIDs: map[string]protocol.Outcome{}, hooks: map[string]func(http.ResponseWriter, *http.Request, []byte) bool{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	t.Cleanup(f.closeSockets)
	return f
}

// hook replaces route's answer while it returns true.
func (f *fakeV1) hook(route string, h func(w http.ResponseWriter, r *http.Request, body []byte) bool) {
	f.mu.Lock()
	f.hooks[route] = h
	f.mu.Unlock()
}

func (f *fakeV1) count(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[route]
}

func (f *fakeV1) last() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *fakeV1) all(path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeV1) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.URL.Path != "/v1/live" {
		body, _ = io.ReadAll(r.Body)
	}
	route := r.Method + " " + r.URL.Path
	if strings.HasPrefix(r.URL.Path, "/v1/export/") {
		route = r.Method + " /v1/export/"
	}
	f.mu.Lock()
	f.requests = append(f.requests, recorded{Method: r.Method, Path: r.URL.Path, Host: r.Host, Header: r.Header.Clone(), Body: body, At: time.Now()})
	f.counts[route]++
	hook := f.hooks[route]
	f.mu.Unlock()
	if _, gone := f.lapsed.Load(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); gone {
		writeErrorResponse(w, http.StatusUnauthorized, "access_expired")
		return
	}
	if hook != nil && hook(w, r, body) {
		return
	}
	switch route {
	case "POST /v1/sync":
		f.syncAs(w, body, f.familyOfBearer(r))
	case "GET /v1/sync/snapshot":
		var familyID string
		f.mu.Lock()
		familyID = f.familyOfBearer(r)
		head := f.seq[familyID]
		f.mu.Unlock()
		writeJSONResponse(w, http.StatusOK, protocol.SnapshotResponse{ServerInstanceID: f.instance, RecoveryEpoch: 1, SnapshotID: newUUID(),
			HighWater: protocol.Cursor{ServerInstanceID: f.instance, RecoveryEpoch: 1, FamilyID: familyID, Sequence: head},
			Entities:  []protocol.CanonicalEntity{}})
	case "POST /v1/auth/refresh-recoverable":
		f.refresh(w, body)
	case "POST /v1/auth/refresh-ack":
		w.WriteHeader(http.StatusNoContent)
	case "GET /v1/live":
		f.live(w, r)
	case "PUT /v1/devices/push-token":
		w.WriteHeader(http.StatusNoContent)
	case "PUT /v1/live-activities/device":
		writeJSONResponse(w, http.StatusOK, map[string]any{"protocol_version": "v1"})
	case "POST /mcp":
		writeJSONResponse(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []any{}, "isError": false}})
	default:
		writeErrorResponse(w, http.StatusNotFound, "not_found")
	}
}

// bearerFamilies maps access tokens to families (fakeFleet fills it).
var bearerFamilies sync.Map

func (f *fakeV1) familyOfBearer(r *http.Request) string {
	if v, ok := bearerFamilies.Load(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); ok {
		return v.(string)
	}
	return "f0000000-0000-4000-8000-000000000000"
}

func (f *fakeV1) sync(w http.ResponseWriter, body []byte) { f.syncAs(w, body, "") }

// syncAs answers a sync; one without a cursor is the bearer's family's.
func (f *fakeV1) syncAs(w http.ResponseWriter, body []byte, bearerFamily string) {
	var request struct {
		Cursor    *protocol.Cursor  `json:"cursor"`
		Mutations []json.RawMessage `json:"mutations"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid_request")
		return
	}
	familyID := "f0000000-0000-4000-8000-000000000000"
	if request.Cursor != nil {
		familyID = request.Cursor.FamilyID
	} else if bearerFamily != "" {
		familyID = bearerFamily
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	outcomes := []protocol.Outcome{}
	for _, raw := range request.Mutations {
		m, err := protocol.DecodeMutation(raw)
		if err != nil {
			f.t.Errorf("the generator sent an invalid mutation: %v\n%s", err, raw)
			writeErrorResponse(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if prior, ok := f.mutIDs[m.MutationID]; ok {
			replay := prior
			original := prior.Status
			replay.Status, replay.OriginalStatus = "already_applied", &original
			outcomes = append(outcomes, replay)
			continue
		}
		f.seq[familyID]++
		o := fakeOutcome(f.instance, familyID, m, f.seq[familyID])
		f.mutIDs[m.MutationID] = o
		outcomes = append(outcomes, o)
	}
	if f.hintDelay > 0 && len(outcomes) > 0 {
		hint := protocol.Hint{FamilyID: familyID, Cursor: protocol.Cursor{ServerInstanceID: f.instance, RecoveryEpoch: 1, FamilyID: familyID,
			Sequence: f.seq[familyID]}}
		time.AfterFunc(f.hintDelay, func() { f.sendFamilyHint(hint) })
	}
	writeJSONResponse(w, http.StatusOK, protocol.SyncResponse{ServerInstanceID: f.instance, RecoveryEpoch: 1, Outcomes: outcomes,
		Changes: []protocol.JournalChange{}, HighWater: protocol.Cursor{ServerInstanceID: f.instance, RecoveryEpoch: 1, FamilyID: familyID,
			Sequence: f.seq[familyID]}})
}

// fakeOutcome applies m the way the server answers: the canonical entity at
// observed+1, tombstoned for a delete or a timer stop.
func fakeOutcome(instance, familyID string, m protocol.Mutation, seq uint64) protocol.Outcome {
	accepted := stamp(time.Now())
	entity := protocol.CanonicalEntity{EntityKind: m.EntityKind, Payload: m.Payload, Meta: protocol.EntityMeta{ID: m.EntityID, FamilyID: familyID,
		EntityVersion: m.ObservedEntityVersion + 1, CreatorAccountID: "c0000000-0000-4000-8000-000000000001",
		LastEditorAccountID: "c0000000-0000-4000-8000-000000000001", ClientMutationTime: m.ClientMutationTime, AcceptedAt: accepted}}
	if m.Operation == "delete" || m.Operation == "timer_stop" {
		entity.Payload, entity.Meta.TombstonedAt = nil, &accepted
	}
	cursor := protocol.Cursor{ServerInstanceID: instance, RecoveryEpoch: 1, FamilyID: familyID, Sequence: seq}
	return protocol.Outcome{MutationID: m.MutationID, Status: "applied", CanonicalEntity: &entity, RelatedEntities: []protocol.CanonicalEntity{},
		Cursor: &cursor}
}

func (f *fakeV1) refresh(w http.ResponseWriter, body []byte) {
	var request protocol.RecoverableRefreshRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid_request")
		return
	}
	f.mu.Lock()
	f.attempts[request.RefreshToken] = append(f.attempts[request.RefreshToken], request.RefreshAttemptID)
	child, ok := f.children[request.RefreshAttemptID]
	if !ok {
		n := f.issued.Add(1)
		child = refreshChild{access: fmt.Sprintf("access-child-%d", n), refresh: fmt.Sprintf("%043d", n)}
		f.children[request.RefreshAttemptID] = child
		if family, ok := bearerFamilies.Load(request.RefreshToken); ok {
			bearerFamilies.Store(child.access, family)
			bearerFamilies.Store(child.refresh, family)
		}
	}
	f.mu.Unlock()
	writeJSONResponse(w, http.StatusOK, protocol.RecoverableRefreshResponse{SessionResponse: protocol.SessionResponse{ProtocolVersion: "v1",
		AccessToken: child.access, RefreshToken: child.refresh, ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano),
		ReauthenticateAt: time.Now().Add(29 * 24 * time.Hour).UTC().Format(time.RFC3339Nano), RecoveryEpoch: 1},
		RefreshAttemptID: request.RefreshAttemptID, ServerInstanceID: f.instance})
}

func (f *fakeV1) live(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{liveSubprotocol}})
	if err != nil {
		return
	}
	f.mu.Lock()
	f.sockets[conn] = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Unlock()
	ctx := conn.CloseRead(context.Background())
	<-ctx.Done()
	f.mu.Lock()
	delete(f.sockets, conn)
	f.mu.Unlock()
}

// oneSocketPerDevice makes the fake's /v1/live hold one slot, as the hub
// does for a device (BT_SOCKET_DEVICE_LIMIT 1): while a socket is open
// (until the fake has read its close, or closed it), a dial is refused 429.
// Tests that use it have one live device.
func (f *fakeV1) oneSocketPerDevice() {
	f.hook("GET /v1/live", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		if f.openSockets() == 0 {
			return false
		}
		w.Header().Set("Retry-After", "5")
		writeErrorResponse(w, http.StatusTooManyRequests, "rate_limited")
		return true
	})
}

// openSockets is the number of sockets the fake holds open.
func (f *fakeV1) openSockets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sockets)
}

// closeSocketsWith closes every open socket server-side with code. Like
// the hub (it detaches a lease before it sends the close), the fake forgets
// each socket before closing it, so a slot hook (oneSocketPerDevice) has
// freed the slot by the time the client hears the close.
func (f *fakeV1) closeSocketsWith(code websocket.StatusCode, reason string) int {
	f.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(f.sockets))
	for c := range f.sockets {
		conns = append(conns, c)
		delete(f.sockets, c)
	}
	f.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Close(code, reason)
		}()
	}
	wg.Wait()
	return len(conns)
}

// sendHint writes a hint to every open socket.
func (f *fakeV1) sendHint(hint protocol.Hint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(hint)
	for c := range f.sockets {
		_ = c.Write(context.Background(), websocket.MessageText, raw)
	}
}

// sendFamilyHint writes hint to the open sockets of its family's devices.
func (f *fakeV1) sendFamilyHint(hint protocol.Hint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(hint)
	for c, bearer := range f.sockets {
		if family, _ := bearerFamilies.Load(bearer); family == hint.FamilyID {
			_ = c.Write(context.Background(), websocket.MessageText, raw)
		}
	}
}

// sendRaw writes raw as a text frame to every open socket.
func (f *fakeV1) sendRaw(raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.sockets {
		_ = c.Write(context.Background(), websocket.MessageText, raw)
	}
}

func (f *fakeV1) closeSockets() { f.closeSocketsWith(websocket.StatusGoingAway, "test ended") }

func writeJSONResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeErrorResponse(w http.ResponseWriter, status int, code string) {
	writeJSONResponse(w, status, protocol.ErrorResponse{Error: protocol.ErrorDetail{Code: protocol.ErrorCode(code), Message: code}})
}

// fakeFleet is a fleet of families × 4 devices (owner ×2, caregiver, and a
// viewer in every other family), written privately; the first owner device
// of every other family holds an MCP credential.
func fakeFleet(t testing.TB, families int) (dir string, header FleetHeader, states []DeviceState) {
	t.Helper()
	dir = t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	header = FleetHeader{Version: FleetVersion, AsOf: time.Now().UTC().Add(-time.Hour), Families: families,
		ServerInstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RecoveryEpoch: 1}
	for f := range families {
		family := fmt.Sprintf("f%07d-0000-4000-8000-000000000000", f+1)
		owner := fmt.Sprintf("a%07d-0000-4000-8000-000000000001", f+1)
		baby := fmt.Sprintf("b%07d-0000-4000-8000-000000000001", f+1)
		for i := range 4 {
			role, account := "owner", owner
			switch {
			case i == 2:
				role, account = "caregiver", fmt.Sprintf("a%07d-0000-4000-8000-000000000002", f+1)
			case i == 3 && f%2 == 1:
				role, account = "viewer", fmt.Sprintf("a%07d-0000-4000-8000-000000000003", f+1)
			case i == 3:
				role, account = "caregiver", fmt.Sprintf("a%07d-0000-4000-8000-000000000003", f+1)
			}
			n := len(states)
			s := DeviceState{FamilyID: family, AccountID: account, DeviceID: fmt.Sprintf("d%07d-0000-4000-8000-%012d", f+1, i),
				Role: role, BabyIDs: []string{baby}, AccessToken: fmt.Sprintf("access-%d", n), RefreshToken: fmt.Sprintf("r%042d", n),
				DeviceCredential: fmt.Sprintf("c%042d", n), AccessExpiresAt: time.Now().Add(10 * time.Minute),
				ReauthenticateAt: time.Now().Add(29 * 24 * time.Hour), Cursor: 0, ClientIP: fmt.Sprintf("100.64.%d.%d", n/250, 1+n%250)}
			if i == 0 && f%2 == 0 {
				s.MCPCredential = fmt.Sprintf("mcp-%d", f)
			}
			bearerFamilies.Store(s.AccessToken, family)
			bearerFamilies.Store(s.RefreshToken, family)
			states = append(states, s)
		}
	}
	header.Devices = len(states)
	if err := WriteFleet(filepath.Join(dir, "fleet.jsonl"), header, states); err != nil {
		t.Fatal(err)
	}
	return dir, header, states
}

// testGenerator is a generator over a fake fleet, outside Run: every
// request falls in the measurement window.
func testGenerator(t testing.TB, f *fakeV1, families int) *generator {
	t.Helper()
	dir, header, states := fakeFleet(t, families)
	cfg := RunConfig{Base: f.URL, Host: "sync.load.test", Fleet: filepath.Join(dir, "fleet.jsonl"), Ledger: filepath.Join(dir, "ledger.jsonl"),
		Tracked: filepath.Join(dir, "tracked.jsonl"), Out: filepath.Join(dir, "result.json"), Scenario: "soak", Load: 1, Seed: 7,
		Measure: time.Minute, WithPump: true}
	cfg.defaults()
	g := newGenerator(cfg, header, states)
	ledger, err := CreateLedger(cfg.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	g.ledger = ledger
	g.work = t.Context()
	g.phases = newPhases(time.Now().Add(-time.Hour))
	return g
}

// outcomes is route's outcome counts in g's measurement.
func (g *generator) outcomes(route string) map[string]uint64 {
	return g.phases.measure.Snapshot()[route].Outcomes
}
