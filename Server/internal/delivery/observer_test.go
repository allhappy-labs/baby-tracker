package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/family"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

type hintPublisher struct{ hints []realtime.Hint }

func (publisher *hintPublisher) Publish(_ context.Context, hint realtime.Hint) {
	publisher.hints = append(publisher.hints, hint)
}

func serviceWith(h *testkit.Harness, guard authority.RequestGuard, publisher care.Publisher) *familysync.Service {
	return familysync.New(familysync.Options{Guard: guard, Server: h.Info, Limits: h.Limits, Now: func() time.Time { return h.Now },
		Publisher: publisher, PageKeys: testkit.PageKeyring("page.v1", 0x77), Allowed: family.Allowed})
}

// listen is a connection LISTENing on the family cursor channel, whose
// queue a write must leave empty.
func listen(t *testing.T, h *testkit.Harness) *pgx.Conn {
	t.Helper()
	listener, err := h.Runtime.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(listener.Release)
	if _, err = listener.Exec(t.Context(), `LISTEN family_cursor_v1`); err != nil {
		t.Fatal(err)
	}
	return listener.Conn()
}

// requireNoNotification fails if a NOTIFY reached the listener, which
// LISTENs on the family cursor channel from before the write. (The server's
// pg_notification_queue_usage() counts whole queue pages, so a small
// pending notification reads 0 there: the listener is the real check.)
func requireNoNotification(t *testing.T, listener *pgx.Conn) {
	t.Helper()
	wait, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if notification, err := listener.WaitForNotification(wait); err == nil || notification != nil {
		t.Fatalf("write sent notification=%+v err=%v", notification, err)
	}
}

// The write transaction schedules nothing: a hosted write notifies no
// listener (and schema 30 has no wake table to write; see the database
// package's TestLegacyCareStorageIsGone), and the hub hint is published
// once, after the commit, with the high-water cursor.
func TestWriteTransactionSchedulesNoWakes(t *testing.T) {
	h := testkit.NewHosted(t)
	if _, err := h.NewDevice("caregiver"); err != nil { // another device a legacy wake would have targeted
		t.Fatal(err)
	}
	listener := listen(t, h)
	publisher := &hintPublisher{}
	observed := &commitWatch{RequestGuard: h.Authority, h: h, publisher: publisher}
	service := serviceWith(h, observed, NewObserver(publisher, nil))
	batch := h.SyncRequest(h.Cursor(0), nil)
	batch.Mutations = []protocol.Mutation{testkit.MustMutation(t, "baby.json"), testkit.MustMutation(t, "feed-bottle.json")}
	response, err := service.ApplyAndPull(t.Context(), h.Owner, batch)
	if err != nil || len(response.Outcomes) != 2 || response.Outcomes[1].Cursor == nil {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	high := *response.Outcomes[1].Cursor
	if len(publisher.hints) != 1 || publisher.hints[0].Cursor != high || publisher.hints[0].FamilyID != h.FamilyID || response.HighWater != high {
		t.Fatalf("published=%+v high water=%+v", publisher.hints, high)
	}
	if len(observed.atCommit) != 1 || observed.atCommit[0] != 0 {
		t.Fatalf("hints published by the end of the write transaction: %v", observed.atCommit)
	}
	requireNoNotification(t, listener)
}

// commitWatch is the authority guard, recording how many hints had been
// published when each guarded transaction ended.
type commitWatch struct {
	authority.RequestGuard
	h         *testkit.Harness
	atCommit  []int
	publisher *hintPublisher
}

func (g *commitWatch) WithFamilyTx(ctx context.Context, p access.Principal, action string, run func(pgx.Tx, access.Grant) error) error {
	err := g.h.Authority.WithFamilyTx(ctx, p, action, run)
	g.atCommit = append(g.atCommit, len(g.publisher.hints))
	return err
}

// A rolled-back write publishes nothing and notifies nothing.
func TestRolledBackWriteLeavesNoWakeHintOrNotification(t *testing.T) {
	h := testkit.NewHosted(t)
	listener := listen(t, h)
	publisher := &hintPublisher{}
	service := serviceWith(h, rollbackGuard{RequestGuard: h.Authority}, NewObserver(publisher, nil))
	if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); !errors.Is(err, errRollback) {
		t.Fatalf("err=%v", err)
	}
	if len(publisher.hints) != 0 || h.FamilySequence(t) != 0 {
		t.Fatalf("rolled back write published %+v at sequence %d", publisher.hints, h.FamilySequence(t))
	}
	digests, err := h.Auth.AccessDigests(h.AccessToken("owner"))
	if err != nil {
		t.Fatal(err)
	}
	batch := h.SyncRequest(nil, nil)
	batch.Mutations = []protocol.Mutation{testkit.MustMutation(t, "baby.json")}
	if _, err := service.ServeSync(t.Context(), authority.Request{Digests: digests}, batch); !errors.Is(err, errRollback) {
		t.Fatalf("ServeSync err=%v", err)
	}
	if len(publisher.hints) != 0 || h.FamilySequence(t) != 0 {
		t.Fatalf("rolled back request published %+v at sequence %d", publisher.hints, h.FamilySequence(t))
	}
	requireNoNotification(t, listener)
}

var errRollback = errors.New("roll back after the write")

type rollbackGuard struct{ authority.RequestGuard }

// WithRequestTx rolls the request seam's transaction back after its work,
// so ServeSync meets the same failure.
func (g rollbackGuard) WithRequestTx(ctx context.Context, req authority.Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	return g.RequestGuard.WithRequestTx(ctx, req, action, func(tx pgx.Tx, p access.Principal, grant access.Grant) error {
		if err := run(tx, p, grant); err != nil {
			return err
		}
		return errRollback
	})
}

func (g rollbackGuard) WithFamilyTx(ctx context.Context, p access.Principal, action string, run func(pgx.Tx, access.Grant) error) error {
	return g.RequestGuard.WithFamilyTx(ctx, p, action, func(tx pgx.Tx, grant access.Grant) error {
		if err := run(tx, grant); err != nil {
			return err
		}
		return errRollback
	})
}

// The committed change survives a lost hint: after a restart without any
// publish, the other device's pull delivers it.
func TestCommitBeforePublishSurvivesRestart(t *testing.T) {
	h := testkit.NewHosted(t)
	applied, err := serviceWith(h, h.Authority, nil).Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json"))
	if err != nil || applied.Status != "applied" || applied.Cursor == nil {
		t.Fatalf("apply=%+v err=%v", applied, err)
	}
	h.RestartServices(t)
	page := h.Pull(t, h.Caregiver, nil, nil)
	if len(page.Changes) != 1 || page.HighWater != *applied.Cursor {
		t.Fatalf("page=%+v", page)
	}
}

type changeRecorder struct{ changes map[string]uint64 }

func (r *changeRecorder) Changed(familyID string, sequence uint64) { r.changes[familyID] = sequence }

// Publish refuses a cursor of another family and works without a database;
// each committed change it publishes is also recorded for the wake worker.
func TestObserverPublishesOnlyItsFamilysCursor(t *testing.T) {
	publisher, changes := &hintPublisher{}, &changeRecorder{changes: map[string]uint64{}}
	observer := NewObserver(publisher, changes)
	cursor := protocol.Cursor{ServerInstanceID: testkit.InstanceID, RecoveryEpoch: 1, FamilyID: "30000000-0000-4000-8000-000000000001", Sequence: 3}
	if err := observer.Publish(context.Background(), "30000000-0000-4000-8000-000000000002", cursor); err == nil || len(changes.changes) != 0 {
		t.Fatalf("published or recorded another family's cursor: %v %v", err, changes.changes)
	}
	if err := observer.Publish(context.Background(), cursor.FamilyID, cursor); err != nil || len(publisher.hints) != 1 || publisher.hints[0].Cursor != cursor {
		t.Fatalf("publish err=%v hints=%+v", err, publisher.hints)
	}
	if len(changes.changes) != 1 || changes.changes[cursor.FamilyID] != 3 {
		t.Fatalf("recorded %v, want the family at 3", changes.changes)
	}
	if err := NewObserver(publisher, nil).Publish(context.Background(), cursor.FamilyID, cursor); err != nil {
		t.Fatalf("an observer without a recorder: %v", err)
	}
}

type recordedChange struct {
	name, family string
	sequence     uint64
}

type changeLog struct {
	name string
	log  *[]recordedChange
}

func (c changeLog) Changed(familyID string, sequence uint64) {
	*c.log = append(*c.log, recordedChange{c.name, familyID, sequence})
}

// Changes(a, b) delivers each committed advance to both recorders, a then
// b, through the observer; nil recorders are skipped.
func TestChangesFansEachAdvanceOutInOrder(t *testing.T) {
	var log []recordedChange
	a, b := changeLog{"a", &log}, changeLog{"b", &log}
	if Changes() != nil || Changes(nil, nil) != nil {
		t.Fatal("no recorder must be nil")
	}
	if got := Changes(nil, a); got != a {
		t.Fatalf("one recorder is itself: %#v", got)
	}
	publisher := &hintPublisher{}
	observer := NewObserver(publisher, Changes(a, nil, b))
	const family = "30000000-0000-4000-8000-000000000001"
	for _, sequence := range []uint64{3, 4} {
		cursor := protocol.Cursor{ServerInstanceID: testkit.InstanceID, RecoveryEpoch: 1, FamilyID: family, Sequence: sequence}
		if err := observer.Publish(context.Background(), family, cursor); err != nil {
			t.Fatal(err)
		}
	}
	want := []recordedChange{{"a", family, 3}, {"b", family, 3}, {"a", family, 4}, {"b", family, 4}}
	if len(log) != len(want) {
		t.Fatalf("recorded %+v, want %+v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("recorded %+v, want %+v", log, want)
		}
	}
	if len(publisher.hints) != 2 {
		t.Fatalf("hints %+v", publisher.hints)
	}
}
