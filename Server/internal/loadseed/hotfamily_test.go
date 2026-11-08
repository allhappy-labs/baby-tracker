//go:build identitytest

package loadseed

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The hot family is opt-in: a spec without it keeps its JSON, so every
// earlier fleet's SeedSpecSHA256 and every determinism pin still hold.
func TestHotFamilyLeavesTheSpecDigestOfASpecWithoutIt(t *testing.T) {
	asOf := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	spec := R10k(1, asOf)
	without, err := spec.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	spec.HotFamily = true
	with, err := spec.SHA256()
	if err != nil || with == without {
		t.Fatalf("the hot family did not change the digest: %v", err)
	}
	if spec.SeededFamilies() != 10001 || spec.SeededDevices() != 40010 {
		t.Fatalf("seeded families %d, devices %d", spec.SeededFamilies(), spec.SeededDevices())
	}
	spec.HotFamily = false
	if again, _ := spec.SHA256(); again != without {
		t.Fatal("a spec without the hot family changed its digest")
	}
}

// seed --hot-family: one more family after the others, 5 accounts (the
// owner and four caregivers) × 2 devices, every device signed in to the
// family through the product's commands, named in the report and the fleet
// header. The other families are seeded exactly as without it.
func TestSeedHotFamily(t *testing.T) {
	env := newSeedEnvironment(t)
	spec := Small(2, 1, time.Now().UTC().Truncate(time.Second))
	spec.HotFamily = true
	report, header, fleet, err := Seed(t.Context(), env, spec)
	if err != nil {
		t.Fatal(err)
	}
	if report.Families != 3 || header.Families != 3 || len(fleet) != 18 || header.Devices != 18 || report.HotFamilyID == "" ||
		header.HotFamilyID != report.HotFamilyID {
		t.Fatalf("report %+v header %+v", report, header)
	}
	assertFleetShape(t, fleet[:8])
	hot := fleet[8:]
	accounts := map[string]int{}
	ips := map[string]bool{}
	for _, d := range fleet {
		ips[d.ClientIP] = true
	}
	if len(ips) != len(fleet) {
		t.Fatalf("%d addresses for %d devices", len(ips), len(fleet))
	}
	for i, d := range hot {
		want := "caregiver"
		if i < 2 {
			want = "owner"
		}
		if d.FamilyID != header.HotFamilyID || d.Role != want || d.AccessToken == "" || d.LiveActivityToken == "" || len(d.BabyIDs) == 0 ||
			d.MCPCredential != "" || !strings.HasPrefix(d.ClientIP, "100.64.") {
			t.Fatalf("hot device %d: %+v", i, redacted(d))
		}
		accounts[d.AccountID]++
	}
	if len(accounts) != HotFamilyAccounts {
		t.Fatalf("%d hot accounts, want %d", len(accounts), HotFamilyAccounts)
	}
	for account, n := range accounts {
		if n != HotFamilyDevicesPerAccount {
			t.Fatalf("hot account %s holds %d devices", account, n)
		}
	}
	var members int
	if err := env.Superuser.QueryRow(t.Context(), `SELECT count(*) FROM memberships WHERE family_id = $1 AND status = 'active'`, header.HotFamilyID).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != HotFamilyAccounts {
		t.Fatalf("the hot family has %d members", members)
	}
	assertTables(t, env, report)
	s, err := compose(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	for i := range hot {
		mustAuthenticate(t, s, &hot[i])
	}
	// Its history is family 2's plan, written by the owner and the first
	// caregiver; the counters hold and a pull returns every row.
	plan := planFamily(spec, 2)
	var rows, writers int
	if err := env.Superuser.QueryRow(t.Context(), `SELECT count(*), count(DISTINCT actor_account_id) FROM care_entities WHERE family_id = $1`,
		header.HotFamilyID).Scan(&rows, &writers); err != nil {
		t.Fatal(err)
	}
	if rows != plan.Entities() || writers < 1 || writers > 2 {
		t.Fatalf("the hot family holds %d rows by %d writers; its plan %d entities", rows, writers, plan.Entities())
	}
	// Every change names a device of the account that wrote it.
	accountOf := map[string]string{}
	for _, d := range hot {
		accountOf[d.DeviceID] = d.AccountID
	}
	pairs, err := env.Superuser.Query(t.Context(), `SELECT DISTINCT actor_account_id::text, device_id::text FROM care_entities WHERE family_id = $1`,
		header.HotFamilyID)
	if err != nil {
		t.Fatal(err)
	}
	var actor, device string
	if _, err := pgx.ForEachRow(pairs, []any{&actor, &device}, func() error {
		if accountOf[device] != actor {
			t.Errorf("a change by account %s names device %s, which is account %q's", actor, device, accountOf[device])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := verifyCounters(t.Context(), env, header.HotFamilyID); err != nil {
		t.Fatal(err)
	}
	mustPullEverything(t, env, s, fleet, header.HotFamilyID)
	// The other families are planFamily's, as without the hot family.
	assertPlannedEntities(t, env, spec, fleet[0].FamilyID)
}
