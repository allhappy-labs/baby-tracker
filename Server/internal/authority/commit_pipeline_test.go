package authority_test

import (
	"errors"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A request transaction's CommitPipeline sends what run queued with the
// COMMIT, in its one round trip: the statements run just before it, read
// sees their results, and the transaction commits. A statement's error
// skips the COMMIT: the transaction rolls back, the error comes back as is
// (not as unavailable), and the connection goes back to the pool clean.
// read's own error comes back as is after the commit. A transaction that
// already wrote is refused in the server before the queued statements and
// the COMMIT run (ErrPipelineAfterWrite), so read can never refuse after a
// write of run's has committed. A transaction that rolls back sends
// nothing it queued.
func TestCommitPipelineSendsItsStatementsWithTheCommit(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	st := seated(t, f, owner, family, "owner")
	pool, w := countedPool(t, f, nil)
	s := serviceOn(t, f, pool, nil)
	ctx := t.Context()
	request := authority.Request{Digests: st.digests(f)}
	const advance = `UPDATE families SET next_sequence = next_sequence + 1 WHERE id = $1 RETURNING next_sequence`
	// fails (22012) when it runs, after the statements before it ran.
	const failing = `SELECT 1 / (random() < 0)::int FROM families WHERE id = $1`
	sequence := func() int64 {
		t.Helper()
		var n int64
		if err := s.WithRequestTx(ctx, request, "read", func(tx pgx.Tx, _ access.Principal, g access.Grant) error {
			return tx.QueryRow(ctx, `SELECT next_sequence FROM families WHERE id = $1`, g.FamilyID).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// pipelined runs one request whose run sends direct itself (if any),
	// then queues statements with the commit, and read reads them.
	pipelined := func(direct string, statements []string, read func(pgx.BatchResults) error, runErr error) (int, []string, int, error) {
		t.Helper()
		w.reset()
		err := s.WithRequestTx(ctx, request, "mutate_record", func(tx pgx.Tx, _ access.Principal, g access.Grant) error {
			if direct != "" {
				if _, err := tx.Exec(ctx, direct, g.FamilyID); err != nil {
					return err
				}
			}
			pipeline, ok := tx.(authority.CommitPipeline)
			if !ok {
				t.Fatalf("request transaction %T is no CommitPipeline", tx)
			}
			pipeline.PipelineWithCommit(func(b *pgx.Batch) {
				for _, statement := range statements {
					b.Queue(statement, g.FamilyID)
				}
			}, read)
			return runErr
		})
		trips, sent, batches := w.snapshot()
		return trips, sent, batches, err
	}
	readSequence := func(got *int64) func(pgx.BatchResults) error {
		return func(r pgx.BatchResults) error { return r.QueryRow().Scan(got) }
	}
	execAll := func(n int) func(pgx.BatchResults) error {
		return func(r pgx.BatchResults) error {
			for range n {
				if _, err := r.Exec(); err != nil {
					return err
				}
			}
			return nil
		}
	}

	// Warm the statements on the one connection (a failed batch drops its
	// statements from pgx's cache, so the failing one first).
	if _, _, _, err := pipelined("", []string{advance, failing}, execAll(2), nil); err == nil {
		t.Fatal("warm-up of the failing statement succeeded")
	}
	var warmed int64
	if _, _, _, err := pipelined("", []string{advance}, readSequence(&warmed), nil); err != nil {
		t.Fatal(err)
	}

	// Queued and committed: two round trips (BEGIN with the authority read,
	// then the read-only check, the statement and the COMMIT), two batches.
	before := sequence()
	var got int64
	trips, statements, batches, err := pipelined("", []string{advance}, readSequence(&got), nil)
	if err != nil || trips != 2 || batches != 2 || len(statements) != 5 || statementsMatching(statements[2:3], "pg_current_xact_id_if_assigned()") != 1 ||
		statements[3] != advance || statements[4] != "commit" {
		t.Fatalf("pipelined: err %v, %d round trips, %d batches, %q; want 2, 2, [begin, authority, check, statement, commit]", err, trips, batches, statements)
	}
	if got != before+1 || sequence() != before+1 {
		t.Fatalf("pipelined statement read %d, family at %d, want %d committed", got, sequence(), before+1)
	}

	// A statement's error: the COMMIT is skipped and the queued write
	// before it rolls back; the error is the statement's; the connection is
	// reused.
	before, conns := sequence(), pool.Stat().NewConnsCount()
	_, _, _, err = pipelined("", []string{advance, failing}, execAll(2), nil)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22012" || errors.Is(err, authority.ErrUnavailable) || errors.Is(err, authority.ErrPipelineAfterWrite) {
		t.Fatalf("failing pipelined statement: %v, want its own 22012", err)
	}
	if sequence() != before || pool.Stat().NewConnsCount() != conns {
		t.Fatalf("after a failing pipelined statement: family at %d (want %d), %d new connections (want %d)", sequence(), before,
			pool.Stat().NewConnsCount(), conns)
	}

	// After a write of run's own the pipeline is refused in the server:
	// neither the queued statements nor the COMMIT run, read is never
	// called, the write rolls back, the connection is reused.
	before, conns = sequence(), pool.Stat().NewConnsCount()
	called := false
	_, _, _, err = pipelined(advance, []string{advance}, func(pgx.BatchResults) error { called = true; return nil }, nil)
	if !errors.Is(err, authority.ErrPipelineAfterWrite) || called || sequence() != before || pool.Stat().NewConnsCount() != conns {
		t.Fatalf("pipeline after a write: %v, read called %v, family at %d (want %d), %d new connections (want %d)", err, called, sequence(), before,
			pool.Stat().NewConnsCount(), conns)
	}
	// A row lock is a write for the check, too.
	_, _, _, err = pipelined(`SELECT 1 FROM families WHERE id = $1 FOR NO KEY UPDATE`, []string{advance}, execAll(1), nil)
	if !errors.Is(err, authority.ErrPipelineAfterWrite) || sequence() != before {
		t.Fatalf("pipeline after a row lock: %v, family at %d (want %d)", err, sequence(), before)
	}

	// read's own error, in a transaction that wrote nothing before the
	// queue: returned as is, after the commit; the queued statement stands.
	refused := errors.New("refused after the commit")
	before = sequence()
	_, _, _, err = pipelined("", []string{advance}, func(r pgx.BatchResults) error { _, _ = r.Exec(); return refused }, nil)
	if !errors.Is(err, refused) || sequence() != before+1 {
		t.Fatalf("read refusal: %v, family at %d, want %v and %d", err, sequence(), refused, before+1)
	}

	// run's error rolls back and sends nothing it queued.
	before = sequence()
	called = false
	trips, statements, _, err = pipelined("", []string{advance}, func(pgx.BatchResults) error { called = true; return nil }, refused)
	if !errors.Is(err, refused) || called || trips != 2 || statementsMatching(statements, "UPDATE families") != 0 || sequence() != before {
		t.Fatalf("rolled back: %v, read called %v, %d round trips, %q, family at %d (want %d)", err, called, trips, statements, sequence(), before)
	}
}
