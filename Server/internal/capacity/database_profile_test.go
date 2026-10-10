package capacity

import (
	"babytracker/server/internal/config"
	"testing"
)

func TestDatabaseProfileIsAtomicAndExplicitlySelected(t *testing.T) {
	for _, profile := range []string{"", "legacy"} {
		env, budget, err := databaseProfile(profile)
		if err != nil || len(env) != 0 || budget != (config.DatabasePools{}) {
			t.Fatalf("legacy altered: %v %+v %v", env, budget, err)
		}
	}
	env, budget, err := databaseProfile("explicit")
	if err != nil || len(env) != 5 || budget != (config.DatabasePools{Runtime: 16, Maintenance: 2, MCP: 0, ServerMax: 64, Reserved: 8}) {
		t.Fatalf("explicit=%v %+v %v", env, budget, err)
	}
	loaded, err := config.LoadDatabasePools(func(k string) string { return env[k] })
	if err != nil || loaded != budget {
		t.Fatalf("launcher settings are not complete: %v %+v", err, loaded)
	}
	for _, profile := range []string{"partial", "16/4/2", " explicit "} {
		if _, _, err := databaseProfile(profile); err == nil {
			t.Fatal("ambiguous profile accepted")
		}
	}
}
func TestResultDatabaseProfileCompatibilityAndValidation(t *testing.T) {
	r := coherentCell()
	if !r.provenanceValid() {
		t.Fatal("old evidence semantics changed")
	}
	r.DatabaseProfile = "legacy"
	if !r.provenanceValid() {
		t.Fatal("legacy rejected")
	}
	r.DatabaseProfile = "explicit"
	if r.provenanceValid() {
		t.Fatal("missing explicit settings accepted")
	}
	_, budget, _ := databaseProfile("explicit")
	r.DatabasePools = &budget
	if !r.provenanceValid() {
		t.Fatal("complete profile rejected")
	}
	r.DatabasePools.Runtime = 17
	if r.provenanceValid() {
		t.Fatal("mismatched profile accepted")
	}
	r.DatabaseProfile = "legacy"
	if r.provenanceValid() {
		t.Fatal("partial/ambiguous legacy metadata accepted")
	}
}
