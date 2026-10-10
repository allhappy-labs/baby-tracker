package familyread

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

const (
	grantA     = "60000000-0000-4000-8000-00000000000a"
	grantB     = "60000000-0000-4000-8000-00000000000b"
	fixtureBab = "10000000-0000-4000-8000-000000000001" // baby.json's entity ID
)

var day0 = time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

// countingGuard wraps the real guard to count guarded reads.
type countingGuard struct {
	inner Guard
	calls int
}

func (g *countingGuard) WithIntegrationRead(ctx context.Context, credential string, run func(context.Context, pgx.Tx, access.Grant) error) error {
	g.calls++
	return g.inner.WithIntegrationRead(ctx, credential, run)
}

// fixture composes the real integration guard (bound authority, live family
// policy, dedicated pool, tenant RLS) over a real family whose canonical
// records are written through the phone sync path.
type fixture struct {
	h          *testkit.Harness
	reader     *authority.IntegrationReader
	guard      *countingGuard
	credential string
	key        [32]byte
	now        time.Time
	service    *Service
	babyID     string
	next       int
}

func fixedKey(fill byte) [32]byte {
	var key [32]byte
	for i := range key {
		key[i] = fill ^ byte(i)
	}
	return key
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	h := testkit.NewBound(t)
	reader := h.IntegrationReader(t)
	f := &fixture{h: h, reader: reader, key: fixedKey(0x42), now: h.Now}
	f.guard = &countingGuard{inner: reader}
	f.credential = f.issue(t, grantA)
	f.service = New(f.guard, f.key, func() time.Time { return f.now })
	f.babyID = f.createBaby(t, fixtureBab, "Élie", 0)
	return f
}

func (f *fixture) issue(t *testing.T, grantID string) string {
	t.Helper()
	return f.h.IssueIntegration(t, grantID)
}

// id returns a fresh canonical UUID whose first group names its role.
func (f *fixture) id(prefix uint32) string {
	f.next++
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", prefix, f.next)
}

func ts(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000Z") }

func ptr[T any](value T) *T { return &value }

func (f *fixture) apply(t *testing.T, m protocol.Mutation) protocol.Outcome {
	t.Helper()
	m.MutationID = f.h.CommandID()
	outcome, err := f.h.Service.Apply(t.Context(), f.h.Owner, f.h.FamilyID, m)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("apply %s %s %s: outcome=%+v code=%v err=%v", m.Operation, m.EntityKind, m.EntityID, outcome, outcome.Code, err)
	}
	return outcome
}

func (f *fixture) createBaby(t *testing.T, id, name string, sortOrder uint32) string {
	t.Helper()
	m := testkit.MustMutation(t, "baby.json")
	m.EntityID = id
	p := m.Payload.(protocol.BabyPayload)
	p.Name, p.SortOrder = name, sortOrder
	m.Payload = p
	f.apply(t, m)
	return id
}

func (f *fixture) feed(t *testing.T, id string, start time.Time, end *time.Time, session *string) string {
	t.Helper()
	m := testkit.MustMutation(t, "feed-bottle.json")
	m.EntityID = id
	p := m.Payload.(protocol.FeedPayload)
	p.BabyID, p.StartedAt, p.FeedingSessionID = f.babyID, ts(start), session
	p.EndedAt = nil
	if end != nil {
		p.EndedAt = ptr(ts(*end))
	}
	p.Components = []protocol.FeedComponent{{ID: f.id(0x31000000), Kind: "bottle", VolumeML: p.Components[0].VolumeML, Contents: p.Components[0].Contents}}
	m.Payload = p
	f.apply(t, m)
	return id
}

func (f *fixture) sleep(t *testing.T, id string, start time.Time, end *time.Time, session *string) string {
	t.Helper()
	m := testkit.MustMutation(t, "sleep.json")
	m.EntityID = id
	p := m.Payload.(protocol.SleepPayload)
	p.BabyID, p.StartedAt, p.SleepSessionID = f.babyID, ts(start), session
	p.EndedAt = nil
	if end != nil {
		p.EndedAt = ptr(ts(*end))
	}
	m.Payload = p
	f.apply(t, m)
	return id
}

func (f *fixture) diaper(t *testing.T, id string, start time.Time, note *string) string {
	t.Helper()
	m := testkit.MustMutation(t, "diaper.json")
	m.EntityID = id
	p := m.Payload.(protocol.DiaperPayload)
	p.BabyID, p.StartedAt, p.Note = f.babyID, ts(start), note
	m.Payload = p
	f.apply(t, m)
	return id
}

func (f *fixture) supplement(t *testing.T, id string, start time.Time) string {
	t.Helper()
	m := testkit.MustMutation(t, "supplement.json")
	m.EntityID = id
	p := m.Payload.(protocol.SupplementPayload)
	p.BabyID, p.StartedAt, p.LoggedAt = f.babyID, ts(start), ts(start.Add(time.Minute))
	m.Payload = p
	f.apply(t, m)
	return id
}

func (f *fixture) measurement(t *testing.T, id string, at time.Time) string {
	t.Helper()
	m := testkit.MustMutation(t, "measurement.json")
	m.EntityID = id
	p := m.Payload.(protocol.MeasurementPayload)
	p.BabyID, p.MeasuredAt, p.LoggedAt = f.babyID, ts(at), ts(at.Add(time.Minute))
	m.Payload = p
	f.apply(t, m)
	return id
}

// record creates one entry of kind at start (sleeps last five minutes).
func (f *fixture) record(t *testing.T, kind, id string, start time.Time) string {
	t.Helper()
	switch kind {
	case "diaper":
		return f.diaper(t, id, start, nil)
	case "feed":
		return f.feed(t, id, start, ptr(start.Add(5*time.Minute)), nil)
	case "sleep":
		return f.sleep(t, id, start, ptr(start.Add(5*time.Minute)), nil)
	case "supplement":
		return f.supplement(t, id, start)
	case "measurement":
		return f.measurement(t, id, start)
	}
	t.Fatalf("unknown kind %s", kind)
	return ""
}

func (f *fixture) remove(t *testing.T, kind, id string, observed uint64) {
	t.Helper()
	baby := f.babyID
	payload := protocol.DeletePayload{BabyID: &baby}
	if kind == "baby" {
		payload = protocol.DeletePayload{}
	}
	f.apply(t, protocol.Mutation{EntityID: id, EntityKind: kind, Operation: "delete", PayloadVersion: 1,
		ObservedEntityVersion: observed, ClientMutationTime: ts(f.h.Now.Add(-time.Minute)), Payload: payload})
}

func (f *fixture) startTimer(t *testing.T, fixtureName, id string, start time.Time, session *string) {
	t.Helper()
	m := testkit.MustMutation(t, fixtureName)
	m.EntityID = id
	p := m.Payload.(protocol.TimerStartPayload)
	p.BabyID, p.StartedAt, p.SessionID = f.babyID, ts(start), session
	if p.CurrentSideStartedAt != nil {
		p.CurrentSideStartedAt = ptr(ts(start))
	}
	m.Payload = p
	f.apply(t, m)
}

func (f *fixture) history(t *testing.T, q HistoryQuery) HistoryResult {
	t.Helper()
	result, err := f.service.History(t.Context(), f.credential, q)
	if err != nil {
		t.Fatalf("history %+v: %v", q, err)
	}
	return result
}

func entryKeys(entries []HistoryEntry) []string {
	keys := make([]string, len(entries))
	for i, entry := range entries {
		keys[i] = entry.Kind + "/" + entry.ID
	}
	return keys
}

func encodedSize(t *testing.T, value any) int {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}
