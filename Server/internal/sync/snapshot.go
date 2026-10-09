package sync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"github.com/jackc/pgx/v5"
)

func (s *Service) FullSnapshot(ctx context.Context, principal access.Principal, request protocol.SnapshotRequest) (protocol.SnapshotResponse, error) {
	if s == nil || s.guard == nil || s.repo == nil || s.now == nil || !validPageKeys(s.pageKeys) || s.identity.Validate() != nil || s.limits.Validate() != nil {
		return protocol.SnapshotResponse{}, fmt.Errorf("snapshot service unavailable")
	}
	operationCtx, cancel := context.WithTimeout(ctx, time.Duration(s.limits.SyncTransactionSeconds)*time.Second)
	defer cancel()
	familyID, err := s.resolveFamily(operationCtx, principal)
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	var response protocol.SnapshotResponse
	err = s.guard.WithAuthorizedTransaction(operationCtx, principal, familyID, "read", func(tx pgx.Tx, grant access.Grant) error {
		if grant.AccountID != principal.AccountID || grant.FamilyID != familyID {
			return ErrFamilyScopeRequired
		}
		if request.PageToken != nil {
			// A continuation reads under the family lock, as the capture does.
			if err := records.LockFamilyForUpdate(operationCtx, tx, familyID); err != nil {
				return err
			}
			return s.readSnapshotContinuation(operationCtx, tx, principal, grant, *request.PageToken, &response)
		}
		return s.captureSnapshot(operationCtx, tx, principal, grant, &response)
	})
	return response, err
}

func (s *Service) captureSnapshot(ctx context.Context, tx pgx.Tx, principal access.Principal, grant access.Grant, response *protocol.SnapshotResponse) error {
	// The family lock is the one-family beta admission serialization point. It
	// is held across cap counts, materialization, and the first page token.
	if err := records.LockFamilyForUpdate(ctx, tx, grant.FamilyID); err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	var accountActive, familyActive int64
	if err := tx.QueryRow(ctx, `SELECT account_active,family_active FROM app.sync_snapshot_admission_counts($1,$2,$3)`,
		principal.AccountID, grant.FamilyID, now).Scan(&accountActive, &familyActive); err != nil {
		return err
	}
	if accountActive >= int64(s.limits.ActiveSnapshotAccountLimit) || familyActive >= int64(s.limits.ActiveSnapshotFamilyLimit) {
		return ErrSnapshotLimit
	}
	var highWater uint64
	if err := tx.QueryRow(ctx, `SELECT next_sequence FROM families WHERE id=$1`, grant.FamilyID).Scan(&highWater); err != nil {
		return err
	}
	snapshotID, err := randomUUID()
	if err != nil {
		return err
	}
	highWaterCursor := protocol.Cursor{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch, FamilyID: grant.FamilyID, Sequence: highWater}
	entityCount, entityBytes := 0, int64(0)
	if err := s.repo.StreamCanonical(ctx, tx, grant.FamilyID, func(entity protocol.CanonicalEntity) error {
		raw, err := json.Marshal(entity)
		if err != nil {
			return err
		}
		entityCount++
		entityBytes += int64(len(raw))
		if entityCount > s.limits.SnapshotMaxEntities || entityBytes > int64(s.limits.SnapshotMaxBytes) {
			return ErrSnapshotLimit
		}
		return nil
	}); err != nil {
		return err
	}
	fullBytes := fullSnapshotEncodedBytes(s.identity, snapshotID, highWaterCursor, entityCount, entityBytes)
	if fullBytes > s.limits.SnapshotMaxBytes {
		return ErrSnapshotLimit
	}
	expires := now.Add(time.Duration(s.limits.SnapshotTTLSeconds) * time.Second)
	if _, err := tx.Exec(ctx, `INSERT INTO sync_snapshots(family_id,id,account_id,device_id,session_id,server_instance_id,recovery_epoch,
		security_revision,high_water_sequence,entity_count,encoded_bytes,captured_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, grant.FamilyID, snapshotID, principal.AccountID, principal.DeviceID,
		principal.SessionID, s.identity.InstanceID, s.identity.RecoveryEpoch, principal.SecurityRevision, highWater, entityCount, fullBytes, now, expires); err != nil {
		return err
	}
	position, afterKind, afterID := 0, 0, "00000000-0000-0000-0000-000000000000"
	for position < entityCount {
		batch, err := s.repo.SnapshotBatch(ctx, tx, grant.FamilyID, afterKind, afterID, 256)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return fmt.Errorf("snapshot source changed while family locked")
		}
		staged := &pgx.Batch{}
		for _, entity := range batch {
			raw, marshalErr := json.Marshal(entity)
			if marshalErr != nil {
				return marshalErr
			}
			staged.Queue(`INSERT INTO sync_snapshot_entities(family_id,snapshot_id,account_id,device_id,session_id,
			server_instance_id,recovery_epoch,security_revision,position,entity_kind,entity_id,entity_version,canonical_entity,encoded_bytes)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, grant.FamilyID, snapshotID, principal.AccountID, principal.DeviceID, principal.SessionID,
				s.identity.InstanceID, s.identity.RecoveryEpoch, principal.SecurityRevision, position, entity.EntityKind, entity.Meta.ID,
				entity.Meta.EntityVersion, raw, len(raw))
			position++
			afterKind, afterID = snapshotKindOrder(entity.EntityKind), entity.Meta.ID
		}
		results := tx.SendBatch(ctx, staged)
		for range batch {
			if _, err := results.Exec(); err != nil {
				_ = results.Close()
				return err
			}
		}
		if err := results.Close(); err != nil {
			return err
		}
	}
	return s.buildSnapshotPage(ctx, tx, principal, grant, snapshotID, highWater, uint64(entityCount), 0,
		s.limits.SyncPageSize, s.limits.SyncPageBytes, now, expires, nil, response)
}

func snapshotKindOrder(kind string) int {
	switch kind {
	case "baby":
		return 1
	case "feed":
		return 2
	case "sleep":
		return 3
	case "diaper":
		return 4
	case "growth_profile":
		return 7
	case "measurement":
		return 8
	case "supplement":
		return 6
	case "timer":
		return 5
	}
	return 0
}

func fullSnapshotEncodedBytes(identity protocol.ServerInfo, snapshotID string, highWater protocol.Cursor, count int, entityBytes int64) int {
	empty := snapshotResponseBytes(identity, snapshotID, highWater, []protocol.CanonicalEntity{}, nil)
	if count == 0 {
		return empty
	}
	return empty + int(entityBytes) + count - 1
}

func (s *Service) buildSnapshotPage(ctx context.Context, tx pgx.Tx, principal access.Principal, grant access.Grant, snapshotID string,
	highWater, rangeEnd, resume uint64, itemLimit, byteLimit int, created, expires time.Time, presented *decodedPageToken,
	response *protocol.SnapshotResponse) error {
	pageKeyID := s.pageKeys.Current.ID
	if presented != nil {
		pageKeyID = presented.key.ID
	}
	entities, end, hasMore, err := readSnapshotPage(ctx, tx, grant.FamilyID, snapshotID, resume, rangeEnd, itemLimit, byteLimit,
		s.identity, highWater, pageTokenPlaceholder(pageKeyID))
	if err != nil {
		return err
	}
	result := protocol.SnapshotResponse{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch,
		SnapshotID: snapshotID, HighWater: protocol.Cursor{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch,
			FamilyID: grant.FamilyID, Sequence: highWater}, Entities: entities}
	if hasMore {
		key, lookup := s.pageKeys.Current, [32]byte{}
		if presented == nil {
			if _, err := rand.Read(lookup[:]); err != nil {
				return err
			}
		} else {
			key = presented.key
			lookup = successorLookup(*presented, pageBinding{kind: "snapshot", accountID: principal.AccountID, deviceID: principal.DeviceID,
				sessionID: principal.SessionID, familyID: grant.FamilyID, instanceID: s.identity.InstanceID, epoch: s.identity.RecoveryEpoch,
				securityRevision: principal.SecurityRevision, highWater: highWater, rangeEnd: rangeEnd, resume: end, snapshotID: &snapshotID})
		}
		nextEntities, nextEnd, _, err := readSnapshotPage(ctx, tx, grant.FamilyID, snapshotID, end, rangeEnd, itemLimit, byteLimit,
			s.identity, highWater, pageTokenPlaceholder(key.ID))
		if err != nil {
			return err
		}
		token, err := encodePageToken(key, lookup)
		if err != nil {
			return err
		}
		binding := pageBinding{kind: "snapshot", familyID: grant.FamilyID, accountID: principal.AccountID, deviceID: principal.DeviceID,
			sessionID: principal.SessionID, instanceID: s.identity.InstanceID, epoch: s.identity.RecoveryEpoch, securityRevision: principal.SecurityRevision,
			highWater: highWater, rangeEnd: rangeEnd, resume: end, pageEnd: nextEnd, pageItems: len(nextEntities), snapshotID: &snapshotID,
			itemLimit: itemLimit, byteLimit: byteLimit, createdAt: created, expiresAt: expires}
		var successor *string
		if nextEnd < rangeEnd {
			placeholder := pageTokenPlaceholder(key.ID)
			successor = &placeholder
		}
		binding.pageBytes = snapshotResponseBytes(s.identity, snapshotID, result.HighWater, nextEntities, successor)
		if err := insertPageBinding(ctx, tx, key.ID, lookup, binding); err != nil {
			return err
		}
		result.NextPageToken = &token
	}
	*response = result
	return nil
}

func readSnapshotPage(ctx context.Context, tx pgx.Tx, familyID, snapshotID string, resume, rangeEnd uint64, itemLimit, byteLimit int,
	identity protocol.ServerInfo, highWater uint64, placeholder string) ([]protocol.CanonicalEntity, uint64, bool, error) {
	rows, err := tx.Query(ctx, `SELECT position,canonical_entity FROM sync_snapshot_entities
		WHERE family_id=$1 AND snapshot_id=$2 AND position >= $3 ORDER BY position LIMIT $4`, familyID, snapshotID, resume, itemLimit+1)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()
	entities := make([]protocol.CanonicalEntity, 0, itemLimit+1)
	positions := make([]uint64, 0, itemLimit+1)
	for rows.Next() {
		var position uint64
		var raw []byte
		if err := rows.Scan(&position, &raw); err != nil {
			return nil, 0, false, err
		}
		var entity protocol.CanonicalEntity
		if err := json.Unmarshal(raw, &entity); err != nil {
			return nil, 0, false, err
		}
		positions, entities = append(positions, position), append(entities, entity)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	return chooseSnapshotEntities(entities, positions, resume, rangeEnd, itemLimit, byteLimit, identity, familyID, snapshotID, highWater, placeholder)
}

func chooseSnapshotEntities(entities []protocol.CanonicalEntity, positions []uint64, resume, rangeEnd uint64, itemLimit, byteLimit int,
	identity protocol.ServerInfo, familyID, snapshotID string, highWater uint64, placeholder string) ([]protocol.CanonicalEntity, uint64, bool, error) {
	page := make([]protocol.CanonicalEntity, 0, min(itemLimit, len(entities)))
	end := resume
	high := protocol.Cursor{ServerInstanceID: identity.InstanceID, RecoveryEpoch: identity.RecoveryEpoch, FamilyID: familyID, Sequence: highWater}
	for index, entity := range entities {
		if index >= itemLimit {
			break
		}
		trial := append(append([]protocol.CanonicalEntity(nil), page...), entity)
		trialEnd := positions[index] + 1
		var next *string
		if trialEnd < rangeEnd {
			next = &placeholder
		}
		if snapshotResponseBytes(identity, snapshotID, high, trial, next) > byteLimit {
			if len(page) == 0 {
				return nil, 0, false, ErrSnapshotLimit
			}
			break
		}
		page, end = trial, trialEnd
	}
	if rangeEnd > resume && len(page) == 0 {
		return nil, 0, false, ErrSnapshotLimit
	}
	return page, end, end < rangeEnd, nil
}

func snapshotResponseBytes(identity protocol.ServerInfo, snapshotID string, highWater protocol.Cursor, entities []protocol.CanonicalEntity, next *string) int {
	return protocol.SnapshotPageEncodedBytes(identity, snapshotID, highWater, entities, next)
}

func (s *Service) readSnapshotContinuation(ctx context.Context, tx pgx.Tx, principal access.Principal, grant access.Grant, raw string, response *protocol.SnapshotResponse) error {
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
	if err := validatePageBinding(binding, token, principal, grant, s.identity, "snapshot", s.now()); err != nil {
		return err
	}
	if binding.snapshotID == nil {
		return ErrInvalidPageToken
	}
	var captured, expires time.Time
	var highWater, count uint64
	err = tx.QueryRow(ctx, `SELECT high_water_sequence,entity_count,captured_at,expires_at FROM sync_snapshots
		WHERE family_id=$1 AND id=$2`, grant.FamilyID, *binding.snapshotID).Scan(&highWater, &count, &captured, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidPageToken
	}
	if err != nil {
		return err
	}
	if !s.now().UTC().Before(expires) {
		return ErrPageExpired
	}
	if err := s.buildSnapshotPage(ctx, tx, principal, grant, *binding.snapshotID, highWater, count, binding.resume,
		binding.itemLimit, binding.byteLimit, captured, expires, &token, response); err != nil {
		return err
	}
	if response.HighWater.Sequence != binding.highWater || len(response.Entities) != binding.pageItems ||
		snapshotResponseBytes(s.identity, *binding.snapshotID, response.HighWater, response.Entities, response.NextPageToken) != binding.pageBytes {
		return ErrInvalidPageToken
	}
	end := binding.resume + uint64(len(response.Entities))
	if end != binding.pageEnd {
		return ErrInvalidPageToken
	}
	return nil
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
