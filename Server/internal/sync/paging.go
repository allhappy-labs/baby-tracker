package sync

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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

// pageBinding is a sync_page_bindings row: who a continuation token was
// issued to and the range it continues. A sync binding resumes after a
// sequence, up to the high water it was issued at; a snapshot binding
// resumes after a (kind, id) key of its snapshot. Neither records the next
// page's size or end: a continuation serves the current rows of its range.
type pageBinding struct {
	kind, keyID, familyID, accountID, deviceID, sessionID, instanceID string
	epoch, securityRevision, highWater                                uint64
	resumeSequence                                                    uint64           // sync
	snapshotID                                                        *string          // snapshot
	resumeKey                                                         care.SnapshotKey // snapshot
	itemLimit, byteLimit                                              int
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

// successorLookup is the lookup of the token that continues after binding,
// derived from the presented token: a replayed continuation that ends where
// it ended before issues the same successor (and finds its binding), one
// that ends elsewhere issues another.
func successorLookup(token decodedPageToken, binding pageBinding) [32]byte {
	mac := hmac.New(sha256.New, token.key.Secret[:])
	_, _ = mac.Write([]byte("baby-tracker-page-successor-v2\x00"))
	_, _ = mac.Write(token.lookup[:])
	snapshotID := ""
	if binding.snapshotID != nil {
		snapshotID = *binding.snapshotID
	}
	for _, value := range []string{binding.kind, binding.accountID, binding.deviceID, binding.sessionID, binding.familyID, binding.instanceID,
		snapshotID, binding.resumeKey.Kind, binding.resumeKey.ID} {
		_, _ = mac.Write([]byte(value))
		_, _ = mac.Write([]byte{0})
	}
	var encoded [8]byte
	for _, value := range []uint64{binding.epoch, binding.securityRevision, binding.highWater, binding.resumeSequence} {
		binary.BigEndian.PutUint64(encoded[:], value)
		_, _ = mac.Write(encoded[:])
	}
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

// sessionFamily is the family a principal method (ApplyAndPull,
// FullSnapshot) serves: the principal's session family, checked in Go
// before any statement, with the legacy refusals of a principal that is not
// a family member's refreshed family session. Nothing reads the family
// first: withFamily's grant comes from the session row, and a grant of any
// other family is refused.
func sessionFamily(p access.Principal) (string, error) {
	if p.Scope != "family" {
		return "", ErrFamilyScopeRequired
	}
	if p.SessionScope != "family" || !protocol.ValidUUID(p.SessionFamilyID) {
		return "", ErrSessionRefreshRequired
	}
	return p.SessionFamilyID, nil
}

// familyReadError is a principal method's guard refusal as the legacy
// family read (family.Service.Current) reported it: a session whose family
// moved into recovery since the principal was authenticated is
// family_unavailable, not recovery_only. (ServeSync and ServeSnapshot never
// see recovery_only: their caller's Admit refuses every session that is not
// a family session before family policy runs, in a pull's binding
// transaction (settle) as in its first.)
func familyReadError(err error) error {
	if errors.Is(err, authority.ErrRecoveryOnly) {
		return authority.ErrFamilyUnavailable
	}
	return err
}

// ApplyAndPull runs one sync request for an authenticated principal in one
// guarded transaction (Exchange), then publishes the new high water and
// returns the refusal or the response. It serves tests and the capacity
// harness; the HTTP route is ServeSync.
func (s *Service) ApplyAndPull(ctx context.Context, principal access.Principal, request protocol.SyncRequest) (protocol.SyncResponse, error) {
	if !s.ready() {
		return protocol.SyncResponse{}, errUnavailable
	}
	familyID, err := sessionFamily(principal)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	if err := s.validateSync(request, familyID); err != nil {
		return protocol.SyncResponse{}, err
	}
	txCtx, cancel := context.WithTimeout(ctx, time.Duration(s.limits.SyncTransactionSeconds)*time.Second)
	var exchanged Exchanged
	err = s.withFamily(txCtx, principal, familyID, func(tx pgx.Tx, grant access.Grant) error {
		var err error
		// validateSync accepted the request above, once per request.
		exchanged, err = s.exchange(txCtx, tx, principal, grant, request, true)
		return err
	})
	if err == nil {
		err = s.settle(txCtx, &exchanged, s.principalBindingTx(txCtx, principal, familyID))
	}
	cancel()
	if err != nil {
		return protocol.SyncResponse{}, familyReadError(err)
	}
	return s.committed(ctx, familyID, exchanged)
}

// committed is a sync request after its transaction committed: the new
// high water is published (best effort), then the refusal (Fatal) or the
// response is returned.
func (s *Service) committed(ctx context.Context, familyID string, exchanged Exchanged) (protocol.SyncResponse, error) {
	if exchanged.Publish != nil && s.publisher != nil {
		_ = s.publisher.Publish(ctx, familyID, *exchanged.Publish)
	}
	if exchanged.Fatal != nil {
		return protocol.SyncResponse{}, exchanged.Fatal
	}
	return exchanged.Response, nil
}

// syncRange is the range a sync page serves, (resume, highWater], its page
// limits, and for a continuation the first page's created and expiry times
// (zero on a first page).
type syncRange struct {
	resume, highWater    uint64
	itemLimit, byteLimit int
	created, expires     time.Time
}

// firstRange is a first page's range under the configured page limits.
func (s *Service) firstRange(after, highWater uint64) syncRange {
	return syncRange{resume: after, highWater: highWater, itemLimit: s.limits.SyncPageSize, byteLimit: s.limits.SyncPageBytes}
}

// issueFunc issues a page's continuation token for binding; presented is
// the continuation's own token (nil on a first page).
type issueFunc func(presented *decodedPageToken, binding pageBinding) (string, error)

// syncPage serves the changes of r from rows (read with a limit of
// itemLimit+1, in sequence order): the longest prefix within the item and
// byte limits, each change encoded once around its stored wire. A row left
// over after the prefix means more: the page carries a continuation token
// (counted in its size) bound to (last sequence, high water], issued by
// issue. A continuation (presented) derives its successor's lookup and
// keeps the first page's created and expiry times.
func (s *Service) syncPage(p access.Principal, g access.Grant, r syncRange, rows []care.ChangeRow,
	outcomes []protocol.Outcome, presented *decodedPageToken, issue issueFunc) (protocol.SyncResponse, error) {
	highWater, itemLimit, byteLimit, created, expires := r.highWater, r.itemLimit, r.byteLimit, r.created, r.expires
	encoded := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		raw, err := row.Encode(s.identity, g.FamilyID)
		if err != nil {
			return protocol.SyncResponse{}, err
		}
		encoded = append(encoded, raw)
	}
	keyID := s.pageKeys.Current.ID
	if presented != nil {
		keyID = presented.key.ID
	}
	count, err := pagePrefix(encoded, itemLimit, byteLimit, ErrPageLimit, func(bytes, count int, next *string) int {
		return protocol.IncrementalPageEncodedBytesRaw(s.identity, g.FamilyID, highWater, bytes, count, next)
	}, pageTokenPlaceholder(keyID))
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	response := protocol.SyncResponse{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch, Outcomes: outcomes,
		EncodedChanges: append(make([]json.RawMessage, 0, count), encoded[:count]...),
		HighWater:      protocol.Cursor{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch, FamilyID: g.FamilyID, Sequence: highWater}}
	if count == len(encoded) {
		return response, nil
	}
	if presented == nil {
		created = s.now().UTC().Truncate(time.Microsecond)
		expires = created.Add(time.Duration(s.limits.SyncPageTTLSeconds) * time.Second)
	}
	binding := s.bindingFor(p, g, "sync", highWater, itemLimit, byteLimit, created, expires)
	binding.resumeSequence = rows[count-1].Seq
	token, err := issue(presented, binding)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	response.NextPageToken = &token
	return response, nil
}

// pagePrefix is the length of the longest prefix of items within itemLimit
// items and byteLimit bytes, sized by size(prefix bytes, prefix count,
// token): the token placeholder counts iff an item is left after the
// prefix. A first item that does not fit is tooLarge.
func pagePrefix(items []json.RawMessage, itemLimit, byteLimit int, tooLarge error, size func(bytes, count int, next *string) int, placeholder string) (int, error) {
	count, bytes := 0, 0
	for i, item := range items {
		if i >= itemLimit {
			break
		}
		var next *string
		if i+1 < len(items) {
			next = &placeholder
		}
		if size(bytes+len(item), i+1, next) > byteLimit {
			if i == 0 {
				return 0, tooLarge
			}
			break
		}
		count, bytes = i+1, bytes+len(item)
	}
	return count, nil
}

func (s *Service) bindingFor(p access.Principal, g access.Grant, kind string, highWater uint64, itemLimit, byteLimit int, created, expires time.Time) pageBinding {
	return pageBinding{kind: kind, familyID: g.FamilyID, accountID: p.AccountID, deviceID: p.DeviceID, sessionID: p.SessionID,
		instanceID: s.identity.InstanceID, epoch: s.identity.RecoveryEpoch, securityRevision: p.SecurityRevision, highWater: highWater,
		itemLimit: itemLimit, byteLimit: byteLimit, createdAt: created, expiresAt: expires}
}

// pendingBinding is a prepared token's binding, not yet stored: its digest
// and, for a continuation (whose successor lookup is derived, so a replay
// finds the binding it stored), continuation set.
type pendingBinding struct {
	digest       [32]byte
	binding      pageBinding
	continuation bool
}

// prepareToken encodes the token for binding: a first page's lookup is
// random, a continuation's is its successor lookup. It returns the token
// and the binding, with its key id, under its digest.
func (s *Service) prepareToken(presented *decodedPageToken, binding pageBinding) (string, pendingBinding, error) {
	key, lookup := s.pageKeys.Current, [32]byte{}
	if presented == nil {
		random := rand.Read
		if s.pageLookup != nil {
			random = s.pageLookup
		}
		if _, err := random(lookup[:]); err != nil {
			return "", pendingBinding{}, err
		}
	} else {
		key, lookup = presented.key, successorLookup(*presented, binding)
	}
	token, err := encodePageToken(key, lookup)
	binding.keyID = key.ID
	return token, pendingBinding{digest: pageDigest(lookup), binding: binding, continuation: presented != nil}, err
}

// issueToken stores binding in tx in one round trip and returns its token.
// A binding already stored under the digest (a replayed continuation) must
// equal it.
func (s *Service) issueToken(ctx context.Context, tx pgx.Tx, presented *decodedPageToken, binding pageBinding) (string, error) {
	token, pending, err := s.prepareToken(presented, binding)
	if err != nil {
		return "", err
	}
	return token, storeBinding(ctx, tx, pending)
}

// issuedIn issues tokens whose bindings are stored in tx before its commit
// (issueToken).
func (s *Service) issuedIn(ctx context.Context, tx pgx.Tx) issueFunc {
	return func(presented *decodedPageToken, binding pageBinding) (string, error) {
		return s.issueToken(ctx, tx, presented, binding)
	}
}

// storeBinding stores b in tx in one round trip; a binding already stored
// under its digest must equal it (requireStoredBinding).
func storeBinding(ctx context.Context, tx pgx.Tx, b pendingBinding) error {
	batch := &pgx.Batch{}
	queueBinding(batch, b.digest, b.binding)
	results := tx.SendBatch(ctx, batch)
	inserted, err := bindingInserted(results)
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	if err == nil && !inserted {
		err = requireStoredBinding(ctx, tx, b.digest, b.binding)
	}
	return err
}

// storeBindingAtCommit stores b in a transaction that wrote nothing (a
// pull's binding, settle): in a request transaction
// (authority.CommitPipeline) the binding's INSERT goes with the COMMIT, in
// its round trip, and the binding is checked from the results. A binding
// not inserted then refuses the request after the commit, which is the
// same as before it: the transaction wrote nothing else, and the INSERT
// nothing. A continuation's binding is read back in the same round trip,
// after the INSERT, and must equal it; a first page's lookup is random, so
// a binding stored under it is never this one. Any other transaction
// stores the binding before the commit (storeBinding).
func storeBindingAtCommit(ctx context.Context, tx pgx.Tx, b pendingBinding) error {
	pipeline, ok := tx.(authority.CommitPipeline)
	if !ok {
		return storeBinding(ctx, tx, b)
	}
	pipeline.PipelineWithCommit(func(batch *pgx.Batch) {
		queueBinding(batch, b.digest, b.binding)
		if b.continuation {
			batch.Queue(sqlStoredBinding, b.binding.familyID, b.digest[:])
		}
	}, func(results pgx.BatchResults) error {
		inserted, err := bindingInserted(results)
		if err != nil {
			return err
		}
		if !b.continuation {
			if !inserted {
				return ErrInvalidPageToken
			}
			return nil
		}
		// Inserted, a read-back error refuses: a statement's error skipped
		// the commit; a Go-side scan error comes after it, leaving the binding
		// stored, unused until the sweep (harmless). Not inserted, anything but
		// an equal binding refuses, as requireStoredBinding does.
		stored, err := scanBinding(results.QueryRow())
		if inserted {
			return err
		}
		if err != nil || !samePageBinding(stored, b.binding) {
			return ErrInvalidPageToken
		}
		return nil
	})
	return nil
}

const bindingColumns = `b.kind::text, b.token_key_id, b.family_id::text, b.account_id::text, b.device_id::text, b.session_id::text,
	b.server_instance_id::text, b.recovery_epoch, b.security_revision, b.high_water_sequence, b.resume_sequence, b.snapshot_id::text,
	b.resume_kind, b.resume_id::text, b.item_limit, b.byte_limit, b.created_at, b.expires_at`

// sqlInsertBinding stores a binding; a stored one under the same digest is
// kept (sync_page_bindings_pkey).
const sqlInsertBinding = `INSERT INTO sync_page_bindings(family_id,token_digest,token_key_id,account_id,device_id,session_id,
	server_instance_id,recovery_epoch,security_revision,kind,high_water_sequence,resume_sequence,snapshot_id,resume_kind,resume_id,
	item_limit,byte_limit,created_at,expires_at)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::text::app.page_kind,$11,$12,$13,$14,$15,$16,$17,$18,$19)
	ON CONFLICT(family_id,token_digest) DO NOTHING`

func queueBinding(batch *pgx.Batch, digest [32]byte, b pageBinding) {
	var resumeSequence, resumeKind, resumeID any
	if b.kind == "sync" {
		resumeSequence = int64(b.resumeSequence)
	} else {
		code, _ := care.KindCode(b.resumeKey.Kind)
		resumeKind, resumeID = code, b.resumeKey.ID
	}
	batch.Queue(sqlInsertBinding, b.familyID, digest[:], b.keyID, b.accountID, b.deviceID, b.sessionID, b.instanceID, int64(b.epoch),
		int64(b.securityRevision), b.kind, int64(b.highWater), resumeSequence, b.snapshotID, resumeKind, resumeID, b.itemLimit, b.byteLimit,
		b.createdAt, b.expiresAt)
}

// bindingInserted reads queueBinding's result from results, which the
// caller owns and closes: whether the binding was inserted, or a binding
// was already stored under its digest.
func bindingInserted(results pgx.BatchResults) (bool, error) {
	tag, err := results.Exec()
	return err == nil && tag.RowsAffected() == 1, err
}

// sqlStoredBinding reads the binding stored under a digest.
const sqlStoredBinding = `SELECT ` + bindingColumns + ` FROM sync_page_bindings b WHERE b.family_id=$1 AND b.token_digest=$2`

// requireStoredBinding accepts a binding that was not inserted only when
// the stored one under its digest equals b (a replayed continuation);
// anything else is an invalid token. The caller has closed the batch
// results first.
func requireStoredBinding(ctx context.Context, tx pgx.Tx, digest [32]byte, b pageBinding) error {
	stored, err := scanBinding(tx.QueryRow(ctx, sqlStoredBinding, b.familyID, digest[:]))
	if err != nil || !samePageBinding(stored, b) {
		return ErrInvalidPageToken
	}
	return nil
}

// scanBinding scans bindingColumns followed by dest.
func scanBinding(row pgx.Row, dest ...any) (pageBinding, error) {
	var b pageBinding
	var resumeSequence *int64
	var resumeKind *int16
	var resumeID *string
	if err := row.Scan(append([]any{&b.kind, &b.keyID, &b.familyID, &b.accountID, &b.deviceID, &b.sessionID, &b.instanceID, &b.epoch,
		&b.securityRevision, &b.highWater, &resumeSequence, &b.snapshotID, &resumeKind, &resumeID, &b.itemLimit, &b.byteLimit,
		&b.createdAt, &b.expiresAt}, dest...)...); err != nil {
		return pageBinding{}, err
	}
	if resumeSequence != nil {
		b.resumeSequence = uint64(*resumeSequence)
	}
	if resumeKind != nil && resumeID != nil {
		name, ok := care.KindName(*resumeKind)
		if !ok {
			return pageBinding{}, fmt.Errorf("sync: binding resume kind %d", *resumeKind)
		}
		b.resumeKey = care.SnapshotKey{Kind: name, ID: *resumeID}
	}
	return b, nil
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
		left.highWater == right.highWater && left.resumeSequence == right.resumeSequence && leftSnapshot == rightSnapshot &&
		left.resumeKey == right.resumeKey && left.itemLimit == right.itemLimit && left.byteLimit == right.byteLimit &&
		left.createdAt.Equal(right.createdAt) && left.expiresAt.Equal(right.expiresAt)
}

func pageTokenPlaceholder(keyID string) string {
	key := identity.DigestKey{ID: keyID, Secret: [32]byte{1}}
	token, _ := encodePageToken(key, [32]byte{})
	return token
}

// sqlSyncContinuation reads a sync binding by digest and, in the same
// statement, the current rows of its range (resume, high water], at most
// item_limit+1 (sync_page_bindings_pkey, then care_entities_seq_key). A
// row edited since the page was issued has moved past the high water: the
// range then holds fewer rows, and that row arrives in the next pull.
const sqlSyncContinuation = `SELECT ` + bindingColumns + `, c.changed_seq, c.kind, c.id::text, c.entity_version, c.actor_account_id::text,
	c.device_id::text, c.operation::text, c.accepted_at, c.wire
	FROM sync_page_bindings b LEFT JOIN LATERAL (
		SELECT changed_seq, kind, id, entity_version, actor_account_id, device_id, operation, accepted_at, wire FROM care_entities
		WHERE family_id = $1 AND changed_seq > b.resume_sequence AND changed_seq <= b.high_water_sequence
		ORDER BY changed_seq LIMIT b.item_limit + 1) c ON true
	WHERE b.family_id = $1 AND b.token_digest = $2`

// syncContinuation serves a continuation in tx: the binding and its
// range's current rows in one statement, then the page, its successor's
// binding stored in tx (a transaction without a commit pipeline; a request
// transaction reads it with the commit, continuationAtCommit).
func (s *Service) syncContinuation(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, raw string) (protocol.SyncResponse, error) {
	token, err := decodePageToken(s.pageKeys, raw)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	digest := pageDigest(token.lookup)
	rows, err := tx.Query(ctx, sqlSyncContinuation, g.FamilyID, digest[:])
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	return s.continuationPage(p, g, token, rows, s.issuedIn(ctx, tx))
}

// continuationPage serves a continuation from sqlSyncContinuation's rows,
// which it closes: the binding must be the token's, the principal's and
// unexpired; the page is its range's current rows, its successor's token
// issued by issue.
func (s *Service) continuationPage(p access.Principal, g access.Grant, token decodedPageToken, rows pgx.Rows, issue issueFunc) (protocol.SyncResponse, error) {
	var binding pageBinding
	found := false
	changes := []care.ChangeRow{}
	for rows.Next() {
		var seq, version *int64
		var kind *int16
		var id, actor, device, operation *string
		var accepted *time.Time
		var wire []byte
		b, err := scanBinding(rows, &seq, &kind, &id, &version, &actor, &device, &operation, &accepted, &wire)
		if err != nil {
			rows.Close()
			return protocol.SyncResponse{}, err
		}
		binding, found = b, true
		if seq == nil {
			continue
		}
		name, ok := care.KindName(*kind)
		if !ok {
			rows.Close()
			return protocol.SyncResponse{}, fmt.Errorf("sync: change %d has kind code %d", *seq, *kind)
		}
		changes = append(changes, care.ChangeRow{Seq: uint64(*seq), Kind: name, ID: *id, Version: uint64(*version), ActorAccountID: *actor,
			DeviceID: *device, Operation: *operation, AcceptedAt: accepted.UTC(), Wire: wire})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return protocol.SyncResponse{}, err
	}
	if !found {
		return protocol.SyncResponse{}, ErrInvalidPageToken
	}
	if err := validatePageBinding(binding, token, p, g, s.identity, "sync", s.now()); err != nil {
		return protocol.SyncResponse{}, err
	}
	return s.syncPage(p, g, syncRange{resume: binding.resumeSequence, highWater: binding.highWater, itemLimit: binding.itemLimit,
		byteLimit: binding.byteLimit, created: binding.createdAt, expires: binding.expiresAt}, changes, []protocol.Outcome{}, &token, issue)
}

// pageAtCommit is a pull page read with the commit of its request
// transaction (pullAtCommit, continuationAtCommit): read once the queued
// statement's results were served, the response, and for a page with more
// the binding its token names, which settle stores after the commit.
type pageAtCommit struct {
	read     bool
	response protocol.SyncResponse
	binding  *pendingBinding
}

// serve keeps a page served from the commit's results.
func (page *pageAtCommit) serve(response protocol.SyncResponse, err error) error {
	if err != nil {
		return err
	}
	page.response, page.read = response, true
	return nil
}

// deferredTo issues a page's token at once and leaves its binding to
// settle (pageAtCommit.binding).
func (s *Service) deferredTo(page *pageAtCommit) issueFunc {
	return func(presented *decodedPageToken, binding pageBinding) (string, error) {
		token, pending, err := s.prepareToken(presented, binding)
		if err != nil {
			return "", err
		}
		page.binding = &pending
		return token, nil
	}
}

// pullAtCommit is a first pull page in a request transaction that wrote
// nothing: its one statement (care.QueuePull) goes with the COMMIT, in its
// round trip (authority.CommitPipeline), so a pull is two round trips,
// BEGIN with the authority read, then the pull with the commit. Every
// Go-side refusal before the statement (authentication, Admit, family
// policy, Charge, validateSync) has run by then. The refusals decided from
// the statement's rows (snapshot_required, page_limit, a family the tenant
// cannot see) come after a commit, which is safe only because the
// transaction wrote nothing: the pipeline checks that in the server before
// the statement (ErrPipelineAfterWrite), and the statement only reads.
// A page with more needs its binding stored, which settle does after this
// commit.
func (s *Service) pullAtCommit(pipeline authority.CommitPipeline, p access.Principal, g access.Grant, after uint64, limit int) Exchanged {
	page := &pageAtCommit{}
	var read func(pgx.BatchResults) (uint64, []care.ChangeRow, error)
	pipeline.PipelineWithCommit(func(b *pgx.Batch) { read = care.QueuePull(b, g.FamilyID, after, limit) }, func(results pgx.BatchResults) error {
		highWater, changes, err := read(results)
		if err != nil {
			return err
		}
		if after > highWater {
			return ErrSnapshotRequired
		}
		return page.serve(s.syncPage(p, g, s.firstRange(after, highWater), changes, []protocol.Outcome{}, nil, s.deferredTo(page)))
	})
	return Exchanged{page: page}
}

// continuationAtCommit is pullAtCommit for a continuation: the token is
// decoded (and refused) before any statement, then sqlSyncContinuation
// goes with the COMMIT and the binding is validated from its rows
// (invalid_page_token, page_expired, epoch_changed after a commit that
// wrote nothing).
func (s *Service) continuationAtCommit(pipeline authority.CommitPipeline, p access.Principal, g access.Grant, raw string) (Exchanged, error) {
	token, err := decodePageToken(s.pageKeys, raw)
	if err != nil {
		return Exchanged{}, err
	}
	digest := pageDigest(token.lookup)
	page := &pageAtCommit{}
	pipeline.PipelineWithCommit(func(b *pgx.Batch) { b.Queue(sqlSyncContinuation, g.FamilyID, digest[:]) }, func(results pgx.BatchResults) error {
		rows, err := results.Query()
		if err != nil {
			return err
		}
		return page.serve(s.continuationPage(p, g, token, rows, s.deferredTo(page)))
	})
	return Exchanged{page: page}, nil
}

// bindingTx runs run in the second transaction of a pull page with more
// (settle), authorized as a new request of the same caller would be, with
// the principal and grant of that authorization.
type bindingTx func(run func(pgx.Tx, access.Principal, access.Grant) error) error

// requestBindingTx is ServeSync's binding transaction: req's credential
// authorized again by WithRequestTx with req's Admit and no Charge (the
// request was charged once, in its first transaction).
func (s *Service) requestBindingTx(ctx context.Context, req authority.Request) bindingTx {
	again := authority.Request{Digests: req.Digests, Admit: req.Admit}
	return func(run func(pgx.Tx, access.Principal, access.Grant) error) error {
		return s.guard.WithRequestTx(ctx, again, "read", run)
	}
}

// principalBindingTx is ApplyAndPull's binding transaction: p's session
// family re-read as ApplyAndPull's own (withFamily).
func (s *Service) principalBindingTx(ctx context.Context, p access.Principal, familyID string) bindingTx {
	return func(run func(pgx.Tx, access.Principal, access.Grant) error) error {
		return s.withFamily(ctx, p, familyID, func(tx pgx.Tx, g access.Grant) error { return run(tx, p, g) })
	}
}

// settle completes a request whose pull page was read with the commit
// (pageAtCommit): Response becomes the page. A page with more names a
// binding not stored yet: settle stores it in a second transaction (in),
// its INSERT with that COMMIT (storeBindingAtCommit). That transaction's
// refusal or failure is the request's, and the page is not served. Any
// other request's exchange is left as it is.
//
// The second transaction is authorized the way a new request of the same
// caller is, in the same order, before any statement, so a refusal there
// is the code a new request would get in that state (docs/protocol/v1.md,
// paged pulls):
//   - ServeSync (requestBindingTx) repeats everything the first
//     transaction checked except the rate limit: the bearer's digests
//     looked up again, the session (live, epoch, deadline, access token
//     expiry), the caller's Admit (family-scope session), family policy for
//     "read" and the entitlement. Charge is not repeated: the request was
//     charged once, and a second charge would count one request twice.
//   - ApplyAndPull (principalBindingTx) repeats withFamily: the session
//     re-read by ID, then family policy, as its first transaction.
//
// The binding must then be the one the page's token names: the same
// session, account, device and security revision (a digest lookup that
// found another session) and the same family (a session whose family
// changed since the page was read). Otherwise the page is refused as
// unavailable (retryable): its token would name a binding that is not the
// caller's, and the next request is served from the new state.
func (s *Service) settle(ctx context.Context, exchanged *Exchanged, in bindingTx) error {
	page := exchanged.page
	if page == nil {
		return nil
	}
	exchanged.page = nil
	if !page.read {
		return errUnavailable
	}
	if page.binding != nil {
		pending := *page.binding
		b := pending.binding
		if err := in(func(tx pgx.Tx, p access.Principal, g access.Grant) error {
			if p.SessionID != b.sessionID || p.AccountID != b.accountID || p.DeviceID != b.deviceID || p.SecurityRevision != b.securityRevision ||
				g.AccountID != b.accountID || g.FamilyID != b.familyID {
				return authority.ErrUnavailable
			}
			return storeBindingAtCommit(ctx, tx, pending)
		}); err != nil {
			return err
		}
	}
	exchanged.Response = page.response
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
