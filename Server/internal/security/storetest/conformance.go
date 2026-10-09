package storetest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/security"
)

type Store struct {
	Ledger security.SecurityLedger
	Heads  security.LedgerHeadStore
	Reopen func(*testing.T) (security.SecurityLedger, security.LedgerHeadStore)
}

type Factory func(*testing.T) Store

func Run(t *testing.T, newStore Factory) {
	t.Helper()
	if newStore == nil {
		t.Fatal("store conformance requires a factory")
	}

	t.Run("append is immutable and an uncommitted suffix is ignored", func(t *testing.T) {
		store := requireStore(t, newStore)
		genesis := security.Head{Generation: 1}
		candidate := candidate(t, genesis, 1, 1)
		if err := store.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
			t.Fatal(err)
		}
		if err := store.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
			t.Fatalf("idempotent append: %v", err)
		}
		changed := candidate
		changed.EventJSON = append([]byte(nil), candidate.EventJSON...)
		changed.EventJSON[0] ^= 1
		if err := store.Ledger.AppendCandidate(t.Context(), changed); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("changed bytes code=%q err=%v", security.Code(err), err)
		}
		committed, err := store.Ledger.ReadCommitted(t.Context(), genesis)
		if err != nil || len(committed) != 0 {
			t.Fatalf("uncommitted suffix leaked: count=%d err=%v", len(committed), err)
		}
	})

	t.Run("one concurrent head CAS wins", func(t *testing.T) {
		store := requireStore(t, newStore)
		genesis := security.Head{Generation: 1}
		left := candidate(t, genesis, 1, 11)
		right := candidate(t, genesis, 1, 12)
		for _, value := range []security.Candidate{left, right} {
			if err := store.Ledger.AppendCandidate(t.Context(), value); err != nil {
				t.Fatal(err)
			}
		}
		next := []security.Head{
			{Generation: 1, Revision: 1, EventHash: left.EventHash},
			{Generation: 1, Revision: 1, EventHash: right.EventHash},
		}
		type result struct {
			won bool
			err error
		}
		results := make(chan result, 2)
		var ready sync.WaitGroup
		ready.Add(2)
		start := make(chan struct{})
		for _, proposed := range next {
			go func() {
				ready.Done()
				<-start
				won, err := store.Heads.CompareAndSwap(t.Context(), genesis, proposed)
				results <- result{won, err}
			}()
		}
		ready.Wait()
		close(start)
		wins := 0
		for range 2 {
			result := <-results
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.won {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("CAS winners=%d", wins)
		}
		trusted, err := store.Heads.Load(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		assertChain(t, store.Ledger, trusted, 1)
	})

	t.Run("committed exact prefix survives adapter restart", func(t *testing.T) {
		store := requireStore(t, newStore)
		genesis := security.Head{Generation: 1}
		first := candidate(t, genesis, 1, 21)
		if err := store.Ledger.AppendCandidate(t.Context(), first); err != nil {
			t.Fatal(err)
		}
		head1 := security.Head{Generation: 1, Revision: 1, EventHash: first.EventHash}
		if swapped, err := store.Heads.CompareAndSwap(t.Context(), genesis, head1); err != nil || !swapped {
			t.Fatalf("first CAS swapped=%v err=%v", swapped, err)
		}
		second := candidate(t, head1, 2, 22)
		if err := store.Ledger.AppendCandidate(t.Context(), second); err != nil {
			t.Fatal(err)
		}
		assertChain(t, store.Ledger, head1, 1)
		head2 := security.Head{Generation: 1, Revision: 2, EventHash: second.EventHash}
		if swapped, err := store.Heads.CompareAndSwap(t.Context(), head1, head2); err != nil || !swapped {
			t.Fatalf("second CAS swapped=%v err=%v", swapped, err)
		}
		reopenedLedger, reopenedHeads := store.Reopen(t)
		if reopenedLedger == nil || reopenedHeads == nil {
			t.Fatal("reopen returned nil store")
		}
		if samePointer(store.Ledger, reopenedLedger) || samePointer(store.Heads, reopenedHeads) {
			t.Fatal("reopen returned the original adapter instance")
		}
		trusted, err := reopenedHeads.Load(t.Context())
		if err != nil || trusted != head2 {
			t.Fatalf("reopened head=%+v err=%v", trusted, err)
		}
		assertChain(t, reopenedLedger, trusted, 2)
	})

	t.Run("missing and gapped chains fail closed", func(t *testing.T) {
		store := requireStore(t, newStore)
		missing := security.Head{Generation: 1, Revision: 1, EventHash: [32]byte{0xff}}
		if _, err := store.Ledger.ReadCommitted(t.Context(), missing); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("missing chain code=%q err=%v", security.Code(err), err)
		}
		genesis := security.Head{Generation: 1}
		first := candidate(t, genesis, 1, 31)
		head1 := security.Head{Generation: 1, Revision: 1, EventHash: first.EventHash}
		second := candidate(t, head1, 2, 32)
		if err := store.Ledger.AppendCandidate(t.Context(), second); err != nil {
			t.Fatal(err)
		}
		head2 := security.Head{Generation: 1, Revision: 2, EventHash: second.EventHash}
		if _, err := store.Ledger.ReadCommitted(t.Context(), head2); security.Code(err) != "ledger_corrupt" {
			t.Fatalf("gapped chain code=%q err=%v", security.Code(err), err)
		}
	})
}

func RunGenerationMismatch(t *testing.T, newStore Factory) {
	t.Helper()
	store := requireStore(t, newStore)
	genesis := security.Head{Generation: 1}
	value := candidate(t, genesis, 1, 41)
	if err := store.Ledger.AppendCandidate(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	committed := security.Head{Generation: 1, Revision: 1, EventHash: value.EventHash}
	if swapped, err := store.Heads.CompareAndSwap(t.Context(), genesis, committed); err != nil || !swapped {
		t.Fatalf("commit swapped=%v err=%v", swapped, err)
	}
	wrongGeneration := committed
	wrongGeneration.Generation = 2
	if _, err := store.Ledger.ReadCommitted(t.Context(), wrongGeneration); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("trusted generation mismatch code=%q err=%v", security.Code(err), err)
	}
	if swapped, err := store.Heads.CompareAndSwap(t.Context(), committed, security.Head{Generation: 2, Revision: 2, EventHash: [32]byte{2}}); err == nil || swapped {
		t.Fatalf("head generation transition accepted swapped=%v err=%v", swapped, err)
	}

	mutated := value
	var event security.Event
	if err := jsonUnmarshal(mutated.EventJSON, &event); err != nil {
		t.Fatal(err)
	}
	event.Generation = 2
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	mutated.EventJSON = encoded
	mutated.EventHash = sha256.Sum256(encoded)
	if _, err := security.Replay([]security.Candidate{mutated}); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("event generation mismatch code=%q err=%v", security.Code(err), err)
	}
}

func requireStore(t *testing.T, newStore Factory) Store {
	t.Helper()
	store := newStore(t)
	if store.Ledger == nil || store.Heads == nil || store.Reopen == nil {
		t.Fatal("store conformance requires ledger, heads, and durable reopen")
	}
	head, err := store.Heads.Load(t.Context())
	if err != nil || head != (security.Head{Generation: 1}) {
		t.Fatalf("factory must return initialized generation-1 store: head=%+v err=%v", head, err)
	}
	return store
}

func assertChain(t *testing.T, ledger security.SecurityLedger, trusted security.Head, count int) {
	t.Helper()
	candidates, err := ledger.ReadCommitted(t.Context(), trusted)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != count {
		t.Fatalf("candidate count=%d want=%d", len(candidates), count)
	}
	state, err := security.Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if state.Head() != trusted {
		t.Fatalf("replayed head=%+v trusted=%+v", state.Head(), trusted)
	}
}

func candidate(t *testing.T, prior security.Head, revision, serial uint64) security.Candidate {
	t.Helper()
	commandID := fmt.Sprintf("30000000-0000-4000-8000-%012d", serial)
	accountID := fmt.Sprintf("10000000-0000-4000-8000-%012d", serial)
	event := security.Event{
		Version: "v1", Generation: 1, Revision: revision,
		PreviousHash: hex.EncodeToString(prior.EventHash[:]), CommandID: commandID,
		CommandHash:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: accountID, Kind: "account_registered",
		At:      time.Date(2026, 9, 20, 12, 0, int(serial%60), 0, time.UTC).Format(time.RFC3339Nano),
		Effects: []security.Effect{{Kind: "account_registered", AccountID: accountID, IdentityKey: fmt.Sprintf("opaque-%d", serial), Status: "active"}},
	}
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	return security.Candidate{CommandID: commandID, Prior: prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
}

func samePointer(left, right any) bool {
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	return leftValue.IsValid() && rightValue.IsValid() && leftValue.Kind() == reflect.Pointer &&
		rightValue.Kind() == reflect.Pointer && leftValue.Type() == rightValue.Type() && leftValue.Pointer() == rightValue.Pointer()
}

func jsonUnmarshal(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
