package care

import (
	"encoding/json"
	"fmt"

	"babytracker/server/internal/protocol"
)

// collectRefs is everything the batch's mutations reference, for one Fetch:
// the targets, the payload babies, the session ids (a session id may also
// name a row) and their live members, the receipts of the mutation ids, and
// the live baby count when the batch creates a baby. A pump create or update
// reads the family's running pump. A timer start also
// reads its baby's live timers; a stop reads the id it names for its
// completion; every other timer command reads the completion of its timer
// if it has stopped; a baby delete reads the baby's live rows (its cascade
// targets). Only well-formed ids are fetched; a malformed one is refused by
// validation without a lookup.
func collectRefs(cmd BatchCommand) Refs {
	refs := Refs{DeviceID: cmd.DeviceID}
	seen := map[string]bool{}
	add := func(into *[]string, kind, id string) {
		if !protocol.ValidUUID(id) || seen[kind+id] {
			return
		}
		seen[kind+id] = true
		*into = append(*into, id)
	}
	for _, m := range cmd.Mutations {
		if !cmd.NoReceipts {
			add(&refs.MutationIDs, "mutation", m.MutationID)
		}
		add(&refs.IDs, "row", m.EntityID)
		add(&refs.IDs, "row", mutationBabyID(m))
		if session := mutationSessionID(m); session != "" {
			add(&refs.IDs, "row", session)
			add(&refs.SessionIDs, "session", session)
		}
		switch {
		case m.EntityKind == "baby" && m.Operation == "create":
			refs.CountLiveBabies = true
		case m.EntityKind == "baby" && m.Operation == "delete":
			add(&refs.CascadeBabyIDs, "cascade", m.EntityID)
		case m.EntityKind == "pump" && m.Operation != "delete":
			refs.OpenPump = true
		case m.EntityKind == "timer":
			add(&refs.IDs, "row", timerMutationBabyID(m))
			switch p := m.Payload.(type) {
			case protocol.TimerStartPayload:
				add(&refs.TimerBabyIDs, "timers", p.BabyID)
			case protocol.TimerStopPayload:
				add(&refs.IDs, "row", p.CompletedRecordID)
				add(&refs.StoppedTimerIDs, "stopped", m.EntityID)
			default:
				add(&refs.StoppedTimerIDs, "stopped", m.EntityID)
			}
		}
	}
	return refs
}

// workingSet is the family as the batch has changed it so far: the fetched
// rows with every earlier write of the batch over them, the receipts, the
// sequence and the counter delta. A mutation changes it only once it is
// decided, so a refusing mutation leaves the written prefix as it was.
type workingSet struct {
	cmd      BatchCommand
	accepted string // the normalized acceptance time of every change
	base     FamilyState

	rows    map[string]Row // fetched rows and live timers, then this batch's writes
	members map[string]Row // live session members as fetched (no wire)
	cascade []Row          // the live rows of the babies the batch deletes, as fetched (no wire)
	order   []string       // written ids, in first-write order
	written map[string]bool

	receipts    map[string]StoredReceipt // stored, then this batch's
	receiptRows []ReceiptRow

	sequence   uint64
	delta      CounterDelta
	liveBabies int

	undo []undoWrite // the current mutation's writes, for rollback
}

// undoWrite is what one write replaced: id's row in rows (had: there was
// one) and whether the batch had written id before.
type undoWrite struct {
	id           string
	row          Row
	had, written bool
}

// checkpoint is the working set before a mutation: everything a write
// changes besides rows and written, which the undo log restores.
type checkpoint struct {
	sequence   uint64
	delta      CounterDelta
	liveBabies int
	order      int
}

// mark starts a mutation: its writes are logged from here, and rollback
// undoes them.
func (ws *workingSet) mark() checkpoint {
	ws.undo = ws.undo[:0]
	return checkpoint{sequence: ws.sequence, delta: ws.delta, liveBabies: ws.liveBabies, order: len(ws.order)}
}

// rollback undoes every write since mark returned c, so a mutation refused
// after its rule wrote (its receipt over the storage quota) leaves the
// written prefix as it was.
func (ws *workingSet) rollback(c checkpoint) {
	for i := len(ws.undo) - 1; i >= 0; i-- {
		u := ws.undo[i]
		if u.had {
			ws.rows[u.id] = u.row
		} else {
			delete(ws.rows, u.id)
		}
		ws.written[u.id] = u.written
	}
	ws.undo = ws.undo[:0]
	ws.sequence, ws.delta, ws.liveBabies, ws.order = c.sequence, c.delta, c.liveBabies, ws.order[:c.order]
}

func newWorkingSet(cmd BatchCommand, accepted string, fetched Fetched) *workingSet {
	ws := &workingSet{cmd: cmd, accepted: accepted, base: fetched.Family, rows: fetched.Rows, members: map[string]Row{},
		cascade: fetched.CascadeTargets, written: map[string]bool{}, receipts: fetched.Receipts, sequence: fetched.Family.Sequence,
		liveBabies: fetched.LiveBabies}
	for _, r := range fetched.SessionMembers {
		ws.members[r.ID] = r
	}
	// Live timers come whole (with their wire), so they are rows like any
	// other: a start's live-timer check and the session check read them.
	for _, r := range fetched.LiveTimers {
		if _, ok := ws.rows[r.ID]; !ok {
			ws.rows[r.ID] = r
		}
	}
	// The running pump comes whole, so it is a row like any other: an open
	// start's check reads it, and it is superseded with it.
	for _, r := range fetched.OpenPumps {
		if _, ok := ws.rows[r.ID]; !ok {
			ws.rows[r.ID] = r
		}
	}
	return ws
}

// Lookup is the current row of id: its latest write in this batch, or as
// fetched.
func (ws *workingSet) Lookup(id string) (Row, bool) {
	r, ok := ws.rows[id]
	return r, ok
}

// nextSequence is the sequence the next change takes.
func (ws *workingSet) nextSequence() (uint64, error) {
	if err := ws.claim(1); err != nil {
		return 0, err
	}
	return ws.sequence + 1, nil
}

// claim checks that the next n changes fit below the largest sequence a
// cursor can carry (the legacy journal reservation's bound).
func (ws *workingSet) claim(n uint64) error {
	if ws.sequence > protocol.MaxSafeInteger || n > protocol.MaxSafeInteger-ws.sequence {
		return fmt.Errorf("%w at %d", ErrSequenceExhausted, ws.sequence)
	}
	return nil
}

// admits reports whether the family stays within its record and storage
// quotas once next replaces previous (nil: a new row), counted on the
// family's counters plus the batch's delta so far.
func (ws *workingSet) admits(previous *Row, next Row) bool {
	return ws.admitsDelta(ws.delta.Replace(previous, next))
}

// admitsDelta reports whether the family stays within its quotas with the
// counter delta d over its fetched counters.
func (ws *workingSet) admitsDelta(d CounterDelta) bool {
	records := int64(ws.base.RecordCount) + d.Records
	charged := int64(ws.base.ChargedBytes) + d.ChargedBytes
	return records <= ws.cmd.Limits.MaxCanonicalRecordsPerFamily && charged <= ws.cmd.Limits.MaxChargedStorageBytesPerFamily
}

// write records next, which takes the next sequence, as the entity's
// current row.
func (ws *workingSet) write(previous *Row, next Row) error {
	if next.ChangedSeq != ws.sequence+1 {
		return fmt.Errorf("care: %s written at sequence %d after %d", next.ID, next.ChangedSeq, ws.sequence)
	}
	stored, had := ws.rows[next.ID]
	ws.undo = append(ws.undo, undoWrite{id: next.ID, row: stored, had: had, written: ws.written[next.ID]})
	ws.sequence = next.ChangedSeq
	ws.delta = ws.delta.Replace(previous, next)
	if next.Kind == "baby" {
		wasLive := previous != nil && previous.Live()
		switch {
		case next.Live() && !wasLive:
			ws.liveBabies++
		case !next.Live() && wasLive:
			ws.liveBabies--
		}
	}
	if !ws.written[next.ID] {
		ws.written[next.ID] = true
		ws.order = append(ws.order, next.ID)
	}
	ws.rows[next.ID] = next
	return nil
}

// addReceipt records outcome as m's receipt and charges it
// (ReceiptChargedBytes; Ruling R1-I2). As the legacy ChargeReceipt, the
// charge is refused (fatal ErrQuotaExceeded, nothing recorded) when the
// family's totals after it would exceed a quota, except for an applied
// delete, which released its reserve and is never refused. A later
// mutation of the batch with the same id replays the receipt as the stored
// receipt would replay: decoded from its JSON.
func (ws *workingSet) addReceipt(m protocol.Mutation, hash [32]byte, outcome protocol.Outcome) (fatal, err error) {
	raw, err := json.Marshal(outcome)
	if err != nil {
		return nil, fmt.Errorf("care: receipt %s outcome: %w", m.MutationID, err)
	}
	delta := ws.delta
	delta.ChargedBytes += ReceiptChargedBytes(raw)
	if !appliedDelete(m, outcome) && !ws.admitsDelta(delta) {
		return ErrQuotaExceeded, nil
	}
	var stored protocol.Outcome
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("care: receipt %s outcome: %w", m.MutationID, err)
	}
	ws.delta = delta
	ws.receipts[m.MutationID] = StoredReceipt{SameFamily: true, Hash: hash, Outcome: stored}
	ws.receiptRows = append(ws.receiptRows, ReceiptRow{DeviceID: ws.cmd.DeviceID, MutationID: m.MutationID, AccountID: ws.cmd.Grant.AccountID,
		Hash: hash, Outcome: outcome, AcceptedAt: ws.cmd.AcceptedAt})
	return nil, nil
}

// appliedDelete reports whether outcome applied m as a delete that
// tombstoned its entity (the legacy deletionReleasedReserve): its receipt
// is charged but never refused, so a family over its quota can still free
// space.
func appliedDelete(m protocol.Mutation, outcome protocol.Outcome) bool {
	return m.Operation == "delete" && outcome.Status == "applied" && outcome.Cursor != nil && outcome.CanonicalEntity != nil &&
		outcome.CanonicalEntity.Meta.TombstonedAt != nil
}

// writes is the batch's write set: the final row of every written entity,
// the receipts, the sequence and the counter delta over the fetched state.
func (ws *workingSet) writes() Writes {
	rows := make([]Row, len(ws.order))
	for i, id := range ws.order {
		rows[i] = ws.rows[id]
	}
	return Writes{FamilyID: ws.cmd.Grant.FamilyID, Base: ws.base, Sequence: ws.sequence, Rows: rows, Receipts: ws.receiptRows, Delta: ws.delta}
}

// openPump is the family's running pump other than excluding, in its
// current state: the fetched one, or one this batch started and has not
// ended. The family lock Fetch took makes this the whole rule; the unique
// index care_entities_one_open_pump only backs it.
func (ws *workingSet) openPump(excluding string) (Row, bool) {
	for _, r := range ws.rows {
		if r.Kind == "pump" && r.Live() && r.EndedAt == nil && r.ID != excluding {
			return r, true
		}
	}
	return Row{}, false
}

// anyRow visits every row the batch can see in its current state: the
// fetched and written rows, then the fetched session members not shadowed
// by them. visit returning true stops the walk and reports true.
func (ws *workingSet) anyRow(visit func(Row) bool) bool {
	for _, r := range ws.rows {
		if visit(r) {
			return true
		}
	}
	for id, r := range ws.members {
		if _, shadowed := ws.rows[id]; !shadowed && visit(r) {
			return true
		}
	}
	return false
}
