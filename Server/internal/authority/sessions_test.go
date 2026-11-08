package authority_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5"
)

// testDerive derives a child deterministically from everything the input
// carries, like the auth package's v2/v3 derivation does.
func testDerive(in authority.RefreshDerivationInput) (authority.RefreshDerivation, error) {
	material := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%d|%d|%s", in.Parent.ID, in.Parent.LineageRootID, in.OutputKeyID, in.Scope, in.FamilyID, in.Role,
		in.AttemptID, in.AccessExpiresAt.UnixMicro(), in.RefreshExpiresAt.UnixMicro(), in.Parent.DeadlineAt.UTC().Format(time.RFC3339Nano))
	if in.Continuity != nil {
		material += fmt.Sprintf("|%s|%s|%x", in.Continuity.DeviceID, in.Continuity.CredentialKeyID, in.Continuity.CredentialDigest)
	}
	out := func(label string) []byte {
		mac := hmac.New(sha256.New, in.ParentKey.Secret[:])
		mac.Write([]byte(label + "\x00" + material))
		mac.Write(in.ParentDigest[:])
		return mac.Sum(nil)
	}
	return authority.RefreshDerivation{CommandID: uuidOf(out("command")), SessionID: uuidOf(out("session")),
		AccessToken: base64.RawURLEncoding.EncodeToString(out("access")), RefreshToken: base64.RawURLEncoding.EncodeToString(out("refresh"))}, nil
}

func uuidOf(raw []byte) string {
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// testMaterial commits to the whole stage, as a naive encoder would; it
// refuses a stage that still carries a key secret.
func testMaterial(stage authority.RefreshStage) ([]byte, [32]byte, error) {
	if stage.Input.ParentKey.Secret != ([32]byte{}) {
		return nil, [32]byte{}, errors.New("material received the parent key secret")
	}
	encoded, err := json.Marshal(stage)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return encoded, sha256.Sum256(encoded), nil
}

func candidates(keys identity.DigestKeyring, raw string) []authority.KeyedDigest {
	out := []authority.KeyedDigest{{KeyID: keys.Current.ID, Sum: authoritytest.TokenDigest(keys.Current, "refresh", raw)}}
	for _, key := range keys.Previous {
		out = append(out, authority.KeyedDigest{KeyID: key.ID, Sum: authoritytest.TokenDigest(key, "refresh", raw)})
	}
	return out
}

func refreshOf(f *authoritytest.Fixture, raw, attempt string, continuity *authority.DeviceBinding) authority.Refresh {
	return authority.Refresh{ParentRefreshDigestCandidates: candidates(f.TokenKeys, raw), At: f.Now, AttemptID: attempt, Continuity: continuity,
		Derive: testDerive, AccessTTL: 10 * time.Minute, RefreshTTL: 30 * 24 * time.Hour, Mode: authority.ModeSelfHosted, Material: testMaterial}
}

// binding is the continuity binding authoritytest.Device installs.
func binding(account, device string) *authority.DeviceBinding {
	b := &authority.DeviceBinding{DeviceID: device, AccountID: account, CanonicalOrigin: "https://tracker.example", CredentialKeyID: "continuity.v1"}
	copy(b.CredentialDigest[:], device)
	return b
}

func sessionRow(t *testing.T, f *authoritytest.Fixture, id string) (row authority.SessionRow, reason string) {
	t.Helper()
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		var access, refresh []byte
		if err := tx.QueryRow(t.Context(), `SELECT id::text, account_id::text, device_id::text, lineage_root_id::text, coalesce(parent_id::text,''),
				coalesce(family_id::text,''), scope::text, coalesce(role::text,''), status::text, recovery_epoch, issued_seq, token_key_id, access_digest,
				refresh_digest, access_expires_at, refresh_expires_at, deadline_at, lineage_started_at, created_at, consumed_at, revoked_at,
				coalesce(consumed_attempt_id::text,''), coalesce(child_id::text,''), coalesce(revoke_reason::text,'')
			FROM sessions WHERE id=$1`, id).Scan(&row.ID, &row.AccountID, &row.DeviceID, &row.LineageRootID, &row.ParentID, &row.FamilyID, &row.Scope,
			&row.Role, &row.Status, &row.RecoveryEpoch, &row.IssuedSeq, &row.TokenKeyID, &access, &refresh, &row.AccessExpiresAt, &row.RefreshExpiresAt,
			&row.DeadlineAt, &row.LineageStartedAt, &row.CreatedAt, &row.ConsumedAt, &row.RevokedAt, &row.ConsumedAttemptID, &row.ChildID, &reason); err != nil {
			t.Fatalf("session %s: %v", id, err)
		}
		copy(row.AccessDigest[:], access)
		copy(row.RefreshDigest[:], refresh)
	})
	return row, reason
}

func count(t *testing.T, f *authoritytest.Fixture, query string, args ...any) int {
	t.Helper()
	var n int
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		if err := tx.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	})
	return n
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := authority.CodeOf(err); got != code {
		t.Fatalf("err = %v (code %q), want %q", err, got, code)
	}
}

func principalOf(f *authoritytest.Fixture, t *testing.T, sessionID string) access.Principal {
	row, _ := sessionRow(t, f, sessionID)
	return access.Principal{AccountID: row.AccountID, DeviceID: row.DeviceID, SessionID: row.ID, RecoveryEpoch: row.RecoveryEpoch,
		SecurityRevision: row.IssuedSeq, Scope: row.Scope, SessionScope: row.Scope, SessionFamilyID: row.FamilyID, AccessExpiresAt: row.AccessExpiresAt}
}

func newAttempt(t *testing.T) string { return authoritytest.NewID(t) }

// Owner decision D1 (2026-10-16): a routine rotation, legacy or recoverable,
// appends no audit row and leaves the account's chain head where it was. The
// consumed parent, the child row and a recoverable rotation's refresh
// receipt are its record, and the child is issued at the account's current
// security revision (the head every later account invalidation is compared
// with, realtime.Hub).
func TestRefreshAppendsNoAuditRow(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	parentID, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	before, _ := sessionRow(t, f, parentID)
	// Earlier security events (the sign-in's genesis row and one more): the
	// account's head is 2 when it rotates.
	appendRows(t, f, authority.ScopeAccount, account, 1)
	headSeq, headHash := chainHead(t, f, authority.ScopeAccount, account)
	unchanged := func(step string) {
		t.Helper()
		if rows := readAudit(t, f, authority.ScopeAccount, account); len(rows) != 2 {
			t.Fatalf("%s: %d account audit rows, want the 2 written before it", step, len(rows))
		}
		if seq, hash := chainHead(t, f, authority.ScopeAccount, account); seq != headSeq || hash != headHash {
			t.Fatalf("%s moved the account head %d -> %d", step, headSeq, seq)
		}
	}

	got, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "committed" || got.Code != "ok" || got.Revision != 2 || got.Recovered {
		t.Fatalf("result %+v", got.Result)
	}
	unchanged("a legacy rotation")
	parent, _ := sessionRow(t, f, parentID)
	child, _ := sessionRow(t, f, got.Child.ID)
	if parent.Status != "consumed" || parent.ChildID != child.ID || parent.ConsumedAttemptID != "" || parent.ConsumedAt == nil || !parent.ConsumedAt.Equal(f.Now) {
		t.Fatalf("parent after rotation %+v", parent)
	}
	if child.Status != "active" || child.ParentID != parentID || child.LineageRootID != before.LineageRootID || !child.DeadlineAt.Equal(before.DeadlineAt) ||
		!child.LineageStartedAt.Equal(before.LineageStartedAt) || child.IssuedSeq != 2 || got.Child.IssuedSeq != 2 || child.Scope != "account" ||
		child.TokenKeyID != f.TokenKeys.Current.ID || !child.AccessExpiresAt.Equal(f.Now.Add(10*time.Minute)) || !child.RefreshExpiresAt.Equal(f.Now.Add(30*24*time.Hour)) {
		t.Fatalf("child %+v", child)
	}
	if child.AccessDigest != authoritytest.TokenDigest(f.TokenKeys.Current, "access", got.AccessToken) ||
		child.RefreshDigest != authoritytest.TokenDigest(f.TokenKeys.Current, "refresh", got.RefreshToken) {
		t.Fatal("returned tokens do not match the child's digests")
	}
	if got.Account.ID != account || got.FamilySlot != nil || !got.EffectiveDeadline.Equal(before.DeadlineAt) {
		t.Fatalf("presentation %+v slot %+v deadline %v", got.Account, got.FamilySlot, got.EffectiveDeadline)
	}
	if n := count(t, f, `SELECT count(*) FROM command_receipts`); n != 0 {
		t.Fatalf("%d command receipts for a rotation, want 0 (the consumed parent is the receipt)", n)
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 0 {
		t.Fatalf("%d refresh receipts for a legacy rotation", n)
	}

	// Recoverable rotations append none either, and keep one receipt per
	// unresolved rotation: a successor rotation proves possession of its
	// predecessor's child.
	attempt1, attempt2 := newAttempt(t), newAttempt(t)
	second, err := f.Service.Refresh(t.Context(), refreshOf(f, got.RefreshToken, attempt1, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	unchanged("a recoverable rotation")
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts WHERE parent_id=$1 AND child_id=$2 AND attempt_id=$3`, child.ID, second.Child.ID, attempt1); n != 1 {
		t.Fatal("recoverable rotation wrote no receipt")
	}
	// Its lost-acknowledgement retry is served from the receipt and
	// appends nothing.
	retry, err := f.Service.Refresh(t.Context(), refreshOf(f, got.RefreshToken, attempt1, binding(account, device)))
	if err != nil || !retry.Recovered || retry.Child.ID != second.Child.ID || retry.Revision != second.Revision {
		t.Fatalf("lost-acknowledgement retry %+v (%v)", retry.Result, err)
	}
	unchanged("a lost-acknowledgement retry")
	third, err := f.Service.Refresh(t.Context(), refreshOf(f, second.RefreshToken, attempt2, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	unchanged("a successor rotation")
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 1 {
		t.Fatalf("%d receipts after the successor rotation, want 1", n)
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts WHERE child_id=$1`, third.Child.ID); n != 1 {
		t.Fatal("successor receipt missing")
	}
	if second.Revision != 2 || third.Revision != 2 || second.Child.IssuedSeq != 2 || third.Child.IssuedSeq != 2 {
		t.Fatalf("revisions %d %d, issued %d %d, want the unchanged head 2", second.Revision, third.Revision, second.Child.IssuedSeq, third.Child.IssuedSeq)
	}
	// A later security event moves the head; the next rotation's child is
	// issued at it.
	appendRows(t, f, authority.ScopeAccount, account, 1)
	fourth, err := f.Service.Refresh(t.Context(), refreshOf(f, third.RefreshToken, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Revision != 3 || fourth.Child.IssuedSeq != 3 || len(readAudit(t, f, authority.ScopeAccount, account)) != 3 {
		t.Fatalf("after a later event: revision %d, issued %d", fourth.Revision, fourth.Child.IssuedSeq)
	}
	// The chain still verifies end to end.
	verifier, _ := newChainVerifier(f)
	if report, err := verifier.VerifyAll(t.Context()); err != nil || len(report.Failed) != 0 {
		t.Fatalf("verify after rotations: %+v (%v)", report, err)
	}
}

// A failed account chain freezes the account (scope_failed), refresh included:
// with no audit row to append since D1, the rotation itself refuses it before
// any write. A legacy or recoverable rotation leaves the parent active, no
// child and no receipt; a lost-acknowledgement retry of a rotation committed
// before the chain failed is not served either.
func TestRefreshOnAFailedAccountChainIsScopeFailed(t *testing.T) {
	for _, recoverable := range []bool{false, true} {
		name := map[bool]string{false: "legacy", true: "recoverable"}[recoverable]
		t.Run(name, func(t *testing.T) {
			f := authoritytest.New(t)
			account := liveAccount(t, f)
			device := f.Device(t, account)
			parentID, _, rawRefresh := f.Session(t, account, device, "account", "", "")
			appendRows(t, f, authority.ScopeAccount, account, 1)
			f.Exec(t, `UPDATE accounts SET chain_failed=true WHERE id=$1`, account)
			in := refreshOf(f, rawRefresh, "", nil)
			if recoverable {
				in = refreshOf(f, rawRefresh, newAttempt(t), binding(account, device))
			}
			_, err := f.Service.Refresh(t.Context(), in)
			wantCode(t, err, "scope_failed")
			if parent, _ := sessionRow(t, f, parentID); parent.Status != "active" || parent.ChildID != "" {
				t.Fatalf("parent after a refused rotation %+v", parent)
			}
			if n := count(t, f, `SELECT count(*) FROM sessions WHERE parent_id=$1`, parentID); n != 0 {
				t.Fatalf("%d children of a refused rotation", n)
			}
			if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 0 {
				t.Fatalf("%d receipts for a refused rotation", n)
			}
		})
	}
	t.Run("lost-acknowledgement retry", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
		appendRows(t, f, authority.ScopeAccount, account, 1)
		attempt := newAttempt(t)
		first, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
		if err != nil {
			t.Fatal(err)
		}
		f.Exec(t, `UPDATE accounts SET chain_failed=true WHERE id=$1`, account)
		_, err = f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
		wantCode(t, err, "scope_failed")
		if child, _ := sessionRow(t, f, first.Child.ID); child.Status != "active" {
			t.Fatalf("the committed child became %s", child.Status)
		}
		if n := count(t, f, `SELECT count(*) FROM refresh_receipts WHERE child_id=$1`, first.Child.ID); n != 1 {
			t.Fatal("the refused retry changed the receipt")
		}
	})
}

// An account with no audit chain has no security revision to issue a child
// at: a rotation of a (hand-seeded) session on it fails closed, writing
// nothing. Issuing a session appends to the chain, so production never meets
// it.
func TestRefreshAtAnEmptyAccountChainFailsClosed(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	parentID, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	// Hand-empty the chain the fixture gave the account (the table owner may).
	f.Exec(t, `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1`, account)
	f.Exec(t, `UPDATE accounts SET audit_seq=0, audit_hash=$2 WHERE id=$1`, account, make([]byte, 32))
	_, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, "", nil))
	wantCode(t, err, "service_unavailable")
	if parent, _ := sessionRow(t, f, parentID); parent.Status != "active" {
		t.Fatalf("parent %s", parent.Status)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE parent_id=$1`, parentID); n != 0 {
		t.Fatalf("%d children", n)
	}
}

// Reuse detection keeps its audit row (D1): a legacy replay (refresh_reused)
// and a recoverable refresh under another attempt (refresh_attempt_conflict)
// each append exactly one session_revoked row, on top of a rotation that
// appended none, and move the account head to it.
func TestRefreshReuseStillAppendsAnAuditRow(t *testing.T) {
	for _, c := range []struct {
		name, code  string
		recoverable bool
	}{{"legacy replay", "refresh_reused", false}, {"recoverable attempt conflict", "refresh_attempt_conflict", true}} {
		t.Run(c.name, func(t *testing.T) {
			f := authoritytest.New(t)
			account := liveAccount(t, f)
			device := f.Device(t, account)
			_, _, rawRefresh := f.Session(t, account, device, "account", "", "") // the sign-in's row: head 1
			refresh := func() authority.Refresh {
				if c.recoverable {
					return refreshOf(f, rawRefresh, newAttempt(t), binding(account, device))
				}
				return refreshOf(f, rawRefresh, "", nil)
			}
			first, err := f.Service.Refresh(t.Context(), refresh())
			if err != nil {
				t.Fatal(err)
			}
			if rows := readAudit(t, f, authority.ScopeAccount, account); len(rows) != 1 {
				t.Fatalf("%d audit rows after a rotation, want the 1 before it", len(rows))
			}
			_, err = f.Service.Refresh(t.Context(), refresh())
			wantCode(t, err, c.code)
			rows := readAudit(t, f, authority.ScopeAccount, account)
			if len(rows) != 2 || rows[1].Kind != "session_revoked" || rows[1].Seq != 2 || recompute(t, rows[1]) != rows[1].Hash {
				t.Fatalf("audit rows after the reuse %+v, want one session_revoked appended", rows)
			}
			var payload map[string]any
			if err := json.Unmarshal(rows[1].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["session_id"] == nil || payload["lineage_root_session_id"] == nil {
				t.Fatalf("session_revoked payload %s", rows[1].Payload)
			}
			if child, reason := sessionRow(t, f, first.Child.ID); child.Status != "revoked" || reason != "refresh_reused" {
				t.Fatalf("child %s %s", child.Status, reason)
			}
			if head, hash := chainHead(t, f, authority.ScopeAccount, account); head != 2 || hash != rows[1].Hash {
				t.Fatalf("account head %d after the reuse, want 2", head)
			}
		})
	}
}

func TestRefreshLostAckRetryReturnsCommittedChild(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	attempt := newAttempt(t)
	first, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	sessions := count(t, f, `SELECT count(*) FROM sessions`)
	audit := len(readAudit(t, f, authority.ScopeAccount, account))

	f.Now = f.Now.Add(time.Minute)
	retry, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Recovered || retry.Child.ID != first.Child.ID || retry.AccessToken != first.AccessToken || retry.RefreshToken != first.RefreshToken ||
		retry.CommandID != first.CommandID || retry.Revision != first.Revision || !retry.EffectiveDeadline.Equal(first.EffectiveDeadline) {
		t.Fatalf("retry %+v, first %+v", retry, first)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions`); n != sessions {
		t.Fatalf("retry changed session count %d -> %d", sessions, n)
	}
	if n := len(readAudit(t, f, authority.ScopeAccount, account)); n != audit {
		t.Fatalf("retry wrote audit rows: %d -> %d", audit, n)
	}

	t.Run("wrong device proof never revokes", func(t *testing.T) {
		wrong := binding(account, device)
		wrong.CredentialDigest[0] ^= 0xff
		_, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, newAttempt(t), wrong))
		wantCode(t, err, "device_continuity_invalid")
		if child, _ := sessionRow(t, f, first.Child.ID); child.Status != "active" {
			t.Fatalf("child %s after invalid proof", child.Status)
		}
	})
	t.Run("a tampered receipt is not served", func(t *testing.T) {
		f.Exec(t, `UPDATE refresh_receipts SET material=$2, material_digest=sha256($2) WHERE child_id=$1`, first.Child.ID, []byte(`{}`))
		_, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
		wantCode(t, err, "service_unavailable")
	})
	t.Run("acknowledged receipt is unavailable, not reuse", func(t *testing.T) {
		f.Exec(t, `DELETE FROM refresh_receipts WHERE child_id=$1`, first.Child.ID)
		_, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
		wantCode(t, err, "refresh_receipt_unavailable")
		if child, _ := sessionRow(t, f, first.Child.ID); child.Status != "active" {
			t.Fatalf("child %s", child.Status)
		}
	})
}

func TestRefreshReuseRevokesLineageOnce(t *testing.T) {
	t.Run("recoverable different attempt", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		rootID, _, rawRefresh := f.Session(t, account, device, "account", "", "")
		otherID, _, _ := f.Session(t, account, device, "account", "", "") // another lineage on the same device
		first, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, newAttempt(t), binding(account, device)))
		if err != nil {
			t.Fatal(err)
		}
		second, err := f.Service.Refresh(t.Context(), refreshOf(f, first.RefreshToken, newAttempt(t), binding(account, device)))
		if err != nil {
			t.Fatal(err)
		}
		other := newAttempt(t)
		_, err = f.Service.Refresh(t.Context(), refreshOf(f, first.RefreshToken, other, binding(account, device)))
		wantCode(t, err, "refresh_attempt_conflict")
		for _, id := range []string{second.Child.ID} {
			if row, reason := sessionRow(t, f, id); row.Status != "revoked" || reason != "refresh_reused" || row.RevokedAt == nil {
				t.Fatalf("lineage session %s: %s %s", id, row.Status, reason)
			}
		}
		if n := count(t, f, `SELECT count(*) FROM sessions WHERE lineage_root_id=$1 AND status='active'`, rootID); n != 0 {
			t.Fatalf("%d active lineage sessions remain", n)
		}
		if row, _ := sessionRow(t, f, otherID); row.Status != "active" {
			t.Fatal("another lineage was revoked")
		}
		if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 0 {
			t.Fatalf("%d receipts survive the lineage revocation", n)
		}
		revoked := func() int {
			n := 0
			for _, row := range readAudit(t, f, authority.ScopeAccount, account) {
				if row.Kind == "session_revoked" {
					n++
				}
			}
			return n
		}
		if n := revoked(); n != 1 {
			t.Fatalf("%d session_revoked rows, want 1", n)
		}
		_, err = f.Service.Refresh(t.Context(), refreshOf(f, first.RefreshToken, other, binding(account, device)))
		wantCode(t, err, "session_revoked")
		_, err = f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, "", nil))
		wantCode(t, err, "session_revoked")
		if n := revoked(); n != 1 {
			t.Fatalf("%d session_revoked rows after retries, want 1", n)
		}
	})
	t.Run("legacy replay", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
		first, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, "", nil))
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, "", nil))
		wantCode(t, err, "refresh_reused")
		if row, reason := sessionRow(t, f, first.Child.ID); row.Status != "revoked" || reason != "refresh_reused" {
			t.Fatalf("child %s %s", row.Status, reason)
		}
		_, err = f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, "", nil))
		wantCode(t, err, "session_revoked")
		_, err = f.Service.Refresh(t.Context(), refreshOf(f, first.RefreshToken, "", nil))
		wantCode(t, err, "session_revoked")
		// The sign-in's row, then the reuse's: the rotation appended none (D1).
		if rows := readAudit(t, f, authority.ScopeAccount, account); len(rows) != 2 || rows[1].Kind != "session_revoked" {
			t.Fatalf("audit %+v", rows)
		}
	})
	t.Run("legacy replay of a recoverable rotation", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
		if _, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, newAttempt(t), binding(account, device))); err != nil {
			t.Fatal(err)
		}
		_, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, "", nil))
		wantCode(t, err, "refresh_reused")
	})
}

func TestRefreshLocksOnlyParentRow(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	family := f.Family(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "family", family, "owner")

	holder, err := f.Service.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM families WHERE id=$1 FOR UPDATE`, family); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM memberships WHERE family_id=$1 FOR UPDATE`, family); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	got, err := f.Service.Refresh(ctx, refreshOf(f, rawRefresh, newAttempt(t), binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("refresh waited %v behind a family lock", elapsed)
	}
	if got.Child.Scope != "family" || got.Child.FamilyID != family || got.Child.Role != "owner" {
		t.Fatalf("child %+v", got.Child)
	}
}

func TestRefreshAtDeadlineRequiresReauthentication(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		id, _, rawRefresh := f.Session(t, account, device, "account", "", "")
		row, _ := sessionRow(t, f, id)
		f.Exec(t, `UPDATE sessions SET refresh_expires_at=deadline_at WHERE id=$1`, id)
		in := refreshOf(f, rawRefresh, "", nil)
		in.At = row.DeadlineAt
		_, err := f.Service.Refresh(t.Context(), in)
		wantCode(t, err, "reauthentication_required")
		in.At = row.DeadlineAt.Add(-time.Microsecond)
		got, err := f.Service.Refresh(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Child.RefreshExpiresAt.Equal(row.DeadlineAt) || !got.Child.AccessExpiresAt.Equal(row.DeadlineAt) {
			t.Fatalf("child expiries %v %v not capped at the deadline %v", got.Child.AccessExpiresAt, got.Child.RefreshExpiresAt, row.DeadlineAt)
		}
	})
	t.Run("refresh expiry", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		id, _, rawRefresh := f.Session(t, account, device, "account", "", "")
		row, _ := sessionRow(t, f, id)
		in := refreshOf(f, rawRefresh, "", nil)
		in.At = row.RefreshExpiresAt
		_, err := f.Service.Refresh(t.Context(), in)
		wantCode(t, err, "reauthentication_required")
	})
	t.Run("self-hosted lineage year", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		id, _, rawRefresh := f.Session(t, account, device, "account", "", "")
		// Lineage started 365 days ago; deadline_at itself is later.
		f.Exec(t, `UPDATE sessions SET lineage_started_at=$2, deadline_at=$3, refresh_expires_at=$3 WHERE id=$1`, id,
			f.Now.Add(-365*24*time.Hour), f.Now.Add(30*24*time.Hour))
		in := refreshOf(f, rawRefresh, "", nil)
		_, err := f.Service.Refresh(t.Context(), in)
		wantCode(t, err, "reauthentication_required")
		in.Mode = authority.ModeHosted
		if _, err := f.Service.Refresh(t.Context(), in); err != nil {
			t.Fatalf("hosted refresh within deadline_at: %v", err)
		}
	})
}

func TestRefreshUpgradesScopeFromSlot(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	family := f.Family(t, account)
	got, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Child.Scope != "family" || got.Child.FamilyID != family || got.Child.Role != "owner" || got.FamilySlot == nil ||
		*got.FamilySlot != (authority.FamilySlot{FamilyID: family, State: "active"}) {
		t.Fatalf("child %+v slot %+v", got.Child, got.FamilySlot)
	}
	// Deletion scheduled: the owner's slot becomes family_recovery and the
	// membership former. A family session no longer matching the slot is
	// refused by a legacy refresh as by a recoverable one (ScheduleDeletion
	// itself revokes it: only this fixture leaves it active).
	f.Exec(t, `UPDATE family_slots SET state='family_recovery' WHERE account_id=$1`, account)
	f.Exec(t, `UPDATE memberships SET status='former' WHERE account_id=$1`, account)
	f.Exec(t, `UPDATE families SET status='deletion_pending', purge_after=$2 WHERE id=$1`, family, f.Now.Add(30*24*time.Hour))
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, got.RefreshToken, "", nil))
	wantCode(t, err, "session_revoked")
	// Only a refresh grants the recovery scope to an account session.
	_, _, rawAccount := f.Session(t, account, device, "account", "", "")
	recovered, err := f.Service.Refresh(t.Context(), refreshOf(f, rawAccount, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Child.Scope != "family_recovery" || recovered.Child.FamilyID != family || recovered.Child.Role != "owner" {
		t.Fatalf("child %+v", recovered.Child)
	}
	// A recoverable refresh whose parent's family scope no longer matches the
	// slot is refused (refresh_recovery.go:154-161).
	f.Exec(t, `UPDATE family_slots SET state='active' WHERE account_id=$1`, account)
	f.Exec(t, `UPDATE memberships SET status='active' WHERE account_id=$1`, account)
	f.Exec(t, `UPDATE families SET status='active', purge_after=NULL WHERE id=$1`, family)
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, recovered.RefreshToken, newAttempt(t), binding(account, device)))
	wantCode(t, err, "session_revoked")

	t.Run("hosted recoverable refresh requires the family entitlement", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		family := f.Family(t, account)
		_, _, rawRefresh := f.Session(t, account, device, "family", family, "owner")
		in := refreshOf(f, rawRefresh, newAttempt(t), binding(account, device))
		in.Mode = authority.ModeHosted
		_, err := f.Service.Refresh(t.Context(), in)
		wantCode(t, err, "session_revoked")
		f.Exec(t, `INSERT INTO family_entitlements(family_id,entitlement_key,source,status,created_at,updated_at) VALUES ($1,'family_sync','test','active',$2,$2)`, family, f.Now)
		if _, err := f.Service.Refresh(t.Context(), in); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLogoutRevokesEveryDeviceSession(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	other := f.Device(t, account)
	first, _, _ := f.Session(t, account, device, "account", "", "")
	second, _, rawSecond := f.Session(t, account, device, "account", "", "")
	kept, _, _ := f.Session(t, account, other, "account", "", "")
	if _, err := f.Service.Refresh(t.Context(), refreshOf(f, rawSecond, newAttempt(t), binding(account, device))); err != nil {
		t.Fatal(err)
	}
	p := principalOf(f, t, first)
	command := authoritytest.NewID(t)
	result, err := f.Service.Logout(t.Context(), p, command, f.Now)
	if err != nil {
		t.Fatal(err)
	}
	if result.CommandID != command || result.Outcome != "committed" || result.Revision != 2 {
		t.Fatalf("result %+v", result)
	}
	if n := count(t, f, `SELECT count(*) FROM devices WHERE id=$1 AND status='revoked' AND revoked_at=$2`, device, f.Now); n != 1 {
		t.Fatal("device not revoked")
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE device_id=$1 AND status='active'`, device); n != 0 {
		t.Fatalf("%d device sessions remain active", n)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE device_id=$1 AND status='revoked' AND revoke_reason='device_revoked'`, device); n != 2 {
		t.Fatalf("%d sessions revoked as device_revoked, want 2 (the rotated parent stays consumed)", n)
	}
	if row, _ := sessionRow(t, f, second); row.Status != "consumed" {
		t.Fatalf("consumed parent became %s", row.Status)
	}
	if row, _ := sessionRow(t, f, kept); row.Status != "active" {
		t.Fatal("another device's session was revoked")
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 0 {
		t.Fatalf("%d receipts survive the device revocation", n)
	}
	rows := readAudit(t, f, authority.ScopeAccount, account)
	if len(rows) != 2 || rows[1].Kind != "device_binding_revoked" || rows[1].CommandID != command { // the sign-in's row, then the logout's: the rotation appended none (D1)
		t.Fatalf("audit %+v", rows)
	}
	retry, err := f.Service.Logout(t.Context(), p, command, f.Now.Add(time.Minute))
	if err != nil || retry != result {
		t.Fatalf("exact retry %+v %v, want %+v", retry, err, result)
	}
	if n := len(readAudit(t, f, authority.ScopeAccount, account)); n != 2 {
		t.Fatalf("retry wrote audit: %d rows", n)
	}
	_, err = f.Service.Logout(t.Context(), p, authoritytest.NewID(t), f.Now)
	wantCode(t, err, "invalid_command")
	wrong := principalOf(f, t, kept)
	wrong.SecurityRevision = 7
	_, err = f.Service.Logout(t.Context(), wrong, authoritytest.NewID(t), f.Now)
	wantCode(t, err, "session_revoked")
	_, err = f.Service.Logout(t.Context(), principalOf(f, t, kept), "not-a-uuid", f.Now)
	wantCode(t, err, "invalid_request")
}

func TestRefreshAcrossTokenKeyRotation(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	parentID, _, rawRefresh := f.Session(t, account, device, "account", "", "") // under session.v1
	next := identity.DigestKey{ID: "session.v2"}
	for i := range next.Secret {
		next.Secret[i] = 0x33
	}
	keys := identity.DigestKeyring{Current: next, Previous: []identity.DigestKey{f.TokenKeys.Current}}
	rotated, err := authority.New(authority.Options{Pool: f.Pool, Now: func() time.Time { return f.Now }, InstanceID: f.InstanceID,
		RecoveryEpoch: authoritytest.RecoveryEpoch, TokenKeys: keys, Allowed: authoritytest.Allowed})
	if err != nil {
		t.Fatal(err)
	}
	in := refreshOf(f, rawRefresh, newAttempt(t), binding(account, device))
	in.ParentRefreshDigestCandidates = candidates(keys, rawRefresh)
	got, err := rotated.Refresh(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Parent.ID != parentID || got.Child.TokenKeyID != "session.v2" || got.Child.RefreshDigest != authoritytest.TokenDigest(next, "refresh", got.RefreshToken) {
		t.Fatalf("child %+v", got.Child)
	}
	// The lost-ack retry still finds the parent under the previous key.
	retry, err := rotated.Refresh(t.Context(), in)
	if err != nil || !retry.Recovered || retry.AccessToken != got.AccessToken {
		t.Fatalf("retry %+v %v", retry, err)
	}
	// A candidate under an unconfigured key matches nothing.
	in.ParentRefreshDigestCandidates = []authority.KeyedDigest{{KeyID: "session.v9", Sum: authoritytest.TokenDigest(f.TokenKeys.Current, "refresh", got.RefreshToken)}}
	_, err = rotated.Refresh(t.Context(), in)
	wantCode(t, err, "session_revoked")
}

func TestRefreshEpochAndUnknownToken(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	id, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	_, err := f.Service.Refresh(t.Context(), refreshOf(f, "unknown-token", "", nil))
	wantCode(t, err, "session_revoked")
	f.Exec(t, `UPDATE sessions SET recovery_epoch=2 WHERE id=$1`, id)
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, "", nil))
	wantCode(t, err, "epoch_changed")
	f.Exec(t, `UPDATE sessions SET recovery_epoch=1 WHERE id=$1`, id)
	f.Exec(t, `UPDATE accounts SET status='identity_unavailable' WHERE id=$1`, account)
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, "", nil))
	wantCode(t, err, "session_revoked")
	subject, _, err := f.Service.ResolveRefreshSubject(t.Context(), candidates(f.TokenKeys, rawRefresh))
	wantCode(t, err, "session_revoked")
	f.Exec(t, `UPDATE accounts SET status='active' WHERE id=$1`, account)
	subject, subjectDevice, err := f.Service.ResolveRefreshSubject(t.Context(), candidates(f.TokenKeys, rawRefresh))
	if err != nil || subject != account || subjectDevice != device {
		t.Fatalf("subject %q %q %v", subject, subjectDevice, err)
	}
	_, err = f.Service.Refresh(t.Context(), authority.Refresh{ParentRefreshDigestCandidates: candidates(f.TokenKeys, rawRefresh), At: f.Now})
	wantCode(t, err, "invalid_command")
}

func TestAcknowledgeRefreshDeletesOnlyTheBoundReceipt(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	attempt := newAttempt(t)
	got, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	p := principalOf(f, t, got.Child.ID)
	wrong := *binding(account, device)
	wrong.CredentialDigest[1] ^= 1
	wantCode(t, f.Service.AcknowledgeRefresh(t.Context(), p, attempt, wrong), "device_continuity_invalid")
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 1 {
		t.Fatal("invalid proof deleted the receipt")
	}
	if err := f.Service.AcknowledgeRefresh(t.Context(), p, newAttempt(t), *binding(account, device)); err != nil {
		t.Fatalf("absent receipt: %v", err)
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 1 {
		t.Fatal("another attempt's acknowledgement deleted the receipt")
	}
	if err := f.Service.AcknowledgeRefresh(t.Context(), p, attempt, *binding(account, device)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 0 {
		t.Fatal("acknowledgement kept the receipt")
	}
	wantCode(t, f.Service.AcknowledgeRefresh(t.Context(), p, "bad", *binding(account, device)), "invalid_request")
	expired := p
	expired.AccessExpiresAt = f.Now
	wantCode(t, f.Service.AcknowledgeRefresh(t.Context(), expired, attempt, *binding(account, device)), "access_expired")
}

// Refreshes of several devices of one account race Logout and
// MarkIdentityUnavailable on that account; the global lock order (accounts,
// then families, then sessions) means none of them deadlocks.
func TestRefreshConcurrentWithLogoutAndIdentityUnavailableNeverDeadlocks(t *testing.T) {
	f := authoritytest.New(t)
	const rounds, devices = 6, 4
	for round := range rounds {
		account := f.Account(t)
		identityKey := seedIdentity(t, f, account, 1)
		type lane struct{ device, refresh, session string }
		lanes := make([]lane, devices)
		for i := range lanes {
			lanes[i].device = f.Device(t, account)
			lanes[i].session, _, lanes[i].refresh = f.Session(t, account, lanes[i].device, "account", "", "")
		}
		logoutPrincipal := principalOf(f, t, lanes[0].session)
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for i := 1; i < devices; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				raw := lanes[i].refresh
				for range 4 {
					attempt := ""
					var continuity *authority.DeviceBinding
					if i%2 == 0 {
						attempt, continuity = authoritytest.NewID(t), binding(account, lanes[i].device)
					}
					got, err := f.Service.Refresh(context.Background(), refreshOf(f, raw, attempt, continuity))
					if err != nil {
						errs <- fmt.Errorf("refresh: %w", err)
						return
					}
					raw = got.RefreshToken
				}
			}()
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := f.Service.Logout(context.Background(), logoutPrincipal, authoritytest.NewID(t), f.Now); err != nil {
				errs <- fmt.Errorf("logout: %w", err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if round%2 == 0 {
				time.Sleep(time.Millisecond)
			}
			if _, err := f.Service.MarkIdentityUnavailable(context.Background(), authority.IdentityUnavailable{CommandID: authoritytest.NewID(t),
				AccountID: account, Candidates: []string{identityKey}, At: f.Now}); err != nil {
				errs <- fmt.Errorf("identity unavailable: %w", err)
			}
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if strings.Contains(err.Error(), "40P01") || strings.Contains(err.Error(), "deadlock") || authority.CodeOf(err) == "service_unavailable" {
				t.Fatalf("round %d: %v", round, err)
			}
			// Refreshes that lose the race to the revocation are refused.
			if code := authority.CodeOf(err); code != "session_revoked" {
				t.Errorf("round %d: unexpected %v", round, err)
			}
		}
		if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND status='active'`, account); n != 0 {
			t.Fatalf("round %d: %d sessions active after identity_unavailable", round, n)
		}
		assertContiguous(t, readAudit(t, f, authority.ScopeAccount, account), uint64(len(readAudit(t, f, authority.ScopeAccount, account))))
	}
}

// liveAccount is an active account with a live identity, as a sign-in leaves it.
func liveAccount(t *testing.T, f *authoritytest.Fixture) string {
	t.Helper()
	account := f.Account(t)
	seedIdentity(t, f, account, 1)
	return account
}

// The material is persisted in refresh_receipts: a naive encoder of the
// whole stage must not be able to store the token key (fix round 1).
func TestRefreshMaterialNeverSeesKeySecrets(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	var seen [][32]byte
	naive := func(stage authority.RefreshStage) ([]byte, [32]byte, error) {
		seen = append(seen, stage.Input.ParentKey.Secret)
		encoded, err := json.Marshal(stage)
		return encoded, sha256.Sum256(encoded), err
	}
	in := refreshOf(f, rawRefresh, newAttempt(t), binding(account, device))
	in.Material = naive
	got, err := f.Service.Refresh(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if retry, err := f.Service.Refresh(t.Context(), in); err != nil || !retry.Recovered || retry.AccessToken != got.AccessToken {
		t.Fatalf("lost-ack retry with a zeroed secret: %+v %v", retry, err)
	}
	for i, secret := range seen {
		if secret != ([32]byte{}) {
			t.Fatalf("Material call %d received a key secret", i)
		}
	}
	var material []byte
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		if err := tx.QueryRow(t.Context(), `SELECT material FROM refresh_receipts WHERE child_id=$1`, got.Child.ID).Scan(&material); err != nil {
			t.Fatal(err)
		}
	})
	secret := f.TokenKeys.Current.Secret
	secretJSON, _ := json.Marshal(secret)
	if bytes.Contains(material, secret[:]) || bytes.Contains(material, secretJSON) || bytes.Contains(material, []byte(base64.StdEncoding.EncodeToString(secret[:]))) {
		t.Fatalf("stored material contains the token key: %s", material)
	}
}

// A legacy rotation of a recoverable child also retires the predecessor's
// receipt (000025:718; fix round 1).
func TestLegacyRotationRetiresPredecessorReceipt(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	first, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, newAttempt(t), binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts WHERE child_id=$1`, first.Child.ID); n != 1 {
		t.Fatal("no receipt")
	}
	if _, err := f.Service.Refresh(t.Context(), refreshOf(f, first.RefreshToken, "", nil)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 0 {
		t.Fatalf("%d receipts survive the legacy successor rotation", n)
	}
}

// Self-hosted lineages last at most a year from sign-in: SignIn refuses a
// later deadline and AcknowledgeRefresh applies the same cap as Refresh
// (fix round 1).
func TestSelfHostedLineageYearCapsSignInAndAcknowledgement(t *testing.T) {
	f := authoritytest.New(t)
	key := identityKey(t, "identity.v1")
	accountID := authoritytest.NewID(t)
	late := newSignIn(t, f, []string{key}, accountID, 1)
	late.in.DeadlineAt = f.Now.Add(365*24*time.Hour + time.Microsecond)
	_, err := f.Service.SignIn(t.Context(), late.in)
	wantCode(t, err, "invalid_command")
	late.in.Mode = ""
	_, err = f.Service.SignIn(t.Context(), late.in)
	wantCode(t, err, "invalid_command")
	late.in.Mode = authority.ModeHosted
	if _, err := f.Service.SignIn(t.Context(), late.in); err != nil {
		t.Fatalf("hosted sign-in: %v", err)
	}

	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawRefresh := f.Session(t, account, device, "account", "", "")
	attempt := newAttempt(t)
	got, err := f.Service.Refresh(t.Context(), refreshOf(f, rawRefresh, attempt, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	// deadline_at is still in the future, but the lineage began a year ago.
	f.Exec(t, `UPDATE sessions SET lineage_started_at=$2 WHERE id=$1`, got.Child.ID, f.Now.Add(-365*24*time.Hour))
	wantCode(t, f.Service.AcknowledgeRefresh(t.Context(), principalOf(f, t, got.Child.ID), attempt, *binding(account, device)), "reauthentication_required")
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 1 {
		t.Fatal("a refused acknowledgement deleted the receipt")
	}
}

// Two refreshes of one parent race: exactly one child, and the loser gets
// the code its kind of retry defines (fix round 1).
func TestRefreshConcurrentSameParentIssuesOneChild(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	cases := []struct {
		name      string
		attempts  func() (string, string)
		wantLoser string // "" = recovered
	}{
		{"legacy twice", func() (string, string) { return "", "" }, "refresh_reused"},
		{"same attempt", func() (string, string) { a := newAttempt(t); return a, a }, ""},
		{"different attempts", func() (string, string) { return newAttempt(t), newAttempt(t) }, "refresh_attempt_conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for range 5 {
				parentID, _, rawRefresh := f.Session(t, account, device, "account", "", "")
				first, second := tc.attempts()
				ins := []authority.Refresh{refreshOf(f, rawRefresh, first, nil), refreshOf(f, rawRefresh, second, nil)}
				for i, attempt := range []string{first, second} {
					if attempt != "" {
						ins[i].Continuity = binding(account, device)
					}
				}
				type outcome struct {
					result authority.RefreshResult
					err    error
				}
				start := make(chan struct{})
				results := make(chan outcome, 2)
				for _, in := range ins {
					go func() {
						<-start
						result, err := f.Service.Refresh(context.Background(), in)
						results <- outcome{result, err}
					}()
				}
				close(start)
				a, b := <-results, <-results
				if a.err != nil || (tc.wantLoser == "" && a.result.Recovered) {
					a, b = b, a // a is the rotation
				}
				if a.err != nil || a.result.Recovered {
					t.Fatalf("no fresh rotation: %+v %v / %+v %v", a.result.Result, a.err, b.result.Result, b.err)
				}
				if tc.wantLoser == "" {
					if b.err != nil || !b.result.Recovered || b.result.Child.ID != a.result.Child.ID || b.result.AccessToken != a.result.AccessToken {
						t.Fatalf("loser %+v %v, want the recovered child %s", b.result.Result, b.err, a.result.Child.ID)
					}
				} else {
					wantCode(t, b.err, tc.wantLoser)
				}
				if n := count(t, f, `SELECT count(*) FROM sessions WHERE parent_id=$1`, parentID); n != 1 {
					t.Fatalf("%d children of one parent", n)
				}
			}
		})
	}
}

// frozenScope is the child scope, family and role a recovery receipt froze
// at the rotation ("" for NULL).
func frozenScope(t *testing.T, f *authoritytest.Fixture, childID string) (scope, family, role string) {
	t.Helper()
	inAuthorityTxQuery(t, f, `SELECT coalesce(child_scope::text,''), coalesce(child_family_id::text,''), coalesce(child_role::text,'')
		FROM refresh_receipts WHERE child_id=$1`, []any{childID}, &scope, &family, &role)
	return scope, family, role
}

// A lost-ack retry re-derives the committed child from the scope frozen at
// the rotation, not from the row ForfeitRecovery rescoped; it returns the
// child exactly once and with its current scope. Another attempt revokes the
// lineage whether or not the receipt restores.
func TestRecoveryRetryAfterRescopeReturnsTheFrozenChild(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	device := f.Device(t, owner.account)
	_, _, raw := f.Session(t, owner.account, device, "account", "", "")
	attempt := newAttempt(t)
	first, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, attempt, binding(owner.account, device)))
	if err != nil || first.Child.Scope != "family_recovery" {
		t.Fatalf("rotation into the recovery scope: %+v %v", first.Child, err)
	}
	if scope, frozenFamily, role := frozenScope(t, f, first.Child.ID); scope != "family_recovery" || frozenFamily != family || role != "owner" {
		t.Fatalf("receipt froze (%q, %q, %q), want (family_recovery, %s, owner)", scope, frozenFamily, role, family)
	}
	// The acknowledgement is lost; the owner forfeits recovery on another device, which moves the child to account scope.
	if _, err := f.Service.ForfeitRecovery(t.Context(), authority.ForfeitRecovery{CommandID: authoritytest.NewID(t), AccountID: owner.account,
		At: f.Now, Principal: principalOf(f, t, first.Child.ID)}); err != nil {
		t.Fatal(err)
	}
	if row, _ := sessionRow(t, f, first.Child.ID); row.Scope != "account" {
		t.Fatalf("child scope %s after forfeit", row.Scope)
	}
	sessions := count(t, f, `SELECT count(*) FROM sessions`)
	audit := len(readAudit(t, f, authority.ScopeAccount, owner.account))
	retry, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, attempt, binding(owner.account, device)))
	if err != nil || !retry.Recovered || retry.AccessToken != first.AccessToken || retry.RefreshToken != first.RefreshToken {
		t.Fatalf("the exact retry after a rescope: %+v %v (want the committed child, not service_unavailable)", retry, err)
	}
	if retry.Child.ID != first.Child.ID || retry.CommandID != first.CommandID || retry.Revision != first.Revision {
		t.Fatalf("retry %+v, first %+v", retry.Result, first.Result)
	}
	// The client learns the child's current scope.
	if retry.Child.Scope != "account" || retry.Child.FamilyID != "" || retry.Child.Role != "" || retry.FamilySlot != nil {
		t.Fatalf("recovered child %+v slot %+v, want the current account scope", retry.Child, retry.FamilySlot)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions`); n != sessions {
		t.Fatalf("the retry changed the session count %d -> %d", sessions, n)
	}
	if n := len(readAudit(t, f, authority.ScopeAccount, owner.account)); n != audit {
		t.Fatalf("the retry wrote audit rows: %d -> %d", audit, n)
	}
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, raw, newAttempt(t), binding(owner.account, device)))
	wantCode(t, err, "refresh_attempt_conflict")
	if row, _ := sessionRow(t, f, first.Child.ID); row.Status != "revoked" {
		t.Fatalf("a different attempt left the lineage %s", row.Status)
	}
}

// The frozen scope also survives RecoverFamily (family_recovery to family)
// and a role change (caregiver to viewer); the result carries the row's
// current scope and role.
func TestRecoveryRetryAfterRecoverAndRoleChangeReturnsTheFrozenChild(t *testing.T) {
	t.Run("recover", func(t *testing.T) {
		f := authoritytest.New(t)
		owner := newMember(t, f)
		family := createFamily(t, f, f.Service, owner)
		if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
			t.Fatal(err)
		}
		device := f.Device(t, owner.account)
		_, _, raw := f.Session(t, owner.account, device, "account", "", "")
		attempt := newAttempt(t)
		first, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, attempt, binding(owner.account, device)))
		if err != nil || first.Child.Scope != "family_recovery" {
			t.Fatalf("rotation into the recovery scope: %+v %v", first.Child, err)
		}
		if _, err := f.Service.RecoverFamily(t.Context(), authority.RecoverFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account,
			At: f.Now, Principal: principalOf(f, t, first.Child.ID)}); err != nil {
			t.Fatal(err)
		}
		retry, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, attempt, binding(owner.account, device)))
		if err != nil || !retry.Recovered || retry.AccessToken != first.AccessToken || retry.RefreshToken != first.RefreshToken {
			t.Fatalf("the exact retry after recovery: %+v %v", retry.Result, err)
		}
		if retry.Child.Scope != "family" || retry.Child.FamilyID != family || retry.Child.Role != "owner" ||
			retry.FamilySlot == nil || retry.FamilySlot.State != "active" || retry.FamilySlot.FamilyID != family {
			t.Fatalf("recovered child %+v slot %+v, want the active family", retry.Child, retry.FamilySlot)
		}
	})
	t.Run("role change", func(t *testing.T) {
		f := authoritytest.New(t)
		owner := newMember(t, f)
		family := createFamily(t, f, f.Service, owner)
		caregiver := joined(t, f, f.Service, owner, family, "caregiver")
		attempt := newAttempt(t)
		first, err := f.Service.Refresh(t.Context(), refreshOf(f, caregiver.refresh, attempt, binding(caregiver.account, caregiver.device)))
		if err != nil || first.Child.Scope != "family" || first.Child.Role != "caregiver" {
			t.Fatalf("rotation into the family: %+v %v", first.Child, err)
		}
		if scope, frozenFamily, role := frozenScope(t, f, first.Child.ID); scope != "family" || frozenFamily != family || role != "caregiver" {
			t.Fatalf("receipt froze (%q, %q, %q), want (family, %s, caregiver)", scope, frozenFamily, role, family)
		}
		if _, err := f.Service.ChangeRole(t.Context(), authority.ChangeRole{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account,
			AccountID: caregiver.account, Role: "viewer", At: f.Now, Principal: owner.principal(t, f)}); err != nil {
			t.Fatal(err)
		}
		retry, err := f.Service.Refresh(t.Context(), refreshOf(f, caregiver.refresh, attempt, binding(caregiver.account, caregiver.device)))
		if err != nil || !retry.Recovered || retry.AccessToken != first.AccessToken || retry.RefreshToken != first.RefreshToken {
			t.Fatalf("the exact retry after a role change: %+v %v", retry.Result, err)
		}
		if retry.Child.Scope != "family" || retry.Child.Role != "viewer" {
			t.Fatalf("recovered child %+v, want the current viewer role", retry.Child)
		}
		_, err = f.Service.Refresh(t.Context(), refreshOf(f, caregiver.refresh, newAttempt(t), binding(caregiver.account, caregiver.device)))
		wantCode(t, err, "refresh_attempt_conflict")
		if row, reason := sessionRow(t, f, first.Child.ID); row.Status != "revoked" || reason != "refresh_reused" {
			t.Fatalf("a different attempt left the lineage %s %s", row.Status, reason)
		}
	})
}

// A receipt written before 000031 froze no scope: the exact retry cannot be
// re-derived and is refresh_receipt_unavailable (terminal for the client),
// never service_unavailable. Another attempt still revokes the lineage.
func TestRecoveryReceiptWrittenBeforeThirtyOneIsUnavailable(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	attempt := newAttempt(t)
	first, err := f.Service.Refresh(t.Context(), refreshOf(f, owner.refresh, attempt, binding(owner.account, owner.device)))
	if err != nil || first.Child.Scope != "family" {
		t.Fatalf("rotation into the family: %+v %v", first.Child, err)
	}
	if scope, frozenFamily, role := frozenScope(t, f, first.Child.ID); scope != "family" || frozenFamily != family || role != "owner" {
		t.Fatalf("receipt froze (%q, %q, %q), want (family, %s, owner)", scope, frozenFamily, role, family)
	}
	f.Exec(t, `UPDATE refresh_receipts SET child_scope=NULL, child_family_id=NULL, child_role=NULL WHERE child_id=$1`, first.Child.ID)
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, owner.refresh, attempt, binding(owner.account, owner.device)))
	wantCode(t, err, "refresh_receipt_unavailable")
	if row, _ := sessionRow(t, f, first.Child.ID); row.Status != "active" {
		t.Fatalf("an unavailable receipt left the child %s", row.Status)
	}
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, owner.refresh, newAttempt(t), binding(owner.account, owner.device)))
	wantCode(t, err, "refresh_attempt_conflict")
	if row, reason := sessionRow(t, f, first.Child.ID); row.Status != "revoked" || reason != "refresh_reused" {
		t.Fatalf("a different attempt left the lineage %s %s", row.Status, reason)
	}
}

// Another attempt is validated against the committed child first
// (validateRecoverySession): a child past its refresh expiry is
// reauthentication_required and nothing is revoked, as before 000031.
func TestRecoveryDifferentAttemptOnAnExpiredChildRequiresReauthentication(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, raw := f.Session(t, account, device, "account", "", "")
	first, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, newAttempt(t), binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	audit := len(readAudit(t, f, authority.ScopeAccount, account))
	f.Now = first.Child.RefreshExpiresAt
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, raw, newAttempt(t), binding(account, device)))
	wantCode(t, err, "reauthentication_required")
	if row, _ := sessionRow(t, f, first.Child.ID); row.Status != "active" {
		t.Fatalf("an expired child's lineage was revoked: %s", row.Status)
	}
	if n := len(readAudit(t, f, authority.ScopeAccount, account)); n != audit {
		t.Fatalf("the refusal wrote audit rows: %d -> %d", audit, n)
	}
}

// A reuse is a reuse whether or not the receipt restores: another attempt
// against a parent whose receipt no longer reproduces the child revokes the
// lineage; the exact attempt stays service_unavailable and revokes nothing.
func TestRecoveryDifferentAttemptRevokesWhenTheReceiptDoesNotRestore(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, raw := f.Session(t, account, device, "account", "", "")
	attempt := newAttempt(t)
	first, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, attempt, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	f.Exec(t, `UPDATE refresh_receipts SET material=$2, material_digest=sha256($2) WHERE child_id=$1`, first.Child.ID, []byte(`{}`))
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, raw, attempt, binding(account, device)))
	wantCode(t, err, "service_unavailable")
	if row, _ := sessionRow(t, f, first.Child.ID); row.Status != "active" {
		t.Fatalf("the exact attempt left the child %s", row.Status)
	}
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, raw, newAttempt(t), binding(account, device)))
	wantCode(t, err, "refresh_attempt_conflict")
	if row, reason := sessionRow(t, f, first.Child.ID); row.Status != "revoked" || reason != "refresh_reused" {
		t.Fatalf("a different attempt left the lineage %s %s", row.Status, reason)
	}
}

// entitledFamily is a hosted family of account with an active family_sync
// entitlement, and a family-scope owner session in it.
func entitledFamily(t *testing.T, f *authoritytest.Fixture, account, device string) (family, sessionID, raw string) {
	t.Helper()
	family = f.Family(t, account)
	f.Exec(t, `INSERT INTO family_entitlements(family_id,entitlement_key,source,status,created_at,updated_at) VALUES ($1,'family_sync','test','active',$2,$2)`, family, f.Now)
	sessionID, _, raw = f.Session(t, account, device, "family", family, "owner")
	return family, sessionID, raw
}

func hosted(in authority.Refresh) authority.Refresh {
	in.Mode = authority.ModeHosted
	return in
}

// lapse ends family's family_sync entitlement: "suspended" or "expired".
func lapse(t *testing.T, f *authoritytest.Fixture, family, how string) {
	t.Helper()
	switch how {
	case "suspended":
		f.Exec(t, `UPDATE family_entitlements SET status='suspended' WHERE family_id=$1`, family)
	case "expired":
		f.Exec(t, `UPDATE family_entitlements SET expires_at=$2 WHERE family_id=$1`, family, f.Now)
	}
}

// The exact retry validates the committed child's current row, not just the
// frozen derivation: a hosted child whose entitlement lapsed is
// session_revoked and a child past its refresh expiry is
// reauthentication_required, and neither is revoked (review m1).
func TestRecoveryExactRetryValidatesTheCurrentChild(t *testing.T) {
	t.Run("hosted lapse", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		family, _, raw := entitledFamily(t, f, account, device)
		in := hosted(refreshOf(f, raw, newAttempt(t), binding(account, device)))
		first, err := f.Service.Refresh(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		lapse(t, f, family, "suspended")
		_, err = f.Service.Refresh(t.Context(), in)
		wantCode(t, err, "session_revoked")
		if row, _ := sessionRow(t, f, first.Child.ID); row.Status != "active" {
			t.Fatalf("the exact retry left the child %s", row.Status)
		}
	})
	t.Run("expired", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		_, _, raw := f.Session(t, account, device, "account", "", "")
		attempt := newAttempt(t)
		first, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, attempt, binding(account, device)))
		if err != nil {
			t.Fatal(err)
		}
		f.Now = first.Child.RefreshExpiresAt
		_, err = f.Service.Refresh(t.Context(), refreshOf(f, raw, attempt, binding(account, device)))
		wantCode(t, err, "reauthentication_required")
	})
}

// A different attempt whose committed child fails only the scope check (a
// hosted family_sync entitlement lapsed) is still a reuse of a live
// lineage: it revokes the lineage (refresh_attempt_conflict, reason
// refresh_reused), so the child's token cannot rotate on (review m2).
func TestRecoveryDifferentAttemptAfterEntitlementLapseRevokesTheLineage(t *testing.T) {
	for _, how := range []string{"suspended", "expired"} {
		t.Run(how, func(t *testing.T) {
			f := authoritytest.New(t)
			account := liveAccount(t, f)
			device := f.Device(t, account)
			family, _, raw := entitledFamily(t, f, account, device)
			first, err := f.Service.Refresh(t.Context(), hosted(refreshOf(f, raw, newAttempt(t), binding(account, device))))
			if err != nil {
				t.Fatal(err)
			}
			lapse(t, f, family, how)
			audit := len(readAudit(t, f, authority.ScopeAccount, account))
			_, err = f.Service.Refresh(t.Context(), hosted(refreshOf(f, raw, newAttempt(t), binding(account, device))))
			wantCode(t, err, "refresh_attempt_conflict")
			if row, reason := sessionRow(t, f, first.Child.ID); row.Status != "revoked" || reason != "refresh_reused" {
				t.Fatalf("a different attempt left the lineage %s %s", row.Status, reason)
			}
			if n := len(readAudit(t, f, authority.ScopeAccount, account)); n != audit+1 {
				t.Fatalf("audit rows %d -> %d, want one revocation", audit, n)
			}
			_, err = f.Service.Refresh(t.Context(), hosted(refreshOf(f, first.RefreshToken, "", nil)))
			wantCode(t, err, "session_revoked")
		})
	}
}

// A different attempt on a revoked device is refused before anything else
// (the device proof) and revokes nothing: there is nothing left to protect.
func TestRecoveryDifferentAttemptOnARevokedDeviceRevokesNothing(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, raw := f.Session(t, account, device, "account", "", "")
	first, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, newAttempt(t), binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	f.Exec(t, `UPDATE devices SET status='revoked', revoked_at=$2 WHERE id=$1`, device, f.Now)
	audit := len(readAudit(t, f, authority.ScopeAccount, account))
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, raw, newAttempt(t), binding(account, device)))
	wantCode(t, err, "device_continuity_invalid")
	if row, _ := sessionRow(t, f, first.Child.ID); row.Status != "active" {
		t.Fatalf("a revoked device's refusal revoked the child: %s", row.Status)
	}
	if n := len(readAudit(t, f, authority.ScopeAccount, account)); n != audit {
		t.Fatalf("the refusal wrote audit rows: %d -> %d", audit, n)
	}
}

// A legacy (non-recoverable) rotation validates the parent's current scope
// as the recoverable one does: a hosted family session whose family_sync
// entitlement lapsed is session_revoked, and the parent is neither consumed
// nor revoked (review m2). An entitled family session still rotates.
func TestLegacyRefreshRefusesALapsedFamilyScope(t *testing.T) {
	for _, how := range []string{"entitled", "suspended", "expired"} {
		t.Run(how, func(t *testing.T) {
			f := authoritytest.New(t)
			account := liveAccount(t, f)
			device := f.Device(t, account)
			family, parent, raw := entitledFamily(t, f, account, device)
			lapse(t, f, family, how)
			audit := len(readAudit(t, f, authority.ScopeAccount, account))
			got, err := f.Service.Refresh(t.Context(), hosted(refreshOf(f, raw, "", nil)))
			if how == "entitled" {
				if err != nil || got.Child.Scope != "family" || got.Child.FamilyID != family {
					t.Fatalf("entitled rotation: %+v, %v", got.Child, err)
				}
				return
			}
			wantCode(t, err, "session_revoked")
			if row, _ := sessionRow(t, f, parent); row.Status != "active" {
				t.Fatalf("the refusal left the parent %s", row.Status)
			}
			if n := len(readAudit(t, f, authority.ScopeAccount, account)); n != audit {
				t.Fatalf("the refusal wrote audit rows: %d -> %d", audit, n)
			}
		})
	}
}

// Neither a legacy nor a recoverable rotation upgrades an account session
// into a hosted family whose family_sync entitlement lapsed: both answer
// session_revoked, the parent stays active and nothing is audited (re-review
// m1; before, the legacy path minted one family child the next rotation
// refused).
func TestRefreshDoesNotUpgradeIntoALapsedHostedFamily(t *testing.T) {
	for _, path := range []string{"legacy", "recoverable"} {
		t.Run(path, func(t *testing.T) {
			f := authoritytest.New(t)
			account := liveAccount(t, f)
			device := f.Device(t, account)
			family, _, _ := entitledFamily(t, f, account, device)
			lapse(t, f, family, "suspended")
			parent, _, raw := f.Session(t, account, device, "account", "", "")
			in := hosted(refreshOf(f, raw, "", nil))
			if path == "recoverable" {
				in = hosted(refreshOf(f, raw, newAttempt(t), binding(account, device)))
			}
			audit := len(readAudit(t, f, authority.ScopeAccount, account))
			got, err := f.Service.Refresh(t.Context(), in)
			if err == nil {
				t.Fatalf("upgraded into the lapsed family: child %+v", got.Child)
			}
			wantCode(t, err, "session_revoked")
			if row, _ := sessionRow(t, f, parent); row.Status != "active" {
				t.Fatalf("the refusal left the parent %s", row.Status)
			}
			if n := len(readAudit(t, f, authority.ScopeAccount, account)); n != audit {
				t.Fatalf("the refusal wrote audit rows: %d -> %d", audit, n)
			}
		})
	}
}

// Self-hosted family sessions need no entitlement row: they keep rotating
// through the legacy and the recoverable path, and a family_recovery session
// keeps rotating, self-hosted and hosted (re-review: no other test rotates a
// self-hosted family parent on the legacy path).
func TestSelfHostedFamilySessionsKeepRotating(t *testing.T) {
	chain := func(t *testing.T, recoverable bool) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		family := f.Family(t, account)
		_, _, raw := f.Session(t, account, device, "family", family, "owner")
		for i := 0; i < 3; i++ {
			in := refreshOf(f, raw, "", nil)
			if recoverable {
				in = refreshOf(f, raw, newAttempt(t), binding(account, device))
			}
			got, err := f.Service.Refresh(t.Context(), in)
			if err != nil {
				t.Fatalf("rotation %d: %v", i, err)
			}
			if got.Child.Scope != "family" || got.Child.FamilyID != family || got.Child.Role != "owner" {
				t.Fatalf("rotation %d child %+v", i, got.Child)
			}
			raw = got.RefreshToken
		}
	}
	t.Run("legacy family chain", func(t *testing.T) { chain(t, false) })
	t.Run("recoverable family chain", func(t *testing.T) { chain(t, true) })
	t.Run("family_recovery, self-hosted then hosted", func(t *testing.T) {
		f := authoritytest.New(t)
		account := liveAccount(t, f)
		device := f.Device(t, account)
		family := f.Family(t, account)
		f.Exec(t, `UPDATE family_slots SET state='family_recovery' WHERE account_id=$1`, account)
		f.Exec(t, `UPDATE memberships SET status='former' WHERE account_id=$1`, account)
		f.Exec(t, `UPDATE families SET status='deletion_pending', purge_after=$2 WHERE id=$1`, family, f.Now.Add(24*time.Hour))
		_, _, raw := f.Session(t, account, device, "family_recovery", family, "owner")
		got, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, "", nil))
		if err != nil || got.Child.Scope != "family_recovery" || got.Child.FamilyID != family {
			t.Fatalf("self-hosted child %+v, %v", got.Child, err)
		}
		again, err := f.Service.Refresh(t.Context(), hosted(refreshOf(f, got.RefreshToken, "", nil)))
		if err != nil || again.Child.Scope != "family_recovery" || again.Child.FamilyID != family {
			t.Fatalf("hosted child %+v, %v", again.Child, err)
		}
	})
}
