// Package importer owns durable staging and explicit, injective identity resolution.
// It deliberately has no dependency on the sync service.
package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

type Service struct {
	guard     authority.FamilyGuard
	info      protocol.ServerInfo
	limits    protocol.Limits
	keys      identity.DigestKeyring
	now       func() time.Time
	publisher care.Publisher
}

func NewService(guard authority.FamilyGuard, info protocol.ServerInfo, limits protocol.Limits, keys identity.DigestKeyring, now func() time.Time) *Service {
	return &Service{guard: guard, info: info, limits: limits, keys: keys, now: now}
}

// SetPublisher announces a published baby's new family high water after
// the commit.
func (s *Service) SetPublisher(p care.Publisher) { s.publisher = p }
func code(value string) error                    { return errors.New(value) }

type session struct {
	id, account, device, mode, normalization string
	total, count, next                       uint32
	complete, mapped                         bool
	revision                                 uint64
	created, expires                         time.Time
	expired                                  *time.Time
}

// sessionFamily is p's session family, checked with no database read: the
// guarded transaction re-reads the session and refuses any other family
// (withFamily). It refuses as the legacy family read did a principal that
// is not a family member (family_scope_required) or not a refreshed family
// session (access_expired).
func sessionFamily(p access.Principal) (string, error) {
	if p.Scope != "family" {
		return "", code("family_scope_required")
	}
	if p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
		return "", code("access_expired")
	}
	return p.SessionFamilyID, nil
}

// withFamily runs run in one guarded read-committed transaction for family,
// the principal's session family; the guard takes the family and role from
// the session row, so a grant for another family or account is refused as
// membership_revoked (the legacy guard's family-mismatch code).
func (s *Service) withFamily(ctx context.Context, p access.Principal, family, action string, run func(pgx.Tx, access.Grant) error) error {
	return s.guard.WithFamilyTx(ctx, p, action, familyChecked(p, family, run))
}

// withFamilyRead is withFamily in a repeatable-read read-only transaction:
// every read of run shares one snapshot, and nothing is locked or written.
func (s *Service) withFamilyRead(ctx context.Context, p access.Principal, family, action string, run func(pgx.Tx, access.Grant) error) error {
	return s.guard.WithFamilyReadTx(ctx, p, action, familyChecked(p, family, run))
}

func familyChecked(p access.Principal, family string, run func(pgx.Tx, access.Grant) error) func(pgx.Tx, access.Grant) error {
	return func(tx pgx.Tx, g access.Grant) error {
		if g.AccountID != p.AccountID || g.FamilyID != family {
			return authority.ErrMembershipRevoked
		}
		return run(tx, g)
	}
}

func loadSession(ctx context.Context, tx pgx.Tx, family, id string) (session, error) {
	v := session{id: id}
	e := tx.QueryRow(ctx, `SELECT initiating_account_id::text,initiating_device_id::text,import_mode,normalization_version,total_record_count,stored_record_count,next_chunk_index,manifest_complete,mapping_complete,resolver_revision,created_at,expires_at,expired_at FROM import_sessions WHERE family_id=$1 AND import_id=$2`, family, id).Scan(&v.account, &v.device, &v.mode, &v.normalization, &v.total, &v.count, &v.next, &v.complete, &v.mapped, &v.revision, &v.created, &v.expires, &v.expired)
	return v, e
}

// Expiry is committed even when the caller receives import_expired. ID markers
// survive cleanup and can never be reused to reopen an import.
func (s *Service) expired(ctx context.Context, tx pgx.Tx, family string, v session) (bool, error) {
	if v.expired != nil {
		return true, nil
	}
	if !v.mapped && !s.now().Before(v.expires) {
		_, e := tx.Exec(ctx, `UPDATE import_sessions SET expired_at=$3,resolver_revision=resolver_revision+1 WHERE family_id=$1 AND import_id=$2`, family, v.id, s.now())
		return true, e
	}
	return false, nil
}

func (s *Service) Resolve(ctx context.Context, p access.Principal, r protocol.ImportResolveRequest) (protocol.ImportResolveResponse, error) {
	var out protocol.ImportResolveResponse
	if e := s.validate(&r); e != nil {
		return out, e
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
		if _, e := care.LockFamily(ctx, tx, family); e != nil {
			return e
		}
		v, e := loadSession(ctx, tx, family, r.ImportID)
		if errors.Is(e, pgx.ErrNoRows) {
			if r.ChunkIndex != 0 {
				return code("import_chunk_out_of_order")
			}
			// Serialize new-session admissions for the same account even across families.
			if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,5712))`, p.AccountID); e != nil {
				return e
			}
			var total, own, unfinished, global int
			e = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE initiating_account_id=$2),count(*) FILTER(WHERE NOT mapping_complete AND expired_at IS NULL AND expires_at>$3) FROM import_sessions WHERE family_id=$1`, family, p.AccountID, s.now()).Scan(&total, &own, &unfinished)
			if e != nil {
				return e
			}
			if total >= s.limits.ImportMaxSessionsPerFamily || own >= s.limits.ImportMaxSessionsPerAccount {
				return code("import_history_limit")
			}
			if e = tx.QueryRow(ctx, `SELECT app.import_unfinished_session_count($1,$2)`, p.AccountID, s.now()).Scan(&global); e != nil {
				return e
			}
			if unfinished >= s.limits.UnfinishedImportFamilyLimit || global >= s.limits.UnfinishedImportAccountLimit {
				return code("import_unfinished_limit")
			}
			v = session{id: r.ImportID, account: p.AccountID, device: p.DeviceID, mode: r.ImportMode, normalization: r.NormalizationVersion, total: r.TotalRecordCount, revision: 1, created: s.now().UTC(), expires: s.now().UTC().Add(time.Duration(s.limits.ImportStagingTTLHours) * time.Hour)}
			_, e = tx.Exec(ctx, `INSERT INTO import_sessions(family_id,import_id,initiating_account_id,initiating_device_id,import_mode,normalization_version,total_record_count,created_at,expires_at,storage_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,512)`, family, v.id, v.account, v.device, v.mode, v.normalization, v.total, v.created, v.expires)
			if e != nil {
				return e
			}
			if e = care.ChargeImport(ctx, tx, family, 512, s.limits); e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		if v.account != p.AccountID {
			return code("import_access_denied")
		}
		// Initiating device is frozen by chunk zero. Resuming from another device
		// hashes that stored identity, never the current caller's device.
		hash = sha256.Sum256(append([]byte("baby-tracker-import-chunk-v1\x00"+v.device+"\x00"), raw...))
		expired, e := s.expired(ctx, tx, family, v)
		if e != nil {
			return e
		}
		if expired {
			resultErr = code("import_expired")
			return nil
		}
		var priorHash, priorBody []byte
		e = tx.QueryRow(ctx, `SELECT canonical_sha256,response_body FROM import_chunks WHERE family_id=$1 AND import_id=$2 AND chunk_index=$3`, family, v.id, r.ChunkIndex).Scan(&priorHash, &priorBody)
		if e == nil {
			if !bytes.Equal(priorHash, hash[:]) {
				return code("import_chunk_reused")
			}
			return json.Unmarshal(priorBody, &out)
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if v.complete || r.ChunkIndex != v.next {
			return code("import_chunk_out_of_order")
		}
		if r.ImportMode != v.mode || r.NormalizationVersion != v.normalization || r.TotalRecordCount != v.total || (r.ChunkIndex > 0 && len(r.Babies) != 0) {
			return code("import_manifest_changed")
		}
		if uint64(v.count)+uint64(len(r.Records)) > uint64(v.total) || r.Finalize && v.count+uint32(len(r.Records)) != v.total {
			return code("import_manifest_incomplete")
		}
		if e = s.stageBabies(ctx, tx, family, v.id, r.Babies); e != nil {
			return e
		}
		// A record is refused if this import already holds its id or names a
		// local baby the import has no candidate for: one statement for the
		// chunk's records.
		if len(r.Records) > 0 {
			ids, babies := make([]string, len(r.Records)), make([]string, len(r.Records))
			for i, m := range r.Records {
				ids[i], babies[i] = m.RecordID, m.LocalBabyID
			}
			var refused bool
			e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM unnest($3::text[],$4::text[]) AS r(record_id,local_baby_id) WHERE EXISTS(SELECT 1 FROM import_record_manifests m WHERE m.family_id=$1 AND m.import_id=$2 AND m.record_id=r.record_id::uuid) OR NOT EXISTS(SELECT 1 FROM import_baby_candidates b WHERE b.family_id=$1 AND b.import_id=$2 AND b.local_baby_id=r.local_baby_id::uuid))`, family, v.id, ids, babies).Scan(&refused)
			if e != nil {
				return e
			}
			if refused {
				return code("invalid_request")
			}
		}
		v.count += uint32(len(r.Records))
		v.next++
		v.complete = r.Finalize
		_, e = tx.Exec(ctx, `UPDATE import_sessions SET stored_record_count=$3,next_chunk_index=$4,manifest_complete=$5,finalized_at=CASE WHEN $5 THEN $6::timestamptz ELSE NULL END WHERE family_id=$1 AND import_id=$2`, family, v.id, v.count, v.next, v.complete, s.now())
		if e != nil {
			return e
		}
		if v.complete {
			if _, publish, e = s.resolveGraph(ctx, tx, g, &v); e != nil {
				return e
			}
		}
		mappings, e := s.mappings(ctx, tx, family, v)
		if e != nil {
			return e
		}
		pending, e := s.recordConflicts(ctx, tx, family, v, mappings, r.Records)
		if e != nil {
			return e
		}
		out, e = s.response(ctx, tx, p, family, v, nil, pending, mappings)
		if e != nil {
			return e
		}
		body, e := json.Marshal(out)
		if e != nil {
			return e
		}
		size := int64(len(body) + len(raw) + 256)
		_, e = tx.Exec(ctx, `INSERT INTO import_chunks(family_id,import_id,chunk_index,canonical_sha256,record_count,finalize,response_status,response_body,storage_bytes) VALUES($1,$2,$3,$4,$5,$6,200,$7,$8)`, family, v.id, r.ChunkIndex, hash[:], len(r.Records), r.Finalize, body, size)
		if e != nil {
			return e
		}
		if e = s.stageManifests(ctx, tx, family, v.id, r.ChunkIndex, r.Records); e != nil {
			return e
		}
		// The chunk and its manifests are charged together: every charge is
		// positive, so the sum is refused exactly when one of the charges
		// one by one would have been, and the transaction rolls back either way.
		if e = care.ChargeImport(ctx, tx, family, size+192*int64(len(r.Records)), s.limits); e != nil {
			return e
		}
		for _, c := range pending {
			if e = s.saveConflict(ctx, tx, family, v, c); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		return out, e
	}
	s.publish(ctx, family, publish)
	return out, resultErr
}

// publish announces the high water a baby publication advanced the family
// to, after the commit, best effort.
func (s *Service) publish(ctx context.Context, family string, highWater uint64) {
	if highWater == 0 || s.publisher == nil {
		return
	}
	_ = s.publisher.Publish(ctx, family, protocol.Cursor{ServerInstanceID: s.info.InstanceID, RecoveryEpoch: s.info.RecoveryEpoch,
		FamilyID: family, Sequence: highWater})
}

// stageBabies stores a chunk's baby candidates in one statement and charges
// them in one: every charge is positive, so the sum is refused exactly when
// one of the charges one by one would have been.
func (s *Service) stageBabies(ctx context.Context, tx pgx.Tx, family, id string, babies []protocol.ImportBaby) error {
	if len(babies) == 0 {
		return nil
	}
	local, names, normalized, dates := make([]string, len(babies)), make([]string, len(babies)), make([]string, len(babies)), make([]string, len(babies))
	orders, sizes := make([]int64, len(babies)), make([]int64, len(babies))
	var total int64
	for i, b := range babies {
		n, _ := protocol.NormalizeBabyName(b.Name)
		local[i], names[i], normalized[i], dates[i], orders[i] = b.LocalBabyID, b.Name, n, b.BirthDate, int64(b.SortOrder)
		sizes[i] = int64(192 + len(b.Name) + len(n))
		total += sizes[i]
	}
	_, e := tx.Exec(ctx, `INSERT INTO import_baby_candidates(family_id,import_id,local_baby_id,name,normalized_name,birth_date,sort_order,storage_bytes) SELECT $1,$2,b.local_baby_id::uuid,b.name,b.normalized_name,b.birth_date::date,b.sort_order,b.storage_bytes FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::bigint[],$8::bigint[]) WITH ORDINALITY AS b(local_baby_id,name,normalized_name,birth_date,sort_order,storage_bytes,ordinal) ORDER BY b.ordinal`, family, id, local, names, normalized, dates, orders, sizes)
	if e != nil {
		return e
	}
	return care.ChargeImport(ctx, tx, family, total, s.limits)
}

// stageManifests stores a chunk's record manifests in one statement; the
// caller charges them (192 bytes each).
func (s *Service) stageManifests(ctx context.Context, tx pgx.Tx, family, id string, chunk uint32, records []protocol.RecordManifest) error {
	if len(records) == 0 {
		return nil
	}
	ids, kinds, babies, hashes := make([]string, len(records)), make([]string, len(records)), make([]string, len(records)), make([][]byte, len(records))
	for i, m := range records {
		hashes[i], _ = hex.DecodeString(m.ContentSHA256)
		ids[i], kinds[i], babies[i] = m.RecordID, m.EntityKind, m.LocalBabyID
	}
	_, e := tx.Exec(ctx, `INSERT INTO import_record_manifests(family_id,import_id,record_id,entity_kind,local_baby_id,chunk_index,content_sha256,storage_bytes) SELECT $1,$2,m.record_id::uuid,m.entity_kind,m.local_baby_id::uuid,$3,m.content_sha256,192 FROM unnest($4::text[],$5::text[],$6::text[],$7::bytea[]) WITH ORDINALITY AS m(record_id,entity_kind,local_baby_id,content_sha256,ordinal) ORDER BY m.ordinal`, family, id, chunk, ids, kinds, babies, hashes)
	return e
}

func (s *Service) Status(ctx context.Context, p access.Principal, id string) (protocol.ImportResolveResponse, error) {
	return s.StatusPage(ctx, p, id, nil)
}

// StatusPage reads an import's status in one read-only snapshot, taking
// no lock: the session (import_not_found), the access check
// (import_access_denied), then a recorded expiry (import_expired) or the
// response. Only an unfinished import found past its expiry and not yet
// marked takes the family lock, in a second transaction that re-reads the
// session and marks it expired (committed although the caller is told
// import_expired), as Resolve and ResolveConflict mark it.
func (s *Service) StatusPage(ctx context.Context, p access.Principal, id string, token *string) (protocol.ImportResolveResponse, error) {
	var out protocol.ImportResolveResponse
	if !protocol.ValidUUID(id) {
		return out, code("invalid_request")
	}
	family, e := sessionFamily(p)
	if e != nil {
		return out, e
	}
	var resultErr error
	unmarked := false
	e = s.withFamilyRead(ctx, p, family, "read", func(tx pgx.Tx, g access.Grant) error {
		v, e := s.statusSession(ctx, tx, p, g, family, id)
		if e != nil {
			return e
		}
		switch {
		case v.expired != nil:
			resultErr = code("import_expired")
			return nil
		case !v.mapped && !s.now().Before(v.expires):
			unmarked = true
			return nil
		}
		out, e = s.response(ctx, tx, p, family, v, token, nil, nil)
		return e
	})
	if e != nil || !unmarked {
		if e != nil {
			return out, e
		}
		return out, resultErr
	}
	e = s.withFamily(ctx, p, family, "read", func(tx pgx.Tx, g access.Grant) error {
		if _, e := care.LockFamily(ctx, tx, family); e != nil {
			return e
		}
		v, e := s.statusSession(ctx, tx, p, g, family, id)
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
		out, e = s.response(ctx, tx, p, family, v, token, nil, nil)
		return e
	})
	if e != nil {
		return out, e
	}
	return out, resultErr
}

// statusSession is a status read's session and its access check: the
// initiating account or the family's owner.
func (s *Service) statusSession(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, family, id string) (session, error) {
	v, e := loadSession(ctx, tx, family, id)
	if errors.Is(e, pgx.ErrNoRows) {
		return v, code("import_not_found")
	}
	if e != nil {
		return v, e
	}
	if v.account != p.AccountID && g.Role != "owner" {
		return v, code("import_access_denied")
	}
	return v, nil
}

func (s *Service) validate(r *protocol.ImportResolveRequest) error {
	if r.ProtocolVersion != "v1" || protocol.ValidateRouteCapabilities(protocol.SurfaceImport, r.Capabilities, r.Capabilities, protocol.KnownCapabilities()) != nil || !protocol.ValidUUID(r.ImportID) || r.NormalizationVersion != "nfc-trim-fold-nfc-v1" || (r.ImportMode != "match_shared" && r.ImportMode != "preserve_local_ids") || r.Babies == nil || r.Records == nil || len(r.Babies) > s.limits.ImportMaxBabies || len(r.Records) > s.limits.ImportChunkSize || r.TotalRecordCount > uint32(s.limits.ImportMaxRecords) || !r.Finalize && len(r.Records) == 0 {
		return code("invalid_request")
	}
	seen := map[string]bool{}
	for i, b := range r.Babies {
		if !protocol.ValidUUID(b.LocalBabyID) || seen[b.LocalBabyID] {
			return code("invalid_request")
		}
		seen[b.LocalBabyID] = true
		n, e := protocol.NormalizeDisplayName(b.Name)
		if e != nil {
			return code("invalid_request")
		}
		b.Name = n
		r.Babies[i] = b
		m := protocol.Mutation{MutationID: r.ImportID, EntityID: b.LocalBabyID, EntityKind: "baby", Operation: "create", PayloadVersion: 1, ClientMutationTime: "2026-01-01T00:00:00.000000Z", Payload: protocol.BabyPayload{Name: b.Name, BirthDate: b.BirthDate, SortOrder: b.SortOrder}}
		if care.ValidateMutation(m, s.limits) != nil {
			return code("invalid_request")
		}
	}
	seen = map[string]bool{}
	for _, m := range r.Records {
		hash, e := hex.DecodeString(m.ContentSHA256)
		if !protocol.ValidUUID(m.RecordID) || !protocol.ValidUUID(m.LocalBabyID) || seen[m.RecordID] || e != nil || len(hash) != 32 || hex.EncodeToString(hash) != m.ContentSHA256 || (m.EntityKind != "feed" && m.EntityKind != "sleep" && m.EntityKind != "diaper" && m.EntityKind != "supplement" && m.EntityKind != "growth_profile" && m.EntityKind != "measurement" && m.EntityKind != "immunization" && m.EntityKind != "timer") {
			return code("invalid_request")
		}
		if m.EntityKind == "growth_profile" && m.RecordID != protocol.GrowthProfileID(m.LocalBabyID) {
			return code("invalid_request")
		}
		seen[m.RecordID] = true
	}
	return nil
}

func deterministicID(parts ...string) string {
	raw, _ := json.Marshal(parts)
	h := sha256.Sum256(raw)
	h[6] = (h[6] & 15) | 80
	h[8] = (h[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[:4], h[4:6], h[6:8], h[8:10], h[10:16])
}
