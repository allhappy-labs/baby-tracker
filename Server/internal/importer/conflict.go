package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"github.com/jackc/pgx/v5"
)

type conflict struct {
	protocol.ImportConflict
	fingerprint    []byte
	choice, target *string
}
type candidate struct {
	protocol.ImportBaby
	matches   []string
	versions  []uint64
	target    string
	collision bool
	existing  *conflict
}

func (s *Service) conflicts(ctx context.Context, tx pgx.Tx, family, id string) ([]conflict, error) {
	rows, e := tx.Query(ctx, `SELECT conflict_id::text,kind,local_baby_id::text,record_id::text,candidate_baby_ids::text[],candidate_versions,candidate_fingerprint,decision_choice,decision_canonical_baby_id::text FROM import_conflicts WHERE family_id=$1 AND import_id=$2 AND record_id IS NULL ORDER BY conflict_id`, family, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []conflict{}
	for rows.Next() {
		var c conflict
		if e = rows.Scan(&c.ConflictID, &c.Kind, &c.LocalBabyID, &c.RecordID, &c.CandidateBabyIDs, &c.CandidateVersions, &c.fingerprint, &c.choice, &c.target); e != nil {
			return nil, e
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
func (s *Service) saveConflict(ctx context.Context, tx pgx.Tx, family string, v session, c conflict) error {
	raw, _ := json.Marshal(c.ImportConflict)
	size := int64(len(raw) + 320)
	var old int64
	e := tx.QueryRow(ctx, `SELECT storage_bytes FROM import_conflicts WHERE family_id=$1 AND import_id=$2 AND conflict_id=$3`, family, v.id, c.ConflictID).Scan(&old)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	if len(c.fingerprint) != 32 {
		h := sha256.Sum256(raw)
		c.fingerprint = h[:]
	}
	_, e = tx.Exec(ctx, `INSERT INTO import_conflicts(family_id,import_id,conflict_id,kind,local_baby_id,record_id,candidate_baby_ids,candidate_versions,candidate_fingerprint,created_revision,updated_revision,storage_bytes) VALUES($1,$2,$3,$4,$5,$6,$7::uuid[],$8,$9,$10,$10,$11) ON CONFLICT(family_id,import_id,conflict_id) DO UPDATE SET kind=EXCLUDED.kind,candidate_baby_ids=EXCLUDED.candidate_baby_ids,candidate_versions=EXCLUDED.candidate_versions,candidate_fingerprint=EXCLUDED.candidate_fingerprint,updated_revision=EXCLUDED.updated_revision,storage_bytes=EXCLUDED.storage_bytes`, family, v.id, c.ConflictID, c.Kind, c.LocalBabyID, c.RecordID, c.CandidateBabyIDs, c.CandidateVersions, c.fingerprint, v.revision, size)
	if e != nil {
		return e
	}
	return s.repo.ChargeImport(ctx, tx, family, size-old, s.limits)
}

// resolveGraph reads every candidate before publishing any baby. The fingerprint
// includes all exact-match edges and occupied local UUIDs, so newly appearing
// candidates invalidate earlier decisions just as renames/deletions do.
func (s *Service) resolveGraph(ctx context.Context, tx pgx.Tx, g access.Grant, v *session) (bool, error) {
	if v.mapped {
		return false, nil
	}
	rows, e := tx.Query(ctx, `SELECT local_baby_id::text,name,birth_date::text,sort_order FROM import_baby_candidates WHERE family_id=$1 AND import_id=$2 ORDER BY local_baby_id`, g.FamilyID, v.id)
	if e != nil {
		return false, e
	}
	babies := []candidate{}
	for rows.Next() {
		var b candidate
		if e = rows.Scan(&b.LocalBabyID, &b.Name, &b.BirthDate, &b.SortOrder); e != nil {
			rows.Close()
			return false, e
		}
		babies = append(babies, b)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, e
	}
	old, e := s.conflicts(ctx, tx, g.FamilyID, v.id)
	if e != nil {
		return false, e
	}
	oldByLocal := map[string]*conflict{}
	for i := range old {
		if old[i].RecordID == nil {
			oldByLocal[old[i].LocalBabyID] = &old[i]
		}
	}
	graph := []any{}
	uses := map[string]int{}
	for i := range babies {
		b := &babies[i]
		b.matches = []string{}
		b.versions = []uint64{}
		b.existing = oldByLocal[b.LocalBabyID]
		n, _ := protocol.NormalizeBabyName(b.Name)
		if v.mode == "match_shared" {
			rs, e := tx.Query(ctx, `SELECT id::text,entity_version FROM babies WHERE family_id=$1 AND normalized_name=$2 AND birth_date=$3 AND normalization_version=$4 AND tombstoned_at IS NULL ORDER BY id`, g.FamilyID, n, b.BirthDate, v.normalization)
			if e != nil {
				return false, e
			}
			for rs.Next() {
				var id string
				var version uint64
				if e = rs.Scan(&id, &version); e != nil {
					rs.Close()
					return false, e
				}
				b.matches = append(b.matches, id)
				b.versions = append(b.versions, version)
			}
			e = rs.Err()
			rs.Close()
			if e != nil {
				return false, e
			}
		}
		kind, e := records.FindEntityKind(ctx, tx, g.FamilyID, b.LocalBabyID)
		if e != nil && !errors.Is(e, records.ErrNotFound) {
			return false, e
		}
		var local any
		if e == nil {
			entity, _, e := s.repo.GetCanonical(ctx, tx, kind, g.FamilyID, b.LocalBabyID)
			if e != nil {
				return false, e
			}
			local = entity
			b.collision = kind != "baby" || entity.Meta.TombstonedAt != nil
			if !b.collision {
				payload := entity.Payload.(protocol.BabyPayload)
				b.collision = payload.Name != b.Name || payload.BirthDate != b.BirthDate || payload.SortOrder != b.SortOrder
			}
		}
		graph = append(graph, []any{b.LocalBabyID, b.matches, b.versions, local})
		if len(b.matches) == 1 {
			uses[b.matches[0]]++
		}
	}
	raw, _ := json.Marshal(graph)
	fp := sha256.Sum256(raw)
	stale := false
	for _, c := range oldByLocal {
		if !bytes.Equal(c.fingerprint, fp[:]) {
			stale = true
		}
	}
	if stale {
		v.revision++
		_, e = tx.Exec(ctx, `UPDATE import_conflicts SET decision_choice=NULL,decision_canonical_baby_id=NULL,decision_account_id=NULL,decision_revision=NULL,decided_at=NULL,updated_revision=$3 WHERE family_id=$1 AND import_id=$2 AND record_id IS NULL`, g.FamilyID, v.id, v.revision)
		if e != nil {
			return false, e
		}
		for _, c := range oldByLocal {
			c.choice = nil
			c.target = nil
		}
		if e = s.revision(ctx, tx, g.FamilyID, *v); e != nil {
			return false, e
		}
	}
	unresolved := false
	targets := map[string]bool{}
	for i := range babies {
		b := &babies[i]
		kind := ""
		if len(b.matches) > 1 {
			kind = "ambiguous_baby"
		} else if len(b.matches) == 1 && uses[b.matches[0]] > 1 {
			kind = "ambiguous_local_babies"
		}
		b.target = b.LocalBabyID
		if v.mode == "match_shared" && len(b.matches) == 1 {
			b.target = b.matches[0]
		}
		if b.target == b.LocalBabyID && b.collision {
			kind = "baby_uuid_collision"
		}
		if b.existing != nil && kind == "" {
			kind = b.existing.Kind
		}
		if kind != "" {
			c := conflict{ImportConflict: protocol.ImportConflict{ConflictID: deterministicID(v.id, b.LocalBabyID, "baby"), Kind: kind, LocalBabyID: b.LocalBabyID, CandidateBabyIDs: b.matches, CandidateVersions: b.versions}, fingerprint: fp[:]}
			if e = s.saveConflict(ctx, tx, g.FamilyID, *v, c); e != nil {
				return false, e
			}
			if b.existing == nil || b.existing.choice == nil || kind == "baby_uuid_collision" {
				unresolved = true
				continue
			}
			if *b.existing.choice == "keep_local" {
				b.target = b.LocalBabyID
				if b.collision {
					unresolved = true
					continue
				}
			} else {
				if b.existing.target == nil || !contains(b.matches, *b.existing.target) {
					unresolved = true
					continue
				}
				b.target = *b.existing.target
			}
		}
		if targets[b.target] {
			return false, code("import_target_already_assigned")
		}
		targets[b.target] = true
	}
	if unresolved || stale {
		return stale, nil
	}
	// Publication is the sole mapping write point and runs only for a complete,
	// revalidated injective assignment. Ordinary record insertion journals babies.
	creator := g
	creator.AccountID = v.account
	for _, b := range babies {
		kind, e := records.FindEntityKind(ctx, tx, g.FamilyID, b.target)
		if e != nil && !errors.Is(e, records.ErrNotFound) {
			return false, e
		}
		if errors.Is(e, records.ErrNotFound) {
			m := protocol.Mutation{MutationID: deterministicID(v.id, b.LocalBabyID, "publication"), EntityID: b.target, EntityKind: "baby", Operation: "create", PayloadVersion: 1, ClientMutationTime: v.created.UTC().Format("2006-01-02T15:04:05.000000Z"), Payload: protocol.BabyPayload{Name: b.Name, BirthDate: b.BirthDate, SortOrder: b.SortOrder}}
			out, e := s.repo.Apply(ctx, tx, records.ApplyCommand{Grant: creator, DeviceID: v.device, Mutation: m, AcceptedAt: s.now(), ServerInstanceID: s.info.InstanceID, RecoveryEpoch: s.info.RecoveryEpoch, Limits: s.limits})
			if e != nil {
				return false, e
			}
			if out.Status != "applied" {
				return false, code("baby_uuid_collision")
			}
			kind = "baby"
		}
		entity, cursor, e := s.repo.GetCanonical(ctx, tx, kind, g.FamilyID, b.target)
		if e != nil {
			return false, e
		}
		_, e = tx.Exec(ctx, `INSERT INTO import_baby_mappings(family_id,import_id,local_baby_id,shared_baby_id,published_revision,shared_baby_entity_version,shared_baby_cursor,storage_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,192)`, g.FamilyID, v.id, b.LocalBabyID, b.target, v.revision, entity.Meta.EntityVersion, cursor.Sequence)
		if e != nil {
			return false, e
		}
		if e = s.repo.ChargeImport(ctx, tx, g.FamilyID, 192, s.limits); e != nil {
			return false, e
		}
	}
	v.mapped = true
	_, e = tx.Exec(ctx, `UPDATE import_sessions SET mapping_complete=true,mapping_completed_at=$3 WHERE family_id=$1 AND import_id=$2`, g.FamilyID, v.id, s.now())
	return false, e
}
func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
func (s *Service) revision(ctx context.Context, tx pgx.Tx, family string, v session) error {
	_, e := tx.Exec(ctx, `UPDATE import_sessions SET resolver_revision=$3 WHERE family_id=$1 AND import_id=$2`, family, v.id, v.revision)
	return e
}

func (s *Service) ResolveConflict(ctx context.Context, p access.Principal, id string, r protocol.ImportConflictRequest) (protocol.ImportResolveResponse, error) {
	var out protocol.ImportResolveResponse
	if !protocol.ValidUUID(id) || !protocol.ValidUUID(r.CommandID) || !protocol.ValidUUID(r.ConflictID) || r.ResolverRevision < 1 || r.ResolverRevision > protocol.MaxSafeInteger || r.ProtocolVersion != "v1" || protocol.ValidateRouteCapabilities(protocol.SurfaceImport, r.Capabilities, r.Capabilities, protocol.KnownCapabilities()) != nil || (r.Choice != "keep_local" && r.Choice != "use_existing") || r.Choice == "keep_local" && r.CanonicalBabyID != nil || r.Choice == "use_existing" && (r.CanonicalBabyID == nil || !protocol.ValidUUID(*r.CanonicalBabyID)) {
		return out, code("invalid_request")
	}
	family, e := s.family(ctx, p)
	if e != nil {
		return out, e
	}
	raw, _ := json.Marshal(r)
	hash := sha256.Sum256(raw)
	var resultErr error
	var publish *protocol.Cursor
	e = s.guard.WithAuthorizedTransaction(ctx, p, family, "add_baby", func(tx pgx.Tx, g access.Grant) error {
		if g.Role != "owner" {
			return code("owner_required")
		}
		if e := records.LockFamilyForUpdate(ctx, tx, family); e != nil {
			return e
		}
		var journalBefore uint64
		if e := tx.QueryRow(ctx, `SELECT next_sequence FROM families WHERE id=$1`, family).Scan(&journalBefore); e != nil {
			return e
		}
		v, e := loadSession(ctx, tx, family, id)
		if errors.Is(e, pgx.ErrNoRows) {
			return code("import_not_found")
		}
		if e != nil {
			return e
		}
		expired, e := s.expired(ctx, tx, family, v)
		if e != nil {
			return e
		}
		if expired {
			resultErr = code("import_expired")
			return nil
		}
		var savedHash, body []byte
		var account string
		e = tx.QueryRow(ctx, `SELECT command_sha256,response_body,account_id::text FROM import_conflict_receipts WHERE family_id=$1 AND import_id=$2 AND command_id=$3`, family, id, r.CommandID).Scan(&savedHash, &body, &account)
		if e == nil {
			if !bytes.Equal(savedHash, hash[:]) || account != p.AccountID {
				return code("command_id_reused")
			}
			return json.Unmarshal(body, &out)
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if r.ResolverRevision != v.revision {
			return code("import_resolution_stale")
		}
		if !v.complete || v.mapped {
			return code("import_resolution_stale")
		}
		stale, e := s.resolveGraph(ctx, tx, g, &v)
		if e != nil {
			return e
		}
		if stale {
			resultErr = code("import_resolution_stale")
			return nil
		}
		conflicts, e := s.conflicts(ctx, tx, family, id)
		if e != nil {
			return e
		}
		var selected *conflict
		for i := range conflicts {
			if conflicts[i].ConflictID == r.ConflictID {
				selected = &conflicts[i]
			}
		}
		if selected == nil || selected.RecordID != nil || selected.choice != nil {
			return code("import_resolution_stale")
		}
		if selected.Kind == "baby_uuid_collision" {
			return code("baby_uuid_collision")
		}
		target := selected.LocalBabyID
		if r.Choice == "use_existing" {
			target = *r.CanonicalBabyID
			if !contains(selected.CandidateBabyIDs, target) {
				return code("invalid_request")
			}
		}
		for _, c := range conflicts {
			if c.choice != nil {
				assigned := c.LocalBabyID
				if c.target != nil {
					assigned = *c.target
				}
				if assigned == target {
					return code("import_target_already_assigned")
				}
			}
		}
		if r.Choice == "keep_local" {
			kind, e := records.FindEntityKind(ctx, tx, family, target)
			if e == nil {
				if kind != "baby" {
					return code("baby_uuid_collision")
				}
				var b protocol.ImportBaby
				e = tx.QueryRow(ctx, `SELECT name,birth_date::text,sort_order FROM import_baby_candidates WHERE family_id=$1 AND import_id=$2 AND local_baby_id=$3`, family, id, target).Scan(&b.Name, &b.BirthDate, &b.SortOrder)
				if e != nil {
					return e
				}
				payload, meta, e := s.repo.GetBaby(ctx, tx, family, target)
				if e != nil {
					return e
				}
				if meta.TombstonedAt != nil || payload.Name != b.Name || payload.BirthDate != b.BirthDate || payload.SortOrder != b.SortOrder {
					return code("baby_uuid_collision")
				}
			} else if !errors.Is(e, records.ErrNotFound) {
				return e
			}
		}
		v.revision++
		if e = s.revision(ctx, tx, family, v); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE import_conflicts SET decision_choice=$4,decision_canonical_baby_id=$5,decision_account_id=$6,decision_revision=$7,decided_at=$8,updated_revision=$7 WHERE family_id=$1 AND import_id=$2 AND conflict_id=$3`, family, id, r.ConflictID, r.Choice, r.CanonicalBabyID, p.AccountID, v.revision, s.now())
		if e != nil {
			return e
		}
		if _, e = s.resolveGraph(ctx, tx, g, &v); e != nil {
			return e
		}
		pending, e := s.recordConflicts(ctx, tx, family, v, nil)
		if e != nil {
			return e
		}
		for _, c := range pending {
			if e = s.saveConflict(ctx, tx, family, v, c); e != nil {
				return e
			}
		}
		out, e = s.response(ctx, tx, p, family, v, nil, nil)
		if e != nil {
			return e
		}
		body, e = json.Marshal(out)
		if e != nil {
			return e
		}
		size := int64(len(body) + len(raw) + 256)
		_, e = tx.Exec(ctx, `INSERT INTO import_conflict_receipts(family_id,import_id,command_id,account_id,device_id,command_sha256,resolver_revision,response_status,response_body,storage_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,200,$8,$9)`, family, id, r.CommandID, p.AccountID, p.DeviceID, hash[:], v.revision, body, size)
		if e != nil {
			return e
		}
		if e = s.repo.ChargeImport(ctx, tx, family, size, s.limits); e != nil {
			return e
		}
		return s.observeJournalAdvance(ctx, tx, g, p.DeviceID, journalBefore, &publish)
	})
	if e != nil {
		return out, e
	}
	if publish != nil && s.observer != nil {
		_ = s.observer.Publish(ctx, family, *publish)
	}
	return out, resultErr
}

func sortConflicts(c []protocol.ImportConflict) {
	sort.Slice(c, func(i, j int) bool { return c[i].ConflictID < c[j].ConflictID })
}
