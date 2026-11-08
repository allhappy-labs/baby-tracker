//go:build identitytest

package loadseed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/loadtest"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// verifyFamilies families have their counters recomputed.
	verifyFamilies = 50
	// verifyDevices devices pull their family from cursor 0.
	verifyDevices = 20
	// vacuumWait bounds the wait for vacuums to finish.
	vacuumWait = 30 * time.Minute
)

// sample is n indices spread evenly over [0, total).
func sample(total, n int) []int {
	if total <= n {
		out := make([]int, total)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, n)
	for i := range out {
		out[i] = i * total / n
	}
	return out
}

// verify checks what the seed wrote:
//   - every audit chain verifies (authority.Verifier.VerifyAll, as
//     `baby-tracker-admin authority verify`);
//   - for sampled families, the four care counters equal
//     care.RecomputeCounters over what the family holds (the definition
//     caretest.AssertCounters checks the write paths against);
//   - for sampled devices, a pull from cursor 0 through the sync service,
//     paged to the end, returns exactly the family's care rows, every
//     entity naming the device's family;
//
// then vacuums and analyzes the whole database (vacuumAnalyze), waits until
// no vacuum of the database is in progress, and returns the database's
// size.
func verify(ctx context.Context, env Environment, s *services, families []*familyState) (int64, error) {
	verifier, err := authority.NewAuditVerifier(env.Superuser, time.Now, s.logger)
	if err != nil {
		return 0, err
	}
	step := time.Now()
	report, err := verifier.VerifyAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("loadseed: verify audit chains: %w", err)
	}
	if len(report.Failed) > 0 {
		return 0, fmt.Errorf("loadseed: %d audit chains failed verification (first %v)", len(report.Failed), report.Failed[0])
	}
	s.logger.Info("loadseed: audit chains verified", "scopes", report.Scopes, "rows", report.Rows, "seconds", time.Since(step).Seconds())
	step = time.Now()
	for _, i := range sample(len(families), verifyFamilies) {
		if err := verifyCounters(ctx, env, families[i].FamilyID); err != nil {
			return 0, err
		}
	}
	var devices []*loadtest.DeviceState
	for _, f := range families {
		devices = append(devices, f.Devices...)
	}
	for _, i := range sample(len(devices), verifyDevices) {
		if err := verifyPull(ctx, env, s, devices[i]); err != nil {
			return 0, err
		}
	}
	s.logger.Info("loadseed: counters and pulls verified", "seconds", time.Since(step).Seconds())
	step = time.Now()
	if err := vacuumAnalyze(ctx, env.Superuser, vacuumWorkers); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(vacuumWait)
	for {
		var running int
		if err := env.Superuser.QueryRow(ctx, `SELECT count(*) FROM pg_stat_progress_vacuum WHERE datname = current_database()`).Scan(&running); err != nil {
			return 0, fmt.Errorf("loadseed: vacuum progress: %w", err)
		}
		if running == 0 {
			break
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("loadseed: %d vacuums still running after %s", running, vacuumWait)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	s.logger.Info("loadseed: vacuumed", "seconds", time.Since(step).Seconds())
	var size int64
	if err := env.Superuser.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&size); err != nil {
		return 0, fmt.Errorf("loadseed: database size: %w", err)
	}
	return size, nil
}

// vacuumWorkers is how many relations vacuumAnalyze processes at once (the
// load node's database has 4 CPUs).
const vacuumWorkers = 4

// vacuumAnalyze does what a database-wide VACUUM (ANALYZE) does, workers
// relations at a time instead of one: the load's runs need the visibility
// map (index-only scans, and no first reader setting hint bits on every
// page) and statistics of every table. Every table and materialized view
// of the database is vacuumed, largest first, and analyzed with it, except
// partitions; each partitioned table that is not itself a partition is
// then analyzed, which analyzes it as a whole and each of its partitions.
// Temporary tables of other sessions are skipped, as a database-wide
// VACUUM skips them.
func vacuumAnalyze(ctx context.Context, pool *pgxpool.Pool, workers int) error {
	rows, err := pool.Query(ctx, `SELECT CASE WHEN c.relkind = 'p' THEN 'ANALYZE ' WHEN c.relispartition THEN 'VACUUM ' ELSE 'VACUUM (ANALYZE) ' END
			|| format('%I.%I', n.nspname, c.relname), c.relkind = 'p'
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'm', 'p') AND c.relpersistence <> 't' AND NOT (c.relkind = 'p' AND c.relispartition)
		ORDER BY c.relkind = 'p', pg_total_relation_size(c.oid) DESC, 1`)
	if err != nil {
		return fmt.Errorf("loadseed: list the relations to vacuum: %w", err)
	}
	var vacuums, analyzes []string
	var statement string
	var partitioned bool
	if _, err := pgx.ForEachRow(rows, []any{&statement, &partitioned}, func() error {
		if partitioned {
			analyzes = append(analyzes, statement)
		} else {
			vacuums = append(vacuums, statement)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("loadseed: list the relations to vacuum: %w", err)
	}
	// Partitioned tables after every vacuum: analyzing one takes each
	// partition's lock, which a vacuum of it would wait for.
	for _, statements := range [][]string{vacuums, analyzes} {
		if err := forEach(ctx, workers, len(statements), func(i int) error {
			if _, err := pool.Exec(ctx, statements[i]); err != nil {
				return fmt.Errorf("loadseed: %s: %w", statements[i], err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// verifyCounters compares the family's counters with what it holds.
func verifyCounters(ctx context.Context, env Environment, familyID string) error {
	tx, err := env.Superuser.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	stored, err := care.ReadFamilyState(ctx, tx, familyID)
	if err != nil {
		return fmt.Errorf("loadseed: family %s state: %w", familyID, err)
	}
	recomputed, err := care.RecomputeCounters(ctx, tx, familyID)
	if err != nil {
		return err
	}
	recomputed.Sequence = stored.Sequence
	if stored != recomputed {
		return fmt.Errorf("loadseed: family %s counters %+v, it holds %+v", familyID, stored, recomputed)
	}
	var maxSeq int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(changed_seq), 0) FROM care_entities WHERE family_id = $1`, familyID).Scan(&maxSeq); err != nil {
		return err
	}
	if uint64(maxSeq) > stored.Sequence {
		return fmt.Errorf("loadseed: family %s holds sequence %d past its high water %d", familyID, maxSeq, stored.Sequence)
	}
	return nil
}

// verifyPull pulls d's family from cursor 0 to the end through the sync
// service and checks it against the family's rows.
func verifyPull(ctx context.Context, env Environment, s *services, d *loadtest.DeviceState) error {
	if err := s.fresh(ctx, d, freshMargin); err != nil {
		return err
	}
	principal, err := s.principal(ctx, d)
	if err != nil {
		return fmt.Errorf("loadseed: device %s authenticate: %w", d.DeviceID, err)
	}
	request := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}},
		Mutations: []protocol.Mutation{}}
	pulled := 0
	for pages := 0; ; pages++ {
		if pages > 100_000 {
			return errors.New("loadseed: the pull did not end")
		}
		response, err := s.sync.ApplyAndPull(ctx, principal, request)
		if err != nil {
			return fmt.Errorf("loadseed: device %s pull: %w", d.DeviceID, err)
		}
		page, err := decodePage(response)
		if err != nil {
			return err
		}
		for _, change := range page.Changes {
			if change.SnapshotOrTombstone.Meta.FamilyID != d.FamilyID || change.Cursor.FamilyID != d.FamilyID {
				return fmt.Errorf("loadseed: device %s of family %s pulled %s of family %s", d.DeviceID, d.FamilyID, change.EntityID,
					change.SnapshotOrTombstone.Meta.FamilyID)
			}
		}
		pulled += len(page.Changes)
		if page.NextPageToken == nil {
			break
		}
		request = protocol.SyncRequest{RequestMeta: request.RequestMeta, Mutations: []protocol.Mutation{}, PageToken: page.NextPageToken}
	}
	var rows int
	if err := env.Superuser.QueryRow(ctx, `SELECT count(*) FROM care_entities WHERE family_id = $1`, d.FamilyID).Scan(&rows); err != nil {
		return err
	}
	if pulled != rows {
		return fmt.Errorf("loadseed: device %s pulled %d changes of family %s, which holds %d rows", d.DeviceID, pulled, d.FamilyID, rows)
	}
	return nil
}

// decodePage is response as a client reads it (encoded as the handler
// writes it, decoded by protocol.SyncResponse).
func decodePage(response protocol.SyncResponse) (protocol.SyncResponse, error) {
	raw, err := json.Marshal(response)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	var decoded protocol.SyncResponse
	if err := decoded.UnmarshalJSON(raw); err != nil {
		return protocol.SyncResponse{}, fmt.Errorf("loadseed: decode a sync page: %w", err)
	}
	return decoded, nil
}
