package database_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"babytracker/server/internal/testkit"
	"babytracker/server/internal/testpg"
)

// The baseline's catalog is pinned: a squash that changes it by accident
// fails here. The baseline is frozen, so this golden no longer changes.
func TestBaselineCatalogMatchesTheChainGolden(t *testing.T) { // Review Focus 2
	f := testpg.NewThrough(t, 32)
	testpg.ProductionShape(t, f)
	got := testpg.SnapshotCatalog(t, f.Owner, testpg.Roles(t, f))
	testkit.AssertGolden(t, "../migrate/testdata/catalog-32.golden", got.Lines)
}

// The catalog with 000033 applied stays pinned beside the baseline's; it no
// longer changes.
func TestImmunizationsCatalogMatchesItsGolden(t *testing.T) {
	f := testpg.NewThrough(t, 33)
	testpg.ProductionShape(t, f)
	got := testpg.SnapshotCatalog(t, f.Owner, testpg.Roles(t, f))
	testkit.AssertGolden(t, "../migrate/testdata/catalog-33.golden", got.Lines)
}

// The catalog with 000034 applied stays pinned beside the earlier ones; it
// no longer changes.
func TestPumpingCatalogMatchesItsGolden(t *testing.T) {
	f := testpg.NewThrough(t, 34)
	testpg.ProductionShape(t, f)
	got := testpg.SnapshotCatalog(t, f.Owner, testpg.Roles(t, f))
	testkit.AssertGolden(t, "../migrate/testdata/catalog-34.golden", got.Lines)
}

// The catalog with 000035 applied stays pinned beside the earlier ones; it
// no longer changes.
func TestAuditRetentionCatalogMatchesItsGolden(t *testing.T) {
	f := testpg.NewThrough(t, 35)
	testpg.ProductionShape(t, f)
	got := testpg.SnapshotCatalog(t, f.Owner, testpg.Roles(t, f))
	testkit.AssertGolden(t, "../migrate/testdata/catalog-35.golden", got.Lines)
}

// The catalog with 000036 applied stays pinned beside the earlier ones; it
// no longer changes.
func TestRequestAuthorityFamilyKeyCatalogMatchesItsGolden(t *testing.T) {
	f := testpg.NewThrough(t, 36)
	testpg.ProductionShape(t, f)
	got := testpg.SnapshotCatalog(t, f.Owner, testpg.Roles(t, f))
	testkit.AssertGolden(t, "../migrate/testdata/catalog-36.golden", got.Lines)
}

// The current fresh-install catalog, 000037 applied, is pinned beside the
// earlier ones: a schema change updates this golden in the same commit
// (BT_GOLDEN_UPDATE=1), so a migration that changes the catalog by accident
// fails here.
func TestCurrentCatalogMatchesItsGolden(t *testing.T) {
	f := testpg.New(t)
	testpg.ProductionShape(t, f)
	got := testpg.SnapshotCatalog(t, f.Owner, testpg.Roles(t, f))
	testkit.AssertGolden(t, "../migrate/testdata/catalog-37.golden", got.Lines)
}

// chainGolden is catalog-32.golden as the 000001–000032 chain recorded it
// (Phase 4 Task 26, commit 0ee63ac7, the database ACL line included),
// vendored: the squashed baseline matched it byte for byte until Task 27
// dropped the roles. Its digest is pinned, so it never changes.
const (
	chainGolden       = "../migrate/testdata/catalog-32-chain.golden"
	chainGoldenSHA256 = "016973764c16bd068ad0460ce5b91a31f2123c513927e93e1b32ef8d63ee97ee"
)

// retiredRoles are the two roles Task 27 drops from the baseline (C2).
var retiredRoles = []string{"baby_tracker_projector", "baby_tracker_apple_revoker"}

// The baseline differs from the chain's golden by exactly the two retired
// roles: their manifest rows, their ACL items and their two values in the
// manifest's principal CHECK. Nothing else in the catalog moved.
func TestBaselineDropsExactlyTheLegacyRoles(t *testing.T) { // Review Focus 2
	chain := readChainGolden(t)
	f := testpg.NewThrough(t, 32)
	testpg.ProductionShape(t, f)
	now := testpg.SnapshotCatalog(t, f.Owner, testpg.Roles(t, f))

	// Every line that differs names a retired role, or is the CHECK that did.
	diff := testpg.Catalog{Lines: chain}.Diff(now)
	for _, line := range diff {
		if strings.HasPrefix(line, "+ ") {
			continue // checked against the expected catalog below
		}
		if !namesRetiredRole(line) && !strings.Contains(line, "schema_privilege_manifest_principal_check") {
			t.Fatalf("the role drop changed more than the two roles: %s", line)
		}
	}
	// And the chain's catalog with those roles taken out is exactly today's.
	expected, changed := withoutRetiredRoles(chain)
	if drift := (testpg.Catalog{Lines: expected}).Diff(now); len(drift) != 0 {
		t.Fatalf("the baseline is not the chain without the two roles:\n%s", strings.Join(drift, "\n"))
	}
	// 5 manifest rows, the ACLs of schema app and the two catalog tables, and the CHECK.
	if changed != 9 || len(diff) != 5+2*4 {
		t.Fatalf("%d chain lines name the retired roles, %d lines differ; want 9 and 13:\n%s", changed, len(diff), strings.Join(diff, "\n"))
	}
	for _, line := range now.Lines {
		if namesRetiredRole(line) {
			t.Fatalf("the baseline still names a retired role: %s", line)
		}
	}
	var granted int
	if err := f.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_class c, aclexplode(coalesce(c.relacl, acldefault('r', c.relowner))) a
		JOIN pg_roles r ON r.oid = a.grantee WHERE r.rolname IN ('baby_tracker_projector','baby_tracker_apple_revoker')`).Scan(&granted); err != nil {
		t.Fatal(err)
	}
	if granted != 0 {
		t.Fatalf("%d grants still name a retired role", granted)
	}
}

func namesRetiredRole(line string) bool {
	for _, role := range retiredRoles {
		if strings.Contains(line, role) {
			return true
		}
	}
	return false
}

// withoutRetiredRoles is the catalog lines with the retired roles taken out:
// their manifest rows dropped, their items removed from every ACL, and their
// values removed from the manifest's principal CHECK. changed counts the lines
// it dropped or rewrote.
func withoutRetiredRoles(lines []string) (kept []string, changed int) {
	for _, line := range lines {
		if !namesRetiredRole(line) {
			kept = append(kept, line)
			continue
		}
		changed++
		if strings.HasPrefix(line, "manifest|") {
			continue
		}
		for _, role := range retiredRoles {
			line = strings.ReplaceAll(line, ", '"+role+"'::text", "")
		}
		if at := strings.LastIndex(line, "|acl="); at >= 0 {
			var items []string
			for _, item := range strings.Split(line[at+len("|acl="):], ",") {
				if item != "" && !namesRetiredRole(item) {
					items = append(items, item)
				}
			}
			line = line[:at] + "|acl=" + strings.Join(items, ",")
		}
		kept = append(kept, line)
	}
	return kept, changed
}

// readChainGolden reads the vendored chain golden (a JSON array of lines),
// refusing any byte change to it.
func readChainGolden(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(chainGolden)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != chainGoldenSHA256 {
		t.Fatalf("%s changed: sha256 %x, want %s (it is the chain's record, never re-recorded)", chainGolden, sum, chainGoldenSHA256)
	}
	var lines []string
	if err := json.Unmarshal(raw, &lines); err != nil {
		t.Fatalf("%s: %v", chainGolden, err)
	}
	return lines
}
