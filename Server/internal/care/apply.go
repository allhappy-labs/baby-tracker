package care

import (
	"context"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrMutationIDReused refuses a mutation whose (device, mutation id)
	// receipt holds another mutation: another payload, or the same id sent
	// in another family.
	ErrMutationIDReused = errors.New("mutation_id_reused")
	// ErrFeatureUnavailable refuses an import-tagged mutation where imports
	// are not admitted.
	ErrFeatureUnavailable = errors.New("feature_unavailable")
	// ErrSequenceExhausted refuses a change past the largest sequence a
	// cursor can carry (2^53-1).
	ErrSequenceExhausted = errors.New("care: family sequence exhausted")
	// ErrReadOnlyCaregiver refuses a viewer's mutation: a viewer reads
	// everything and originates nothing. A Policy returns it where a
	// caregiver would be allowed the action.
	ErrReadOnlyCaregiver = errors.New("read_only_caregiver")
)

// Policy decides whether role may perform action (ActionFor). A refusal is
// the batch's Fatal error at that mutation. A nil Policy checks no role, for
// server-driven batches; ActionFor still refuses an unknown kind or
// operation.
type Policy func(role, action string) error

// View is the batch's working set: the rows Fetch read with every earlier
// write of this batch applied over them.
type View interface {
	Lookup(id string) (Row, bool)
}

// ImportAdmission decides an import-tagged mutation. handled false sends it
// through the ordinary record rules.
type ImportAdmission interface {
	AdmitRecord(ctx context.Context, tx pgx.Tx, g access.Grant, deviceID string, m protocol.Mutation, view View) (protocol.Outcome, bool, error)
}

// Publisher announces a family's new high water after the commit.
type Publisher interface {
	Publish(ctx context.Context, familyID string, cursor protocol.Cursor) error
}

// BatchCommand is one device's sync batch. The family and role come from
// the Grant; AcceptedAt is the acceptance time of every change.
type BatchCommand struct {
	Grant      access.Grant
	DeviceID   string
	Mutations  []protocol.Mutation
	AcceptedAt time.Time
	Server     protocol.ServerInfo
	Limits     protocol.Limits
	Policy     Policy
	Imports    ImportAdmission
	// NoReceipts marks a server-built batch (the importer's baby
	// publication): no device sent its mutations, so none is receipted and
	// no stored receipt is consulted, as the legacy publication read and
	// wrote none (a client that reused a publication's derivable mutation id
	// cannot replay into or refuse it). Its device need not be the grant
	// account's visible active device.
	NoReceipts bool
}

// PullRequest asks ApplyBatch for the changes after After, at most Limit,
// read in the write round trip.
type PullRequest struct {
	After uint64
	Limit int
}

// BatchResult is what ApplyBatch wrote.
type BatchResult struct {
	Outcomes        []protocol.Outcome // the written prefix, in mutation order
	Base, HighWater uint64             // families.next_sequence before and after
	Changes         []ChangeRow        // the pull read (After, HighWater], when requested, After <= HighWater and Fatal == nil
	Fatal           error              // the first refusing error; everything before it is written
}

// ActionFor is the family policy action a mutation needs.
func ActionFor(m protocol.Mutation) (string, error) {
	if m.EntityKind == "baby" {
		switch m.Operation {
		case "create":
			return "add_baby", nil
		case "delete":
			return "delete_baby", nil
		case "update":
			return "mutate_record", nil
		}
	}
	switch m.EntityKind {
	case "feed", "sleep", "diaper", "supplement", "growth_profile", "measurement":
		if m.Operation == "create" || m.Operation == "update" || m.Operation == "delete" {
			return "mutate_record", nil
		}
	case "timer":
		return "mutate_record", nil
	}
	return "", protocol.ErrInvalidMutation
}

// ApplyBatch applies a sync batch in the caller's transaction, in exactly
// two round trips whatever its size: Fetch locks the family row and reads
// everything the batch references, every mutation is decided in Go against
// the working set (the fetched rows with the batch's earlier writes over
// them), and one pipelined batch writes the final row of every changed
// entity, the receipts, the family's sequence and counters, and reads the
// pull page. Import-tagged mutations add whatever round trips
// ImportAdmission.AdmitRecord makes for them.
//
// Each mutation is decided as its own legacy transaction decided it
// (the legacy Repository.Apply under sync.Service.Apply), in order. A mutation
// the legacy path refused with an error (policy, a reused mutation id,
// imports unavailable, a quota) stops the batch: it is BatchResult.Fatal,
// and the mutations before it are written with their receipts, as the
// legacy per-mutation transactions left them committed. An error return is
// an infrastructure failure; the caller rolls the transaction back.
func ApplyBatch(ctx context.Context, tx pgx.Tx, cmd BatchCommand, pull *PullRequest) (BatchResult, error) {
	accepted, err := protocol.NormalizeTimestamp(cmd.AcceptedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return BatchResult{}, fmt.Errorf("care: acceptance time: %w", err)
	}
	fetched, err := Fetch(ctx, tx, cmd.Grant.FamilyID, collectRefs(cmd)) // round trip 1
	if err != nil {
		return BatchResult{}, err
	}
	ws := newWorkingSet(cmd, accepted, fetched)
	result := BatchResult{Base: fetched.Family.Sequence, Outcomes: make([]protocol.Outcome, 0, len(cmd.Mutations))}
	for _, m := range cmd.Mutations {
		outcome, fatal, err := ws.apply(ctx, tx, m)
		if err != nil {
			return BatchResult{}, err
		}
		if fatal != nil {
			result.Fatal = fatal
			break
		}
		result.Outcomes = append(result.Outcomes, outcome)
	}
	result.HighWater = ws.sequence

	batch := &pgx.Batch{}
	check := QueueWrites(batch, ws.writes())
	var readPull func(pgx.BatchResults) ([]ChangeRow, error)
	if pull != nil && result.Fatal == nil && pull.After <= result.HighWater {
		readPull = QueueChanges(batch, cmd.Grant.FamilyID, pull.After, result.HighWater, pull.Limit)
	}
	if batch.Len() == 0 {
		// Nothing to write and no pull: a check that failed before queuing
		// still reports its error.
		if err := check(nil); err != nil {
			return BatchResult{}, err
		}
		return result, nil
	}
	results := tx.SendBatch(ctx, batch) // round trip 2
	if err := check(results); err != nil {
		_ = results.Close()
		return BatchResult{}, err
	}
	if readPull != nil {
		if result.Changes, err = readPull(results); err != nil {
			_ = results.Close()
			return BatchResult{}, err
		}
	}
	if err := results.Close(); err != nil {
		return BatchResult{}, err
	}
	return result, nil
}

// apply decides one mutation: its action and the policy, the receipt,
// import admission, then the record rules. fatal stops the batch before m;
// err is an infrastructure failure.
func (ws *workingSet) apply(ctx context.Context, tx pgx.Tx, m protocol.Mutation) (outcome protocol.Outcome, fatal, err error) {
	// An unknown kind or operation stops the batch, as the legacy actionFor
	// did, whether or not a policy checks the action.
	action, err := ActionFor(m)
	if err != nil {
		return protocol.Outcome{}, err, nil
	}
	if ws.cmd.Policy != nil {
		if err := ws.cmd.Policy(ws.cmd.Grant.Role, action); err != nil {
			return protocol.Outcome{}, err, nil
		}
	}
	if !protocol.ValidUUID(m.MutationID) {
		return protocol.Outcome{}, fmt.Errorf("%w: mutation id %q", protocol.ErrInvalidMutation, m.MutationID), nil
	}
	hash, err := protocol.CanonicalMutationHash(m)
	if err != nil {
		return protocol.Outcome{}, err, nil
	}
	if stored, ok := ws.receipts[m.MutationID]; ok && !ws.cmd.NoReceipts {
		// The receipt read admits the account's receipts from another
		// family; only this family's receipt of this very mutation replays.
		if !stored.SameFamily || stored.Hash != hash {
			return protocol.Outcome{}, ErrMutationIDReused, nil
		}
		replay := stored.Outcome
		original := replay.Status
		replay.Status, replay.OriginalStatus = "already_applied", &original
		return replay, nil, nil
	}
	mark := ws.mark()
	handled := false
	if m.ImportID != nil {
		if ws.cmd.Imports == nil {
			return protocol.Outcome{}, ErrFeatureUnavailable, nil
		}
		outcome, handled, err = ws.cmd.Imports.AdmitRecord(ctx, tx, ws.cmd.Grant, ws.cmd.DeviceID, m, ws)
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
	}
	if !handled {
		if outcome, fatal, err = ws.applyRecord(m); err != nil || fatal != nil {
			return protocol.Outcome{}, fatal, err
		}
	}
	// A server-built batch (NoReceipts) writes no receipt, so charges none.
	if !ws.cmd.NoReceipts {
		fatal, err := ws.addReceipt(m, hash, outcome)
		if err != nil {
			return protocol.Outcome{}, nil, err
		}
		if fatal != nil {
			// The receipt is over the quota: m is refused whole, its
			// writes undone, as the legacy mutation transaction rolled back.
			ws.rollback(mark)
			return protocol.Outcome{}, fatal, nil
		}
	}
	return outcome, nil, nil
}
