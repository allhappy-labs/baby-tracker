package identity_test

import (
	"testing"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/identity/credentialtest"
)

func TestMemoryCredentialVaultConformance(t *testing.T) {
	credentialtest.Run(t, func(*testing.T) credentialtest.Harness {
		now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
		vault := identity.NewMemoryCredentialVault(func() time.Time { return now })
		reopen := func(*testing.T) (identity.CredentialVault, identity.CredentialInventory) {
			reopened := vault.Reopen()
			return reopened, reopened
		}
		return credentialtest.Harness{
			Vault:     vault,
			Inventory: vault,
			Now:       func() time.Time { return now },
			Advance:   func(delta time.Duration) { now = now.Add(delta) },
			Reopen:    reopen,
		}
	})
}
