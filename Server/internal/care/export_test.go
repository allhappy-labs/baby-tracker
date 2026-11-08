package care

import "github.com/jackc/pgx/v5"

// Statements is every read statement the store sends, by name, for the plan
// test (TestCareQueriesUseOnePartitionAndIndexes).
func Statements() map[string]string {
	return map[string]string{
		"fetch_receipts":        sqlFetchReceipts,
		"fetch_rows":            sqlFetchRows,
		"fetch_session_members": sqlFetchSessionMembers,
		"fetch_live_timers":     sqlFetchLiveTimers,
		"fetch_cascade_targets": sqlFetchCascadeTargets,
		"fetch_live_babies":     sqlFetchLiveBabies,
		"fetch_open_pump":       sqlFetchOpenPump,
		"read_pull":             sqlReadPull,
		"changes":               sqlChanges,
		"snapshot_page":         sqlSnapshotPage,
		"stream_page":           sqlStreamPage,
		"get":                   sqlGet,
		"get_many":              sqlGetMany,
		"match_babies":          sqlMatchBabies,
	}
}

// SetStreamPageSize sets the rows Stream and StreamChanges read per page and
// returns the restore.
func SetStreamPageSize(n int) (restore func()) {
	previous := streamPageSize
	streamPageSize = n
	return func() { streamPageSize = previous }
}

// QueueWritesUnguarded is QueueWrites without its Go checks of the write
// set (sequence range, duplicate rows and receipts), for tests that pin the
// database's own constraints or write rows at given legacy sequences.
func QueueWritesUnguarded(b *pgx.Batch, w Writes) (check func(pgx.BatchResults) error) {
	return queueWrites(b, w, false)
}

// CompletedTimerPayload is completedTimerPayload, for the stop-cleanup tests.
var CompletedTimerPayload = completedTimerPayload
