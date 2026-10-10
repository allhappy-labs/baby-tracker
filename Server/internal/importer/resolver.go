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
		for _, b := range r.Babies {
			n, _ := protocol.NormalizeBabyName(b.Name)
			size := int64(192 + len(b.Name) + len(n))
			_, e = tx.Exec(ctx, `INSERT INTO import_baby_candidates(family_id,import_id,local_baby_id,name,normalized_name,birth_date,sort_order,storage_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, family, v.id, b.LocalBabyID, b.Name, n, b.BirthDate, b.SortOrder, size)
			if e != nil {
				return e
			}
			if e = care.ChargeImport(ctx, tx, family, size, s.limits); e != nil {
				return e
			}
		}
		for _, m := range r.Records {
			var exists, local bool
			e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM import_record_manifests WHERE family_id=$1 AND import_id=$2 AND record_id=$3),EXISTS(SELECT 1 FROM import_baby_candidates WHERE family_id=$1 AND import_id=$2 AND local_baby_id=$4)`, family, v.id, m.RecordID, m.LocalBabyID).Scan(&exists, &local)
			if e != nil {
				return e
			}
			if exists || !local {
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
		pending, e := s.recordConflicts(ctx, tx, family, v, r.Records)
		if e != nil {
			return e
		}
		out, e = s.response(ctx, tx, p, family, v, nil, pending)
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
		if e = care.ChargeImport(ctx, tx, family, size, s.limits); e != nil {
			return e
		}
		for _, m := range r.Records {
			hash, _ := hex.DecodeString(m.ContentSHA256)
			_, e = tx.Exec(ctx, `INSERT INTO import_record_manifests(family_id,import_id,record_id,entity_kind,local_baby_id,chunk_index,content_sha256,storage_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,192)`, family, v.id, m.RecordID, m.EntityKind, m.LocalBabyID, r.ChunkIndex, hash)
			if e != nil {
				return e
			}
			if e = care.ChargeImport(ctx, tx, family, 192, s.limits); e != nil {
				return e
			}
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
		out, e = s.response(ctx, tx, p, family, v, token, nil)
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
		out, e = s.response(ctx, tx, p, family, v, token, nil)
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
		if !protocol.ValidUUID(m.RecordID) || !protocol.ValidUUID(m.LocalBabyID) || seen[m.RecordID] || e != nil || len(hash) != 32 || hex.EncodeToString(hash) != m.ContentSHA256 || (m.EntityKind != "feed" && m.EntityKind != "sleep" && m.EntityKind != "diaper" && m.EntityKind != "supplement" && m.EntityKind != "growth_profile" && m.EntityKind != "measurement" && m.EntityKind != "timer") {
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
