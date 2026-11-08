package maintenance_test

import (
	"log/slog"
	"strconv"
	"testing"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/maintenance"
	"babytracker/server/internal/testkit"
)

// The harness clock trails the database clock by less than a day, and the
// sweeps below run a window or more ahead of it, so a sweep that compared
// these app-stamped columns with the database's now() would keep what the
// steps below require deleted.

func TestSessionSweepKeepsConsumedSessionsForTwentyFourHours(t *testing.T) {
	h := testkit.New(t)
	old := h.RefreshToken("owner")
	consumedAt := h.Now          // the harness clock is fixed: the setup refreshes consumed their parents at this time too
	h.Owner = h.Refresh("owner") // consumes old's session; the owner continues on the child
	sweep := &maintenance.SessionSweep{Authority: h.Authority}
	before := consumedSessions(t, h)
	if before == 0 {
		t.Fatal("no consumed session to prune")
	}
	// The window is the owner's 24 hours (decision D2), pinned literally.
	const window = 24 * time.Hour
	if maintenance.ConsumedSessionRetention != window {
		t.Fatalf("consumed-session window %v, want decision D2's 24 h", maintenance.ConsumedSessionRetention)
	}
	if _, err := sweep.Run(t.Context(), consumedAt.Add(window-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := consumedSessions(t, h); got != before {
		t.Fatalf("a consumed session younger than 24 h was pruned: %d -> %d", before, got)
	}
	// Exactly 24 hours old is still inside the window.
	if _, err := sweep.Run(t.Context(), consumedAt.Add(window)); err != nil {
		t.Fatal(err)
	}
	if got := consumedSessions(t, h); got != before {
		t.Fatalf("a consumed session exactly 24 h old was pruned: %d -> %d", before, got)
	}
	if _, err := sweep.Run(t.Context(), consumedAt.Add(window+time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := consumedSessions(t, h); got != 0 {
		t.Fatalf("%d consumed sessions older than 24 h survived", got)
	}
	// The accepted trade-off: past the window a replay is refused, and the owner's lineage lives on.
	if _, err := h.Auth.Refresh(t.Context(), old); auth.Code(err) != "session_revoked" {
		t.Fatalf("replay of a pruned consumed token: %v, want session_revoked", err)
	}
	_ = h.Pull(t, h.Owner, h.Cursor(0), nil) // the current session still works
}

// A lost-acknowledgement retry keeps working past the 24-hour window for as
// long as its receipt lives: the sweep keeps the consumed parent the receipt
// names. When the receipt falls due, one pass deletes the receipt and then
// the parent it no longer protects.
func TestSessionSweepKeepsWhatARefreshRetryNeeds(t *testing.T) {
	h := testkit.New(t)
	// The owner's lineage started 355 days before the harness clock, so its
	// self-hosted year ends ten days after it: the receipt falls due then.
	// The database clock is less than a day ahead of the harness's, so a
	// sweep comparing with it keeps the receipt at the "deleted" step below.
	lineageStarted := h.Now.Add(-355 * 24 * time.Hour)
	h.AuthorityExec(t, `UPDATE sessions SET lineage_started_at=$2 WHERE id=$1`, h.Owner.SessionID, lineageStarted)
	request := auth.RecoverableRefreshInput{RefreshToken: h.RefreshToken("owner"), AttemptID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		DeviceContinuity: identity.DeviceContinuityClaim{DeviceID: h.Owner.DeviceID, Credential: h.DeviceCredential("owner")}}
	first, err := h.Auth.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	sweep := &maintenance.SessionSweep{Authority: h.Authority}
	if _, err := sweep.Run(t.Context(), h.Now.Add(maintenance.ConsumedSessionRetention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	retry, err := h.Auth.RefreshRecoverable(t.Context(), request)
	if err != nil || retry.RefreshToken != first.RefreshToken || retry.AccessToken != first.AccessToken {
		t.Fatalf("the lost-ack retry after a sweep: %v", err)
	}
	due := receiptDue(t, h)
	if want := lineageStarted.Add(365 * 24 * time.Hour); !due.Equal(want) {
		t.Fatalf("the receipt falls due %s, want %s (the lineage's year)", due, want)
	}
	if _, err := sweep.Run(t.Context(), due.Add(-time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	if n := authorityCount(t, h, `SELECT count(*) FROM refresh_receipts`); n != 1 {
		t.Fatalf("%d receipts left a microsecond before the receipt fell due", n)
	}
	if _, err := sweep.Run(t.Context(), due); err != nil {
		t.Fatal(err)
	}
	if n := authorityCount(t, h, `SELECT count(*) FROM refresh_receipts`); n != 0 {
		t.Fatal("the due receipt survived")
	}
	if n := consumedSessions(t, h); n != 0 {
		t.Fatalf("%d consumed sessions survived the pass that deleted their receipt", n)
	}
	if _, err := h.Auth.RefreshRecoverable(t.Context(), request); auth.Code(err) != "session_revoked" {
		t.Fatalf("a retry after its receipt and parent went: %v, want session_revoked", err)
	}
}

// The sweep deletes sign-in challenges at their own expiry (now, not now
// minus the session window).
func TestSessionSweepDeletesExpiredChallenges(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Identity.Challenge(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := authorityCount(t, h, `SELECT count(*) FROM auth_challenges`)
	if before == 0 {
		t.Fatal("no challenge to prune")
	}
	expiry := h.Now.Add(5 * time.Minute) // every harness challenge was issued at h.Now
	sweep := &maintenance.SessionSweep{Authority: h.Authority}
	if _, err := sweep.Run(t.Context(), expiry.Add(-time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	if got := authorityCount(t, h, `SELECT count(*) FROM auth_challenges`); got != before {
		t.Fatalf("challenges pruned before their expiry: %d -> %d", before, got)
	}
	n, err := sweep.Run(t.Context(), expiry)
	if err != nil {
		t.Fatal(err)
	}
	if got := authorityCount(t, h, `SELECT count(*) FROM auth_challenges`); got != 0 || n < before {
		t.Fatalf("%d challenges left at their expiry (sweep reported %d)", got, n)
	}
}

// A backlog drains in batches of 1,000 per statement, re-run while full, up
// to the pass's cap; the pass that reaches the cap logs the backlog at Info
// and the next pass continues.
func TestSessionSweepDrainsABacklogInBoundedBatches(t *testing.T) {
	h := testkit.New(t)
	h.AuthorityExec(t, `INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,lineage_started_at,scope,recovery_epoch,issued_seq,
			token_key_id,refresh_digest,access_expires_at,refresh_expires_at,deadline_at,status,consumed_at,child_id,created_at)
		SELECT sha256(('backlog-a'||i)::bytea), md5('backlog'||i)::uuid, $1, $2, md5('backlog'||i)::uuid, $3::timestamptz, 'account', 1, 1, 'session.v1',
			sha256(('backlog-r'||i)::bytea), $3::timestamptz + interval '1 hour', $3::timestamptz + interval '30 days', $3::timestamptz + interval '365 days',
			'consumed', $3::timestamptz, md5('backlog-c'||i)::uuid, $3::timestamptz
		FROM generate_series(1, 16000) i`, h.Owner.AccountID, h.Owner.DeviceID, h.Now)
	before := consumedSessions(t, h)
	log := &records{}
	sweep := &maintenance.SessionSweep{Authority: h.Authority, Logger: slog.New(log)}
	later := h.Now.Add(maintenance.ConsumedSessionRetention + time.Hour)
	if _, err := sweep.Run(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	if got := before - consumedSessions(t, h); got != maintenance.SessionPassCap {
		t.Fatalf("one pass removed %d consumed sessions, want the %d cap", got, maintenance.SessionPassCap)
	}
	backlog := named(log.at(slog.LevelInfo), backlogMessage)
	if len(backlog) != 1 || attr(backlog[0], "kind") != "sessions" {
		t.Fatalf("logged %v at Info, want one backlog record for sessions", backlog)
	}
	if _, err := sweep.Run(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	if got := consumedSessions(t, h); got != 0 {
		t.Fatalf("%d consumed sessions left after the second pass", got)
	}
	if n := len(named(log.at(slog.LevelInfo), backlogMessage)); n != 1 {
		t.Fatalf("%d backlog records after a pass under the cap, want still 1", n)
	}
	// Each pass logged its line at Info (it removed rows): the rows per
	// kind, the duration and the batch sizes.
	passes := named(log.at(slog.LevelInfo), "session sweep pass")
	if len(passes) != 2 {
		t.Fatalf("pass lines %v, want one per pass", passes)
	}
	for i, want := range []string{strconv.Itoa(maintenance.SessionPassCap), strconv.Itoa(before - maintenance.SessionPassCap)} {
		if got := attr(passes[i], "sessions"); got != want {
			t.Fatalf("pass %d removed %s sessions, want %s", i+1, got, want)
		}
		if attr(passes[i], "sessions_batch") != "1000" || attr(passes[i], "duration_ms") == "" || attr(passes[i], "failed") != "false" {
			t.Fatalf("pass %d line %v", i+1, passes[i])
		}
	}
	// A pass with nothing to remove logs its line at Debug only.
	if _, err := sweep.Run(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	if n := len(named(log.at(slog.LevelInfo), "session sweep pass")); n != 2 {
		t.Fatalf("an idle pass logged at Info (%d pass lines)", n)
	}
	if idle := named(log.at(slog.LevelDebug), "session sweep pass"); len(idle) != 3 || idle[2].Level != slog.LevelDebug || attr(idle[2], "sessions") != "0" {
		t.Fatalf("pass lines %v, want the idle pass at Debug", idle)
	}
}

const backlogMessage = "retention backlog: the pass reached its cap; the next pass continues"

// named keeps the records with message.
func named(records []slog.Record, message string) []slog.Record {
	var out []slog.Record
	for _, record := range records {
		if record.Message == message {
			out = append(out, record)
		}
	}
	return out
}

// consumedSessions counts consumed sessions as an authority transaction does.
func consumedSessions(t *testing.T, h *testkit.Harness) int {
	t.Helper()
	return authorityCount(t, h, `SELECT count(*) FROM sessions WHERE status='consumed'`)
}

// authorityCount runs a count as the runtime inside an authority transaction.
func authorityCount(t *testing.T, h *testkit.Harness, query string, args ...any) int {
	t.Helper()
	tx, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// receiptDue is when the one refresh receipt falls due.
func receiptDue(t *testing.T, h *testkit.Harness) time.Time {
	t.Helper()
	tx, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	if err := tx.QueryRow(t.Context(), `SELECT LEAST(refresh_expires_at, effective_deadline) FROM refresh_receipts`).Scan(&due); err != nil {
		t.Fatal(err)
	}
	return due.UTC()
}
