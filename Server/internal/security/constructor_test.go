package security_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/security"
	"github.com/jackc/pgx/v5/pgxpool"
)

type memoryLedger struct{}

func (*memoryLedger) AppendCandidate(context.Context, security.Candidate) error { return nil }
func (*memoryLedger) ReadCommitted(context.Context, security.Head) ([]security.Candidate, error) {
	return nil, nil
}
func (*memoryLedger) WalkCommitted(context.Context, security.Head, func(security.Candidate) error) error {
	return nil
}

type memoryHeads struct{}

func (*memoryHeads) Load(context.Context) (security.Head, error) {
	return security.Head{Generation: 1}, nil
}
func (*memoryHeads) CompareAndSwap(context.Context, security.Head, security.Head) (bool, error) {
	return true, nil
}

type memoryFence struct{}

func (memoryFence) Read(context.Context) (func(), error)  { return func() {}, nil }
func (memoryFence) Write(context.Context) (func(), error) { return func() {}, nil }

func fixedClock() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) }

func completeSecurityOptions(options security.Options) security.Options {
	options.Runtime = &pgxpool.Pool{}
	options.Decide = func(context.Context, access.Principal, string, string) (access.Grant, error) {
		return access.Grant{}, nil
	}
	options.Epoch = func(context.Context) (uint64, error) { return 1, nil }
	return options
}

type memoryProjection struct{ head security.Head }

func (projection *memoryProjection) Head(context.Context) (security.Head, error) {
	return projection.head, nil
}
func (projection *memoryProjection) Apply(_ context.Context, candidate security.Candidate) error {
	projection.head = security.Head{Generation: candidate.Prior.Generation, Revision: candidate.Prior.Revision + 1, EventHash: candidate.EventHash}
	return nil
}

type streamOnlyLedger struct {
	readCalled bool
	walkCalled bool
}

type fixedLedger struct{ candidate security.Candidate }

func (ledger *fixedLedger) AppendCandidate(context.Context, security.Candidate) error { return nil }
func (ledger *fixedLedger) ReadCommitted(context.Context, security.Head) ([]security.Candidate, error) {
	return []security.Candidate{ledger.candidate}, nil
}
func (ledger *fixedLedger) WalkCommitted(_ context.Context, _ security.Head, visit func(security.Candidate) error) error {
	return visit(ledger.candidate)
}

type fixedHeads struct{ head security.Head }

func (heads *fixedHeads) Load(context.Context) (security.Head, error) { return heads.head, nil }
func (heads *fixedHeads) CompareAndSwap(context.Context, security.Head, security.Head) (bool, error) {
	return false, nil
}

func (ledger *streamOnlyLedger) AppendCandidate(context.Context, security.Candidate) error {
	return nil
}
func (ledger *streamOnlyLedger) ReadCommitted(context.Context, security.Head) ([]security.Candidate, error) {
	ledger.readCalled = true
	return nil, errors.New("production called ReadCommitted")
}
func (ledger *streamOnlyLedger) WalkCommitted(_ context.Context, _ security.Head, _ func(security.Candidate) error) error {
	ledger.walkCalled = true
	return nil
}

func TestNewRequiresEveryP1Collaborator(t *testing.T) {
	valid := security.Options{
		Ledger: &memoryLedger{}, Heads: &memoryHeads{}, Projection: &memoryProjection{head: security.Head{Generation: 1}},
		Fence: memoryFence{}, Now: fixedClock, Runtime: &pgxpool.Pool{},
		Decide: func(context.Context, access.Principal, string, string) (access.Grant, error) {
			return access.Grant{}, nil
		},
		Epoch: func(context.Context) (uint64, error) { return 1, nil },
	}
	for name, mutate := range map[string]func(*security.Options){
		"ledger":     func(options *security.Options) { options.Ledger = nil },
		"heads":      func(options *security.Options) { options.Heads = nil },
		"projection": func(options *security.Options) { options.Projection = nil },
		"fence":      func(options *security.Options) { options.Fence = nil },
		"now":        func(options *security.Options) { options.Now = nil },
		"runtime":    func(options *security.Options) { options.Runtime = nil },
		"decide":     func(options *security.Options) { options.Decide = nil },
		"epoch":      func(options *security.Options) { options.Epoch = nil },
	} {
		t.Run(name, func(t *testing.T) {
			options := valid
			mutate(&options)
			if service, err := security.New(options); err == nil || service != nil {
				t.Fatalf("service=%v err=%v", service, err)
			}
		})
	}
	if service, err := security.New(valid); err != nil || service == nil {
		t.Fatalf("service=%v err=%v", service, err)
	}
}

func TestUnknownErrorsMapToServiceUnavailable(t *testing.T) {
	if got := security.Code(errors.New("private adapter detail")); got != "service_unavailable" {
		t.Fatalf("code=%q", got)
	}
	if got := security.Code(errors.Join(errors.New("wrapper"), errors.New("private detail"))); got != "service_unavailable" {
		t.Fatalf("wrapped code=%q", got)
	}
}

func TestProductionReconstructionUsesStreamingWalk(t *testing.T) {
	ledger := &streamOnlyLedger{}
	service, err := security.New(completeSecurityOptions(security.Options{
		Ledger: ledger, Heads: &memoryHeads{}, Projection: &memoryProjection{head: security.Head{Generation: 1}},
		Fence: memoryFence{}, Now: fixedClock,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !ledger.walkCalled || ledger.readCalled {
		t.Fatalf("walk=%v read=%v", ledger.walkCalled, ledger.readCalled)
	}
}

func TestProductionRejectsUnverifiedGenerationTwo(t *testing.T) {
	prior := security.Head{Generation: 2}
	event := security.Event{
		Version: "v1", Generation: 2, Revision: 1, PreviousHash: hex.EncodeToString(prior.EventHash[:]),
		CommandID: "30000000-0000-4000-8000-000000000001", CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: "10000000-0000-4000-8000-000000000001", ActorDeviceID: "", Kind: "account_registered", At: "2026-09-20T12:00:00Z",
		Effects: []security.Effect{{Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000001", IdentityKey: "opaque", Status: "active"}},
	}
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	candidate := security.Candidate{CommandID: event.CommandID, Prior: prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
	head := security.Head{Generation: 2, Revision: 1, EventHash: candidate.EventHash}
	service, err := security.New(completeSecurityOptions(security.Options{
		Ledger: &fixedLedger{candidate: candidate}, Heads: &fixedHeads{head: head}, Projection: &memoryProjection{head: prior},
		Fence: memoryFence{}, Now: fixedClock,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(t.Context()); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("code=%s err=%v", security.Code(err), err)
	}
}
