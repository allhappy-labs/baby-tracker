package sync

// Statements are the request-path statements this package sends itself,
// by name, for the plan test.
func Statements() map[string]string {
	return map[string]string{
		"sync_continuation":     sqlSyncContinuation,
		"snapshot_continuation": sqlSnapshotContinuation,
		"snapshot_admission":    sqlSnapshotAdmission,
	}
}
