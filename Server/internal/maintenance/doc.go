// Package maintenance runs the node's background sweeps: bounded,
// idempotent units of retention work (Sweep) driven by a Loop per cadence.
//
// Worker ruling (Phase 3): every sweep runs bounded batches, each in one
// transaction with a lock_timeout (200ms where a request may wait on what
// the batch holds, else 2s) and statement_timeout 10s; it is safe beside
// requests and beside itself (rows are chosen FOR UPDATE SKIP LOCKED, or
// deleted by a plain bounded DELETE where the credential cannot lock them,
// or, for care receipts and the family purge, behind their family rows
// locked FOR NO KEY UPDATE SKIP LOCKED). No sweep locks an account row, and
// none locks a session row after a family row: ReceiptSweep and Purger are
// the only ones that lock family rows, and they lock nothing else but rows
// of the families they hold (the purge's StartFamilyPurge and
// CompleteFamilyPurge are authority commands, which lock accounts, then the
// family, in the commands' order). A rolled-back batch changes nothing and
// the next run repeats it.
//
// Pools: the authority sweeps (SessionSweep) run on the background pool, the
// runtime credential's one-connection pool they share with the audit
// verifier and the key inventory; everything on the maintenance pool
// (AuditPartitionSweep, AuditRetentionSweep, ReceiptSweep, ExpirySweep,
// Purger) holds a
// database.WorkerSlots slot for each transaction, so the readiness audit
// always finds a maintenance connection. The purge's authority commands run
// on the request authority, so their invalidations reach the live hub.
package maintenance
