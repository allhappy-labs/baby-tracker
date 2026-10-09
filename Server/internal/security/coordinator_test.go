package security

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"github.com/jackc/pgx/v5/pgxpool"
)

type authorityFixture struct {
	Service    *Service
	Heads      LedgerHeadStore
	Projection *faultProjection
	ledger     *fixtureLedger
	heads      *fixtureHeads
	clock      func() time.Time
}

type fixtureLedger struct {
	mu         sync.Mutex
	candidates map[[32]byte]Candidate
	appends    int
}

func (ledger *fixtureLedger) AppendCandidate(_ context.Context, candidate Candidate) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.appends++
	if existing, ok := ledger.candidates[candidate.EventHash]; ok && string(existing.EventJSON) != string(candidate.EventJSON) {
		return ErrLedgerCorrupt
	}
	ledger.candidates[candidate.EventHash] = candidate
	return nil
}
func (ledger *fixtureLedger) ReadCommitted(ctx context.Context, head Head) ([]Candidate, error) {
	var result []Candidate
	err := ledger.WalkCommitted(ctx, head, func(candidate Candidate) error { result = append(result, candidate); return nil })
	return result, err
}
func (ledger *fixtureLedger) WalkCommitted(ctx context.Context, head Head, visit func(Candidate) error) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	reversed := make([]Candidate, 0, head.Revision)
	hash := head.EventHash
	for revision := head.Revision; revision > 0; revision-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		candidate, ok := ledger.candidates[hash]
		if !ok || candidate.Prior.Revision+1 != revision {
			return ErrLedgerCorrupt
		}
		reversed = append(reversed, candidate)
		hash = candidate.Prior.EventHash
	}
	for index := len(reversed) - 1; index >= 0; index-- {
		if err := visit(reversed[index]); err != nil {
			return err
		}
	}
	return nil
}

type fixtureHeads struct {
	mu         sync.Mutex
	head       Head
	swaps      int
	rejectOnce bool
}

func (heads *fixtureHeads) Load(context.Context) (Head, error) {
	heads.mu.Lock()
	defer heads.mu.Unlock()
	return heads.head, nil
}
func (heads *fixtureHeads) CompareAndSwap(_ context.Context, expected, next Head) (bool, error) {
	heads.mu.Lock()
	defer heads.mu.Unlock()
	heads.swaps++
	if heads.rejectOnce {
		heads.rejectOnce = false
		return false, nil
	}
	if heads.head != expected {
		return false, nil
	}
	heads.head = next
	return true, nil
}

type fixtureFence struct{ mu sync.RWMutex }

func (fence *fixtureFence) Read(context.Context) (func(), error) {
	fence.mu.RLock()
	return fence.mu.RUnlock, nil
}
func (fence *fixtureFence) Write(context.Context) (func(), error) {
	fence.mu.Lock()
	return fence.mu.Unlock, nil
}

type faultProjection struct {
	mu           sync.Mutex
	head         Head
	failOnce     bool
	applications int
}

func (projection *faultProjection) Head(context.Context) (Head, error) {
	projection.mu.Lock()
	defer projection.mu.Unlock()
	return projection.head, nil
}
func (projection *faultProjection) Apply(_ context.Context, candidate Candidate) error {
	projection.mu.Lock()
	defer projection.mu.Unlock()
	if projection.failOnce {
		projection.failOnce = false
		return errors.New("injected projection crash")
	}
	next := Head{Generation: candidate.Prior.Generation, Revision: candidate.Prior.Revision + 1, EventHash: candidate.EventHash}
	if projection.head == next {
		return nil
	}
	if projection.head != candidate.Prior {
		return errors.New("projection prior mismatch")
	}
	projection.head = next
	projection.applications++
	return nil
}

func newAuthority(t *testing.T) *authorityFixture {
	t.Helper()
	h := &authorityFixture{ledger: &fixtureLedger{candidates: map[[32]byte]Candidate{}}, heads: &fixtureHeads{head: Head{Generation: 1}},
		Projection: &faultProjection{head: Head{Generation: 1}}, clock: func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 123, time.UTC) }}
	h.Heads = h.heads
	var err error
	h.Service, err = New(completeTestOptions(Options{Ledger: h.ledger, Heads: h.heads, Projection: h.Projection, Fence: &fixtureFence{}, Now: h.clock}))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *authorityFixture) Register(accountID, commandID string) Command {
	return newVerifiedIdentityRegistration(accountID, "sha256:opaque-test-identity", commandID)
}
func (h *authorityFixture) Head() Head {
	head, err := h.Heads.Load(context.Background())
	if err != nil {
		panic(err)
	}
	return head
}
func (h *authorityFixture) FailProjectionOnce() { h.Projection.failOnce = true }
func (h *authorityFixture) Reopen() {
	service, err := New(completeTestOptions(Options{Ledger: h.ledger, Heads: h.heads, Projection: h.Projection, Fence: &fixtureFence{}, Now: h.clock}))
	if err != nil {
		panic(err)
	}
	h.Service = service
}

func completeTestOptions(options Options) Options {
	options.Runtime = &pgxpool.Pool{}
	options.Decide = func(context.Context, access.Principal, string, string) (access.Grant, error) {
		return access.Grant{}, ErrUnavailable
	}
	options.Epoch = func(context.Context) (uint64, error) { return 1, nil }
	return options
}
func (h *authorityFixture) CandidateCount() int {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	return len(h.ledger.candidates)
}

func (h *authorityFixture) AppendCount() int {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	return h.ledger.appends
}

func TestExecuteRejectsStaleRecoveryEpochBeforeAppend(t *testing.T) {
	h := newAuthority(t)
	h.Service.epoch = func(context.Context) (uint64, error) { return 2, nil }
	principal := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account", RecoveryEpoch: 1}
	if _, err := h.Service.Execute(t.Context(), principal, h.Register(principal.AccountID, "30000000-0000-4000-8000-000000000091")); Code(err) != "epoch_changed" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	if h.AppendCount() != 0 || h.heads.swaps != 0 || h.Head().Revision != 0 {
		t.Fatalf("stale epoch mutated authority appends=%d swaps=%d head=%+v", h.AppendCount(), h.heads.swaps, h.Head())
	}
}

func TestExecuteRechecksRecoveryEpochBeforeCASRetry(t *testing.T) {
	h := newAuthority(t)
	h.heads.rejectOnce = true
	calls := 0
	h.Service.epoch = func(context.Context) (uint64, error) {
		calls++
		if calls == 1 {
			return 1, nil
		}
		return 2, nil
	}
	principal := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account", RecoveryEpoch: 1}
	if _, err := h.Service.Execute(t.Context(), principal, h.Register(principal.AccountID, "30000000-0000-4000-8000-000000000092")); Code(err) != "epoch_changed" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	if calls != 2 || h.AppendCount() != 1 || h.heads.swaps != 1 || h.Head().Revision != 0 {
		t.Fatalf("retry crossed epoch calls=%d appends=%d swaps=%d head=%+v", calls, h.AppendCount(), h.heads.swaps, h.Head())
	}
}

func TestExecuteRejectsMalformedUTF8Payload(t *testing.T) {
	h := newAuthority(t)
	principal := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account", RecoveryEpoch: 1}
	command := h.Register(principal.AccountID, "30000000-0000-4000-8000-000000000099")
	payload := append([]byte(`{"version":"v1","account_id":"10000000-0000-4000-8000-000000000001","identity_lookup_digest":"`), 0xff)
	command.Payload = append(payload, []byte(`"}`)...)
	if _, err := h.Service.Execute(t.Context(), principal, command); Code(err) != "invalid_command" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	if h.Head().Revision != 0 || h.CandidateCount() != 0 {
		t.Fatalf("malformed payload mutated authority head=%+v candidates=%d", h.Head(), h.CandidateCount())
	}
}

func TestHeadCommitSurvivesProjectionCrash(t *testing.T) {
	h := newAuthority(t)
	p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account", RecoveryEpoch: 1}
	cmd := h.Register(p.AccountID, "30000000-0000-4000-8000-000000000001")
	h.FailProjectionOnce()
	if _, err := h.Service.Execute(t.Context(), p, cmd); Code(err) != "service_unavailable" {
		t.Fatalf("code=%s", Code(err))
	}
	if h.Head().Revision != 1 {
		t.Fatal("head was not the commit")
	}
	h.Reopen()
	if err := h.Service.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := h.Service.Execute(t.Context(), p, cmd)
	if err != nil || got.Revision != 1 || h.Head().Revision != 1 || h.Projection.applications != 1 {
		t.Fatalf("replay=%+v err=%v applications=%d", got, err, h.Projection.applications)
	}
}

func TestRejectedCommandNeverAppends(t *testing.T) {
	h := newAuthority(t)
	_, err := h.Service.Execute(t.Context(), access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account", RecoveryEpoch: 1}, Command{ID: "bad", Kind: "owner_transferred"})
	if Code(err) != "invalid_command" || h.CandidateCount() != 0 || h.Head().Revision != 0 {
		t.Fatalf("code=%s candidates=%d head=%+v", Code(err), h.CandidateCount(), h.Head())
	}
}

func TestOrdinaryPrincipalCannotBootstrap(t *testing.T) {
	h := newAuthority(t)
	p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account", RecoveryEpoch: 1}
	payload, _ := json.Marshal(AccountRegisteredPayload{Version: "v1", AccountID: p.AccountID, IdentityKey: "sha256:opaque-test-identity"})
	command := Command{ID: "30000000-0000-4000-8000-000000000001", Kind: "account_registered", AffectedAccountIDs: []string{p.AccountID}, Payload: payload}
	if _, err := h.Service.Execute(t.Context(), p, command); Code(err) != "invalid_command" {
		t.Fatalf("code=%s", Code(err))
	}
	if h.Head().Revision != 0 || h.CandidateCount() != 0 {
		t.Fatalf("head=%+v candidates=%d", h.Head(), h.CandidateCount())
	}
}

func TestFutureOwnedCommandsFailClosed(t *testing.T) {
	h := newAuthority(t)
	p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account", RecoveryEpoch: 1}
	for _, kind := range []string{"device_registered", "session_granted", "family_created", "invitation_created", "owner_transferred", "account_deleted"} {
		t.Run(kind, func(t *testing.T) {
			command := Command{ID: "30000000-0000-4000-8000-000000000009", Kind: EventKind(kind), Payload: json.RawMessage(`{"version":"v1"}`)}
			if _, err := h.Service.Execute(t.Context(), p, command); Code(err) != "invalid_command" {
				t.Fatalf("code=%s", Code(err))
			}
		})
	}
}

func TestCommandIDReuse(t *testing.T) {
	h := newAuthority(t)
	p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account", RecoveryEpoch: 1}
	command := h.Register(p.AccountID, "30000000-0000-4000-8000-000000000001")
	first, err := h.Service.Execute(t.Context(), p, command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Service.Execute(t.Context(), p, command)
	if err != nil || second != first || h.Head().Revision != 1 {
		t.Fatalf("repeat=%+v err=%v head=%+v", second, err, h.Head())
	}
	command.Payload, _ = json.Marshal(AccountRegisteredPayload{Version: "v1", AccountID: p.AccountID, IdentityKey: "sha256:different"})
	if _, err := h.Service.Execute(t.Context(), p, command); Code(err) != "command_id_reused" {
		t.Fatalf("code=%s", Code(err))
	}
}
