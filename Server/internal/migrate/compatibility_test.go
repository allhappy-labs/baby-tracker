package migrate

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestKnownAdditiveMigrationOverlap(t *testing.T) {
	applied := []MigrationDigest{
		{Version: 1, Name: "000001_first.sql", SHA256: strings.Repeat("1", 64)},
		{Version: 2, Name: "000002_second.sql", SHA256: strings.Repeat("2", 64)},
		{Version: 3, Name: "000003_third.sql", SHA256: strings.Repeat("3", 64)},
	}
	known := append(append([]MigrationDigest(nil), applied...), MigrationDigest{Version: 4, Name: "000004_additive.sql", SHA256: strings.Repeat("4", 64)})
	compatibility := Compatibility{Min: 3, Max: 4, Known: known}

	for _, current := range []int{3, 4} {
		if err := ValidateCompatibility(known[:current], compatibility); err != nil {
			t.Fatalf("known schema %d rejected: %v", current, err)
		}
	}

	tests := []struct {
		name    string
		applied []MigrationDigest
	}{
		{name: "changed additive digest", applied: append(append([]MigrationDigest(nil), applied...), MigrationDigest{Version: 4, Name: "000004_additive.sql", SHA256: strings.Repeat("9", 64)})},
		{name: "unknown version", applied: append(append([]MigrationDigest(nil), known...), MigrationDigest{Version: 5, Name: "000005_unknown.sql", SHA256: strings.Repeat("5", 64)})},
		{name: "missing historical version", applied: []MigrationDigest{applied[0], applied[2]}},
		{name: "changed historical name", applied: []MigrationDigest{applied[0], MigrationDigest{Version: 2, Name: "000002_changed.sql", SHA256: strings.Repeat("2", 64)}, applied[2]}},
		{name: "duplicate version", applied: []MigrationDigest{applied[0], applied[0], applied[1], applied[2]}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateCompatibility(test.applied, compatibility); err == nil {
				t.Fatal("invalid applied migration catalog was accepted")
			}
		})
	}
}

func TestMigration000017CompatibilityOverlap(t *testing.T) {
	compatibility, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if compatibility.Min != 29 || compatibility.Max != 29 || len(compatibility.Known) != 29 {
		t.Fatalf("invitation-roles compatibility manifest=%+v", compatibility)
	}
	if err := ValidateCompatibility(compatibility.Known[:4], compatibility); err == nil {
		t.Fatal("feature manifest accepted schema four")
	}
	if err := ValidateCompatibility(compatibility.Known[:5], compatibility); err == nil {
		t.Fatal("family feature manifest accepted schema five")
	}
	if err := ValidateCompatibility(compatibility.Known[:6], compatibility); err == nil {
		t.Fatal("worker-resume manifest accepted schema six")
	}
	if err := ValidateCompatibility(compatibility.Known[:7], compatibility); err == nil {
		t.Fatal("lifecycle feature manifest accepted schema seven")
	}
	if err := ValidateCompatibility(compatibility.Known[:8], compatibility); err == nil {
		t.Fatal("feature manifest accepted schema eight")
	}
	if err := ValidateCompatibility(compatibility.Known[:10], compatibility); err == nil {
		t.Fatal("import predecessor manifest accepted schema ten")
	}
	if err := ValidateCompatibility(compatibility.Known[:11], compatibility); err == nil {
		t.Fatal("delivery predecessor accepted schema eleven")
	}
	if err := ValidateCompatibility(compatibility.Known[:12], compatibility); err == nil {
		t.Fatal("viewer-role manifest accepted schema twelve")
	}
	// The window moves by exactly one, and never widens.
	//
	// 000014 could be deployed ahead of the code — it only widened two CHECK
	// constraints, and a schema-13 binary simply never emitted the new value.
	// Neither of its successors can: this binary emits `member_role_changed`
	// and mints invitations for either role, and against a database without
	// the projections for those the event commits to the ledger and then
	// strands the projection. Refusing to start is the correct behaviour and
	// is what this bound expresses.
	if err := ValidateCompatibility(compatibility.Known[:14], compatibility); err == nil {
		t.Fatal("invitation-roles manifest accepted schema fourteen")
	}
	if err := ValidateCompatibility(compatibility.Known[:15], compatibility); err == nil {
		t.Fatal("feed-component manifest accepted schema fifteen")
	}
	if err := ValidateCompatibility(compatibility.Known[:16], compatibility); err == nil {
		t.Fatal("feed-component feature accepted schema sixteen")
	}
	if err := ValidateCompatibility(compatibility.Known, compatibility); err != nil {
		t.Fatalf("exact embedded schema seventeen rejected: %v", err)
	}
	for _, expected := range []struct {
		index   int
		version int64
		name    string
	}{{15, 16, "000016_invitation_roles.sql"}, {16, 17, "000017_feed_components.sql"}} {
		encoded, err := os.ReadFile("../../migrations/" + expected.name)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("%x", sha256.Sum256(encoded))
		entry := compatibility.Known[expected.index]
		if entry.Version != expected.version || entry.Name != expected.name || entry.SHA256 != want {
			t.Fatalf("migration %d manifest entry=%+v want sha256=%s", expected.version, entry, want)
		}
	}
	changed := append([]MigrationDigest(nil), compatibility.Known...)
	changed[13].SHA256 = strings.Repeat("9", 64)
	if err := ValidateCompatibility(changed, compatibility); err == nil {
		t.Fatal("changed migration fourteen digest was accepted")
	}
	unknown := append(append([]MigrationDigest(nil), compatibility.Known...), MigrationDigest{
		Version: 15, Name: "000015_unknown.sql", SHA256: strings.Repeat("a", 64),
	})
	if err := ValidateCompatibility(unknown, compatibility); err == nil {
		t.Fatal("unknown migration fifteen was accepted")
	}
}

func TestMigrationFeedComponentsRequiresSeventeen(t *testing.T) {
	compatibility, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if compatibility.Min != 29 || compatibility.Max != 29 || len(compatibility.Known) != 29 {
		t.Fatalf("feed-component compatibility manifest=%+v", compatibility)
	}
	if err := ValidateCompatibility(compatibility.Known[:15], compatibility); err == nil {
		t.Fatal("feed-component feature accepted schema fifteen")
	}
	if err := ValidateCompatibility(compatibility.Known[:16], compatibility); err == nil {
		t.Fatal("schema-sixteen predecessor was accepted by feature binary")
	}
	if err := ValidateCompatibility(compatibility.Known, compatibility); err != nil {
		t.Fatalf("schema-seventeen feature rejected: %v", err)
	}
	encoded, err := os.ReadFile("../../migrations/000017_feed_components.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(encoded))
	entry := compatibility.Known[16]
	if entry.Version != 17 || entry.Name != "000017_feed_components.sql" || entry.SHA256 != want {
		t.Fatalf("migration 17 manifest entry=%+v want sha256=%s", entry, want)
	}
}

func TestSupplementsRequiresSchemaEighteen(t *testing.T) {
	c, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompatibility(c.Known[:17], c); err == nil {
		t.Fatal("supplement binary accepted schema seventeen")
	}
	raw, err := os.ReadFile("../../migrations/000018_supplements.sql")
	if err != nil {
		t.Fatal(err)
	}
	if c.Known[17].SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("supplement migration digest mismatch")
	}
}

func TestExportRequiresSchemaNineteen(t *testing.T) {
	c, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompatibility(c.Known[:18], c); err == nil {
		t.Fatal("export binary accepted schema eighteen")
	}
	raw, err := os.ReadFile("../../migrations/000019_exports.sql")
	if err != nil {
		t.Fatal(err)
	}
	if c.Known[18].SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("export migration digest mismatch")
	}
}

func TestSessionLineageStartRequiresSchemaTwenty(t *testing.T) {
	c, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompatibility(c.Known[:19], c); err == nil {
		t.Fatal("one-year session binary accepted schema nineteen")
	}
	raw, err := os.ReadFile("../../migrations/000020_session_lineage_start.sql")
	if err != nil {
		t.Fatal(err)
	}
	if c.Known[19].SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("session lineage migration digest mismatch")
	}
}

func TestGrowthFollowsTimerPauseAtSchemaTwentyFour(t *testing.T) {
	c, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if c.Min != 29 || c.Max != 29 || len(c.Known) != 29 {
		t.Fatalf("combined compatibility=%+v", c)
	}
	if err := ValidateCompatibility(c.Known[:23], c); err == nil {
		t.Fatal("growth binary accepted timer-pause-only schema")
	}
	for _, expected := range []struct {
		index   int
		version int64
		name    string
	}{
		{22, 23, "000023_timer_pause.sql"}, {23, 24, "000024_growth.sql"},
	} {
		raw, err := os.ReadFile("../../migrations/" + expected.name)
		if err != nil {
			t.Fatal(err)
		}
		entry := c.Known[expected.index]
		if entry.Version != expected.version || entry.Name != expected.name || entry.SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
			t.Fatalf("migration identity mismatch: %+v", entry)
		}
	}
	if err := ValidateCompatibility(c.Known, c); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRequiresSchemaTwentyFive(t *testing.T) {
	c, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompatibility(c.Known[:24], c); err == nil {
		t.Fatal("recovery binary accepted schema24")
	}
	raw, err := os.ReadFile("../../migrations/000025_refresh_recovery.sql")
	if err != nil {
		t.Fatal(err)
	}
	if c.Known[24].Version != 25 || c.Known[24].Name != "000025_refresh_recovery.sql" || c.Known[24].SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("recovery migration identity mismatch")
	}
}

func TestSleepSessionsFollowRefreshRecoveryAtSchemaTwentySix(t *testing.T) {
	c, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if c.Min != 29 || c.Max != 29 || len(c.Known) != 29 {
		t.Fatalf("unexpected compatibility %+v", c)
	}
	if err := ValidateCompatibility(c.Known[:25], c); err == nil {
		t.Fatal("sleep-sessions binary accepted schema25")
	}
	raw, err := os.ReadFile("../../migrations/000026_sleep_sessions.sql")
	if err != nil {
		t.Fatal(err)
	}
	if c.Known[25].Version != 26 || c.Known[25].Name != "000026_sleep_sessions.sql" || c.Known[25].SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("sleep-sessions migration identity mismatch")
	}
}

// 000027 rewrites stored records and sync history into the current formats,
// so this binary refuses a schema-26 database: the app stays stopped while
// `migrate` runs, and 26 is no longer inside the window.
func TestFormatCleanupRequiresSchemaTwentySeven(t *testing.T) {
	c, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if c.Min != 29 || c.Max != 29 || len(c.Known) != 29 {
		t.Fatalf("unexpected compatibility %+v", c)
	}
	if err := ValidateCompatibility(c.Known[:26], c); err == nil {
		t.Fatal("format-cleanup binary accepted schema26")
	}
	raw, err := os.ReadFile("../../migrations/000027_format_cleanup.sql")
	if err != nil {
		t.Fatal(err)
	}
	if c.Known[26].Version != 27 || c.Known[26].Name != "000027_format_cleanup.sql" || c.Known[26].SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("format-cleanup migration identity mismatch")
	}
	if err := ValidateCompatibility(c.Known, c); err != nil {
		t.Fatal(err)
	}
}

func TestLiveActivityRequiresSchemaTwentyEight(t *testing.T) {
	c, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateCompatibility(c.Known[:27], c); err == nil {
		t.Fatal("live activity binary accepted schema27")
	}
	raw, err := os.ReadFile("../../migrations/000028_live_activity_push.sql")
	if err != nil {
		t.Fatal(err)
	}
	if c.Known[27].Version != 28 || c.Known[27].Name != "000028_live_activity_push.sql" || c.Known[27].SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("live activity migration identity mismatch")
	}
}

func TestBabyIconRequiresSchemaTwentyNine(t *testing.T) {
	c, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateCompatibility(c.Known[:28], c); err == nil {
		t.Fatal("baby icon binary accepted schema 28")
	}
	raw, err := os.ReadFile("../../migrations/000029_baby_icon.sql")
	if err != nil {
		t.Fatal(err)
	}
	if c.Known[28].Version != 29 || c.Known[28].Name != "000029_baby_icon.sql" || c.Known[28].SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("baby icon migration identity mismatch")
	}
}
