package database

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type roleInventory struct {
	group     string
	tables    map[string]string
	schemas   map[string]string
	sequences map[string]string
	columns   map[string]string
	functions map[string]bool
}

var runtimeInventory = roleInventory{
	group: "baby_tracker_runtime",
}

var projectorInventory = roleInventory{
	group: "baby_tracker_projector",
}

func VerifyRoleSeparation(ctx context.Context, runtime, projector *pgxpool.Pool, compatibleVersion int64) error {
	if runtime == nil || projector == nil || runtime == projector || compatibleVersion < 4 {
		return errors.New("distinct runtime and projector pools are required")
	}
	runtimeTx, err := runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ErrUnavailable
	}
	defer runtimeTx.Rollback(ctx)
	var snapshot string
	if err := runtimeTx.QueryRow(ctx, "SELECT pg_export_snapshot()").Scan(&snapshot); err != nil || !snapshotID.MatchString(snapshot) {
		return errors.New("database_role_separation_invalid")
	}
	projectorTx, err := projector.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ErrUnavailable
	}
	defer projectorTx.Rollback(ctx)
	if _, err := projectorTx.Exec(ctx, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", snapshot)); err != nil {
		return errors.New("database_role_separation_invalid")
	}
	runtimeUser, err := verifyRoleSnapshot(ctx, runtimeTx, runtimeInventory.group, compatibleVersion)
	if err != nil {
		return err
	}
	projectorUser, err := verifyRoleSnapshot(ctx, projectorTx, projectorInventory.group, compatibleVersion)
	if err != nil {
		return err
	}
	if runtimeUser == projectorUser {
		return errors.New("database_role_separation_invalid")
	}
	if err := projectorTx.Commit(ctx); err != nil {
		return ErrUnavailable
	}
	if err := runtimeTx.Commit(ctx); err != nil {
		return ErrUnavailable
	}
	return nil
}

var snapshotID = regexp.MustCompile(`^[0-9A-Fa-f]+-[0-9A-Fa-f]+-[0-9A-Fa-f]+$`)

type roleQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func verifyRoleSnapshot(ctx context.Context, tx pgx.Tx, group string, compatibleVersion int64) (string, error) {
	var observedVersion int64
	if err := tx.QueryRow(ctx, "SELECT current_version FROM schema_compatibility WHERE singleton").Scan(&observedVersion); err != nil || observedVersion != compatibleVersion {
		return "", errors.New("database_role_separation_invalid")
	}
	allowed, err := loadRoleInventory(ctx, tx, group, compatibleVersion)
	if err != nil {
		return "", err
	}
	user, err := verifyRoleInventory(ctx, tx, allowed)
	if err != nil {
		return "", err
	}
	return user, nil
}

func loadRoleInventory(ctx context.Context, query roleQuerier, group string, compatibleVersion int64) (roleInventory, error) {
	allowed := roleInventory{group: group, tables: map[string]string{}, schemas: map[string]string{}, sequences: map[string]string{}, columns: map[string]string{}, functions: map[string]bool{}}
	rows, err := query.Query(ctx, `SELECT object_kind,object_identity,privileges
		FROM schema_privilege_manifest
		WHERE principal=$1 AND schema_version=$2
		ORDER BY object_kind,object_identity`, group, compatibleVersion)
	if err != nil {
		return roleInventory{}, ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var kind, identity, privileges string
		if err := rows.Scan(&kind, &identity, &privileges); err != nil {
			return roleInventory{}, ErrUnavailable
		}
		switch kind {
		case "table":
			if !canonicalPrivileges(privileges, []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"}) {
				return roleInventory{}, errors.New("database_role_separation_invalid")
			}
			allowed.tables[identity] = privileges
		case "schema":
			if !canonicalPrivileges(privileges, []string{"USAGE", "CREATE"}) {
				return roleInventory{}, errors.New("database_role_separation_invalid")
			}
			allowed.schemas[identity] = privileges
		case "sequence":
			if !canonicalPrivileges(privileges, []string{"SELECT", "UPDATE", "USAGE"}) {
				return roleInventory{}, errors.New("database_role_separation_invalid")
			}
			allowed.sequences[identity] = privileges
		case "column":
			if !canonicalPrivileges(privileges, []string{"SELECT", "INSERT", "UPDATE", "REFERENCES"}) {
				return roleInventory{}, errors.New("database_role_separation_invalid")
			}
			allowed.columns[identity] = privileges
		case "function":
			if privileges != "EXECUTE" {
				return roleInventory{}, errors.New("database_role_separation_invalid")
			}
			allowed.functions[identity] = true
		default:
			return roleInventory{}, errors.New("database_role_separation_invalid")
		}
	}
	if err := rows.Err(); err != nil {
		return roleInventory{}, ErrUnavailable
	}
	return allowed, nil
}

func canonicalPrivileges(privileges string, order []string) bool {
	parts := strings.Split(privileges, ",")
	seen := make(map[string]bool, len(parts))
	for _, privilege := range parts {
		if seen[privilege] {
			return false
		}
		seen[privilege] = true
	}
	var canonical []string
	for _, privilege := range order {
		if seen[privilege] {
			canonical = append(canonical, privilege)
			delete(seen, privilege)
		}
	}
	return len(seen) == 0 && len(canonical) != 0 && strings.Join(canonical, ",") == privileges
}

func verifyRoleInventory(ctx context.Context, pool roleQuerier, allowed roleInventory) (string, error) {
	var user string
	var connect, create, temporary, safeRole, exactMembership, safeGroup, ownsObjects bool
	if err := pool.QueryRow(ctx, `SELECT current_user,
        has_database_privilege(current_user,current_database(),'CONNECT'),
        has_database_privilege(current_user,current_database(),'CREATE'),
		has_database_privilege(current_user,current_database(),'TEMPORARY'),
		r.rolcanlogin AND r.rolinherit AND NOT (r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls)
		  AND r.rolconfig IS NULL AND (r.rolvaliduntil IS NULL OR r.rolvaliduntil='infinity'::timestamptz),
		(SELECT count(*)=1 AND bool_and(parent.rolname=$1 AND NOT m.admin_option AND m.inherit_option AND m.set_option)
		 FROM pg_auth_members m JOIN pg_roles parent ON parent.oid=m.roleid WHERE m.member=r.oid),
		NOT g.rolcanlogin AND g.rolinherit AND NOT (g.rolsuper OR g.rolcreatedb OR g.rolcreaterole OR g.rolreplication OR g.rolbypassrls)
		  AND g.rolconfig IS NULL AND (g.rolvaliduntil IS NULL OR g.rolvaliduntil='infinity'::timestamptz)
		  AND NOT EXISTS (SELECT 1 FROM pg_auth_members outgoing WHERE outgoing.member=g.oid)
		  AND (SELECT count(*)=1 AND bool_and(member.rolname=current_user AND NOT incoming.admin_option AND incoming.inherit_option AND incoming.set_option)
		       FROM pg_auth_members incoming JOIN pg_roles member ON member.oid=incoming.member WHERE incoming.roleid=g.oid),
		EXISTS (
		  SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND pg_has_role(current_user,c.relowner,'USAGE')
		  UNION ALL SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND pg_has_role(current_user,p.proowner,'USAGE')
		  UNION ALL SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND pg_has_role(current_user,t.typowner,'USAGE')
		  UNION ALL SELECT 1 FROM pg_namespace n WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND pg_has_role(current_user,n.nspowner,'USAGE'))
	  FROM pg_roles r JOIN pg_roles g ON g.rolname=$1 WHERE r.rolname=current_user`, allowed.group).Scan(&user, &connect, &create, &temporary, &safeRole, &exactMembership, &safeGroup, &ownsObjects); err != nil {
		return "", ErrUnavailable
	}
	if !connect || create || temporary || !safeRole || !exactMembership || !safeGroup || ownsObjects {
		return "", errors.New("database_role_separation_invalid")
	}
	if err := verifyExactInventory(ctx, pool, allowed.tables, `SELECT n.nspname || '.' || c.relname,
        concat_ws(',', CASE WHEN has_table_privilege(current_user,c.oid,'SELECT') THEN 'SELECT' END,
          CASE WHEN has_table_privilege(current_user,c.oid,'INSERT') THEN 'INSERT' END,
          CASE WHEN has_table_privilege(current_user,c.oid,'UPDATE') THEN 'UPDATE' END,
          CASE WHEN has_table_privilege(current_user,c.oid,'DELETE') THEN 'DELETE' END,
          CASE WHEN has_table_privilege(current_user,c.oid,'TRUNCATE') THEN 'TRUNCATE' END,
          CASE WHEN has_table_privilege(current_user,c.oid,'REFERENCES') THEN 'REFERENCES' END,
          CASE WHEN has_table_privilege(current_user,c.oid,'TRIGGER') THEN 'TRIGGER' END,
          CASE WHEN has_table_privilege(current_user,c.oid,'MAINTAIN') THEN 'MAINTAIN' END)
      FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
      WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
		AND c.relpersistence <> 't' AND c.relkind IN ('r','p','v','m','f')`); err != nil {
		return "", err
	}
	if err := verifyExactInventory(ctx, pool, allowed.columns, `SELECT n.nspname || '.' || c.relname || '.' || a.attname,
		concat_ws(',',
		  CASE WHEN NOT has_table_privilege(current_user,c.oid,'SELECT') AND has_column_privilege(current_user,c.oid,a.attnum,'SELECT') THEN 'SELECT' END,
		  CASE WHEN NOT has_table_privilege(current_user,c.oid,'INSERT') AND has_column_privilege(current_user,c.oid,a.attnum,'INSERT') THEN 'INSERT' END,
		  CASE WHEN NOT has_table_privilege(current_user,c.oid,'UPDATE') AND has_column_privilege(current_user,c.oid,a.attnum,'UPDATE') THEN 'UPDATE' END,
		  CASE WHEN NOT has_table_privilege(current_user,c.oid,'REFERENCES') AND has_column_privilege(current_user,c.oid,a.attnum,'REFERENCES') THEN 'REFERENCES' END)
		FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relpersistence <> 't' AND c.relkind IN ('r','p','v','m','f')
		  AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`); err != nil {
		return "", err
	}
	if err := verifyExactInventory(ctx, pool, allowed.sequences, `SELECT n.nspname || '.' || c.relname,
		concat_ws(',', CASE WHEN has_sequence_privilege(current_user,c.oid,'SELECT') THEN 'SELECT' END,
		  CASE WHEN has_sequence_privilege(current_user,c.oid,'UPDATE') THEN 'UPDATE' END,
		  CASE WHEN has_sequence_privilege(current_user,c.oid,'USAGE') THEN 'USAGE' END)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind='S' AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`); err != nil {
		return "", err
	}
	if err := verifyExactInventory(ctx, pool, allowed.schemas, `SELECT nspname, concat_ws(',',
        CASE WHEN has_schema_privilege(current_user,oid,'USAGE') THEN 'USAGE' END,
        CASE WHEN has_schema_privilege(current_user,oid,'CREATE') THEN 'CREATE' END)
		FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema'`); err != nil {
		return "", err
	}
	if err := verifyExactInventory(ctx, pool, allowed.functions, `SELECT p.oid::regprocedure::text, has_function_privilege(current_user,p.oid,'EXECUTE')
      FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`); err != nil {
		return "", err
	}
	return user, nil
}

func verifyExactInventory[T comparable](ctx context.Context, query roleQuerier, expected map[string]T, statement string) error {
	rows, err := query.Query(ctx, statement)
	if err != nil {
		return ErrUnavailable
	}
	defer rows.Close()
	remaining := cloneInventory(expected)
	for rows.Next() {
		var name string
		var observed T
		if err := rows.Scan(&name, &observed); err != nil {
			return ErrUnavailable
		}
		if observed != expected[name] {
			return errors.New("database_role_separation_invalid")
		}
		delete(remaining, name)
	}
	if err := rows.Err(); err != nil {
		return ErrUnavailable
	}
	if len(remaining) != 0 {
		return errors.New("database_role_separation_invalid")
	}
	return nil
}

func cloneInventory[T any](source map[string]T) map[string]T {
	clone := make(map[string]T, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// VerifyLiveActivityRole verifies the dedicated worker's exact EXECUTE and metadata-only inventory.
func VerifyLiveActivityRole(ctx context.Context, pool *pgxpool.Pool, version int64) error {
	if pool == nil || version < 28 {
		return errors.New("database_role_separation_invalid")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ErrUnavailable
	}
	defer tx.Rollback(ctx)
	if _, err = verifyRoleSnapshot(ctx, tx, "baby_tracker_live_activity", version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// VerifyLiveActivityRoleSeparation proves the worker shares runtime database
// state, then verifies its exact narrow privileges in that same snapshot.
// Importing a snapshot fails across either a database or a PostgreSQL cluster.
func VerifyLiveActivityRoleSeparation(ctx context.Context, runtime, worker *pgxpool.Pool, version int64) error {
	if runtime == nil || worker == nil || runtime == worker || version < 28 {
		return errors.New("database_role_separation_invalid")
	}
	runtimeTx, err := runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ErrUnavailable
	}
	defer runtimeTx.Rollback(ctx)
	var snapshot string
	if err := runtimeTx.QueryRow(ctx, "SELECT pg_export_snapshot()").Scan(&snapshot); err != nil || !snapshotID.MatchString(snapshot) {
		return errors.New("database_role_separation_invalid")
	}
	workerTx, err := worker.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ErrUnavailable
	}
	defer workerTx.Rollback(ctx)
	if _, err := workerTx.Exec(ctx, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", snapshot)); err != nil {
		return errors.New("database_role_separation_invalid")
	}
	if _, err := verifyRoleSnapshot(ctx, workerTx, "baby_tracker_live_activity", version); err != nil {
		return err
	}
	if err := workerTx.Commit(ctx); err != nil {
		return ErrUnavailable
	}
	if err := runtimeTx.Commit(ctx); err != nil {
		return ErrUnavailable
	}
	return nil
}
