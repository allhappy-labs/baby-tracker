package loadtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"babytracker/server/internal/protocol"
)

// MaintenanceHot is the --maintenance option that runs one family purge and
// one export in the middle of a soak (Task 16 Step 4: the wake tick delay
// while maintenance holds the maintenance pool). Both are the product's
// own work:
//   - the export: an owner of another family creates an export through
//     POST /v1/export, polls it ready and downloads it at full speed;
//   - the purge: the owner of the family with the most history schedules
//     its deletion through POST /v1/family/delete; the disposable stack's
//     recovery window (30 days) is then advanced to now, the one step that
//     is not a product command (a superuser UPDATE of purge_after under the
//     authority context); the product's purge worker starts, batches and
//     completes the purge on its own ticks.
//
// The purge family takes no part in the background from the start (no
// refresh, pull, write, socket or tracked device: its sessions end with the
// deletion), and the fleet's checkpoints drop it once its deletion is
// scheduled. The cell records when each step happened, so the app's
// resource samples (the wake tick figures) can be read over the window.
const MaintenanceHot = "hot"

// maintenanceDB is the database side of the hot purge: the superuser's
// view of the disposable stack (tests use a fake).
type maintenanceDB interface {
	// purgeDueNow advances the family's recovery window to now.
	purgeDueNow(ctx context.Context, familyID string) error
	// familyStatus is the family's status ("" when the row is gone).
	familyStatus(ctx context.Context, familyID string) (string, error)
	// careRows is the family's care_entities and care_receipts rows.
	careRows(ctx context.Context, familyID string) (entities, receipts int64, err error)
}

// superuserDB is maintenanceDB over the stack's superuser pool (the
// caller checked that the database is a disposable load stack).
type superuserDB struct{ pool *pgxpool.Pool }

func (s superuserDB) purgeDueNow(ctx context.Context, familyID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL app.authority = '1'`); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE families SET purge_after = now() WHERE id = $1 AND status = 'deletion_pending'`, familyID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("family %s is not deletion_pending", familyID)
		}
		return nil
	})
}

func (s superuserDB) familyStatus(ctx context.Context, familyID string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status::text FROM families WHERE id = $1`, familyID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return status, err
}

func (s superuserDB) careRows(ctx context.Context, familyID string) (int64, int64, error) {
	var entities, receipts int64
	err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM care_entities WHERE family_id = $1), (SELECT count(*) FROM care_receipts WHERE family_id = $1)`,
		familyID).Scan(&entities, &receipts)
	return entities, receipts, err
}

// RouteFamilyDelete is the purge's deletion request.
const RouteFamilyDelete = "family_delete"

// maintenancePoll is how often the purge's status is read.
const maintenancePoll = time.Second

// hotMaintenance is the cell's plan: the purge family's devices (parked,
// out of the background) and the export's owner.
type hotMaintenance struct {
	family   string
	parked   []*device
	owner    *device
	exporter *device
	mu       sync.Mutex
	dropped  bool // the deletion is scheduled: the checkpoints leave the family out
}

// splitMaintenance picks the purge family from the fleet: the family with
// the most history (its devices' highest cursor), never the hot family nor
// one holding an MCP credential, with an owner device; ties go to the
// lowest family ID. It returns that family's states and the others.
func splitMaintenance(header FleetHeader, states []DeviceState) (family string, parked, others []DeviceState, err error) {
	type stats struct {
		cursor uint64
		owner  bool
		mcp    bool
	}
	byFamily := map[string]*stats{}
	for _, s := range states {
		f := byFamily[s.FamilyID]
		if f == nil {
			f = &stats{}
			byFamily[s.FamilyID] = f
		}
		f.cursor = max(f.cursor, s.Cursor)
		f.owner = f.owner || s.Role == "owner"
		f.mcp = f.mcp || s.MCPCredential != ""
	}
	ids := make([]string, 0, len(byFamily))
	for id := range byFamily {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		f := byFamily[id]
		if id == header.HotFamilyID || f.mcp || !f.owner {
			continue
		}
		if family == "" || f.cursor > byFamily[family].cursor {
			family = id
		}
	}
	if family == "" {
		return "", nil, nil, errors.New("run: --maintenance hot: no family to purge (one with an owner and no MCP grant)")
	}
	for _, s := range states {
		if s.FamilyID == family {
			parked = append(parked, s)
		} else {
			others = append(others, s)
		}
	}
	if len(others) == 0 {
		return "", nil, nil, errors.New("run: --maintenance hot: the purge would leave no device")
	}
	return family, parked, others, nil
}

// planMaintenance parks the purge family's devices and picks the export's
// owner (an owner of another family, by the seed).
func (g *generator) planMaintenance(family string, parked []DeviceState) error {
	m := &hotMaintenance{family: family}
	for i := range parked {
		d := &device{DeviceState: parked[i], index: -1}
		m.parked = append(m.parked, d)
		if d.Role == "owner" && m.owner == nil {
			m.owner = d
		}
	}
	owners := distinctOwners(g, 1, 0xe5)
	if len(owners) == 0 || m.owner == nil {
		return errors.New("run: --maintenance hot needs an owner in the purge family and another family's owner for the export")
	}
	m.exporter = owners[0]
	g.maintenance = m
	return nil
}

// checkpointStates is the fleet the checkpoint writes: every device,
// with the purge family's until its deletion is scheduled.
func (g *generator) checkpointStates() (FleetHeader, []DeviceState) {
	header := g.header
	states := make([]DeviceState, 0, len(g.devices)+4)
	for _, d := range g.devices {
		states = append(states, d.state())
	}
	if m := g.maintenance; m != nil {
		m.mu.Lock()
		dropped := m.dropped
		m.mu.Unlock()
		if dropped {
			header.Families--
		} else {
			for _, d := range m.parked {
				states = append(states, d.state())
			}
		}
	}
	header.Devices = len(states)
	return header, states
}

// runHotMaintenance runs the export and the purge from at, concurrently,
// until both end or the measurement does, and records the cell.
func (g *generator) runHotMaintenance(at time.Time) {
	m := g.maintenance
	if !sleepUntil(g.work, at) {
		return
	}
	end := g.phases.windowStart.Add(g.cfg.Measure)
	ctx, cancel := context.WithDeadline(g.work, end)
	defer cancel()
	var wg sync.WaitGroup
	var export, purge map[string]any
	wg.Add(2)
	go func() {
		defer wg.Done()
		export = g.hotExport(ctx, m.exporter)
	}()
	go func() {
		defer wg.Done()
		purge = g.hotPurge(ctx, m)
	}()
	wg.Wait()
	g.cell("maintenance", map[string]any{"mode": MaintenanceHot, "from": at.UTC(), "to": time.Now().UTC(), "export": export, "purge": purge,
		"note": "the wake tick delay over [from, to] is in the app's resource samples (app.log); the purge's recovery window was advanced " +
			"to now by a superuser UPDATE of purge_after (the disposable stack's clock jump), every other step is the product's"})
}

// hotExport is one owner's export, created, polled ready and downloaded at
// full speed.
func (g *generator) hotExport(ctx context.Context, d *device) map[string]any {
	started := time.Now()
	r := g.slowExport(ctx, d, started, 0)
	out := map[string]any{"family_id": d.FamilyID, "started_at": started.UTC(), "ok": r.ok, "bytes": r.bytes,
		"seconds": time.Since(started).Seconds()}
	if !r.ready.IsZero() {
		out["ready_at"], out["seconds_to_ready"] = r.ready.UTC(), r.ready.Sub(started).Seconds()
	}
	return out
}

// hotPurge schedules the purge family's deletion through the API, makes it
// due and follows the product's purge worker until the family is
// purge_completed (or the measurement ends).
func (g *generator) hotPurge(ctx context.Context, m *hotMaintenance) map[string]any {
	out := map[string]any{"family_id": m.family, "devices": len(m.parked)}
	db := g.cfg.maintenanceDB
	if db == nil && g.cfg.Superuser != nil {
		db = superuserDB{pool: g.cfg.Superuser}
	}
	if db == nil {
		out["error"] = "no database to advance the recovery window"
		return out
	}
	if entities, receipts, err := db.careRows(ctx, m.family); err == nil {
		out["care_entities"], out["care_receipts"] = entities, receipts
	}
	owner := m.owner
	if owner.needsRefresh(time.Now()) && !g.refreshForDial(ctx, owner) {
		out["error"] = "the owner's access could not be refreshed"
		return out
	}
	requested := time.Now()
	body := protocol.ConfirmedCommandRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1",
		Capabilities: []string{protocol.CapabilityFamilyLifecycleV1}}, CommandID: newUUID(), Confirmed: true}
	res := g.sendWithRaces(ctx, RouteFamilyDelete, requested, request{method: http.MethodPost, path: "/v1/family/delete", device: owner,
		session: owner, capabilities: []string{protocol.CapabilityFamilyLifecycleV1}, body: body}, nil)
	out["delete_requested_at"], out["delete_outcome"] = requested.UTC(), res.Outcome
	if !res.ok() {
		return out
	}
	m.mu.Lock()
	m.dropped = true // its sessions are gone: the fleet leaves it out from now on
	m.mu.Unlock()
	if err := db.purgeDueNow(ctx, m.family); err != nil {
		out["error"] = "advance the recovery window: " + err.Error()
		return out
	}
	due := time.Now()
	out["due_at"] = due.UTC()
	ticker := time.NewTicker(maintenancePoll)
	defer ticker.Stop()
	for {
		status, err := db.familyStatus(ctx, m.family)
		now := time.Now()
		if err == nil {
			out["status"] = status
			if _, seen := out["purge_started_seen_at"]; !seen && (status == "purge_started" || status == "purge_completed" || status == "") {
				out["purge_started_seen_at"], out["seconds_due_to_start"] = now.UTC(), now.Sub(due).Seconds()
			}
			if status == "purge_completed" || status == "" {
				out["completed_seen_at"], out["seconds_due_to_completed"] = now.UTC(), now.Sub(due).Seconds()
				return out
			}
		}
		select {
		case <-ctx.Done():
			out["note"] = "the measurement ended before the purge completed"
			return out
		case <-ticker.C:
		}
	}
}
