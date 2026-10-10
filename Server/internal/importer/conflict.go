package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"

	"babytracker/server/internal/access"
	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
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
	rows, e := tx.Query(ctx, `SELECT conflict_id::text,kind,local_baby_id::text,record_id::text,candidate_baby_ids::text[],candidate_versions,candidate_fingerprint,decision_choice,decision_canonical_baby_id::text FROM import_conflicts WHERE family_id=$1 AND import_id=$2 AND record_id IS NULL ORDER BY import_conflicts.conflict_id`, family, id)
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
	return care.ChargeImport(ctx, tx, family, size-old, s.limits)
}

// resolveGraph reads every candidate before publishing any baby. The fingerprint
// includes all exact-match edges and occupied local UUIDs, so newly appearing
// candidates invalidate earlier decisions just as renames/deletions do. It
// returns the family high water a baby publication advanced to (0: none).
func (s *Service) resolveGraph(ctx context.Context, tx pgx.Tx, g access.Grant, v *session) (bool, uint64, error) {
	if v.mapped {
		return false, 0, nil
	}
	rows, e := tx.Query(ctx, `SELECT local_baby_id::text,name,birth_date::text,sort_order FROM import_baby_candidates WHERE family_id=$1 AND import_id=$2 ORDER BY import_baby_candidates.local_baby_id`, g.FamilyID, v.id)
	if e != nil {
		return false, 0, e
	}
	babies := []candidate{}
	for rows.Next() {
		var b candidate
		if e = rows.Scan(&b.LocalBabyID, &b.Name, &b.BirthDate, &b.SortOrder); e != nil {
			rows.Close()
			return false, 0, e
		}
		babies = append(babies, b)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, 0, e
	}
	old, e := s.conflicts(ctx, tx, g.FamilyID, v.id)
	if e != nil {
		return false, 0, e
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
		if v.mode == "match_shared" {
			// The session's normalization version is the only one
			// (validate), which BabyNameHash implements.
			hash, e := care.BabyNameHash(b.Name, b.BirthDate)
			if e != nil {
				return false, 0, e
			}
			matched, e := care.MatchBabies(ctx, tx, g.FamilyID, hash)
			if e != nil {
				return false, 0, e
			}
			for _, row := range matched {
				b.matches = append(b.matches, row.ID)
				b.versions = append(b.versions, row.EntityVersion)
			}
		}
		row, e := care.Get(ctx, tx, g.FamilyID, b.LocalBabyID)
		if e != nil && !errors.Is(e, care.ErrNotFound) {
			return false, 0, e
		}
		var local any
		if e == nil {
			entity, e := row.Entity()
			if e != nil {
				return false, 0, e
			}
			local = entity
			b.collision = row.Kind != "baby" || entity.Meta.TombstonedAt != nil
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
			return false, 0, e
		}
		for _, c := range oldByLocal {
			c.choice = nil
			c.target = nil
		}
		if e = s.revision(ctx, tx, g.FamilyID, *v); e != nil {
			return false, 0, e
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
				return false, 0, e
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
			return false, 0, code("import_target_already_assigned")
		}
		targets[b.target] = true
	}
	if unresolved || stale {
		return stale, 0, nil
	}
	// Publication is the sole mapping write point and runs only for a complete,
	// revalidated injective assignment. Every baby not yet stored is created
	// in one care batch, server-built (no Policy), as the initiating account
	// and device; each mapping records its baby's version and changed_seq.
	stored, e := care.GetMany(ctx, tx, g.FamilyID, func() []string {
		ids := make([]string, 0, len(babies))
		for _, b := range babies {
			ids = append(ids, b.target)
		}
		return ids
	}())
	if e != nil {
		return false, 0, e
	}
	type published struct{ version, cursor uint64 }
	mapped := map[string]published{}
	creates := []protocol.Mutation{}
	for _, b := range babies {
		if row, ok := stored[b.target]; ok {
			mapped[b.target] = published{row.EntityVersion, row.ChangedSeq}
			continue
		}
		creates = append(creates, protocol.Mutation{MutationID: deterministicID(v.id, b.LocalBabyID, "publication"), EntityID: b.target, EntityKind: "baby", Operation: "create", PayloadVersion: 1, ClientMutationTime: v.created.UTC().Format("2006-01-02T15:04:05.000000Z"), Payload: protocol.BabyPayload{Name: b.Name, BirthDate: b.BirthDate, SortOrder: b.SortOrder}})
	}
	var highWater uint64
	if len(creates) > 0 {
		creator := g
		creator.AccountID = v.account
		result, e := care.ApplyBatch(ctx, tx, care.BatchCommand{Grant: creator, DeviceID: v.device, Mutations: creates, AcceptedAt: s.now(),
			Server: s.info, Limits: s.limits, NoReceipts: true}, nil)
		if e != nil {
			return false, 0, e
		}
		if result.Fatal != nil {
			return false, 0, result.Fatal
		}
		for _, out := range result.Outcomes {
			if out.Status != "applied" || out.CanonicalEntity == nil || out.Cursor == nil {
				return false, 0, code("baby_uuid_collision")
			}
			mapped[out.CanonicalEntity.Meta.ID] = published{out.CanonicalEntity.Meta.EntityVersion, out.Cursor.Sequence}
		}
		if result.HighWater > result.Base {
			highWater = result.HighWater
		}
	}
	for _, b := range babies {
		baby := mapped[b.target]
		_, e = tx.Exec(ctx, `INSERT INTO import_baby_mappings(family_id,import_id,local_baby_id,shared_baby_id,published_revision,shared_baby_entity_version,shared_baby_cursor,storage_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,192)`, g.FamilyID, v.id, b.LocalBabyID, b.target, v.revision, baby.version, baby.cursor)
		if e != nil {
			return false, 0, e
		}
		if e = care.ChargeImport(ctx, tx, g.FamilyID, 192, s.limits); e != nil {
			return false, 0, e
		}
	}
	v.mapped = true
	_, e = tx.Exec(ctx, `UPDATE import_sessions SET mapping_complete=true,mapping_completed_at=$3 WHERE family_id=$1 AND import_id=$2`, g.FamilyID, v.id, s.now())
	return false, highWater, e
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
	family, e := sessionFamily(p)
	if e != nil {
		return out, e
	}
	raw, _ := json.Marshal(r)
	hash := sha256.Sum256(raw)
	var resultErr error
	var publish uint64
	e = s.withFamily(ctx, p, family, "add_baby", func(tx pgx.Tx, g access.Grant) error {
		if g.Role != "owner" {
			return code("owner_required")
		}
		if _, e := care.LockFamily(ctx, tx, family); e != nil {
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
		// This first pass only revalidates, so its high water is discarded.
		// resolveGraph publishes only when no baby conflict is undecided;
		// the conflict being chosen must be undecided, so a pass that
		// published (v.mapped) means the choice is stale, and the error
		// rolls the publication back with its high water. (Without this
		// check the stale refusal came from selected.choice != nil below.)
		stale, _, e := s.resolveGraph(ctx, tx, g, &v)
		if e != nil {
			return e
		}
		if stale {
			resultErr = code("import_resolution_stale")
			return nil
		}
		if v.mapped {
			return code("import_resolution_stale")
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
			row, e := care.Get(ctx, tx, family, target)
			if e == nil {
				if row.Kind != "baby" {
					return code("baby_uuid_collision")
				}
				var b protocol.ImportBaby
				e = tx.QueryRow(ctx, `SELECT name,birth_date::text,sort_order FROM import_baby_candidates WHERE family_id=$1 AND import_id=$2 AND local_baby_id=$3`, family, id, target).Scan(&b.Name, &b.BirthDate, &b.SortOrder)
				if e != nil {
					return e
				}
				entity, e := row.Entity()
				if e != nil {
					return e
				}
				payload, ok := entity.Payload.(protocol.BabyPayload)
				if !ok || entity.Meta.TombstonedAt != nil || payload.Name != b.Name || payload.BirthDate != b.BirthDate || payload.SortOrder != b.SortOrder {
					return code("baby_uuid_collision")
				}
			} else if !errors.Is(e, care.ErrNotFound) {
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
		if _, publish, e = s.resolveGraph(ctx, tx, g, &v); e != nil {
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
		return care.ChargeImport(ctx, tx, family, size, s.limits)
	})
	if e != nil {
		return out, e
	}
	s.publish(ctx, family, publish)
	return out, resultErr
}

func sortConflicts(c []protocol.ImportConflict) {
	sort.Slice(c, func(i, j int) bool { return c[i].ConflictID < c[j].ConflictID })
}
