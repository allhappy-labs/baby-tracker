package database

import "errors"

var ErrBudget = errors.New("database_pool_budget_invalid")

// Identity is a composition-owned identifier for an actual pool, never a DSN.
type NamedPoolBudget struct {
	Identity string
	Max      int32
}

// ValidateBudget counts shared identities once and fails on ambiguous maxima.
func ValidateBudget(pools []NamedPoolBudget, serverMax, reserved int32) error {
	if serverMax <= 0 || reserved < 0 || reserved >= serverMax {
		return ErrBudget
	}
	seen := make(map[string]int32, len(pools))
	var total int64
	for _, pool := range pools {
		if pool.Identity == "" || pool.Max <= 0 {
			return ErrBudget
		}
		if max, exists := seen[pool.Identity]; exists {
			if max != pool.Max {
				return ErrBudget
			}
			continue
		}
		seen[pool.Identity] = pool.Max
		total += int64(pool.Max)
	}
	if total > int64(serverMax)-int64(reserved) {
		return ErrBudget
	}
	return nil
}
