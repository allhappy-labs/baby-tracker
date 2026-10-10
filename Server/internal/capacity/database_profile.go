package capacity

import (
	"babytracker/server/internal/config"
	"errors"
	"strconv"
)

// mcpProfiles names every MCP-enabled matched profile and its MCP concurrency.
// The dedicated MCP pool and the global MCP admission maximum are always set
// to the same value, so a lowered limit is one named, recorded profile.
var mcpProfiles = map[string]int{"explicit-mcp": 4, "explicit-mcp-3": 3, "explicit-mcp-2": 2, "explicit-mcp-1": 1}

// databaseProfile returns only owned laboratory settings, never ambient values.
// Selecting explicit supplies all five variables together; default stays legacy.
func databaseProfile(profile string) (map[string]string, config.DatabasePools, error) {
	if profile == "" || profile == "legacy" {
		return nil, config.DatabasePools{}, nil
	}
	mcp, isMCP := mcpProfiles[profile]
	if profile != "explicit" && !isMCP {
		return nil, config.DatabasePools{}, errors.New("capacity_database_profile_invalid")
	}
	env := map[string]string{"BT_DB_RUNTIME_MAX_CONNS": "16", "BT_DB_MAINTENANCE_MAX_CONNS": "2", "BT_DB_MCP_MAX_CONNS": "0", "BT_DB_SERVER_MAX_CONNECTIONS": "64", "BT_DB_RESERVED_CONNECTIONS": "8"}
	// The MCP-enabled matched profiles differ only by the dedicated MCP pool.
	if isMCP {
		env["BT_DB_MCP_MAX_CONNS"] = strconv.Itoa(mcp)
	}
	budget, err := config.LoadDatabasePools(func(k string) string { return env[k] })
	return env, budget, err
}

// mcpProfile reports whether the database profile enables the MCP pool.
func mcpProfile(profile string) bool { return mcpProfiles[profile] > 0 }

// mcpAdmissionMax is BT_ADMISSION_MCP_MAX for a cell: equal to the dedicated
// MCP pool, and empty when the profile has no MCP pool.
func mcpAdmissionMax(budget config.DatabasePools) string {
	if budget.MCP <= 0 {
		return ""
	}
	return strconv.Itoa(int(budget.MCP))
}
