package testpg

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"babytracker/server/migrations"
)

// Versions 1–31 were squashed into 000032_baseline.sql: Through(…, 0) is still
// the empty database and Through(…, 32) the baseline, but anything between
// names a schema no file builds, and fails instead of silently migrating
// nothing.
func TestThroughRefusesSquashedVersions(t *testing.T) {
	names := func(source fs.FS) ([]string, error) {
		entries, err := fs.ReadDir(source, ".")
		var out []string
		for _, entry := range entries {
			out = append(out, entry.Name())
		}
		return out, err
	}
	if got, err := names(Through(migrations.Files, 0)); err != nil || len(got) != 0 {
		t.Fatalf("Through 0: %v %v", got, err)
	}
	if pending, err := hasMigrations(Through(migrations.Files, 0)); err != nil || pending {
		t.Fatalf("Through 0 pending %t: %v", pending, err)
	}
	if got, err := names(Through(migrations.Files, 32)); err != nil || len(got) != 1 || got[0] != "000032_baseline.sql" {
		t.Fatalf("Through 32: %v %v", got, err)
	}
	for _, version := range []int64{1, 28, 29, 31} {
		if _, err := names(Through(migrations.Files, version)); err == nil ||
			!strings.Contains(err.Error(), "pre-baseline versions were squashed into 000032_baseline.sql") {
			t.Fatalf("Through %d: %v", version, err)
		}
		if _, err := hasMigrations(Through(migrations.Files, version)); err == nil {
			t.Fatalf("Through %d: newFixture would build an empty database", version)
		}
	}
	// A chain without a baseline (as before the squash) filters as before.
	chain := fstest.MapFS{
		"000001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		"000002_b.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
	}
	if got, err := names(Through(chain, 1)); err != nil || len(got) != 1 || got[0] != "000001_a.sql" {
		t.Fatalf("chain through 1: %v %v", got, err)
	}
}
