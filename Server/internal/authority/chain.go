package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// maxAuditPayloadBytes bounds one audit row's canonical payload.
const maxAuditPayloadBytes = 64 << 10

// chainHead is one audit chain's head (scope row audit_seq, audit_hash,
// chain_failed) as read under the scope row's FOR NO KEY UPDATE lock by lockAccounts
// or lockFamilies. appendAudit computes the next row from it and advances it,
// so several appends to one scope in one transaction chain correctly.
type chainHead struct {
	Scope  scopeRef
	Seq    uint64
	Hash   [32]byte
	Failed bool
}

func newChainHead(scope scopeRef, seq uint64, hash []byte, failed bool) (*chainHead, error) {
	if len(hash) != sha256.Size || seq > maxWireInteger {
		return nil, fmt.Errorf("authority: corrupt audit head on %s %s: %w", scope.kind, scope.id, ErrUnavailable)
	}
	head := &chainHead{Scope: scope, Seq: seq, Failed: failed}
	copy(head.Hash[:], hash)
	return head, nil
}

// appendAudit appends one row to head's audit chain and moves the chain head
// on the scope's account or family row, in one round trip, returning the new
// seq and hash and advancing *head to them.
//
// head must come from the caller's lockAccounts/lockFamilies in this
// transaction. appendAudit takes no lock of its own: the head UPDATE is
// guarded on the expected (audit_seq, audit_hash) and NOT chain_failed, and
// the audit INSERT reads from the UPDATE, so a stale head (a scope the caller
// did not lock, which another transaction moved) inserts nothing and fails
// closed instead of forking the chain. A failed chain is ErrScopeFailed; a
// stale head is a wrapped ErrUnavailable. at is stored, and hashed, at
// microsecond precision.
func appendAudit(ctx context.Context, tx pgx.Tx, head *chainHead, kind Kind, commandID, actorID string, at time.Time, payload any) (uint64, [32]byte, error) {
	if head == nil {
		return 0, [32]byte{}, ErrInvalidCommand
	}
	table, ok := scopeTable(head.Scope.kind)
	if !ok || !validID(head.Scope.id) || !kind.valid() || !validID(commandID) || !validID(actorID) || !validTime(at) {
		return 0, [32]byte{}, ErrInvalidCommand
	}
	if head.Failed {
		return 0, [32]byte{}, ErrScopeFailed
	}
	if head.Seq >= maxWireInteger {
		return 0, [32]byte{}, fmt.Errorf("authority: append audit: chain exhausted on %s %s: %w", head.Scope.kind, head.Scope.id, ErrUnavailable)
	}
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return 0, [32]byte{}, err
	}
	if len(canonical) > maxAuditPayloadBytes {
		return 0, [32]byte{}, ErrInvalidCommand
	}
	at = at.UTC().Truncate(time.Microsecond)
	seq := head.Seq + 1
	hash := auditRowHash(head.Scope.kind, head.Scope.id, seq, head.Hash, kind, commandID, actorID, at, canonical)

	var stored uint64
	err = tx.QueryRow(ctx, `WITH moved AS (
			UPDATE `+table+` SET audit_seq=$3, audit_hash=$5
			 WHERE id=$2 AND audit_seq=$4 AND audit_hash=$6 AND NOT chain_failed
			RETURNING id)
		INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
		SELECT $1::text::app.audit_scope, moved.id, $3, $6, $5, $7::text::app.audit_kind, $8, $9, $10, $11 FROM moved
		RETURNING seq`,
		string(head.Scope.kind), head.Scope.id, int64(seq), int64(head.Seq), hash[:], head.Hash[:], string(kind), commandID, actorID, at, canonical).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, [32]byte{}, staleHead(ctx, tx, table, head)
	}
	if err != nil {
		return 0, [32]byte{}, fmt.Errorf("authority: append audit: %w", err)
	}
	if stored != seq {
		return 0, [32]byte{}, fmt.Errorf("authority: append audit: stored seq %d, want %d: %w", stored, seq, ErrUnavailable)
	}
	head.Seq, head.Hash = seq, hash
	return seq, hash, nil
}

// staleHead explains a guarded append that matched no row (only on that
// failure path): a chain that failed meanwhile is ErrScopeFailed, anything
// else (head moved, row missing) is ErrUnavailable.
func staleHead(ctx context.Context, tx pgx.Tx, table string, head *chainHead) error {
	var failed bool
	err := tx.QueryRow(ctx, `SELECT chain_failed FROM `+table+` WHERE id=$1`, head.Scope.id).Scan(&failed)
	if err == nil && failed {
		return ErrScopeFailed
	}
	return fmt.Errorf("authority: append audit: stale audit head on %s %s (seq %d): %w", head.Scope.kind, head.Scope.id, head.Seq, errors.Join(err, ErrUnavailable))
}

// auditRowHash is an audit row's hash:
//
//	sha256(scope 0x00 scope_id 0x00 seq(8 bytes BE) prev_hash kind 0x00
//	       command_id 0x00 actor_id 0x00 at.UTC().Format(RFC3339Nano) 0x00 payload)
//
// IDs are lowercase canonical UUID text, prev_hash is 32 bytes (zeros for
// seq 1) and payload is the row's canonical JSON (canonicalJSON).
func auditRowHash(scope Scope, scopeID string, seq uint64, prev [32]byte, kind Kind, commandID, actorID string, at time.Time, payload []byte) [32]byte {
	digest := sha256.New()
	var be [8]byte
	binary.BigEndian.PutUint64(be[:], seq)
	write := func(parts ...[]byte) {
		for _, part := range parts {
			digest.Write(part)
		}
	}
	zero := []byte{0}
	write([]byte(scope), zero, []byte(scopeID), zero, be[:], prev[:], []byte(kind), zero, []byte(commandID), zero, []byte(actorID), zero,
		[]byte(at.UTC().Format(time.RFC3339Nano)), zero, payload)
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	return sum
}

func scopeTable(scope Scope) (string, bool) {
	switch scope {
	case ScopeAccount:
		return "accounts", true
	case ScopeFamily:
		return "families", true
	default:
		return "", false
	}
}

func validTime(at time.Time) bool {
	return !at.IsZero() && at.Year() >= 1970 && at.Year() <= 9999
}

var integerLiteral = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)

// canonicalJSON encodes v as canonical JSON: object keys sorted bytewise, no
// insignificant whitespace, strings escaped as encoding/json escapes them
// without HTML escaping, and numbers restricted to integer literals. The
// encoding survives a jsonb round trip: canonicalJSON(json.RawMessage(text of
// the stored jsonb)) reproduces the bytes that were hashed, which is how the
// verifier recomputes row hashes. Anything json.Marshal refuses, or any
// fractional or exponent number, or any U+0000, is ErrInvalidCommand.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, ErrInvalidCommand
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		return nil, ErrInvalidCommand
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidCommand
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		if !integerLiteral.MatchString(v.String()) {
			return ErrInvalidCommand
		}
		buf.WriteString(v.String())
	case string:
		if err := writeCanonicalString(buf, v); err != nil {
			return err
		}
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalString(buf, key); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, v[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return ErrInvalidCommand
	}
	return nil
}

// writeCanonicalString refuses U+0000, which jsonb cannot store, so such a
// payload is an invalid command rather than a failed insert.
func writeCanonicalString(buf *bytes.Buffer, s string) error {
	if strings.ContainsRune(s, 0) {
		return ErrInvalidCommand
	}
	encoder := json.NewEncoder(buf)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(s) // a decoded string always encodes
	buf.Truncate(buf.Len() - 1)
	return nil
}
