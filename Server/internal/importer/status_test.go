package importer_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// statusWrites is what a status read sent that a read must not: a family
// row lock, a write, or a read-write transaction.
func statusWrites(statements []string) []string {
	var found []string
	for _, sql := range statements {
		upper := strings.ToUpper(sql)
		if strings.Contains(upper, "FOR NO KEY UPDATE") || strings.Contains(upper, "FOR UPDATE") ||
			strings.HasPrefix(strings.TrimSpace(upper), "UPDATE") || strings.Contains(upper, "READ WRITE") {
			found = append(found, sql)
		}
	}
	return found
}

func TestStatusPageTakesNoFamilyLockAndChecksAccessFirst(t *testing.T) {
	h := testkit.New(t)
	resolved := h.Import(t, h.Owner, "8a000000-0000-4000-8000-000000000001", "Mira", "2026-01-01")
	pool, wire := testkit.CountedPool(t, h.Runtime)
	counted := h.ImporterOn(h.AuthorityOn(t, pool))
	holder := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	defer holder.Rollback(t.Context())
	if _, err := care.LockFamily(t.Context(), holder, h.FamilyID); err != nil {
		t.Fatal(err) // a writer holds the family row
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	wire.Reset()
	out, err := counted.StatusPage(ctx, h.Owner, resolved.ImportID, nil)
	if err != nil {
		t.Fatalf("a status read waited on the family lock: %v", err)
	}
	if !out.MappingComplete || len(out.Mappings) != 1 || out.ImportID != resolved.ImportID {
		t.Fatalf("status %+v", out)
	}
	if writes := statusWrites(wire.Statements()); len(writes) != 0 {
		t.Fatalf("a status read locked or wrote: %q", writes)
	}
	if _, err := counted.StatusPage(ctx, h.Caregiver, resolved.ImportID, nil); err == nil || err.Error() != "import_access_denied" {
		t.Fatalf("another member's status: %v", err)
	}
	if _, err := counted.StatusPage(ctx, h.Caregiver, "8a000000-0000-4000-8000-0000000000ff", nil); err == nil || err.Error() != "import_not_found" {
		t.Fatalf("an unknown import: %v", err)
	}
}

// An unfinished import past its expiry is marked expired once, under the
// family lock, and refused import_expired; a later status reads the mark
// and writes nothing. Another member's expired import is refused
// import_access_denied before anything is marked.
func TestStatusPageMarksExpiryUnderTheFamilyLock(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Mira", "2026-01-01")
	r.TotalRecordCount = 2
	r.Finalize = false
	r.Records = []protocol.RecordManifest{{RecordID: testkit.RecordA, LocalBabyID: r.Babies[0].LocalBabyID, EntityKind: "feed", ContentSHA256: fmt.Sprintf("%064d", 0)}}
	if _, err := h.Importer.Resolve(t.Context(), h.Owner, r); err != nil {
		t.Fatal(err)
	}
	h.ExecFamily(t, `UPDATE import_sessions SET expires_at=created_at+interval '1 second' WHERE family_id=$1`, h.FamilyID)
	h.Now = h.Now.Add(2 * time.Second)
	pool, wire := testkit.CountedPool(t, h.Runtime)
	counted := h.ImporterOn(h.AuthorityOn(t, pool))
	expiredAt := func() *time.Time {
		t.Helper()
		var at *time.Time
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
		defer tx.Rollback(t.Context())
		if err := tx.QueryRow(t.Context(), `SELECT expired_at FROM import_sessions WHERE family_id=$1 AND import_id=$2`, h.FamilyID, r.ImportID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}

	wire.Reset()
	if _, err := counted.StatusPage(t.Context(), h.Caregiver, r.ImportID, nil); err == nil || err.Error() != "import_access_denied" {
		t.Fatalf("another member's expired import: %v", err)
	}
	if writes := statusWrites(wire.Statements()); len(writes) != 0 || expiredAt() != nil {
		t.Fatalf("another member's status marked the import: %q", writes)
	}

	wire.Reset()
	if _, err := counted.StatusPage(t.Context(), h.Owner, r.ImportID, nil); err == nil || err.Error() != "import_expired" {
		t.Fatalf("expired import: %v", err)
	}
	statements := wire.Statements()
	lock, update, updates := -1, -1, 0
	for i, sql := range statements {
		switch {
		case strings.Contains(sql, "FOR NO KEY UPDATE") && strings.Contains(sql, "FROM families"):
			if lock < 0 {
				lock = i
			}
		case strings.HasPrefix(strings.TrimSpace(sql), "UPDATE import_sessions SET expired_at"):
			update = i
			updates++
		}
	}
	if updates != 1 || lock < 0 || update < lock {
		t.Fatalf("marking: lock at %d, %d update(s), the last at %d: %q", lock, updates, update, statements)
	}
	if at := expiredAt(); at == nil || !at.Equal(h.Now) {
		t.Fatalf("expired_at %v, want %v", at, h.Now)
	}

	wire.Reset()
	if _, err := counted.StatusPage(t.Context(), h.Owner, r.ImportID, nil); err == nil || err.Error() != "import_expired" {
		t.Fatalf("expired import, again: %v", err)
	}
	if writes := statusWrites(wire.Statements()); len(writes) != 0 {
		t.Fatalf("a marked import was written again: %q", writes)
	}
}

// The import service serves the principal's session family with no family
// read: a principal that is not a refreshed family session is refused
// before any statement, and one naming another family than its session's
// row is refused by the guarded transaction (membership_revoked).
func TestImporterServesTheSessionFamilyWithoutReadingIt(t *testing.T) {
	h := testkit.New(t)
	resolved := h.Import(t, h.Owner, "8a000000-0000-4000-8000-000000000001", "Mira", "2026-01-01")
	pool, wire := testkit.CountedPool(t, h.Runtime)
	counted := h.ImporterOn(h.AuthorityOn(t, pool))
	account := h.Owner
	account.Scope = "account"
	unrefreshed := h.Owner
	unrefreshed.SessionScope, unrefreshed.SessionFamilyID = "account", ""
	forged := h.Owner
	forged.SessionFamilyID = "8b000000-0000-4000-8000-000000000001"
	for _, c := range []struct {
		name  string
		p     func() error
		code  string
		trips bool
	}{
		{"account scope, status", func() error { _, e := counted.StatusPage(t.Context(), account, resolved.ImportID, nil); return e }, "family_scope_required", false},
		{"account scope, resolve", func() error {
			_, e := counted.Resolve(t.Context(), account, testkit.ImportRequest(1, "Mira", "2026-01-01"))
			return e
		}, "family_scope_required", false},
		{"unrefreshed session, status", func() error { _, e := counted.StatusPage(t.Context(), unrefreshed, resolved.ImportID, nil); return e }, "access_expired", false},
		{"another family, status", func() error { _, e := counted.StatusPage(t.Context(), forged, resolved.ImportID, nil); return e }, "membership_revoked", true},
		{"another family, resolve", func() error {
			_, e := counted.Resolve(t.Context(), forged, testkit.ImportRequest(1, "Mira", "2026-01-01"))
			return e
		}, "membership_revoked", true},
	} {
		wire.Reset()
		if e := c.p(); e == nil || !strings.Contains(e.Error(), c.code) {
			t.Errorf("%s: %v, want %s", c.name, e, c.code)
		}
		if trips := wire.Trips(); (trips != 0) != c.trips {
			t.Errorf("%s: %d round trips", c.name, trips)
		}
		for _, sql := range wire.Statements() {
			if strings.Contains(sql, "FROM memberships") {
				t.Errorf("%s: read the family: %q", c.name, sql)
			}
		}
	}
}
