package authority_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
)

// The homelab's audit chains were written by the one-time legacy import of
// 2026-10-10 (docs/verification/2026-10-10-homelab-cutover.md), whose code
// was then deleted (Phase 4 Task 29). Each of its account and family chains
// starts at genesis with the legacy events, each in its legacy kind and
// carrying its legacy_hash, then one legacy_imported anchor, and every live
// command since chains on top. The verifier must keep accepting exactly those
// rows, so this pins the shapes as the importer wrote them, hashed with the
// package's own auditRowHash over canonicalJSON payloads (the importer's
// LegacyChains did the same), and proves a tampered legacy row fails only its
// own scope.
func TestTheHomelabsImportedChainsVerify(t *testing.T) {
	f := authoritytest.New(t)
	v, sink := newChainVerifier(f)
	owner := newMember(t, f)
	family := f.Family(t, owner.account)
	// The importer wrote the owner's whole chain: drop the sign-in row the
	// fixture gave the account with its session.
	f.Exec(t, `DELETE FROM security_audit_events WHERE scope='account' AND scope_id=$1`, owner.account)

	type head struct {
		seq  uint64
		hash [32]byte
	}
	heads := map[string]*head{}
	// insert chains one row onto a scope's in-memory head and stores it as
	// the importer's COPY did (legacy is nil on anchors).
	insert := func(scope authority.Scope, id string, kind authority.Kind, commandID, actorID string, at time.Time, payload any, legacy *[32]byte) {
		t.Helper()
		h := heads[string(scope)+":"+id]
		if h == nil {
			h = &head{}
			heads[string(scope)+":"+id] = h
		}
		canonical, err := authority.CanonicalJSON(payload)
		if err != nil {
			t.Fatal(err)
		}
		at = at.UTC().Truncate(time.Microsecond)
		seq := h.seq + 1
		hash := authority.AuditRowHash(scope, id, seq, h.hash, kind, commandID, actorID, at, canonical)
		var legacyHash any
		if legacy != nil {
			legacyHash = legacy[:]
		}
		f.Exec(t, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload,legacy_hash)
			VALUES ($1::text::app.audit_scope,$2,$3,$4,$5,$6::text::app.audit_kind,$7,$8,$9,$10::text::jsonb,$11)`,
			string(scope), id, int64(seq), h.hash[:], hash[:], string(kind), commandID, actorID, at, string(canonical), legacyHash)
		h.seq, h.hash = seq, hash
	}
	// legacyEvent chains one legacy event into every scope it names,
	// accounts first, as LegacyChains.Append did.
	events := map[string]int{}
	legacyEvent := func(kind authority.Kind, n int, at time.Time, accounts, families []string) {
		t.Helper()
		raw := json.RawMessage(fmt.Sprintf(`{"kind":%q,"revision":%d,"note":"<b>&</b>","effects":[]}`, kind, n)) // as committed, not canonical
		legacy := sha256.Sum256(raw)
		commandID := authoritytest.NewID(t)
		for _, id := range accounts {
			insert(authority.ScopeAccount, id, kind, commandID, accounts[0], at, raw, &legacy)
			events["account:"+id]++
		}
		for _, id := range families {
			insert(authority.ScopeFamily, id, kind, commandID, accounts[0], at, raw, &legacy)
			events["family:"+id]++
		}
	}
	legacyEvent(authority.KindAccountRegistered, 1, time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC), []string{owner.account}, nil) // the default partition
	legacyEvent(authority.KindFamilyCreated, 2, time.Date(2026, 9, 25, 8, 0, 0, 123456789, time.UTC), []string{owner.account}, []string{family})

	// One legacy_imported anchor per scope: command and actor are the import
	// ID, no legacy_hash, the payload names the legacy head.
	importID := authoritytest.NewID(t)
	importedAt := f.Now.Add(-time.Hour)
	legacyHead := sha256.Sum256([]byte("legacy head"))
	for _, ref := range []struct {
		scope authority.Scope
		id    string
	}{{authority.ScopeAccount, owner.account}, {authority.ScopeFamily, family}} {
		insert(ref.scope, ref.id, authority.KindLegacyImported, importID, importID, importedAt, map[string]any{
			"import_id": importID, "legacy_events": events[string(ref.scope)+":"+ref.id], "legacy_generation": 1,
			"legacy_head": hex.EncodeToString(legacyHead[:]), "legacy_revision": 2,
		}, nil)
	}
	f.Exec(t, `UPDATE accounts SET audit_seq=$2, audit_hash=$3 WHERE id=$1`, owner.account, int64(heads["account:"+owner.account].seq), heads["account:"+owner.account].hash[:])
	f.Exec(t, `UPDATE families SET audit_seq=$2, audit_hash=$3 WHERE id=$1`, family, int64(heads["family:"+family].seq), heads["family:"+family].hash[:])

	// A live command chains on top of the anchor.
	invite(t, f, f.Service, owner, family, "caregiver")
	var seq int64
	var prev []byte
	inAuthorityTxQuery(t, f, `SELECT e.seq, e.prev_hash FROM families fa JOIN security_audit_events e ON e.scope='family' AND e.scope_id=fa.id AND e.seq=fa.audit_seq
		WHERE fa.id=$1`, []any{family}, &seq, &prev)
	anchor := heads["family:"+family]
	if seq != int64(anchor.seq)+1 || [32]byte(prev) != anchor.hash {
		t.Fatalf("the live command's row is seq %d linking %x; want seq %d linking the anchor %x", seq, prev, anchor.seq+1, anchor.hash)
	}

	report := verifyAll(t, v)
	if len(report.Failed) != 0 || report.Scopes != 2 {
		t.Fatalf("the imported chains with a live command on top: %+v, alerts %v", report, sink.mismatches(t))
	}

	// Tampering the family's legacy row fails exactly the family's scope.
	if n := f.Exec(t, `UPDATE security_audit_events SET payload=jsonb_set(payload,'{note}','"tampered"') WHERE scope='family' AND scope_id=$1 AND seq=1`, family); n != 1 {
		t.Fatalf("tamper affected %d rows", n)
	}
	report = verifyAll(t, v)
	if got, want := failedIDs(report), refs("family:"+family); !slices.Equal(got, want) {
		t.Fatalf("failed = %v, want %v", got, want)
	}
	if alerts := sink.mismatches(t); len(alerts) != 1 || alerts[family] != 1 {
		t.Fatalf("alerts %v, want only the family at seq 1", alerts)
	}
}
