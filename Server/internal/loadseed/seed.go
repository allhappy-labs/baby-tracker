//go:build identitytest

package loadseed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/loadtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Environment is what Seed runs against: the node's configuration (the
// same peppers and keys the server loads, so every digest matches), the
// runtime pool the product's commands run on, and a superuser pool for
// COPY, the key drop and re-add, the counters, verification and VACUUM.
type Environment struct {
	Config    config.Config
	Runtime   *pgxpool.Pool
	Superuser *pgxpool.Pool
	Logger    *slog.Logger
}

// MinSuperuserConns is the smallest superuser pool Seed accepts: the care
// stage holds one connection per hash partition and per receipt COPY for
// its whole duration, plus one for its other statements, and Seed holds
// one more for the whole seed (the seed lock). The backlog, which loads
// alongside the care stage, takes what is left (backlogCopies streams and
// its restore); in a pool this small the two take turns.
const MinSuperuserConns = partitionCount + receiptCopies + 1 + 1

// seedLockKey is the session advisory lock one seed of a database holds
// for its whole run, so two seeds never interleave.
const seedLockKey int64 = 0x10ad_5eed_0000_0001

// OpenPool opens a seeder pool of at most max connections on databaseURL.
// Its sessions commit without waiting for the WAL flush
// (synchronous_commit off): provisioning is thousands of small product
// commands, each of which would otherwise wait for its own fsync, and the
// seed is disposable (a crash means seeding again). superuser sessions also
// get the memory the key validation and VACUUM use.
func OpenPool(ctx context.Context, databaseURL string, max int32, superuser bool) (*pgxpool.Pool, error) {
	return OpenPoolWithMemory(ctx, databaseURL, max, superuser, SessionMemory{})
}

// SessionMemory is the per-session memory a superuser seeder session asks
// for. Empty fields take the defaults (256MB, 1GB). A node whose db cap
// holds large shared buffers lowers them: every superuser session may use
// both at once, so max x (work_mem + maintenance_work_mem) plus
// shared_buffers must fit the db's memory cap (an 8 GiB cap with 5GB of
// shared buffers was OOM-killed at the defaults on the Linux load VM).
type SessionMemory struct {
	WorkMem, MaintenanceWorkMem string
}

// validMemory is a PostgreSQL memory value as the seeder passes it on:
// digits and a kB, MB or GB unit.
var validMemory = regexp.MustCompile(`^[1-9][0-9]{0,5}(kB|MB|GB)$`)

// Valid reports whether every set field is a plain PostgreSQL memory value.
func (m SessionMemory) Valid() bool {
	return (m.WorkMem == "" || validMemory.MatchString(m.WorkMem)) &&
		(m.MaintenanceWorkMem == "" || validMemory.MatchString(m.MaintenanceWorkMem))
}

// OpenPoolWithMemory is OpenPool with the superuser sessions' memory set.
func OpenPoolWithMemory(ctx context.Context, databaseURL string, max int32, superuser bool, mem SessionMemory) (*pgxpool.Pool, error) {
	if !mem.Valid() {
		return nil, fmt.Errorf("loadseed: invalid session memory")
	}
	if mem.WorkMem == "" {
		mem.WorkMem = "256MB"
	}
	if mem.MaintenanceWorkMem == "" {
		mem.MaintenanceWorkMem = "1GB"
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("loadseed: the database URL is invalid")
	}
	cfg.MaxConns, cfg.MinConns = max, 0
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	if superuser {
		cfg.ConnConfig.RuntimeParams["work_mem"] = mem.WorkMem
		cfg.ConnConfig.RuntimeParams["maintenance_work_mem"] = mem.MaintenanceWorkMem
		// The index builds write every page as a full-page image: compressed,
		// they take less of the WAL bandwidth the COPYs are bound by.
		cfg.ConnConfig.RuntimeParams["wal_compression"] = "zstd"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("loadseed: open a pool: %w", err)
	}
	return pool, nil
}

// lockSeed takes the seed lock (a session advisory lock) on a connection
// of its own, held until unlock: a second seed of the same database while
// one runs is refused, not interleaved.
func lockSeed(ctx context.Context, env Environment) (unlock func(), err error) {
	conn, err := env.Superuser.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("loadseed: acquire the seed lock connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, seedLockKey).Scan(&locked); err != nil {
		conn.Release()
		return nil, fmt.Errorf("loadseed: take the seed lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, errors.New("loadseed: refusing to seed: another seed is running on this database")
	}
	return func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, seedLockKey); err != nil {
			// The session still holds the lock: close it rather than pool it.
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
		conn.Release()
	}, nil
}

func (env Environment) logger() *slog.Logger {
	if env.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return env.Logger
}

// Report is what Seed wrote and how long each stage took.
// LiveActivityRegistrations counts device registrations and running-timer
// cards. Audit rows come only from the provisioning commands; the backlog
// writes none (D1).
//
// Phases do not sum to the seed's time: the backlog runs alongside the care
// stage, and the care baby key's validation runs alongside the side stage
// and is in no phase (the "care baby key re-added" log line times it). The
// wall time (the seed command's TotalSeconds) is the seed's figure.
type Report struct {
	Families, Accounts, Devices, CareEntities, Receipts, ConsumedSessions, LiveActivityRegistrations, PushTokens, MCPGrants int
	DatabaseBytes                                                                                                           int64
	Phases                                                                                                                  map[string]time.Duration
	// HotFamilyID is the hot family's (spec.HotFamily), counted in Families.
	HotFamilyID string `json:",omitempty"`
}

// The stages, in order (Report.Phases keys).
const (
	PhaseAuthority = "authority"
	PhaseCare      = "care"
	PhaseSide      = "side"
	PhaseBacklog   = "backlog"
	PhaseVerify    = "verify"
)

// Seed seeds spec into env's database and returns the report, the fleet
// header and every device's state. It runs the stages in order:
// authority (provisioning through the product's commands), care (history
// through care.Encode and COPY) with backlog (consumed sessions) alongside
// it, side (cursors, push tokens, Live Activity) and verify. A stage's
// phase is its own duration: care's and backlog's overlap.
func Seed(ctx context.Context, env Environment, spec Spec) (Report, loadtest.FleetHeader, []loadtest.DeviceState, error) {
	report := Report{Phases: map[string]time.Duration{}}
	if err := spec.Validate(); err != nil {
		return report, loadtest.FleetHeader{}, nil, fmt.Errorf("loadseed: spec: %w", err)
	}
	digest, err := spec.SHA256()
	if err != nil {
		return report, loadtest.FleetHeader{}, nil, err
	}
	if env.Runtime == nil || env.Superuser == nil {
		return report, loadtest.FleetHeader{}, nil, fmt.Errorf("loadseed: the environment needs the runtime and superuser pools")
	}
	if got := env.Superuser.Config().MaxConns; got < MinSuperuserConns {
		return report, loadtest.FleetHeader{}, nil, fmt.Errorf("loadseed: the superuser pool allows %d connections; the care stage holds %d COPY streams at once and needs at least %d",
			got, partitionCount+receiptCopies, MinSuperuserConns)
	}
	if err := checkStack(ctx, env); err != nil { // before the lock: an unmarked database is not touched
		return report, loadtest.FleetHeader{}, nil, err
	}
	unlock, err := lockSeed(ctx, env)
	if err != nil {
		return report, loadtest.FleetHeader{}, nil, err
	}
	defer unlock()
	s, err := compose(ctx, env) // checks the marker and server_state again
	if err != nil {
		return report, loadtest.FleetHeader{}, nil, err
	}
	if err := refuseSeeded(ctx, env); err != nil {
		return report, loadtest.FleetHeader{}, nil, err
	}
	stage := func(name string, run func() error) error {
		started := time.Now()
		err := run()
		report.Phases[name] = time.Since(started)
		s.logger.Info("loadseed: stage", "stage", name, "seconds", report.Phases[name].Seconds(), "ok", err == nil)
		return err
	}
	var families []*familyState
	if err := stage(PhaseAuthority, func() error {
		families, err = s.provisionAll(ctx, spec)
		return err
	}); err != nil {
		return report, loadtest.FleetHeader{}, nil, err
	}
	accounts := map[string]bool{}
	var devices []*loadtest.DeviceState
	for _, f := range families {
		for _, d := range f.Devices {
			accounts[d.AccountID] = true
			devices = append(devices, d)
		}
		if f.MCPGrantID != "" {
			report.MCPGrants++
		}
		if f.Hot {
			report.HotFamilyID = f.FamilyID
		}
	}
	report.Families, report.Accounts, report.Devices = len(families), len(accounts), len(devices)

	// The backlog writes only sessions, from the live ones the authority
	// stage left (their lineage, which a refresh keeps), and no stage reads
	// it: it loads while the care stage does, which leaves the database's
	// CPUs idle as it waits on WAL. The side stage, which refreshes
	// sessions, starts once both are done.
	backlogCtx, cancelBacklog := context.WithCancel(ctx)
	// A failed backlog stops the care stage at once (rather than after
	// minutes of COPY): its error is the seed's.
	careCtx, cancelCare := context.WithCancelCause(ctx)
	defer cancelCare(nil)
	backlogDone := make(chan error, 1)
	var backlogTime time.Duration
	var consumed int
	go func() {
		started := time.Now()
		n, err := loadBacklog(backlogCtx, env, s, spec, devices)
		consumed, backlogTime = n, time.Since(started)
		if err != nil {
			cancelCare(fmt.Errorf("loadseed: the backlog failed: %w", err))
		}
		backlogDone <- err
	}()
	joinBacklog := func() error {
		err := <-backlogDone
		backlogDone = nil
		report.ConsumedSessions, report.Phases[PhaseBacklog] = consumed, backlogTime
		s.logger.Info("loadseed: stage", "stage", PhaseBacklog, "seconds", backlogTime.Seconds(), "ok", err == nil, "alongside", PhaseCare)
		return err
	}
	defer func() {
		if backlogDone != nil { // the care stage failed: stop the backlog and wait for it
			cancelBacklog()
			<-backlogDone
		}
		cancelBacklog()
	}()

	var cares careLoad
	defer func() { _ = cares.waitBabyKey() }() // a later stage failed: the key still comes back first
	if err := stage(PhaseCare, func() error {
		cares, err = loadCare(careCtx, env, spec, families, s.server)
		return err
	}); err != nil {
		if cause := context.Cause(careCtx); cause != nil && ctx.Err() == nil {
			return report, loadtest.FleetHeader{}, nil, cause
		}
		return report, loadtest.FleetHeader{}, nil, err
	}
	report.CareEntities, report.Receipts = cares.entities, cares.receipts
	for i, f := range families {
		var babies []string
		for _, b := range cares.plans[i].Babies {
			babies = append(babies, b.ID)
		}
		for _, d := range f.Devices {
			d.BabyIDs = babies
		}
	}

	if err := joinBacklog(); err != nil {
		return report, loadtest.FleetHeader{}, nil, err
	}

	if err := stage(PhaseSide, func() error {
		side, err := loadSide(ctx, env, s, spec, families, cares)
		report.PushTokens, report.LiveActivityRegistrations = side.pushTokens, side.deviceRegistrations+side.activities
		return err
	}); err != nil {
		return report, loadtest.FleetHeader{}, nil, err
	}
	if err := cares.waitBabyKey(); err != nil {
		return report, loadtest.FleetHeader{}, nil, err
	}
	if err := stage(PhaseVerify, func() error {
		report.DatabaseBytes, err = verify(ctx, env, s, families)
		return err
	}); err != nil {
		return report, loadtest.FleetHeader{}, nil, err
	}
	fleet := make([]loadtest.DeviceState, len(devices))
	for i, d := range devices {
		fleet[i] = *d
	}
	header := loadtest.FleetHeader{Version: loadtest.FleetVersion, SeedSpecSHA256: digest, AsOf: spec.AsOf, Families: len(families),
		Devices: len(fleet), ServerInstanceID: env.Config.ServerInstanceID, RecoveryEpoch: env.Config.RecoveryEpoch, HotFamilyID: report.HotFamilyID}
	return report, header, fleet, nil
}
