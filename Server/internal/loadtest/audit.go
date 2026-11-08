package loadtest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TrackedDevice is a device whose view of its family the generator kept
// for the whole run: every entity it learned (from its warm-up snapshot and
// every applied page), at the cursor its last pull reached. The audit checks
// it against the database: a device that converged holds exactly the rows
// with changed_seq <= Cursor.
type TrackedDevice struct {
	FamilyID string                   `json:"family_id"`
	DeviceID string                   `json:"device_id"`
	Cursor   uint64                   `json:"cursor"`
	Entities map[string]TrackedEntity `json:"entities"`
}

// TrackedEntity is what a tracked device holds for one entity: the version,
// the SHA-256 (hex) of its wire form as json.Marshal writes the decoded
// canonical entity, and whether it is a tombstone.
type TrackedEntity struct {
	Version    uint64 `json:"version"`
	WireSHA256 string `json:"wire_sha256"`
	Tombstone  bool   `json:"tombstone,omitempty"`
}

// AuditFinding is one discrepancy the audit counted, for the report.
type AuditFinding struct {
	// Class is lost, cross_family, diverged or tracked_diverged.
	Class    string `json:"class"`
	FamilyID string `json:"family_id"`
	EntityID string `json:"entity_id,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
	Detail   string `json:"detail"`
}

// MaxAuditFindings bounds the findings AuditWithFindings returns; the
// counts in AuditResult are always complete.
const MaxAuditFindings = 200

// auditBatch is how many ledger entries one query checks.
const auditBatch = 1000

// suspectBatch is how many suspect ids one cross-family query looks up:
// that query has no family key and reads every partition, so the suspects
// of all ledger batches are gathered and looked up together.
const suspectBatch = 10000

// auditStatementTimeout bounds each audit statement: a cross-family scan
// of a 10,000-family history that runs longer is an error, not a hang.
const auditStatementTimeout = "300s"

// Audit checks every acknowledged write in the ledger and every tracked
// device against the database (see AuditWithFindings).
func Audit(ctx context.Context, db *pgxpool.Pool, ledger []LedgerEntry, tracked []TrackedDevice) (AuditResult, error) {
	result, _, err := AuditWithFindings(ctx, db, ledger, tracked)
	return result, err
}

// AuditWithFindings is Audit that also returns the first MaxAuditFindings
// discrepancies.
//
// A ledger entry is:
//   - Lost when its row is missing, or holds an older version, and no other
//     family files the entity;
//   - CrossFamily when it is missing from or older in its family and another
//     family files a row with its id;
//   - Diverged when the row holds the entry's version with another wire;
//   - fine when the row holds that version and wire, or a newer version (a
//     later write the ledger does not hold, such as a baby deletion's
//     cascade).
//
// A tombstoned entry is compared the same way: its version and wire are the
// tombstone's. A tracked device is diverged when the rows with
// changed_seq <= its cursor differ from its map in any id, version, wire or
// tombstone, or when it holds an entity its family has no row for. An entity
// whose row changed after the device's cursor is judged by version: held
// above the row's version is a divergence (a version the server never
// kept); held at it (the device's own acknowledged write after its last
// pull) must match the row's wire and tombstone; held below it cannot be
// compared and counts in TrackedSkipped. Each diverged device counts once.
//
// Everything is read in one read-only repeatable-read transaction, as the
// database user db connects as (the load stack's superuser: row security
// does not hide another family's rows from it).
func AuditWithFindings(ctx context.Context, db *pgxpool.Pool, ledger []LedgerEntry, tracked []TrackedDevice) (AuditResult, []AuditFinding, error) {
	a := &auditor{result: AuditResult{Acknowledged: len(ledger), TrackedDevices: len(tracked)}}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AuditResult{}, nil, fmt.Errorf("audit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+auditStatementTimeout+"'"); err != nil {
		return AuditResult{}, nil, fmt.Errorf("audit: statement timeout: %w", err)
	}
	for start := 0; start < len(ledger); start += auditBatch {
		if err := a.ledgerBatch(ctx, tx, ledger[start:min(start+auditBatch, len(ledger))]); err != nil {
			return AuditResult{}, nil, err
		}
	}
	for start := 0; start < len(a.suspects); start += suspectBatch {
		if err := a.classifyCandidates(ctx, tx, a.suspects[start:min(start+suspectBatch, len(a.suspects))]); err != nil {
			return AuditResult{}, nil, err
		}
	}
	for _, device := range tracked {
		if err := a.trackedDevice(ctx, tx, device); err != nil {
			return AuditResult{}, nil, err
		}
	}
	return a.result, a.findings, nil
}

type auditor struct {
	result   AuditResult
	findings []AuditFinding
	// suspects are the entries missing from, or older in, their family,
	// from every ledger batch: classified together once the batches end.
	suspects []candidate
}

func (a *auditor) find(f AuditFinding) {
	if len(a.findings) < MaxAuditFindings {
		a.findings = append(a.findings, f)
	}
}

// sqlLedgerBatch reads each entry's row by primary key (partition pruning
// on family_id). The ordinality maps a result back to its entry; the
// canonical text of each id is what later comparisons use.
const sqlLedgerBatch = `SELECT e.n, e.family_id::text, e.id::text, ce.entity_version, encode(sha256(ce.wire), 'hex')
FROM unnest($1::uuid[], $2::uuid[]) WITH ORDINALITY AS e(family_id, id, n)
LEFT JOIN care_entities ce ON ce.family_id = e.family_id AND ce.id = e.id`

// sqlFiledUnder finds the families that file each candidate id: reached
// only for entries missing from (or older in) their own family.
const sqlFiledUnder = `SELECT family_id::text, id::text FROM care_entities WHERE id = ANY($1::uuid[])`

// candidate is a ledger entry missing from, or older in, its family.
type candidate struct {
	entry        LedgerEntry
	familyID, id string // canonical text
	found        bool   // its family has a row, at version
	version      int64
}

func (a *auditor) ledgerBatch(ctx context.Context, tx pgx.Tx, batch []LedgerEntry) error {
	families := make([]string, len(batch))
	ids := make([]string, len(batch))
	for i, e := range batch {
		families[i], ids[i] = e.FamilyID, e.EntityID
	}
	rows, err := tx.Query(ctx, sqlLedgerBatch, families, ids)
	if err != nil {
		return fmt.Errorf("audit: read the ledger's rows: %w", err)
	}
	seen := 0
	for rows.Next() {
		var (
			n          int64
			family, id string
			version    *int64
			wireSHA256 *string
		)
		if err := rows.Scan(&n, &family, &id, &version, &wireSHA256); err != nil {
			rows.Close()
			return fmt.Errorf("audit: read the ledger's rows: %w", err)
		}
		seen++
		e := batch[n-1]
		switch {
		case version == nil:
			a.suspects = append(a.suspects, candidate{entry: e, familyID: family, id: id})
		case uint64(*version) < e.Version:
			a.suspects = append(a.suspects, candidate{entry: e, familyID: family, id: id, found: true, version: *version})
		case uint64(*version) == e.Version && !strings.EqualFold(*wireSHA256, e.WireSHA256):
			a.result.Diverged++
			a.find(AuditFinding{Class: "diverged", FamilyID: family, EntityID: id,
				Detail: fmt.Sprintf("version %d: stored wire sha256 %s, acknowledged %s", e.Version, *wireSHA256, e.WireSHA256)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("audit: read the ledger's rows: %w", err)
	}
	if seen != len(batch) {
		return fmt.Errorf("audit: %d ledger entries read back %d rows", len(batch), seen)
	}
	return nil
}

// classifyCandidates splits the entries missing from (or older in) their
// family into CrossFamily, when another family files a row with the id, and
// Lost.
func (a *auditor) classifyCandidates(ctx context.Context, tx pgx.Tx, candidates []candidate) error {
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.id
	}
	rows, err := tx.Query(ctx, sqlFiledUnder, ids)
	if err != nil {
		return fmt.Errorf("audit: find the families filing lost entities: %w", err)
	}
	filedUnder := map[string][]string{}
	for rows.Next() {
		var family, id string
		if err := rows.Scan(&family, &id); err != nil {
			rows.Close()
			return fmt.Errorf("audit: find the families filing lost entities: %w", err)
		}
		filedUnder[id] = append(filedUnder[id], family)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("audit: find the families filing lost entities: %w", err)
	}
	for _, c := range candidates {
		var elsewhere []string
		for _, family := range filedUnder[c.id] {
			if family != c.familyID {
				elsewhere = append(elsewhere, family)
			}
		}
		state := "no row"
		if c.found {
			state = fmt.Sprintf("row at version %d", c.version)
		}
		if len(elsewhere) > 0 {
			slices.Sort(elsewhere)
			a.result.CrossFamily++
			a.find(AuditFinding{Class: "cross_family", FamilyID: c.familyID, EntityID: c.id,
				Detail: fmt.Sprintf("acknowledged version %d; %s in its family; filed under %s", c.entry.Version, state, strings.Join(elsewhere, ", "))})
			continue
		}
		a.result.Lost++
		a.find(AuditFinding{Class: "lost", FamilyID: c.familyID, EntityID: c.id,
			Detail: fmt.Sprintf("acknowledged version %d; %s", c.entry.Version, state)})
	}
	return nil
}

// sqlTrackedRows is every row a device at cursor $2 must hold, tombstones
// included (pulls and snapshots both serve them).
const sqlTrackedRows = `SELECT id::text, entity_version, encode(sha256(wire), 'hex'), tombstoned_at IS NOT NULL
FROM care_entities WHERE family_id = $1 AND changed_seq <= $2`

// sqlTrackedLater reads the rows of a device's other entities its family
// still files (changed after its cursor).
const sqlTrackedLater = `SELECT id::text, entity_version, encode(sha256(wire), 'hex'), tombstoned_at IS NOT NULL
FROM care_entities WHERE family_id = $1 AND id = ANY($2::uuid[])`

func (a *auditor) trackedDevice(ctx context.Context, tx pgx.Tx, d TrackedDevice) error {
	if d.Cursor > 1<<53-1 {
		return fmt.Errorf("audit: tracked device %s cursor %d is out of range", d.DeviceID, d.Cursor)
	}
	held := make(map[string]TrackedEntity, len(d.Entities))
	for id, e := range d.Entities {
		held[strings.ToLower(id)] = e
	}
	rows, err := tx.Query(ctx, sqlTrackedRows, d.FamilyID, int64(d.Cursor))
	if err != nil {
		return fmt.Errorf("audit: read tracked device %s's family: %w", d.DeviceID, err)
	}
	var differences []string
	compared := map[string]bool{}
	for rows.Next() {
		var (
			id, wireSHA256 string
			version        int64
			tombstone      bool
		)
		if err := rows.Scan(&id, &version, &wireSHA256, &tombstone); err != nil {
			rows.Close()
			return fmt.Errorf("audit: read tracked device %s's family: %w", d.DeviceID, err)
		}
		compared[id] = true
		e, ok := held[id]
		switch {
		case !ok:
			differences = append(differences, fmt.Sprintf("%s missing (server version %d)", id, version))
		case e.Version != uint64(version) || !strings.EqualFold(e.WireSHA256, wireSHA256) || e.Tombstone != tombstone:
			differences = append(differences, fmt.Sprintf("%s holds version %d tombstone %v, server version %d tombstone %v%s",
				id, e.Version, e.Tombstone, version, tombstone, wireNote(e.Version == uint64(version) && !strings.EqualFold(e.WireSHA256, wireSHA256))))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("audit: read tracked device %s's family: %w", d.DeviceID, err)
	}
	var others []string
	for id := range held {
		if !compared[id] {
			others = append(others, id)
		}
	}
	if len(others) > 0 {
		type laterRow struct {
			version    uint64
			wireSHA256 string
			tombstone  bool
		}
		later := map[string]laterRow{}
		rows, err := tx.Query(ctx, sqlTrackedLater, d.FamilyID, others)
		if err != nil {
			return fmt.Errorf("audit: read tracked device %s's later rows: %w", d.DeviceID, err)
		}
		for rows.Next() {
			var (
				id  string
				row laterRow
				v   int64
			)
			if err := rows.Scan(&id, &v, &row.wireSHA256, &row.tombstone); err != nil {
				rows.Close()
				return fmt.Errorf("audit: read tracked device %s's later rows: %w", d.DeviceID, err)
			}
			row.version = uint64(v)
			later[id] = row
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("audit: read tracked device %s's later rows: %w", d.DeviceID, err)
		}
		slices.Sort(others)
		for _, id := range others {
			row, ok := later[id]
			e := held[id]
			switch {
			case !ok:
				differences = append(differences, fmt.Sprintf("%s held but its family files no such row", id))
			case e.Version > row.version:
				// A version the server never kept.
				differences = append(differences, fmt.Sprintf("%s holds version %d, above the server's version %d", id, e.Version, row.version))
			case e.Version == row.version:
				// The device learned the current version after its last
				// pull (its own acknowledged write): it must hold that
				// version's content.
				if !strings.EqualFold(e.WireSHA256, row.wireSHA256) || e.Tombstone != row.tombstone {
					differences = append(differences, fmt.Sprintf("%s holds version %d tombstone %v with another wire than the server's (tombstone %v), changed after cursor %d",
						id, e.Version, e.Tombstone, row.tombstone, d.Cursor))
				}
			default:
				// An older copy of a row changed after the cursor: not
				// comparable, only counted.
				a.result.TrackedSkipped++
			}
		}
	}
	if len(differences) == 0 {
		return nil
	}
	a.result.TrackedDiverged++
	shown := differences[:min(len(differences), 5)]
	a.find(AuditFinding{Class: "tracked_diverged", FamilyID: d.FamilyID, DeviceID: d.DeviceID,
		Detail: fmt.Sprintf("cursor %d: %d differences: %s", d.Cursor, len(differences), strings.Join(shown, "; "))})
	return nil
}

func wireNote(differs bool) string {
	if differs {
		return " (same version, another wire)"
	}
	return ""
}

// WriteTracked writes the tracked devices as JSON lines to a new private
// (0600) file; it never replaces an existing one.
func WriteTracked(path string, devices []TrackedDevice) (resultErr error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); resultErr == nil {
			resultErr = err
		}
	}()
	w := bufio.NewWriterSize(f, 1<<16)
	enc := json.NewEncoder(w)
	for _, d := range devices {
		if err := enc.Encode(d); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

// ReadTracked reads a tracked-devices file: one TrackedDevice per line.
// Every family, device and entity id must be a UUID (one malformed id would
// otherwise fail the audit's queries far from its line), and a device
// listed twice is an error, not a choice. An empty file is no
// devices (Evaluate fails it as vacuous).
func ReadTracked(path string) ([]TrackedDevice, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	type key struct{ family, device string }
	seen := map[key]bool{}
	var out []TrackedDevice
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 64<<20)
	for n := 1; sc.Scan(); n++ {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var d TrackedDevice
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			return nil, fmt.Errorf("tracked %s line %d: %w", path, n, err)
		}
		if !isUUID(d.FamilyID) || !isUUID(d.DeviceID) {
			return nil, fmt.Errorf("tracked %s line %d: family_id %q and device_id %q must be UUIDs", path, n, d.FamilyID, d.DeviceID)
		}
		for id := range d.Entities {
			if !isUUID(id) {
				return nil, fmt.Errorf("tracked %s line %d: device %s holds entity %q, not a UUID", path, n, d.DeviceID, id)
			}
		}
		k := key{d.FamilyID, d.DeviceID}
		if seen[k] {
			return nil, fmt.Errorf("tracked %s line %d: device %s listed twice", path, n, d.DeviceID)
		}
		seen[k] = true
		out = append(out, d)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("tracked %s: %w", path, err)
	}
	return out, nil
}

// isUUID reports whether s is a UUID in its hyphenated text form, in either
// case.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}
