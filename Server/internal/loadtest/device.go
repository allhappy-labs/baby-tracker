package loadtest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/protocol"
)

// device is one seeded device as the generator drives it: its fleet state
// (guarded by mu), the entities it created this run, and its live socket.
type device struct {
	DeviceState

	index   int
	mu      sync.Mutex
	live    atomic.Bool // designated to hold a live socket
	tracked *trackedState

	entities []*ownEntity // created by this device this run (updates and deletes pick from them)

	// session serialises the device's paged reads with its own refresh,
	// as the client does: a pull's or a snapshot's pages hold it shared,
	// and a refresh's exchange holds it exclusively. A page token is bound
	// to the session that started the paging (sync validatePageBinding),
	// and a refresh revokes the parent session's access, so a refresh
	// committed between two pages would fail the next one
	// (invalid_page_token, session_revoked).
	session sync.RWMutex

	refreshing  bool      // a refresh is in flight
	refreshedAt time.Time // the last refresh that replaced the tokens
	nextRefresh time.Time // the refresh schedule's next timer for it
	laVersion   uint64    // the last Live Activity registration version sent
	socket      *liveConn
	dialing     bool // a dial loop runs for this device
	hintPulling bool // a hint pull is in flight
	hintPending bool // a hint arrived during it
}

// ownEntity is a care entity this device created and still may change.
type ownEntity struct {
	id, kind, babyID string
	version          uint64
	loggedAt         string // supplements and measurements keep their logged_at on update
	busy             bool   // a mutation on it is in flight
	deleted          bool
}

// setNextRefresh records the schedule's next timer for d.
func (d *device) setNextRefresh(t time.Time) {
	d.mu.Lock()
	d.nextRefresh = t
	d.mu.Unlock()
}

// needsRefresh reports whether d must refresh before it sends anything at
// now: a lost refresh answer is pending, its access lapses within 60 s, or
// it lapses (less 60 s) before the schedule's next timer for it.
func (d *device) needsRefresh(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	lapse := d.AccessExpiresAt.Add(-60 * time.Second)
	return d.PendingAttemptID != "" || lapse.Before(now) || !d.nextRefresh.IsZero() && lapse.Before(d.nextRefresh)
}

// mustRenewBeforeUse reports whether d must renew its session before it
// sends a request with its access at now, as iOS's prepareSession does
// before every sync attempt (FamilySessionRenewer.prepare): a refresh
// attempt is pending (sent, its answer lost or refused 5xx: the recovery
// checkpoint is not ackPending), or its access has expired
// (expiresAt <= now).
func (d *device) mustRenewBeforeUse(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mustRenewLocked(now)
}

// mustRenewLocked is mustRenewBeforeUse; the caller holds d.mu.
func (d *device) mustRenewLocked(now time.Time) bool {
	return d.PendingAttemptID != "" || !d.AccessExpiresAt.IsZero() && !now.Before(d.AccessExpiresAt)
}

// holdSession takes d's session for a pull's or a snapshot's pages and
// returns its release and how long it waited for d's refresh: the caller
// moves its intended time by the wait, which is the client's
// serialisation, not the server's latency. A holder must release it before
// it refreshes (expired).
func (d *device) holdSession() (release func(), waited time.Duration) {
	start := time.Now()
	d.session.RLock()
	return d.session.RUnlock, time.Since(start)
}

// access is the device's current access token.
func (d *device) access() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.AccessToken
}

// state is a copy of the device's fleet state.
func (d *device) state() DeviceState {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.DeviceState
	s.BabyIDs = append([]string(nil), d.BabyIDs...)
	if p := d.PendingImport; p != nil {
		copied := *p
		s.PendingImport = &copied
	}
	if until := d.ImportBlockedUntil; until != nil {
		copied := *until
		s.ImportBlockedUntil = &copied
	}
	return s
}

// advanceCursor moves the acknowledged cursor forward, never back.
func (d *device) advanceCursor(sequence uint64) {
	d.mu.Lock()
	if sequence > d.Cursor {
		d.Cursor = sequence
	}
	d.mu.Unlock()
}

// trackedState is a tracked device's view of its family: every entity it
// has seen (snapshot and pulled pages) at the highest version seen, and the
// cursor its last complete pull reached. The audit (Task 11) compares it
// with the database.
type trackedState struct {
	mu       sync.Mutex
	seeded   bool
	cursor   uint64
	entities map[string]TrackedEntity
}

func newTrackedState() *trackedState { return &trackedState{entities: map[string]TrackedEntity{}} }

// markSeeded records that the map took a complete seed: a snapshot to its
// last page and the pull after it, or a pull from 0 to the head.
func (t *trackedState) markSeeded() {
	t.mu.Lock()
	t.seeded = true
	t.mu.Unlock()
}

func (t *trackedState) isSeeded() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seeded
}

// apply records entity if it is newer than what the device holds.
func (t *trackedState) apply(entity protocol.CanonicalEntity) {
	if t == nil {
		return
	}
	sum, err := entityWireSHA256(entity)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if prev, ok := t.entities[entity.Meta.ID]; ok && prev.Version >= entity.Meta.EntityVersion {
		return
	}
	t.entities[entity.Meta.ID] = TrackedEntity{Version: entity.Meta.EntityVersion, WireSHA256: sum, Tombstone: entity.Meta.TombstonedAt != nil}
}

func (t *trackedState) complete(cursor uint64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if cursor > t.cursor {
		t.cursor = cursor
	}
	t.mu.Unlock()
}

// entityWireSHA256 is the hex SHA-256 of the entity's JSON as the protocol
// encodes it: what the ledger and the tracked map record, and what the
// audit compares with sha256(care_entities.wire).
func entityWireSHA256(entity protocol.CanonicalEntity) (string, error) {
	raw, err := json.Marshal(entity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// lockedRand is a seeded source shared by concurrent operations.
type lockedRand struct {
	mu  sync.Mutex
	rng *mrand.Rand
}

func newLockedRand(seed, stream uint64) *lockedRand {
	return &lockedRand{rng: mrand.New(mrand.NewPCG(seed, stream))}
}

func (r *lockedRand) IntN(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rng.IntN(n)
}

func (r *lockedRand) Float64() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rng.Float64()
}

// Duration is uniform over [0, d).
func (r *lockedRand) Duration(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Duration(r.rng.Int64N(int64(d)))
}

// newUUID is a random UUIDv4. IDs come from crypto/rand, never from the run
// seed: the fleet is reused run after run and the server keeps mutation
// receipts, so a seeded ID would replay an earlier run's mutation.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return uuidV4(b[:])
}

// randomHex is n random bytes as lowercase hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// designate picks the live order and the tracked devices: deterministic
// shuffles by the run seed, so the same seed drives the same devices. The
// first live devices of the order hold sockets from warm-up on.
//
// Excluded devices (a scenario's own, hot-family) are never live.
func designate(devices []*device, excluded map[*device]bool, seed uint64, liveOrder, live, tracked int) []*device {
	pick := func(stream uint64, n int, skip map[*device]bool) []int {
		order := make([]int, 0, len(devices))
		for i := range devices {
			if !skip[devices[i]] {
				order = append(order, i)
			}
		}
		rng := mrand.New(mrand.NewPCG(seed, stream))
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		return order[:min(n, len(order))]
	}
	var order []*device
	for n, i := range pick(0x11fe, liveOrder, excluded) {
		order = append(order, devices[i])
		if n < live {
			devices[i].live.Store(true)
		}
	}
	for _, i := range pick(0x7a0c, tracked, nil) {
		devices[i].tracked = newTrackedState()
	}
	return order
}
