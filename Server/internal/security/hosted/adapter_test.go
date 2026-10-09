package hosted

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/security"
	"babytracker/server/internal/security/storetest"
)

const testInstanceID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func TestValidatePropertiesRequiresIndependentApprovedStores(t *testing.T) {
	valid := Properties{
		Provider:            "fixture-provider",
		Region:              "eu-test-1",
		LedgerFailureDomain: "ledger-domain",
		HeadFailureDomain:   "head-domain",
		LedgerKeyID:         "ledger-key-v1",
		HeadKeyID:           "head-key-v1",
		Encrypted:           true,
		ImmutableLedger:     true,
		ReplicatedHead:      true,
		ConditionalCreate:   true,
		LinearizableCAS:     true,
	}
	policy := PropertyPolicy{
		AllowedProviders: []string{"fixture-provider"},
		AllowedRegions:   []string{"eu-test-1"},
		LedgerKeyID:      "ledger-key-v1",
		HeadKeyID:        "head-key-v1",
	}
	if err := ValidateProperties(valid, policy); err != nil {
		t.Fatalf("valid properties: %v", err)
	}

	tests := map[string]func(*Properties, *PropertyPolicy){
		"provider not approved":           func(p *Properties, _ *PropertyPolicy) { p.Provider = "other" },
		"region not approved":             func(p *Properties, _ *PropertyPolicy) { p.Region = "us-test-1" },
		"same failure domain":             func(p *Properties, _ *PropertyPolicy) { p.HeadFailureDomain = p.LedgerFailureDomain },
		"same key":                        func(p *Properties, q *PropertyPolicy) { p.HeadKeyID, q.HeadKeyID = p.LedgerKeyID, q.LedgerKeyID },
		"observed ledger key mismatch":    func(p *Properties, _ *PropertyPolicy) { p.LedgerKeyID = "other-key" },
		"observed head key mismatch":      func(p *Properties, _ *PropertyPolicy) { p.HeadKeyID = "other-key" },
		"encryption not observed":         func(p *Properties, _ *PropertyPolicy) { p.Encrypted = false },
		"immutability not observed":       func(p *Properties, _ *PropertyPolicy) { p.ImmutableLedger = false },
		"replication not observed":        func(p *Properties, _ *PropertyPolicy) { p.ReplicatedHead = false },
		"conditional create not observed": func(p *Properties, _ *PropertyPolicy) { p.ConditionalCreate = false },
		"linearizable CAS not observed":   func(p *Properties, _ *PropertyPolicy) { p.LinearizableCAS = false },
		"empty provider policy":           func(_ *Properties, q *PropertyPolicy) { q.AllowedProviders = nil },
		"empty region policy":             func(_ *Properties, q *PropertyPolicy) { q.AllowedRegions = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			got, configured := valid, policy
			configured.AllowedProviders = append([]string(nil), policy.AllowedProviders...)
			configured.AllowedRegions = append([]string(nil), policy.AllowedRegions...)
			mutate(&got, &configured)
			if err := ValidateProperties(got, configured); err == nil {
				t.Fatal("invalid hosted store properties accepted")
			}
		})
	}
}

func TestLedgerUsesExactFramedAADAndOpaqueImmutableKey(t *testing.T) {
	objects := newMemoryObjects()
	cipher := &recordingCipher{}
	scratch := &memoryScratch{}
	ledger, err := NewLedger(objects, cipher, scratch, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	wantKey := fmt.Sprintf("security-ledger/v1/g%020d/r%020d/%x", 1, 1, candidate.EventHash)
	created, err := objects.Read(t.Context(), wantKey)
	if err != nil {
		t.Fatal(err)
	}
	if created.KeyID != "ledger-key-v1" || created.Key != wantKey || created.SHA256 != sha256.Sum256(created.Ciphertext) {
		t.Fatalf("created object metadata=%+v", created)
	}
	wantAAD := literalFrame(
		[]byte("baby-tracker-security-ledger-aad-v1"),
		[]byte(testInstanceID),
		[]byte("ledger-key-v1"),
		[]byte{0, 0, 0, 0, 0, 0, 0, 1},
		[]byte{0, 0, 0, 0, 0, 0, 0, 1},
		candidate.EventHash[:],
	)
	if !bytes.Equal(cipher.lastSealAAD, wantAAD) {
		t.Fatalf("ledger AAD\n got %x\nwant %x", cipher.lastSealAAD, wantAAD)
	}

	head := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
	var walked []security.Candidate
	if err := ledger.WalkCommitted(t.Context(), head, func(got security.Candidate) error {
		walked = append(walked, got)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(walked) != 1 || !equalCandidate(walked[0], candidate) {
		t.Fatalf("walked=%+v", walked)
	}
	if scratch.last == nil || !scratch.last.closed {
		t.Fatal("replay scratch was not erased after walk")
	}
}

func TestConstructorsRejectUnboundOrUnboundedInputs(t *testing.T) {
	objects := newMemoryObjects()
	cipher := &recordingCipher{}
	scratch := &memoryScratch{}
	for name, makeLedger := range map[string]func() error{
		"nil objects": func() error {
			_, err := NewLedger(nil, cipher, scratch, testInstanceID, "ledger-key-v1", 4096)
			return err
		},
		"nil cipher": func() error {
			_, err := NewLedger(objects, nil, scratch, testInstanceID, "ledger-key-v1", 4096)
			return err
		},
		"nil scratch": func() error {
			_, err := NewLedger(objects, cipher, nil, testInstanceID, "ledger-key-v1", 4096)
			return err
		},
		"invalid instance": func() error {
			_, err := NewLedger(objects, cipher, scratch, "not-a-uuid", "ledger-key-v1", 4096)
			return err
		},
		"empty key": func() error { _, err := NewLedger(objects, cipher, scratch, testInstanceID, "", 4096); return err },
		"unbounded scratch": func() error {
			_, err := NewLedger(objects, cipher, scratch, testInstanceID, "ledger-key-v1", 0)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := makeLedger(); err == nil {
				t.Fatal("invalid ledger constructor input accepted")
			}
		})
	}
}

func TestHeadsUseExactFramedAADAndProviderCASVersion(t *testing.T) {
	cipher := &recordingCipher{}
	wantAAD := literalFrame(
		[]byte("baby-tracker-security-head-aad-v1"),
		[]byte(testInstanceID),
		[]byte("head-key-v1"),
	)
	genesisJSON := []byte(`{"version":"security-head-v1","generation":1,"revision":0,"event_hash":"0000000000000000000000000000000000000000000000000000000000000000","instance_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`)
	sealed, err := cipher.Seal(t.Context(), "head-key-v1", genesisJSON, wantAAD)
	if err != nil {
		t.Fatal(err)
	}
	provider := &memoryHeads{value: EncryptedHeadValue{Version: "head-1", KeyID: "head-key-v1", Ciphertext: sealed}}
	heads, err := NewHeads(provider, cipher, testInstanceID, "head-key-v1")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := heads.Load(t.Context())
	if err != nil || genesis != (security.Head{Generation: 1}) {
		t.Fatalf("genesis=%+v err=%v", genesis, err)
	}
	next := security.Head{Generation: 1, Revision: 1, EventHash: [32]byte{1}}
	swapped, err := heads.CompareAndSwap(t.Context(), genesis, next)
	if err != nil || !swapped {
		t.Fatalf("swapped=%v err=%v", swapped, err)
	}
	if provider.lastExpected != "head-1" {
		t.Fatalf("provider expected version=%q", provider.lastExpected)
	}
	if !bytes.Equal(cipher.lastSealAAD, wantAAD) {
		t.Fatalf("head AAD\n got %x\nwant %x", cipher.lastSealAAD, wantAAD)
	}
	loaded, err := heads.Load(t.Context())
	if err != nil || loaded != next {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if swapped, err := heads.CompareAndSwap(t.Context(), genesis, next); err != nil || swapped {
		t.Fatalf("stale swap=%v err=%v", swapped, err)
	}
}

func TestHeadsRejectRollbackInvalidGenerationAndAbsentTrust(t *testing.T) {
	cipher := &recordingCipher{}
	provider := &memoryHeads{loadErr: ErrHeadNotFound}
	heads, err := NewHeads(provider, cipher, testInstanceID, "head-key-v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := heads.Load(t.Context()); !errors.Is(err, security.ErrTrustedHeadRequired) {
		t.Fatalf("absent head err=%v", err)
	}
	provider.loadErr = nil
	provider.value = encryptedHeadFixture(t, cipher, security.Head{Generation: 1, Revision: 1, EventHash: [32]byte{1}})
	for name, next := range map[string]security.Head{
		"rollback":              {Generation: 1},
		"skip revision":         {Generation: 1, Revision: 3, EventHash: [32]byte{3}},
		"generation transition": {Generation: 2, Revision: 2, EventHash: [32]byte{2}},
		"zero hash":             {Generation: 1, Revision: 2},
	} {
		t.Run(name, func(t *testing.T) {
			if swapped, err := heads.CompareAndSwap(t.Context(), security.Head{Generation: 1, Revision: 1, EventHash: [32]byte{1}}, next); err == nil || swapped {
				t.Fatalf("invalid transition swapped=%v err=%v", swapped, err)
			}
		})
	}
}

func TestHostedProviderDoubleContract(t *testing.T) {
	storetest.Run(t, hostedFactory())
}

func TestHostedGenerationMismatchFailsClosed(t *testing.T) {
	storetest.RunGenerationMismatch(t, hostedFactory())
}

func hostedFactory() storetest.Factory {
	return func(t *testing.T) storetest.Store {
		t.Helper()
		objects := newMemoryObjects()
		providerHeads := &memoryHeads{}
		keys := map[string][]byte{
			"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32),
			"head-key-v1":   bytes.Repeat([]byte{0x22}, 32),
		}
		cipher := &authenticatedCipher{keys: keys}
		plaintext, err := encodeHead(security.Head{Generation: 1}, testInstanceID)
		if err != nil {
			t.Fatal(err)
		}
		ciphertext, err := cipher.Seal(t.Context(), "head-key-v1", plaintext, headAAD(testInstanceID, "head-key-v1"))
		if err != nil {
			t.Fatal(err)
		}
		providerHeads.value = EncryptedHeadValue{Version: "head-1", KeyID: "head-key-v1", Ciphertext: ciphertext}
		open := func(t *testing.T) (security.SecurityLedger, security.LedgerHeadStore) {
			t.Helper()
			ledger, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			heads, err := NewHeads(providerHeads, cipher, testInstanceID, "head-key-v1")
			if err != nil {
				t.Fatal(err)
			}
			return ledger, heads
		}
		ledger, heads := open(t)
		return storetest.Store{Ledger: ledger, Heads: heads, Reopen: open}
	}
}

func TestLedgerAndHeadTamperingFailsClosedWithoutLeakingProviderErrors(t *testing.T) {
	objects := newMemoryObjects()
	keys := map[string][]byte{"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32), "head-key-v1": bytes.Repeat([]byte{0x22}, 32)}
	cipher := &authenticatedCipher{keys: keys}
	ledger, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	key := ledgerObjectKey(1, 1, candidate.EventHash)
	object := objects.objects[key]
	object.Ciphertext[0] ^= 1
	object.SHA256 = sha256.Sum256(object.Ciphertext)
	objects.objects[key] = object
	if _, err := ledger.ReadCommitted(t.Context(), security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}); security.Code(err) != "ledger_corrupt" || bytes.Contains([]byte(err.Error()), []byte("secret")) {
		t.Fatalf("tampered ledger code=%q err=%v", security.Code(err), err)
	}

	providerHeads := &memoryHeads{loadErr: errors.New("provider secret bucket name")}
	heads, err := NewHeads(providerHeads, cipher, testInstanceID, "head-key-v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := heads.Load(t.Context()); err == nil || bytes.Contains([]byte(err.Error()), []byte("secret")) {
		t.Fatalf("unsafe head error=%v", err)
	}
}

func TestLedgerCrossInstanceAndWrongKeyAADFailClosed(t *testing.T) {
	objects := newMemoryObjects()
	cipher := &authenticatedCipher{keys: map[string][]byte{
		"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32),
		"ledger-key-v2": bytes.Repeat([]byte{0x33}, 32),
	}}
	writer, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := writer.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
	for name, instanceAndKey := range map[string][2]string{
		"instance": {"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "ledger-key-v1"},
		"key":      {testInstanceID, "ledger-key-v2"},
	} {
		t.Run(name, func(t *testing.T) {
			reader, err := NewLedger(objects, cipher, &memoryScratch{}, instanceAndKey[0], instanceAndKey[1], 4096)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.ReadCommitted(t.Context(), head); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%q err=%v", security.Code(err), err)
			}
		})
	}
}

func TestProviderTimeoutAfterCreateAndCASReconcilesExactWrite(t *testing.T) {
	objects := newMemoryObjects()
	objects.createAfter = context.DeadlineExceeded
	cipher := &authenticatedCipher{keys: map[string][]byte{
		"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32),
		"head-key-v1":   bytes.Repeat([]byte{0x22}, 32),
	}}
	ledger, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatalf("post-create reconciliation: %v", err)
	}

	providerHeads := &memoryHeads{}
	providerHeads.value = encryptedHeadFixtureWithCipher(t, cipher, security.Head{Generation: 1})
	providerHeads.casAfter = context.DeadlineExceeded
	heads, err := NewHeads(providerHeads, cipher, testInstanceID, "head-key-v1")
	if err != nil {
		t.Fatal(err)
	}
	next := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
	if swapped, err := heads.CompareAndSwap(t.Context(), security.Head{Generation: 1}, next); err != nil || !swapped {
		t.Fatalf("post-CAS reconciliation swapped=%v err=%v", swapped, err)
	}
}

func TestConcurrentIdempotentAppendAcceptsAuthenticatedSameCandidate(t *testing.T) {
	objects := &barrierObjects{memoryObjects: newMemoryObjects(), release: make(chan struct{})}
	cipher := &authenticatedCipher{keys: map[string][]byte{
		"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32),
		"head-key-v1":   bytes.Repeat([]byte{0x22}, 32),
	}}
	left, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, ledger := range []security.SecurityLedger{left, right} {
		go func() {
			<-start
			results <- ledger.AppendCandidate(t.Context(), candidate)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("idempotent append: %v", err)
		}
	}
}

func TestAmbiguousCreateRejectsDifferentStoredBytes(t *testing.T) {
	objects := &ambiguousReplacingObjects{memoryObjects: newMemoryObjects()}
	cipher := &authenticatedCipher{keys: map[string][]byte{
		"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32),
		"head-key-v1":   bytes.Repeat([]byte{0x22}, 32),
	}}
	ledger, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := ledger.AppendCandidate(t.Context(), candidate); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("ambiguous different write code=%q err=%v", security.Code(err), err)
	}
}

func TestAmbiguousReconciliationUsesBoundedInternalContext(t *testing.T) {
	objects := &deadlineCheckingObjects{memoryObjects: newMemoryObjects()}
	cipher := &authenticatedCipher{keys: map[string][]byte{
		"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32),
		"head-key-v1":   bytes.Repeat([]byte{0x22}, 32),
	}}
	ledger, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := ledger.AppendCandidate(t.Context(), candidate); err == nil {
		t.Fatal("ambiguous create unexpectedly succeeded")
	}
	if !objects.reconciliationHadDeadline {
		t.Fatal("ambiguous reconciliation used an unbounded context")
	}

	ciphertext := encryptedHeadFixtureWithCipher(t, cipher, security.Head{Generation: 1})
	headProvider := &deadlineCheckingHeads{value: ciphertext}
	heads, err := NewHeads(headProvider, cipher, testInstanceID, "head-key-v1")
	if err != nil {
		t.Fatal(err)
	}
	if swapped, err := heads.CompareAndSwap(t.Context(), security.Head{Generation: 1}, security.Head{Generation: 1, Revision: 1, EventHash: [32]byte{1}}); err == nil || swapped {
		t.Fatalf("ambiguous head swap=%v err=%v", swapped, err)
	}
	if !headProvider.reconciliationHadDeadline {
		t.Fatal("head reconciliation used an unbounded context")
	}
}

func TestReadCommittedRejectsLargeMaterializationBeforeScratchAllocation(t *testing.T) {
	scratch := &memoryScratch{}
	ledger, err := NewLedger(newMemoryObjects(), &recordingCipher{}, scratch, testInstanceID, "ledger-key-v1", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 4097, EventHash: [32]byte{1}}
	if _, err := ledger.ReadCommitted(t.Context(), head); !errors.Is(err, ErrScratchFull) {
		t.Fatalf("large materialization err=%v", err)
	}
	if scratch.last != nil {
		t.Fatal("large materialization allocated replay scratch")
	}
}

func TestMalformedDecryptedPlaintextIsZeroed(t *testing.T) {
	objects := newMemoryObjects()
	cipher := &retainedPlaintextCipher{plaintext: []byte(`{"not":"a candidate"}`)}
	ledgerValue, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	ledger := ledgerValue.(*Ledger)
	hash := [32]byte{1}
	object := ObjectVersion{Key: ledgerObjectKey(1, 1, hash), Version: "v1", KeyID: "ledger-key-v1", Ciphertext: []byte("opaque")}
	object.SHA256 = sha256.Sum256(object.Ciphertext)
	if _, err := ledger.openObject(t.Context(), object.Key, 1, 1, hash, object); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("malformed plaintext err=%v", err)
	}
	if !allZero(cipher.plaintext) {
		t.Fatalf("ledger plaintext retained=%q", cipher.plaintext)
	}

	headCipher := &retainedPlaintextCipher{plaintext: []byte(`{"not":"a head"}`)}
	headsValue, err := NewHeads(&memoryHeads{}, headCipher, testInstanceID, "head-key-v1")
	if err != nil {
		t.Fatal(err)
	}
	heads := headsValue.(*Heads)
	if _, err := heads.open(t.Context(), EncryptedHeadValue{Version: "v1", KeyID: "head-key-v1", Ciphertext: []byte("opaque")}); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("malformed head plaintext err=%v", err)
	}
	if !allZero(headCipher.plaintext) {
		t.Fatalf("head plaintext retained=%q", headCipher.plaintext)
	}
}

func TestLocallyEncodedPlaintextIsZeroedAfterSeal(t *testing.T) {
	objects := newMemoryObjects()
	cipher := &retainingSealCipher{}
	ledger, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.AppendCandidate(t.Context(), hostedCandidate(t, security.Head{Generation: 1}, 1)); err != nil {
		t.Fatal(err)
	}
	if !allZero(cipher.plaintext) {
		t.Fatalf("encoded ledger plaintext retained=%q", cipher.plaintext)
	}

	headCipher := &retainingSealCipher{}
	provider := &memoryHeads{value: encryptedHeadFixture(t, &recordingCipher{}, security.Head{Generation: 1})}
	heads, err := NewHeads(provider, headCipher, testInstanceID, "head-key-v1")
	if err != nil {
		t.Fatal(err)
	}
	if swapped, err := heads.CompareAndSwap(t.Context(), security.Head{Generation: 1}, security.Head{Generation: 1, Revision: 1, EventHash: [32]byte{1}}); err != nil || !swapped {
		t.Fatalf("head swap=%v err=%v", swapped, err)
	}
	if !allZero(headCipher.plaintext) {
		t.Fatalf("encoded head plaintext retained=%q", headCipher.plaintext)
	}
}

func TestLongChainStreamsThroughBoundedScratch(t *testing.T) {
	objects := newMemoryObjects()
	cipher := &authenticatedCipher{keys: map[string][]byte{"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32)}}
	scratch := &memoryScratch{}
	ledger, err := NewLedger(objects, cipher, scratch, testInstanceID, "ledger-key-v1", 64*48)
	if err != nil {
		t.Fatal(err)
	}
	prior := security.Head{Generation: 1}
	for revision := uint64(1); revision <= 64; revision++ {
		candidate := hostedCandidateWithSerial(t, prior, revision, revision+100)
		if err := ledger.AppendCandidate(t.Context(), candidate); err != nil {
			t.Fatal(err)
		}
		prior = security.Head{Generation: 1, Revision: revision, EventHash: candidate.EventHash}
	}
	count := 0
	if err := ledger.WalkCommitted(t.Context(), prior, func(security.Candidate) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 64 || scratch.last == nil || !scratch.last.closed {
		t.Fatalf("count=%d scratch=%+v", count, scratch.last)
	}
}

func TestCanceledReplayErasesScratch(t *testing.T) {
	objects := newMemoryObjects()
	cipher := &authenticatedCipher{keys: map[string][]byte{"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32)}}
	ctx, cancel := context.WithCancel(t.Context())
	scratch := &memoryScratch{appendHook: cancel}
	ledger, err := NewLedger(objects, cipher, scratch, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
	if err := ledger.WalkCommitted(ctx, head, func(security.Candidate) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled walk err=%v", err)
	}
	if scratch.last == nil || !scratch.last.closed {
		t.Fatal("scratch session was retained after cancellation")
	}
}

func TestHeadTamperWrongKeyAndWrongInstanceFailClosed(t *testing.T) {
	cipher := &authenticatedCipher{keys: map[string][]byte{
		"head-key-v1": bytes.Repeat([]byte{0x22}, 32),
		"head-key-v2": bytes.Repeat([]byte{0x33}, 32),
	}}
	valid := encryptedHeadFixtureWithCipher(t, cipher, security.Head{Generation: 1})
	for name, mutate := range map[string]func(*EncryptedHeadValue, *string){
		"ciphertext": func(value *EncryptedHeadValue, _ *string) { value.Ciphertext[0] ^= 1 },
		"key":        func(value *EncryptedHeadValue, _ *string) { value.KeyID = "head-key-v2" },
		"instance":   func(_ *EncryptedHeadValue, instance *string) { *instance = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			value.Ciphertext = append([]byte(nil), valid.Ciphertext...)
			instance := testInstanceID
			mutate(&value, &instance)
			heads, err := NewHeads(&memoryHeads{value: value}, cipher, instance, "head-key-v1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := heads.Load(t.Context()); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%q err=%v", security.Code(err), err)
			}
		})
	}
}

func TestProviderFaultsBeforeAndAfterCreateAndCAS(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
			objects := newMemoryObjects()
			providerHeads := &memoryHeads{}
			fault := errors.New("provider secret injected fault")
			if after {
				objects.createAfter = fault
				providerHeads.casAfter = fault
			} else {
				objects.createBefore = fault
				providerHeads.casBefore = fault
			}
			cipher := &authenticatedCipher{keys: map[string][]byte{
				"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32),
				"head-key-v1":   bytes.Repeat([]byte{0x22}, 32),
			}}
			ledger, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
			if err != nil {
				t.Fatal(err)
			}
			candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
			ledgerErr := ledger.AppendCandidate(t.Context(), candidate)
			if after && ledgerErr != nil {
				t.Fatalf("after-create did not reconcile: %v", ledgerErr)
			}
			if !after && (ledgerErr == nil || bytes.Contains([]byte(ledgerErr.Error()), []byte("secret"))) {
				t.Fatalf("before-create unsafe err=%v", ledgerErr)
			}

			providerHeads.value = encryptedHeadFixtureWithCipher(t, cipher, security.Head{Generation: 1})
			heads, err := NewHeads(providerHeads, cipher, testInstanceID, "head-key-v1")
			if err != nil {
				t.Fatal(err)
			}
			swapped, headErr := heads.CompareAndSwap(t.Context(), security.Head{Generation: 1}, security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash})
			if after && (headErr != nil || !swapped) {
				t.Fatalf("after-CAS did not reconcile swapped=%v err=%v", swapped, headErr)
			}
			if !after && (headErr == nil || swapped || bytes.Contains([]byte(headErr.Error()), []byte("secret"))) {
				t.Fatalf("before-CAS unsafe swapped=%v err=%v", swapped, headErr)
			}
		})
	}
}

func TestSealOutputMayAliasPlaintextWithoutBeingZeroed(t *testing.T) {
	objects := newMemoryObjects()
	cipher := &aliasSealCipher{}
	ledger, err := NewLedger(objects, cipher, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	object, err := objects.Read(t.Context(), ledgerObjectKey(1, 1, candidate.EventHash))
	if err != nil {
		t.Fatal(err)
	}
	if allZero(object.Ciphertext) {
		t.Fatal("zeroing plaintext destroyed aliased ciphertext")
	}
}

func TestWalkPropagatesVisitorErrorAndReportsCleanupFailure(t *testing.T) {
	objects := newMemoryObjects()
	cipher := &authenticatedCipher{keys: map[string][]byte{"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32)}}
	scratch := &memoryScratch{}
	ledger, err := NewLedger(objects, cipher, scratch, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
	visitorErr := errors.New("stop replay")
	if err := ledger.WalkCommitted(t.Context(), head, func(security.Candidate) error { return visitorErr }); !errors.Is(err, visitorErr) {
		t.Fatalf("visitor error=%v", err)
	}

	scratch.closeErr = errors.New("scratch secret cleanup failure")
	if err := ledger.WalkCommitted(t.Context(), head, func(security.Candidate) error { return nil }); err == nil || bytes.Contains([]byte(err.Error()), []byte("secret")) {
		t.Fatalf("unsafe cleanup error=%v", err)
	}
}

func TestTransientProviderReadIsUnavailableButMissingCommittedObjectIsCorrupt(t *testing.T) {
	objects := newMemoryObjects()
	objects.readErr = errors.New("provider secret outage")
	ledger, err := NewLedger(objects, &recordingCipher{}, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 1, EventHash: [32]byte{1}}
	if _, err := ledger.ReadCommitted(t.Context(), head); security.Code(err) != "service_unavailable" || bytes.Contains([]byte(err.Error()), []byte("secret")) {
		t.Fatalf("provider outage code=%q err=%v", security.Code(err), err)
	}
	objects.readErr = nil
	if _, err := ledger.ReadCommitted(t.Context(), head); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("missing committed object code=%q err=%v", security.Code(err), err)
	}
}

func TestScratchSessionClosesWhenBeginReturnsSessionAndCancellation(t *testing.T) {
	scratch := &beginErrorScratch{err: context.Canceled}
	ledger, err := NewLedger(newMemoryObjects(), &recordingCipher{}, scratch, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 1, EventHash: [32]byte{1}}
	if err := ledger.WalkCommitted(t.Context(), head, func(security.Candidate) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("begin cancellation err=%v", err)
	}
	if scratch.session == nil || !scratch.session.closed {
		t.Fatal("scratch session leaked after Begin returned cancellation")
	}
}

func TestCipherCancellationPropagatesFromAppendAndWalk(t *testing.T) {
	objects := newMemoryObjects()
	writer, err := NewLedger(objects, &recordingCipher{}, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := writer.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	reader, err := NewLedger(objects, errorOpenCipher{err: context.DeadlineExceeded}, &memoryScratch{}, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.AppendCandidate(t.Context(), candidate); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("append open cancellation err=%v", err)
	}
	head := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
	if err := reader.WalkCommitted(t.Context(), head, func(security.Candidate) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("walk open cancellation err=%v", err)
	}
}

func TestWalkReportsConfiguredScratchCapacityExhaustion(t *testing.T) {
	scratch := &memoryScratch{}
	ledger, err := NewLedger(newMemoryObjects(), &recordingCipher{}, scratch, testInstanceID, "ledger-key-v1", 48)
	if err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 2, EventHash: [32]byte{1}}
	if err := ledger.WalkCommitted(t.Context(), head, func(security.Candidate) error { return nil }); !errors.Is(err, ErrScratchFull) {
		t.Fatalf("capacity err=%v", err)
	}
	if scratch.last != nil {
		t.Fatal("capacity failure allocated scratch")
	}
}

type barrierObjects struct {
	*memoryObjects
	barrierMu    sync.Mutex
	missingReads int
	release      chan struct{}
}

type ambiguousReplacingObjects struct {
	*memoryObjects
}

func (store *ambiguousReplacingObjects) Create(ctx context.Context, object ObjectVersion) error {
	if err := store.memoryObjects.Create(ctx, object); err != nil {
		return err
	}
	store.mu.Lock()
	replaced := store.objects[object.Key]
	replaced.Ciphertext = append([]byte(nil), replaced.Ciphertext...)
	replaced.Ciphertext[0] ^= 1
	replaced.SHA256 = sha256.Sum256(replaced.Ciphertext)
	store.objects[object.Key] = replaced
	store.mu.Unlock()
	return errors.New("provider ambiguous secret detail")
}

type deadlineCheckingObjects struct {
	*memoryObjects
	reads                     int
	reconciliationHadDeadline bool
}

type deadlineCheckingHeads struct {
	value                     EncryptedHeadValue
	loads                     int
	reconciliationHadDeadline bool
}

func (store *deadlineCheckingHeads) Load(ctx context.Context) (EncryptedHeadValue, error) {
	store.loads++
	if store.loads == 1 {
		return store.value, nil
	}
	_, store.reconciliationHadDeadline = ctx.Deadline()
	return EncryptedHeadValue{}, errors.New("provider secret detail")
}

func (store *deadlineCheckingHeads) CompareAndSwap(context.Context, string, EncryptedHeadValue) (bool, error) {
	return false, errors.New("provider ambiguous secret detail")
}

func (store *deadlineCheckingObjects) Read(ctx context.Context, key string) (ObjectVersion, error) {
	store.reads++
	if store.reads == 1 {
		return ObjectVersion{}, ErrObjectNotFound
	}
	_, store.reconciliationHadDeadline = ctx.Deadline()
	return ObjectVersion{}, errors.New("provider secret detail")
}

func (store *deadlineCheckingObjects) Create(context.Context, ObjectVersion) error {
	return errors.New("provider ambiguous secret detail")
}

type retainedPlaintextCipher struct {
	plaintext []byte
}

type aliasSealCipher struct{}

func (*aliasSealCipher) Seal(_ context.Context, _ string, plaintext, _ []byte) ([]byte, error) {
	return plaintext, nil
}

func (*aliasSealCipher) Open(_ context.Context, _ string, ciphertext, _ []byte) ([]byte, error) {
	return append([]byte(nil), ciphertext...), nil
}

type errorOpenCipher struct{ err error }

func (cipher errorOpenCipher) Seal(_ context.Context, _ string, plaintext, _ []byte) ([]byte, error) {
	return append([]byte(nil), plaintext...), nil
}

func (cipher errorOpenCipher) Open(context.Context, string, []byte, []byte) ([]byte, error) {
	return nil, cipher.err
}

func (cipher *retainedPlaintextCipher) Seal(context.Context, string, []byte, []byte) ([]byte, error) {
	return []byte("opaque"), nil
}

func (cipher *retainedPlaintextCipher) Open(context.Context, string, []byte, []byte) ([]byte, error) {
	return cipher.plaintext, nil
}

type retainingSealCipher struct {
	plaintext []byte
}

func (cipher *retainingSealCipher) Seal(_ context.Context, _ string, plaintext, _ []byte) ([]byte, error) {
	cipher.plaintext = plaintext
	return append([]byte("sealed:"), plaintext...), nil
}

func (cipher *retainingSealCipher) Open(_ context.Context, _ string, ciphertext, _ []byte) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte("sealed:")) {
		return nil, errors.New("invalid ciphertext")
	}
	return append([]byte(nil), ciphertext[len("sealed:"):]...), nil
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func (store *barrierObjects) Read(ctx context.Context, key string) (ObjectVersion, error) {
	object, err := store.memoryObjects.Read(ctx, key)
	if !errors.Is(err, ErrObjectNotFound) {
		return object, err
	}
	store.barrierMu.Lock()
	store.missingReads++
	ordinal := store.missingReads
	if ordinal == 2 {
		close(store.release)
	}
	store.barrierMu.Unlock()
	if ordinal <= 2 {
		select {
		case <-ctx.Done():
			return ObjectVersion{}, ctx.Err()
		case <-store.release:
		}
	}
	return ObjectVersion{}, ErrObjectNotFound
}

func TestReplayScratchFailureAlwaysErasesSession(t *testing.T) {
	objects := newMemoryObjects()
	cipher := &authenticatedCipher{keys: map[string][]byte{"ledger-key-v1": bytes.Repeat([]byte{0x11}, 32)}}
	scratch := &memoryScratch{appendErr: ErrScratchFull}
	ledger, err := NewLedger(objects, cipher, scratch, testInstanceID, "ledger-key-v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	candidate := hostedCandidate(t, security.Head{Generation: 1}, 1)
	if err := ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
	if err := ledger.WalkCommitted(t.Context(), head, func(security.Candidate) error { return nil }); !errors.Is(err, ErrScratchFull) {
		t.Fatalf("scratch error=%v", err)
	}
	if scratch.last == nil || !scratch.last.closed {
		t.Fatal("scratch session was retained after failure")
	}
}

type memoryObjects struct {
	mu           sync.Mutex
	objects      map[string]ObjectVersion
	createBefore error
	createAfter  error
	readErr      error
}

func newMemoryObjects() *memoryObjects {
	return &memoryObjects{objects: map[string]ObjectVersion{}}
}

func (store *memoryObjects) Create(_ context.Context, object ObjectVersion) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.createBefore != nil {
		return store.createBefore
	}
	if _, ok := store.objects[object.Key]; ok {
		return ErrObjectExists
	}
	object.Version = fmt.Sprintf("object-%d", len(store.objects)+1)
	object.Ciphertext = append([]byte(nil), object.Ciphertext...)
	store.objects[object.Key] = object
	return store.createAfter
}

func (store *memoryObjects) Read(_ context.Context, key string) (ObjectVersion, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.readErr != nil {
		return ObjectVersion{}, store.readErr
	}
	object, ok := store.objects[key]
	if !ok {
		return ObjectVersion{}, ErrObjectNotFound
	}
	object.Ciphertext = append([]byte(nil), object.Ciphertext...)
	return object, nil
}

type recordingCipher struct {
	lastSealAAD []byte
}

type memoryHeads struct {
	mu           sync.Mutex
	value        EncryptedHeadValue
	loadErr      error
	lastExpected string
	version      int
	casBefore    error
	casAfter     error
}

func (store *memoryHeads) Load(_ context.Context) (EncryptedHeadValue, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.loadErr != nil {
		return EncryptedHeadValue{}, store.loadErr
	}
	value := store.value
	value.Ciphertext = append([]byte(nil), value.Ciphertext...)
	return value, nil
}

func (store *memoryHeads) CompareAndSwap(_ context.Context, expected string, next EncryptedHeadValue) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.lastExpected = expected
	if store.casBefore != nil {
		return false, store.casBefore
	}
	if store.value.Version != expected {
		return false, nil
	}
	store.version++
	next.Version = fmt.Sprintf("head-%d", store.version+1)
	next.Ciphertext = append([]byte(nil), next.Ciphertext...)
	store.value = next
	return true, store.casAfter
}

func encryptedHeadFixture(t *testing.T, cipher EnvelopeCipher, head security.Head) EncryptedHeadValue {
	t.Helper()
	plaintext := []byte(fmt.Sprintf(`{"version":"security-head-v1","generation":%d,"revision":%d,"event_hash":"%x","instance_id":"%s"}`, head.Generation, head.Revision, head.EventHash, testInstanceID))
	ciphertext, err := cipher.Seal(t.Context(), "head-key-v1", plaintext, literalFrame([]byte("baby-tracker-security-head-aad-v1"), []byte(testInstanceID), []byte("head-key-v1")))
	if err != nil {
		t.Fatal(err)
	}
	return EncryptedHeadValue{Version: "head-1", KeyID: "head-key-v1", Ciphertext: ciphertext}
}

func encryptedHeadFixtureWithCipher(t *testing.T, cipher EnvelopeCipher, head security.Head) EncryptedHeadValue {
	t.Helper()
	plaintext, err := encodeHead(head, testInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := cipher.Seal(t.Context(), "head-key-v1", plaintext, headAAD(testInstanceID, "head-key-v1"))
	if err != nil {
		t.Fatal(err)
	}
	return EncryptedHeadValue{Version: "head-1", KeyID: "head-key-v1", Ciphertext: ciphertext}
}

type authenticatedCipher struct {
	keys map[string][]byte
}

func (cipher *authenticatedCipher) Seal(_ context.Context, keyID string, plaintext, aad []byte) ([]byte, error) {
	key, ok := cipher.keys[keyID]
	if !ok {
		return nil, errors.New("secret key unavailable")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher2GCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, aead.Seal(nil, nonce, plaintext, aad)...), nil
}

func (cipher *authenticatedCipher) Open(_ context.Context, keyID string, ciphertext, aad []byte) ([]byte, error) {
	key, ok := cipher.keys[keyID]
	if !ok {
		return nil, errors.New("secret key unavailable")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher2GCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aead.NonceSize() {
		return nil, errors.New("secret ciphertext truncated")
	}
	return aead.Open(nil, ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():], aad)
}

func cipher2GCM(block cipher.Block) (cipher.AEAD, error) {
	return cipher.NewGCM(block)
}

func (cipher *recordingCipher) Seal(_ context.Context, _ string, plaintext, aad []byte) ([]byte, error) {
	cipher.lastSealAAD = append([]byte(nil), aad...)
	return append([]byte("sealed:"), plaintext...), nil
}

func (cipher *recordingCipher) Open(_ context.Context, _ string, ciphertext, _ []byte) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte("sealed:")) {
		return nil, errors.New("authentication failed with secret provider detail")
	}
	return append([]byte(nil), ciphertext[len("sealed:"):]...), nil
}

type memoryScratch struct {
	last       *memoryScratchSession
	appendErr  error
	appendHook func()
	closeErr   error
}

type beginErrorScratch struct {
	session *memoryScratchSession
	err     error
}

func (scratch *beginErrorScratch) Begin(_ context.Context, request ReplayScratchRequest) (ReplayScratchSession, error) {
	scratch.session = &memoryScratchSession{maxBytes: request.MaxBytes}
	return scratch.session, scratch.err
}

func (scratch *memoryScratch) Begin(_ context.Context, request ReplayScratchRequest) (ReplayScratchSession, error) {
	scratch.last = &memoryScratchSession{maxBytes: request.MaxBytes, appendErr: scratch.appendErr, appendHook: scratch.appendHook, closeErr: scratch.closeErr}
	return scratch.last, nil
}

type memoryScratchSession struct {
	entries    []ReplayIndexEntry
	maxBytes   int64
	closed     bool
	appendErr  error
	appendHook func()
	closeErr   error
}

func (session *memoryScratchSession) Append(_ context.Context, entry ReplayIndexEntry) error {
	if session.appendErr != nil {
		return session.appendErr
	}
	if session.appendHook != nil {
		session.appendHook()
		session.appendHook = nil
	}
	if int64(len(session.entries)+1)*48 > session.maxBytes {
		return ErrScratchFull
	}
	session.entries = append(session.entries, entry)
	return nil
}

func (session *memoryScratchSession) WalkReverse(ctx context.Context, visit func(ReplayIndexEntry) error) error {
	for index := len(session.entries) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(session.entries[index]); err != nil {
			return err
		}
	}
	return nil
}

func (session *memoryScratchSession) Close() error {
	for index := range session.entries {
		session.entries[index] = ReplayIndexEntry{}
	}
	session.entries = nil
	session.closed = true
	return session.closeErr
}

func hostedCandidate(t *testing.T, prior security.Head, revision uint64) security.Candidate {
	return hostedCandidateWithSerial(t, prior, revision, revision)
}

func hostedCandidateWithSerial(t *testing.T, prior security.Head, revision, serial uint64) security.Candidate {
	t.Helper()
	commandID := fmt.Sprintf("30000000-0000-4000-8000-%012d", serial)
	accountID := fmt.Sprintf("10000000-0000-4000-8000-%012d", serial)
	event := security.Event{
		Version: "v1", Generation: prior.Generation, Revision: revision,
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

func literalFrame(fields ...[]byte) []byte {
	var result []byte
	for _, field := range fields {
		length := len(field)
		result = append(result, byte(length>>24), byte(length>>16), byte(length>>8), byte(length))
		result = append(result, field...)
	}
	return result
}

func equalCandidate(left, right security.Candidate) bool {
	return left.CommandID == right.CommandID && left.Prior == right.Prior && left.EventHash == right.EventHash && bytes.Equal(left.EventJSON, right.EventJSON)
}
