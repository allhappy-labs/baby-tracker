package sync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// FullSnapshot serves one snapshot page for an authenticated principal in
// one guarded transaction (SnapshotPage). It serves tests and the capacity
// harness; the HTTP route is ServeSnapshot.
func (s *Service) FullSnapshot(ctx context.Context, principal access.Principal, request protocol.SnapshotRequest) (protocol.SnapshotResponse, error) {
	if !s.ready() {
		return protocol.SnapshotResponse{}, errUnavailable
	}
	familyID, err := sessionFamily(principal)
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	operationCtx, cancel := context.WithTimeout(ctx, time.Duration(s.limits.SyncTransactionSeconds)*time.Second)
	defer cancel()
	var response protocol.SnapshotResponse
	err = s.withFamily(operationCtx, principal, familyID, func(tx pgx.Tx, grant access.Grant) error {
		var err error
		response, err = s.SnapshotPage(operationCtx, tx, principal, grant, request)
		return err
	})
	if err != nil {
		return protocol.SnapshotResponse{}, familyReadError(err)
	}
	return response, nil
}

// sqlSnapshotAdmissionLocks serializes snapshot admission per account and
// per family for the rest of the transaction, so concurrent captures cannot
// both pass a cap. Every capture takes the account lock before the family
// lock, so they cannot deadlock; no care row and no family row is locked.
const (
	sqlSnapshotAccountLock = `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 5713))`
	sqlSnapshotFamilyLock  = `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 5714))`
)

// sqlSnapshotAdmission counts the active snapshot sessions of the account
// (in any family) and of the family, each through its own index
// (sync_snapshot_sessions_account_idx, _family_idx) with both columns as
// index conditions. (One scan with "account_id = $1 OR family_id = $2"
// planned as a scan of a whole index bounded by expires_at alone.)
const sqlSnapshotAdmission = `SELECT
	(SELECT count(*) FROM sync_snapshot_sessions WHERE account_id = $1 AND expires_at > $3),
	(SELECT count(*) FROM sync_snapshot_sessions WHERE family_id = $2 AND expires_at > $3)`

const sqlInsertSnapshotSession = `INSERT INTO sync_snapshot_sessions(family_id,id,account_id,device_id,session_id,server_instance_id,
	recovery_epoch,security_revision,high_water_sequence,captured_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`

// SnapshotPage serves one snapshot page inside tx, a transaction the
// caller's guard opened with g's tenant context.
//
// A first page records the family's high water and pages the current rows
// in (kind, id) order with no row lock and no copy: one round trip takes
// the admission locks and reads the admission counts, the family state
// (high water and size counters) and the first page; a second stores the session and, when there is more, the
// continuation binding. A continuation reads its binding, its session and
// the page in one statement. A row changed during paging has a sequence
// above the high water and also arrives in the next pull.
func (s *Service) SnapshotPage(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, request protocol.SnapshotRequest) (protocol.SnapshotResponse, error) {
	if err := s.seamReady(p, g); err != nil {
		return protocol.SnapshotResponse{}, err
	}
	if request.PageToken != nil {
		return s.snapshotContinuation(ctx, tx, p, g, *request.PageToken)
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	batch := &pgx.Batch{}
	batch.Queue(sqlSnapshotAccountLock, p.AccountID)
	batch.Queue(sqlSnapshotFamilyLock, g.FamilyID)
	batch.Queue(sqlSnapshotAdmission, p.AccountID, g.FamilyID, now)
	// The state is read before the page, so every row changed at or below
	// the recorded high water is on the pages.
	readState := care.QueueFamilyState(batch, g.FamilyID)
	readPage := care.QueueSnapshotPage(batch, g.FamilyID, care.FirstSnapshotKey, s.limits.SyncPageSize+1)
	results := tx.SendBatch(ctx, batch)
	var accountActive, familyActive int64
	_, err := results.Exec()
	if err == nil {
		_, err = results.Exec()
	}
	if err == nil {
		err = results.QueryRow().Scan(&accountActive, &familyActive)
	}
	var state care.FamilyState
	if err == nil {
		state, err = readState(results)
	}
	var rows []care.SnapshotRow
	if err == nil {
		rows, err = readPage(results)
	}
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	if accountActive >= int64(s.limits.ActiveSnapshotAccountLimit) || familyActive >= int64(s.limits.ActiveSnapshotFamilyLimit) {
		return protocol.SnapshotResponse{}, ErrSnapshotLimit
	}
	snapshotID, err := randomUUID()
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	highWater := protocol.Cursor{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch, FamilyID: g.FamilyID, Sequence: state.Sequence}
	full := protocol.SnapshotPageEncodedBytesRaw(s.identity, snapshotID, highWater, int(state.WireBytes), int(state.EntityCount), nil)
	if state.EntityCount > uint64(s.limits.SnapshotMaxEntities) || full > s.limits.SnapshotMaxBytes {
		return protocol.SnapshotResponse{}, ErrSnapshotLimit
	}
	expires := now.Add(time.Duration(s.limits.SnapshotTTLSeconds) * time.Second)
	response, binding, err := s.snapshotPage(p, g, snapshotID, highWater, rows, s.limits.SyncPageSize, s.limits.SyncPageBytes, now, expires, nil)
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	store := &pgx.Batch{}
	store.Queue(sqlInsertSnapshotSession, g.FamilyID, snapshotID, p.AccountID, p.DeviceID, p.SessionID, s.identity.InstanceID,
		int64(s.identity.RecoveryEpoch), int64(p.SecurityRevision), int64(state.Sequence), now, expires)
	var digest [32]byte
	if binding != nil {
		token, pending, err := s.prepareToken(nil, *binding)
		if err != nil {
			return protocol.SnapshotResponse{}, err
		}
		digest, *binding = pending.digest, pending.binding
		queueBinding(store, digest, *binding)
		response.NextPageToken = &token
	}
	stored := tx.SendBatch(ctx, store)
	_, err = stored.Exec()
	inserted := true
	if err == nil && binding != nil {
		inserted, err = bindingInserted(stored)
	}
	if closeErr := stored.Close(); err == nil {
		err = closeErr
	}
	if err == nil && !inserted {
		err = requireStoredBinding(ctx, tx, digest, *binding)
	}
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	return response, nil
}

// snapshotPage is the longest prefix of rows (read with a limit of
// itemLimit+1, in key order) within the item and byte limits; with a row
// left over it also returns the binding of the continuation, resuming
// after the page's last key.
//
// Rows are served verbatim, as stored. A running timer's stored parts and
// bottle start carry the timestamps as the phone sent them (e.g. .250Z),
// as the legacy pull served the same entity version; the legacy snapshot
// rendered them from its tables normalized (.250000Z). That difference is
// intended (Phase 2 Task 7 ruling): both name the same instant and the
// client parses either.
func (s *Service) snapshotPage(p access.Principal, g access.Grant, snapshotID string, highWater protocol.Cursor, rows []care.SnapshotRow,
	itemLimit, byteLimit int, created, expires time.Time, presented *decodedPageToken) (protocol.SnapshotResponse, *pageBinding, error) {
	entities := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		entities = append(entities, row.Wire)
	}
	keyID := s.pageKeys.Current.ID
	if presented != nil {
		keyID = presented.key.ID
	}
	count, err := pagePrefix(entities, itemLimit, byteLimit, ErrSnapshotLimit, func(bytes, count int, next *string) int {
		return protocol.SnapshotPageEncodedBytesRaw(s.identity, snapshotID, highWater, bytes, count, next)
	}, pageTokenPlaceholder(keyID))
	if err != nil {
		return protocol.SnapshotResponse{}, nil, err
	}
	response := protocol.SnapshotResponse{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch,
		SnapshotID: snapshotID, HighWater: highWater, EncodedEntities: append(make([]json.RawMessage, 0, count), entities[:count]...)}
	if count == len(entities) {
		return response, nil, nil
	}
	binding := s.bindingFor(p, g, "snapshot", highWater.Sequence, itemLimit, byteLimit, created, expires)
	id := snapshotID
	binding.snapshotID, binding.resumeKey = &id, rows[count-1].Key
	return response, &binding, nil
}

// sqlSnapshotContinuation reads a snapshot binding by digest, its session
// and, in the same statement, the rows after its resume key, at most
// item_limit+1 (sync_page_bindings_pkey, sync_snapshot_sessions_pkey,
// care_entities_snapshot_idx). The session is a one-row probe per binding
// (a lateral subquery with LIMIT 1, which the planner cannot flatten): as a
// plain join, a generic plan could merge-join it over the pkey scanned from
// the family's first session whenever the bound session's id sorts first.
const sqlSnapshotContinuation = `SELECT ` + bindingColumns + `, s.high_water_sequence, s.captured_at, s.expires_at, c.kind, c.id::text, c.wire
	FROM sync_page_bindings b
	LEFT JOIN LATERAL (
		SELECT high_water_sequence, captured_at, expires_at FROM sync_snapshot_sessions
		WHERE family_id = $1 AND id = b.snapshot_id LIMIT 1) s ON true
	LEFT JOIN LATERAL (
		SELECT kind, id, wire FROM care_entities
		WHERE family_id = $1 AND (kind, id) > (b.resume_kind, b.resume_id) ORDER BY kind, id LIMIT b.item_limit + 1) c ON b.kind = 'snapshot'
	WHERE b.family_id = $1 AND b.token_digest = $2`

func (s *Service) snapshotContinuation(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, raw string) (protocol.SnapshotResponse, error) {
	token, err := decodePageToken(s.pageKeys, raw)
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	digest := pageDigest(token.lookup)
	rows, err := tx.Query(ctx, sqlSnapshotContinuation, g.FamilyID, digest[:])
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	var binding pageBinding
	var highWater *int64
	var captured, expires *time.Time
	found := false
	page := []care.SnapshotRow{}
	for rows.Next() {
		var kind *int16
		var id *string
		var wire []byte
		b, err := scanBinding(rows, &highWater, &captured, &expires, &kind, &id, &wire)
		if err != nil {
			rows.Close()
			return protocol.SnapshotResponse{}, err
		}
		binding, found = b, true
		if kind == nil {
			continue
		}
		name, ok := care.KindName(*kind)
		if !ok {
			rows.Close()
			return protocol.SnapshotResponse{}, fmt.Errorf("sync: row %s has kind code %d", *id, *kind)
		}
		page = append(page, care.SnapshotRow{Key: care.SnapshotKey{Kind: name, ID: *id}, Wire: wire})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return protocol.SnapshotResponse{}, err
	}
	if !found {
		return protocol.SnapshotResponse{}, ErrInvalidPageToken
	}
	if err := validatePageBinding(binding, token, p, g, s.identity, "snapshot", s.now()); err != nil {
		return protocol.SnapshotResponse{}, err
	}
	if binding.snapshotID == nil || highWater == nil || uint64(*highWater) != binding.highWater {
		return protocol.SnapshotResponse{}, ErrInvalidPageToken
	}
	if !s.now().UTC().Before(*expires) {
		return protocol.SnapshotResponse{}, ErrPageExpired
	}
	cursor := protocol.Cursor{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch, FamilyID: g.FamilyID, Sequence: binding.highWater}
	response, next, err := s.snapshotPage(p, g, *binding.snapshotID, cursor, page, binding.itemLimit, binding.byteLimit, *captured, *expires, &token)
	if err != nil || next == nil {
		return response, err
	}
	nextToken, err := s.issueToken(ctx, tx, &token, *next)
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	response.NextPageToken = &nextToken
	return response, nil
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
