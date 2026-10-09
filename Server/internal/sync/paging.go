package sync

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"github.com/jackc/pgx/v5"
)

var (
	ErrInvalidPageToken = errors.New("invalid_page_token")
	ErrPageExpired      = errors.New("page_expired")
	ErrEpochChanged     = errors.New("epoch_changed")
	ErrSnapshotRequired = errors.New("snapshot_required")
	ErrBatchLimit       = errors.New("batch_limit")
	ErrInvalidSync      = errors.New("invalid_request")
	ErrPageLimit        = errors.New("page_limit")
	ErrSnapshotLimit    = errors.New("snapshot_limit")
)

const pageTokenVersion byte = 1

type pageBinding struct {
	kind, keyID, familyID, accountID, deviceID, sessionID, instanceID string
	epoch, securityRevision, highWater, rangeEnd                      uint64
	resume, pageEnd                                                   uint64
	pageItems, pageBytes, itemLimit, byteLimit                        int
	snapshotID                                                        *string
	createdAt, expiresAt                                              time.Time
}

type decodedPageToken struct {
	key    identity.DigestKey
	lookup [32]byte
	raw    string
}

func clonePageKeys(keys identity.DigestKeyring) identity.DigestKeyring {
	return identity.DigestKeyring{Current: keys.Current, Previous: append([]identity.DigestKey(nil), keys.Previous...)}
}

func validPageKeys(keys identity.DigestKeyring) bool {
	if keys.Current.ID == "" || keys.Current.Secret == ([32]byte{}) || len(keys.Current.ID) > 64 {
		return false
	}
	seen := map[string]bool{keys.Current.ID: true}
	for _, key := range keys.Previous {
		if key.ID == "" || len(key.ID) > 64 || key.Secret == ([32]byte{}) || seen[key.ID] {
			return false
		}
		seen[key.ID] = true
	}
	return true
}

func pageKey(keys identity.DigestKeyring, id string) (identity.DigestKey, bool) {
	for _, key := range append([]identity.DigestKey{keys.Current}, keys.Previous...) {
		if key.ID == id {
			return key, true
		}
	}
	return identity.DigestKey{}, false
}

func encodePageToken(key identity.DigestKey, lookup [32]byte) (string, error) {
	if len(key.ID) == 0 || len(key.ID) > 64 || key.Secret == ([32]byte{}) {
		return "", ErrInvalidPageToken
	}
	payload := make([]byte, 0, 2+len(key.ID)+32+32)
	payload = append(payload, pageTokenVersion, byte(len(key.ID)))
	payload = append(payload, key.ID...)
	payload = append(payload, lookup[:]...)
	mac := hmac.New(sha256.New, key.Secret[:])
	_, _ = mac.Write([]byte("baby-tracker-page-token-v1\x00"))
	_, _ = mac.Write(payload)
	payload = append(payload, mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodePageToken(keys identity.DigestKeyring, token string) (decodedPageToken, error) {
	if token == "" || len(token) > 512 {
		return decodedPageToken{}, ErrInvalidPageToken
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) < 2+1+32+32 || raw[0] != pageTokenVersion {
		return decodedPageToken{}, ErrInvalidPageToken
	}
	keyLength := int(raw[1])
	if keyLength < 1 || keyLength > 64 || len(raw) != 2+keyLength+32+32 {
		return decodedPageToken{}, ErrInvalidPageToken
	}
	key, ok := pageKey(keys, string(raw[2:2+keyLength]))
	if !ok {
		return decodedPageToken{}, ErrInvalidPageToken
	}
	message, supplied := raw[:len(raw)-32], raw[len(raw)-32:]
	mac := hmac.New(sha256.New, key.Secret[:])
	_, _ = mac.Write([]byte("baby-tracker-page-token-v1\x00"))
	_, _ = mac.Write(message)
	if !hmac.Equal(supplied, mac.Sum(nil)) {
		return decodedPageToken{}, ErrInvalidPageToken
	}
	var lookup [32]byte
	copy(lookup[:], raw[2+keyLength:2+keyLength+32])
	return decodedPageToken{key: key, lookup: lookup, raw: token}, nil
}

func pageDigest(lookup [32]byte) [32]byte { return sha256.Sum256(lookup[:]) }

func successorLookup(token decodedPageToken, binding pageBinding) [32]byte {
	mac := hmac.New(sha256.New, token.key.Secret[:])
	_, _ = mac.Write([]byte("baby-tracker-page-successor-v1\x00"))
	_, _ = mac.Write(token.lookup[:])
	for _, value := range []string{binding.kind, binding.accountID, binding.deviceID, binding.sessionID, binding.familyID, binding.instanceID} {
		_, _ = mac.Write([]byte(value))
		_, _ = mac.Write([]byte{0})
	}
	var encoded [8]byte
	for _, value := range []uint64{binding.epoch, binding.securityRevision, binding.highWater, binding.rangeEnd, binding.resume, binding.pageEnd} {
		binary.BigEndian.PutUint64(encoded[:], value)
		_, _ = mac.Write(encoded[:])
	}
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func (s *Service) resolveFamily(ctx context.Context, principal access.Principal) (string, error) {
	if s == nil || s.resolver == nil || principal.Scope != "family" {
		return "", ErrFamilyScopeRequired
	}
	if principal.SessionScope != "family" || !protocol.ValidUUID(principal.SessionFamilyID) {
		return "", ErrSessionRefreshRequired
	}
	familyID, err := s.resolver.Current(ctx, principal)
	if err != nil {
		return "", err
	}
	if !protocol.ValidUUID(familyID) {
		return "", ErrFamilyScopeRequired
	}
	if principal.SessionFamilyID != familyID {
		return "", ErrSessionRefreshRequired
	}
	return familyID, nil
}

func (s *Service) ApplyAndPull(ctx context.Context, principal access.Principal, request protocol.SyncRequest) (protocol.SyncResponse, error) {
	if s == nil || s.guard == nil || s.repo == nil || s.now == nil || !validPageKeys(s.pageKeys) || s.identity.Validate() != nil || s.limits.Validate() != nil {
		return protocol.SyncResponse{}, fmt.Errorf("sync service unavailable")
	}
	familyID, err := s.resolveFamily(ctx, principal)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	surface := protocol.SurfaceSync
	for _, mutation := range request.Mutations {
		if mutation.EntityKind == "timer" {
			surface = protocol.SurfaceSyncTimer
			break
		}
	}
	if request.ProtocolVersion != "v1" || protocol.ValidateRouteCapabilities(surface, request.Capabilities, request.Capabilities, s.identity.Capabilities) != nil || request.Mutations == nil {
		return protocol.SyncResponse{}, ErrInvalidSync
	}
	if len(request.Mutations) > s.limits.SyncBatchSize {
		return protocol.SyncResponse{}, ErrBatchLimit
	}
	if request.PageToken != nil && (request.Cursor != nil || len(request.Mutations) != 0) {
		return protocol.SyncResponse{}, ErrInvalidSync
	}
	if request.PageToken == nil && request.Cursor != nil {
		if request.Cursor.Validate() != nil || request.Cursor.ServerInstanceID != s.identity.InstanceID || request.Cursor.RecoveryEpoch != s.identity.RecoveryEpoch || request.Cursor.FamilyID != familyID {
			return protocol.SyncResponse{}, ErrSnapshotRequired
		}
	}
	outcomes := make([]protocol.Outcome, 0, len(request.Mutations))
	for _, mutation := range request.Mutations {
		itemCtx, cancel := context.WithTimeout(ctx, time.Duration(s.limits.SyncTransactionSeconds)*time.Second)
		outcome, applyErr := s.Apply(itemCtx, principal, familyID, mutation)
		cancel()
		if applyErr != nil {
			return protocol.SyncResponse{}, applyErr
		}
		outcomes = append(outcomes, outcome)
	}
	var response protocol.SyncResponse
	err = s.guard.WithAuthorizedTransaction(ctx, principal, familyID, "read", func(tx pgx.Tx, grant access.Grant) error {
		if grant.AccountID != principal.AccountID || grant.FamilyID != familyID {
			return ErrFamilyScopeRequired
		}
		// A first page and a continuation both read under the family lock,
		// so no write commits in the middle of a page.
		if err := records.LockFamilyForUpdate(ctx, tx, familyID); err != nil {
			return err
		}
		if request.PageToken != nil {
			return s.readSyncContinuation(ctx, tx, principal, grant, *request.PageToken, &response)
		}
		var highWater uint64
		if err := tx.QueryRow(ctx, `SELECT next_sequence FROM families WHERE id=$1`, familyID).Scan(&highWater); err != nil {
			return err
		}
		after := uint64(0)
		if request.Cursor != nil {
			after = request.Cursor.Sequence
		}
		if after > highWater {
			return ErrSnapshotRequired
		}
		return s.buildSyncPage(ctx, tx, principal, grant, after, highWater, s.limits.SyncPageSize, s.limits.SyncPageBytes, outcomes, nil, &response)
	})
	return response, err
}

func (s *Service) buildSyncPage(ctx context.Context, tx pgx.Tx, principal access.Principal, grant access.Grant, after, highWater uint64,
	itemLimit, byteLimit int, outcomes []protocol.Outcome, presented *decodedPageToken, response *protocol.SyncResponse) error {
	candidates, err := ReadJournal(ctx, tx, grant.FamilyID, s.identity.InstanceID, s.identity.RecoveryEpoch, after, highWater, itemLimit+1)
	if err != nil {
		return err
	}
	pageKeyID := s.pageKeys.Current.ID
	if presented != nil {
		pageKeyID = presented.key.ID
	}
	page, end, hasMore, err := chooseSyncChanges(candidates, after, highWater, itemLimit, byteLimit, s.identity, grant.FamilyID, pageTokenPlaceholder(pageKeyID))
	if err != nil {
		return err
	}
	result := protocol.SyncResponse{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch, Outcomes: outcomes,
		Changes: page, HighWater: protocol.Cursor{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch, FamilyID: grant.FamilyID, Sequence: highWater}}
	if hasMore {
		key, lookup := s.pageKeys.Current, [32]byte{}
		if presented == nil {
			if _, err := rand.Read(lookup[:]); err != nil {
				return err
			}
		} else {
			key = presented.key
			lookup = successorLookup(*presented, pageBinding{kind: "sync", accountID: principal.AccountID, deviceID: principal.DeviceID,
				sessionID: principal.SessionID, familyID: grant.FamilyID, instanceID: s.identity.InstanceID, epoch: s.identity.RecoveryEpoch,
				securityRevision: principal.SecurityRevision, highWater: highWater, rangeEnd: highWater, resume: end})
		}
		nextPage, nextEnd, _, nextErr := s.previewSyncPage(ctx, tx, grant.FamilyID, end, highWater, itemLimit, byteLimit, key.ID)
		if nextErr != nil {
			return nextErr
		}
		token, err := encodePageToken(key, lookup)
		if err != nil {
			return err
		}
		created := s.now().UTC().Truncate(time.Microsecond)
		expires := created.Add(time.Duration(s.limits.SyncPageTTLSeconds) * time.Second)
		if presented != nil {
			binding, loadErr := loadPageBinding(ctx, tx, grant.FamilyID, pageDigest(presented.lookup))
			if loadErr != nil {
				return loadErr
			}
			created, expires = binding.createdAt, binding.expiresAt
		}
		binding := pageBinding{kind: "sync", familyID: grant.FamilyID, accountID: principal.AccountID, deviceID: principal.DeviceID,
			sessionID: principal.SessionID, instanceID: s.identity.InstanceID, epoch: s.identity.RecoveryEpoch, securityRevision: principal.SecurityRevision,
			highWater: highWater, rangeEnd: highWater, resume: end, pageEnd: nextEnd, pageItems: len(nextPage), itemLimit: itemLimit,
			byteLimit: byteLimit, createdAt: created, expiresAt: expires}
		binding.pageBytes = syncResponseBytes(s.identity, grant.FamilyID, highWater, nextPage, nextEnd < highWater, pageTokenPlaceholder(key.ID))
		if err := insertPageBinding(ctx, tx, key.ID, lookup, binding); err != nil {
			return err
		}
		result.NextPageToken = &token
	}
	*response = result
	return nil
}

func (s *Service) previewSyncPage(ctx context.Context, tx pgx.Tx, familyID string, after, highWater uint64, itemLimit, byteLimit int, keyID string) ([]protocol.JournalChange, uint64, bool, error) {
	candidates, err := ReadJournal(ctx, tx, familyID, s.identity.InstanceID, s.identity.RecoveryEpoch, after, highWater, itemLimit+1)
	if err != nil {
		return nil, 0, false, err
	}
	return chooseSyncChanges(candidates, after, highWater, itemLimit, byteLimit, s.identity, familyID, pageTokenPlaceholder(keyID))
}

func chooseSyncChanges(candidates []protocol.JournalChange, after, highWater uint64, itemLimit, byteLimit int, identity protocol.ServerInfo, familyID, placeholder string) ([]protocol.JournalChange, uint64, bool, error) {
	page := make([]protocol.JournalChange, 0, min(itemLimit, len(candidates)))
	end := after
	for index, change := range candidates {
		if index >= itemLimit {
			break
		}
		trial := append(append([]protocol.JournalChange(nil), page...), change)
		more := change.Cursor.Sequence < highWater
		if syncResponseBytes(identity, familyID, highWater, trial, more, placeholder) > byteLimit {
			if len(page) == 0 {
				return nil, 0, false, ErrPageLimit
			}
			break
		}
		page, end = trial, change.Cursor.Sequence
	}
	return page, end, end < highWater, nil
}

func syncResponseBytes(identity protocol.ServerInfo, familyID string, highWater uint64, changes []protocol.JournalChange, hasMore bool, placeholder string) int {
	var next *string
	if hasMore {
		next = &placeholder
	}
	return protocol.IncrementalPageEncodedBytes(identity, familyID, highWater, changes, next)
}

func encodedSyncPageBytes(response protocol.SyncResponse) int {
	return protocol.IncrementalPageEncodedBytes(protocol.ServerInfo{InstanceID: response.ServerInstanceID, RecoveryEpoch: response.RecoveryEpoch},
		response.HighWater.FamilyID, response.HighWater.Sequence, response.Changes, response.NextPageToken)
}

func pageTokenPlaceholder(keyID string) string {
	key := identity.DigestKey{ID: keyID, Secret: [32]byte{1}}
	token, _ := encodePageToken(key, [32]byte{})
	return token
}

func (s *Service) readSyncContinuation(ctx context.Context, tx pgx.Tx, principal access.Principal, grant access.Grant, raw string, response *protocol.SyncResponse) error {
	token, err := decodePageToken(s.pageKeys, raw)
	if err != nil {
		return err
	}
	binding, err := loadPageBinding(ctx, tx, grant.FamilyID, pageDigest(token.lookup))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidPageToken
	}
	if err != nil {
		return err
	}
	if err := validatePageBinding(binding, token, principal, grant, s.identity, "sync", s.now()); err != nil {
		return err
	}
	if err := s.buildSyncPage(ctx, tx, principal, grant, binding.resume, binding.highWater, binding.itemLimit, binding.byteLimit, []protocol.Outcome{}, &token, response); err != nil {
		return err
	}
	if response.HighWater.Sequence != binding.highWater || len(response.Changes) != binding.pageItems || encodedSyncPageBytes(*response) != binding.pageBytes {
		return ErrInvalidPageToken
	}
	end := binding.resume
	if len(response.Changes) != 0 {
		end = response.Changes[len(response.Changes)-1].Cursor.Sequence
	}
	if end != binding.pageEnd {
		return ErrInvalidPageToken
	}
	return nil
}

func validatePageBinding(binding pageBinding, token decodedPageToken, principal access.Principal, grant access.Grant, identity protocol.ServerInfo, kind string, now time.Time) error {
	if binding.epoch != identity.RecoveryEpoch || principal.RecoveryEpoch != identity.RecoveryEpoch {
		return ErrEpochChanged
	}
	if !hmac.Equal([]byte(binding.keyID), []byte(token.key.ID)) || binding.kind != kind || binding.accountID != principal.AccountID || binding.deviceID != principal.DeviceID || binding.sessionID != principal.SessionID ||
		binding.familyID != grant.FamilyID || binding.instanceID != identity.InstanceID || binding.securityRevision != principal.SecurityRevision {
		return ErrInvalidPageToken
	}
	if !now.UTC().Before(binding.expiresAt) {
		return ErrPageExpired
	}
	return nil
}

func loadPageBinding(ctx context.Context, tx pgx.Tx, familyID string, digest [32]byte) (pageBinding, error) {
	var b pageBinding
	var snapshotID *string
	err := tx.QueryRow(ctx, `SELECT kind,token_key_id,family_id::text,account_id::text,device_id::text,session_id::text,server_instance_id::text,
		recovery_epoch,security_revision,high_water_sequence,range_end_position,resume_position,page_end_position,page_item_count,
		page_encoded_bytes,snapshot_id::text,item_limit,byte_limit,created_at,expires_at FROM sync_page_sessions
		WHERE family_id=$1 AND token_digest=$2`, familyID, digest[:]).Scan(&b.kind, &b.keyID, &b.familyID, &b.accountID, &b.deviceID, &b.sessionID,
		&b.instanceID, &b.epoch, &b.securityRevision, &b.highWater, &b.rangeEnd, &b.resume, &b.pageEnd, &b.pageItems,
		&b.pageBytes, &snapshotID, &b.itemLimit, &b.byteLimit, &b.createdAt, &b.expiresAt)
	b.snapshotID = snapshotID
	return b, err
}

func insertPageBinding(ctx context.Context, tx pgx.Tx, keyID string, lookup [32]byte, b pageBinding) error {
	digest := pageDigest(lookup)
	b.keyID = keyID
	result, err := tx.Exec(ctx, `INSERT INTO sync_page_sessions(family_id,token_digest,token_key_id,account_id,device_id,session_id,
		server_instance_id,recovery_epoch,security_revision,kind,high_water_sequence,range_end_position,resume_position,page_end_position,
		page_item_count,page_encoded_bytes,snapshot_id,item_limit,byte_limit,created_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT(family_id,token_digest) DO NOTHING`, b.familyID, digest[:], keyID, b.accountID, b.deviceID, b.sessionID, b.instanceID,
		b.epoch, b.securityRevision, b.kind, b.highWater, b.rangeEnd, b.resume, b.pageEnd, b.pageItems, b.pageBytes, b.snapshotID,
		b.itemLimit, b.byteLimit, b.createdAt, b.expiresAt)
	if err != nil || result.RowsAffected() == 1 {
		return err
	}
	existing, err := loadPageBinding(ctx, tx, b.familyID, digest)
	if err != nil || !samePageBinding(existing, b) {
		return ErrInvalidPageToken
	}
	return nil
}

func samePageBinding(left, right pageBinding) bool {
	leftSnapshot, rightSnapshot := "", ""
	if left.snapshotID != nil {
		leftSnapshot = *left.snapshotID
	}
	if right.snapshotID != nil {
		rightSnapshot = *right.snapshotID
	}
	return hmac.Equal([]byte(left.keyID), []byte(right.keyID)) && left.kind == right.kind && left.familyID == right.familyID &&
		left.accountID == right.accountID && left.deviceID == right.deviceID && left.sessionID == right.sessionID &&
		left.instanceID == right.instanceID && left.epoch == right.epoch && left.securityRevision == right.securityRevision &&
		left.highWater == right.highWater && left.rangeEnd == right.rangeEnd && left.resume == right.resume &&
		left.pageEnd == right.pageEnd && left.pageItems == right.pageItems && left.pageBytes == right.pageBytes &&
		leftSnapshot == rightSnapshot && left.itemLimit == right.itemLimit && left.byteLimit == right.byteLimit &&
		left.createdAt.Equal(right.createdAt) && left.expiresAt.Equal(right.expiresAt)
}
