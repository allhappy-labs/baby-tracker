package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
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

func collectArchive(ctx context.Context, tx pgx.Tx, grant access.Grant, identity protocol.ServerInfo, now time.Time) (ArchiveV1, error) {
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
	if err := records.NewRepository().StreamCanonical(ctx, tx, grant.FamilyID, func(e protocol.CanonicalEntity) error {
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
		case "timer":
			a.ActiveTimers = append(a.ActiveTimers, e)
		default:
			return fmt.Errorf("unknown export entity kind %q", e.EntityKind)
		}
		if len(a.Babies)+len(a.Feeds)+len(a.Sleeps)+len(a.Diapers)+len(a.Supplements)+len(a.GrowthProfiles)+len(a.Measurements)+len(a.ActiveTimers) > 250000 {
			return ErrTooLarge
		}
		return nil
	}); err != nil {
		return a, err
	}
	rows, err = tx.Query(ctx, `SELECT sequence,entity_kind,entity_id::text,entity_version,actor_account_id::text,device_id::text,operation,
		canonical_entity,accepted_at FROM public.mutation_journal WHERE family_id=$1 AND sequence<=$2 ORDER BY sequence`, grant.FamilyID, high)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var j protocol.JournalChange
		var raw []byte
		var accepted time.Time
		if err := rows.Scan(&j.Cursor.Sequence, &j.EntityKind, &j.EntityID, &j.EntityVersion, &j.ActorAccountID, &j.DeviceID, &j.Operation, &raw, &accepted); err != nil {
			rows.Close()
			return a, err
		}
		j.Cursor.ServerInstanceID = identity.InstanceID
		j.Cursor.RecoveryEpoch = identity.RecoveryEpoch
		j.Cursor.FamilyID = grant.FamilyID
		j.AcceptedAt = accepted.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
		if err := json.Unmarshal(raw, &j.SnapshotOrTombstone); err != nil {
			rows.Close()
			return a, err
		}
		if err := charge(j); err != nil {
			rows.Close()
			return a, err
		}
		a.Journal = append(a.Journal, j)
		if len(a.Journal) > 250000 {
			rows.Close()
			return a, ErrTooLarge
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return a, err
	}
	rows, err = tx.Query(ctx, `SELECT revision,kind,actor_account_id::text,at FROM app.export_family_audit($1)`, grant.FamilyID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var item ArchiveAudit
		if err := rows.Scan(&item.Revision, &item.Kind, &item.ActorAccountID, &item.At); err != nil {
			rows.Close()
			return a, err
		}
		at, err := time.Parse(time.RFC3339Nano, item.At)
		if err != nil {
			rows.Close()
			return a, err
		}
		item.At = at.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
		if err := charge(item); err != nil {
			rows.Close()
			return a, err
		}
		a.Audit = append(a.Audit, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return a, err
	}
	if len(a.Members) == 0 {
		return a, errors.New("export membership projection missing")
	}
	return a, nil
}
