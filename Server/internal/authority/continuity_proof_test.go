package authority_test

import (
	"sync"
	"testing"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
)

// proofRecorder is a ProveContinuity callback that records the stored
// bindings it was shown and answers accept.
type proofRecorder struct {
	mu     sync.Mutex
	seen   []authority.DeviceBinding
	accept bool
}

func (r *proofRecorder) prove(stored authority.DeviceBinding) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, stored)
	return r.accept
}

// A recoverable refresh proven by callback is one transaction: the callback
// sees the device row's binding under the locks, a refusal is
// device_continuity_invalid with nothing written, and an exact retry
// re-derives the child from the proven binding.
func TestRefreshProvesContinuityInsideOneTransaction(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	parentID, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	service, w := countedService(t, f)
	attempt := newAttempt(t)

	refused := &proofRecorder{}
	in := refreshOf(f, rawRefresh, attempt, nil)
	in.ProveContinuity = refused.prove
	w.reset()
	_, err := service.Refresh(t.Context(), in)
	wantCode(t, err, "device_continuity_invalid")
	if len(refused.seen) != 1 || refused.seen[0] != *binding(account, device) {
		t.Fatalf("proof saw %+v", refused.seen)
	}
	if parent, _ := sessionRow(t, f, parentID); parent.Status != "active" || count(t, f, `SELECT count(*) FROM refresh_receipts`) != 0 {
		t.Fatalf("refused proof wrote: parent=%s", parent.Status)
	}

	accepted := &proofRecorder{accept: true}
	in.ProveContinuity = accepted.prove
	w.reset()
	got, err := service.Refresh(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	_, statements, _ := w.snapshot()
	if n := statementsMatching(statements, "BEGIN ISOLATION"); n != 1 {
		t.Fatalf("recoverable refresh opened %d transactions: %q", n, statements)
	}
	// The proven binding is the continuity the derivation and material see,
	// so the same retry with an equal binding recovers the same child.
	retry, err := service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
	if err != nil || !retry.Recovered || retry.Child.ID != got.Child.ID || retry.AccessToken != got.AccessToken {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	again := refreshOf(f, rawRefresh, attempt, nil)
	again.ProveContinuity = (&proofRecorder{accept: true}).prove
	if recovered, err := service.Refresh(t.Context(), again); err != nil || !recovered.Recovered || recovered.RefreshToken != got.RefreshToken {
		t.Fatalf("callback retry=%+v err=%v", recovered, err)
	}

	both := refreshOf(f, rawRefresh, attempt, binding(account, device))
	both.ProveContinuity = accepted.prove
	_, err = service.Refresh(t.Context(), both)
	wantCode(t, err, "invalid_command")
	legacy := refreshOf(f, rawRefresh, "", nil)
	legacy.ProveContinuity = accepted.prove
	_, err = service.Refresh(t.Context(), legacy)
	wantCode(t, err, "invalid_command")
}

// An acknowledgement proven by callback is one transaction, and a refused
// proof keeps the receipt.
func TestAcknowledgeRefreshProvesContinuityInsideOneTransaction(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	attempt := newAttempt(t)
	got, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	p := principalOf(f, t, got.Child.ID)
	service, w := countedService(t, f)

	refused := &proofRecorder{}
	wantCode(t, service.ProveAndAcknowledgeRefresh(t.Context(), p, attempt, refused.prove), "device_continuity_invalid")
	if len(refused.seen) != 1 || refused.seen[0] != *binding(account, device) || count(t, f, `SELECT count(*) FROM refresh_receipts`) != 1 {
		t.Fatalf("refused proof: seen=%+v", refused.seen)
	}
	wantCode(t, service.ProveAndAcknowledgeRefresh(t.Context(), p, attempt, nil), "device_continuity_invalid")

	w.reset()
	if err := service.ProveAndAcknowledgeRefresh(t.Context(), p, attempt, (&proofRecorder{accept: true}).prove); err != nil {
		t.Fatal(err)
	}
	_, statements, _ := w.snapshot()
	if n := statementsMatching(statements, "BEGIN ISOLATION"); n != 1 {
		t.Fatalf("acknowledgement opened %d transactions: %q", n, statements)
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 0 {
		t.Fatal("acknowledgement kept the receipt")
	}
}
