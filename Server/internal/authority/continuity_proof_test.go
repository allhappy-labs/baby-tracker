package authority_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
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

// A rotation given the parent ResolveRefreshParent found rotates it in one
// transaction without reading it by refresh digest again, and decides
// exactly as a rotation that looks it up: a parent consumed since it was
// resolved is re-read under the lock (an exact retry recovers, another
// attempt is a conflict), and a parent the candidates do not name is
// invalid_command.
func TestRefreshWithResolvedParent(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	_, _, otherRefresh := f.Session(t, account, device, "account", "", "")
	service, w := countedService(t, f)

	parent, err := service.ResolveRefreshParent(t.Context(), candidates(f.TokenKeys, rawRefresh))
	if err != nil {
		t.Fatal(err)
	}
	if got, status := parent.Device(); got != *binding(account, device) || status != "active" {
		t.Fatalf("device %+v %s", got, status)
	}
	mismatched := refreshOf(f, otherRefresh, newAttempt(t), binding(account, device))
	mismatched.Parent = parent
	_, err = service.Refresh(t.Context(), mismatched)
	wantCode(t, err, "invalid_command")

	attempt := newAttempt(t)
	in := refreshOf(f, rawRefresh, attempt, binding(account, device))
	in.Parent = parent
	w.reset()
	got, err := service.Refresh(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	_, statements, _ := w.snapshot()
	if n := statementsMatching(statements, "BEGIN ISOLATION"); n != 1 {
		t.Fatalf("rotation opened %d transactions: %q", n, statements)
	}
	if n := statementsMatching(statements, "refresh_digest = ANY("); n != 0 {
		t.Fatalf("rotation read the parent by refresh digest again: %q", statements)
	}
	// The same resolved parent, now consumed: the exact retry recovers the
	// child and another attempt is a conflict, as without a resolved parent.
	retry, err := service.Refresh(t.Context(), in)
	if err != nil || !retry.Recovered || retry.Child.ID != got.Child.ID {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	conflict := refreshOf(f, rawRefresh, newAttempt(t), binding(account, device))
	conflict.Parent = parent
	_, err = service.Refresh(t.Context(), conflict)
	wantCode(t, err, "refresh_attempt_conflict")
	_, err = service.Refresh(t.Context(), refreshOf(f, rawRefresh, newAttempt(t), binding(account, device)))
	wantCode(t, err, "session_revoked")
}

// A parent revoked, or deleted by retention, between ResolveRefreshParent and
// the rotation is refused by the locked re-read with the code a rotation
// that looks the parent up itself returns, legacy and recoverable alike, and
// nothing is written.
func TestRefreshRevokedOrDeletedAfterResolve(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	for _, change := range []struct{ name, sql string }{
		{"revoked", `UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='member_removed' WHERE id=$1`},
		{"deleted", `DELETE FROM sessions WHERE id=$1 AND $2::timestamptz IS NOT NULL`},
	} {
		for _, attempt := range []string{"", newAttempt(t)} {
			parentID, _, rawRefresh := f.Session(t, account, device, "account", "", "")
			parent, err := f.Service.ResolveRefreshParent(t.Context(), candidates(f.TokenKeys, rawRefresh))
			if err != nil {
				t.Fatal(err)
			}
			inAuthorityTx(t, f, func(tx pgx.Tx) {
				if _, err := tx.Exec(t.Context(), change.sql, parentID, f.Now); err != nil {
					t.Fatal(err)
				}
			})
			var proof *authority.DeviceBinding
			if attempt != "" {
				proof = binding(account, device)
			}
			in := refreshOf(f, rawRefresh, attempt, proof)
			_, lookedUp := f.Service.Refresh(t.Context(), in)
			in.Parent = parent
			_, resolved := f.Service.Refresh(t.Context(), in)
			if authority.CodeOf(lookedUp) != "session_revoked" || authority.CodeOf(resolved) != authority.CodeOf(lookedUp) {
				t.Fatalf("%s (attempt %q): resolved parent %v, looked up %v", change.name, attempt, resolved, lookedUp)
			}
			if n := count(t, f, `SELECT count(*) FROM sessions WHERE parent_id='`+parentID+`'`); n != 0 {
				t.Fatalf("%s: a child was issued", change.name)
			}
		}
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 0 {
		t.Fatalf("%d receipts written", n)
	}
}

// A resolved parent whose epoch the table owner moved since (the runtime
// cannot) is refused under the lock as the unlocked lookup refuses it:
// epoch_changed, nothing written.
func TestRefreshEpochMovedAfterResolve(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	parentID, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	parent, err := f.Service.ResolveRefreshParent(t.Context(), candidates(f.TokenKeys, rawRefresh))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.DB.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	for _, statement := range []string{`SELECT set_config('app.authority','1',true)`, `UPDATE sessions SET recovery_epoch=recovery_epoch+1 WHERE id='` + parentID + `'`} {
		if _, err := tx.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	in := refreshOf(f, rawRefresh, newAttempt(t), binding(account, device))
	_, lookedUp := f.Service.Refresh(t.Context(), in)
	in.Parent = parent
	_, resolved := f.Service.Refresh(t.Context(), in)
	wantCode(t, lookedUp, "epoch_changed")
	wantCode(t, resolved, "epoch_changed")
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE parent_id='`+parentID+`'`); n != 0 {
		t.Fatal("a child was issued")
	}
}

// When the acknowledgement's BEGIN fails (a database that refuses
// read-write transactions), the bearer is still authenticated first, as the
// two calls did: an unknown or expired bearer keeps its 401 code, and a
// valid one is service_unavailable. The receipt is untouched.
func TestAcknowledgeByTokenWhenBeginFails(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	attempt := newAttempt(t)
	got, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	accept := (&proofRecorder{accept: true}).prove
	defer authority.SetAckBegin(`SELECT 1/0`)()
	wantCode(t, f.Service.AuthenticateAndAcknowledgeRefresh(t.Context(), accessDigests(f.TokenKeys, strings.Repeat("A", 43)), attempt, accept),
		"session_revoked")
	wantCode(t, f.Service.AuthenticateAndAcknowledgeRefresh(t.Context(), accessDigests(f.TokenKeys, got.AccessToken), attempt, accept),
		"service_unavailable")
	later := serviceOn(t, f, f.Pool, func(o *authority.Options) { o.Now = func() time.Time { return f.Now.Add(time.Hour) } })
	wantCode(t, later.AuthenticateAndAcknowledgeRefresh(t.Context(), accessDigests(f.TokenKeys, got.AccessToken), attempt, accept),
		"access_expired")
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 1 {
		t.Fatalf("%d receipts, want 1", n)
	}
}
