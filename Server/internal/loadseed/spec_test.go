//go:build identitytest

package loadseed

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
)

// Every planned mutation is one the write path accepts, and survives the
// wire unchanged: a phone sending its JSON sends the same mutation.
func TestPlannedMutationsAreValid(t *testing.T) {
	spec := R10k(3, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	limits := protocol.DefaultLimits()
	kinds := map[string]int{}
	for i := 0; i < 60; i++ {
		plan := planFamily(spec, i)
		for _, m := range plan.Mutations() {
			if err := care.ValidateMutation(m, limits); err != nil {
				t.Fatalf("family %d (%d days): %s %s: %v", i, plan.Days, m.EntityKind, m.Operation, err)
			}
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := protocol.DecodeMutation(raw)
			if err != nil {
				t.Fatalf("family %d: %s %s does not decode: %v\n%s", i, m.EntityKind, m.Operation, err, raw)
			}
			again, _ := json.Marshal(decoded)
			if !bytes.Equal(raw, again) {
				t.Fatalf("family %d: %s %s changes on the wire:\n%s\n%s", i, m.EntityKind, m.Operation, raw, again)
			}
			kinds[m.EntityKind+" "+m.Operation]++
		}
	}
	for _, want := range []string{"baby create", "growth_profile create", "feed create", "feed update", "feed delete", "diaper create",
		"sleep create", "supplement create", "measurement create", "immunization create", "pump create", "timer timer_start"} {
		if kinds[want] == 0 {
			t.Errorf("no %s planned in 60 families: %v", want, kinds)
		}
	}
}

func TestR10kShape(t *testing.T) {
	s := R10k(1, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	if s.Families != 10000 || s.BacklogHours != 24 || s.BacklogLoad != 2 {
		t.Fatalf("%+v", s)
	}
	if mean := s.History.MeanEntitiesPerFamily(s.TwinShare); mean < 950 || mean > 1250 {
		t.Fatalf("mean entities per family %.0f, want about 1,100", mean)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPlanIsDeterministicPerSeed(t *testing.T) {
	spec := Small(3, 42, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	a, b := planFamily(spec, 2), planFamily(spec, 2)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("the same seed and index planned two different families")
	}
	if reflect.DeepEqual(a, planFamily(Small(3, 43, spec.AsOf), 2)) {
		t.Fatal("the seed is ignored")
	}
	if reflect.DeepEqual(a, planFamily(spec, 1)) {
		t.Fatal("the family index is ignored")
	}
}

// The planned R10k families average what MeanEntitiesPerFamily promises,
// so the analytical model and the generator agree.
func TestPlannedFamiliesMatchTheProfile(t *testing.T) {
	spec := R10k(7, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	const sample = 400
	entities, viewers, twins, mcp := 0, 0, 0, 0
	for i := 0; i < sample; i++ {
		plan := planFamily(spec, i)
		entities += plan.Entities()
		if plan.Viewer {
			viewers++
		}
		if len(plan.Babies) == 2 {
			twins++
		}
		if plan.MCP {
			mcp++
		}
	}
	mean := float64(entities) / sample
	want := spec.History.MeanEntitiesPerFamily(spec.TwinShare)
	if mean < want*0.85 || mean > want*1.15 {
		t.Fatalf("planned %.0f entities per family, the profile says %.0f", mean, want)
	}
	if viewers < sample*15/100 || viewers > sample*35/100 {
		t.Fatalf("%d of %d families have a viewer, want about 25%%", viewers, sample)
	}
	if twins < sample*8/100 || twins > sample*22/100 {
		t.Fatalf("%d of %d families have two babies, want about 15%%", twins, sample)
	}
	if mcp > sample*6/100 {
		t.Fatalf("%d of %d families have an MCP grant, want about 2%%", mcp, sample)
	}
}

// MCP grants are an exact quota, so the 100-family smoke has the
// integrations its model's MCP calls need.
func TestMCPGrantsAreAQuotaSpreadOverTheFamilies(t *testing.T) {
	asOf := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ families, want int }{{100, 2}, {10_000, 200}, {10, 1}} {
		spec := R10k(1, asOf)
		spec.Families = c.families
		var at []int
		for i := 0; i < c.families; i++ {
			if _, mcp := familyShape(spec, i); mcp {
				at = append(at, i)
			}
		}
		if len(at) != c.want {
			t.Fatalf("%d families: %d MCP grants, want %d", c.families, len(at), c.want)
		}
		for k := 1; k < len(at); k++ {
			if gap := at[k] - at[k-1]; gap < 49 || gap > 51 {
				t.Fatalf("%d families: grants at %d and %d, want them 50 apart", c.families, at[k-1], at[k])
			}
		}
	}
}

func TestSpecDigestNamesTheShape(t *testing.T) {
	asOf := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	a, err := R10k(1, asOf).SHA256()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := R10k(1, asOf).SHA256()
	c, _ := R10k(2, asOf).SHA256()
	if a != b || a == c || len(a) != 64 {
		t.Fatalf("digests %s %s %s", a, b, c)
	}
	// Workers shapes nothing: the same seed on more workers is the same seed.
	more := R10k(1, asOf)
	more.Workers = 32
	if d, _ := more.SHA256(); d != a {
		t.Fatal("the worker count changed the seed's digest")
	}
}

func TestEntityIDsAreUUIDv4AndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for family := 0; family < 3; family++ {
		for ordinal := uint64(0); ordinal < 100; ordinal++ {
			id := entityID(9, family, ordinal)
			if len(id) != 36 || id[14] != '4' || seen[id] {
				t.Fatalf("entity id %q (family %d, ordinal %d)", id, family, ordinal)
			}
			seen[id] = true
		}
	}
	if entityID(9, 1, 1) != entityID(9, 1, 1) || entityID(9, 1, 1) == entityID(10, 1, 1) {
		t.Fatal("entity ids are not a function of seed, family and ordinal")
	}
}
