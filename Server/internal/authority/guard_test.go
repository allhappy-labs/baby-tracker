package authority_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// seat is a family member's family-scope session as a refresh after joining
// leaves it: the member, the session ID and its raw access token.
type seat struct {
	member
	familySession, access string
}

func seated(t *testing.T, f *authoritytest.Fixture, m member, family, role string) seat {
	t.Helper()
	id, raw, _ := f.Session(t, m.account, m.device, "family", family, role)
	return seat{member: m, familySession: id, access: raw}
}

func (st seat) digests(f *authoritytest.Fixture) []authority.KeyedDigest {
	return accessDigests(f.TokenKeys, st.access)
}

func (st seat) authenticated(t *testing.T, f *authoritytest.Fixture) access.Principal {
	t.Helper()
	p, err := f.Service.Authenticate(t.Context(), st.digests(f))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// insertBaby writes one care baby row as the tenant inside a guarded
// transaction, taking the family row lock and its next sequence as the care
// write path does.
func insertBaby(ctx context.Context, tx pgx.Tx, g access.Grant, device, babyID string) error {
	var seq int64
	if err := tx.QueryRow(ctx, `UPDATE families SET next_sequence=next_sequence+1 WHERE id=$1 RETURNING next_sequence`, g.FamilyID).Scan(&seq); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO care_entities(family_id,id,kind,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,
		operation,accepted_at,sort_order,name_hash,wire)
		VALUES($1,$2,1,1,$3,$4,$4,$5,'create',now(),0,sha256(convert_to('Baby','UTF8')),convert_to(repeat('x',64),'UTF8'))`,
		g.FamilyID, babyID, seq, g.AccountID, device)
	return err
}

func countIn(ctx context.Context, t *testing.T, tx pgx.Tx, query string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// The binding round-trip budget: the combined forms reach run after exactly
// one round trip (BEGIN and app.request_authority pipelined in one batch).
// The principal forms' budget is BEGIN plus one batch; they pipeline BEGIN
// too, so they also reach run after one. Commit is one more.
func TestRequestTransactionRoundTrips(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	st := seated(t, f, owner, family, "owner")
	p := st.authenticated(t, f)
	s, w := countedService(t, f)

	type entry struct {
		name     string
		maxTrips int
		call     func(run func(pgx.Tx)) error
	}
	entries := []entry{
		{"WithRequestTx", 1, func(run func(pgx.Tx)) error {
			return s.WithRequestTx(t.Context(), authority.Request{Digests: st.digests(f)}, "mutate_record", func(tx pgx.Tx, _ access.Principal, _ access.Grant) error { run(tx); return nil })
		}},
		{"WithFamilyTx", 1, func(run func(pgx.Tx)) error {
			return s.WithFamilyTx(t.Context(), p, "mutate_record", func(tx pgx.Tx, _ access.Grant) error { run(tx); return nil })
		}},
		{"WithFamilyReadTx", 1, func(run func(pgx.Tx)) error {
			return s.WithFamilyReadTx(t.Context(), p, "read", func(tx pgx.Tx, _ access.Grant) error { run(tx); return nil })
		}},
		{"WithAccountTx", 1, func(run func(pgx.Tx)) error {
			return s.WithAccountTx(t.Context(), p, func(tx pgx.Tx) error { run(tx); return nil })
		}},
	}
	for _, e := range entries {
		// The first call on the connection prepares the statements.
		if err := e.call(func(pgx.Tx) {}); err != nil {
			t.Fatalf("%s warm-up: %v", e.name, err)
		}
		for round := 0; round < 3; round++ {
			w.reset()
			var trips, batches int
			var statements []string
			if err := e.call(func(pgx.Tx) { trips, statements, batches = w.snapshot() }); err != nil {
				t.Fatalf("%s: %v", e.name, err)
			}
			total, _, _ := w.snapshot()
			if trips > e.maxTrips || batches != 1 || len(statements) != 2 || statementsMatching(statements, "app.request_authority") != 1 {
				t.Errorf("%s round %d: %d round trips, %d batches, statements %q before run", e.name, round, trips, batches, statements)
			}
			if total != trips+1 {
				t.Errorf("%s round %d: %d round trips including commit, want %d", e.name, round, total, trips+1)
			}
			t.Logf("%s: %d round trip(s) before run, %d with commit", e.name, trips, total)
		}
	}

	// A refusal costs the same round trip plus the rollback.
	viewer := seated(t, f, joined(t, f, f.Service, owner, family, "viewer"), family, "viewer")
	called := false
	err := s.WithRequestTx(t.Context(), authority.Request{Digests: viewer.digests(f)}, "mutate_record", func(pgx.Tx, access.Principal, access.Grant) error { called = true; return nil })
	wantCode(t, err, "read_only_caregiver")
	w.reset()
	err = s.WithRequestTx(t.Context(), authority.Request{Digests: viewer.digests(f)}, "mutate_record", func(pgx.Tx, access.Principal, access.Grant) error { called = true; return nil })
	wantCode(t, err, "read_only_caregiver")
	if trips, _, _ := w.snapshot(); trips != 2 || called {
		t.Fatalf("refusal: %d round trips, called=%v; want 2 (request + rollback)", trips, called)
	}
}

// Revocation after commit wins: a transaction that passed authorization
// completes and commits; every transaction that starts after the revoking
// commit is refused. Removal revokes the member's sessions in the removing
// transaction (session_revoked, legacy parity); a demotion rewrites the
// session role in the demoting transaction (read_only_caregiver).
func TestRevocationAppliesToNextTransaction(t *testing.T) {
	for _, change := range []string{"remove", "demote"} {
		t.Run(change, func(t *testing.T) {
			f := authoritytest.New(t)
			owner := newMember(t, f)
			family := createFamily(t, f, f.Service, owner)
			caregiver := seated(t, f, joined(t, f, f.Service, owner, family, "caregiver"), family, "caregiver")
			p := caregiver.authenticated(t, f)

			inside, resume, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			babyID := authoritytest.NewID(t)
			go func() {
				done <- f.Service.WithFamilyTx(context.Background(), p, "mutate_record", func(tx pgx.Tx, g access.Grant) error {
					close(inside)
					<-resume
					// The in-flight transaction still writes after the revoking commit.
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
				t.Fatalf("in-flight transaction: %v", err)
			}
			if n := countBabies(t, f, family); n != 1 {
				t.Fatalf("in-flight write committed %d babies, want 1", n)
			}

			want := map[string]string{"remove": "session_revoked", "demote": "read_only_caregiver"}[change]
			called := false
			err = f.Service.WithFamilyTx(t.Context(), p, "mutate_record", func(pgx.Tx, access.Grant) error { called = true; return nil })
			wantCode(t, err, want)
			err = f.Service.WithRequestTx(t.Context(), authority.Request{Digests: caregiver.digests(f)}, "mutate_record",
				func(pgx.Tx, access.Principal, access.Grant) error { called = true; return nil })
			wantCode(t, err, want)
			if called {
				t.Fatal("a transaction after the revoking commit ran")
			}
			if change == "demote" {
				// Reading is still granted, at the demoting revision.
				var grant access.Grant
				if err := f.Service.WithFamilyReadTx(t.Context(), p, "read", func(_ pgx.Tx, g access.Grant) error { grant = g; return nil }); err != nil {
					t.Fatal(err)
				}
				if grant.Role != "viewer" || grant.Revision != familyRevision(t, f, family) {
					t.Fatalf("grant after demotion %+v", grant)
				}
			}
		})
	}
}

// countBabies counts a family's care baby rows as the table owner in its
// tenant context.
func countBabies(t *testing.T, f *authoritytest.Fixture, family string) int {
	t.Helper()
	var n int
	labelQuery(t, f, family, `SELECT count(*) FROM care_entities WHERE family_id=$1 AND kind=1`, []any{family}, &n)
	return n
}

func TestCrossFamilyIsolationUnderTenantContext(t *testing.T) {
	f := authoritytest.New(t)
	ownerA, ownerB := newMember(t, f), newMember(t, f)
	familyA, familyB := createFamily(t, f, f.Service, ownerA), createFamily(t, f, f.Service, ownerB)
	seatA, seatB := seated(t, f, ownerA, familyA, "owner"), seated(t, f, ownerB, familyB, "owner")
	babyB := authoritytest.NewID(t)
	if err := f.Service.WithRequestTx(t.Context(), authority.Request{Digests: seatB.digests(f)}, "add_baby", func(tx pgx.Tx, _ access.Principal, g access.Grant) error {
		return insertBaby(t.Context(), tx, g, ownerB.device, babyB)
	}); err != nil {
		t.Fatal(err)
	}

	probe := func(tx pgx.Tx, g access.Grant, family string) {
		ctx := t.Context()
		if g.FamilyID != family {
			t.Fatalf("grant family %s, want %s", g.FamilyID, family)
		}
		var account, current string
		var authorityFlag bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(app.current_account_id()::text,''), coalesce(app.current_family_id()::text,''), app.authority_context()`).
			Scan(&account, &current, &authorityFlag); err != nil {
			t.Fatal(err)
		}
		if account != g.AccountID || current != family || authorityFlag {
			t.Fatalf("tenant context account=%s family=%s authority=%v, want %s %s false", account, current, authorityFlag, g.AccountID, family)
		}
		// Authority-only rows are invisible to the tenant.
		if n := countIn(ctx, t, tx, `SELECT count(*) FROM sessions`); n != 0 {
			t.Fatalf("tenant sees %d sessions", n)
		}
	}
	for _, form := range []string{"principal", "authenticated"} {
		read := func(st seat, family string, check func(pgx.Tx)) {
			t.Helper()
			var err error
			if form == "principal" {
				err = f.Service.WithFamilyReadTx(t.Context(), st.authenticated(t, f), "read", func(tx pgx.Tx, g access.Grant) error {
					probe(tx, g, family)
					check(tx)
					return nil
				})
			} else {
				err = f.Service.WithRequestTx(t.Context(), authority.Request{Digests: st.digests(f)}, "read", func(tx pgx.Tx, _ access.Principal, g access.Grant) error {
					probe(tx, g, family)
					check(tx)
					return nil
				})
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		read(seatA, familyA, func(tx pgx.Tx) {
			if n := countIn(t.Context(), t, tx, `SELECT count(*) FROM care_entities`); n != 0 {
				t.Fatalf("%s: family A sees %d babies", form, n)
			}
			if n := countIn(t.Context(), t, tx, `SELECT count(*) FROM care_entities WHERE family_id=$1 AND id=$2`, familyB, babyB); n != 0 {
				t.Fatalf("%s: family A reads family B's baby", form)
			}
			// Nor can it write into family B.
			if form == "authenticated" {
				tag, err := tx.Exec(t.Context(), `UPDATE care_entities SET sort_order=7 WHERE family_id=$1`, familyB)
				if err != nil || tag.RowsAffected() != 0 {
					t.Fatalf("family A updated %d of family B's babies: %v", tag.RowsAffected(), err)
				}
			}
		})
		read(seatB, familyB, func(tx pgx.Tx) {
			if n := countIn(t.Context(), t, tx, `SELECT count(*) FROM care_entities WHERE kind=1 AND id=$1`, babyB); n != 1 {
				t.Fatalf("%s: family B sees %d of its babies", form, n)
			}
		})
	}
	// The tenant context is transaction-local: the next transaction on the
	// pool starts with none.
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var account, family string
	if err := tx.QueryRow(t.Context(), `SELECT coalesce(current_setting('app.account_id',true),''), coalesce(current_setting('app.family_id',true),'')`).
		Scan(&account, &family); err != nil || account != "" || family != "" {
		t.Fatalf("settings leaked: account=%q family=%q err=%v", account, family, err)
	}
}

func TestFamilyTxMapsPolicyCodes(t *testing.T) {
	f := authoritytest.New(t)
	hosted := serviceOn(t, f, f.Pool, func(o *authority.Options) { o.RequireFamilySyncEntitlement = true })
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	ownerSeat := seated(t, f, owner, family, "owner")
	caregiver := seated(t, f, joined(t, f, f.Service, owner, family, "caregiver"), family, "caregiver")
	viewer := seated(t, f, joined(t, f, f.Service, owner, family, "viewer"), family, "viewer")
	recovery := owner
	recoverySession, recoveryRaw, _ := f.Session(t, owner.account, owner.device, "family_recovery", family, "owner")
	recoverySeat := seat{member: recovery, familySession: recoverySession, access: recoveryRaw}
	_, ownerAccountRaw, _ := f.Session(t, owner.account, owner.device, "account", "", "")
	ownerAccount := seat{member: owner, access: ownerAccountRaw}
	loner := newMember(t, f)
	_, lonerRaw, _ := f.Session(t, loner.account, loner.device, "account", "", "")
	lonerSeat := seat{member: loner, access: lonerRaw}
	f.Exec(t, `INSERT INTO family_entitlements(family_id,entitlement_key,source,status,expires_at,created_at,updated_at)
		VALUES ($1,'family_sync','hosted_beta','active',$2,$2,$2)`, family, f.Now)

	cases := []struct {
		name    string
		service *authority.Service
		seat    seat
		action  string
		code    string
		role    string
	}{
		{"owner mutates", f.Service, ownerSeat, "mutate_record", "", "owner"},
		{"caregiver mutates", f.Service, caregiver, "mutate_record", "", "caregiver"},
		{"viewer reads", f.Service, viewer, "read", "", "viewer"},
		{"viewer mutates", f.Service, viewer, "mutate_record", "read_only_caregiver", ""},
		{"viewer manages", f.Service, viewer, "manage_members", "owner_required", ""},
		{"caregiver manages", f.Service, caregiver, "manage_members", "owner_required", ""},
		{"unknown action", f.Service, ownerSeat, "drop_tables", "membership_revoked", ""},
		{"recovery session", f.Service, recoverySeat, "read", "recovery_only", ""},
		{"account session with an owner slot", f.Service, ownerAccount, "manage_members", "", "owner"},
		{"account session without a slot", f.Service, lonerSeat, "read", "family_unavailable", ""},
		{"hosted expired entitlement", hosted, caregiver, "mutate_record", "entitlement_required", ""},
		{"hosted expired entitlement, exempt export", hosted, ownerSeat, "export", "", "owner"},
		{"hosted expired entitlement, exempt receipt read", hosted, viewer, "read_receipt", "", "viewer"},
	}
	for _, c := range cases {
		check := func(form string, g access.Grant, err error) {
			t.Helper()
			if authority.CodeOf(err) != c.code {
				t.Errorf("%s (%s): err = %v, want %q", c.name, form, err, c.code)
				return
			}
			if c.code == "" && (g.Role != c.role || g.FamilyID != family || g.AccountID != c.seat.account || g.Revision != familyRevision(t, f, family)) {
				t.Errorf("%s (%s): grant %+v", c.name, form, g)
			}
		}
		var grant access.Grant
		err := c.service.WithRequestTx(t.Context(), authority.Request{Digests: c.seat.digests(f)}, c.action, func(_ pgx.Tx, _ access.Principal, g access.Grant) error {
			grant = g
			return nil
		})
		check("authenticated", grant, err)
		p, err := c.service.Authenticate(t.Context(), c.seat.digests(f))
		if err != nil {
			t.Fatal(err)
		}
		grant = access.Grant{}
		err = c.service.WithFamilyTx(t.Context(), p, c.action, func(_ pgx.Tx, g access.Grant) error { grant = g; return nil })
		check("principal", grant, err)
	}

	// A principal that is not the session's as authenticated is refused.
	p := caregiver.authenticated(t, f)
	for _, bad := range []func(*access.Principal){
		func(p *access.Principal) { p.SecurityRevision++ },
		func(p *access.Principal) { p.DeviceID = authoritytest.NewID(t) },
		func(p *access.Principal) { p.SessionID = authoritytest.NewID(t) },
		func(p *access.Principal) { p.AccountID = authoritytest.NewID(t) },
	} {
		q := p
		bad(&q)
		err := f.Service.WithFamilyTx(t.Context(), q, "read", func(pgx.Tx, access.Grant) error { return nil })
		wantCode(t, err, "session_revoked")
	}
	q := p
	q.RecoveryEpoch = 2
	wantCode(t, f.Service.WithFamilyTx(t.Context(), q, "read", func(pgx.Tx, access.Grant) error { return nil }), "epoch_changed")
	// An account transaction refuses a recovery session.
	rp, err := f.Service.Authenticate(t.Context(), recoverySeat.digests(f))
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.Service.WithAccountTx(t.Context(), rp, func(pgx.Tx) error { return nil }), "session_revoked")
	// The read form is read-only.
	err = f.Service.WithFamilyReadTx(t.Context(), p, "read", func(tx pgx.Tx, g access.Grant) error {
		_, err := tx.Exec(t.Context(), `UPDATE families SET next_sequence=next_sequence WHERE id=$1`, g.FamilyID)
		return err
	})
	if sqlState(err) != "25006" {
		t.Fatalf("write in read transaction: %v", err)
	}
	// run's error is returned and the transaction rolled back.
	sentinel := errors.New("callback failed")
	if err := f.Service.WithFamilyTx(t.Context(), p, "mutate_record", func(tx pgx.Tx, g access.Grant) error {
		if err := insertBaby(t.Context(), tx, g, caregiver.device, authoritytest.NewID(t)); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("callback error %v", err)
	}
	if n := countBabies(t, f, family); n != 0 {
		t.Fatalf("rolled-back write left %d babies", n)
	}
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestFamilyTxFailsClosedWhenFamilyChainFailed(t *testing.T) {
	f := authoritytest.New(t)
	ownerA, ownerB := newMember(t, f), newMember(t, f)
	familyA, familyB := createFamily(t, f, f.Service, ownerA), createFamily(t, f, f.Service, ownerB)
	seatA, seatB := seated(t, f, ownerA, familyA, "owner"), seated(t, f, ownerB, familyB, "owner")
	pA := seatA.authenticated(t, f)
	f.Exec(t, `UPDATE families SET chain_failed=true WHERE id=$1`, familyA)

	called := false
	err := f.Service.WithFamilyTx(t.Context(), pA, "read", func(pgx.Tx, access.Grant) error { called = true; return nil })
	wantCode(t, err, "scope_failed")
	err = f.Service.WithRequestTx(t.Context(), authority.Request{Digests: seatA.digests(f)}, "read", func(pgx.Tx, access.Principal, access.Grant) error { called = true; return nil })
	wantCode(t, err, "scope_failed")
	if called {
		t.Fatal("a failed family's transaction ran")
	}
	// The account itself is fine: account transactions and authentication work.
	if _, err := f.Service.Authenticate(t.Context(), seatA.digests(f)); err != nil {
		t.Fatal(err)
	}
	if err := f.Service.WithAccountTx(t.Context(), pA, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.Service.WithRequestTx(t.Context(), authority.Request{Digests: seatB.digests(f)}, "mutate_record", func(pgx.Tx, access.Principal, access.Grant) error { return nil }); err != nil {
		t.Fatalf("another family: %v", err)
	}
	// A failed account chain fails every transaction of that account.
	f.Exec(t, `UPDATE accounts SET chain_failed=true WHERE id=$1`, ownerB.account)
	err = f.Service.WithRequestTx(t.Context(), authority.Request{Digests: seatB.digests(f)}, "read", func(pgx.Tx, access.Principal, access.Grant) error { called = true; return nil })
	wantCode(t, err, "scope_failed")
	if called {
		t.Fatal("a failed account's transaction ran")
	}
}

func TestCheckManyDeniesRemovedMemberInOneBatch(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	ownerSeat := seated(t, f, owner, family, "owner")
	caregiver := seated(t, f, joined(t, f, f.Service, owner, family, "caregiver"), family, "caregiver")
	viewer := seated(t, f, joined(t, f, f.Service, owner, family, "viewer"), family, "viewer")
	lease := func(st seat) authority.LiveLease {
		t.Helper()
		var l authority.LiveLease
		if err := f.Service.WithRequestTx(t.Context(), authority.Request{Digests: st.digests(f)}, "read", func(_ pgx.Tx, p access.Principal, g access.Grant) error {
			l = authority.LiveLease{Principal: p, Grant: g}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return l
	}
	leases := []authority.LiveLease{lease(ownerSeat), lease(caregiver), lease(viewer)}
	forged := leases[2]
	forged.Grant.Role = "caregiver"
	expired := leases[0]
	expired.Principal.AccessExpiresAt = f.Now
	leases = append(leases, forged, expired)

	s, w := countedService(t, f)
	if _, err := s.CheckMany(t.Context(), leases); err != nil {
		t.Fatal(err)
	}
	check := func(want []bool) {
		t.Helper()
		w.reset()
		got, err := s.CheckMany(t.Context(), leases)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("CheckMany = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("CheckMany = %v, want %v", got, want)
			}
		}
		trips, statements, _ := w.snapshot()
		if n := statementsMatching(statements, "FROM unnest"); n != 1 || trips != 2 {
			t.Fatalf("CheckMany: %d lease statements, %d round trips (%q); want one statement, pipelined begin+flag+query then rollback", n, trips, statements)
		}
	}
	check([]bool{true, true, true, false, false})
	if _, err := f.Service.RemoveMember(t.Context(), authority.RemoveMember{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account,
		AccountID: caregiver.account, At: f.Now, Principal: owner.principal(t, f)}); err != nil {
		t.Fatal(err)
	}
	// The removal advanced the family revision; the owner's and viewer's
	// leases keep their admission revision, which the family has reached.
	check([]bool{true, false, true, false, false})
	f.Exec(t, `UPDATE families SET chain_failed=true WHERE id=$1`, family)
	check([]bool{false, false, false, false, false})
	if got, err := s.CheckMany(t.Context(), nil); err != nil || len(got) != 0 {
		t.Fatalf("empty CheckMany %v %v", got, err)
	}
}

func TestCheckExportSubject(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver := joined(t, f, f.Service, owner, family, "caregiver")
	counted, w := countedService(t, f)
	for round := 0; round < 2; round++ {
		w.reset()
		if err := counted.CheckExportSubject(t.Context(), family, owner.account, authoritytest.RecoveryEpoch); err != nil {
			t.Fatal(err)
		}
		// Round 0 also prepares the statements.
		if trips, _, batches := w.snapshot(); round == 1 && (trips != 2 || batches != 1) {
			t.Fatalf("CheckExportSubject: %d round trips, %d batches; want 2 (pipelined read, rollback)", trips, batches)
		}
	}
	wantCode(t, f.Service.CheckExportSubject(t.Context(), family, caregiver.account, authoritytest.RecoveryEpoch), "export_authorization_changed")
	wantCode(t, f.Service.CheckExportSubject(t.Context(), family, owner.account, authoritytest.RecoveryEpoch+1), "export_authorization_changed")
	wantCode(t, f.Service.CheckExportSubject(t.Context(), authoritytest.NewID(t), owner.account, authoritytest.RecoveryEpoch), "export_authorization_changed")
	if _, err := f.Service.TransferOwner(t.Context(), authority.TransferOwner{CommandID: authoritytest.NewID(t), OldOwnerID: owner.account,
		NewOwnerID: caregiver.account, At: f.Now, Principal: owner.principal(t, f)}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.Service.CheckExportSubject(t.Context(), family, owner.account, authoritytest.RecoveryEpoch), "export_authorization_changed")
	if err := f.Service.CheckExportSubject(t.Context(), family, caregiver.account, authoritytest.RecoveryEpoch); err != nil {
		t.Fatalf("new owner: %v", err)
	}
	f.Exec(t, `UPDATE families SET chain_failed=true WHERE id=$1`, family)
	wantCode(t, f.Service.CheckExportSubject(t.Context(), family, caregiver.account, authoritytest.RecoveryEpoch), "scope_failed")
}

// recoverySeat signs the owner of a deletion-pending family in again and
// refreshes, so the session is family_recovery scoped, as iOS holds it.
func recoverySeat(t *testing.T, f *authoritytest.Fixture, owner member, family string) (seat, access.Principal) {
	t.Helper()
	device := f.Device(t, owner.account)
	_, _, raw := f.Session(t, owner.account, device, "account", "", "")
	res, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	st := seat{member: member{account: owner.account, device: device, session: res.Child.ID}, familySession: res.Child.ID, access: res.AccessToken}
	p := st.authenticated(t, f)
	if p.Scope != "family_recovery" || p.SessionFamilyID != family {
		t.Fatalf("recovery principal %+v", p)
	}
	return st, p
}

// Legacy derived a session's scope from the live slot (auth/refresh.go:80-86):
// after RecoverFamily the same session acts for the active family as its
// owner; after ForfeitRecovery it is an account session, not revoked.
func TestRecoveryCommandsMoveRecoverySessionScope(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	recovering, p := recoverySeat(t, f, owner, family)
	// While the slot is family_recovery, a recovery session and an
	// account-scope session of the owner are both recovery_only.
	_, accountRaw, _ := f.Session(t, owner.account, owner.device, "account", "", "")
	for _, digests := range [][]authority.KeyedDigest{recovering.digests(f), accessDigests(f.TokenKeys, accountRaw)} {
		err := f.Service.WithRequestTx(t.Context(), authority.Request{Digests: digests}, "read", func(pgx.Tx, access.Principal, access.Grant) error {
			t.Fatal("recovery-only account reached a family transaction")
			return nil
		})
		wantCode(t, err, "recovery_only")
	}

	if _, err := f.Service.RecoverFamily(t.Context(), authority.RecoverFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account, At: f.Now,
		Principal: p}); err != nil {
		t.Fatal(err)
	}
	if row, _ := sessionRow(t, f, recovering.familySession); row.Status != "active" || row.Scope != "family" || row.FamilyID != family || row.Role != "owner" {
		t.Fatalf("recovered session %+v", row)
	}
	revision := familyRevision(t, f, family)
	var grant access.Grant
	// The principal authenticated before the recovery still works …
	if err := f.Service.WithFamilyTx(t.Context(), p, "mutate_record", func(_ pgx.Tx, g access.Grant) error { grant = g; return nil }); err != nil {
		t.Fatalf("pre-recovery principal: %v", err)
	}
	if grant.Role != "owner" || grant.FamilyID != family || grant.Revision != revision {
		t.Fatalf("grant %+v", grant)
	}
	// … and so does the same access token, now family scoped.
	var principal access.Principal
	if err := f.Service.WithRequestTx(t.Context(), authority.Request{Digests: recovering.digests(f)}, "mutate_record", func(_ pgx.Tx, q access.Principal, g access.Grant) error {
		principal, grant = q, g
		return nil
	}); err != nil {
		t.Fatalf("same token after recovery: %v", err)
	}
	if principal.Scope != "family" || principal.SessionFamilyID != family || grant.Role != "owner" || grant.FamilyID != family {
		t.Fatalf("after recovery %+v %+v", principal, grant)
	}

	// Forfeit: a second family, deleted and forfeited.
	other := newMember(t, f)
	otherFamily := createFamily(t, f, f.Service, other)
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, other.account, other.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	forfeiting, fp := recoverySeat(t, f, other, otherFamily)
	if _, err := f.Service.ForfeitRecovery(t.Context(), authority.ForfeitRecovery{CommandID: authoritytest.NewID(t), AccountID: other.account, At: f.Now,
		Principal: fp}); err != nil {
		t.Fatal(err)
	}
	if row, _ := sessionRow(t, f, forfeiting.familySession); row.Status != "active" || row.Scope != "account" || row.FamilyID != "" || row.Role != "" {
		t.Fatalf("forfeited session %+v", row)
	}
	if q := forfeiting.authenticated(t, f); q.Scope != "account" || q.SessionFamilyID != "" {
		t.Fatalf("principal after forfeit %+v", q)
	}
	if err := f.Service.WithAccountTx(t.Context(), fp, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("account transaction after forfeit: %v", err)
	}
	wantCode(t, f.Service.WithRequestTx(t.Context(), authority.Request{Digests: forfeiting.digests(f)}, "read", func(pgx.Tx, access.Principal, access.Grant) error {
		t.Fatal("forfeited account reached a family transaction")
		return nil
	}), "family_unavailable")
}

// families.entitlement_required alone (node flag off) requires the
// family_sync entitlement for non-exempt actions.
func TestFamilyEntitlementRequiredWithoutNodeFlag(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	ownerSeat := seated(t, f, owner, family, "owner")
	caregiver := seated(t, f, joined(t, f, f.Service, owner, family, "caregiver"), family, "caregiver")
	f.Exec(t, `UPDATE families SET entitlement_required=true WHERE id=$1`, family)
	run := func(st seat, action string) error {
		return f.Service.WithRequestTx(t.Context(), authority.Request{Digests: st.digests(f)}, action, func(pgx.Tx, access.Principal, access.Grant) error { return nil })
	}
	wantCode(t, run(caregiver, "mutate_record"), "entitlement_required")
	wantCode(t, run(caregiver, "read"), "entitlement_required")
	if err := run(ownerSeat, "export"); err != nil {
		t.Fatalf("exempt export: %v", err)
	}
	f.Exec(t, `INSERT INTO family_entitlements(family_id,entitlement_key,source,status,expires_at,created_at,updated_at)
		VALUES ($1,'family_sync','hosted_beta','active',$2,$3,$3)`, family, f.Now.Add(time.Hour), f.Now)
	if err := run(caregiver, "mutate_record"); err != nil {
		t.Fatalf("entitled: %v", err)
	}
	f.Exec(t, `UPDATE family_entitlements SET status='suspended' WHERE family_id=$1`, family)
	wantCode(t, run(caregiver, "mutate_record"), "entitlement_required")
}

// A panic or a cancelled context inside run leaves the one pooled
// connection clean (rolled back and idle) or destroyed: the next
// transaction works and no setting leaks onto the connection.
func TestRequestTxPanicAndCancelLeaveConnectionClean(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	st := seated(t, f, owner, family, "owner")
	pool, _ := countedPool(t, f, nil)
	s := serviceOn(t, f, pool, nil)
	p := st.authenticated(t, f)

	clean := func(when string) {
		t.Helper()
		var account, current, flag string
		if err := pool.QueryRow(t.Context(), `SELECT coalesce(current_setting('app.account_id',true),''), coalesce(current_setting('app.family_id',true),''),
				coalesce(current_setting('app.authority',true),'')`).Scan(&account, &current, &flag); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if account != "" || current != "" || flag != "" {
			t.Fatalf("%s: settings leaked account=%q family=%q authority=%q", when, account, current, flag)
		}
		babyID := authoritytest.NewID(t)
		if err := s.WithFamilyTx(t.Context(), p, "mutate_record", func(tx pgx.Tx, g access.Grant) error {
			var got string
			if err := tx.QueryRow(t.Context(), `SELECT app.current_family_id()::text`).Scan(&got); err != nil || got != family {
				t.Fatalf("%s: family context %q %v", when, got, err)
			}
			return insertBaby(t.Context(), tx, g, owner.device, babyID)
		}); err != nil {
			t.Fatalf("%s: next transaction: %v", when, err)
		}
		if total := pool.Stat().TotalConns(); total > 1 {
			t.Fatalf("%s: %d connections", when, total)
		}
	}

	// A panic after a write inside run.
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recovered %v, want the callback's panic", r)
			}
		}()
		_ = s.WithRequestTx(t.Context(), authority.Request{Digests: st.digests(f)}, "mutate_record", func(tx pgx.Tx, _ access.Principal, g access.Grant) error {
			if err := insertBaby(t.Context(), tx, g, owner.device, authoritytest.NewID(t)); err != nil {
				return err
			}
			panic("boom")
		})
	}()
	clean("after panic")

	// The caller's context is cancelled while run waits on the database.
	ctx, cancel := context.WithCancel(t.Context())
	err := s.WithFamilyTx(ctx, p, "mutate_record", func(tx pgx.Tx, g access.Grant) error {
		if err := insertBaby(ctx, tx, g, owner.device, authoritytest.NewID(t)); err != nil {
			return err
		}
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := tx.Exec(ctx, `SELECT pg_sleep(5)`)
		return err
	})
	cancel()
	if err == nil {
		t.Fatal("cancelled transaction succeeded")
	}
	clean("after cancel")
	// Only the two clean() writes committed.
	if n := countBabies(t, f, family); n != 2 {
		t.Fatalf("%d babies committed, want 2", n)
	}

	// After commit the transaction refuses use, loudly where pgx cannot
	// return an error.
	var done pgx.Tx
	if err := s.WithFamilyReadTx(t.Context(), p, "read", func(tx pgx.Tx, _ access.Grant) error { done = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := done.Exec(t.Context(), `SELECT 1`); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("Exec after commit: %v", err)
	}
	if _, err := done.Begin(t.Context()); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("Begin after commit: %v", err)
	}
	for name, use := range map[string]func(){"Conn": func() { done.Conn() }, "LargeObjects": func() { done.LargeObjects() }} {
		func() {
			defer func() {
				r := recover()
				if err, ok := r.(error); !ok || err.Error() == "" {
					t.Fatalf("%s after commit: recovered %v, want a clear error", name, r)
				}
			}()
			use()
		}()
	}
	if err := s.WithFamilyReadTx(t.Context(), p, "read", func(tx pgx.Tx, _ access.Grant) error {
		_, err := tx.Begin(t.Context())
		if err == nil || errors.Is(err, pgx.ErrTxClosed) {
			t.Fatalf("nested Begin: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A Service built without EntitlementExempt applies the legacy exemptions:
// an entitlement-required family still exports and reads receipts.
func TestNilEntitlementExemptDefaultsToLegacyList(t *testing.T) {
	f := authoritytest.New(t)
	service := serviceOn(t, f, f.Pool, func(o *authority.Options) { o.EntitlementExempt = nil })
	owner := newMember(t, f)
	family := createFamily(t, f, service, owner)
	ownerSeat := seated(t, f, owner, family, "owner")
	f.Exec(t, `UPDATE families SET entitlement_required=true WHERE id=$1`, family)
	run := func(action string) error {
		return service.WithRequestTx(t.Context(), authority.Request{Digests: ownerSeat.digests(f)}, action, func(pgx.Tx, access.Principal, access.Grant) error { return nil })
	}
	for _, action := range []string{"export", "read_receipt"} {
		if err := run(action); err != nil {
			t.Fatalf("exempt %s: %v", action, err)
		}
	}
	wantCode(t, run("read"), "entitlement_required")
}
