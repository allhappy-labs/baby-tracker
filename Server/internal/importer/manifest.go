package importer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"babytracker/server/internal/access"
	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

func parent(payload protocol.Payload) string {
	switch p := payload.(type) {
	case protocol.FeedPayload:
		return p.BabyID
	case protocol.SleepPayload:
		return p.BabyID
	case protocol.GrowthProfilePayload:
		return p.BabyID
	case protocol.MeasurementPayload:
		return p.BabyID
	case protocol.SupplementPayload:
		return p.BabyID
	case protocol.DiaperPayload:
		return p.BabyID
	case protocol.TimerStartPayload:
		return p.BabyID
	}
	return ""
}

// compare judges a manifest record against the stored entity of its id
// (found false: none, the record may be inserted): equal content, or the
// conflict a deleted or different entity is.
func (s *Service) compare(family string, row care.Row, found bool, kind, baby string, hash []byte) (*protocol.CanonicalEntity, *protocol.Cursor, string, error) {
	if !found {
		return nil, nil, "", nil
	}
	existingKind := row.Kind
	entity, e := row.Entity()
	if e != nil {
		return nil, nil, "", e
	}
	cursor := protocol.Cursor{ServerInstanceID: s.info.InstanceID, RecoveryEpoch: s.info.RecoveryEpoch, FamilyID: family, Sequence: row.ChangedSeq}
	if entity.Meta.TombstonedAt != nil {
		return &entity, &cursor, "record_deleted_conflict", nil
	}
	if existingKind != kind || parent(entity.Payload) != baby {
		return &entity, &cursor, "record_content_conflict", nil
	}
	canonical, e := protocol.CanonicalContentHash(kind, entity.Payload)
	if e != nil {
		return nil, nil, "", e
	}
	if !bytes.Equal(canonical[:], hash) {
		return &entity, &cursor, "record_content_conflict", nil
	}
	return &entity, &cursor, "", nil
}

// mappings is the import's local baby id to shared baby id mapping.
func (s *Service) mappings(ctx context.Context, tx pgx.Tx, family, importID string) (map[string]string, error) {
	rows, e := tx.Query(ctx, `SELECT local_baby_id::text,shared_baby_id::text FROM import_baby_mappings WHERE family_id=$1 AND import_id=$2`, family, importID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	shared := map[string]string{}
	for rows.Next() {
		var local, baby string
		if e := rows.Scan(&local, &baby); e != nil {
			return nil, e
		}
		shared[local] = baby
	}
	return shared, rows.Err()
}

func (s *Service) recordConflicts(ctx context.Context, tx pgx.Tx, family string, v session, pending []protocol.RecordManifest) ([]conflict, error) {
	result := []conflict{}
	if !v.mapped {
		return result, nil
	}
	rows, e := tx.Query(ctx, `SELECT record_id::text,entity_kind,local_baby_id::text,encode(content_sha256,'hex') FROM import_record_manifests WHERE family_id=$1 AND import_id=$2 ORDER BY import_record_manifests.record_id`, family, v.id)
	if e != nil {
		return nil, e
	}
	manifest := append([]protocol.RecordManifest{}, pending...)
	for rows.Next() {
		var m protocol.RecordManifest
		if e = rows.Scan(&m.RecordID, &m.EntityKind, &m.LocalBabyID, &m.ContentSHA256); e != nil {
			rows.Close()
			return nil, e
		}
		manifest = append(manifest, m)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	// The import's mappings, read once: an import maps at most
	// ImportMaxBabies babies, and a manifest may hold ImportMaxRecords
	// records, each looked up here under the family lock.
	shared, e := s.mappings(ctx, tx, family, v.id)
	if e != nil {
		return nil, e
	}
	type target struct {
		manifest protocol.RecordManifest
		id, baby string
	}
	targets := make([]target, 0, len(manifest))
	ids := make([]string, 0, len(manifest))
	for _, m := range manifest {
		if m.EntityKind == "timer" {
			continue
		}
		baby, ok := shared[m.LocalBabyID]
		if !ok {
			// A mapped import maps every local baby its manifest names (the
			// manifest's babies are the import's candidates, and publication
			// maps them all), as the per-record lookup's ErrNoRows asserted.
			return nil, fmt.Errorf("importer: import %s maps no local baby %s: %w", v.id, m.LocalBabyID, pgx.ErrNoRows)
		}
		targetID := m.RecordID
		if m.EntityKind == "growth_profile" {
			targetID = protocol.GrowthProfileID(baby)
		}
		targets, ids = append(targets, target{m, targetID, baby}), append(ids, targetID)
	}
	stored, e := care.GetMany(ctx, tx, family, ids)
	if e != nil {
		return nil, e
	}
	for _, t := range targets {
		m := t.manifest
		hash, _ := hex.DecodeString(m.ContentSHA256)
		row, found := stored[t.id]
		_, _, reason, e := s.compare(family, row, found, m.EntityKind, t.baby, hash)
		if e != nil {
			return nil, e
		}
		if reason != "" {
			id := m.RecordID
			result = append(result, conflict{ImportConflict: protocol.ImportConflict{ConflictID: deterministicID(v.id, m.RecordID, "record"), Kind: reason, LocalBabyID: m.LocalBabyID, RecordID: &id, CandidateBabyIDs: []string{}, CandidateVersions: []uint64{}}})
		}
	}
	return result, nil
}

// AdmitRecord executes inside care.ApplyBatch, after the mutation receipt
// lookup and under the family lock. A handled equal record is a no-op;
// absent records proceed to ordinary insertion. Timer starts always retain
// ordinary timer collision semantics. Care entities are read only through
// view, the batch's working set (the batch fetched every import mutation's
// entity and payload baby); the import tables are read here, one or two
// statements per mutation.
func (s *Service) AdmitRecord(ctx context.Context, tx pgx.Tx, g access.Grant, device string, m protocol.Mutation, view care.View) (protocol.Outcome, bool, error) {
	if m.ImportID == nil {
		return protocol.Outcome{}, false, nil
	}
	reject := func(reason string) (protocol.Outcome, bool, error) {
		return protocol.Outcome{MutationID: m.MutationID, Status: "rejected", Code: &reason, RelatedEntities: []protocol.CanonicalEntity{}}, true, nil
	}
	if (m.EntityKind == "timer" && m.Operation != "timer_start") || (m.EntityKind != "timer" && (m.Operation != "create" || m.EntityKind == "baby")) {
		return reject("invalid_import_operation")
	}
	if care.ValidateMutation(m, s.limits) != nil {
		return reject("invalid_mutation")
	}
	v, e := loadSession(ctx, tx, g.FamilyID, *m.ImportID)
	if errors.Is(e, pgx.ErrNoRows) {
		return reject("import_not_found")
	}
	if e != nil {
		return protocol.Outcome{}, true, e
	}
	if v.account != g.AccountID {
		return reject("import_access_denied")
	}
	expired, e := s.expired(ctx, tx, g.FamilyID, v)
	if e != nil {
		return protocol.Outcome{}, true, e
	}
	if expired {
		return reject("import_expired")
	}
	if !v.mapped {
		return reject("import_mapping_incomplete")
	}
	var kind, baby string
	var hash []byte
	if m.EntityKind == "growth_profile" {
		rows, err := tx.Query(ctx, `SELECT m.entity_kind,b.local_baby_id::text,b.shared_baby_id::text,m.record_id::text,m.content_sha256 FROM import_record_manifests m JOIN import_baby_mappings b USING(family_id,import_id,local_baby_id) WHERE m.family_id=$1 AND m.import_id=$2 AND m.entity_kind='growth_profile'`, g.FamilyID, v.id)
		if err != nil {
			return protocol.Outcome{}, true, err
		}
		e = pgx.ErrNoRows
		for rows.Next() {
			var local, shared, id, k string
			var digest []byte
			if err := rows.Scan(&k, &local, &shared, &id, &digest); err != nil {
				rows.Close()
				return protocol.Outcome{}, true, err
			}
			if m.EntityID == protocol.GrowthProfileID(shared) && id == protocol.GrowthProfileID(local) {
				kind, baby, hash, e = k, shared, digest, nil
				break
			}
		}
		rowErr := rows.Err()
		rows.Close()
		if rowErr != nil {
			return protocol.Outcome{}, true, rowErr
		}
	} else {
		e = tx.QueryRow(ctx, `SELECT m.entity_kind,b.shared_baby_id::text,m.content_sha256 FROM import_record_manifests m JOIN import_baby_mappings b USING(family_id,import_id,local_baby_id) WHERE m.family_id=$1 AND m.import_id=$2 AND m.record_id=$3`, g.FamilyID, v.id, m.EntityID).Scan(&kind, &baby, &hash)
	}
	if errors.Is(e, pgx.ErrNoRows) {
		return reject("import_manifest_mismatch")
	}
	if e != nil {
		return protocol.Outcome{}, true, e
	}
	actual, e := protocol.CanonicalContentHash(m.EntityKind, m.Payload)
	if e != nil || kind != m.EntityKind || baby != parent(m.Payload) || !bytes.Equal(actual[:], hash) {
		return reject("import_manifest_mismatch")
	}
	// The mapping's shared baby is stored (the mapping's key references it).
	stored, ok := view.Lookup(baby)
	if !ok || stored.Kind != "baby" {
		return protocol.Outcome{}, true, fmt.Errorf("importer: mapped baby %s is not a stored baby", baby)
	}
	if !stored.Live() {
		return reject("baby_deleted")
	}
	if m.EntityKind == "timer" {
		return protocol.Outcome{}, false, nil
	}
	row, found := view.Lookup(m.EntityID)
	entity, cursor, reason, e := s.compare(g.FamilyID, row, found, m.EntityKind, baby, hash)
	if e != nil {
		return protocol.Outcome{}, true, e
	}
	if reason != "" {
		return reject(reason)
	}
	if entity == nil {
		return protocol.Outcome{}, false, nil
	}
	return protocol.Outcome{MutationID: m.MutationID, Status: "applied", CanonicalEntity: entity, Cursor: cursor, RelatedEntities: []protocol.CanonicalEntity{}}, true, nil
}
