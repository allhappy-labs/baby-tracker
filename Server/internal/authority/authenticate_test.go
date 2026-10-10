package authority_test

import (
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5"
)

// accessDigests is an access token's candidate digests under every token key.
func accessDigests(keys identity.DigestKeyring, raw string) []authority.KeyedDigest {
	out := []authority.KeyedDigest{{KeyID: keys.Current.ID, Sum: authoritytest.TokenDigest(keys.Current, "access", raw)}}
	for _, key := range keys.Previous {
		out = append(out, authority.KeyedDigest{KeyID: key.ID, Sum: authoritytest.TokenDigest(key, "access", raw)})
	}
	return out
}

func TestAuthenticateIsOneQueryAndMapsEveryCode(t *testing.T) {
	f := authoritytest.New(t)
	start := f.Now
	s, w := countedService(t, f)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	family := f.Family(t, account)

	type tc struct {
		name   string
		scope  string
		mutate func(session string)
		at     time.Time
		keyID  string // "" means the session's key
		code   string
	}
	cases := []tc{
		{name: "active family session", scope: "family", at: start, code: ""},
		{name: "active account session", scope: "account", at: start, code: ""},
		{name: "access expired at the exact expiry", scope: "family", at: start.Add(time.Hour), code: "access_expired"},
		{name: "revoked", scope: "family", at: start, code: "session_revoked", mutate: func(id string) {
			f.Exec(t, `UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='member_removed' WHERE id=$1`, id, start)
		}},
		{name: "consumed", scope: "family", at: start, code: "session_revoked", mutate: func(id string) {
			f.Exec(t, `UPDATE sessions SET status='consumed', consumed_at=$2, child_id=$3 WHERE id=$1`, id, start, authoritytest.NewID(t))
		}},
		{name: "past the one-year lineage deadline", scope: "family", at: start, code: "reauthentication_required", mutate: func(id string) {
			f.Exec(t, `UPDATE sessions SET lineage_started_at=$2 WHERE id=$1`, id, start.Add(-365*24*time.Hour))
		}},
		{name: "past deadline_at", scope: "family", at: start.Add(30 * time.Minute), code: "reauthentication_required", mutate: func(id string) {
			f.Exec(t, `UPDATE sessions SET access_expires_at=$2, refresh_expires_at=$2, deadline_at=$2 WHERE id=$1`, id, start.Add(30*time.Minute))
		}},
		{name: "wrong epoch", scope: "family", at: start, code: "epoch_changed", mutate: func(id string) {
			f.Exec(t, `UPDATE sessions SET recovery_epoch=2 WHERE id=$1`, id)
		}},
		{name: "account chain failed", scope: "family", at: start, code: "scope_failed", mutate: func(string) {
			f.Exec(t, `UPDATE accounts SET chain_failed=true WHERE id=$1`, account)
		}},
		{name: "digest under another key name", scope: "family", at: start, keyID: "session.v0", code: "session_revoked"},
	}
	// Warm the connection's statement cache: steady state is what is measured.
	_, raw, _ := f.Session(t, account, device, "family", family, "owner")
	if _, err := s.Authenticate(t.Context(), accessDigests(f.TokenKeys, raw)); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		f.Now = start
		f.Exec(t, `UPDATE accounts SET chain_failed=false WHERE id=$1`, account)
		var session, raw string
		if c.scope == "family" {
			session, raw, _ = f.Session(t, account, device, "family", family, "owner")
		} else {
			session, raw, _ = f.Session(t, account, device, "account", "", "")
		}
		if c.mutate != nil {
			c.mutate(session)
		}
		digests := accessDigests(f.TokenKeys, raw)
		if c.keyID != "" {
			digests[0].KeyID = c.keyID
		}
		f.Now = c.at
		w.reset()
		p, err := s.Authenticate(t.Context(), digests)
		trips, statements, batches := w.snapshot()
		if trips != 1 || len(statements) != 1 || batches != 0 {
			t.Errorf("%s: %d round trips, %d statements %q, %d batches; want one query", c.name, trips, len(statements), statements, batches)
		}
		if authority.CodeOf(err) != c.code {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.code)
			continue
		}
		if c.code != "" {
			continue
		}
		want := principalOf(f, t, session)
		want.AccessExpiresAt = want.AccessExpiresAt.UTC()
		// The account holds the family's active slot, so an account row is
		// family scope (TestAuthenticateDerivesFamilyScopeLikeLegacy).
		want.Scope = "family"
		if p != want {
			t.Errorf("%s: principal %+v, want %+v", c.name, p, want)
		}
		if p.Scope != "family" || p.SessionScope != c.scope || p.SecurityRevision < 1 || !p.AccessExpiresAt.Equal(start.Add(time.Hour)) {
			t.Errorf("%s: principal %+v", c.name, p)
		}
	}

	// An unknown token and a malformed candidate list are session_revoked.
	f.Now = start
	_, err := s.Authenticate(t.Context(), accessDigests(f.TokenKeys, "not-a-token"))
	wantCode(t, err, "session_revoked")
	_, err = s.Authenticate(t.Context(), nil)
	wantCode(t, err, "session_revoked")
}

// A family_recovery session authenticates with the recovery scope, and a
// token key rotation keeps sessions issued under the previous key valid.
func TestAuthenticateRecoveryScopeAndPreviousKey(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	family := f.Family(t, account)
	session, raw, _ := f.Session(t, account, device, "family_recovery", family, "owner")
	// A recovery session exists only beside a deletion-recovery slot; the
	// principal's scope is derived from it (TestAuthenticateDerivesFamilyScopeLikeLegacy).
	f.Exec(t, `UPDATE family_slots SET state='family_recovery' WHERE account_id=$1`, account)
	p, err := f.Service.Authenticate(t.Context(), accessDigests(f.TokenKeys, raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope != "family_recovery" || p.SessionScope != "family_recovery" || p.SessionFamilyID != family || p.SessionID != session {
		t.Fatalf("recovery principal %+v", p)
	}

	rotated := f.TokenKeys
	rotated.Previous = []identity.DigestKey{f.TokenKeys.Current}
	rotated.Current = identity.DigestKey{ID: "session.v2"}
	for i := range rotated.Current.Secret {
		rotated.Current.Secret[i] = 0x33
	}
	s := serviceOn(t, f, f.Pool, func(o *authority.Options) { o.TokenKeys = rotated })
	if p, err := s.Authenticate(t.Context(), accessDigests(rotated, raw)); err != nil || p.SessionID != session {
		t.Fatalf("previous-key session: %+v %v", p, err)
	}
}

// Principal.Scope is derived as legacy Authenticate derived it
// (auth/refresh.go authorityFamilySlot at 071dc4d): an active slot with an
// active membership in that family is family scope whatever the session row
// says (a session signed in before its family existed); a deletion-recovery
// slot is family_recovery only for a session row that refresh already
// narrowed to family_recovery, and account scope for any other row (a fresh
// identity proof); no slot is account scope. SessionScope and
// SessionFamilyID stay the session row's. Authenticate and the combined
// guard derive it from the same one query.
func TestAuthenticateDerivesFamilyScopeLikeLegacy(t *testing.T) {
	f := authoritytest.New(t)
	type want struct{ scope, sessionScope, sessionFamily string }
	check := func(name, raw string, w want) {
		t.Helper()
		p, err := f.Service.Authenticate(t.Context(), accessDigests(f.TokenKeys, raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Scope != w.scope || p.SessionScope != w.sessionScope || p.SessionFamilyID != w.sessionFamily {
			t.Errorf("%s: scope=%q session scope=%q session family=%q, want %+v", name, p.Scope, p.SessionScope, p.SessionFamilyID, w)
		}
	}

	// No slot: account.
	loner := liveAccount(t, f)
	_, raw, _ := f.Session(t, loner, f.Device(t, loner), "account", "", "")
	check("account session without a slot", raw, want{"account", "account", ""})

	// Active slot and active membership: family, from an account row too.
	owner := liveAccount(t, f)
	ownerDevice := f.Device(t, owner)
	family := f.Family(t, owner)
	_, accountRaw, _ := f.Session(t, owner, ownerDevice, "account", "", "")
	check("account session with an active slot", accountRaw, want{"family", "account", ""})
	_, familyRaw, _ := f.Session(t, owner, ownerDevice, "family", family, "owner")
	check("family session with an active slot", familyRaw, want{"family", "family", family})
	called := false
	if err := f.Service.WithRequestTx(t.Context(), authority.Request{Digests: accessDigests(f.TokenKeys, accountRaw)}, "read",
		func(_ pgx.Tx, p access.Principal, g access.Grant) error {
			called = true
			if p.Scope != "family" || p.SessionScope != "account" || p.SessionFamilyID != "" || g.FamilyID != family {
				t.Errorf("combined guard principal=%+v grant=%+v", p, g)
			}
			return nil
		}); err != nil || !called {
		t.Fatalf("combined guard called=%v err=%v", called, err)
	}

	// Active slot whose membership is not active: account.
	former := liveAccount(t, f)
	formerFamily := f.Family(t, former)
	_, formerRaw, _ := f.Session(t, former, f.Device(t, former), "account", "", "")
	f.Exec(t, `UPDATE memberships SET status='former' WHERE family_id=$1 AND account_id=$2`, formerFamily, former)
	check("account session with an active slot but no active membership", formerRaw, want{"account", "account", ""})

	// Deletion-recovery slot: family_recovery only for a narrowed row.
	recovering := liveAccount(t, f)
	recoveringDevice := f.Device(t, recovering)
	recoveringFamily := f.Family(t, recovering)
	_, proofRaw, _ := f.Session(t, recovering, recoveringDevice, "account", "", "")
	_, narrowedRaw, _ := f.Session(t, recovering, recoveringDevice, "family_recovery", recoveringFamily, "owner")
	f.Exec(t, `UPDATE family_slots SET state='family_recovery' WHERE account_id=$1`, recovering)
	check("fresh proof with a recovery slot", proofRaw, want{"account", "account", ""})
	check("recovery session with a recovery slot", narrowedRaw, want{"family_recovery", "family_recovery", recoveringFamily})

	// A recovery row whose slot is gone (a purge started): account.
	f.Exec(t, `DELETE FROM family_slots WHERE account_id=$1`, recovering)
	check("recovery session without a slot", narrowedRaw, want{"account", "family_recovery", recoveringFamily})
}
