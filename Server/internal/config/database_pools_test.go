package config

import (
	"strings"
	"testing"
)

// Catches silent use of legacy defaults after the operator supplies a partial profile.
func TestLoadRejectsPartialDatabasePools(t *testing.T) {
	values := validHostedEnvironment(t)
	values["BT_DB_RUNTIME_MAX_CONNS"] = "16"
	if _, err := Load(mapGetter(values)); err == nil {
		t.Fatal("partial explicit pool profile accepted")
	}
}

func laboratoryPools() map[string]string {
	return map[string]string{
		"BT_DB_RUNTIME_MAX_CONNS": "16", "BT_DB_MAINTENANCE_MAX_CONNS": "2", "BT_DB_MCP_MAX_CONNS": "0", "BT_DB_SERVER_MAX_CONNECTIONS": "64", "BT_DB_RESERVED_CONNECTIONS": "8",
	}
}
func TestDatabasePoolsAtomicProfile(t *testing.T) {
	if _, err := LoadDatabasePools(nil); err == nil {
		t.Fatal("nil getter accepted")
	}
	pools, err := LoadDatabasePools(mapGetter(nil))
	if err != nil || pools != (DatabasePools{}) {
		t.Fatalf("legacy=%+v %v", pools, err)
	}
	pools, err = LoadDatabasePools(mapGetter(laboratoryPools()))
	if err != nil || pools != (DatabasePools{16, 2, 0, 64, 8}) {
		t.Fatalf("explicit=%+v %v", pools, err)
	}
	for _, key := range []string{"BT_DB_RUNTIME_MAX_CONNS", "BT_DB_MAINTENANCE_MAX_CONNS", "BT_DB_MCP_MAX_CONNS", "BT_DB_SERVER_MAX_CONNECTIONS", "BT_DB_RESERVED_CONNECTIONS"} {
		for _, value := range []string{"", " ", "-1", "2147483648", "1.5"} {
			t.Run(key+value, func(t *testing.T) {
				env := laboratoryPools()
				env[key] = value
				if _, err := LoadDatabasePools(mapGetter(env)); err == nil {
					t.Fatal("invalid or partial explicit profile accepted")
				}
			})
		}
	}
	for key, value := range map[string]string{"BT_DB_RUNTIME_MAX_CONNS": "1", "BT_DB_MCP_MAX_CONNS": "5", "BT_DB_SERVER_MAX_CONNECTIONS": "0", "BT_DB_RESERVED_CONNECTIONS": "64"} {
		env := laboratoryPools()
		env[key] = value
		if _, err := LoadDatabasePools(mapGetter(env)); err == nil {
			t.Fatalf("unsafe %s accepted", key)
		}
	}
	env := laboratoryPools()
	env["BT_DB_MAINTENANCE_MAX_CONNS"] = "0"
	env["BT_DB_RESERVED_CONNECTIONS"] = "0"
	if _, err := LoadDatabasePools(mapGetter(env)); err != nil {
		t.Fatalf("disabled maintenance or zero reserved rejected: %v", err)
	}
	// The dedicated MCP pool may hold at most four connections.
	for _, value := range []string{"1", "4"} {
		env := laboratoryPools()
		env["BT_DB_MCP_MAX_CONNS"] = value
		if pools, err := LoadDatabasePools(mapGetter(env)); err != nil || pools.MCP == 0 {
			t.Fatalf("MCP=%s rejected: %+v %v", value, pools, err)
		}
	}
	// The projector pool is retired: Load names it (RetiredSecuritySettings);
	// the profile itself never reads it.
	env = laboratoryPools()
	env["BT_DB_PROJECTOR_MAX_CONNS"] = "4"
	if pools, err := LoadDatabasePools(mapGetter(env)); err != nil || pools != (DatabasePools{16, 2, 0, 64, 8}) {
		t.Fatalf("projector budget changed the profile: %+v %v", pools, err)
	}
	// An injected all-empty getter must not fall through to ambient environment.
	t.Setenv("BT_DB_RUNTIME_MAX_CONNS", "99")
	if pools, err := LoadDatabasePools(mapGetter(nil)); err != nil || pools != (DatabasePools{}) {
		t.Fatal("ambient configuration leaked through getter")
	}
}

// Maintenance is always enabled (decision D3), and one maintenance
// connection is reserved for the readiness audit beside the workers, so an
// explicit profile needs at least two.
func TestEnabledMaintenanceNeedsPositiveBudget(t *testing.T) {
	values := validHostedEnvironment(t)
	for key, value := range laboratoryPools() {
		values[key] = value
	}
	if _, err := Load(mapGetter(values)); err != nil {
		t.Fatalf("two-connection maintenance budget rejected: %v", err)
	}
	for _, budget := range []string{"0", "1"} {
		values["BT_DB_MAINTENANCE_MAX_CONNS"] = budget
		if _, err := Load(mapGetter(values)); err == nil || !strings.Contains(err.Error(), "BT_DB_MAINTENANCE_MAX_CONNS must be at least 2") {
			t.Fatalf("maintenance budget %s accepted: %v", budget, err)
		}
	}
}
