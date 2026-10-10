package config

import (
	"errors"
	"fmt"
)

// RetiredSecuritySettings are the settings of the retired file security
// ledger, its projector credential and pool, the hosted security provider
// and the hosted Apple revocation worker. PostgreSQL is now the only
// security authority, so none of them configures anything; a deployment
// that still sets one was written for the old runtime and is refused rather
// than silently started without the state it expects.
var RetiredSecuritySettings = []string{
	"BT_SECURITY_LEDGER_DIR",
	"BT_LEDGER_HEAD_DIR",
	"BT_PROJECTOR_DATABASE_URL",
	"BT_PROJECTOR_DATABASE_URL_FILE",
	"BT_DB_PROJECTOR_MAX_CONNS",
	"BT_HOSTED_PROVIDER_ID",
	"BT_HOSTED_REGION",
	"BT_HOSTED_APPROVED_REGIONS",
	"BT_HOSTED_LEDGER_FAILURE_DOMAIN",
	"BT_HOSTED_HEAD_FAILURE_DOMAIN",
	"BT_HOSTED_LEDGER_KEY_ID",
	"BT_HOSTED_HEAD_KEY_ID",
	"BT_HOSTED_VAULT_KEY_ID",
	"BT_SECURITY_REPLAY_SCRATCH_PATH",
	"BT_SECURITY_REPLAY_SCRATCH_MAX_BYTES",
	"BT_APPLE_REVOCATION_DATABASE_URL",
	"BT_APPLE_REVOCATION_DATABASE_URL_FILE",
	"BT_APPLE_REVOCATION_WORKER_ID",
	"BT_APPLE_REVOCATION_POLL_INTERVAL",
	"BT_APPLE_REVOCATION_LEASE_TTL",
	"BT_CREDENTIAL_ORPHAN_GRACE",
}

// RejectRetiredSecuritySettings fails closed, in every mode, when a retired
// security setting is set. The error names every offending setting.
func RejectRetiredSecuritySettings(getenv func(string) string) error {
	if getenv == nil {
		return errors.New("configuration getter is required")
	}
	var errs []error
	for _, name := range RetiredSecuritySettings {
		if getenv(name) != "" {
			errs = append(errs, fmt.Errorf("%s is retired: PostgreSQL is the only security authority; remove the setting (docs/operations/security-state.md)", name))
		}
	}
	return errors.Join(errs...)
}
