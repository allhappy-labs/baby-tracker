//go:build identitytest

package loadseed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A bulk load keeps indexes and keys out of the way while it copies and
// puts them back afterwards. An index built from the loaded rows is
// written page by page once, where COPY into an indexed table writes a WAL
// record per row and index (and, once a checkpoint has passed, a full page
// image of every randomly hit index page); a key validated once by a join
// replaces a trigger, a query and a row lock per copied row.
//
// What comes back is the object the migration made: its catalog
// definition is read before the drop (pg_get_indexdef,
// pg_get_constraintdef) and executed as read, and the restore checks what
// the migration's object checks (a unique index is built over the rows, a
// primary key or unique constraint is added on it, a foreign key is added
// validated). TestDeferredSchemaIsRestoredExactly compares the catalog
// before and after.

// deferredIndex is an index a load drops and builds again. A partitioned
// index carries its partitions' indexes: the parent is recreated ON ONLY
// its table (invalid until every partition's index is attached), each
// partition's index is built under its own name and then attached, which
// makes the parent valid again.
type deferredIndex struct {
	name, definition, table string
	partitions              []deferredIndex
}

// deferredConstraint is a constraint a load drops and adds back. A foreign
// key is added validated, under the planner settings its validation runs
// with (SET LOCAL); a primary key or unique constraint (index set) has its
// index built with the other indexes and is then added on it (ADD
// CONSTRAINT … USING INDEX, the index carrying the constraint's name).
type deferredConstraint struct {
	table, name, definition string
	settings                []string
	index                   *deferredIndex
}

// deferredSchema is what one load drops and restores.
type deferredSchema struct {
	indexes     []deferredIndex
	constraints []deferredConstraint
}

// deferral names what a load defers: the secondary indexes of tables (every
// index not backing a constraint), single indexes by name, and constraints
// (table and name; foreign keys, primary keys and unique constraints).
type deferral struct {
	secondaryIndexesOf []string
	indexes            []string
	constraints        []deferredConstraint
}

// recordDeferral reads the definitions of what d names.
func recordDeferral(ctx context.Context, pool *pgxpool.Pool, d deferral) (deferredSchema, error) {
	var out deferredSchema
	names := append([]string(nil), d.indexes...)
	for _, table := range d.secondaryIndexesOf {
		rows, err := pool.Query(ctx, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE i.indrelid = $1::regclass AND NOT EXISTS (SELECT FROM pg_constraint k WHERE k.conindid = i.indexrelid)
			ORDER BY c.relname`, table)
		if err != nil {
			return out, fmt.Errorf("loadseed: the secondary indexes of %s: %w", table, err)
		}
		secondary, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return out, fmt.Errorf("loadseed: the secondary indexes of %s: %w", table, err)
		}
		names = append(names, secondary...)
	}
	for _, name := range names {
		index, err := recordIndex(ctx, pool, name)
		if err != nil {
			return out, err
		}
		out.indexes = append(out.indexes, index)
	}
	for _, c := range d.constraints {
		var kind, index, indexName string
		if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(k.oid), k.contype::text, coalesce(format('%I.%I', n.nspname, i.relname), ''), coalesce(i.relname::text, '')
			FROM pg_constraint k LEFT JOIN pg_class i ON i.oid = k.conindid LEFT JOIN pg_namespace n ON n.oid = i.relnamespace
			WHERE k.conname = $1 AND k.conrelid = $2::regclass`, c.name, c.table).Scan(&c.definition, &kind, &index, &indexName); err != nil {
			return out, fmt.Errorf("loadseed: read %s: %w", c.name, err)
		}
		switch kind {
		case "f":
		case "p", "u":
			recorded, err := recordIndex(ctx, pool, index)
			if err != nil {
				return out, err
			}
			if len(recorded.partitions) > 0 || indexName != c.name {
				return out, fmt.Errorf("loadseed: %s is on a partitioned table or an index of another name (%s)", c.name, index)
			}
			c.index = &recorded
		default:
			return out, fmt.Errorf("loadseed: %s is a constraint of type %s, which a load does not defer", c.name, kind)
		}
		out.constraints = append(out.constraints, c)
	}
	return out, nil
}

// recordIndex reads an index's definition and, for a partitioned index,
// its partitions' (one level: the seeded tables are partitioned once).
func recordIndex(ctx context.Context, pool *pgxpool.Pool, name string) (deferredIndex, error) {
	index := deferredIndex{name: name}
	var kind string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(c.oid), c.relkind::text, format('%I.%I', n.nspname, t.relname)
		FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid JOIN pg_class t ON t.oid = i.indrelid JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE c.oid = $1::regclass`, name).Scan(&index.definition, &kind, &index.table); err != nil {
		return index, fmt.Errorf("loadseed: read index %s: %w", name, err)
	}
	switch kind {
	case "i":
		return index, nil
	case "I":
	default:
		return index, fmt.Errorf("loadseed: %s is not an index (relkind %s)", name, kind)
	}
	rows, err := pool.Query(ctx, `SELECT format('%I.%I', n.nspname, c.relname), pg_get_indexdef(c.oid), c.relkind::text,
			format('%I.%I', tn.nspname, t.relname)
		FROM pg_inherits h JOIN pg_class c ON c.oid = h.inhrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_index i ON i.indexrelid = c.oid JOIN pg_class t ON t.oid = i.indrelid JOIN pg_namespace tn ON tn.oid = t.relnamespace
		WHERE h.inhparent = $1::regclass ORDER BY c.relname`, name)
	if err != nil {
		return index, fmt.Errorf("loadseed: read the partitions of index %s: %w", name, err)
	}
	var child deferredIndex
	if _, err := pgx.ForEachRow(rows, []any{&child.name, &child.definition, &kind, &child.table}, func() error {
		if kind != "i" {
			return fmt.Errorf("loadseed: index %s has a partition index %s of relkind %s", name, child.name, kind)
		}
		index.partitions = append(index.partitions, child)
		return nil
	}); err != nil {
		return index, fmt.Errorf("loadseed: read the partitions of index %s: %w", name, err)
	}
	return index, nil
}

// drop drops every deferred constraint and index in one transaction:
// foreign keys first (one may reference a deferred unique constraint),
// then primary keys and unique constraints (their indexes go with them),
// then indexes (a partitioned index takes its partitions' indexes). The
// commit is not cancelled with ctx: a cancel that landed during it could
// leave the drop committed while the caller, seeing the error, registers
// no restore.
func (s deferredSchema) drop(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, keys := range []bool{false, true} {
		for _, c := range s.constraints {
			if (c.index != nil) != keys {
				continue
			}
			if _, err := tx.Exec(ctx, `ALTER TABLE `+c.table+` DROP CONSTRAINT `+pgx.Identifier{c.name}.Sanitize()); err != nil {
				return fmt.Errorf("loadseed: drop %s: %w", c.name, err)
			}
		}
	}
	for _, index := range s.indexes {
		if _, err := tx.Exec(ctx, `DROP INDEX `+index.name); err != nil {
			return fmt.Errorf("loadseed: drop index %s: %w", index.name, err)
		}
	}
	return tx.Commit(context.WithoutCancel(ctx))
}

// restore puts back whatever of s is missing: partitioned parents ON ONLY
// their tables; every leaf index, and every deferred primary key's and
// unique constraint's index, built (workers at a time); partition indexes
// attached; primary keys and unique constraints added on their indexes;
// then the foreign keys added validated (a table's one after another,
// tables at once). It skips what exists, so it can finish a restore that
// failed part way.
func (s deferredSchema) restore(ctx context.Context, pool *pgxpool.Pool, workers int) error {
	indexExists := func(name string) (bool, error) {
		var ok bool
		err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok)
		return ok, err
	}
	constraintExists := func(c deferredConstraint) (bool, error) {
		var ok bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_constraint WHERE conname = $1 AND conrelid = $2::regclass)`, c.name, c.table).Scan(&ok)
		return ok, err
	}
	var builds []deferredIndex
	for _, index := range s.indexes {
		ok, err := indexExists(index.name)
		if err != nil {
			return err
		}
		if !ok && len(index.partitions) > 0 {
			if _, err := pool.Exec(ctx, index.definition); err != nil {
				return fmt.Errorf("loadseed: recreate index %s: %w", index.name, err)
			}
		} else if !ok {
			builds = append(builds, index)
		}
		for _, child := range index.partitions {
			ok, err := indexExists(child.name)
			if err != nil {
				return err
			}
			if !ok {
				builds = append(builds, child)
			}
		}
	}
	var keys, foreign []deferredConstraint
	for _, c := range s.constraints {
		if c.index == nil {
			foreign = append(foreign, c)
			continue
		}
		keys = append(keys, c)
		present, err := constraintExists(c)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		ok, err := indexExists(c.index.name)
		if err != nil {
			return err
		}
		if !ok {
			builds = append(builds, *c.index)
		}
	}
	// One table's indexes after another: the builds running at once scan
	// the same table, which is read once for them (synchronized scans)
	// rather than once per index.
	slices.SortStableFunc(builds, func(a, b deferredIndex) int { return strings.Compare(a.table, b.table) })
	if err := forEach(ctx, workers, len(builds), func(i int) error {
		if _, err := pool.Exec(ctx, builds[i].definition); err != nil {
			return fmt.Errorf("loadseed: build index %s: %w", builds[i].name, err)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, index := range s.indexes {
		for _, child := range index.partitions {
			var attached bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_inherits WHERE inhrelid = $1::regclass AND inhparent = $2::regclass)`,
				child.name, index.name).Scan(&attached); err != nil {
				return err
			}
			if attached {
				continue
			}
			if _, err := pool.Exec(ctx, `ALTER INDEX `+index.name+` ATTACH PARTITION `+child.name); err != nil {
				return fmt.Errorf("loadseed: attach index %s to %s: %w", child.name, index.name, err)
			}
		}
	}
	for _, c := range keys {
		present, err := constraintExists(c)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		kind := "UNIQUE"
		if strings.HasPrefix(c.definition, "PRIMARY KEY") {
			kind = "PRIMARY KEY"
		}
		if _, err := pool.Exec(ctx, `ALTER TABLE `+c.table+` ADD CONSTRAINT `+pgx.Identifier{c.name}.Sanitize()+` `+kind+` USING INDEX `+
			pgx.Identifier{c.name}.Sanitize()); err != nil {
			return fmt.Errorf("loadseed: re-add %s: %w", c.name, err)
		}
	}
	byTable := map[string][]deferredConstraint{}
	var tables []string
	for _, c := range foreign {
		if _, ok := byTable[c.table]; !ok {
			tables = append(tables, c.table)
		}
		byTable[c.table] = append(byTable[c.table], c)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(tables))
	for t, table := range tables {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, c := range byTable[table] {
				present, err := constraintExists(c)
				if err != nil {
					errs[t] = err
					return
				}
				if present {
					continue
				}
				if err := addConstraint(ctx, pool, c); err != nil {
					errs[t] = fmt.Errorf("loadseed: re-add %s: %w", c.name, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// addConstraint adds the foreign key c back under its recorded definition,
// validated (a plain ADD CONSTRAINT, never NOT VALID), with its settings
// for that transaction only.
func addConstraint(ctx context.Context, pool *pgxpool.Pool, c deferredConstraint) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, setting := range c.settings {
		if _, err := tx.Exec(ctx, `SET LOCAL `+setting); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE `+c.table+` ADD CONSTRAINT `+pgx.Identifier{c.name}.Sanitize()+` `+c.definition); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// split is s without the constraint name, and that constraint alone.
func (s deferredSchema) split(name string) (rest, one deferredSchema) {
	rest.indexes = s.indexes
	for _, c := range s.constraints {
		if c.name == name {
			one.constraints = append(one.constraints, c)
		} else {
			rest.constraints = append(rest.constraints, c)
		}
	}
	return rest, one
}

// String names what s holds, for logs.
func (s deferredSchema) String() string {
	var names []string
	for _, index := range s.indexes {
		names = append(names, index.name)
	}
	for _, c := range s.constraints {
		names = append(names, c.name)
	}
	return strings.Join(names, ", ")
}
