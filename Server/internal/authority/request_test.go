package authority_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRequestRefusalOrder(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	viewer := seated(t, f, joined(t, f, f.Service, owner, family, "viewer"), family, "viewer")
	var admitted, charged, ran int
	req := func(digests []authority.KeyedDigest, admit, charge error) authority.Request {
		return authority.Request{Digests: digests,
			Admit:  func(access.Principal) error { admitted++; return admit },
			Charge: func(access.Principal, access.Grant) error { charged++; return charge }}
	}
	run := func(pgx.Tx, access.Principal, access.Grant) error { ran++; return nil }
	unknown := accessDigests(f.TokenKeys, base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	wantCode(t, f.Service.WithRequestTx(t.Context(), req(unknown, nil, nil), "read", run), "session_revoked")
	if admitted+charged+ran != 0 {
		t.Fatalf("an unknown credential reached the caller: %d %d %d", admitted, charged, ran)
	}
	held := errors.New("invalid_request")
	if err := f.Service.WithRequestTx(t.Context(), req(viewer.digests(f), held, nil), "mutate_record", run); !errors.Is(err, held) {
		t.Fatalf("Admit's refusal must win over family policy (read_only_caregiver): %v", err)
	}
	wantCode(t, f.Service.WithRequestTx(t.Context(), req(viewer.digests(f), nil, nil), "mutate_record", run), "read_only_caregiver")
	if charged != 0 || ran != 0 {
		t.Fatalf("a policy refusal charged %d, ran %d", charged, ran)
	}
	limited := errors.New("rate_limited")
	if err := f.Service.WithRequestTx(t.Context(), req(viewer.digests(f), nil, limited), "read", run); !errors.Is(err, limited) || ran != 0 {
		t.Fatalf("Charge's refusal: %v, ran %d", err, ran)
	}
	var grant access.Grant
	if err := f.Service.WithRequestTx(t.Context(), req(viewer.digests(f), nil, nil), "read",
		func(_ pgx.Tx, p access.Principal, g access.Grant) error { grant = g; return nil }); err != nil || grant.Role != "viewer" || grant.FamilyID != family {
		t.Fatalf("grant %+v %v", grant, err)
	}
}

func TestRequestRoundTrips(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	st := seated(t, f, owner, family, "owner")
	s, w := countedService(t, f)
	ok := authority.Request{Digests: st.digests(f)}
	refused := authority.Request{Digests: st.digests(f), Charge: func(access.Principal, access.Grant) error { return errors.New("rate_limited") }}
	guarded := s.WithRequestTx
	_ = guarded(t.Context(), ok, "read", func(pgx.Tx, access.Principal, access.Grant) error { return nil }) // prepares
	w.reset()
	var before int
	if err := guarded(t.Context(), ok, "read", func(pgx.Tx, access.Principal, access.Grant) error { before, _, _ = w.snapshot(); return nil }); err != nil {
		t.Fatal(err)
	}
	if total, _, _ := w.snapshot(); before != 1 || total != 2 {
		t.Fatalf("%d round trips before run, %d with commit; want 1 and 2", before, total)
	}
	w.reset()
	_ = guarded(t.Context(), refused, "read", func(pgx.Tx, access.Principal, access.Grant) error { return nil })
	if total, _, _ := w.snapshot(); total != 2 {
		t.Fatalf("a charged refusal took %d round trips, want 2 (request + rollback)", total)
	}
}

func TestAuthorizeRequestIsOneStatementWithoutATransaction(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	st := seated(t, f, owner, family, "owner")
	s, w := countedService(t, f)
	_, _, _ = s.AuthorizeRequest(t.Context(), authority.Request{Digests: st.digests(f)}, "read") // prepares
	w.reset()
	p, g, err := s.AuthorizeRequest(t.Context(), authority.Request{Digests: st.digests(f)}, "read")
	trips, statements, _ := w.snapshot()
	if err != nil || g.FamilyID != family || p.AccountID != owner.account || trips != 1 || len(statements) != 1 || statementsMatching(statements, "BEGIN") != 0 {
		t.Fatalf("authorize: %v, %d round trips, statements %q", err, trips, statements)
	}
}

// Every refusal class, through each entry point, costs the authority round
// trip and (for a transaction) its rollback, and reaches exactly the hooks
// that rank before it: authentication refusals reach neither hook, family
// policy refusals reach Admit only, a Charge refusal reaches both. run never
// runs.
func TestRequestRefusalRoundTrips(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	ownerSeat := seated(t, f, owner, family, "owner")
	viewer := seated(t, f, joined(t, f, f.Service, owner, family, "viewer"), family, "viewer")
	removed := seated(t, f, joined(t, f, f.Service, owner, family, "caregiver"), family, "caregiver")
	if _, err := f.Service.RemoveMember(t.Context(), authority.RemoveMember{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account,
		AccountID: removed.account, At: f.Now, Principal: owner.principal(t, f)}); err != nil {
		t.Fatal(err)
	}
	recoverySession, recoveryRaw, _ := f.Session(t, owner.account, owner.device, "family_recovery", family, "owner")
	recovery := seat{member: owner, familySession: recoverySession, access: recoveryRaw}
	loner := newMember(t, f)
	_, lonerRaw, _ := f.Session(t, loner.account, loner.device, "account", "", "")
	lonerSeat := seat{member: loner, access: lonerRaw}
	unknown := accessDigests(f.TokenKeys, base64.RawURLEncoding.EncodeToString(make([]byte, 32)))

	pool, w := countedPool(t, f, nil)
	now := serviceOn(t, f, pool, nil)
	later := serviceOn(t, f, pool, func(o *authority.Options) { o.Now = func() time.Time { return f.Now.Add(2 * time.Hour) } })

	held, limited := errors.New("held refusal"), errors.New("rate limited")
	const (
		authentication = iota // neither hook runs
		policy                // Admit runs, Charge does not
		charge                // both run
	)
	cases := []struct {
		name    string
		service *authority.Service
		digests []authority.KeyedDigest
		action  string
		admit   error
		charge  error
		code    string
		want    error
		reached int
	}{
		{"unknown token", now, unknown, "read", nil, nil, "session_revoked", nil, authentication},
		{"expired access", later, ownerSeat.digests(f), "read", nil, nil, "access_expired", nil, authentication},
		{"revoked session", now, removed.digests(f), "read", nil, nil, "session_revoked", nil, authentication},
		{"no family", now, lonerSeat.digests(f), "read", nil, nil, "family_unavailable", nil, policy},
		{"recovery family", now, recovery.digests(f), "read", nil, nil, "recovery_only", nil, policy},
		{"unknown action", now, ownerSeat.digests(f), "drop_tables", nil, nil, "membership_revoked", nil, policy},
		{"role refused", now, viewer.digests(f), "mutate_record", nil, nil, "read_only_caregiver", nil, policy},
		{"Admit refusal", now, viewer.digests(f), "mutate_record", held, nil, "", held, policy},
		{"Charge refusal", now, ownerSeat.digests(f), "read", nil, limited, "", limited, charge},
	}
	type entry struct {
		name              string
		trips, statements int
		call              func(s *authority.Service, req authority.Request, action string, ran *bool) error
	}
	entries := []entry{
		{"WithRequestTx", 2, 3, func(s *authority.Service, req authority.Request, action string, ran *bool) error { // BEGIN+authority, rollback
			return s.WithRequestTx(t.Context(), req, action, func(pgx.Tx, access.Principal, access.Grant) error { *ran = true; return nil })
		}},
		{"AuthorizeRequest", 1, 1, func(s *authority.Service, req authority.Request, action string, _ *bool) error {
			_, _, err := s.AuthorizeRequest(t.Context(), req, action)
			return err
		}},
	}
	for _, e := range entries {
		for _, c := range cases {
			var admitted, charged int
			var ran bool
			req := authority.Request{Digests: c.digests,
				Admit:  func(access.Principal) error { admitted++; return c.admit },
				Charge: func(access.Principal, access.Grant) error { charged++; return c.charge }}
			_ = e.call(c.service, req, c.action, &ran) // prepares
			admitted, charged, ran = 0, 0, false
			w.reset()
			err := e.call(c.service, req, c.action, &ran)
			trips, statements, _ := w.snapshot()
			if c.want != nil {
				if !errors.Is(err, c.want) {
					t.Errorf("%s, %s: err %v, want %v", e.name, c.name, err, c.want)
				}
			} else if got := authority.CodeOf(err); got != c.code {
				t.Errorf("%s, %s: err %v (code %q), want %q", e.name, c.name, err, got, c.code)
			}
			wantAdmitted, wantCharged := 0, 0
			if c.reached >= policy {
				wantAdmitted = 1
			}
			if c.reached >= charge {
				wantCharged = 1
			}
			if ran || admitted != wantAdmitted || charged != wantCharged {
				t.Errorf("%s, %s: ran %v, Admit %d (want %d), Charge %d (want %d)", e.name, c.name, ran, admitted, wantAdmitted, charged, wantCharged)
			}
			if trips != e.trips || len(statements) != e.statements || statementsMatching(statements, "app.request_authority") != 1 {
				t.Errorf("%s, %s: %d round trips, statements %q; want %d and %d statements", e.name, c.name, trips, statements, e.trips, e.statements)
			}
			if e.trips == 1 && statementsMatching(statements, "BEGIN") != 0 {
				t.Errorf("%s, %s: opened a transaction: %q", e.name, c.name, statements)
			}
			// A refused transaction ends in exactly one rollback and never
			// commits (Task 4 review m1): nothing a refusal reached persists.
			if e.trips == 2 && (exactStatements(statements, "rollback") != 1 || exactStatements(statements, "commit") != 0) {
				t.Errorf("%s, %s: a refusal did not end in one rollback without a commit: %q", e.name, c.name, statements)
			}
			t.Logf("%s, %s: %d round trip(s)", e.name, c.name, trips)
		}
	}
	// A malformed credential (no digests) is refused before any database work.
	w.reset()
	for _, e := range entries {
		var ran bool
		wantCode(t, e.call(now, authority.Request{}, "read", &ran), "session_revoked")
	}
	if trips, statements, _ := w.snapshot(); trips != 0 || len(statements) != 0 {
		t.Fatalf("no digests: %d round trips, statements %q", trips, statements)
	}
}

// The family and role come only from the authenticated session row: run,
// Admit and Charge see one principal and one grant, the tenant context is
// the grant's, the authority flag is down and authority tables are hidden.
func TestRequestGrantAndTenantContextComeFromTheSession(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver := seated(t, f, joined(t, f, f.Service, owner, family, "caregiver"), family, "caregiver")
	// An account-scope session (signed in, not refreshed) resolves its
	// family through the account's slot, never through the request.
	_, ownerAccountRaw, _ := f.Session(t, owner.account, owner.device, "account", "", "")
	ownerAccount := seat{member: owner, access: ownerAccountRaw}
	other := newMember(t, f)
	otherFamily := createFamily(t, f, f.Service, other)
	cases := []struct {
		seat               seat
		role, sessionScope string
	}{
		{caregiver, "caregiver", "family"},
		{ownerAccount, "owner", "account"},
	}
	for _, c := range cases {
		var admitted access.Principal
		var charged access.Grant
		req := authority.Request{Digests: c.seat.digests(f),
			Admit:  func(p access.Principal) error { admitted = p; return nil },
			Charge: func(_ access.Principal, g access.Grant) error { charged = g; return nil }}
		err := f.Service.WithRequestTx(t.Context(), req, "read", func(tx pgx.Tx, p access.Principal, g access.Grant) error {
			if p != admitted || g != charged {
				t.Errorf("run saw %+v %+v; Admit saw %+v, Charge saw %+v", p, g, admitted, charged)
			}
			if g.FamilyID != family || g.Role != c.role || g.AccountID != c.seat.account || p.AccountID != c.seat.account ||
				p.SessionScope != c.sessionScope || p.Scope != "family" || g.Revision != familyRevision(t, f, family) {
				t.Errorf("%s: principal %+v grant %+v", c.role, p, g)
			}
			var account, current, level string
			var flag bool
			if err := tx.QueryRow(t.Context(), `SELECT coalesce(app.current_account_id()::text,''), coalesce(app.current_family_id()::text,''),
					app.authority_context(), current_setting('transaction_isolation')`).Scan(&account, &current, &flag, &level); err != nil {
				return err
			}
			if account != g.AccountID || current != g.FamilyID || flag || level != "read committed" {
				t.Errorf("%s: tenant account=%s family=%s authority=%v isolation=%s", c.role, account, current, flag, level)
			}
			if n := countIn(t.Context(), t, tx, `SELECT count(*) FROM sessions`); n != 0 {
				t.Errorf("%s: the tenant sees %d sessions", c.role, n)
			}
			if n := countIn(t.Context(), t, tx, `SELECT count(*) FROM families WHERE id=$1`, otherFamily); n != 0 {
				t.Errorf("%s: the tenant sees another family", c.role)
			}
			_, err := tx.Exec(t.Context(), `UPDATE families SET next_sequence=next_sequence WHERE id=$1`, g.FamilyID)
			return err
		})
		if err != nil {
			t.Fatalf("%s: a write returned %v", c.role, err)
		}
	}
}

// AuthorizeRequest's tenant context is local to its one implicit
// transaction: the pooled connection keeps none of it.
func TestAuthorizeRequestLeavesNoContextOnTheConnection(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	st := seated(t, f, owner, family, "owner")
	pool, _ := countedPool(t, f, nil)
	s := serviceOn(t, f, pool, nil)
	if _, g, err := s.AuthorizeRequest(t.Context(), authority.Request{Digests: st.digests(f)}, "read"); err != nil || g.FamilyID != family {
		t.Fatalf("authorize: %+v %v", g, err)
	}
	var account, current, flag string
	if err := pool.QueryRow(t.Context(), `SELECT coalesce(current_setting('app.account_id',true),''), coalesce(current_setting('app.family_id',true),''),
			coalesce(current_setting('app.authority',true),'')`).Scan(&account, &current, &flag); err != nil {
		t.Fatal(err)
	}
	if account != "" || current != "" || flag != "" {
		t.Fatalf("settings leaked account=%q family=%q authority=%q", account, current, flag)
	}
}

// A revocation committed before a request's transaction begins refuses it
// (before Admit); a request already past authorization completes and
// commits.
func TestRequestRevocationAppliesToTheNextRequest(t *testing.T) {
	for _, change := range []string{"remove", "demote"} {
		t.Run(change, func(t *testing.T) {
			f := authoritytest.New(t)
			owner := newMember(t, f)
			family := createFamily(t, f, f.Service, owner)
			caregiver := seated(t, f, joined(t, f, f.Service, owner, family, "caregiver"), family, "caregiver")
			inside, resume, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			babyID := authoritytest.NewID(t)
			go func() {
				done <- f.Service.WithRequestTx(context.Background(), authority.Request{Digests: caregiver.digests(f)}, "mutate_record",
					func(tx pgx.Tx, _ access.Principal, g access.Grant) error {
						close(inside)
						<-resume
						return insertBaby(context.Background(), tx, g, caregiver.device, babyID)
					})
			}()
			<-inside
			var err error
			if change == "remove" {
				_, err = f.Service.RemoveMember(t.Context(), authority.RemoveMember{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account,
					AccountID: caregiver.account, At: f.Now, Principal: owner.principal(t, f)})
			} else {
				_, err = f.Service.ChangeRole(t.Context(), authority.ChangeRole{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account,
					AccountID: caregiver.account, Role: "viewer", At: f.Now, Principal: owner.principal(t, f)})
			}
			if err != nil {
				t.Fatal(err)
			}
			close(resume)
			if err := <-done; err != nil {
				t.Fatalf("in-flight request: %v", err)
			}
			if n := countBabies(t, f, family); n != 1 {
				t.Fatalf("the in-flight request committed %d babies, want 1", n)
			}
			want := map[string]string{"remove": "session_revoked", "demote": "read_only_caregiver"}[change]
			var admitted, ran int
			req := authority.Request{Digests: caregiver.digests(f), Admit: func(access.Principal) error { admitted++; return nil }}
			wantCode(t, f.Service.WithRequestTx(t.Context(), req, "mutate_record", func(pgx.Tx, access.Principal, access.Grant) error { ran++; return nil }), want)
			_, _, err = f.Service.AuthorizeRequest(t.Context(), req, "mutate_record")
			wantCode(t, err, want)
			if ran != 0 {
				t.Fatal("a request after the revoking commit ran")
			}
			if wantAdmitted := map[string]int{"remove": 0, "demote": 2}[change]; admitted != wantAdmitted {
				t.Fatalf("Admit ran %d times, want %d", admitted, wantAdmitted)
			}
		})
	}
}

// A panic in Admit or Charge (inside the open transaction) propagates, rolls
// back and returns the connection clean to the pool.
func TestRequestHookPanicLeavesConnectionClean(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	st := seated(t, f, owner, family, "owner")
	// A one-connection pool closed at cleanup only if nothing is held, so a
	// leaked connection fails this test at once instead of hanging Close.
	config := f.Pool.Config().Copy()
	config.MaxConns, config.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if pool.Stat().AcquiredConns() == 0 {
			pool.Close()
		}
	})
	s := serviceOn(t, f, pool, nil)
	for _, hook := range []string{"Admit", "Charge"} {
		req := authority.Request{Digests: st.digests(f)}
		if hook == "Admit" {
			req.Admit = func(access.Principal) error { panic("boom") }
		} else {
			req.Charge = func(access.Principal, access.Grant) error { panic("boom") }
		}
		func() {
			defer func() {
				if r := recover(); r != "boom" {
					t.Fatalf("%s: recovered %v, want the hook's panic", hook, r)
				}
			}()
			_ = s.WithRequestTx(t.Context(), req, "read", func(pgx.Tx, access.Principal, access.Grant) error { return nil })
		}()
		if held := pool.Stat().AcquiredConns(); held != 0 {
			t.Fatalf("after a %s panic %d connection(s) are still held", hook, held)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		err := s.WithRequestTx(ctx, authority.Request{Digests: st.digests(f)}, "read", func(tx pgx.Tx, _ access.Principal, g access.Grant) error {
			var current string
			if err := tx.QueryRow(ctx, `SELECT coalesce(app.current_family_id()::text,'')`).Scan(&current); err != nil || current != family {
				t.Errorf("after a %s panic: family context %q %v", hook, current, err)
			}
			return nil
		})
		cancel()
		if err != nil {
			t.Fatalf("after a %s panic the next request failed (connection not released?): %v", hook, err)
		}
	}
}

func TestRequestWithoutRunIsUnavailable(t *testing.T) {
	f := authoritytest.New(t)
	wantCode(t, f.Service.WithRequestTx(t.Context(), authority.Request{}, "read", nil), "service_unavailable")
}

// exactStatements counts the statements that are exactly sql (the
// transaction control statements pgx sends: "rollback", "commit").
func exactStatements(statements []string, sql string) int {
	n := 0
	for _, s := range statements {
		if strings.TrimSpace(s) == sql {
			n++
		}
	}
	return n
}
