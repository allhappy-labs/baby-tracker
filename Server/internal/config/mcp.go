package config

import (
	"errors"
	"fmt"
)

// MaxMCPConnections bounds both the dedicated MCP database pool and the
// global in-flight MCP admission maximum.
const MaxMCPConnections = 4

// MCPConfig is the opt-in private MCP adapter configuration. The zero value is
// the disabled default: no MCP secrets are read and no pool or route exists.
type MCPConfig struct {
	Enabled bool
	// DatabaseURL is the dedicated MCP pool's connection string.
	DatabaseURL string
	// CursorKey authenticates history continuation cursors.
	CursorKey [32]byte
}

// LoadMCP reads BT_MCP_ENABLED ("true" or "false"/unset). When enabled it
// requires BT_MCP_DATABASE_URL_FILE (one nonempty line) and
// BT_MCP_CURSOR_KEY_FILE (64 lowercase hex characters, a 32-byte nonzero key)
// as regular, non-symlink secret files. Errors never include paths or
// contents.
func LoadMCP(getenv func(string) string) (MCPConfig, error) {
	if getenv == nil {
		return MCPConfig{}, errors.New("configuration getter is required")
	}
	switch getenv("BT_MCP_ENABLED") {
	case "", "false":
		return MCPConfig{}, nil
	case "true":
	default:
		return MCPConfig{}, errors.New("BT_MCP_ENABLED must be true or false")
	}
	if getenv("BT_MCP_DATABASE_URL") != "" || getenv("BT_MCP_CURSOR_KEY") != "" {
		return MCPConfig{}, errors.New("the MCP database URL and cursor key are accepted only through BT_MCP_DATABASE_URL_FILE and BT_MCP_CURSOR_KEY_FILE")
	}
	if getenv("BT_MCP_DATABASE_URL_FILE") == "" {
		return MCPConfig{}, errors.New("BT_MCP_DATABASE_URL_FILE is required when BT_MCP_ENABLED=true")
	}
	if getenv("BT_MCP_CURSOR_KEY_FILE") == "" {
		return MCPConfig{}, errors.New("BT_MCP_CURSOR_KEY_FILE is required when BT_MCP_ENABLED=true")
	}
	fileOnly := func(name string) string {
		if name == "BT_MCP_DATABASE_URL_FILE" || name == "BT_MCP_CURSOR_KEY_FILE" {
			return getenv(name)
		}
		return ""
	}
	databaseURL, err := credentialURL(fileOnly, "BT_MCP_DATABASE_URL", "BT_MCP_DATABASE_URL_FILE")
	if err != nil {
		return MCPConfig{}, err
	}
	// A non-development mode keeps the shared pepper loader file-only.
	key, err := loadPepper(fileOnly, ModeSelfHosted, "BT_MCP_CURSOR_KEY", "BT_MCP_CURSOR_KEY_FILE")
	if err != nil {
		return MCPConfig{}, err
	}
	if key == ([32]byte{}) {
		return MCPConfig{}, fmt.Errorf("BT_MCP_CURSOR_KEY_FILE must not contain an all-zero key")
	}
	return MCPConfig{Enabled: true, DatabaseURL: databaseURL, CursorKey: key}, nil
}

// validateMCPComposition ties the opt-in adapter to the shared budgets.
// Enabled MCP requires a self-hosted or development node (never the public
// hosted mode), a complete explicit database profile whose dedicated MCP pool
// is 1..4 connections and fits the aggregate budget, and a global MCP
// admission maximum of 1..4 below the total that exactly equals the dedicated
// MCP pool size (so an admitted read can never queue on pgxpool.Acquire
// inside the request's 5-second budget — every evidence cell and the
// documented operational guidance uses equal values). Disabled MCP reserves
// nothing.
func validateMCPComposition(mcp MCPConfig, mode Mode, admission AdmissionConfig, pools DatabasePools) error {
	if !mcp.Enabled {
		if pools.MCP != 0 || admission.MCPMax != 0 {
			return errors.New("BT_DB_MCP_MAX_CONNS and BT_ADMISSION_MCP_MAX must be 0 while BT_MCP_ENABLED is not true")
		}
		return nil
	}
	if mode == ModeHosted {
		return errors.New("private MCP is not available in hosted mode")
	}
	if !pools.Explicit() || pools.MCP < 1 || pools.MCP > MaxMCPConnections {
		return errors.New("enabled MCP requires a complete explicit database pool profile with BT_DB_MCP_MAX_CONNS from 1 through 4")
	}
	if int64(pools.Runtime)+int64(pools.Maintenance)+int64(pools.MCP) > int64(pools.ServerMax)-int64(pools.Reserved) {
		return errors.New("enabled MCP database pools exceed the declared server connection budget")
	}
	if admission.MCPMax < 1 || admission.MCPMax > MaxMCPConnections || admission.MCPMax >= admission.Total {
		return errors.New("enabled MCP requires BT_ADMISSION_MCP_MAX from 1 through 4 and below BT_ADMISSION_TOTAL")
	}
	if int32(admission.MCPMax) != pools.MCP {
		return errors.New("enabled MCP requires BT_ADMISSION_MCP_MAX to equal BT_DB_MCP_MAX_CONNS")
	}
	return nil
}
