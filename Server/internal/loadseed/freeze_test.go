//go:build identitytest

package loadseed

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The care stage writes every care_entities partition frozen: each page
// all-visible and all-frozen in the visibility map, so the readers and
// the vacuum that follow rewrite none of them.
func TestCareIsCopiedFrozen(t *testing.T) {
	env := newSeedEnvironment(t)
	spec := Small(4, 1, time.Now().UTC().Truncate(time.Second))
	s, err := compose(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	families, err := s.provisionAll(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	cares, err := loadCare(t.Context(), env, spec, families, s.server)
	if err != nil {
		t.Fatal(err)
	}
	if err := cares.waitBabyKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Superuser.Exec(t.Context(), `CREATE EXTENSION IF NOT EXISTS pg_visibility`); err != nil {
		t.Fatal(err)
	}
	pages := 0
	for p := range partitionCount {
		table := fmt.Sprintf("care_entities_p%02d", p)
		var blocks, visible, frozen int
		if err := env.Superuser.QueryRow(t.Context(), `SELECT pg_relation_size($1::regclass) / current_setting('block_size')::int,
			v.all_visible, v.all_frozen FROM pg_visibility_map_summary($1::regclass) v`, table).Scan(&blocks, &visible, &frozen); err != nil {
			t.Fatal(err)
		}
		if visible != blocks || frozen != blocks {
			t.Fatalf("%s: %d pages, %d all-visible, %d all-frozen", table, blocks, visible, frozen)
		}
		pages += blocks
	}
	if pages == 0 {
		t.Fatal("the care stage wrote no care pages")
	}
}

// copyFrozen truncates only an empty table: one that holds a row is
// refused and keeps it.
func TestCopyFrozenRefusesATableWithRows(t *testing.T) {
	env := newSeedEnvironment(t)
	exec := func(sql string) {
		t.Helper()
		if _, err := env.Superuser.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE public.freeze_probe (id int PRIMARY KEY, label text)`)
	exec(`INSERT INTO public.freeze_probe VALUES (1, 'kept')`)
	rows := [][]any{{2, "new"}}
	_, err := copyFrozen(t.Context(), env.Superuser, "freeze_probe", []string{"id", "label"}, pgx.CopyFromRows(rows))
	if err == nil || !strings.Contains(err.Error(), "holds rows") {
		t.Fatalf("copyFrozen into a table with rows: %v", err)
	}
	var labels []string
	r, err := env.Superuser.Query(t.Context(), `SELECT label FROM public.freeze_probe ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if labels, err = pgx.CollectRows(r, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if strings.Join(labels, ",") != "kept" {
		t.Fatalf("the table holds %v after a refused load", labels)
	}
	// Empty, it loads (NULLs and text values included).
	exec(`DELETE FROM public.freeze_probe`)
	rows = [][]any{{1, "one"}, {2, nil}}
	if n, err := copyFrozen(t.Context(), env.Superuser, "freeze_probe", []string{"id", "label"}, pgx.CopyFromRows(rows)); err != nil || n != 2 {
		t.Fatalf("copyFrozen into an empty table: %d rows, %v", n, err)
	}
	var loaded string
	if err := env.Superuser.QueryRow(t.Context(), `SELECT string_agg(id || '=' || coalesce(label, 'NULL'), ',' ORDER BY id) FROM public.freeze_probe`).
		Scan(&loaded); err != nil || loaded != "1=one,2=NULL" {
		t.Fatalf("loaded %q, %v", loaded, err)
	}
}
