package security

import (
	"babytracker/server/internal/access"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/diagnostics"
)

// This adapter models the optional retained-inventory contract. Filesystem
// canonical envelope and path attacks are covered by localstore's own tests.
type inventoryFixture struct {
	*fixtureLedger
	passes int
	cold   int
}

func (l *inventoryFixture) ReadValidatedInventory(ctx context.Context, head Head) (CommittedInventory, []Candidate, error) {
	l.passes++
	l.cold++
	candidates, err := l.ReadCommitted(ctx, head)
	inv := CommittedInventory{Head: head}
	for _, c := range candidates {
		inv.Files = append(inv.Files, CommittedFile{Revision: c.Prior.Revision + 1, Prior: c.Prior, EventHash: c.EventHash, RawDigest: sha256.Sum256(c.EventJSON)})
	}
	return inv, candidates, err
}
func (l *inventoryFixture) VerifyInventory(ctx context.Context, inv CommittedInventory) error {
	l.passes++
	for _, f := range inv.Files {
		c, ok := l.candidates[f.EventHash]
		if !ok || sha256.Sum256(c.EventJSON) != f.RawDigest {
			return ErrLedgerCorrupt
		}
	}
	return ctx.Err()
}
func (l *inventoryFixture) ReadValidatedExtension(ctx context.Context, inv CommittedInventory, head Head) (CommittedInventory, []Candidate, error) {
	if err := l.VerifyInventory(ctx, inv); err != nil {
		return CommittedInventory{}, nil, err
	}
	all, err := l.ReadCommitted(ctx, head)
	if err != nil {
		return CommittedInventory{}, nil, err
	}
	if head.Revision < inv.Head.Revision {
		return CommittedInventory{}, nil, ErrLedgerCorrupt
	}
	next := CommittedInventory{Head: head, Files: append([]CommittedFile(nil), inv.Files...)}
	extension := all[inv.Head.Revision:]
	for _, c := range extension {
		next.Files = append(next.Files, CommittedFile{Revision: c.Prior.Revision + 1, Prior: c.Prior, EventHash: c.EventHash, RawDigest: sha256.Sum256(c.EventJSON)})
	}
	return next, extension, nil
}
func cacheFixture(t *testing.T) (*authorityFixture, *inventoryFixture, *AuthorityBinding) {
	h := newAuthority(t)
	l := &inventoryFixture{fixtureLedger: h.ledger}
	b := &AuthorityBinding{InstanceID: "40000000-0000-4000-8000-000000000001", Epoch: 1, TrustedHead: Head{Generation: 1}}
	h.Service.ledger = l
	h.Service.binding = func(context.Context) (AuthorityBinding, error) { return *b, nil }
	p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account", RecoveryEpoch: 1}
	if _, err := h.Service.Execute(t.Context(), p, h.Register(p.AccountID, "30000000-0000-4000-8000-000000000001")); err != nil {
		t.Fatal(err)
	}
	b.TrustedHead = h.Head()
	// Exercise cold readers independently from write-successor publication.
	var err error
	h.Service, err = New(completeTestOptions(Options{Ledger: l, Heads: h.heads, Projection: h.Projection, Fence: h.Service.fence, Now: h.clock, Binding: func(context.Context) (AuthorityBinding, error) { return *b, nil }}))
	if err != nil {
		t.Fatal(err)
	}
	l.passes = 0
	l.cold = 0
	return h, l, b
}
func readView(t *testing.T, h *authorityFixture) {
	t.Helper()
	if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestWarmAuthorityRejectsRollbackAndCorruption(t *testing.T) {
	for _, warm := range []bool{false, true} {
		for _, attack := range []string{"rollback", "hash", "generation", "missing", "bytes", "projection", "epoch", "instance", "quarantine"} {
			t.Run(fmt.Sprintf("warm=%v/%s", warm, attack), func(t *testing.T) {
				h, _, b := cacheFixture(t)
				if warm {
					readView(t, h)
				}
				switch attack {
				case "rollback":
					h.heads.head = Head{Generation: 1}
					h.Projection.head = h.heads.head
				case "hash":
					h.heads.head.EventHash[0] ^= 1
				case "generation":
					h.heads.head.Generation++
				case "missing":
					delete(h.ledger.candidates, h.Head().EventHash)
				case "bytes":
					c := h.ledger.candidates[h.Head().EventHash]
					c.EventJSON = append(c.EventJSON, ' ')
					h.ledger.candidates[h.Head().EventHash] = c
				case "projection":
					h.Projection.head = Head{Generation: 1}
				case "epoch":
					b.Epoch = 0
				case "instance":
					b.InstanceID = ""
				case "quarantine":
					h.Service.setQuarantine(true)
				}
				for i := 0; i < 2; i++ {
					if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { t.Error("exposed invalid authority"); return nil }); err == nil {
						t.Fatal("accepted drift")
					}
				}
			})
		}
	}
}
func TestWarmAuthorityAcceptsExternalExtension(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprint(warm), func(t *testing.T) {
			h, l, _ := cacheFixture(t)
			if warm {
				readView(t, h)
			}
			// A separate service commits under the same fence without touching this cache.
			other, err := New(completeTestOptions(Options{Ledger: h.ledger, Heads: h.heads, Projection: h.Projection, Fence: h.Service.fence, Now: h.clock}))
			if err != nil {
				t.Fatal(err)
			}
			p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000002", Scope: "account", RecoveryEpoch: 1}
			if _, err := other.Execute(t.Context(), p, newVerifiedIdentityRegistration(p.AccountID, "sha256:other-test-identity", "30000000-0000-4000-8000-000000000002")); err != nil {
				t.Fatal(err)
			}
			if err := h.Service.WithAuthorityView(t.Context(), func(v AuthorityView) error {
				if _, ok := v.Account(p.AccountID); !ok {
					t.Error("external append missing")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if l.cold != 1 {
				t.Fatalf("cold reconstructions=%d", l.cold)
			}
		})
	}
}
func TestWarmAuthorityBindingRequiresReconcile(t *testing.T) {
	for _, field := range []string{"epoch", "instance"} {
		t.Run(field, func(t *testing.T) {
			h, _, b := cacheFixture(t)
			readView(t, h)
			if field == "epoch" {
				b.Epoch++
			} else {
				b.InstanceID = "40000000-0000-4000-8000-000000000002"
			}
			for i := 0; i < 2; i++ {
				if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { return nil }); err == nil {
					t.Fatal("binding change accepted")
				}
			}
			if _, err := h.Service.ReconcileTrusted(t.Context(), b.TrustedHead); err != nil {
				t.Fatal(err)
			}
			readView(t, h)
		})
	}
}

func TestWarmAuthorityRejectsRollbackAfterLocalCommit(t *testing.T) {
	h, _, _ := cacheFixture(t)
	readView(t, h)
	prior := h.Head()
	p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000002", Scope: "account", RecoveryEpoch: 1}
	if _, err := h.Service.Execute(t.Context(), p, newVerifiedIdentityRegistration(p.AccountID, "sha256:second-identity", "30000000-0000-4000-8000-000000000002")); err != nil {
		t.Fatal(err)
	}
	h.heads.head = prior
	h.Projection.head = prior
	for i := 0; i < 2; i++ {
		if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { return nil }); err == nil {
			t.Fatal("accepted rollback to cached head after local commit")
		}
	}
}
func TestWarmAuthorityFailedReconcileRetainsProof(t *testing.T) {
	h, _, b := cacheFixture(t)
	readView(t, h)
	b.Epoch++
	h.Projection.failOnce = true
	h.Projection.head = Head{Generation: 1}
	if _, err := h.Service.ReconcileTrusted(t.Context(), b.TrustedHead); err == nil {
		t.Fatal("reconciled failed projection")
	}
	if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { return nil }); err == nil {
		t.Fatal("failed reconcile reset proof")
	}
	if _, err := h.Service.ReconcileTrusted(t.Context(), b.TrustedHead); err != nil {
		t.Fatal(err)
	}
	readView(t, h)
	// A later rollback cannot use the interval between reconcile and warm inventory.
	if _, err := h.Service.ReconcileTrusted(t.Context(), b.TrustedHead); err != nil {
		t.Fatal(err)
	}
	h.heads.head = Head{Generation: 1}
	h.Projection.head = h.heads.head
	if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { return nil }); err == nil {
		t.Fatal("reconcile discarded high-water proof")
	}
}
func TestWarmAuthorityUnboundAndUnsupportedAdaptersDoFullValidation(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprint(bound), func(t *testing.T) {
			h, l, _ := cacheFixture(t)
			if bound {
				h.Service.ledger = h.ledger
			} else {
				h.Service.binding = nil
			}
			readView(t, h)
			readView(t, h)
			if l.passes != 0 {
				t.Fatal("fallback accelerated")
			}
			c := h.ledger.candidates[h.Head().EventHash]
			c.EventJSON = append(c.EventJSON, ' ')
			h.ledger.candidates[h.Head().EventHash] = c
			if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { return nil }); err == nil {
				t.Fatal("fallback accepted corruption")
			}
		})
	}
}

func TestWarmAuthorityRejectsTrustedRootRollback(t *testing.T) {
	h, _, b := cacheFixture(t)
	readView(t, h)
	b.TrustedHead = Head{Generation: 1}
	for i := 0; i < 2; i++ {
		if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { return nil }); err == nil {
			t.Fatal("accepted persistent trusted-root rollback")
		}
	}
}

func TestWarmAuthorityReconcilePopulatesInventoryOnce(t *testing.T) {
	h, l, b := cacheFixture(t)
	if _, err := h.Service.ReconcileTrusted(t.Context(), b.TrustedHead); err != nil {
		t.Fatal(err)
	}
	readView(t, h)
	readView(t, h)
	if l.cold != 1 || l.passes != 2 {
		t.Fatalf("post-reconcile cold=%d passes=%d", l.cold, l.passes)
	}
}

func TestWarmAuthorityConcurrentExtensionPublishesOneOwner(t *testing.T) {
	h, l, _ := cacheFixture(t)
	readView(t, h)
	other, err := New(completeTestOptions(Options{Ledger: h.ledger, Heads: h.heads, Projection: h.Projection, Fence: h.Service.fence, Now: h.clock}))
	if err != nil {
		t.Fatal(err)
	}
	p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000002", Scope: "account", RecoveryEpoch: 1}
	if _, err := other.Execute(t.Context(), p, newVerifiedIdentityRegistration(p.AccountID, "sha256:second-identity", "30000000-0000-4000-8000-000000000002")); err != nil {
		t.Fatal(err)
	}
	results := make(chan *validatedAuthority, 16)
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() {
			errs <- h.Service.WithAuthorityView(t.Context(), func(v AuthorityView) error { results <- v.owner; return nil })
		}()
	}
	for i := 0; i < 16; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	owner := <-results
	for i := 1; i < 16; i++ {
		if <-results != owner {
			t.Fatal("concurrent readers cloned duplicate successors")
		}
	}
	if l.passes != 17 {
		t.Fatalf("historical passes=%d", l.passes)
	}
}

func TestWarmAuthorityBindingCallbackOutsideCacheMutex(t *testing.T) {
	h, _, b := cacheFixture(t)
	h.Service.binding = func(context.Context) (AuthorityBinding, error) {
		if !h.Service.cacheMu.TryLock() {
			t.Error("binding callback invoked under cache mutex")
			return AuthorityBinding{}, ErrUnavailable
		}
		h.Service.cacheMu.Unlock()
		return *b, nil
	}
	readView(t, h)
	readView(t, h)
}

func TestWarmAuthorityDeniedViewRetainsExternalProof(t *testing.T) {
	for _, warm := range []bool{false, true} {
		for _, denial := range []string{"projection", "quarantine"} {
			for _, drift := range []string{"head", "trusted_root", "epoch", "instance"} {
				t.Run(fmt.Sprintf("warm=%v/%s/%s", warm, denial, drift), func(t *testing.T) {
					h, l, b := cacheFixture(t)
					// Start a genuinely cold service with neither an owner nor a local-CAS floor.
					var err error
					h.Service, err = New(completeTestOptions(Options{Ledger: l, Heads: h.heads, Projection: h.Projection, Fence: h.Service.fence, Now: h.clock,
						Binding: func(context.Context) (AuthorityBinding, error) { return *b, nil }}))
					if err != nil {
						t.Fatal(err)
					}
					if warm {
						readView(t, h)
					}
					h1 := h.Head()
					other, err := New(completeTestOptions(Options{Ledger: h.ledger, Heads: h.heads, Projection: h.Projection, Fence: h.Service.fence, Now: h.clock}))
					if err != nil {
						t.Fatal(err)
					}
					p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000002", Scope: "account", RecoveryEpoch: 1}
					if _, err := other.Execute(t.Context(), p, newVerifiedIdentityRegistration(p.AccountID, "sha256:second-identity", "30000000-0000-4000-8000-000000000002")); err != nil {
						t.Fatal(err)
					}
					if drift == "trusted_root" {
						b.TrustedHead = h.Head()
					}
					if denial == "projection" {
						h.Projection.head = h1
					} else {
						h.Service.setQuarantine(true)
					}
					callbacks := 0
					if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { callbacks++; return nil }); err == nil || callbacks != 0 {
						t.Fatalf("denied view escaped: err=%v callbacks=%d", err, callbacks)
					}
					// Remove the denial itself; the newly learned durable proof must independently
					// reject the drift, including on every subsequent attempted read.
					h.Projection.head = h.Head()
					h.Service.setQuarantine(false)
					// A transient binding-read failure must not erase the proof
					// either; its subsequent return cannot bootstrap a new root.
					bindingReader := h.Service.binding
					h.Service.binding = func(context.Context) (AuthorityBinding, error) { return AuthorityBinding{}, ErrUnavailable }
					if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { callbacks++; return nil }); err == nil {
						t.Fatal("binding failure accepted")
					}
					h.Service.binding = bindingReader
					switch drift {
					case "head":
						h.heads.head = h1
						h.Projection.head = h1
					case "trusted_root":
						b.TrustedHead = h1
					case "epoch":
						b.Epoch++
					case "instance":
						b.InstanceID = "40000000-0000-4000-8000-000000000002"
					}
					for attempt := 0; attempt < 2; attempt++ {
						if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { callbacks++; return nil }); err == nil {
							t.Errorf("attempt %d accepted %s after verified H2 denial", attempt, drift)
						}
					}
					if callbacks != 0 {
						t.Errorf("exposed %d unauthorized views", callbacks)
					}
				})
			}
		}
	}
}

func TestIntegrityMetricFlushOutsideAuthorityLocks(t *testing.T) {
	h, _, _ := cacheFixture(t)
	calls := 0
	ctx := diagnostics.WithRecorder(t.Context(), func(_ context.Context, phase diagnostics.Phase, elapsed time.Duration) {
		if phase != diagnostics.IntegrityInventory && phase != diagnostics.IntegrityExtension && phase != diagnostics.IntegrityVerify {
			return
		}
		calls++
		if elapsed < 0 {
			t.Fatal("invalid integrity sample")
		}
		if !h.Service.cacheMu.TryLock() {
			t.Fatal("observer under cache lock")
		}
		h.Service.cacheMu.Unlock()
		if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { return nil }); err != nil {
			t.Fatal("observer could not reenter", err)
		}
	})
	if err := h.Service.WithAuthorityView(ctx, func(AuthorityView) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("observer ran before outer lease returned")
	}
	diagnostics.Flush(ctx)
	if calls == 0 {
		t.Fatal("actual integrity hook not observed")
	}
}

// An unavailable error that carries its cause (fmt.Errorf("%w: %w",
// ErrUnavailable, cause)) is still the same code to every mapper: the cause
// is for the log line, never for the client.
func TestUnavailableWrappedWithItsCauseKeepsItsCode(t *testing.T) {
	wrapped := fmt.Errorf("%w: %w", ErrUnavailable, fmt.Errorf("pool: %w", context.Canceled))
	if Code(wrapped) != "service_unavailable" || !IsCodedError(wrapped) || !errors.Is(wrapped, ErrUnavailable) || !errors.Is(wrapped, context.Canceled) {
		t.Fatalf("wrapped unavailable lost its code or cause: %v", wrapped)
	}
}
