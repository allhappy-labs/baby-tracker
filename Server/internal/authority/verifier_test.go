package authority_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
)

// logSink collects a verifier's JSON log lines.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// mismatches returns the scope ID → seq of every "audit chain mismatch" alert.
func (s *logSink) mismatches(t *testing.T) map[string]uint64 {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]uint64{}
	scanner := bufio.NewScanner(bytes.NewReader(s.buf.Bytes()))
	for scanner.Scan() {
		var line struct {
			Level, Msg, Scope, ID string
			Seq                   uint64
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("log line %q: %v", scanner.Text(), err)
		}
		if line.Msg == "audit chain mismatch" {
			if line.Level != "ERROR" || line.Scope == "" || line.ID == "" {
				t.Fatalf("alert %q is not an ERROR naming the scope", scanner.Text())
			}
			out[line.ID] = line.Seq
		}
	}
	return out
}

func newChainVerifier(f *authoritytest.Fixture) (*authority.Verifier, *logSink) {
	sink := &logSink{}
	logger := slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// Upper-case keys decode into the struct fields above.
			switch a.Key {
			case slog.LevelKey:
				a.Key = "Level"
			case slog.MessageKey:
				a.Key = "Msg"
			case "scope":
				a.Key = "Scope"
			case "id":
				a.Key = "ID"
			case "seq":
				a.Key = "Seq"
			}
			return a
		}}))
	return authority.NewVerifier(f.Service, logger), sink
}

// appendRows appends n rows to a scope's chain through the production append
// path, with payloads jsonb re-renders differently from Go.
func appendRows(t *testing.T, f *authoritytest.Fixture, scope authority.Scope, id string, n int) {
	t.Helper()
	if err := tryAppendRows(t.Context(), t, f, scope, id, n); err != nil {
		t.Fatal(err)
	}
}

// tryAppendRows is appendRows for goroutines other than the test's.
func tryAppendRows(ctx context.Context, t *testing.T, f *authoritytest.Fixture, scope authority.Scope, id string, n int) error {
	tx, err := f.Service.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var head *authority.LockedChainHead
	kind := authority.KindSessionGranted
	if scope == authority.ScopeFamily {
		rows, err := authority.LockFamilies(ctx, tx, id)
		if err != nil {
			return err
		}
		head, kind = rows[id].Chain, authority.KindFamilyProfileRepaired
	} else {
		rows, err := authority.LockAccounts(ctx, tx, id)
		if err != nil {
			return err
		}
		head = rows[id].Chain
	}
	for i := range n {
		payload := map[string]any{"i": head.Seq, "note": "<é>&\u2028 😀 \x01", "big": uint64(math.MaxUint64), "list": []any{"b", "a", nil, true}}
		at := f.Now.Add(time.Duration(head.Seq)*time.Second + time.Duration(i)*123456789)
		if _, _, err := authority.AppendAudit(ctx, tx, head, kind, authoritytest.NewID(t), authoritytest.NewID(t), at, payload); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func scopeTable(scope authority.Scope) string {
	if scope == authority.ScopeFamily {
		return "families"
	}
	return "accounts"
}

func chainFailed(t *testing.T, f *authoritytest.Fixture, scope authority.Scope, id string) bool {
	t.Helper()
	var failed bool
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		if err := tx.QueryRow(t.Context(), `SELECT chain_failed FROM `+scopeTable(scope)+` WHERE id=$1`, id).Scan(&failed); err != nil {
			t.Fatal(err)
		}
	})
	return failed
}

type checkpointRow struct {
	Seq  uint64
	Hash [32]byte
	At   time.Time
}

func checkpointOf(t *testing.T, f *authoritytest.Fixture, scope authority.Scope, id string) (checkpointRow, bool) {
	t.Helper()
	var cp checkpointRow
	found := true
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		var hash []byte
		err := tx.QueryRow(t.Context(), `SELECT verified_seq, verified_hash, verified_at FROM audit_checkpoints WHERE scope=$1::text::app.audit_scope AND scope_id=$2`,
			string(scope), id).Scan(&cp.Seq, &hash, &cp.At)
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		copy(cp.Hash[:], hash)
	})
	return cp, found
}

func failedIDs(report authority.VerifyReport) []string {
	out := make([]string, 0, len(report.Failed))
	for _, ref := range report.Failed {
		out = append(out, string(ref.Scope)+":"+ref.ID)
	}
	slices.Sort(out)
	return out
}

func refs(pairs ...string) []string {
	slices.Sort(pairs)
	return pairs
}

func verifyIncremental(t *testing.T, v *authority.Verifier, maxScopes int) authority.VerifyReport {
	t.Helper()
	report, err := v.VerifyIncremental(t.Context(), maxScopes)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func verifyAll(t *testing.T, v *authority.Verifier) authority.VerifyReport {
	t.Helper()
	report, err := v.VerifyAll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestVerifierAcceptsCleanChainsAndAdvancesCheckpoints(t *testing.T) {
	authority.SetVerifyPageRows(t, 2) // walks cross keyset page boundaries
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	// Real command chains: family_created, invitation_created and
	// invitation_redeemed on the family; the owner and caregiver accounts.
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver := joined(t, f, f.Service, owner, family, "caregiver")
	extra := f.Account(t)
	appendRows(t, f, authority.ScopeAccount, extra, 5)
	_ = f.Account(t) // genesis-only: nothing to verify

	type scopeHead struct {
		scope authority.Scope
		id    string
	}
	heads := []scopeHead{{authority.ScopeFamily, family}, {authority.ScopeAccount, owner.account}, {authority.ScopeAccount, caregiver.account}, {authority.ScopeAccount, extra}}
	wantRows := 0
	for _, h := range heads {
		seq, _ := chainHead(t, f, h.scope, h.id)
		if seq == 0 {
			t.Fatalf("%s %s has no audit rows", h.scope, h.id)
		}
		wantRows += int(seq)
	}

	report := verifyIncremental(t, v, 100)
	if report.Scopes != len(heads) || report.Rows != wantRows || len(report.Failed) != 0 {
		t.Fatalf("first pass = %+v, want %d scopes, %d rows, none failed", report, len(heads), wantRows)
	}
	for _, h := range heads {
		seq, hash := chainHead(t, f, h.scope, h.id)
		cp, ok := checkpointOf(t, f, h.scope, h.id)
		if !ok || cp.Seq != seq || cp.Hash != hash || !cp.At.Equal(f.Now) {
			t.Fatalf("%s %s checkpoint %+v (found %v), want head (%d,%x) at %v", h.scope, h.id, cp, ok, seq, hash, f.Now)
		}
		if chainFailed(t, f, h.scope, h.id) {
			t.Fatalf("%s %s marked failed", h.scope, h.id)
		}
	}

	// Nothing moved: nothing to walk.
	if report := verifyIncremental(t, v, 100); report.Scopes != 0 || report.Rows != 0 || len(report.Failed) != 0 {
		t.Fatalf("idle pass = %+v, want nothing", report)
	}

	// Two new rows: the walk re-checks the checkpointed row and the two after it.
	f.Now = f.Now.Add(time.Minute)
	appendRows(t, f, authority.ScopeAccount, extra, 2)
	if report := verifyIncremental(t, v, 100); report.Scopes != 1 || report.Rows != 3 || len(report.Failed) != 0 {
		t.Fatalf("incremental pass = %+v, want 1 scope, 3 rows", report)
	}
	if cp, _ := checkpointOf(t, f, authority.ScopeAccount, extra); cp.Seq != 7 || !cp.At.Equal(f.Now) {
		t.Fatalf("checkpoint %+v, want seq 7 at %v", cp, f.Now)
	}
	if got := sink.mismatches(t); len(got) != 0 {
		t.Fatalf("clean chains raised alerts: %v", got)
	}
	if _, err := v.VerifyIncremental(t.Context(), 0); err == nil {
		t.Fatal("maxScopes 0 accepted")
	}
}

func TestVerifyIncrementalIsBoundedOldestCheckpointFirst(t *testing.T) {
	f := authoritytest.New(t)
	v, _ := newChainVerifier(f)
	a := f.Account(t)
	appendRows(t, f, authority.ScopeAccount, a, 1)
	verifyIncremental(t, v, 10) // a checkpointed at T0
	f.Now = f.Now.Add(time.Minute)
	b := f.Account(t)
	appendRows(t, f, authority.ScopeAccount, b, 1)
	verifyIncremental(t, v, 10) // b checkpointed at T1
	f.Now = f.Now.Add(time.Minute)
	appendRows(t, f, authority.ScopeAccount, a, 1)
	appendRows(t, f, authority.ScopeAccount, b, 1)
	c := f.Account(t) // never checkpointed: first
	appendRows(t, f, authority.ScopeAccount, c, 1)

	for _, want := range []string{c, a, b} {
		before := map[string]uint64{}
		for _, id := range []string{a, b, c} {
			cp, _ := checkpointOf(t, f, authority.ScopeAccount, id)
			before[id] = cp.Seq
		}
		if report := verifyIncremental(t, v, 1); report.Scopes != 1 {
			t.Fatalf("bounded pass verified %d scopes, want 1", report.Scopes)
		}
		for _, id := range []string{a, b, c} {
			cp, _ := checkpointOf(t, f, authority.ScopeAccount, id)
			if moved := cp.Seq != before[id]; moved != (id == want) {
				t.Fatalf("checkpoint of %s moved=%v; want only %s (order: never verified, then oldest checkpoint)", id, moved, want)
			}
		}
	}
	if report := verifyIncremental(t, v, 1); report.Scopes != 0 {
		t.Fatalf("drained pass = %+v", report)
	}
}

// A tampered audit row fails closed for its scope only: that family's
// transactions return scope_failed, its members still authenticate, the
// other family keeps working, and the full verify names the scope.
func TestVerifierFailsClosedPerScope(t *testing.T) {
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	ownerA, ownerB := newMember(t, f), newMember(t, f)
	familyA, familyB := createFamily(t, f, f.Service, ownerA), createFamily(t, f, f.Service, ownerB)
	caregiverA := joined(t, f, f.Service, ownerA, familyA, "caregiver")
	_ = joined(t, f, f.Service, ownerB, familyB, "caregiver")
	if seq, _ := chainHead(t, f, authority.ScopeFamily, familyA); seq < 3 {
		t.Fatalf("family A has %d audit rows, want at least 3", seq)
	}
	seatA, seatB := seated(t, f, ownerA, familyA, "owner"), seated(t, f, ownerB, familyB, "owner")
	caregiverSeat := seated(t, f, caregiverA, familyA, "caregiver")
	pA := seatA.authenticated(t, f)

	if n := f.Exec(t, `UPDATE security_audit_events SET payload = payload || '{"tampered":true}' WHERE scope='family' AND scope_id=$1 AND seq=3`, familyA); n != 1 {
		t.Fatalf("tampered %d rows", n)
	}
	report := verifyIncremental(t, v, 100)
	if got, want := failedIDs(report), refs("family:"+familyA); !slices.Equal(got, want) {
		t.Fatalf("failed = %v, want %v", got, want)
	}
	if got := sink.mismatches(t); len(got) != 1 || got[familyA] != 3 {
		t.Fatalf("alerts = %v, want family %s at seq 3", got, familyA)
	}
	if !chainFailed(t, f, authority.ScopeFamily, familyA) {
		t.Fatal("tampered family not marked failed")
	}
	for _, id := range []string{ownerA.account, ownerB.account, caregiverA.account} {
		if chainFailed(t, f, authority.ScopeAccount, id) {
			t.Fatalf("account %s marked failed", id)
		}
	}
	if chainFailed(t, f, authority.ScopeFamily, familyB) {
		t.Fatal("other family marked failed")
	}
	if _, ok := checkpointOf(t, f, authority.ScopeFamily, familyA); ok {
		t.Fatal("a failed chain was checkpointed")
	}

	// Account level still works for the failed family's members.
	if _, err := f.Service.Authenticate(t.Context(), seatA.digests(f)); err != nil {
		t.Fatalf("owner authentication: %v", err)
	}
	if _, err := f.Service.Authenticate(t.Context(), caregiverSeat.digests(f)); err != nil {
		t.Fatalf("caregiver authentication: %v", err)
	}
	if err := f.Service.WithAccountTx(t.Context(), pA, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("owner account transaction: %v", err)
	}
	// Family level fails closed.
	called := false
	wantCode(t, f.Service.WithFamilyTx(t.Context(), pA, "read", func(pgx.Tx, access.Grant) error { called = true; return nil }), "scope_failed")
	wantCode(t, f.Service.WithRequestTx(t.Context(), authority.Request{Digests: caregiverSeat.digests(f)}, "mutate_record",
		func(pgx.Tx, access.Principal, access.Grant) error { called = true; return nil }), "scope_failed")
	if called {
		t.Fatal("a failed family's transaction ran")
	}
	if _, err := f.Service.CreateInvitation(t.Context(), invitationOf(t, f, ownerA, familyA, "caregiver")); authority.CodeOf(err) != "scope_failed" {
		t.Fatalf("command on a failed family: %v", err)
	}
	// The other family is unaffected.
	if err := f.Service.WithRequestTx(t.Context(), authority.Request{Digests: seatB.digests(f)}, "mutate_record",
		func(pgx.Tx, access.Principal, access.Grant) error { return nil }); err != nil {
		t.Fatalf("other family: %v", err)
	}
	invite(t, f, f.Service, ownerB, familyB, "viewer")

	// The admin full verify names the scope.
	if got, want := failedIDs(verifyAll(t, v)), refs("family:"+familyA); !slices.Equal(got, want) {
		t.Fatalf("full verify failed = %v, want %v", got, want)
	}
}

// The verifier never clears chain_failed: repairing the row, rerunning both
// passes, and a clean chain an operator marked failed all stay failed.
func TestVerifierNeverClearsChainFailed(t *testing.T) {
	f := authoritytest.New(t)
	v, _ := newChainVerifier(f)
	tampered, marked := f.Account(t), f.Account(t)
	appendRows(t, f, authority.ScopeAccount, tampered, 3)
	appendRows(t, f, authority.ScopeAccount, marked, 3)
	original := readAudit(t, f, authority.ScopeAccount, tampered)[1].Payload
	f.Exec(t, `UPDATE security_audit_events SET payload='{"x":1}' WHERE scope='account' AND scope_id=$1 AND seq=2`, tampered)
	f.Exec(t, `UPDATE accounts SET chain_failed=true WHERE id=$1`, marked)
	if got, want := failedIDs(verifyIncremental(t, v, 100)), refs("account:"+tampered); !slices.Equal(got, want) {
		t.Fatalf("failed = %v, want %v", got, want)
	}
	f.Exec(t, `UPDATE security_audit_events SET payload=$2::jsonb WHERE scope='account' AND scope_id=$1 AND seq=2`, tampered, string(original))

	if report := verifyIncremental(t, v, 100); report.Scopes != 0 || len(report.Failed) != 0 {
		t.Fatalf("incremental pass revisited failed scopes: %+v", report)
	}
	if got, want := failedIDs(verifyAll(t, v)), refs("account:"+tampered, "account:"+marked); !slices.Equal(got, want) {
		t.Fatalf("full verify failed = %v, want %v", got, want)
	}
	for _, id := range []string{tampered, marked} {
		if !chainFailed(t, f, authority.ScopeAccount, id) {
			t.Fatalf("account %s chain_failed was cleared", id)
		}
		if _, ok := checkpointOf(t, f, authority.ScopeAccount, id); ok {
			t.Fatalf("failed account %s was checkpointed", id)
		}
	}
}

func TestVerifyAllIgnoresCheckpoints(t *testing.T) {
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	account, clean := f.Account(t), f.Account(t)
	appendRows(t, f, authority.ScopeAccount, account, 4)
	appendRows(t, f, authority.ScopeAccount, clean, 2)
	if report := verifyIncremental(t, v, 100); len(report.Failed) != 0 || report.Rows != 6 {
		t.Fatalf("clean pass = %+v", report)
	}
	// Behind the checkpoint: the incremental pass does not walk it.
	f.Exec(t, `UPDATE security_audit_events SET payload='{"x":2}' WHERE scope='account' AND scope_id=$1 AND seq=2`, account)
	appendRows(t, f, authority.ScopeAccount, account, 1)
	if report := verifyIncremental(t, v, 100); len(report.Failed) != 0 || report.Scopes != 1 || report.Rows != 2 {
		t.Fatalf("incremental pass = %+v, want the checkpointed row and the new one, clean", report)
	}
	report := verifyAll(t, v)
	if got, want := failedIDs(report), refs("account:"+account); !slices.Equal(got, want) {
		t.Fatalf("full verify failed = %v, want %v", got, want)
	}
	// Every account (the fixture seeds none other) is a scope, genesis-only ones included.
	// Rows counts verified rows: the clean chain's two and the row before the tampered one.
	if report.Scopes != 2 || report.Rows != 3 {
		t.Fatalf("full verify = %+v, want 2 scopes and 3 verified rows", report)
	}
	if got := sink.mismatches(t); got[account] != 2 {
		t.Fatalf("alerts = %v, want seq 2 for %s", got, account)
	}
	if !chainFailed(t, f, authority.ScopeAccount, account) || chainFailed(t, f, authority.ScopeAccount, clean) {
		t.Fatal("full verify did not fail exactly the tampered scope")
	}
}

// 000031 splits audit history into monthly partitions from 2027. A chain whose
// rows straddle a partition boundary walks (in two-row keyset pages) and
// verifies as one chain, incrementally and in full, and none of its rows
// reaches the default partition. The boundary is the first one 000031 created
// after the test's own clock: 2027-01-01, or the start of next month.
func TestVerifierWalksAChainAcrossMonthlyAuditPartitions(t *testing.T) {
	authority.SetVerifyPageRows(t, 2)
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	var boundary time.Time
	if err := f.DB.Owner.QueryRow(t.Context(), `SELECT greatest(timestamptz '2027-01-01 00:00:00+00',
		(date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC')`).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	account := f.Account(t)
	f.Now = boundary.Add(-10 * time.Second)
	appendRows(t, f, authority.ScopeAccount, account, 3)
	f.Now = boundary.Add(time.Minute)
	appendRows(t, f, authority.ScopeAccount, account, 3)
	if report := verifyIncremental(t, v, 100); report.Scopes != 1 || report.Rows != 6 || len(report.Failed) != 0 {
		t.Fatalf("incremental pass = %+v, want one clean six-row chain", report)
	}
	f.Now = f.Now.Add(time.Minute)
	appendRows(t, f, authority.ScopeAccount, account, 1)
	if report := verifyIncremental(t, v, 100); report.Scopes != 1 || report.Rows != 2 || len(report.Failed) != 0 {
		t.Fatalf("second incremental pass = %+v, want the checkpointed row and the new one", report)
	}
	if report := verifyAll(t, v); report.Rows != 7 || len(report.Failed) != 0 {
		t.Fatalf("full verify = %+v, want seven clean rows", report)
	}
	if got := sink.mismatches(t); len(got) != 0 {
		t.Fatalf("a chain across partitions raised alerts: %v", got)
	}
	var partitions []string
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		rows, err := tx.Query(t.Context(), `SELECT DISTINCT tableoid::regclass::text FROM security_audit_events WHERE scope_id=$1 ORDER BY 1`, account)
		if err != nil {
			t.Fatal(err)
		}
		if partitions, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			t.Fatal(err)
		}
	})
	if len(partitions) != 2 || slices.Contains(partitions, "security_audit_events_default") {
		t.Fatalf("the chain's rows are in %v, want two named partitions", partitions)
	}
}

// Every way to break a chain that the schema does not prevent is a mismatch
// at the first broken seq; the untouched control chain stays clean.
func TestVerifierDetectsEveryMismatchClass(t *testing.T) {
	authority.SetVerifyPageRows(t, 2) // walks cross keyset page boundaries
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	const rowCols = `scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload`
	cases := []struct {
		name    string
		tamper  string // $1 is the account ID
		wantSeq uint64
	}{
		{"payload", `UPDATE security_audit_events SET payload=jsonb_set(payload,'{i}','7') WHERE scope='account' AND scope_id=$1 AND seq=2`, 2},
		{"non-canonical payload", `UPDATE security_audit_events SET payload='{"i":1.5}' WHERE scope='account' AND scope_id=$1 AND seq=2`, 2},
		{"at by one microsecond", `UPDATE security_audit_events SET at=at+interval '1 microsecond' WHERE scope='account' AND scope_id=$1 AND seq=2`, 2},
		{"at into another partition", `UPDATE security_audit_events SET at=at+interval '1 year' WHERE scope='account' AND scope_id=$1 AND seq=2`, 2},
		{"kind", `UPDATE security_audit_events SET kind='session_revoked' WHERE scope='account' AND scope_id=$1 AND seq=2`, 2},
		{"command id", `UPDATE security_audit_events SET command_id=gen_random_uuid() WHERE scope='account' AND scope_id=$1 AND seq=2`, 2},
		{"actor", `UPDATE security_audit_events SET actor_account_id=gen_random_uuid() WHERE scope='account' AND scope_id=$1 AND seq=2`, 2},
		{"prev hash", `UPDATE security_audit_events SET prev_hash=sha256(prev_hash) WHERE scope='account' AND scope_id=$1 AND seq=3`, 3},
		{"row hash", `UPDATE security_audit_events SET hash=sha256(hash) WHERE scope='account' AND scope_id=$1 AND seq=3`, 3},
		{"duplicate seq after the original", `INSERT INTO security_audit_events(` + rowCols + `) SELECT ` + strings.Replace(rowCols, "at,", "at+interval '1 microsecond',", 1) +
			` FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=2`, 2},
		{"duplicate seq before the original", `INSERT INTO security_audit_events(` + rowCols + `) SELECT ` + strings.Replace(rowCols, "at,", "at-interval '1 microsecond',", 1) +
			` FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=2`, 2},
		{"gap", `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=2`, 2},
		{"first row deleted", `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=1`, 1},
		{"deleted tail", `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=4`, 4},
		{"head hash", `UPDATE accounts SET audit_hash=sha256(audit_hash) WHERE id=$1`, 4},
		{"truncated head, rows after it", `UPDATE accounts a SET audit_seq=3, audit_hash=e.hash FROM security_audit_events e
			WHERE a.id=$1 AND e.scope='account' AND e.scope_id=a.id AND e.seq=3`, 4},
		{"row beyond head", `INSERT INTO security_audit_events(` + rowCols + `) SELECT scope,scope_id,5,hash,hash,kind,command_id,actor_account_id,at,payload
			FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=4`, 5},
	}
	control := f.Account(t)
	appendRows(t, f, authority.ScopeAccount, control, 4)
	ids := make([]string, len(cases))
	for i, c := range cases {
		ids[i] = f.Account(t)
		appendRows(t, f, authority.ScopeAccount, ids[i], 4)
		if n := f.Exec(t, c.tamper, ids[i]); n != 1 {
			t.Fatalf("%s: tamper affected %d rows", c.name, n)
		}
	}
	report := verifyIncremental(t, v, 100)
	alerts := sink.mismatches(t)
	want := []string{}
	for i, c := range cases {
		want = append(want, "account:"+ids[i])
		if seq, ok := alerts[ids[i]]; !ok || seq != c.wantSeq {
			t.Errorf("%s: alert seq %d (raised %v), want %d", c.name, seq, ok, c.wantSeq)
		}
		if !chainFailed(t, f, authority.ScopeAccount, ids[i]) {
			t.Errorf("%s: not marked failed", c.name)
		}
	}
	if got := failedIDs(report); !slices.Equal(got, refs(want...)) {
		t.Errorf("failed = %v, want %v", got, refs(want...))
	}
	if chainFailed(t, f, authority.ScopeAccount, control) || alerts[control] != 0 {
		t.Fatal("control chain failed")
	}
}

// Checkpoint-anchored walks: the checkpointed row is re-verified, and a head
// moved behind the checkpoint is a mismatch even when no row after it exists.
func TestVerifyIncrementalChecksTheCheckpointAnchor(t *testing.T) {
	authority.SetVerifyPageRows(t, 2) // walks cross keyset page boundaries
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	cases := []struct {
		name    string
		tamper  string
		wantSeq uint64
	}{
		{"anchor payload", `UPDATE security_audit_events SET payload='{"x":3}' WHERE scope='account' AND scope_id=$1 AND seq=3`, 3},
		{"anchor deleted", `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=3`, 3},
		{"anchor duplicated", `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
			SELECT scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at+interval '1 microsecond',payload
			FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=3`, 3},
		{"head moved behind the checkpoint", `WITH gone AS (DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq>=2 RETURNING 1)
			UPDATE accounts a SET audit_seq=1, audit_hash=e.hash FROM security_audit_events e
			WHERE a.id=$1 AND e.scope='account' AND e.scope_id=a.id AND e.seq=1`, 3},
		{"head reset to genesis", `WITH gone AS (DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 RETURNING 1)
			UPDATE accounts SET audit_seq=0, audit_hash=decode(repeat('00',32),'hex') WHERE id=$1`, 3},
	}
	ids := make([]string, len(cases))
	for i := range cases {
		ids[i] = f.Account(t)
		appendRows(t, f, authority.ScopeAccount, ids[i], 3)
	}
	if report := verifyIncremental(t, v, 100); len(report.Failed) != 0 {
		t.Fatalf("clean pass = %+v", report)
	}
	for i, c := range cases {
		if c.name != "head moved behind the checkpoint" && c.name != "head reset to genesis" {
			appendRows(t, f, authority.ScopeAccount, ids[i], 1) // the head moves past the checkpoint
		}
		if n := f.Exec(t, c.tamper, ids[i]); n < 1 {
			t.Fatalf("%s: tamper affected %d rows", c.name, n)
		}
	}
	verifyIncremental(t, v, 100)
	alerts := sink.mismatches(t)
	for i, c := range cases {
		if seq, ok := alerts[ids[i]]; !ok || seq != c.wantSeq {
			t.Errorf("%s: alert seq %d (raised %v), want %d", c.name, seq, ok, c.wantSeq)
		}
		if !chainFailed(t, f, authority.ScopeAccount, ids[i]) {
			t.Errorf("%s: not marked failed", c.name)
		}
	}
}

// A checkpointed tail row rewritten consistently (row hash and head
// recomputed, as only someone with the hash definition and owner access
// can) is caught by the checkpoint: the head moved without its seq moving.
// Both passes hold the chain to its checkpoint (the full verify read-only,
// so it never launders the rewrite into a new checkpoint), and the full
// verify also catches a head truncated consistently behind its checkpoint.
func TestVerifierCatchesAConsistentRewriteOfTheCheckpointedHead(t *testing.T) {
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	forFull, forIncremental, truncated, clean := f.Account(t), f.Account(t), f.Account(t), f.Account(t)
	for _, id := range []string{forFull, forIncremental, truncated, clean} {
		appendRows(t, f, authority.ScopeAccount, id, 3)
	}
	verifyIncremental(t, v, 100)
	rewrite := func(account string) {
		t.Helper()
		last := readAudit(t, f, authority.ScopeAccount, account)[2]
		forged := []byte(`{"forged":true}`)
		hash := authority.AuditRowHash(authority.ScopeAccount, account, last.Seq, last.PrevHash, authority.Kind(last.Kind), last.CommandID, last.ActorID, last.At, forged)
		f.Exec(t, `UPDATE security_audit_events SET payload=$2::jsonb, hash=$3 WHERE scope='account' AND scope_id=$1 AND seq=3`, account, string(forged), hash[:])
		f.Exec(t, `UPDATE accounts SET audit_hash=$2 WHERE id=$1`, account, hash[:])
	}
	rewrite(forFull)
	f.Exec(t, `WITH gone AS (DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq>=2 RETURNING 1)
		UPDATE accounts a SET audit_seq=1, audit_hash=e.hash FROM security_audit_events e
		WHERE a.id=$1 AND e.scope='account' AND e.scope_id=a.id AND e.seq=1`, truncated)

	if got, want := failedIDs(verifyAll(t, v)), refs("account:"+forFull, "account:"+truncated); !slices.Equal(got, want) {
		t.Fatalf("full verify failed = %v, want %v", got, want)
	}
	if cp, _ := checkpointOf(t, f, authority.ScopeAccount, clean); cp.Seq != 3 || !cp.At.Equal(f.Now) {
		t.Fatalf("full verify moved a checkpoint: %+v", cp)
	}
	rewrite(forIncremental)
	if got, want := failedIDs(verifyIncremental(t, v, 100)), refs("account:"+forIncremental); !slices.Equal(got, want) {
		t.Fatalf("incremental failed = %v, want %v", got, want)
	}
	if got := sink.mismatches(t); got[forFull] != 3 || got[truncated] != 3 || got[forIncremental] != 3 || len(got) != 3 {
		t.Fatalf("alerts = %v, want seq 3 for the three tampered chains", got)
	}
}

// A tampered value the walk cannot decode (a non-finite at, which also
// moves the row to the default partition) is a mismatch for that chain
// only: it fails closed with an alert, and the rest of the pass, here a
// control chain, is still verified and checkpointed.
func TestVerifierFailsClosedOnUndecodableRows(t *testing.T) {
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	control, plusInf, minusInf := f.Account(t), f.Account(t), f.Account(t)
	for _, id := range []string{control, plusInf, minusInf} {
		appendRows(t, f, authority.ScopeAccount, id, 3)
	}
	f.Exec(t, `UPDATE security_audit_events SET at='infinity' WHERE scope='account' AND scope_id=$1 AND seq=2`, plusInf)
	report, err := v.VerifyIncremental(t.Context(), 100)
	if err != nil {
		t.Fatalf("incremental pass: %v", err)
	}
	if got, want := failedIDs(report), refs("account:"+plusInf); !slices.Equal(got, want) || report.Scopes != 3 {
		t.Fatalf("incremental = %+v, want 3 scopes and %v failed", report, want)
	}
	if !chainFailed(t, f, authority.ScopeAccount, plusInf) {
		t.Fatal("undecodable chain not failed")
	}
	for _, id := range []string{control, minusInf} {
		if cp, ok := checkpointOf(t, f, authority.ScopeAccount, id); !ok || cp.Seq != 3 {
			t.Fatalf("chain %s not checkpointed in the same pass: %+v", id, cp)
		}
	}

	f.Exec(t, `UPDATE security_audit_events SET at='-infinity' WHERE scope='account' AND scope_id=$1 AND seq=1`, minusInf)
	report, err = v.VerifyAll(t.Context())
	if err != nil {
		t.Fatalf("full verify: %v", err)
	}
	if got, want := failedIDs(report), refs("account:"+plusInf, "account:"+minusInf); !slices.Equal(got, want) || report.Scopes != 3 {
		t.Fatalf("full verify = %+v, want 3 scopes and %v failed", report, want)
	}
	if got := sink.mismatches(t); got[plusInf] != 2 || got[minusInf] != 1 || got[control] != 0 {
		t.Fatalf("alerts = %v, want seq 2 and seq 1", got)
	}
	if chainFailed(t, f, authority.ScopeAccount, control) || !chainFailed(t, f, authority.ScopeAccount, minusInf) {
		t.Fatal("full verify did not fail exactly the tampered chains")
	}
}

// Rows of a scope whose head is at genesis are found by the full verify.
func TestVerifyAllFindsRowsBehindAGenesisHead(t *testing.T) {
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	account := f.Account(t)
	appendRows(t, f, authority.ScopeAccount, account, 2)
	f.Exec(t, `UPDATE accounts SET audit_seq=0, audit_hash=decode(repeat('00',32),'hex') WHERE id=$1`, account)
	if report := verifyIncremental(t, v, 100); report.Scopes != 0 {
		t.Fatalf("incremental pass = %+v, want nothing (genesis head, no checkpoint)", report)
	}
	if got, want := failedIDs(verifyAll(t, v)), refs("account:"+account); !slices.Equal(got, want) {
		t.Fatalf("full verify failed = %v, want %v", got, want)
	}
	if got := sink.mismatches(t); got[account] != 1 {
		t.Fatalf("alerts = %v, want seq 1", got)
	}
}

// Verification reads one snapshot: appends committing while it walks are
// never mistaken for rows beyond the head or a moved head.
func TestVerifierHasNoFalsePositivesUnderConcurrentAppends(t *testing.T) {
	authority.SetVerifyPageRows(t, 2) // walks cross keyset page boundaries
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	accounts := []string{f.Account(t), f.Account(t)}
	var writers sync.WaitGroup
	defer writers.Wait() // after cancel: no writer outlives the test
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for _, id := range accounts {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for range 40 {
				if err := tryAppendRows(ctx, t, f, authority.ScopeAccount, id, 2); err != nil {
					t.Error(err)
					return
				}
				if err := tryAppendRows(ctx, t, f, authority.ScopeFamily, family, 1); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { writers.Wait(); close(done) }()
	passes := 0
	for running := true; running; passes++ {
		select {
		case <-done:
			running = false
		default:
		}
		incremental, err := v.VerifyIncremental(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		all, err := v.VerifyAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(incremental.Failed)+len(all.Failed) != 0 {
			t.Fatalf("pass %d: false positive: %+v %+v; alerts %v", passes, incremental, all, sink.mismatches(t))
		}
	}
	if passes < 2 {
		t.Fatalf("only %d passes overlapped the writers", passes)
	}
	for _, id := range accounts {
		seq, hash := chainHead(t, f, authority.ScopeAccount, id)
		if cp, _ := checkpointOf(t, f, authority.ScopeAccount, id); seq != 80 || cp.Seq != seq || cp.Hash != hash {
			t.Fatalf("account head %d, checkpoint %+v", seq, cp)
		}
	}
}

func TestVerifierRunLoopsUntilCancelled(t *testing.T) {
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	account := f.Account(t)
	appendRows(t, f, authority.ScopeAccount, account, 2)
	f.Exec(t, `UPDATE security_audit_events SET payload='{}' WHERE scope='account' AND scope_id=$1 AND seq=2`, account)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { v.Run(ctx, 10*time.Millisecond); close(stopped) }()
	deadline := time.Now().Add(10 * time.Second)
	for !chainFailed(t, f, authority.ScopeAccount, account) {
		if time.Now().After(deadline) {
			t.Fatal("Run never verified the tampered chain")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if got := sink.mismatches(t); got[account] != 2 {
		t.Fatalf("alerts = %v", got)
	}
}
