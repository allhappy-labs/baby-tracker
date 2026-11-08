package authority_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/maintenance"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// retentionClock is the sweep's app clock in the retention tests: 13 months
// back from it is 2025-09-16, so the 2025-03 and 2025-04 partitions below
// have expired and the 2026 one has not, whenever the tests run.
var retentionClock = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// createAuditMonth attaches a monthly audit partition for month as the
// table owner, as app.ensure_audit_partitions would have when it was current.
func createAuditMonth(t *testing.T, f *authoritytest.Fixture, month time.Time) {
	t.Helper()
	name := pgx.Identifier{"public", "security_audit_events_" + month.Format("2006_01")}.Sanitize()
	if _, err := f.DB.Owner.Exec(t.Context(), `CREATE TABLE `+name+` PARTITION OF public.security_audit_events FOR VALUES FROM ('`+
		month.Format(time.RFC3339)+`') TO ('`+month.AddDate(0, 1, 0).Format(time.RFC3339)+`')`); err != nil {
		t.Fatal(err)
	}
}

// Owner decision D1: the retention sweep drops whole expired partitions and
// records, per chain, the seq and hash of its last dropped row. Both
// verifiers then walk each chain from that anchor: every chain whose oldest
// rows expired verifies clean, whatever its checkpoint (none, below the
// anchor, at it, or at a head that is the anchor), and a kept row that was
// changed, removed or consistently rewritten from the oldest kept row still
// fails its chain closed, at the first broken seq.
func TestVerifierAnchorsAtTheOldestKeptPartition(t *testing.T) {
	authority.SetVerifyPageRows(t, 2) // walks cross keyset page boundaries
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	march, april := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	createAuditMonth(t, f, march)
	createAuditMonth(t, f, april)
	kept := f.Now // 2026-10-04: the 2026 partition

	owner := f.Account(t)
	family := f.Family(t, owner)
	chains := map[string]string{}
	for _, name := range []string{"split", "expired", "boundary", "recent", "rewritten", "edited", "deleted"} {
		chains[name] = f.Account(t)
	}
	account := func(name string, n int) { appendRows(t, f, authority.ScopeAccount, chains[name], n) }

	f.Now = march.Add(4 * 24 * time.Hour)
	for _, name := range []string{"split", "expired", "boundary", "rewritten", "edited", "deleted"} {
		account(name, 2)
	}
	// Checkpoints at seq 2: below split's anchor, at the others'.
	if report := verifyIncremental(t, v, 100); report.Scopes != 6 || len(report.Failed) != 0 {
		t.Fatalf("first pass %+v", report)
	}
	account("split", 1)
	f.Now = april.Add(4 * 24 * time.Hour)
	account("split", 2)
	appendRows(t, f, authority.ScopeFamily, family, 2)
	f.Now = kept
	account("split", 3)
	for _, name := range []string{"boundary", "recent", "rewritten", "edited", "deleted"} {
		account(name, 2)
	}
	appendRows(t, f, authority.ScopeFamily, family, 2)
	if report := verifyAll(t, v); len(report.Failed) != 0 {
		t.Fatalf("before the drop: %+v", report)
	}
	// The anchors the drop must record: each chain's last row before 2026.
	wantAnchors := map[string]uint64{"split": 5, "expired": 2, "boundary": 2, "rewritten": 2, "edited": 2, "deleted": 2, "family": 2}
	lastDropped := map[string][32]byte{}
	for name, seq := range wantAnchors {
		scope, id := authority.ScopeAccount, chains[name]
		if name == "family" {
			scope, id = authority.ScopeFamily, family
		}
		lastDropped[name] = readAudit(t, f, scope, id)[seq-1].Hash
	}

	sweep := maintenance.AuditRetentionSweep{Pool: f.DB.Maintenance}
	if n, err := sweep.Run(t.Context(), retentionClock); err != nil || n != 2 {
		t.Fatalf("the sweep dropped %d partitions (%v), want 2025-03 and 2025-04", n, err)
	}
	if n := count(t, f, `SELECT count(*) FROM security_audit_events WHERE at < '2026-01-01'`); n != 0 {
		t.Fatalf("%d expired rows survive the drop", n)
	}
	for name, seq := range wantAnchors {
		scope, id := "account", chains[name]
		if name == "family" {
			scope, id = "family", family
		}
		var gotSeq uint64
		var gotHash []byte
		inAuthorityTx(t, f, func(tx pgx.Tx) {
			if err := tx.QueryRow(t.Context(), `SELECT anchor_seq, anchor_hash FROM audit_retention_anchors WHERE scope=$1::text::app.audit_scope AND scope_id=$2`,
				scope, id).Scan(&gotSeq, &gotHash); err != nil {
				t.Fatalf("%s anchor: %v", name, err)
			}
		})
		if gotSeq != seq || [32]byte(gotHash) != lastDropped[name] {
			t.Fatalf("%s anchor at %d, want %d and the last dropped row's hash", name, gotSeq, seq)
		}
	}
	if n := count(t, f, `SELECT count(*) FROM audit_retention_anchors WHERE scope_id=$1`, chains["recent"]); n != 0 {
		t.Fatal("a chain with no expired row was anchored")
	}

	// Clean after the drop, from genesis and incrementally.
	if report := verifyAll(t, v); len(report.Failed) != 0 || report.Rows != 15 {
		t.Fatalf("full verify after the drop = %+v, want the 15 kept rows clean", report)
	}
	if report := verifyIncremental(t, v, 100); len(report.Failed) != 0 || report.Scopes != 7 {
		t.Fatalf("incremental pass after the drop = %+v, want the 7 moved chains clean", report)
	}
	if report := verifyAll(t, v); len(report.Failed) != 0 {
		t.Fatalf("full verify against the new checkpoints = %+v", report)
	}
	if got := sink.mismatches(t); len(got) != 0 {
		t.Fatalf("alerts after the drop: %v", got)
	}

	// A kept row tampered after the drop still fails closed.
	f.Exec(t, `UPDATE security_audit_events SET payload=jsonb_set(payload,'{i}','7') WHERE scope='account' AND scope_id=$1 AND seq=4`, chains["edited"])
	f.Exec(t, `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND seq=3`, chains["deleted"])
	// The oldest kept row and every row after it rewritten consistently, head
	// included: only the anchor shows it.
	prev := sha256.Sum256([]byte("forged"))
	for _, row := range readAudit(t, f, authority.ScopeAccount, chains["rewritten"]) {
		row.PrevHash = prev
		payload, err := authority.CanonicalJSON(json.RawMessage(row.Payload))
		if err != nil {
			t.Fatal(err)
		}
		hash := authority.AuditRowHash(authority.ScopeAccount, row.ScopeID, row.Seq, row.PrevHash, authority.Kind(row.Kind), row.CommandID, row.ActorID, row.At, payload)
		f.Exec(t, `UPDATE security_audit_events SET prev_hash=$3, hash=$4 WHERE scope='account' AND scope_id=$1 AND seq=$2`, row.ScopeID, int64(row.Seq), prev[:], hash[:])
		prev = hash
	}
	f.Exec(t, `UPDATE accounts SET audit_hash=$2 WHERE id=$1`, chains["rewritten"], prev[:])

	report := verifyAll(t, v)
	failed := map[string]bool{}
	for _, ref := range report.Failed {
		failed[ref.ID] = true
	}
	want := map[string]uint64{chains["edited"]: 4, chains["deleted"]: 3, chains["rewritten"]: 3}
	if len(failed) != len(want) || !failed[chains["edited"]] || !failed[chains["deleted"]] || !failed[chains["rewritten"]] {
		t.Fatalf("failed %v, want exactly the edited, deleted and rewritten chains", report.Failed)
	}
	if got := sink.mismatches(t); !maps.Equal(got, want) {
		t.Fatalf("alerts %v, want %v", got, want)
	}
	for name, id := range chains {
		if chainFailed(t, f, authority.ScopeAccount, id) != slices.Contains([]string{"edited", "deleted", "rewritten"}, name) {
			t.Fatalf("chain %s failed=%t", name, !chainFailed(t, f, authority.ScopeAccount, id))
		}
	}
	if chainFailed(t, f, authority.ScopeFamily, family) {
		t.Fatal("the clean family chain failed")
	}
}

// Rows at or below a retention anchor are not walked, so a row inserted
// there after the drop (a copy of an expired or kept seq) would never be
// seen. Every row the chain had at the drop was stamped before the end of
// the partition dropped with the anchor, give or take boundary skew: the
// full verify fails a chain with a row at or below its anchor stamped more
// than a day after that end. A lower seq stamped just after the boundary (a
// legitimate skew, here in the default partition) still verifies.
func TestVerifierFailsARowBelowTheAnchorInsertedAfterTheDrop(t *testing.T) {
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	march, april := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	createAuditMonth(t, f, march)
	forged, skewed := f.Account(t), f.Account(t)
	f.Now = march.Add(4 * 24 * time.Hour)
	appendRows(t, f, authority.ScopeAccount, forged, 2)
	// skewed: seq 1 stamped a second after the March partition's end (it
	// lands in the default partition: there is no April one), seq 2 an hour
	// before it, in March. (appendRows stamps seq n at f.Now + (n-1) s.)
	f.Now = april.Add(time.Second)
	appendRows(t, f, authority.ScopeAccount, skewed, 1)
	f.Now = april.Add(-time.Hour - time.Second)
	appendRows(t, f, authority.ScopeAccount, skewed, 1)
	f.Now = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	appendRows(t, f, authority.ScopeAccount, forged, 1)
	appendRows(t, f, authority.ScopeAccount, skewed, 1)

	if n, err := (maintenance.AuditRetentionSweep{Pool: f.DB.Maintenance}).Run(t.Context(), retentionClock); err != nil || n != 1 {
		t.Fatalf("dropped %d (%v), want March", n, err)
	}
	if report := verifyAll(t, v); len(report.Failed) != 0 {
		t.Fatalf("verify after the drop = %+v (alerts %v), want both chains clean", report, sink.mismatches(t))
	}
	// After the drop, the runtime inserts a copy of the expired seq 2 (into
	// the current partition, stamped now): below the anchor, never walked.
	f.Exec(t, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
		SELECT e.scope,e.scope_id,2,e.prev_hash,a.anchor_hash,'session_granted',gen_random_uuid(),e.scope_id,$2,'{}'
		  FROM security_audit_events e JOIN audit_retention_anchors a ON a.scope=e.scope AND a.scope_id=e.scope_id
		 WHERE e.scope='account' AND e.scope_id=$1 AND e.seq=3`, forged, f.Now)
	report := verifyAll(t, v)
	if len(report.Failed) != 1 || report.Failed[0].ID != forged || !chainFailed(t, f, authority.ScopeAccount, forged) || chainFailed(t, f, authority.ScopeAccount, skewed) {
		t.Fatalf("verify after a post-drop insertion = %+v, want exactly the forged chain failed", report)
	}
	if alerts := sink.mismatches(t); len(alerts) != 1 || alerts[forged] != 2 {
		t.Fatalf("alerts %v, want the forged chain at seq 2", alerts)
	}
}

// A verifier transaction holds the audit table from before its snapshot: a
// retention drop cannot commit between the anchor a walk reads and the rows
// it reads, even when the walk has read only the anchor so far. The drop
// gives up (55P03) while the transaction is open, and succeeds after it.
func TestVerifierReadHoldsTheAuditTableAgainstADrop(t *testing.T) {
	f := authoritytest.New(t)
	v, _ := newChainVerifier(f)
	createAuditMonth(t, f, time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))
	account := f.Account(t)
	f.Now = time.Date(2025, 3, 5, 0, 0, 0, 0, time.UTC)
	appendRows(t, f, authority.ScopeAccount, account, 2)
	sweep := maintenance.AuditRetentionSweep{Pool: f.DB.Maintenance}
	err := authority.VerifierReadForTest(v, t.Context(), func(tx pgx.Tx) error {
		var anchors int
		if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM audit_retention_anchors`).Scan(&anchors); err != nil {
			return err
		}
		start := time.Now()
		n, err := sweep.Run(t.Context(), retentionClock)
		var pgErr *pgconn.PgError
		if n != 0 || !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Errorf("a drop inside a verifier transaction: dropped %d (%v), want 55P03", n, err)
		}
		if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
			t.Errorf("the drop waited %v", elapsed)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := sweep.Run(t.Context(), retentionClock); err != nil || n != 1 {
		t.Fatalf("after the verifier transaction: dropped %d (%v), want 1", n, err)
	}
	if report := verifyAll(t, v); len(report.Failed) != 0 || report.Rows != 0 {
		t.Fatalf("verify after the drop = %+v, want the chain clean at its anchor", report)
	}
}
