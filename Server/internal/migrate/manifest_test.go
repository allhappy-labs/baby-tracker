package migrate

import (
	"testing"
	"testing/fstest"
)

func TestManifestRejectsChangedHistoricalMigration(t *testing.T) {
	files := fstest.MapFS{
		"000001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;\n")},
		"000002_b.sql": &fstest.MapFile{Data: []byte("SELECT 2;\n")},
	}
	old, err := GenerateManifest(files, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	files["000001_a.sql"] = &fstest.MapFile{Data: []byte("SELECT 9;\n")}
	next, err := GenerateManifest(files, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifestExtension(old, next); err == nil {
		t.Fatal("historical SQL digest changed")
	}
}

func TestManifestExtensionRequiresOneContiguousEntry(t *testing.T) {
	files := fstest.MapFS{"000001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;\n")}}
	old, err := GenerateManifest(files, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	files["000002_b.sql"] = &fstest.MapFile{Data: []byte("SELECT 2;\n")}
	next, err := GenerateManifest(files, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifestExtension(old, next); err != nil {
		t.Fatalf("one-entry extension rejected: %v", err)
	}
	if err := VerifyManifestExtension(next, next); err != nil {
		t.Fatalf("same-version exact regeneration rejected: %v", err)
	}
	files["000003_c.sql"] = &fstest.MapFile{Data: []byte("SELECT 3;\n")}
	twoAhead, err := GenerateManifest(files, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifestExtension(old, twoAhead); err == nil {
		t.Fatal("two-entry extension was accepted")
	}
}

func TestPreparedManifestHistoryAllowsOnlyBoundChanges(t *testing.T) {
	files := fstest.MapFS{
		"000001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;\n")},
		"000002_b.sql": &fstest.MapFile{Data: []byte("SELECT 2;\n")},
	}
	prior, err := GenerateManifest(files, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	next, err := GenerateManifest(files, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifestHistory(prior, next); err != nil {
		t.Fatal(err)
	}
	next.Known[0].SHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := VerifyManifestHistory(prior, next); err == nil {
		t.Fatal("changed prepared migration digest was accepted")
	}
}
