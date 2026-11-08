//go:build identitytest

package loadseed

import (
	"context"
	crand "crypto/rand"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/loadtest"
	"github.com/jackc/pgx/v5"
)

// sessionColumns are the sessions columns the backlog writes.
var sessionColumns = []string{"access_digest", "id", "account_id", "device_id", "lineage_root_id", "lineage_started_at", "scope", "family_id",
	"role", "recovery_epoch", "issued_seq", "token_key_id", "refresh_digest", "access_expires_at", "refresh_expires_at", "deadline_at", "status",
	"consumed_at", "child_id", "created_at"}

// backlogCopies is how many COPY streams write the backlog.
const backlogCopies = 8

// probeSettings are the planner settings a key from a small table into a
// large one validates under: without hash and merge joins the validation
// probes the referenced key's index once per referencing row instead of
// reading the whole referenced table (refresh_receipts' few rows against
// every session the backlog wrote).
var probeSettings = []string{"enable_hashjoin = off", "enable_mergejoin = off"}

// sessionsDeferral is what the backlog keeps out of its COPY: the sessions
// indexes a consumed row enters (the primary key and the unique ID and
// refresh digest, all random, and the consumed index; the partial indexes
// of active sessions take no backlog row), the account and device keys
// (a trigger, a query and a row lock per row, the same rows locked by
// every stream), and refresh_receipts' keys to sessions(id), which hold
// the unique ID constraint in place.
var sessionsDeferral = deferral{
	indexes: []string{"public.sessions_consumed_idx"},
	constraints: []deferredConstraint{
		{table: "public.refresh_receipts", name: "refresh_receipts_parent_id_fkey", settings: probeSettings},
		{table: "public.refresh_receipts", name: "refresh_receipts_child_id_fkey", settings: probeSettings},
		{table: "public.sessions", name: "sessions_account_id_fkey"}, {table: "public.sessions", name: "sessions_device_id_account_id_fkey"},
		{table: "public.sessions", name: "sessions_pkey"}, {table: "public.sessions", name: "sessions_id_key"},
		{table: "public.sessions", name: "sessions_refresh_digest_key"},
	},
}

// loadBacklog writes the consumed-session backlog with COPY as the
// superuser: spec.BacklogRows(devices) sessions consumed uniformly over
// the BacklogHours before AsOf, spread evenly over the devices, each with
// random access and refresh digests and child, the device's real account,
// device and lineage root, its live session's scope, family and role, the
// current token key, issued_seq 1, and expiry columns ordered as
// sessions_check1 requires (access ≤ refresh ≤ the lineage deadline).
//
// sessionsDeferral is dropped for the COPY and restored after it (built
// and validated as the migration's objects are, see deferredSchema), also
// when the COPY fails.
//
// The rows are what a day of rotations at the run's load leaves behind for
// the retention sweep and the session indexes; they write no audit rows
// (decision D1: routine refresh writes none).
func loadBacklog(ctx context.Context, env Environment, s *services, spec Spec, devices []*loadtest.DeviceState) (int, error) {
	rows := spec.BacklogRows(len(devices))
	if rows == 0 || len(devices) == 0 {
		return 0, nil
	}
	ids := make([]string, len(devices))
	for i, d := range devices {
		ids[i] = d.DeviceID
	}
	live, err := readLiveSessions(ctx, env, ids)
	if err != nil {
		return 0, err
	}
	sessions := make([]liveSession, len(devices))
	for i, id := range ids {
		sessions[i] = live[id]
	}
	tokenKey := s.cfg.Auth.SessionTokenKeys.Current.ID
	epoch := int64(s.cfg.RecoveryEpoch)
	window := time.Duration(spec.BacklogHours) * time.Hour
	from := spec.AsOf.Add(-window)
	deferred, err := recordDeferral(ctx, env.Superuser, sessionsDeferral)
	if err != nil {
		return 0, err
	}
	if err := deferred.drop(ctx, env.Superuser); err != nil {
		return 0, err
	}
	restored := false
	defer func() {
		if !restored { // a failed load leaves the schema whole
			env.logger().Warn("loadseed: the backlog failed: restoring the sessions keys and indexes; do not kill the seed")
			if err := deferred.restore(context.WithoutCancel(ctx), env.Superuser, restoreWorkers); err != nil {
				env.logger().Error("loadseed: restoring the sessions schema failed; recreate the stack", "error", err)
			}
		}
	}()
	started := time.Now()
	var written atomic.Int64
	var wg sync.WaitGroup
	errs := make([]error, backlogCopies)
	for c := range backlogCopies {
		start, end := rows*c/backlogCopies, rows*(c+1)/backlogCopies
		wg.Add(1)
		go func() {
			defer wg.Done()
			var key [32]byte
			if _, err := crand.Read(key[:]); err != nil {
				errs[c] = err
				return
			}
			r := rand.New(rand.NewChaCha8(key))
			j := start
			n, err := copyFrom(ctx, env.Superuser, "sessions", sessionColumns, pgx.CopyFromFunc(func() ([]any, error) {
				if j == end {
					return nil, nil
				}
				live := sessions[j%len(sessions)]
				j++
				var access, refresh [32]byte
				var id, child [16]byte
				fill(r, access[:], refresh[:], id[:], child[:])
				consumed := from.Add(time.Duration(1+r.Int64N(int64(window/time.Microsecond)-1)) * time.Microsecond)
				created := consumed.Add(-time.Duration(r.Int64N(int64(10*time.Minute/time.Microsecond))) * time.Microsecond)
				accessExpires := created.Add(10 * time.Minute)
				refreshExpires := created.Add(30 * 24 * time.Hour)
				if refreshExpires.After(live.DeadlineAt) {
					refreshExpires = live.DeadlineAt
				}
				if accessExpires.After(refreshExpires) {
					accessExpires = refreshExpires
				}
				return []any{access[:], shapeUUIDv4(id[:]), live.AccountID, live.DeviceID, live.LineageRootID, live.LineageStartedAt, live.Scope,
					live.FamilyID, live.Role, epoch, int64(1), tokenKey, refresh[:], accessExpires, refreshExpires, live.DeadlineAt, "consumed",
					consumed, shapeUUIDv4(child[:]), created}, nil
			}))
			written.Add(n)
			errs[c] = err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return int(written.Load()), err
		}
	}
	env.logger().Info("loadseed: backlog copied", "sessions", written.Load(), "seconds", time.Since(started).Seconds())
	step := time.Now()
	if err := deferred.restore(ctx, env.Superuser, restoreWorkers); err != nil {
		return int(written.Load()), err
	}
	restored = true
	env.logger().Info("loadseed: sessions indexes and keys restored", "seconds", time.Since(step).Seconds())
	return int(written.Load()), nil
}

// fill fills each buffer with r's bytes.
func fill(r *rand.Rand, buffers ...[]byte) {
	for _, b := range buffers {
		for i := 0; i < len(b); i += 8 {
			v := r.Uint64()
			for k := 0; k < 8 && i+k < len(b); k++ {
				b[i+k] = byte(v >> (8 * k))
			}
		}
	}
}
