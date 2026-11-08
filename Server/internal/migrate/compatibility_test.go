package migrate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
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

// 000001–000032 were squashed into 000032_baseline.sql before the homelab
// cutover (Phase 4 Task 26): the binary embeds that file and the migrations
// after it, its manifest knows exactly those, and a history from before the
// baseline is never adopted.
func TestTheBaselineAndItsSuccessorsAreEmbedded(t *testing.T) {
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"000032_baseline.sql", "000033_immunizations.sql", "000034_pumping.sql", "000035_audit_retention.sql",
		"000036_request_authority_family_key.sql", "000037_family_delete_cascade.sql"}) {
		t.Fatalf("embedded migrations %v; want the baseline, 000033_immunizations.sql, 000034_pumping.sql, 000035_audit_retention.sql, "+
			"000036_request_authority_family_key.sql and 000037_family_delete_cascade.sql", names)
	}
	digestOf := func(version int64, name string) MigrationDigest {
		raw, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		return MigrationDigest{Version: version, Name: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	}
	c, err := EmbeddedCompatibility()
	if err != nil {
		t.Fatal(err)
	}
	baseline, immunizations := digestOf(32, "000032_baseline.sql"), digestOf(33, "000033_immunizations.sql")
	pumping, retention := digestOf(34, "000034_pumping.sql"), digestOf(35, "000035_audit_retention.sql")
	familyKey, cascade := digestOf(36, "000036_request_authority_family_key.sql"), digestOf(37, "000037_family_delete_cascade.sql")
	if baseline.SHA256 != "c157a7e3e8df3d98ce57ca557ca5282872eedd4355b35707db3ecc8cfe31da8b" {
		t.Fatalf("the frozen baseline changed: %s", baseline.SHA256)
	}
	if immunizations.SHA256 != "40380fddef9683b640b825ffc7e5afa2b990554f978afb8b26053e8e6bf59b82" {
		t.Fatalf("the frozen 000033 changed: %s", immunizations.SHA256)
	}
	if pumping.SHA256 != "47f5ed52e73a915db7cb07f2ce66dd9522bc87e74d3a0d35aa746402066f245d" {
		t.Fatalf("the frozen 000034 changed: %s", pumping.SHA256)
	}
	if retention.SHA256 != "d2f260885db3d56e6aa396482cddc6ca1fdea32d30097089bc5b6df23dad6c65" {
		t.Fatalf("the frozen 000035 changed: %s", retention.SHA256)
	}
	if familyKey.SHA256 != "8c116b63d0fef8bb623c5117d83084bea126f05b514efda5e68b367dea414a5d" {
		t.Fatalf("the frozen 000036 changed: %s", familyKey.SHA256)
	}
	if c.Min != 37 || c.Max != 37 || len(c.Known) != 6 || c.Known[0] != baseline || c.Known[1] != immunizations || c.Known[2] != pumping ||
		c.Known[3] != retention || c.Known[4] != familyKey || c.Known[5] != cascade {
		t.Fatalf("compatibility %+v; want min 37 max 37 known [%+v %+v %+v %+v %+v %+v]", c, baseline, immunizations, pumping, retention, familyKey, cascade)
	}
	loaded, err := New(migrations.Files).loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 6 || loaded[0].version != 32 || loaded[0].name != baseline.Name || loaded[0].checksum != baseline.SHA256 ||
		loaded[1].version != 33 || loaded[1].name != immunizations.Name || loaded[1].checksum != immunizations.SHA256 ||
		loaded[2].version != 34 || loaded[2].name != pumping.Name || loaded[2].checksum != pumping.SHA256 ||
		loaded[3].version != 35 || loaded[3].name != retention.Name || loaded[3].checksum != retention.SHA256 ||
		loaded[4].version != 36 || loaded[4].name != familyKey.Name || loaded[4].checksum != familyKey.SHA256 ||
		loaded[5].version != 37 || loaded[5].name != cascade.Name || loaded[5].checksum != cascade.SHA256 {
		t.Fatalf("runner loads %+v", loaded)
	}
	if err := ValidateCompatibility(c.Known, c); err != nil {
		t.Fatalf("the current install is refused: %v", err)
	}
	if err := ValidateCompatibility(c.Known[:5], c); err == nil {
		t.Fatal("a database still at schema 36 is accepted by the schema-37 binary")
	}

	// The chain's history, the homelab's legacy history: none is this binary's.
	chain := make([]MigrationDigest, 31)
	for i := range chain {
		chain[i] = MigrationDigest{Version: int64(i + 1), Name: fmt.Sprintf("%06d_chain.sql", i+1), SHA256: strings.Repeat("a", 64)}
	}
	for name, applied := range map[string][]MigrationDigest{
		"version 1 alone":                 {{Version: 1, Name: "000001_server_state.sql", SHA256: "e6bf0d5b545b295f1a03c78d607ce8edc8bc2772d49f58bcbe77edb757266085"}},
		"the chain through 31":            chain,
		"the chain through 31, then 32":   append(append([]MigrationDigest(nil), chain...), baseline),
		"the pre-squash 32":               {{Version: 32, Name: "000032_live_activity.sql", SHA256: "64cb3c3ab2a043a7138f4407cc865cfb148b4e0f680d47f44d8a8548a6f6716f"}},
		"the baseline with a changed sum": {{Version: 32, Name: baseline.Name, SHA256: strings.Repeat("0", 64)}},
	} {
		if err := ValidateCompatibility(applied, c); err == nil {
			t.Errorf("%s: accepted by the baseline binary", name)
		}
	}

	databaseURL := os.Getenv("BABY_TRACKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BABY_TRACKER_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	runner := New(migrations.Files)
	for name, rows := range map[string]string{
		"version 1":                        `(1, '000001_server_state.sql', 'e6bf0d5b545b295f1a03c78d607ce8edc8bc2772d49f58bcbe77edb757266085')`,
		"versions 1 and the baseline":      `(1, '000001_server_state.sql', 'e6bf0d5b545b295f1a03c78d607ce8edc8bc2772d49f58bcbe77edb757266085'), (32, '000032_baseline.sql', '` + baseline.SHA256 + `')`,
		"the homelab's legacy 29":          `(29, '000029_baby_icon.sql', '8a296b40a8a1aba5e8d4f0256a8bb2aa050c50c8ea30a6d9ae2d6cfd9e087288')`,
		"the pre-squash 32 as the top row": `(32, '000032_live_activity.sql', '64cb3c3ab2a043a7138f4407cc865cfb148b4e0f680d47f44d8a8548a6f6716f')`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS app CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
				t.Fatal(err)
			}
			if err := ensureTable(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name, sha256) VALUES `+rows); err != nil {
				t.Fatal(err)
			}
			// A pre-baseline row is a history this binary does not have, not a
			// newer database: migration_checksum_mismatch.
			if err := runner.Apply(ctx, pool); !errors.Is(err, ErrChecksumMismatch) {
				t.Fatalf("Apply over %s: %v", name, err)
			}
			if _, err := runner.Status(ctx, pool); !errors.Is(err, ErrChecksumMismatch) {
				t.Fatalf("Status over %s: %v", name, err)
			}
			if _, err := runner.CompatibleVersion(ctx, pool, c); err == nil {
				t.Fatalf("CompatibleVersion accepted %s", name)
			}
			var applied bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_namespace WHERE nspname = 'app')
				OR to_regclass('public.schema_compatibility') IS NOT NULL`).Scan(&applied); err != nil {
				t.Fatal(err)
			}
			if applied {
				t.Fatalf("the baseline ran over %s", name)
			}
		})
	}
}

// A squashed baseline may start the history; nothing else may start it at a
// version other than 1, and the history stays contiguous after it.
func TestTheHistoryStartsAtOneOrABaseline(t *testing.T) {
	digest := func(version int64, name string) MigrationDigest {
		return MigrationDigest{Version: version, Name: name, SHA256: strings.Repeat(fmt.Sprint(version%10), 64)}
	}
	baseline, next := digest(32, "000032_baseline.sql"), digest(33, "000033_next.sql")
	for name, c := range map[string]Compatibility{
		"the baseline":            {Min: 32, Max: 32, Known: []MigrationDigest{baseline}},
		"the baseline and 33":     {Min: 32, Max: 33, Known: []MigrationDigest{baseline, next}},
		"33 over the baseline":    {Min: 33, Max: 33, Known: []MigrationDigest{baseline, next}},
		"a chain from 1":          {Min: 1, Max: 1, Known: []MigrationDigest{digest(1, "000001_first.sql")}},
		"a baseline at version 1": {Min: 1, Max: 1, Known: []MigrationDigest{digest(1, "000001_baseline.sql")}},
	} {
		if err := validateCompatibilityCatalog(c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]Compatibility{
		"a non-baseline start at 32": {Min: 32, Max: 32, Known: []MigrationDigest{digest(32, "000032_live_activity.sql")}},
		"a gap after the baseline":   {Min: 32, Max: 34, Known: []MigrationDigest{baseline, digest(34, "000034_next.sql")}},
		"min below the baseline":     {Min: 31, Max: 32, Known: []MigrationDigest{baseline}},
		"max above the history":      {Min: 32, Max: 33, Known: []MigrationDigest{baseline}},
		"nothing known":              {Min: 32, Max: 32},
		"a misnamed baseline":        {Min: 32, Max: 32, Known: []MigrationDigest{digest(32, "000032_baseline_v2.sql")}},
	} {
		if err := validateCompatibilityCatalog(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A binary with 33 over the baseline runs on 32 and 33; a 32 database
	// still lacks 33's schema when min is 33.
	both := Compatibility{Min: 32, Max: 33, Known: []MigrationDigest{baseline, next}}
	if err := ValidateCompatibility([]MigrationDigest{baseline}, both); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompatibility([]MigrationDigest{baseline, next}, both); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompatibility([]MigrationDigest{next}, both); err == nil {
		t.Fatal("33 without the baseline was accepted")
	}
	if err := ValidateCompatibility([]MigrationDigest{baseline}, Compatibility{Min: 33, Max: 33, Known: both.Known}); err == nil {
		t.Fatal("a min-33 binary accepted the baseline alone")
	}

	sql := func(text string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(text)} }
	for name, files := range map[string]fstest.MapFS{
		"the baseline":        {"000032_baseline.sql": sql("SELECT 32;")},
		"the baseline and 33": {"000032_baseline.sql": sql("SELECT 32;"), "000033_next.sql": sql("SELECT 33;")},
	} {
		if _, err := New(files).loadMigrations(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, files := range map[string]fstest.MapFS{
		"a non-baseline start at 32":    {"000032_live_activity.sql": sql("SELECT 32;")},
		"a gap after the baseline":      {"000032_baseline.sql": sql("SELECT 32;"), "000034_next.sql": sql("SELECT 34;")},
		"a chain below the baseline":    {"000031_chain.sql": sql("SELECT 31;"), "000032_baseline.sql": sql("SELECT 32;")},
		"a gap before a later baseline": {"000001_first.sql": sql("SELECT 1;"), "000032_baseline.sql": sql("SELECT 32;")},
	} {
		if _, err := New(files).loadMigrations(); err == nil || !strings.Contains(err.Error(), "migration_gap") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
