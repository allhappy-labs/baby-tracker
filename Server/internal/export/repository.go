package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

type dbChunkSink struct {
	tx  pgx.Tx
	job Job
}

func (sink dbChunkSink) Put(ctx context.Context, c Chunk) error {
	_, err := sink.tx.Exec(ctx, `INSERT INTO public.export_chunks(family_id,export_id,attempt,chunk_index,plaintext_length,is_final,ciphertext)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, sink.job.FamilyID, sink.job.ID, sink.job.Attempt, c.Index, c.PlaintextLength, c.Final, c.Ciphertext)
	return err
}

// auditPager reads one page of a family's audit chain
// (authority.Service.FamilyAuditPage).
type auditPager func(ctx context.Context, familyID string, afterSeq, throughSeq uint64, limit int) ([]authority.AuditEntry, error)

// careSource is where the archive's care data comes from: every entity of
// the family (tombstones included), and the change list through a sequence.
// Both read tx, the archive's one snapshot.
type careSource interface {
	entities(ctx context.Context, tx pgx.Tx, familyID string, visit func(protocol.CanonicalEntity) error) error
	journal(ctx context.Context, tx pgx.Tx, familyID string, through uint64, identity protocol.ServerInfo, visit func(protocol.JournalChange) error) error
}

// collectArchive reads the family archive: care data from tx through
// source, a read-only snapshot under the requester's tenant context, and the
// audit section through audit, up to grant.Revision.
func collectArchive(ctx context.Context, tx pgx.Tx, grant access.Grant, identity protocol.ServerInfo, now time.Time, audit auditPager,
	source careSource) (ArchiveV1, error) {
	// Keep the temporary typed snapshot bounded even before EncodeArchive
	// applies its exact 50 MiB wire cap. Each retained item is charged at its
	// JSON size plus array punctuation.
	var charged uint64 = 4096
	charge := func(value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		charged += uint64(len(raw)) + 1
		if charged > MaxArchivePlaintextBytes {
			return ErrTooLarge
		}
		return nil
	}
	a := ArchiveV1{Version: "v1", ServerInstanceID: identity.InstanceID, RecoveryEpoch: identity.RecoveryEpoch,
		FamilyID: grant.FamilyID, CapturedAt: now.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z"),
		SecurityRevision: grant.Revision, Members: []ArchiveMember{}, Babies: []protocol.CanonicalEntity{}, Feeds: []protocol.CanonicalEntity{},
		Sleeps: []protocol.CanonicalEntity{}, Diapers: []protocol.CanonicalEntity{}, Supplements: []protocol.CanonicalEntity{}, GrowthProfiles: []protocol.CanonicalEntity{}, Measurements: []protocol.CanonicalEntity{},
		Immunizations: []protocol.CanonicalEntity{}, Pumps: []protocol.CanonicalEntity{},
		ActiveTimers: []protocol.CanonicalEntity{}, Journal: []protocol.JournalChange{}, Audit: []ArchiveAudit{},
		Attachments: AttachmentManifest{Version: "v1", Attachments: []protocol.AttachmentMetadata{}}}
	var high uint64
	if err := tx.QueryRow(ctx, `SELECT next_sequence FROM public.families WHERE id=$1`, grant.FamilyID).Scan(&high); err != nil {
		return a, err
	}
	a.HighWater = protocol.Cursor{ServerInstanceID: identity.InstanceID, RecoveryEpoch: identity.RecoveryEpoch, FamilyID: grant.FamilyID, Sequence: high}
	rows, err := tx.Query(ctx, `SELECT m.account_id::text,CASE WHEN m.status='removed' OR l.former THEN 'Former caregiver' ELSE l.display_name END,
		m.role,m.status FROM public.memberships m JOIN public.family_actor_labels l
		ON l.family_id=m.family_id AND l.account_id=m.account_id WHERE m.family_id=$1 ORDER BY m.account_id`, grant.FamilyID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var m ArchiveMember
		if err := rows.Scan(&m.AccountID, &m.DisplayName, &m.Role, &m.Status); err != nil {
			rows.Close()
			return a, err
		}
		a.Members = append(a.Members, m)
		if err := charge(m); err != nil {
			rows.Close()
			return a, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return a, err
	}
	if err := source.entities(ctx, tx, grant.FamilyID, func(e protocol.CanonicalEntity) error {
		if err := charge(e); err != nil {
			return err
		}
		switch e.EntityKind {
		case "baby":
			a.Babies = append(a.Babies, e)
		case "feed":
			a.Feeds = append(a.Feeds, e)
		case "sleep":
			a.Sleeps = append(a.Sleeps, e)
		case "diaper":
			a.Diapers = append(a.Diapers, e)
		case "supplement":
			a.Supplements = append(a.Supplements, e)
		case "growth_profile":
			a.GrowthProfiles = append(a.GrowthProfiles, e)
		case "measurement":
			a.Measurements = append(a.Measurements, e)
		case "immunization":
			a.Immunizations = append(a.Immunizations, e)
		case "pump":
			a.Pumps = append(a.Pumps, e)
		case "timer":
			a.ActiveTimers = append(a.ActiveTimers, e)
		default:
			return fmt.Errorf("unknown export entity kind %q", e.EntityKind)
		}
		if len(a.Babies)+len(a.Feeds)+len(a.Sleeps)+len(a.Diapers)+len(a.Supplements)+len(a.GrowthProfiles)+len(a.Measurements)+len(a.Immunizations)+len(a.Pumps)+len(a.ActiveTimers) > 250000 {
			return ErrTooLarge
		}
		return nil
	}); err != nil {
		return a, err
	}
	if err := source.journal(ctx, tx, grant.FamilyID, high, identity, func(j protocol.JournalChange) error {
		if err := charge(j); err != nil {
			return err
		}
		a.Journal = append(a.Journal, j)
		if len(a.Journal) > 250000 {
			return ErrTooLarge
		}
		return nil
	}); err != nil {
		return a, err
	}
	// The audit section is the family's own chain through the revision the
	// snapshot saw (grant.Revision, families.audit_seq), read page by page in
	// authority transactions: audit rows are authority-only under RLS, and
	// they are append-only, so rows at or below that seq are the ones the
	// snapshot's family state follows from.
	for after := uint64(0); after < grant.Revision; {
		page, err := audit(ctx, grant.FamilyID, after, grant.Revision, authority.MaxAuditPageRows)
		if err != nil {
			return a, err
		}
		for _, entry := range page {
			if entry.Seq <= after || entry.Seq > grant.Revision {
				return a, fmt.Errorf("export audit page out of order: %w", ErrUnavailable)
			}
			after = entry.Seq
			item := ArchiveAudit{Revision: entry.Seq, Kind: entry.Kind, ActorAccountID: entry.ActorAccountID,
				At: entry.At.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")}
			if err := charge(item); err != nil {
				return a, err
			}
			a.Audit = append(a.Audit, item)
		}
		if len(page) < authority.MaxAuditPageRows {
			break
		}
	}
	if len(a.Members) == 0 {
		return a, errors.New("export membership projection missing")
	}
	return a, nil
}
