package export

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", s[:8], s[8:12], s[12:16], s[16:20], s[20:]), nil
}

// sessionFamily is p's session family, checked with no database read: the
// request authorized family policy for it, and the guarded transaction
// re-reads the session and refuses any other family (withFamily). A
// principal that is not a refreshed family session is owner_required, as
// the legacy family read refused it.
func (s *Service) sessionFamily(p access.Principal) (string, error) {
	if s == nil || p.Scope != "family" || p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
		return "", ErrOwnerRequired
	}
	return p.SessionFamilyID, nil
}

// ValidCreateRequest reports whether request names an export Create
// accepts: protocol v1, exactly the export capability and a request ID.
// The HTTP route checks it before family policy, where Create's own check
// always ranked.
func ValidCreateRequest(request protocol.CreateExportRequest) bool {
	return request.ProtocolVersion == "v1" && len(request.Capabilities) == 1 && request.Capabilities[0] == protocol.CapabilityExportV1 &&
		protocol.ValidUUID(request.RequestID)
}

// withFamily runs run in one guarded export_family transaction for familyID,
// the principal's session family: read-only and repeatable-read (one
// snapshot) when read is set. The family and role come from the grant; a
// grant for any other family or account is membership_revoked, as the legacy
// guard refused a family mismatch.
func (s *Service) withFamily(ctx context.Context, p access.Principal, familyID string, read bool, run func(pgx.Tx, access.Grant) error) error {
	if s == nil || s.options.Authority == nil {
		return ErrUnavailable
	}
	guarded := s.options.Authority.WithFamilyTx
	if read {
		guarded = s.options.Authority.WithFamilyReadTx
	}
	return guarded(ctx, p, "export_family", func(tx pgx.Tx, grant access.Grant) error {
		if grant.AccountID != p.AccountID || grant.FamilyID != familyID {
			return authority.ErrMembershipRevoked
		}
		return run(tx, grant)
	})
}

func statusFromJob(j Job, bytes *uint64, sha, code *string) protocol.ExportStatus {
	return protocol.ExportStatus{ProtocolVersion: "v1", ExportID: j.ID, State: j.State,
		CreatedAt:      j.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000Z"),
		ExpiresAt:      j.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000000Z"),
		PlaintextBytes: bytes, SHA256: sha, Code: code}
}

func (s *Service) Create(ctx context.Context, p access.Principal, request protocol.CreateExportRequest) (protocol.ExportStatus, bool, error) {
	if s == nil || s.options.Authority == nil || !ValidCreateRequest(request) {
		return protocol.ExportStatus{}, false, ErrInvalidRequest
	}
	familyID, err := s.sessionFamily(p)
	if err != nil {
		return protocol.ExportStatus{}, false, err
	}
	var result protocol.ExportStatus
	created := false
	err = s.withFamily(ctx, p, familyID, false, func(tx pgx.Tx, grant access.Grant) error {
		if grant.Role != "owner" {
			return ErrOwnerRequired
		}
		var j Job
		var bytes *uint64
		var sha, code *string
		err := tx.QueryRow(ctx, `SELECT id::text,family_id::text,requester_account_id::text,request_id::text,server_instance_id::text,recovery_epoch,state,created_at,expires_at,plaintext_bytes,plaintext_sha256,last_code
			FROM public.export_jobs WHERE requester_account_id=$1 AND request_id=$2`, p.AccountID, request.RequestID).
			Scan(&j.ID, &j.FamilyID, &j.RequesterAccountID, &j.RequestID, &j.InstanceID, &j.RecoveryEpoch, &j.State, &j.CreatedAt, &j.ExpiresAt, &bytes, &sha, &code)
		if err == nil {
			if j.InstanceID != s.options.Identity.InstanceID || j.RecoveryEpoch != s.options.Identity.RecoveryEpoch {
				return ErrAuthorizationChanged
			}
			if !s.options.Now().Before(j.ExpiresAt) {
				j.State, bytes, sha, code = "expired", nil, nil, nil
			}
			result = statusFromJob(j, bytes, sha, code)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Serialize account and family quotas, even across different families.
		// Runtime has no account row-lock grant, so use transaction advisory
		// locks in a fixed order. Hash collisions only serialize extra requests.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('export-account:'||$1::text,0))`, p.AccountID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('export-family:'||$1::text,0))`, familyID); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.export_jobs WHERE family_id=$1 AND state IN ('queued','running','ready'))`, familyID).Scan(&active); err != nil {
			return err
		}
		if active {
			return ErrInProgress
		}
		var recent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.export_rate_reservations WHERE accepted_at>$3
			AND ((scope='family' AND key_id=$1) OR (scope='account' AND key_id=$2)))`, familyID, p.AccountID, s.options.Now().UTC().Add(-6*time.Hour)).Scan(&recent); err != nil {
			return err
		}
		if recent {
			return ErrRateLimited
		}
		id, err := newUUID()
		if err != nil {
			return err
		}
		now := s.options.Now().UTC().Truncate(time.Microsecond)
		j = Job{ID: id, FamilyID: familyID, RequesterAccountID: p.AccountID, RequestID: request.RequestID, State: "queued", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
		if _, err := tx.Exec(ctx, `INSERT INTO public.export_jobs(id,family_id,requester_account_id,request_id,server_instance_id,recovery_epoch,security_revision,state,created_at,expires_at,next_attempt_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,'queued',$8,$9,now())`, id, familyID, p.AccountID, request.RequestID, s.options.Identity.InstanceID, s.options.Identity.RecoveryEpoch, grant.Revision, now, j.ExpiresAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.export_rate_reservations(scope,key_id,request_id,accepted_at)
			VALUES('family',$1,$3,$4),('account',$2,$3,$4)`, familyID, p.AccountID, request.RequestID, now); err != nil {
			return err
		}
		result = statusFromJob(j, nil, nil, nil)
		created = true
		return nil
	})
	return result, created, err
}

func (s *Service) Status(ctx context.Context, p access.Principal, id string) (protocol.ExportStatus, error) {
	if !protocol.ValidUUID(id) {
		return protocol.ExportStatus{}, ErrNotFound
	}
	familyID, err := s.sessionFamily(p)
	if err != nil {
		return protocol.ExportStatus{}, err
	}
	var result protocol.ExportStatus
	err = s.withFamily(ctx, p, familyID, true, func(tx pgx.Tx, grant access.Grant) error {
		if grant.Role != "owner" {
			return ErrOwnerRequired
		}
		var j Job
		var bytes *uint64
		var sha, code *string
		err := tx.QueryRow(ctx, `SELECT id::text,family_id::text,requester_account_id::text,request_id::text,server_instance_id::text,recovery_epoch,state,created_at,expires_at,plaintext_bytes,plaintext_sha256,last_code
			FROM public.export_jobs WHERE id=$1 AND family_id=$2 AND requester_account_id=$3`, id, familyID, p.AccountID).
			Scan(&j.ID, &j.FamilyID, &j.RequesterAccountID, &j.RequestID, &j.InstanceID, &j.RecoveryEpoch, &j.State, &j.CreatedAt, &j.ExpiresAt, &bytes, &sha, &code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if j.InstanceID != s.options.Identity.InstanceID || j.RecoveryEpoch != s.options.Identity.RecoveryEpoch {
			return ErrAuthorizationChanged
		}
		if !s.options.Now().Before(j.ExpiresAt) {
			j.State = "expired"
			bytes = nil
			sha = nil
			code = nil
		}
		result = statusFromJob(j, bytes, sha, code)
		return nil
	})
	return result, err
}

type txChunkSource struct {
	tx       pgx.Tx
	exportID string
	attempt  int
}

func (source txChunkSource) Each(ctx context.Context, visit func(Chunk) error) error {
	rows, err := source.tx.Query(ctx, `SELECT chunk_index,plaintext_length,is_final,ciphertext FROM public.export_chunks
		WHERE export_id=$1 AND attempt=$2 ORDER BY chunk_index`, source.exportID, source.attempt)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.Index, &c.PlaintextLength, &c.Final, &c.Ciphertext); err != nil {
			return err
		}
		if err := visit(c); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Service) Download(ctx context.Context, p access.Principal, id string, out io.Writer) error {
	if !protocol.ValidUUID(id) || out == nil {
		return ErrNotFound
	}
	familyID, err := s.sessionFamily(p)
	if err != nil {
		return err
	}
	return s.withFamily(ctx, p, familyID, true, func(tx pgx.Tx, grant access.Grant) error {
		if grant.Role != "owner" {
			return ErrOwnerRequired
		}
		var headerRaw []byte
		var attempt int
		var state string
		var expires time.Time
		var instanceID string
		var recoveryEpoch uint64
		err := tx.QueryRow(ctx, `SELECT header,attempt,state,expires_at,server_instance_id::text,recovery_epoch FROM public.export_jobs WHERE id=$1 AND family_id=$2 AND requester_account_id=$3`, id, familyID, p.AccountID).
			Scan(&headerRaw, &attempt, &state, &expires, &instanceID, &recoveryEpoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if instanceID != s.options.Identity.InstanceID || recoveryEpoch != s.options.Identity.RecoveryEpoch {
			return ErrAuthorizationChanged
		}
		if !s.options.Now().Before(expires) {
			return ErrExpired
		}
		if state != "ready" {
			return ErrNotReady
		}
		var header EnvelopeHeader
		if err := json.Unmarshal(headerRaw, &header); err != nil {
			return ErrIntegrity
		}
		if header.Core.ExportID != id || header.Core.InstanceID != s.options.Identity.InstanceID ||
			header.Core.FamilyID != familyID || header.Core.RequesterAccountID != p.AccountID {
			return ErrIntegrity
		}
		key, ok := s.options.Keys[header.Core.KeyID]
		if !ok {
			return ErrIntegrity
		}
		source := txChunkSource{tx: tx, exportID: id, attempt: attempt}
		if err := OpenArchive(ctx, header, key[:], source, io.Discard); err != nil {
			return ErrIntegrity
		}
		if err := OpenArchive(ctx, header, key[:], source, out); err != nil {
			return ErrIntegrity
		}
		return nil
	})
}
