package config

import "errors"

type RecoveryCleanupConfig struct {
	Enabled           bool
	WorkerDatabaseURL string
}

// LoadRecoveryCleanup reuses the isolated export maintenance principal when
// available. Installations without exports can supply only this secret file.
func LoadRecoveryCleanup(getenv func(string) string, exports ExportConfig) (RecoveryCleanupConfig, error) {
	if getenv == nil {
		return RecoveryCleanupConfig{}, errors.New("configuration getter is required")
	}
	const inline = "BT_RECOVERY_MAINTENANCE_DATABASE_URL"
	const file = "BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE"
	if getenv(inline) != "" {
		return RecoveryCleanupConfig{}, errors.New("BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE is required")
	}
	if getenv(file) == "" {
		return RecoveryCleanupConfig{Enabled: exports.Enabled, WorkerDatabaseURL: exports.WorkerDatabaseURL}, nil
	}
	url, err := credentialURL(getenv, inline, file)
	if err != nil {
		return RecoveryCleanupConfig{}, err
	}
	return RecoveryCleanupConfig{Enabled: true, WorkerDatabaseURL: url}, nil
}
