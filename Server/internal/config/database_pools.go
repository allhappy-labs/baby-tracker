package config

import (
	"errors"
	"fmt"
	"strconv"
)

// DatabasePools is zero in legacy mode. Explicit profiles are complete and
// atomic; enabled maintenance is validated by the composition. MCP is zero
// unless private MCP is enabled, and its dedicated pool never exceeds
// MaxMCPConnections. There is no projector pool: the authority runs on the
// runtime pool (BT_DB_PROJECTOR_MAX_CONNS is a retired setting).
type DatabasePools struct {
	Runtime     int32 `json:"runtime"`
	Maintenance int32 `json:"maintenance"`
	MCP         int32 `json:"mcp"`
	ServerMax   int32 `json:"server_max"`
	Reserved    int32 `json:"reserved"`
}

func (p DatabasePools) Explicit() bool { return p != (DatabasePools{}) }

// LoadDatabasePools treats five empty getter values as legacy mode. The getter
// cannot distinguish unset from explicitly empty; it never consults ambient env.
func LoadDatabasePools(getenv func(string) string) (DatabasePools, error) {
	if getenv == nil {
		return DatabasePools{}, errors.New("configuration getter is required")
	}
	names := []string{"BT_DB_RUNTIME_MAX_CONNS", "BT_DB_MAINTENANCE_MAX_CONNS", "BT_DB_MCP_MAX_CONNS", "BT_DB_SERVER_MAX_CONNECTIONS", "BT_DB_RESERVED_CONNECTIONS"}
	var raw [5]string
	supplied := false
	for i, name := range names {
		raw[i] = getenv(name)
		supplied = supplied || raw[i] != ""
	}
	if !supplied {
		return DatabasePools{}, nil
	}
	var values [5]int32
	for i, name := range names {
		n, err := strconv.ParseInt(raw[i], 10, 32)
		if err != nil || n < 0 {
			return DatabasePools{}, fmt.Errorf("%s must be a nonnegative integer in a complete database pool profile", name)
		}
		values[i] = int32(n)
	}
	p := DatabasePools{values[0], values[1], values[2], values[3], values[4]}
	if p.Runtime < 2 || p.MCP > MaxMCPConnections || p.ServerMax < 1 || p.Reserved >= p.ServerMax {
		return DatabasePools{}, errors.New("database pool profile needs runtime request and authority capacity, a positive server ceiling, reserved below server ceiling, and at most four MCP connections")
	}
	return p, nil
}
