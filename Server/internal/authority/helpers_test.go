package authority_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
)

type auditRow struct {
	Scope     string
	ScopeID   string
	Seq       uint64
	PrevHash  [32]byte
	Hash      [32]byte
	Kind      string
	CommandID string
	ActorID   string
	At        time.Time
	Payload   []byte
}

// readAudit returns a scope's audit rows in seq order, read through an
// authority transaction on the runtime pool.
func readAudit(t *testing.T, f *authoritytest.Fixture, scope authority.Scope, scopeID string) []auditRow {
	t.Helper()
	var out []auditRow
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		rows, err := tx.Query(t.Context(), `SELECT scope::text, scope_id::text, seq, prev_hash, hash, kind::text, command_id::text, actor_account_id::text, at, payload::text
			FROM security_audit_events WHERE scope=$1::text::app.audit_scope AND scope_id=$2 ORDER BY seq, at`, string(scope), scopeID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var row auditRow
			var prev, hash []byte
			var payload string
			if err := rows.Scan(&row.Scope, &row.ScopeID, &row.Seq, &prev, &hash, &row.Kind, &row.CommandID, &row.ActorID, &row.At, &payload); err != nil {
				t.Fatal(err)
			}
			copy(row.PrevHash[:], prev)
			copy(row.Hash[:], hash)
			row.Payload = []byte(payload)
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})
	return out
}

// chainHead reads the audit head stored on an account or family row.
func chainHead(t *testing.T, f *authoritytest.Fixture, scope authority.Scope, id string) (uint64, [32]byte) {
	t.Helper()
	table := "accounts"
	if scope == authority.ScopeFamily {
		table = "families"
	}
	var seq uint64
	var hash []byte
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		if err := tx.QueryRow(t.Context(), `SELECT audit_seq, audit_hash FROM `+table+` WHERE id=$1`, id).Scan(&seq, &hash); err != nil {
			t.Fatal(err)
		}
	})
	var out [32]byte
	copy(out[:], hash)
	return seq, out
}

func inAuthorityTx(t *testing.T, f *authoritytest.Fixture, run func(pgx.Tx)) {
	t.Helper()
	tx, err := f.Service.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	run(tx)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// recompute rebuilds a stored row's hash the way the verifier will: from the
// stored columns, with the payload re-canonicalised from its jsonb text.
func recompute(t *testing.T, row auditRow) [32]byte {
	t.Helper()
	payload, err := authority.CanonicalJSON(json.RawMessage(row.Payload))
	if err != nil {
		t.Fatal(err)
	}
	return authority.AuditRowHash(authority.Scope(row.Scope), row.ScopeID, row.Seq, row.PrevHash, authority.Kind(row.Kind), row.CommandID, row.ActorID, row.At, payload)
}
