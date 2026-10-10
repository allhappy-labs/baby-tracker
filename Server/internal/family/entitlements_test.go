package family

import (
	"testing"
)

// Ported from TestSelfHostEntitlementAlwaysOnForFamilySync: a self-hosted
// node needs no family_sync entitlement row (the authority requires one only
// on a hosted node or for a family marked entitlement_required), so a
// family without one reads and is managed freely; a hosted node bootstraps
// the grant at creation and refuses once it is gone.
func TestSelfHostEntitlementAlwaysOnForFamilySync(t *testing.T) {
	h := newFamilyFixture(t)
	familyID := h.familyID(h.Owner.AccountID)
	var rows int
	h.query(`SELECT count(*) FROM family_entitlements WHERE family_id=$1`, []any{familyID}, &rows)
	if rows != 0 {
		t.Fatalf("self-hosted family has %d entitlement rows", rows)
	}
	if _, err := h.Service.Current(t.Context(), h.Owner); err != nil {
		t.Fatalf("self-hosted read: %v", err)
	}
	if _, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "caregiver"); err != nil {
		t.Fatalf("self-hosted manage: %v", err)
	}

	hosted := newFamilyFixtureMode(t, true)
	hostedFamily := hosted.familyID(hosted.Owner.AccountID)
	var key, source, status string
	hosted.query(`SELECT entitlement_key, source, status::text FROM family_entitlements WHERE family_id=$1`, []any{hostedFamily}, &key, &source, &status)
	if key != "family_sync" || source != "hosted_beta" || status != "active" {
		t.Fatalf("hosted bootstrap entitlement key=%q source=%q status=%q", key, source, status)
	}
	if affected := hosted.exec(`DELETE FROM family_entitlements WHERE family_id=$1`, hostedFamily); affected != 1 {
		t.Fatalf("entitlement removal affected %d", affected)
	}
	if _, err := hosted.Service.Current(t.Context(), hosted.Owner); Code(err) != "entitlement_required" {
		t.Fatalf("hosted read without entitlement code=%q err=%v", Code(err), err)
	}
}
