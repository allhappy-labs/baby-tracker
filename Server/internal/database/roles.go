package database

import (
	"context"
	"errors"
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

// VerifyRuntimeRole checks that the runtime pool's login role holds exactly
// the privileges the schema privilege manifest records for
// baby_tracker_runtime at compatibleVersion, and nothing else.
func VerifyRuntimeRole(ctx context.Context, runtime *pgxpool.Pool, compatibleVersion int64) error {
	if runtime == nil || compatibleVersion < 4 {
		return errors.New("runtime pool is required")
	}
	tx, err := runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Unavailable(err)
	}
	defer tx.Rollback(ctx)
	if _, err := verifyRoleSnapshot(ctx, tx, runtimeInventory.group, compatibleVersion); err != nil {
		return err
	}
	if err := verifyNoPresetAuthority(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return Unavailable(err)
	}
	return nil
}

var maintenanceInventory = roleInventory{
	group: "baby_tracker_maintenance",
}

// VerifyMaintenanceRole checks that the maintenance pool's login holds
// exactly the privileges manifest version compatibleVersion records for
// baby_tracker_maintenance. The maintenance login cannot read the manifest,
// so the manifest is read through manifest (a runtime pool) and the
// privileges are compared on the maintenance connection.
func VerifyMaintenanceRole(ctx context.Context, manifest, maintenance *pgxpool.Pool, compatibleVersion int64) error {
	if manifest == nil || maintenance == nil || compatibleVersion < 19 {
		return errors.New("manifest and maintenance pools are required")
	}
	tx, err := manifest.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Unavailable(err)
	}
	defer tx.Rollback(ctx)
	var observedVersion int64
	if err := tx.QueryRow(ctx, "SELECT current_version FROM schema_compatibility WHERE singleton").Scan(&observedVersion); err != nil || observedVersion != compatibleVersion {
		return errors.New("database_role_separation_invalid")
	}
	allowed, err := loadRoleInventory(ctx, tx, maintenanceInventory.group, compatibleVersion)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return Unavailable(err)
	}
	check, err := maintenance.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Unavailable(err)
	}
	defer check.Rollback(ctx)
	if _, err := verifyRoleInventory(ctx, check, allowed); err != nil {
		return err
	}
	if err := verifyNoPresetAuthority(ctx, check); err != nil {
		return err
	}
	if err := check.Commit(ctx); err != nil {
		return Unavailable(err)
	}
	return nil
}

// verifyNoPresetAuthority rejects a runtime whose sessions start with
// app.authority already set: through connection options (visible as the
// current value at the start of a transaction) or through a database, role or
// role-in-database default in pg_db_role_setting. Either would turn every
// runtime transaction into an authority transaction.
func verifyNoPresetAuthority(ctx context.Context, tx pgx.Tx) error {
	var preset bool
	if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.authority', true), '') <> ''
		OR EXISTS (
		  SELECT 1 FROM pg_db_role_setting s CROSS JOIN LATERAL unnest(s.setconfig) AS setting(value)
		  WHERE (s.setdatabase=0 OR s.setdatabase=(SELECT oid FROM pg_database WHERE datname=current_database()))
		    AND (s.setrole=0 OR pg_has_role(current_user, s.setrole, 'MEMBER'))
		    AND lower(split_part(setting.value, '=', 1))='app.authority')`).Scan(&preset); err != nil {
		return Unavailable(err)
	}
	if preset {
		return errors.New("database_role_separation_invalid")
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
		return roleInventory{}, Unavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, identity, privileges string
		if err := rows.Scan(&kind, &identity, &privileges); err != nil {
			return roleInventory{}, Unavailable(err)
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
		return roleInventory{}, Unavailable(err)
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
		return "", Unavailable(err)
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
		return Unavailable(err)
	}
	defer rows.Close()
	remaining := cloneInventory(expected)
	for rows.Next() {
		var name string
		var observed T
		if err := rows.Scan(&name, &observed); err != nil {
			return Unavailable(err)
		}
		if observed != expected[name] {
			return errors.New("database_role_separation_invalid")
		}
		delete(remaining, name)
	}
	if err := rows.Err(); err != nil {
		return Unavailable(err)
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
