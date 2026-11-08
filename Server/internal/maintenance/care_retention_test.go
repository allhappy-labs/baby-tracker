package maintenance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/database"
	"babytracker/server/internal/maintenance"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
	"github.com/jackc/pgx/v5"
)

// The harness clock trails the database clock by less than a day, and these
// tests sweep a retention window or more ahead of it, so a sweep that cut
// receipts, bindings, sessions or cursors by the database's now() would keep
// what these tests require deleted.

// The retention windows and bounds, pinned by value.
func TestCareRetentionBoundsArePinned(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want time.Duration
	}{
		{"ReceiptRetention", maintenance.ReceiptRetention, 30 * 24 * time.Hour},
		{"PageGrace", maintenance.PageGrace, time.Hour},
		{"DeviceCursorRetention", maintenance.DeviceCursorRetention, 35 * 24 * time.Hour},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"ReceiptBatch", maintenance.ReceiptBatch, 1000},
		{"ReceiptPassCap", maintenance.ReceiptPassCap, 2000},
		{"ExpiryBatch", maintenance.ExpiryBatch, 1000},
		{"ExpiryPassCap", maintenance.ExpiryPassCap, 5000},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if !strings.Contains(maintenance.ReceiptTimeouts, "'lock_timeout', '200ms'") || !strings.Contains(maintenance.ReceiptTimeouts, "'statement_timeout', '10s'") {
		t.Errorf("receipt sweep timeouts %q, want lock_timeout 200ms and statement_timeout 10s", maintenance.ReceiptTimeouts)
	}
}

// Review Focus 1: a retry 29 days 23 hours after acceptance, arriving while
// a writer holds the family row and the sweep runs beside it, still replays
// its receipt (already_applied); the sweep skips the held family at once and
// deletes only receipts at least 30 days old by the clock that stamped them.
func TestReceiptRetentionKeepsTheRetryWindow(t *testing.T) {
	h := testkit.New(t)
	accepted := h.Now
	h.Now = accepted.Add(-48 * time.Hour)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	ancient := caretest.Feed(caretest.BabyA, 1) // accepted two days before old: due while old is not
	caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{ancient})
	h.Now = accepted
	old := caretest.Feed(caretest.BabyA, 2)
	caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{old})
	sweep := &maintenance.ReceiptSweep{Pool: h.Maintenance, Batch: 1000}

	// 29 days 23 hours after old's acceptance a writer holds the family row;
	// old's retry queues behind it while the sweep runs.
	retryAt := accepted.Add(29*24*time.Hour + 23*time.Hour)
	h.Now = retryAt
	holder := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, err := care.LockFamily(t.Context(), holder, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	retryTx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	retryCommand := caretest.Command(t, h, h.Owner, []protocol.Mutation{old})
	type retried struct {
		result care.BatchResult
		err    error
	}
	done := make(chan retried, 1)
	go func() {
		result, err := care.ApplyBatch(t.Context(), retryTx, retryCommand, nil)
		if err == nil {
			err = retryTx.Commit(t.Context())
		}
		done <- retried{result, err}
	}()
	for range 20 {
		start := time.Now()
		if n, err := sweep.Run(t.Context(), retryAt); err != nil || n != 0 || time.Since(start) > time.Second {
			t.Fatalf("sweep beside a writer: removed %d (%v) after %v; want 0 at once, the held family skipped", n, err, time.Since(start))
		}
	}
	select {
	case r := <-done:
		t.Fatalf("the retry did not wait for the writer: %+v %v", r.result.Outcomes, r.err)
	default:
	}
	_ = holder.Rollback(t.Context())
	r := <-done
	if r.err != nil || len(r.result.Outcomes) != 1 || r.result.Outcomes[0].Status != "already_applied" {
		t.Fatalf("retry 29 days 23 hours after acceptance: %+v %v", r.result.Outcomes, r.err)
	}
	// The family free, the sweep takes the receipts at least 30 days old
	// (the baby's and ancient's) and never old's.
	if n, err := sweep.Run(t.Context(), retryAt); err != nil || n != 2 {
		t.Fatalf("sweep at 29 days 23 hours: removed %d (%v), want the two receipts past 30 days", n, err)
	}
	if _, found := caretest.StoredReceipt(t, h, h.Owner, ancient.MutationID); found {
		t.Fatal("a receipt 31 days 23 hours old survived")
	}
	if _, found := caretest.StoredReceipt(t, h, h.Owner, old.MutationID); !found {
		t.Fatal("a receipt 29 days 23 hours old was deleted")
	}
	caretest.AssertCounters(t, h, h.FamilyID)

	// One second short of 30 days the receipt is kept and a retry replays it;
	// at 30 days it goes, and a younger one stays.
	h.Now = accepted.Add(maintenance.ReceiptRetention - time.Second)
	young := caretest.Feed(caretest.BabyA, 3)
	caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{young})
	if _, err := sweep.Run(t.Context(), h.Now); err != nil {
		t.Fatal(err)
	}
	if _, found := caretest.StoredReceipt(t, h, h.Owner, old.MutationID); !found {
		t.Fatal("a receipt one second short of 30 days was deleted")
	}
	if replay := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{old}); replay.Outcomes[0].Status != "already_applied" {
		t.Fatalf("retry inside the window: %+v", replay.Outcomes[0])
	}
	h.Now = accepted.Add(30 * 24 * time.Hour) // the spec's 30 days, literally
	if _, err := sweep.Run(t.Context(), h.Now); err != nil {
		t.Fatal(err)
	}
	if _, found := caretest.StoredReceipt(t, h, h.Owner, old.MutationID); found {
		t.Fatal("a 30-day-old receipt survived")
	}
	if _, found := caretest.StoredReceipt(t, h, h.Owner, young.MutationID); !found {
		t.Fatal("a younger receipt was deleted")
	}
	caretest.AssertCounters(t, h, h.FamilyID)

	// A writer holding the family row: the sweep skips the family at once.
	holder = caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, err := care.LockFamily(t.Context(), holder, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	h.Now = h.Now.Add(maintenance.ReceiptRetention)
	start := time.Now()
	if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != 0 || time.Since(start) > time.Second {
		t.Fatalf("sweep beside a writer: %d %v after %v", n, err, time.Since(start))
	}
	_ = holder.Rollback(t.Context())
	if n, err := sweep.Run(t.Context(), h.Now); err != nil || n == 0 {
		t.Fatalf("after the writer: %d %v", n, err)
	}
	caretest.AssertCounters(t, h, h.FamilyID)
}

// receipt is a stored receipt's key and acceptance.
type receipt struct {
	key      string
	accepted time.Time
}

// familyReceipts is every receipt of the family, read by the maintenance
// login (which sees care_receipts whole).
func familyReceipts(t *testing.T, h *testkit.Harness, familyID string) []receipt {
	t.Helper()
	rows, err := h.Maintenance.Query(t.Context(), `SELECT device_id::text || '/' || mutation_id::text, accepted_at FROM care_receipts WHERE family_id = $1`, familyID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (receipt, error) {
		var r receipt
		err := row.Scan(&r.key, &r.accepted)
		return r, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// Review Focus 2: receipts of creates, updates, deletes, refusals, timer
// commands and replays across 90 days are swept in batches of 7 (crossing
// acceptance times and mutation boundaries), with one batch rolled back
// between its delete and its release. After every committed batch the
// family's counters equal the charge recomputed from what remains; each
// pass removes exactly the receipts due at its clock.
func TestReceiptRetentionReleasesExactCharges(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	mutation := func(n int) string { return fmt.Sprintf("7a000000-0000-4000-8000-%012d", n) }
	// Notes whose encoding differs between Go and jsonb text (HTML escapes,
	// tab and newline, U+2028/2029, astral and combining characters, a
	// BOM, a literal backslash-u, a long non-Latin text): the stored outcome
	// must re-encode to the bytes it was charged with.
	notes := []string{"<&>\"\\/", "tab\tand\nnewline (the only control characters a note admits)", "\u2028 and \u2029", "👩‍👩‍👧 ZWJ family, q\u0301 combining (NFC keeps it)", "BOM \ufeff, U+FFFD \ufffd, \U0010ffff",
		`a literal \u0000 escape`, strings.Repeat("ж", 1000)}
	for step := range 40 {
		h.Now = h.Now.Add(time.Duration(step%7) * 18 * time.Hour)
		feed := caretest.Feed(caretest.BabyA, 1000+step)
		batch := []protocol.Mutation{feed,
			caretest.Update(feed, mutation(10*step+1), 1, caretest.WithNote(fmt.Sprintf("step %d %s", step, notes[step%len(notes)]))), // an update
			caretest.Feed("11000000-0000-4000-8000-0000000000ff", step),                                                               // baby_not_found: a refusal with a receipt
			caretest.Record("measurement", caretest.BabyA, 2000+step)}                                                                 // decimal weight and length
		if step%3 == 0 {
			batch = append(batch, caretest.Delete(feed, mutation(10*step+2), 2))
		}
		if step%5 == 0 { // timer and completion IDs in their own ranges, clear of caretest.Feed's
			start, finish, edit, stop := caretest.FeedTimerLifecycle(caretest.BabyA, fmt.Sprintf("5c000000-0000-4000-8000-%012d", step),
				fmt.Sprintf("2c000000-0000-4000-8000-%012d", step))
			batch = append(batch, start, finish, edit, stop)
		}
		result := caretest.ApplyCommitted(t, h, h.Owner, batch)
		if result.Fatal != nil || len(result.Outcomes) != len(batch) {
			t.Fatalf("step %d: %v, %d of %d outcomes", step, result.Fatal, len(result.Outcomes), len(batch))
		}
		if code := caretest.Code(result.Outcomes[2]); code != "baby_not_found" {
			t.Fatalf("step %d: the refusal is %q", step, code)
		}
		if result.Outcomes[1].Status != "applied" || result.Outcomes[3].Status != "applied" {
			t.Fatalf("step %d: the noted update %+v (%s), the measurement %+v (%s)", step, result.Outcomes[1].Status, caretest.Code(result.Outcomes[1]),
				result.Outcomes[3].Status, caretest.Code(result.Outcomes[3]))
		}
		if replay := caretest.ApplyCommitted(t, h, h.Owner, batch[:1]); replay.Outcomes[0].Status != "already_applied" { // a replay writes no receipt
			t.Fatalf("step %d replay: %+v", step, replay.Outcomes[0])
		}
	}
	caretest.AssertCounters(t, h, h.FamilyID)
	all := familyReceipts(t, h, h.FamilyID)
	first, last := all[0].accepted, all[0].accepted
	for _, r := range all {
		first, last = minTime(first, r.accepted), maxTime(last, r.accepted)
	}
	if len(all) < 150 || last.Sub(first) < 60*24*time.Hour {
		t.Fatalf("%d receipts over %v: the mix does not span the passes", len(all), last.Sub(first))
	}

	sweep := &maintenance.ReceiptSweep{Pool: h.Maintenance, Batch: 7} // small batches cross time and mutation boundaries
	batches := 0
	sweep.AfterCommit(func() { batches++; caretest.AssertCounters(t, h, h.FamilyID) }) // after every batch
	ctx, cancel := context.WithCancel(t.Context())
	sweep.AfterDelete(cancel) // the first batch is rolled back between its delete and its release
	if _, err := sweep.Run(ctx, last.Add(maintenance.ReceiptRetention)); err == nil {
		t.Fatal("a cancelled batch reported success")
	}
	sweep.AfterDelete(nil)
	caretest.AssertCounters(t, h, h.FamilyID) // nothing deleted, nothing released
	if left := familyReceipts(t, h, h.FamilyID); len(left) != len(all) || batches != 0 {
		t.Fatalf("the rolled-back batch: %d of %d receipts left, %d batches committed", len(left), len(all), batches)
	}
	passes, widest := 0, 0
	var clocks []time.Time // every 12 hours from before the first receipt falls due, then when the last does
	for now := first.Add(maintenance.ReceiptRetention).Add(-6 * time.Hour); now.Before(last.Add(maintenance.ReceiptRetention)); now = now.Add(12 * time.Hour) {
		clocks = append(clocks, now)
	}
	for _, now := range append(clocks, last.Add(maintenance.ReceiptRetention)) {
		before := batches
		if _, err := sweep.Run(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		if batches > before {
			passes++
		}
		widest = max(widest, batches-before)
		caretest.AssertCounters(t, h, h.FamilyID)
		cutoff := now.Add(-maintenance.ReceiptRetention)
		want := 0
		for _, r := range all {
			if r.accepted.After(cutoff) {
				want++
			}
		}
		left := familyReceipts(t, h, h.FamilyID)
		for _, r := range left {
			if !r.accepted.After(cutoff) {
				t.Fatalf("at %v a receipt accepted %v (due) survived", now, r.accepted)
			}
		}
		if len(left) != want {
			t.Fatalf("at %v %d receipts left, want the %d accepted after %v", now, len(left), want, cutoff)
		}
	}
	if left := familyReceipts(t, h, h.FamilyID); len(left) != 0 {
		t.Fatalf("%d receipts left after every one fell due", len(left))
	}
	if passes < 10 || batches < len(all)/7 || widest < 2 {
		t.Fatalf("%d passes deleted in %d batches (at most %d in one pass): the sweep did not cross the history in batches of 7", passes, batches, widest)
	}
	var charged int64
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT care_charged_bytes FROM families WHERE id = $1`, h.FamilyID).Scan(&charged); err != nil || charged <= 0 {
		t.Fatalf("charged bytes %d (%v) once every receipt is released: the rows still hold their charge", charged, err)
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Two receipt sweeps beside each other never share a family: each receipt
// is deleted once, and the counters stay exact.
func TestReceiptSweepsRunBesideThemselves(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	for step := range 30 {
		caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{caretest.Feed(caretest.BabyA, 10+step), caretest.Feed(caretest.BabyA, 100+step)})
	}
	total := len(familyReceipts(t, h, h.FamilyID))
	now := h.Now.Add(maintenance.ReceiptRetention)
	var wg sync.WaitGroup
	counts := make([]int, 2)
	errs := make([]error, 2)
	for i := range 2 {
		sweep := &maintenance.ReceiptSweep{Pool: h.Maintenance, Batch: 3}
		wg.Go(func() {
			for {
				n, err := sweep.Run(t.Context(), now)
				counts[i] += n
				if err != nil {
					errs[i] = err
					return
				}
				if n == 0 && len(familyReceipts(t, h, h.FamilyID)) == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if counts[0]+counts[1] != total {
		t.Fatalf("the two sweeps deleted %d + %d of %d receipts", counts[0], counts[1], total)
	}
	caretest.AssertCounters(t, h, h.FamilyID)
}

// outsiderFamily is a second family, created by the outsider (its owner),
// and the outsider's principal refreshed into it.
func outsiderFamily(t *testing.T, h *testkit.Harness) (string, access.Principal) {
	t.Helper()
	created, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "B")
	if err != nil {
		t.Fatal(err)
	}
	return created.ID, h.Refresh("outsider")
}

// applyIn applies mutations as p, the owner of family, accepted at h.Now,
// and requires every one to have an outcome.
func applyIn(t *testing.T, h *testkit.Harness, p access.Principal, family string, mutations ...protocol.Mutation) {
	t.Helper()
	command := care.BatchCommand{Grant: access.Grant{AccountID: p.AccountID, FamilyID: family, Role: "owner"}, DeviceID: p.DeviceID,
		Mutations: mutations, AcceptedAt: h.Now, Server: h.Info, Limits: h.Limits, Policy: caretest.Policy}
	tx := caretest.TenantTx(t, h.Runtime, p.AccountID, family)
	if result, err := care.ApplyBatch(t.Context(), tx, command, nil); err != nil || result.Fatal != nil || len(result.Outcomes) != len(mutations) {
		t.Fatalf("batch in %s: %+v %v", family, result, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// familyCharged is a family's care_charged_bytes, read by the maintenance
// login.
func familyCharged(t *testing.T, h *testkit.Harness, family string) int64 {
	t.Helper()
	var charged int64
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT care_charged_bytes FROM families WHERE id = $1`, family).Scan(&charged); err != nil {
		t.Fatal(err)
	}
	return charged
}

// errorLogs are the Error records of a JSON log.
func errorLogs(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if record["level"] == "ERROR" {
			records = append(records, record)
		}
	}
	return records
}

// A family whose release fails (its counters have drifted below its
// receipts' charge) or whose stored outcome does not decode is quarantined
// (for an hour; see TestReceiptQuarantineExpiresAndIsCapped): logged once at Error with its family ID and
// the cause class, left untouched, and every other family's due receipts are
// still swept, in the same pass. Family A here holds the globally oldest due
// receipts, so without the quarantine every batch would select it and roll
// back.
func TestReceiptSweepQuarantinesAFailingFamily(t *testing.T) {
	for _, c := range []struct{ name, cause string }{{"drift", "release_below_zero"}, {"undecodable", "undecodable_outcome"}} {
		t.Run(c.name, func(t *testing.T) {
			h := testkit.New(t)
			accepted := h.Now
			h.Now = accepted.Add(-48 * time.Hour) // A's receipts are the oldest
			caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Feed(caretest.BabyA, 1), caretest.Feed(caretest.BabyA, 2))
			switch c.name {
			case "drift":
				h.ExecFamily(t, `UPDATE families SET care_charged_bytes = 1 WHERE id = $1`, h.FamilyID)
			case "undecodable":
				tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
				if _, err := tx.Exec(t.Context(), `INSERT INTO care_receipts(device_id, mutation_id, family_id, account_id, semantic_hash, outcome, accepted_at)
					VALUES ($1, '7b000000-0000-4000-8000-000000000001', $2, $3, sha256('x'::bytea), '{"status": 7}', $4)`,
					h.Owner.DeviceID, h.FamilyID, h.Owner.AccountID, h.Now.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			h.Now = accepted
			other, outsider := outsiderFamily(t, h)
			applyIn(t, h, outsider, other, caretest.Baby(caretest.BabyB), caretest.Feed(caretest.BabyB, 1), caretest.Feed(caretest.BabyB, 2))
			ours, theirs := familyReceipts(t, h, h.FamilyID), familyReceipts(t, h, other)
			charged := familyCharged(t, h, h.FamilyID)
			var logs bytes.Buffer
			sweep := &maintenance.ReceiptSweep{Pool: h.Maintenance, Batch: 2, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			now := accepted.Add(maintenance.ReceiptRetention)
			if n, err := sweep.Run(t.Context(), now); err != nil || n != len(theirs) {
				t.Fatalf("with family A failing: removed %d (%v), want family B's %d in the same pass", n, err, len(theirs))
			}
			caretest.AssertCounters(t, h, other)
			if left := familyReceipts(t, h, other); len(left) != 0 {
				t.Fatalf("%d of family B's receipts left", len(left))
			}
			if left := familyReceipts(t, h, h.FamilyID); len(left) != len(ours) || familyCharged(t, h, h.FamilyID) != charged {
				t.Fatalf("the quarantined family: %d of %d receipts left, charged %d (was %d)", len(left), len(ours), familyCharged(t, h, h.FamilyID), charged)
			}
			// Later passes skip it without a second log; B's new due receipts still go.
			applyIn(t, h, outsider, other, caretest.Feed(caretest.BabyB, 3))
			if n, err := sweep.Run(t.Context(), now); err != nil || n != 1 {
				t.Fatalf("the next pass: removed %d (%v), want family B's new receipt", n, err)
			}
			records := errorLogs(t, &logs)
			if len(records) != 1 || records[0]["family_id"] != h.FamilyID || records[0]["cause"] != c.cause {
				t.Fatalf("error logs %v, want one naming family A and %s", records, c.cause)
			}
			if c.name == "undecodable" && (records[0]["device_id"] != h.Owner.DeviceID || records[0]["mutation_id"] != "7b000000-0000-4000-8000-000000000001") {
				t.Fatalf("error log %v, want the undecodable receipt's key", records[0])
			}
			if strings.Contains(logs.String(), `"status"`) || strings.Contains(logs.String(), "outcome\":") {
				t.Fatalf("the log carries outcome contents: %s", logs.String())
			}
			if left := familyReceipts(t, h, h.FamilyID); len(left) != len(ours) {
				t.Fatalf("the quarantined family lost receipts on a later pass: %d of %d", len(left), len(ours))
			}
		})
	}
}

// A batch steps past at most 16 failing families, so n failing families
// (their release would go below zero) converge in ceil(n/16) passes: each
// of the first ceil(n/16)-1 passes quarantines 16 and gives up (an error,
// the batch rolled back, the good families' receipts kept); the last
// quarantines the rest and deletes every good family's receipts in the same
// batch. It never loops: a pass beyond that finds nothing due (the failing
// families are quarantined, their receipts and charge kept) and logs
// nothing; each failing family is logged once.
func TestReceiptSweepConvergesPastSixteenBadFamilies(t *testing.T) {
	const good = 5
	for _, bad := range []int{16, 17, 32, 33} {
		t.Run(fmt.Sprintf("%d bad families", bad), func(t *testing.T) {
			h := testkit.New(t)
			accepted := h.Now
			caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Feed(caretest.BabyA, 1), caretest.Feed(caretest.BabyA, 2))
			// The harness family's own receipts are the template and stay
			// younger than the sweep's cutoff (two hours before accepted); the
			// copies are older.
			template := familyReceipts(t, h, h.FamilyID)
			if len(template) == 0 {
				t.Fatal("no receipts to copy")
			}
			// Bare families (the sweep selects by receipt, not by status): the
			// first `bad` carry no charge, so releasing any receipt would take
			// them below zero; the rest carry plenty.
			h.AuthorityExec(t, `INSERT INTO families(id, display_name, status, care_charged_bytes)
				SELECT ('c0000000-0000-4000-8000-' || lpad(i::text, 12, '0'))::uuid, 'sweep ' || i, 'purge_completed', CASE WHEN i <= $1 THEN 0 ELSE 1 << 20 END
				FROM generate_series(1, $2) AS i`, bad, bad+good)
			var devices, accounts, outcomes []string
			var hashes [][]byte
			templateRows, err := h.Maintenance.Query(t.Context(), `SELECT device_id::text, account_id::text, semantic_hash, outcome::text FROM care_receipts WHERE family_id = $1`, h.FamilyID)
			if err != nil {
				t.Fatal(err)
			}
			for templateRows.Next() {
				var device, account, outcome string
				var hash []byte
				if err := templateRows.Scan(&device, &account, &hash, &outcome); err != nil {
					t.Fatal(err)
				}
				devices, accounts, hashes, outcomes = append(devices, device), append(accounts, account), append(hashes, hash), append(outcomes, outcome)
			}
			templateRows.Close()
			if err := templateRows.Err(); err != nil {
				t.Fatal(err)
			}
			families := func(from, to int) []string {
				ids := make([]string, 0, to-from+1)
				for i := from; i <= to; i++ {
					ids = append(ids, fmt.Sprintf("c0000000-0000-4000-8000-%012d", i))
				}
				return ids
			}
			badFamilies, goodFamilies := families(1, bad), families(bad+1, bad+good)
			// A receipt is written within its family's tenant context.
			for _, id := range append(slices.Clone(badFamilies), goodFamilies...) {
				tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, id)
				if _, err := tx.Exec(t.Context(), `INSERT INTO care_receipts(device_id, mutation_id, family_id, account_id, semantic_hash, outcome, accepted_at)
					SELECT r.device::uuid, gen_random_uuid(), $5::uuid, r.account::uuid, r.hash, r.outcome::jsonb, $6
					FROM unnest($1::text[], $2::text[], $3::bytea[], $4::text[]) AS r(device, account, hash, outcome)`,
					devices, accounts, hashes, outcomes, id, accepted.Add(-3*time.Hour)); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			count := func(ids []string) (receipts int) {
				for _, id := range ids {
					receipts += len(familyReceipts(t, h, id))
				}
				return receipts
			}
			if count(badFamilies) != bad*len(template) || count(goodFamilies) != good*len(template) {
				t.Fatalf("fixture: %d bad receipts, %d good", count(badFamilies), count(goodFamilies))
			}

			var logs bytes.Buffer
			sweep := &maintenance.ReceiptSweep{Pool: h.Maintenance, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			now := accepted.Add(maintenance.ReceiptRetention - 2*time.Hour)
			want := (bad + 15) / 16 // ceil(bad/16)
			passes, removed := 0, 0
			for {
				passes++
				if passes > want+2 {
					t.Fatalf("the sweep has not converged after %d passes, want %d", passes-1, want)
				}
				n, err := sweep.Run(t.Context(), now)
				if err == nil {
					removed = n
					break
				}
				if n != 0 || count(goodFamilies) != good*len(template) {
					t.Fatalf("pass %d failed (%v) yet removed %d (good families hold %d receipts)", passes, err, n, count(goodFamilies))
				}
			}
			if passes != want || removed != good*len(template) {
				t.Fatalf("converged in %d passes removing %d, want %d passes removing %d", passes, removed, want, good*len(template))
			}
			if left := count(goodFamilies); left != 0 {
				t.Fatalf("%d of the good families' receipts left", left)
			}
			if left := count(badFamilies); left != bad*len(template) {
				t.Fatalf("the failing families hold %d receipts, want all %d kept", left, bad*len(template))
			}
			for _, id := range badFamilies {
				if familyCharged(t, h, id) != 0 {
					t.Fatalf("family %s charge changed", id)
				}
			}
			// One more pass: nothing due, no error, no further log.
			if n, err := sweep.Run(t.Context(), now); err != nil || n != 0 {
				t.Fatalf("the pass after convergence: removed %d (%v)", n, err)
			}
			logged := map[string]int{}
			for _, record := range errorLogs(t, &logs) {
				logged[fmt.Sprint(record["family_id"])]++
			}
			if len(logged) != bad {
				t.Fatalf("%d families logged, want %d", len(logged), bad)
			}
			for _, id := range badFamilies {
				if logged[id] != 1 {
					t.Fatalf("family %s logged %d times", id, logged[id])
				}
			}
		})
	}
}

// The receipt quarantine expires and is capped like the purge's (final
// review R2-M4): a quarantined family is skipped for an hour, then tried
// (and, failing again, quarantined and logged) again; and the quarantine
// holds at most ReceiptQuarantineCap families, the oldest evicted (logged
// at Warn) to make room.
func TestReceiptQuarantineExpiresAndIsCapped(t *testing.T) {
	t.Run("cap and expiry", func(t *testing.T) {
		logs := &records{}
		sweep := &maintenance.ReceiptSweep{Logger: slog.New(logs)}
		sweep.SetQuarantineCap(2)
		at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
		const a, b, c = "a0000000-0000-4000-8000-000000000002", "b0000000-0000-4000-8000-000000000002", "c0000000-0000-4000-8000-000000000002"
		sweep.Quarantine(at, a, "release_below_zero")
		sweep.Quarantine(at.Add(time.Minute), b, "undecodable_outcome")
		sweep.Quarantine(at.Add(time.Minute), b, "undecodable_outcome") // known: neither moved nor logged again
		if got := sortedIDs(sweep.Quarantined(at.Add(2 * time.Minute))); !slices.Equal(got, []string{a, b}) {
			t.Fatalf("quarantine %v, want a and b", got)
		}
		sweep.Quarantine(at.Add(3*time.Minute), c, "release_below_zero")
		if got := sortedIDs(sweep.Quarantined(at.Add(4 * time.Minute))); !slices.Equal(got, []string{b, c}) {
			t.Fatalf("quarantine at its cap %v, want the oldest (a) evicted", got)
		}
		var warned []slog.Record
		for _, record := range logs.at(slog.LevelWarn) {
			if record.Level == slog.LevelWarn {
				warned = append(warned, record)
			}
		}
		if len(warned) != 1 || attr(warned[0], "family_id") != a || len(logs.at(slog.LevelError)) != 3 {
			t.Fatalf("logs: %d warnings %+v and %d errors, want one warning naming a and three quarantines", len(warned), warned, len(logs.at(slog.LevelError)))
		}
		if got := sortedIDs(sweep.Quarantined(at.Add(time.Minute + maintenance.ReceiptQuarantineTTL - time.Microsecond))); !slices.Equal(got, []string{b, c}) {
			t.Fatalf("a microsecond short of b's hour: %v", got)
		}
		if got := sortedIDs(sweep.Quarantined(at.Add(time.Minute + maintenance.ReceiptQuarantineTTL))); !slices.Equal(got, []string{c}) {
			t.Fatalf("an hour after b's quarantine: %v, want only c", got)
		}
		if got := sweep.Quarantined(at.Add(3*time.Minute + maintenance.ReceiptQuarantineTTL)); len(got) != 0 {
			t.Fatalf("an hour after c's quarantine: %v, want none", got)
		}
		if maintenance.ReceiptQuarantineTTL != time.Hour || maintenance.ReceiptQuarantineCap != 1000 {
			t.Fatalf("quarantine TTL %v and cap %d, want an hour and 1000 (as the purge's)", maintenance.ReceiptQuarantineTTL, maintenance.ReceiptQuarantineCap)
		}
	})
	t.Run("a failing family is tried again after an hour", func(t *testing.T) {
		h := testkit.New(t)
		accepted := h.Now
		caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Feed(caretest.BabyA, 1))
		h.ExecFamily(t, `UPDATE families SET care_charged_bytes = 1 WHERE id = $1`, h.FamilyID)
		receipts := familyReceipts(t, h, h.FamilyID)
		var logs bytes.Buffer
		sweep := &maintenance.ReceiptSweep{Pool: h.Maintenance, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
		now := accepted.Add(maintenance.ReceiptRetention)
		for _, at := range []time.Duration{0, time.Minute, maintenance.ReceiptQuarantineTTL - time.Microsecond} {
			if n, err := sweep.Run(t.Context(), now.Add(at)); err != nil || n != 0 {
				t.Fatalf("at +%v: removed %d (%v)", at, n, err)
			}
			if records := errorLogs(t, &logs); len(records) != 1 {
				t.Fatalf("at +%v: %d error logs, want the one quarantine", at, len(records))
			}
		}
		if _, err := sweep.Run(t.Context(), now.Add(maintenance.ReceiptQuarantineTTL)); err != nil {
			t.Fatal(err)
		}
		if records := errorLogs(t, &logs); len(records) != 2 || records[1]["family_id"] != h.FamilyID || records[1]["cause"] != "release_below_zero" {
			t.Fatalf("after an hour: error logs %v, want the family tried and quarantined again", records)
		}
		if left := familyReceipts(t, h, h.FamilyID); len(left) != len(receipts) || familyCharged(t, h, h.FamilyID) != 1 {
			t.Fatalf("the failing family: %d of %d receipts left, charged %d", len(left), len(receipts), familyCharged(t, h, h.FamilyID))
		}
	})
}

// A writer holding one family's row costs the sweep that family only: the
// other family's due receipts go at once, their charge released to that
// family; the held family's go once it is free. Each family's counters stay
// exact throughout.
func TestReceiptSweepSkipsOnlyTheHeldFamily(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Feed(caretest.BabyA, 1), caretest.Feed(caretest.BabyA, 2))
	created, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "B")
	if err != nil {
		t.Fatal(err)
	}
	outsider := h.Refresh("outsider")
	other := created.ID
	command := care.BatchCommand{Grant: access.Grant{AccountID: outsider.AccountID, FamilyID: other, Role: "owner"}, DeviceID: outsider.DeviceID,
		Mutations: []protocol.Mutation{caretest.Baby(caretest.BabyB), caretest.Feed(caretest.BabyB, 1), caretest.Feed(caretest.BabyB, 2),
			caretest.Feed(caretest.BabyB, 3)},
		AcceptedAt: h.Now, Server: h.Info, Limits: h.Limits, Policy: caretest.Policy}
	tx := caretest.TenantTx(t, h.Runtime, outsider.AccountID, other)
	if result, err := care.ApplyBatch(t.Context(), tx, command, nil); err != nil || result.Fatal != nil || len(result.Outcomes) != 4 {
		t.Fatalf("family B's batch: %+v %v", result, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	ours, theirs := len(familyReceipts(t, h, h.FamilyID)), len(familyReceipts(t, h, other))
	if ours != 3 || theirs != 4 {
		t.Fatalf("receipts %d and %d", ours, theirs)
	}
	holder := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, err := care.LockFamily(t.Context(), holder, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	sweep := &maintenance.ReceiptSweep{Pool: h.Maintenance}
	now := h.Now.Add(maintenance.ReceiptRetention)
	start := time.Now()
	if n, err := sweep.Run(t.Context(), now); err != nil || n != theirs || time.Since(start) > time.Second {
		t.Fatalf("beside a writer on the harness family: removed %d (%v) after %v, want family B's %d at once", n, err, time.Since(start), theirs)
	}
	if left := len(familyReceipts(t, h, h.FamilyID)); left != ours {
		t.Fatalf("%d of the held family's %d receipts left", left, ours)
	}
	caretest.AssertCounters(t, h, other)
	_ = holder.Rollback(t.Context())
	caretest.AssertCounters(t, h, h.FamilyID)
	// Both families due at once, in one batch: each family is released its own charge.
	caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{caretest.Feed(caretest.BabyA, 3)})
	tx = caretest.TenantTx(t, h.Runtime, outsider.AccountID, other)
	command.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyB, 4), caretest.Feed(caretest.BabyB, 5)}
	if result, err := care.ApplyBatch(t.Context(), tx, command, nil); err != nil || result.Fatal != nil {
		t.Fatalf("family B's second batch: %+v %v", result, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := sweep.Run(t.Context(), now); err != nil || n != ours+1+2 {
		t.Fatalf("both families free: removed %d (%v), want %d", n, err, ours+1+2)
	}
	caretest.AssertCounters(t, h, h.FamilyID)
	caretest.AssertCounters(t, h, other)
}

// The family selection reads care_receipts_retention_idx, and the delete
// chooses its rows through it too and deletes them by address (a TID scan),
// with no sequential scan of care_receipts, under both custom and generic
// plans: 20,000 receipts across 50 families, 1% due.
func TestReceiptSweepPlanUsesTheRetentionIndex(t *testing.T) {
	h := testkit.New(t)
	h.AuthorityExec(t, `INSERT INTO families(id, display_name, owner_account_id) SELECT md5('family'||i)::uuid, 'Family '||i, $1
		FROM generate_series(1, 49) i`, h.Owner.AccountID)
	families := []string{h.FamilyID}
	for i := 1; i <= 49; i++ {
		var id string
		if err := h.Maintenance.QueryRow(t.Context(), `SELECT md5('family'||$1::int)::uuid::text`, i).Scan(&id); err != nil {
			t.Fatal(err)
		}
		families = append(families, id)
	}
	base := h.Now
	for f, family := range families {
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, family)
		if _, err := tx.Exec(t.Context(), `INSERT INTO care_receipts(device_id, mutation_id, family_id, account_id, semantic_hash, outcome, accepted_at)
			SELECT $1, md5($2::int::text||'/'||i)::uuid, $3, $4, sha256(i::text::bytea), '{"status":"applied"}',
				$5::timestamptz - CASE WHEN i % 100 = 0 THEN interval '31 days' ELSE i * interval '1 second' END
			FROM generate_series(1, 400) i`, h.Owner.DeviceID, f, family, h.Owner.AccountID, base); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	analyze(t, h, "care_receipts", "families")
	cutoff := base.Add(-maintenance.ReceiptRetention)
	for _, c := range []struct {
		name, sql, index string
		args             []any
		tid              bool
	}{
		{"families", maintenance.ReceiptSweepSQL["families"], "care_receipts_retention_idx", []any{cutoff, maintenance.ReceiptBatch, []string{}}, false},
		{"delete", maintenance.ReceiptSweepSQL["delete"], "care_receipts_retention_idx", []any{cutoff, maintenance.ReceiptBatch, families}, true},
	} {
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				nodes, raw := planNodes(t, h, mode, c.sql, c.args)
				if nodes.has("Seq Scan", "care_receipts", "") {
					t.Fatalf("a sequential scan of care_receipts: %s", raw)
				}
				if !nodes.has("", "", c.index) || (c.tid && !nodes.has("Tid Scan", "care_receipts", "")) {
					t.Fatalf("the plan does not read %s (TID scan wanted: %v): %s", c.index, c.tid, raw)
				}
				// The delete rechecks the selection's predicates on the row it deletes.
				if filter := nodes.tidFilter("care_receipts"); c.tid && (!strings.Contains(filter, "family_id") || !strings.Contains(filter, "accepted_at")) {
					t.Fatalf("the delete's TID scan filter %q does not repeat the selection's predicates", filter)
				}
			})
		}
	}
}

// analyze refreshes the planner statistics of tables (as their owner).
func analyze(t *testing.T, h *testkit.Harness, tables ...string) {
	t.Helper()
	for _, table := range tables {
		if _, err := h.Migrator.Exec(t.Context(), `ANALYZE `+table); err != nil {
			t.Fatal(err)
		}
	}
}

// planNodes is every node of EXPLAIN of statement, as the maintenance
// login under plan cache mode, and the raw plan.
func planNodes(t *testing.T, h *testkit.Harness, mode, statement string, args []any) (plan, string) {
	t.Helper()
	raw := explain(t, h, mode, statement, args)
	var plans []struct{ Plan planNode }
	if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
		t.Fatalf("plan %s: %v", raw, err)
	}
	var nodes plan
	var visit func(planNode)
	visit = func(n planNode) {
		nodes = append(nodes, n)
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	return nodes, raw
}

// plan is a plan's nodes.
type plan []planNode

// tidFilter is the filter of the TID scan on relation ("" without one).
func (p plan) tidFilter(relation string) string {
	for _, n := range p {
		if n.NodeType == "Tid Scan" && n.RelationName == relation {
			return n.Filter
		}
	}
	return ""
}

// has reports a node of nodeType on relation reading index (each "" for
// any).
func (p plan) has(nodeType, relation, index string) bool {
	for _, n := range p {
		if (nodeType == "" || n.NodeType == nodeType) && (relation == "" || n.RelationName == relation) && (index == "" || n.IndexName == index) {
			return true
		}
	}
	return false
}

// explain is EXPLAIN (FORMAT JSON) of statement as the maintenance login
// under plan cache mode.
func explain(t *testing.T, h *testkit.Harness, mode, statement string, args []any) string {
	t.Helper()
	tx, err := h.Maintenance.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `SELECT set_config('plan_cache_mode', $1, true)`, mode); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := tx.QueryRow(t.Context(), `EXPLAIN (FORMAT JSON) `+statement, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

type planNode struct {
	NodeType     string     `json:"Node Type"`
	Filter       string     `json:"Filter"`
	RelationName string     `json:"Relation Name"`
	IndexName    string     `json:"Index Name"`
	Plans        []planNode `json:"Plans"`
}

// quietSender discards wakes.
type quietSender struct{}

func (quietSender) Wake(context.Context, []wake.Wake) error { return nil }

// familyRows counts table's rows of the family as the maintenance login.
func familyRows(t *testing.T, h *testkit.Harness, table string) int {
	t.Helper()
	var n int
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE family_id = $1`, h.FamilyID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A pull of page size 1 issues a binding, a snapshot a session (and its
// binding), and a flushed Synced a device cursor. An hour past expires_at,
// less a microsecond, nothing is deleted and a replay still answers
// page_expired; at expires_at + 1 h both go and the replay answers
// invalid_page_token; unexpired rows survive. The device cursor goes at
// seen_at + 35 days and not a second before.
func TestExpirySweepsDeleteOnlyWhatIsPastTheGrace(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.Limits.SyncPageTTLSeconds = 60
	h.Limits.SnapshotTTLSeconds = 60
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 2)
	issued := h.Now
	pull := h.Pull(t, h.Owner, h.Cursor(0), nil)
	snapshot, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if err != nil || pull.NextPageToken == nil || snapshot.NextPageToken == nil {
		t.Fatalf("pull token %v, snapshot token %v (%v)", pull.NextPageToken, snapshot.NextPageToken, err)
	}
	tracker := wake.NewTracker(0)
	tracker.Synced(h.FamilyID, h.Owner.DeviceID, 1, 2, issued)
	worker := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: quietSender{}, Now: func() time.Time { return issued }}
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b, s, c := familyRows(t, h, "sync_page_bindings"), familyRows(t, h, "sync_snapshot_sessions"), familyRows(t, h, "device_sync_cursors"); b != 2 || s != 1 || c != 1 {
		t.Fatalf("%d bindings, %d sessions, %d cursors; want the pull's and the snapshot's bindings, one session and one cursor", b, s, c)
	}
	expires := issued.Add(60 * time.Second)
	sweeps := []*maintenance.ExpirySweep{{Pool: h.Maintenance, Table: "sync_page_bindings"}, {Pool: h.Maintenance, Table: "sync_snapshot_sessions"},
		{Pool: h.Maintenance, Table: "device_sync_cursors"}}
	runAll := func(now time.Time) map[string]int {
		counts := map[string]int{}
		for _, sweep := range sweeps {
			n, err := sweep.Run(t.Context(), now)
			if err != nil {
				t.Fatalf("%s: %v", sweep.Name(), err)
			}
			counts[sweep.Name()] = n
		}
		return counts
	}
	for _, at := range []time.Duration{59 * time.Minute, maintenance.PageGrace - time.Microsecond} {
		h.Now = expires.Add(at)
		if counts := runAll(h.Now); counts["sync_page_bindings"]+counts["sync_snapshot_sessions"]+counts["device_sync_cursors"] != 0 {
			t.Fatalf("at expires_at + %v the sweeps deleted %v", at, counts)
		}
		if _, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, pull.NextPageToken)); !errors.Is(err, familysync.ErrPageExpired) {
			t.Fatalf("pull replay at expires_at + %v: %v, want page_expired", at, err)
		}
		if _, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{PageToken: snapshot.NextPageToken}); !errors.Is(err, familysync.ErrPageExpired) {
			t.Fatalf("snapshot replay at expires_at + %v: %v, want page_expired", at, err)
		}
	}
	// Exactly an hour past expiry: the expired binding and session go; a
	// fresh pull's binding, a fresh snapshot's and its session stay.
	h.Now = expires.Add(maintenance.PageGrace)
	fresh := h.Pull(t, h.Owner, h.Cursor(0), nil)
	freshSnapshot, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if err != nil || fresh.NextPageToken == nil || freshSnapshot.NextPageToken == nil {
		t.Fatalf("fresh tokens %v %v (%v)", fresh.NextPageToken, freshSnapshot.NextPageToken, err)
	}
	if counts := runAll(h.Now); counts["sync_page_bindings"] != 2 || counts["sync_snapshot_sessions"] != 1 || counts["device_sync_cursors"] != 0 {
		t.Fatalf("at expires_at + 1 h the sweeps deleted %v, want both expired bindings and the session", counts)
	}
	if _, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, pull.NextPageToken)); !errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("pull replay after the sweep: %v, want invalid_page_token", err)
	}
	if _, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{PageToken: snapshot.NextPageToken}); !errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("snapshot replay after the sweep: %v, want invalid_page_token", err)
	}
	if _, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, fresh.NextPageToken)); err != nil {
		t.Fatalf("the unexpired continuation after the sweep: %v", err)
	}
	if _, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{PageToken: freshSnapshot.NextPageToken}); err != nil {
		t.Fatalf("the unexpired snapshot continuation after the sweep: %v", err)
	}
	if b, s := familyRows(t, h, "sync_page_bindings"), familyRows(t, h, "sync_snapshot_sessions"); b != 2 || s != 1 {
		t.Fatalf("%d bindings and %d sessions survive, want the fresh ones", b, s)
	}

	// Device cursors: 35 days unseen. Another device seen since stays.
	tracker.Synced(h.FamilyID, h.Caregiver.DeviceID, 1, 2, issued.Add(30*24*time.Hour))
	worker.Now = func() time.Time { return issued.Add(30 * 24 * time.Hour) }
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := runAll(issued.Add(maintenance.DeviceCursorRetention - time.Second))["device_sync_cursors"]; n != 0 {
		t.Fatalf("a cursor unseen one second short of 35 days was deleted (%d)", n)
	}
	if n := runAll(issued.Add(35 * 24 * time.Hour))["device_sync_cursors"]; n != 1 || familyRows(t, h, "device_sync_cursors") != 1 {
		t.Fatalf("at 35 days unseen %d cursors deleted, %d left; want the owner's gone, the caregiver's kept", n, familyRows(t, h, "device_sync_cursors"))
	}
}

// seedExpiry seeds rows rows of table in the harness family, duePercent of
// every hundred past their retention at the harness clock (bindings and
// sessions expired two hours ago, cursors unseen for 40 days), the rest
// live.
func seedExpiry(t *testing.T, h *testkit.Harness, table string, rows int, duePercent int) {
	t.Helper()
	switch table {
	case "sync_page_bindings":
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
		if _, err := tx.Exec(t.Context(), `INSERT INTO sync_page_bindings(family_id, token_digest, token_key_id, account_id, device_id, session_id,
				server_instance_id, recovery_epoch, security_revision, kind, high_water_sequence, resume_sequence, item_limit, byte_limit, created_at, expires_at)
			SELECT $1, sha256(('b'||i)::bytea), 'page.v1', $2, $3, $3, $3, 1, 1, 'sync'::app.page_kind, 0, 0, 1, 65536, e - interval '15 minutes', e
			FROM generate_series(1, $4::int) i,
				LATERAL (SELECT $5::timestamptz + CASE WHEN i % 100 < $6 THEN -interval '2 hours' - i * interval '1 millisecond' ELSE interval '10 minutes' + i * interval '1 millisecond' END) x(e)`,
			h.FamilyID, h.Owner.AccountID, h.Owner.DeviceID, rows, h.Now, duePercent); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	case "sync_snapshot_sessions":
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
		if _, err := tx.Exec(t.Context(), `INSERT INTO sync_snapshot_sessions(family_id, id, account_id, device_id, session_id, server_instance_id,
				recovery_epoch, security_revision, high_water_sequence, captured_at, expires_at)
			SELECT $1, md5('s'||i)::uuid, $2, $3, $3, $3, 1, 1, 0, e - interval '15 minutes', e
			FROM generate_series(1, $4::int) i,
				LATERAL (SELECT $5::timestamptz + CASE WHEN i % 100 < $6 THEN -interval '2 hours' - i * interval '1 millisecond' ELSE interval '10 minutes' + i * interval '1 millisecond' END) x(e)`,
			h.FamilyID, h.Owner.AccountID, h.Owner.DeviceID, rows, h.Now, duePercent); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	case "device_sync_cursors":
		if _, err := h.Maintenance.Exec(t.Context(), `INSERT INTO device_sync_cursors(family_id, device_id, acked_sequence, delivered_sequence, seen_at)
			SELECT $1, md5('d'||i)::uuid, 0, 0,
				$3::timestamptz - CASE WHEN i % 100 < $4 THEN interval '40 days' + i * interval '1 second' ELSE i * interval '1 second' END
			FROM generate_series(1, $2::int) i`, h.FamilyID, rows, h.Now, duePercent); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("no seed for %s", table)
	}
}

var expiryTables = []string{"sync_page_bindings", "sync_snapshot_sessions", "device_sync_cursors"}

// Crash safety: a sweep cancelled after its delete and before COMMIT
// deletes nothing, and the next run deletes the same rows. Two sweeps of the
// same table running beside each other delete each row once, without error.
func TestExpirySweepsAreCrashSafeAndRunBesideThemselves(t *testing.T) {
	h := testkit.New(t)
	for _, table := range expiryTables {
		t.Run(table, func(t *testing.T) {
			seedExpiry(t, h, table, 3000, 100)
			ctx, cancel := context.WithCancel(t.Context())
			cancelled := &maintenance.ExpirySweep{Pool: h.Maintenance, Table: table}
			cancelled.AfterDelete(cancel)
			if _, err := cancelled.Run(ctx, h.Now); err == nil {
				t.Fatal("a cancelled batch reported success")
			}
			if n := familyRows(t, h, table); n != 3000 {
				t.Fatalf("%d of 3,000 rows left after the cancelled batch", n)
			}
			if table == "device_sync_cursors" {
				// A cursor the wake worker holds (its flush or mark) is
				// skipped, never waited on; it goes once released.
				held, err := h.Maintenance.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := held.Exec(t.Context(), `SELECT 1 FROM device_sync_cursors WHERE family_id = $1 ORDER BY seen_at LIMIT 1 FOR UPDATE`, h.FamilyID); err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				n, err := (&maintenance.ExpirySweep{Pool: h.Maintenance, Table: table}).Run(t.Context(), h.Now)
				_ = held.Rollback(t.Context())
				if err != nil || n != 2999 || time.Since(start) > time.Second {
					t.Fatalf("beside a held cursor: removed %d (%v) after %v, want the 2,999 free ones at once", n, err, time.Since(start))
				}
				seedExpiry(t, h, table, 2999, 100) // the same 3,000 again (the held one is still there)
				if n := familyRows(t, h, table); n != 3000 {
					t.Fatalf("%d rows after reseeding", n)
				}
			}
			var wg sync.WaitGroup
			counts := make([]int, 2)
			errs := make([]error, 2)
			for i := range 2 {
				sweep := &maintenance.ExpirySweep{Pool: h.Maintenance, Table: table}
				wg.Go(func() {
					for {
						n, err := sweep.Run(t.Context(), h.Now)
						counts[i] += n
						if err != nil {
							errs[i] = err
							return
						}
						if n == 0 {
							return
						}
					}
				})
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				t.Fatal(err)
			}
			if counts[0]+counts[1] != 3000 || familyRows(t, h, table) != 0 {
				t.Fatalf("the two sweeps deleted %d + %d of 3,000 rows, %d left", counts[0], counts[1], familyRows(t, h, table))
			}
		})
	}
}

// Each statement deletes at most ExpiryBatch rows, and a pass re-runs full
// batches up to ExpiryPassCap: 2,500 due rows go in batches of 1,000, 1,000
// and 500; of 6,000, one pass takes 5,000 and the next the rest.
func TestExpirySweepsAreBounded(t *testing.T) {
	h := testkit.New(t)
	for _, table := range expiryTables {
		t.Run(table, func(t *testing.T) {
			seedExpiry(t, h, table, 2500, 100)
			sweep := &maintenance.ExpirySweep{Pool: h.Maintenance, Table: table}
			// Inside each batch, before its commit, another connection sees
			// what the earlier batches deleted.
			var seen []int
			sweep.AfterDelete(func() { seen = append(seen, 2500-familyRows(t, h, table)) })
			if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != 2500 {
				t.Fatalf("removed %d (%v), want 2,500", n, err)
			}
			if fmt.Sprint(seen) != "[0 1000 2000]" {
				t.Fatalf("rows deleted before each batch %v, want batches of 1,000, 1,000 and 500", seen)
			}
			sweep.AfterDelete(nil)
			seedExpiry(t, h, table, 6000, 100)
			if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != maintenance.ExpiryPassCap {
				t.Fatalf("a 6,000-row backlog: one pass removed %d (%v), want the %d cap", n, err, maintenance.ExpiryPassCap)
			}
			if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != 1000 {
				t.Fatalf("the next pass removed %d (%v), want the remaining 1,000", n, err)
			}
		})
	}
}

// Each expiry statement is one bounded delete that removes its rows by
// address (a TID scan, never a sequential scan of the table on the delete
// side): on 20,000 rows (1% due) the bindings and cursors are chosen through
// their expiry indexes with no sequential scan at all, under custom and
// generic plans. The snapshot-session selection scans its table by design
// (000031 has no expiry index there).
func TestExpirySweepPlansUseTheirIndexes(t *testing.T) {
	h := testkit.New(t)
	for _, table := range expiryTables {
		seedExpiry(t, h, table, 20000, 1)
	}
	analyze(t, h, expiryTables...)
	for _, c := range []struct{ table, index string }{
		{"sync_page_bindings", "sync_page_bindings_expiry_idx"},
		{"sync_snapshot_sessions", ""},
		{"device_sync_cursors", "device_sync_cursors_seen_idx"},
	} {
		cutoff := h.Now.Add(-maintenance.PageGrace)
		if c.table == "device_sync_cursors" {
			cutoff = h.Now.Add(-maintenance.DeviceCursorRetention)
		}
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			t.Run(c.table+"/"+mode, func(t *testing.T) {
				nodes, raw := planNodes(t, h, mode, maintenance.ExpirySQL(c.table), []any{cutoff, maintenance.ExpiryBatch})
				if !nodes.has("Tid Scan", c.table, "") {
					t.Fatalf("the delete does not remove its rows by address: %s", raw)
				}
				column := "expires_at"
				if c.table == "device_sync_cursors" {
					column = "seen_at"
				}
				if filter := nodes.tidFilter(c.table); !strings.Contains(filter, column) {
					t.Fatalf("the delete's TID scan filter %q does not repeat the selection's %s predicate", filter, column)
				}
				if c.index == "" {
					return
				}
				if nodes.has("Seq Scan", c.table, "") || !nodes.has("", c.table, c.index) {
					t.Fatalf("the selection does not read %s alone: %s", c.index, raw)
				}
			})
		}
	}
	// Every sweep deletes exactly its due 1%.
	for _, table := range expiryTables {
		sweep := &maintenance.ExpirySweep{Pool: h.Maintenance, Table: table}
		if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != 200 {
			t.Fatalf("%s sweep deleted %d (%v), want the 200 due", table, n, err)
		}
	}
}

// Each sweep on the maintenance pool holds a worker slot for each batch:
// with every slot taken it waits (and gives up with its context) without
// touching the pool, and it runs once a slot frees.
func TestCareSweepsRunInsideAWorkerSlot(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	now := h.Now.Add(maintenance.ReceiptRetention)
	for _, table := range []string{"care_receipts", "sync_page_bindings", "sync_snapshot_sessions", "device_sync_cursors"} {
		t.Run(table, func(t *testing.T) {
			pool, err := database.OpenWithMax(t.Context(), h.Maintenance.Config().ConnString(), 2)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			slots, err := database.NewWorkerSlots(pool)
			if err != nil {
				t.Fatal(err)
			}
			var sweep maintenance.Sweep = &maintenance.ExpirySweep{Pool: pool, Table: table, Slots: slots}
			if table == "care_receipts" {
				sweep = &maintenance.ReceiptSweep{Pool: pool, Slots: slots}
			}
			release, err := slots.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			if _, err := sweep.Run(ctx, now); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("with every slot held: %v, want to wait for one", err)
			}
			if acquired := pool.Stat().AcquireCount(); acquired != 0 {
				t.Fatalf("the sweep took %d pool connections outside a slot", acquired)
			}
			release()
			if _, err := sweep.Run(t.Context(), now); err != nil || pool.Stat().AcquireCount() == 0 {
				t.Fatalf("with a free slot: %v after %d acquisitions", err, pool.Stat().AcquireCount())
			}
		})
	}
	if n := len(familyReceipts(t, h, h.FamilyID)); n != 0 {
		t.Fatalf("%d receipts left after the slotted receipt sweep", n)
	}
}

// Every table the sweeps delete from by ctid is a plain table. A ctid names a
// row only within one relation: were one of these tables partitioned, a ctid
// delete would also remove the rows at the same address in the other
// partitions, and the deletes would need (tableoid, ctid) pairs.
func TestCtidDeletesTargetPlainTables(t *testing.T) {
	h := testkit.New(t)
	for _, table := range append([]string{"care_receipts"}, expiryTables...) {
		var kind string
		if err := h.Maintenance.QueryRow(t.Context(), `SELECT relkind::text FROM pg_class WHERE oid = to_regclass('public.' || $1)`, table).Scan(&kind); err != nil || kind != "r" {
			t.Fatalf("%s has relkind %q (%v), want a plain table ('r')", table, kind, err)
		}
	}
}
