# PostgreSQL Authority — Phase 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Store every care record in one hash-partitioned `care_entities` table that carries typed filter columns, derived summary columns and the pre-encoded canonical wire entity. Serve pulls by `changed_seq` from one index with the stored bytes verbatim, serve snapshots by paging current rows in key order with no staging, and apply each sync batch set-based in two database round trips inside one transaction. The wire protocol stays unchanged.
**Architecture:** A new `Server/internal/care` package owns care storage:
- `Encode` is the only entity encoder. It produces the row, the derived columns and the wire bytes.
- `Fetch` takes the family row lock, reads the receipts and reads every referenced entity in one pipelined round trip.
- An in-memory working-set planner ports today's record, timer and cascade semantics.
- `QueueWrites` sends one pipelined batch: a multi-row upsert, the sequence range and counters on the family row, bulk receipts, and optionally the pull read.
- Read helpers cover pulls, snapshot pages, lookups and streams.

`sync` runs a whole request (batch plus pull page) in one guarded transaction through `Service.Exchange(ctx, tx, principal, grant, request)`. Phase 3 calls that seam from `WithAuthenticatedFamilyTx`. `familyread`, `export` and `importer` read and write through `care`. The journal, snapshot staging, per-kind tables, component child tables, `family_record_usage`, `delivery_pending` and in-transaction wake fan-out are removed.
**Tech Stack:** Go 1.26.2, pgx v5.11 (pipelined batches, per-connection statement cache), PostgreSQL 17 (hash partitioning, enum types, partial unique indexes, RLS), the existing disposable-Docker test harness, and the SwiftPM iOS E2E suites driven by `Server/scripts/protocol-e2e.sh`.
**Spec:** docs/superpowers/specs/2026-10-04-postgres-authority-performance-design.md

**Migration ruling:** Task 2 writes `000030_care_entities.sql` as additive: new tables, types, family counters, grants and manifest 30, plus compatibility 30. The legacy care tables keep working until the switch. Task 9 amends 000030 once to append the removal section (fresh-install guard, drops, FK repoint, manifest filter) and regenerates the digest. 000030 is FROZEN at the end of Task 9, and later changes go in 000031+.
- Why: this keeps every boundary before Task 8 green and leaves one coherent Phase 2 migration. It repeats the Phase 1 rule of amending only while unpublished.
- Cost if wrong: a reviewer re-checks 000030 at Task 9.

**Big-bang ruling:** Writers and readers of care data must switch together because they share the data. Task 8 is that switch, isolated as one dispatch. It may commit in two coherent parts (8a: sync, importer, delivery, testkit, cmd; 8b: familyread, export, mcpapi, tests/protocol, capacity) with a single gate at the end. Tasks 3–7 build and parity-test every new path beforehand, so Task 8 is wiring plus test adaptation.

**Transaction-shape ruling:** One sync request is one transaction.
- **Mutations.** The family row is locked once, inside the `Fetch` round trip. Mutations are applied in order against a working set. The first refusing error (quota, `mutation_id_reused`, `read_only_caregiver`, `owner_required`, `feature_unavailable`, `snapshot_required` after writes) stops planning. Everything before that error is written and committed, and then the error is returned. This matches today's per-mutation transactions, where earlier mutations of a failed batch stayed committed.
- **Pull-only.** These take no lock. They use READ COMMITTED with high water and rows read in one statement, which is one snapshot. Continuations read the binding and the rows in one statement.
- **Infrastructure errors.** These roll back the whole transaction.

**Sequence ruling:** Every applied change consumes the next family sequence, in mutation order. A row keeps the sequence of its last change, so a row written twice in one batch leaves a gap. `UNIQUE (family_id, changed_seq)` makes pull order total, so pages may split anywhere. The baby-delete cascade keeps today's order: root first, then feed, sleep, diaper, supplement, growth_profile, measurement, timer, each by id. Single-mutation requests therefore get exactly today's cursor values, which is what the golden corpus relies on.

**Wake ruling:** The write transaction schedules nothing. `CommitObserver.Schedule` (`app.schedule_family_wakes` plus `pg_notify`) is deleted. After commit, `Publish` to the in-process hub is the only hint. The hub already received it, and on a single node the NOTIFY copy duplicated it. `delivery_pending` and `app.schedule_family_wakes` are dropped in Task 9; they had no consumer, since the worker was never composed. The interim mechanism is no push wake at all. Phase 3 builds the worker that compares each device's acknowledged cursor with `families.next_sequence`, and deletes the now idle `realtime.PostgresListener`.

**Quota ruling:** Usage lives on the family row and is maintained by delta in the one write batch:
- `care_record_count` counts live rows whose kind is not baby or timer, as today.
- `care_charged_bytes` is the sum over rows of (wire bytes + 256), plus 4096 per live row, plus retained import staging (`care.ChargeImport`).
- Journal history and receipts are no longer charged, because the journal is gone and receipts expire after 30 days. Limits stay at their configured defaults.
- `care_entity_count` and `care_wire_bytes` size snapshots in O(1).

**Page-continuation ruling:** The token format, TTLs and limits are unchanged. Server-side bindings drop the page item count, page bytes and page end. A replayed continuation returns the current rows of the same range (resume, high water]. A row edited since moves past the high water and arrives in the next pull. "More" means a fetched row was left over, never `end < high water`, so a range emptied by edits ends without a token.

**Snapshot ruling:** Snapshots page every current row, tombstones included as today, in `(kind, id)` order. The `app.care_kind` declaration order equals the legacy `snapshotKindOrder`. Snapshots are not frozen: an entity may arrive at a newer version than the recorded high water, and the next pull re-delivers it idempotently.

**Export ruling:** The archive `journal` section keeps its shape but now holds the latest-state change list, one change per entity at its `changed_seq` (what a pull from cursor 0 returns). All other archive sections stay byte-identical. iOS does not parse archives (no Swift reference).

## Global Constraints
- "All care records (babies, feeds, sleeps, diapers, supplements, growth profiles, measurements, active timers) live in `care_entities`, primary key `(family_id, id)`, hash-partitioned by family (initially 16 partitions)"
- "typed fields for filtering and aggregation: kind, baby, version, `changed_seq`, start, end, session, tombstone time; derived fields computed at write time for summaries, for example bottle millilitres and breast seconds; the canonical wire entity, stored already encoded, served verbatim"
- "The per-mutation 8-table kind probe becomes one primary-key lookup. Child-row tables for components are removed; components live in the stored entity and its derived fields. Baby-name duplicate matching uses a 32-byte hash"
- "There is no mutation journal. Each row carries `changed_seq`, the family sequence of its latest change. A pull reads rows with `changed_seq` above the device cursor, ordered by `changed_seq`, from one index … only each entity's latest state … Tombstones remain rows, so old cursors still see every delete"
- "A snapshot records the current family high-water mark and pages live rows in key order with no lock and no copy. Changes made during paging receive a higher sequence and arrive in the next pull. Snapshot page-token format, TTLs and limits stay as they are; the snapshot staging tables are removed"
- "Per sync batch, about four round trips regardless of batch size: 1. Fetch every existing entity the batch references in one query. 2. Validate in Go. 3. One pipelined batch: multi-row upsert, one sequence-range allocation, bulk receipt insert, one usage update on the already-locked family row. 4. Commit"
- "Validation stays in Go with database checks as a second line (7-day maximum, zero-duration rejection, value bounds). Last-writer-wins by version and the 'overwrote newer' signal are unchanged. Quotas move onto the family row"
- "Pull-only requests take no lock and read under the transaction snapshot" / "Requests with mutations lock the family row once for the whole batch" / "Apply the batch (see Write path), read the pull page, commit"
- "Push wake intents leave the write transaction: a background worker compares each device's acknowledged cursor with the family sequence"
- "Mutation receipts: 30 days." / "Page and snapshot sessions: until their TTL." / "Care entities and tombstones are kept"
- "Every index is justified by a named request-path query" / "Fill factor below 100 on tables with frequent in-place updates" / "Native `uuid`, small integer or enum types for kinds and statuses, never free text for closed sets"
- "Row-level security remains on every family-scoped table as defence in depth, with policies written so the planner keeps using indexes"
- "All request-path statements are prepared and cached per connection. Fixed statement sequences are batched into one round trip"
- "Old binaries fail closed: the schema compatibility version is bumped and pre-migration binaries refuse to start"
- "The wire format is identical, but two behaviours change and must be verified against the shipped client before release: 1. Pulls deliver each entity's latest state, not every intermediate version. 2. A snapshot is not a frozen point in time; the device is consistent after it catches up with the following pull"
- Quota defaults stay `max_canonical_records_per_family` 100000 and `max_charged_storage_bytes_per_family` 268435456 (256 MiB); every `protocol.Limits` default and range is unchanged (protocol/sync.go:128-213).
- Phase 1 carry: "handlers take family and role from the Grant, never from Principal.Scope".

## Review Focus
1. **Continuation replayed after a concurrent edit** (lost response, client retries the same `page_token`) once rows in the range have moved past the high water. It must return the current range, never `invalid_page_token`. A range emptied entirely by edits must end without a token, with no token loop. The device converges after the next pull. Pinned in Task 8 (`TestSyncContinuationReplayAfterConcurrentEdit`, `TestEmptiedRangeEndsWithoutToken`).
2. **A batch whose k-th mutation hits quota, `mutation_id_reused` or `owner_required`.** Mutations 1..k-1 are committed with receipts exactly as today's per-mutation transactions left them, and the client's retry gets `already_applied` for them. A viewer's batch writes nothing. Pinned in Task 5 (`TestBatchFatalErrorCommitsPrefix`).
3. **Notes and names containing `<`, `>`, `&`, U+2028/U+2029 or emoji, served verbatim.** Stored wire bytes plus the HTTP encoder must equal today's decode-then-encode bytes. Pinned in Task 3 (`TestWireServedVerbatimEscapesLikeEncoder`).
4. **Deleting a baby with a large history** (5,000 children including a running timer, growth profile and measurements). It must be one transaction and two round trips, with consecutive sequences in legacy order. Reserves are released, every tombstone reaches the other phone across pages, and later child mutations get `baby_deleted`. Pinned in Task 6 (`TestBabyDeleteCascadeIsSetBasedAndOrdered`).
5. **One entity mutated several times in one offline batch.** Cases: timer start, finish bottle, edit and stop; feed create then update; the same mutation id twice. Each must chain through the working set, write one row with the last sequence and give exact receipts. Pinned in Task 6 (`TestTimerLifecycleInOneBatch`) and Task 5 (`TestBatchChainsMutationsOnOneEntity`).

## File Structure

**Create**
- `Server/migrations/000030_care_entities.sql` — care types, `care_entities` (16 hash partitions), `care_receipts`, `sync_page_bindings`, `sync_snapshot_sessions`, family counters, RLS, grants, manifest 30, compatibility 30. Task 9 appends removal of the legacy care storage.
- `Server/internal/care/doc.go` — package contract: the one encoder, the two-round-trip write, the read helpers.
- `Server/internal/care/entity.go` — `Row`, `Derived`, `Change`, `Encode`, `Row.Entity`, charging rules.
- `Server/internal/care/derived.go` — feed derived columns.
- `Server/internal/care/namehash.go` — the 32-byte baby match hash.
- `Server/internal/care/store.go` — every SQL statement on care tables: lock, fetch, write batch, pull, snapshot page, get, stream, match, import charge.
- `Server/internal/care/validate.go` — moved from `records/validation.go`.
- `Server/internal/care/apply.go` — `BatchCommand`, `ApplyBatch`, policy and receipt handling, fatal-prefix rule.
- `Server/internal/care/plan.go` — working set, sequence allocation, counters.
- `Server/internal/care/records.go` — record create/update/delete semantics (port of `records/repository.go:88-305`, `sessions.go`).
- `Server/internal/care/timers.go` — timer start, finish bottle, edit, stop and ended (port of `records/timers.go`).
- `Server/internal/care/cascade.go` — baby delete (port of `records/child_delete.go`).
- `Server/internal/care/caretest/caretest.go` — tenant transactions, seeding through `ApplyBatch`, mutation builders, mirror seeding.
- `Server/internal/care/*_test.go` — the tests named in the tasks.
- `Server/internal/testkit/golden.go` — golden scenario, ID normalizer, corpus assertion.
- `Server/internal/testkit/roundtrips.go` — counted single-connection pool for round-trip tests.
- `Server/internal/sync/exchange.go` — `Exchange`/`SnapshotPage`, the in-transaction seam for Phase 3.
- `Server/internal/sync/golden_test.go`, `convergence_test.go`, `roundtrip_test.go`.
- `Server/internal/sync/testdata/golden/legacy-sync.json`.
- `Server/internal/familyread/golden_test.go`, `familyread/testdata/golden/legacy-familyread.json`.
- `Server/internal/familyread/care_reads.go`, `familyread/reads.go`.
- `Server/internal/export/golden_test.go`, `export/testdata/golden/legacy-export.json`.
- `Server/internal/database/care_schema_test.go`.
- `Server/internal/database/legacy_format_cleanup_test.go` and `legacy_format_cleanup_join_test.go` — moved from `records`.

**Modify**
- `Server/internal/protocol/sync.go` — `EncodeChange`, `EncodedChanges`/`EncodedEntities`, `MarshalJSON`, raw size helpers.
- `Server/internal/protocol/normalization.go` — `SumDecimals`.
- `Server/internal/sync/{mutation.go,paging.go,snapshot.go}`.
- `Server/internal/importer/{resolver.go,conflict.go,manifest.go,repository.go}`.
- `Server/internal/delivery/observer.go`.
- `Server/internal/familyread/{service.go,history.go,summary.go,summary_sql.go}`.
- `Server/internal/export/repository.go`.
- `Server/internal/testkit/{fixtures.go,postgres.go}`.
- `Server/internal/capacity/fixture_identitytest.go`.
- `Server/cmd/baby-tracker-server/{main.go,performance_composition_identitytest_test.go}`.
- `Server/internal/authority/locks_test.go`.
- Tests in `sync`, `importer`, `familyread`, `export`, `mcpapi`, `httpapi`, `realtime`, `tests/protocol`, `database`, `migrate`, `tenant`.
- `Server/internal/migrate/{compatibility.json,compatibility_test.go}`.
- `Server/scripts/{check-principals.sh,smoke.sh,protocol-e2e.sh,mcp-e2e.sh,lib/disposable-sync.sh}` and their `_test.go` expectations.
- `docs/protocol/v1.md`.

**Delete (Task 9)**
- `Server/internal/records/**` — format-cleanup SQL tests move to `database`.
- `Server/internal/sync/{journal.go,receipts.go,format_cleanup_interim_test.go}`.
- `Server/internal/delivery/{repository.go,queue.go,queue_test.go}`.
- testkit `NewThrough`, `newCareHarness`, `tenantGuard` and `careResolver`.
- `Server/internal/database/{records_schema_test.go,snapshot_schema_test.go,timer_schema_test.go,delivery_schema_test.go}` — replaced by `care_schema_test.go`.

---

### Task 1: Golden wire corpus from the legacy implementation

**Files:**
- Create `Server/internal/testkit/golden.go`.
- Create `Server/internal/sync/golden_test.go` and `Server/internal/sync/testdata/golden/legacy-sync.json`.
- Create `Server/internal/familyread/golden_test.go` and `Server/internal/familyread/testdata/golden/legacy-familyread.json`.
- Create `Server/internal/export/golden_test.go` and `Server/internal/export/testdata/golden/legacy-export.json`.

**Interfaces:** Consumes `testkit.Harness` (fixed clock `h.Now`), `testkit.MustMutation`, `h.Service.ApplyAndPull`, `h.Service.FullSnapshot`, `h.Importer.Resolve`, `familyread.Service`, `export.collectArchive`/`EncodeArchive`. Produces:
```go
package testkit
type GoldenStep struct { Name, Subject string /* "owner"|"caregiver" */; Mutations []protocol.Mutation }
type GoldenRun struct {
	Outcomes  [][]json.RawMessage // per step, normalized outcome objects as encoded on the wire
	Imports   []json.RawMessage   // normalized import resolve responses
	HighWater []uint64            // families.next_sequence after each step
}
func GoldenSteps(t *testing.T, h *Harness) []GoldenStep
func (h *Harness) RunGolden(t *testing.T) GoldenRun
func (h *Harness) Normalize(raw []byte) []byte // dynamic IDs -> fixed valid UUIDs, so normalized entities still decode
func AssertGolden(t *testing.T, path string, got any)  // BT_GOLDEN_UPDATE=1 rewrites path; otherwise byte-compares json.MarshalIndent(got)
func LatestPerEntity(t *testing.T, changes []json.RawMessage) []json.RawMessage // last change per entity_id, in sequence order (used from Task 8)
```
The normalizer maps:
- `h.FamilyID` → `f0000000-0000-4000-8000-0000000000f1`
- owner account and device → `a0000000-0000-4000-8000-0000000000a1` and `d0000000-0000-4000-8000-0000000000d1`
- caregiver account and device → `…a2` and `…d2`
- snapshot ids → `5a000000-0000-4000-8000-000000000001`
- page tokens and history cursors → `"{{token}}"`

All replacements are the same byte length, except tokens.

The steps, with fixed mutation ids, client times and `h.Now`, one `ApplyAndPull` per step:
1. Owner: `baby.json` (baby A); baby B created programmatically (name `Zoë <b>&</b>`, birth 2026-02-01, sort 1).
2. Owner: `feed-breast.json`, `feed-bottle.json`, `feed-combination.json`, `feed-solids.json`.
3. Caregiver: bottle feed update, note `"line sep <script>&"`, observed 1.
4. Owner: `sleep.json`, `diaper.json`, `supplement.json`, `growth-profile.json`, `measurement.json`.
5. Caregiver: supplement update with the same `logged_at`; diaper delete.
6. Owner: `timer-feed.json` start, `timer_finish_bottle`, timer edit, `timer_stop` (completion feed).
7. Caregiver: `timer-sleep.json` start; stop with a note and `sleep_kind_override` "nap".
8. Owner: refusals — a feed under an unknown baby (`baby_not_found`), a duplicate create (`entity_exists`), and a stale update with observed 1 on version 3 (`overwrote_newer_version: true`).
9. Owner: an exact replay of step 2's first mutation (`already_applied`).
10. Owner, under baby B: feed, sleep, feed timer start and growth profile; then delete baby B (cascade).
11. Owner: import — `Resolve` in `preserve_local_ids` with one baby and two records, then one import-tagged feed create (`applied`) and one equal-content import (handled equal).

- [ ] **Step 1: Write the failing tests.** `Server/internal/sync/golden_test.go`:
```go
func TestLegacyWireCorpus(t *testing.T) {
	h := testkit.New(t)
	run := h.RunGolden(t)
	full := pullAll(t, h, h.Owner, 0)                   // ApplyAndPull with no mutations, following next_page_token
	mid := pullAll(t, h, h.Caregiver, run.HighWater[4]) // a cursor taken mid-history
	snap := snapshotAll(t, h, h.Owner)                  // FullSnapshot pages
	testkit.AssertGolden(t, "testdata/golden/legacy-sync.json", goldenSync{Outcomes: run.Outcomes, Imports: run.Imports, HighWater: run.HighWater, FullPull: full, MidPull: mid, Snapshot: snap})
}
type goldenPull struct { Changes []json.RawMessage `json:"changes"`; HighWater json.RawMessage `json:"high_water"` }
type goldenSnapshot struct { Entities []json.RawMessage `json:"entities"`; HighWater json.RawMessage `json:"high_water"` }
// pullAll/snapshotAll json.Marshal each page (writeJSON's encoding minus the newline), decode into
// struct{Changes []json.RawMessage; HighWater json.RawMessage; NextPageToken *string} (RawMessage keeps exact bytes),
// then concatenate h.Normalize(change) across pages.
```
- `Server/internal/familyread/golden_test.go`, `TestLegacyFamilyReadCorpus`: build `newFixture(t)`, call `f.h.RunGolden(t)`, then record the normalized JSON of:
  - `ListBabies`;
  - `History` for baby A, all kinds, range 2026-09-01..2026-09-30;
  - page two of the same history query through `NextCursor`;
  - `Summary` for baby A over 2026-09-19..2026-09-22 in `Europe/Zurich`.
- `Server/internal/export/golden_test.go`, `TestLegacyExportCorpus`: `h.RunGolden(t)`, then `collectArchive` inside the requester's read tx, then the normalized `EncodeArchive` bytes, split into sections with `map[string]json.RawMessage`.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/sync ./internal/familyread ./internal/export' BT_TEST_RUN='TestLegacy' bash Server/scripts/test-db.sh`. Expected: `golden file testdata/golden/legacy-sync.json missing (set BT_GOLDEN_UPDATE=1)`, and the same for the other two.
- [ ] **Step 3: Implement and record.**
  - `AssertGolden` writes the indented JSON when `os.Getenv("BT_GOLDEN_UPDATE")=="1"`, otherwise it `bytes.Equal`s and prints the first differing path.
  - `RunGolden` fails the test on any `ApplyAndPull` error.
  - Record with `rtk proxy env BT_GOLDEN_UPDATE=1 BT_TEST_PACKAGES='./internal/sync ./internal/familyread ./internal/export' BT_TEST_RUN='TestLegacy' bash Server/scripts/test-db.sh`.
  - Inspect the files: the step 3 note must appear as `line sep <script>&` inside the entity bytes, and step 10's cascade must be one root and five child tombstones with consecutive sequences.
- [ ] **Step 4: Run green twice** with the Step 2 command and no update flag, to prove determinism.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/testkit/golden.go Server/internal/sync/golden_test.go Server/internal/sync/testdata Server/internal/familyread/golden_test.go Server/internal/familyread/testdata Server/internal/export/golden_test.go Server/internal/export/testdata && rtk git commit -m "test(sync): record the legacy wire, MCP and export corpus"`

### Task 2: Migration 000030 — care schema, additive

**Files:**
- Create `Server/migrations/000030_care_entities.sql`.
- Modify `Server/internal/migrate/compatibility.json` (`"min": 30, "max": 30`, append `{version:30, name:"000030_care_entities.sql", sha256:<shasum -a 256>}`).
- Modify `Server/internal/migrate/compatibility_test.go` (bounds and known count 30, digest row 30).
- Create `Server/internal/database/care_schema_test.go`.
- Bump every fully-migrated-fixture assertion from version 29 to 30:
  - `internal/database/{authority_schema_test.go:23,51, records_schema_test.go:30,68,80,153, roles_test.go, snapshot_schema_test.go, timer_schema_test.go, import_schema_test.go, delivery_schema_test.go, identity_schema_test.go, family_schema_test.go, refresh_recovery_schema_test.go}`
  - `internal/tenant/context_test.go`
  - `internal/capacity/workload_test.go`

  Assertions that migrate explicitly *through* 29 keep 29. `scripts/lib/disposable-sync.sh:277` changes `$schema == 29` to `30`.

**Interfaces:** Produces the DDL objects below, which every later task uses, and `database.VerifyRuntimeRole(ctx, runtime, 30)`.

- [ ] **Step 1: Write the failing test** `Server/internal/database/care_schema_test.go`:
```go
func TestCareSchemaIsVersionThirty(t *testing.T) {
	f := testpg.New(t)
	var v int64
	if err := f.Runtime.QueryRow(t.Context(), `SELECT current_version FROM schema_compatibility WHERE singleton`).Scan(&v); err != nil || v != 30 { t.Fatalf("version=%d err=%v", v, err) }
	if err := database.VerifyRuntimeRole(t.Context(), f.Runtime, 30); err != nil { t.Fatal(err) }
	var parts int; var strategy string
	_ = f.Admin.QueryRow(t.Context(), `SELECT count(*) FROM pg_inherits WHERE inhparent='public.care_entities'::regclass`).Scan(&parts)
	_ = f.Admin.QueryRow(t.Context(), `SELECT partstrat::text FROM pg_partitioned_table WHERE partrelid='public.care_entities'::regclass`).Scan(&strategy)
	if parts != 16 || strategy != "h" { t.Fatalf("partitions=%d strategy=%s", parts, strategy) }
	var unfilled, unprotected, granted int
	_ = f.Admin.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE NOT coalesce(c.reloptions @> ARRAY['fillfactor=90'],false)),
		count(*) FILTER (WHERE NOT (c.relrowsecurity AND c.relforcerowsecurity)),
		count(*) FILTER (WHERE has_table_privilege('baby_tracker_runtime', c.oid, 'SELECT,INSERT,UPDATE,DELETE'))
		FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid WHERE i.inhparent='public.care_entities'::regclass`).Scan(&unfilled, &unprotected, &granted)
	if unfilled+unprotected+granted != 0 { t.Fatalf("partitions: fillfactor %d, rls %d, runtime-granted %d", unfilled, unprotected, granted) }
}
func TestCareSchemaIndexesAreNamedQueries(t *testing.T) {
	f := testpg.New(t)
	want := []string{"care_entities_baby_match_idx", "care_entities_history_idx", "care_entities_one_growth_profile", "care_entities_one_live_timer",
		"care_entities_pkey", "care_entities_seq_key", "care_entities_session_idx", "care_entities_snapshot_idx"}
	rows, _ := f.Admin.Query(t.Context(), `SELECT indexrelid::regclass::text FROM pg_index WHERE indrelid='public.care_entities'::regclass ORDER BY 1`)
	got, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	if !slices.Equal(got, want) { t.Fatalf("care_entities indexes %v, want %v", got, want) }
}
func TestCareSchemaRuntimeGrantsAreExact(t *testing.T) {
	f := testpg.New(t)
	for _, c := range []struct{ sql string; want bool }{
		{`has_table_privilege(current_user,'public.care_entities','SELECT,INSERT')`, true},
		{`has_table_privilege(current_user,'public.care_entities','DELETE') OR has_table_privilege(current_user,'public.care_entities','TRUNCATE')`, false},
		{`has_column_privilege(current_user,'public.care_entities','wire','UPDATE') AND has_column_privilege(current_user,'public.care_entities','changed_seq','UPDATE')`, true},
		{`has_column_privilege(current_user,'public.care_entities','kind','UPDATE') OR has_column_privilege(current_user,'public.care_entities','family_id','UPDATE') OR has_column_privilege(current_user,'public.care_entities','creator_account_id','UPDATE') OR has_column_privilege(current_user,'public.care_entities','timer_kind','UPDATE')`, false},
		{`has_table_privilege(current_user,'public.care_receipts','UPDATE') OR has_table_privilege(current_user,'public.care_receipts','DELETE')`, false},
		{`has_column_privilege(current_user,'public.families','care_charged_bytes','UPDATE') AND has_column_privilege(current_user,'public.families','care_wire_bytes','UPDATE')`, true},
	} {
		var got bool
		if err := f.Runtime.QueryRow(t.Context(), `SELECT `+c.sql).Scan(&got); err != nil || got != c.want { t.Fatalf("%s = %v (%v)", c.sql, got, err) }
	}
}
```
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/database ./internal/migrate' BT_TEST_RUN='TestCareSchema|TestCompatibility' bash Server/scripts/test-db.sh`. Expected: `version=29` and `relation "public.care_entities" does not exist`.
- [ ] **Step 3: Write the migration** `Server/migrations/000030_care_entities.sql` (complete DDL):
```sql
-- 000030: care data in one hash-partitioned entity table (Phase 2).
-- Additive until Phase 2 Task 9 appends section 8 (removal of the legacy care
-- storage). Fresh installs only, like 000029; existing installs reach this
-- schema through the Phase 4 importer.

-- 1. Closed sets. care_kind's declaration order is the snapshot key order
-- (legacy sync/snapshot.go:130-150 snapshotKindOrder), so ORDER BY kind, id
-- pages a snapshot in the order the staged snapshot used.
CREATE TYPE app.care_kind AS ENUM ('baby','feed','sleep','diaper','timer','supplement','growth_profile','measurement');
CREATE TYPE app.care_operation AS ENUM ('create','update','delete','timer_start','timer_finish_bottle','timer_stop');
CREATE TYPE app.timer_kind AS ENUM ('feed','sleep');
CREATE TYPE app.page_kind AS ENUM ('sync','snapshot');

-- 2. Usage and snapshot sizing on the family row the write batch already locks.
ALTER TABLE public.families
  ADD COLUMN care_record_count bigint NOT NULL DEFAULT 0 CHECK (care_record_count BETWEEN 0 AND 9007199254740991),
  ADD COLUMN care_charged_bytes bigint NOT NULL DEFAULT 0 CHECK (care_charged_bytes BETWEEN 0 AND 9007199254740991),
  ADD COLUMN care_entity_count bigint NOT NULL DEFAULT 0 CHECK (care_entity_count BETWEEN 0 AND 9007199254740991),
  ADD COLUMN care_wire_bytes bigint NOT NULL DEFAULT 0 CHECK (care_wire_bytes BETWEEN 0 AND 9007199254740991);

-- 3. Every care record. Typed columns filter and aggregate; derived columns
-- are computed by care.Encode at write time; wire is the canonical entity as
-- encoding/json writes it, served verbatim. A tombstone keeps its last typed
-- columns (baby, start, session, …) with zero derived values.
CREATE TABLE public.care_entities (
  family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
  id uuid NOT NULL CHECK (id <> '00000000-0000-0000-0000-000000000000'),
  kind app.care_kind NOT NULL,
  baby_id uuid CHECK (baby_id <> '00000000-0000-0000-0000-000000000000'),
  entity_version bigint NOT NULL CHECK (entity_version BETWEEN 1 AND 9007199254740991),
  changed_seq bigint NOT NULL CHECK (changed_seq BETWEEN 1 AND 9007199254740991),
  creator_account_id uuid NOT NULL CHECK (creator_account_id <> '00000000-0000-0000-0000-000000000000'),
  actor_account_id uuid NOT NULL CHECK (actor_account_id <> '00000000-0000-0000-0000-000000000000'),   -- change envelope actor_account_id
  device_id uuid NOT NULL CHECK (device_id <> '00000000-0000-0000-0000-000000000000'),                 -- change envelope device_id
  operation app.care_operation NOT NULL,                                                               -- change envelope operation
  accepted_at timestamptz NOT NULL CHECK (isfinite(accepted_at)),
  tombstoned_at timestamptz CHECK (tombstoned_at IS NULL OR (isfinite(tombstoned_at) AND tombstoned_at >= accepted_at)),
  started_at timestamptz CHECK (started_at IS NULL OR (isfinite(started_at) AND started_at >= '1900-01-01T00:00:00Z' AND started_at < '2101-01-01T00:00:00Z')),
  ended_at timestamptz CHECK (ended_at IS NULL OR isfinite(ended_at)),
  session_id uuid,
  timer_kind app.timer_kind,
  completed_record_id uuid CHECK (completed_record_id <> '00000000-0000-0000-0000-000000000000'),
  sort_order integer CHECK (sort_order BETWEEN 0 AND 1000000),
  label text CHECK (char_length(btrim(label)) BETWEEN 1 AND 64 AND octet_length(label) <= 256),      -- supplement name
  name_hash bytea CHECK (octet_length(name_hash) = 32),                                              -- baby match hash
  breast_seconds integer NOT NULL DEFAULT 0 CHECK (breast_seconds BETWEEN 0 AND 604800),
  bottle_volume_ml numeric(13,6) CHECK (bottle_volume_ml BETWEEN 0 AND 1024000),
  bottle_observations smallint NOT NULL DEFAULT 0 CHECK (bottle_observations BETWEEN 0 AND 512),
  bottle_missing smallint NOT NULL DEFAULT 0 CHECK (bottle_missing BETWEEN 0 AND 512),
  shortest_part_seconds integer CHECK (shortest_part_seconds BETWEEN 1 AND 604800),                  -- zero-duration second line
  wire bytea NOT NULL CHECK (octet_length(wire) BETWEEN 64 AND 65536),
  PRIMARY KEY (family_id, id),
  CONSTRAINT care_entities_seq_key UNIQUE (family_id, changed_seq),
  CONSTRAINT care_entities_seven_days CHECK (ended_at IS NULL OR (started_at IS NOT NULL AND ended_at >= started_at AND ended_at <= started_at + interval '7 days')),
  CONSTRAINT care_entities_kind_shape CHECK (
        (kind = 'baby') = (baby_id IS NULL)
    AND (kind = 'baby') = (sort_order IS NOT NULL)
    AND (kind = 'baby') = (name_hash IS NOT NULL)
    AND (kind = 'timer') = (timer_kind IS NOT NULL)
    AND (kind = 'supplement') = (label IS NOT NULL)
    AND (kind IN ('feed','sleep','diaper','timer','supplement','measurement')) = (started_at IS NOT NULL)
    AND (ended_at IS NULL OR kind IN ('feed','sleep','diaper'))
    AND (session_id IS NULL OR kind IN ('feed','sleep','timer'))
    AND (completed_record_id IS NULL OR (kind = 'timer' AND tombstoned_at IS NOT NULL))
    AND (kind = 'feed' OR (breast_seconds = 0 AND bottle_volume_ml IS NULL AND bottle_observations = 0 AND bottle_missing = 0))
    AND (shortest_part_seconds IS NULL OR kind IN ('feed','timer'))
    AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0))
    AND (tombstoned_at IS NULL OR (breast_seconds = 0 AND bottle_observations = 0 AND bottle_missing = 0 AND shortest_part_seconds IS NULL)))
) PARTITION BY HASH (family_id);
-- A record's baby is a row of the same family (second line for baby_not_found).
ALTER TABLE public.care_entities ADD CONSTRAINT care_entities_baby_fk FOREIGN KEY (family_id, baby_id) REFERENCES public.care_entities(family_id, id);
DO $$ DECLARE r int; part text; BEGIN
  FOR r IN 0..15 LOOP
    part := format('care_entities_p%s', lpad(r::text, 2, '0'));
    EXECUTE format('CREATE TABLE public.%I PARTITION OF public.care_entities FOR VALUES WITH (MODULUS 16, REMAINDER %s) WITH (fillfactor = 90)', part, r);
    EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', part);
    EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', part);
    EXECUTE format('CREATE POLICY family_context ON public.%I USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id())', part);
    EXECUTE format('REVOKE ALL ON public.%I FROM PUBLIC', part);
  END LOOP;
END $$;
-- Index -> named request-path query (and its second user):
-- pkey (family_id,id):            care.Fetch by id (write path), care.Get/GetMany (familyread entries, importer), FK target.
-- care_entities_seq_key:          care.ReadPull/QueueChanges `changed_seq > $after AND <= $hw ORDER BY changed_seq` (incremental pull, continuations, export journal).
CREATE INDEX care_entities_snapshot_idx ON public.care_entities (family_id, kind, id);
--                                 care.ReadSnapshotPage `(kind,id) > ($k,$id) ORDER BY kind,id` (snapshot pages); care.Stream (export).
CREATE INDEX care_entities_history_idx ON public.care_entities (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE tombstoned_at IS NULL;
--                                 familyread history candidates + summary CTEs (sleep overlap reads ended_at from the index); cascade targets `baby_id = ANY($babies)`.
CREATE INDEX care_entities_session_idx ON public.care_entities (family_id, session_id, baby_id, kind, started_at) WHERE tombstoned_at IS NULL AND session_id IS NOT NULL;
--                                 write-path session owner check `session_id = ANY($sessions)`; familyread earliest session member.
CREATE INDEX care_entities_baby_match_idx ON public.care_entities (family_id, name_hash) WHERE kind = 'baby' AND tombstoned_at IS NULL;
--                                 importer match_shared `name_hash = $1`; live-baby count for max_live_babies; ListBabies filter.
CREATE UNIQUE INDEX care_entities_one_live_timer ON public.care_entities (family_id, baby_id, timer_kind) WHERE kind = 'timer' AND tombstoned_at IS NULL;
--                                 node-wide rule (one live timer per kind per baby); write-path timer_start lookup; familyread active timers.
CREATE UNIQUE INDEX care_entities_one_growth_profile ON public.care_entities (family_id, baby_id) WHERE kind = 'growth_profile';
--                                 node-wide rule (one growth profile per baby, as growth_profiles UNIQUE(family_id,baby_id)).
ALTER TABLE public.care_entities ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.care_entities FORCE ROW LEVEL SECURITY;
CREATE POLICY family_context ON public.care_entities USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id());

-- 4. Mutation receipts: idempotency by (device, mutation id); 30-day retention.
CREATE TABLE public.care_receipts (
  device_id uuid NOT NULL CHECK (device_id <> '00000000-0000-0000-0000-000000000000'),
  mutation_id uuid NOT NULL CHECK (mutation_id <> '00000000-0000-0000-0000-000000000000'),
  family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
  account_id uuid NOT NULL CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
  semantic_hash bytea NOT NULL CHECK (octet_length(semantic_hash) = 32),
  outcome jsonb NOT NULL CHECK (jsonb_typeof(outcome) = 'object'),
  accepted_at timestamptz NOT NULL CHECK (isfinite(accepted_at)),
  PRIMARY KEY (device_id, mutation_id));                               -- care.Fetch receipt lookup `device_id=$1 AND mutation_id=ANY($2)`
CREATE INDEX care_receipts_retention_idx ON public.care_receipts (accepted_at); -- 30-day retention sweep (Phase 3 worker)
ALTER TABLE public.care_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.care_receipts FORCE ROW LEVEL SECURITY;
-- The account sees its own receipts in any family, so a device that reuses a
-- mutation id after changing families is refused mutation_id_reused before any
-- write (app.save_mutation_receipt's collision, without a definer function).
CREATE POLICY care_receipts_read ON public.care_receipts FOR SELECT USING (family_id = app.current_family_id() OR account_id = app.current_account_id());
CREATE POLICY care_receipts_write ON public.care_receipts FOR INSERT WITH CHECK (family_id = app.current_family_id() AND account_id = app.current_account_id());

-- 5. Continuation bindings (token format unchanged; the lookup digest keys the row).
CREATE TABLE public.sync_page_bindings (
  family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
  token_digest bytea NOT NULL CHECK (octet_length(token_digest) = 32),
  token_key_id text NOT NULL CHECK (token_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
  account_id uuid NOT NULL, device_id uuid NOT NULL, session_id uuid NOT NULL,
  server_instance_id uuid NOT NULL CHECK (server_instance_id <> '00000000-0000-0000-0000-000000000000'),
  recovery_epoch bigint NOT NULL CHECK (recovery_epoch BETWEEN 1 AND 9007199254740991),
  security_revision bigint NOT NULL CHECK (security_revision BETWEEN 1 AND 9007199254740991),
  kind app.page_kind NOT NULL,
  high_water_sequence bigint NOT NULL CHECK (high_water_sequence BETWEEN 0 AND 9007199254740991),
  resume_sequence bigint CHECK (resume_sequence BETWEEN 0 AND 9007199254740991),
  snapshot_id uuid, resume_kind app.care_kind, resume_id uuid,
  item_limit integer NOT NULL CHECK (item_limit BETWEEN 1 AND 1000),
  byte_limit integer NOT NULL CHECK (byte_limit BETWEEN 65536 AND 4194304),
  created_at timestamptz NOT NULL CHECK (isfinite(created_at)),
  expires_at timestamptz NOT NULL CHECK (isfinite(expires_at) AND expires_at > created_at),
  PRIMARY KEY (family_id, token_digest),                               -- continuation lookup by token digest
  CHECK ((kind = 'sync' AND resume_sequence IS NOT NULL AND resume_sequence <= high_water_sequence AND snapshot_id IS NULL AND resume_kind IS NULL AND resume_id IS NULL)
      OR (kind = 'snapshot' AND resume_sequence IS NULL AND snapshot_id IS NOT NULL AND resume_kind IS NOT NULL AND resume_id IS NOT NULL)));
CREATE INDEX sync_page_bindings_expiry_idx ON public.sync_page_bindings (expires_at); -- TTL sweep (Phase 3 worker)
ALTER TABLE public.sync_page_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.sync_page_bindings FORCE ROW LEVEL SECURITY;
CREATE POLICY family_context ON public.sync_page_bindings
  USING (family_id = app.current_family_id() AND account_id = app.current_account_id())
  WITH CHECK (family_id = app.current_family_id() AND account_id = app.current_account_id());

-- 6. Snapshot sessions: the recorded high water and admission (2/account, 8/family); no entity copy.
CREATE TABLE public.sync_snapshot_sessions (
  family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
  id uuid NOT NULL CHECK (id <> '00000000-0000-0000-0000-000000000000'),
  account_id uuid NOT NULL, device_id uuid NOT NULL, session_id uuid NOT NULL,
  server_instance_id uuid NOT NULL, recovery_epoch bigint NOT NULL CHECK (recovery_epoch BETWEEN 1 AND 9007199254740991),
  security_revision bigint NOT NULL CHECK (security_revision BETWEEN 1 AND 9007199254740991),
  high_water_sequence bigint NOT NULL CHECK (high_water_sequence BETWEEN 0 AND 9007199254740991),
  captured_at timestamptz NOT NULL CHECK (isfinite(captured_at)),
  expires_at timestamptz NOT NULL CHECK (isfinite(expires_at) AND expires_at > captured_at),
  PRIMARY KEY (family_id, id));                                        -- snapshot continuation lookup
CREATE INDEX sync_snapshot_sessions_account_idx ON public.sync_snapshot_sessions (account_id, expires_at); -- active snapshots per account
CREATE INDEX sync_snapshot_sessions_family_idx ON public.sync_snapshot_sessions (family_id, expires_at);   -- active snapshots per family
ALTER TABLE public.sync_snapshot_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshot_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY snapshot_admission_read ON public.sync_snapshot_sessions FOR SELECT USING (family_id = app.current_family_id() OR account_id = app.current_account_id());
CREATE POLICY snapshot_write ON public.sync_snapshot_sessions FOR INSERT WITH CHECK (family_id = app.current_family_id() AND account_id = app.current_account_id());

-- 7. Grants (runtime; column-scoped UPDATE; no DELETE anywhere).
REVOKE ALL ON public.care_entities, public.care_receipts, public.sync_page_bindings, public.sync_snapshot_sessions FROM PUBLIC;
GRANT SELECT, INSERT ON public.care_entities, public.care_receipts, public.sync_page_bindings, public.sync_snapshot_sessions TO baby_tracker_runtime;
GRANT UPDATE (baby_id, entity_version, changed_seq, actor_account_id, device_id, operation, accepted_at, tombstoned_at, started_at, ended_at, session_id,
  completed_record_id, sort_order, label, name_hash, breast_seconds, bottle_volume_ml, bottle_observations, bottle_missing, shortest_part_seconds, wire)
  ON public.care_entities TO baby_tracker_runtime;
GRANT UPDATE (care_record_count, care_charged_bytes, care_entity_count, care_wire_bytes) ON public.families TO baby_tracker_runtime;

-- Manifest 30 = manifest 29 + the rows above. Compatibility 30.
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
  SELECT 30,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=29;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
  (30,'baby_tracker_runtime','table','public.care_entities','SELECT,INSERT'),
  (30,'baby_tracker_runtime','table','public.care_receipts','SELECT,INSERT'),
  (30,'baby_tracker_runtime','table','public.sync_page_bindings','SELECT,INSERT'),
  (30,'baby_tracker_runtime','table','public.sync_snapshot_sessions','SELECT,INSERT'),
  (30,'baby_tracker_runtime','column','public.families.care_record_count','UPDATE'),
  (30,'baby_tracker_runtime','column','public.families.care_charged_bytes','UPDATE'),
  (30,'baby_tracker_runtime','column','public.families.care_entity_count','UPDATE'),
  (30,'baby_tracker_runtime','column','public.families.care_wire_bytes','UPDATE'),
  -- one ('column','public.care_entities.<c>','UPDATE') row for each of the 21 columns in the GRANT UPDATE above, listed explicitly:
  (30,'baby_tracker_runtime','column','public.care_entities.baby_id','UPDATE'), (30,'baby_tracker_runtime','column','public.care_entities.entity_version','UPDATE'),
  (30,'baby_tracker_runtime','column','public.care_entities.changed_seq','UPDATE'), (30,'baby_tracker_runtime','column','public.care_entities.actor_account_id','UPDATE'),
  (30,'baby_tracker_runtime','column','public.care_entities.device_id','UPDATE'), (30,'baby_tracker_runtime','column','public.care_entities.operation','UPDATE'),
  (30,'baby_tracker_runtime','column','public.care_entities.accepted_at','UPDATE'), (30,'baby_tracker_runtime','column','public.care_entities.tombstoned_at','UPDATE'),
  (30,'baby_tracker_runtime','column','public.care_entities.started_at','UPDATE'), (30,'baby_tracker_runtime','column','public.care_entities.ended_at','UPDATE'),
  (30,'baby_tracker_runtime','column','public.care_entities.session_id','UPDATE'), (30,'baby_tracker_runtime','column','public.care_entities.completed_record_id','UPDATE'),
  (30,'baby_tracker_runtime','column','public.care_entities.sort_order','UPDATE'), (30,'baby_tracker_runtime','column','public.care_entities.label','UPDATE'),
  (30,'baby_tracker_runtime','column','public.care_entities.name_hash','UPDATE'), (30,'baby_tracker_runtime','column','public.care_entities.breast_seconds','UPDATE'),
  (30,'baby_tracker_runtime','column','public.care_entities.bottle_volume_ml','UPDATE'), (30,'baby_tracker_runtime','column','public.care_entities.bottle_observations','UPDATE'),
  (30,'baby_tracker_runtime','column','public.care_entities.bottle_missing','UPDATE'), (30,'baby_tracker_runtime','column','public.care_entities.shortest_part_seconds','UPDATE'),
  (30,'baby_tracker_runtime','column','public.care_entities.wire','UPDATE');
UPDATE public.schema_compatibility SET current_version=30, introduced_by='000030_care_entities.sql', recorded_at=now() WHERE singleton;
```
- [ ] **Step 4: Run green:** `rtk proxy env BT_TEST_PACKAGES='./internal/database ./internal/migrate ./internal/tenant' bash Server/scripts/test-db.sh`, then `rtk make server-test`. The whole suite stays green because legacy care code still uses its tables.
- [ ] **Step 5: Commit:** `rtk git add Server/migrations/000030_care_entities.sql Server/internal/migrate Server/internal/database Server/internal/tenant Server/internal/capacity/workload_test.go Server/scripts/lib/disposable-sync.sh && rtk git commit -m "feat(schema): add hash-partitioned care entities and bump compatibility to 30"`

### Task 3: The one encoder — wire bytes, derived columns, encoded responses

**Files:**
- Create `Server/internal/care/{doc.go,entity.go,derived.go,namehash.go,entity_test.go}`.
- Modify `Server/internal/protocol/sync.go` and `Server/internal/protocol/normalization.go`.
- Create `Server/internal/protocol/encoded_test.go`.

**Interfaces:** Consumes `protocol.CanonicalEntity`, `protocol.NormalizeBabyName`, `protocol.NormalizationVersion`, and the Task 1 corpus (`../sync/testdata/golden/legacy-sync.json`). Produces:
```go
package care
const RowOverheadBytes, LiveReserveBytes = 256, 4096
type Derived struct { BreastSeconds int32; BottleVolumeML *protocol.Decimal; BottleObservations, BottleMissing int16; ShortestPartSeconds *int32 }
type Change struct { Seq uint64; ActorAccountID, DeviceID, Operation string; AcceptedAt time.Time; CompletedRecordID *string }
type Row struct {
	FamilyID, ID, Kind string; BabyID *string; EntityVersion, ChangedSeq uint64
	CreatorAccountID, ActorAccountID, DeviceID, Operation string; AcceptedAt time.Time; TombstonedAt *time.Time
	StartedAt, EndedAt *time.Time; SessionID, TimerKind, CompletedRecordID *string; SortOrder *int32; Label *string; NameHash []byte
	Derived Derived; Wire []byte; WireBytes int // WireBytes = len(Wire) or octet_length(wire) when Wire is not loaded
}
func Encode(entity protocol.CanonicalEntity, previous *Row, change Change) (Row, error) // wire = json.Marshal(entity); tombstones keep previous typed columns
func (r Row) Entity() (protocol.CanonicalEntity, error)
func (r Row) Live() bool
func (r Row) ChargedBytes() int64              // WireBytes + 256, + 4096 while live
func (r Row) CountsAsRecord() bool             // live and kind not baby/timer
func DeriveFeed(components []protocol.FeedComponent) Derived
func BabyNameHash(name, birthDate string) ([32]byte, error) // sha256("baby-tracker-baby-match-v1\x00"||NormalizationVersion||0||NormalizeBabyName(name)||0||birthDate)

package protocol
func EncodeChange(cursor Cursor, kind, entityID string, version uint64, actorAccountID, deviceID, operation, acceptedAt string, entity json.RawMessage) (json.RawMessage, error)
// SyncResponse gains EncodedChanges []json.RawMessage `json:"-"`; SnapshotResponse gains EncodedEntities []json.RawMessage `json:"-"`.
func (response SyncResponse) MarshalJSON() ([]byte, error)     // EncodedChanges when non-nil, else Changes; field order unchanged
func (response SnapshotResponse) MarshalJSON() ([]byte, error)
func IncrementalPageEncodedBytesRaw(server ServerInfo, familyID string, highWater uint64, changeBytes, changeCount int, nextPageToken *string) int
func SnapshotPageEncodedBytesRaw(server ServerInfo, snapshotID string, highWater Cursor, entityBytes, entityCount int, nextPageToken *string) int
func SumDecimals(values []Decimal) Decimal
```
- [ ] **Step 1: Write the failing tests.**
  - `care/entity_test.go`:
    - `TestEncodeWireIsCanonicalMarshal`: for every kind in the corpus snapshot, `Encode(decoded, nil, change)`; then `Wire == json.Marshal(entity)` and `json.Marshal(row.Entity()) == Wire`. Tombstones keep the typed columns of `previous`.
    - `TestEncodeReproducesLegacyCorpus`: every `snapshot.entities[i]` and `full_pull.changes[i].snapshot_or_tombstone` raw value of `legacy-sync.json` equals `Encode(json.Unmarshal(raw)).Wire` byte for byte.
    - `TestWireServedVerbatimEscapesLikeEncoder` (Review Focus 3): notes and names with `<`, `>`, `&`, U+2028, U+2029, `"`, `\`, emoji and NFC-normalised accents. `json.NewEncoder(&b).Encode(SyncResponse{EncodedChanges: …})` must equal `json.NewEncoder(&b).Encode(SyncResponse{Changes: decoded})`.
    - `TestDeriveFeed`: breast 600+300 → 900; bottles 120.5 and 89.5 → volume 210, observations 2; solids only → zero; shortest timed part → min seconds.
    - `TestBabyNameHash`: `"Élie"` and `" élie "` give the same hash; a different birth date gives a different hash; the result is 32 bytes.
  - `protocol/encoded_test.go`:
    - `TestEncodeChangeMatchesJournalChange`: random cursors, operations and entities; the result equals `json.Marshal(JournalChange{…})`.
    - `TestEncodedResponsesMatchStructEncoding`: `MarshalJSON` with encoded slices equals the default encoding of the same response through `type plain SyncResponse`, including empty `changes: []`.
    - `TestRawPageSizesMatchMarshal`: `IncrementalPageEncodedBytesRaw`/`SnapshotPageEncodedBytesRaw` equal `IncrementalPageEncodedBytes`/`SnapshotPageEncodedBytes` for 0, 1 and n items, with and without a token.
- [ ] **Step 2: Run to see the failure:** `rtk go -C Server test ./internal/care ./internal/protocol -run 'TestEncode|TestWireServed|TestDerive|TestBabyNameHash|TestRawPage'`. Expected: `undefined: care.Encode`, `undefined: protocol.EncodeChange`.
- [ ] **Step 3: Implement.**
  - `EncodeChange` marshals a struct with `JournalChange`'s field order and `SnapshotOrTombstone json.RawMessage`.
  - Each raw size helper marshals the empty envelope once and adds `bytes + max(count-1, 0)`.
  - `SumDecimals` sums coefficients at a common scale and normalises as `ParseDecimal` does.
  - `Encode`:
```go
func Encode(entity protocol.CanonicalEntity, previous *Row, c Change) (Row, error) {
	wire, err := json.Marshal(entity)
	if err != nil { return Row{}, err }
	r := Row{FamilyID: entity.Meta.FamilyID, ID: entity.Meta.ID, Kind: entity.EntityKind, EntityVersion: entity.Meta.EntityVersion, ChangedSeq: c.Seq,
		CreatorAccountID: entity.Meta.CreatorAccountID, ActorAccountID: c.ActorAccountID, DeviceID: c.DeviceID, Operation: c.Operation,
		AcceptedAt: c.AcceptedAt, CompletedRecordID: c.CompletedRecordID, Wire: wire, WireBytes: len(wire)}
	if entity.Meta.TombstonedAt != nil {
		if previous == nil { return Row{}, fmt.Errorf("care: tombstone of %s without its row", r.ID) }
		r.BabyID, r.StartedAt, r.EndedAt, r.SessionID, r.TimerKind = previous.BabyID, previous.StartedAt, previous.EndedAt, previous.SessionID, previous.TimerKind
		r.SortOrder, r.Label, r.NameHash = previous.SortOrder, previous.Label, previous.NameHash
		at := c.AcceptedAt; r.TombstonedAt = &at
		return r, nil
	}
	return r, typedColumns(&r, entity.Payload) // per payload type: baby_id, started/ended (measurement: measured_at), session (feeding/sleep/timer), timer_kind,
	                                           // sort_order + name_hash (baby), label (supplement name), Derived via DeriveFeed (feed) / shortest part (timer)
}
```
- [ ] **Step 4: Run green:** the Step 2 command, then `rtk go -C Server test ./internal/protocol/...`, which covers the existing encoding tests.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/care Server/internal/protocol && rtk git commit -m "feat(care): encode canonical wire entities once and serve them verbatim"`

### Task 4: Care store — fetch, write batch, pull, snapshot page, lookups

**Files:**
- Create `Server/internal/care/store.go`.
- Create `Server/internal/care/store_test.go` and `Server/internal/care/rls_test.go` (port of `records/rls_test.go` to the four new tables).
- Create `Server/internal/care/caretest/caretest.go`.
- Create `Server/internal/testkit/roundtrips.go`.

**Interfaces:** Consumes the Task 2 tables, Task 3 `Row`/`Encode`, `tenant.SetLocal`, `testkit.New`. Produces:
```go
package care
var ErrNotFound = errors.New("record_not_found")
var ErrQuotaExceeded = errors.New("family_record_quota_exceeded")
type FamilyState struct { Sequence, RecordCount, ChargedBytes, EntityCount, WireBytes uint64 }
func LockFamily(ctx context.Context, tx pgx.Tx, familyID string) (FamilyState, error)      // FOR NO KEY UPDATE (records.LockFamilyForUpdate semantics)
func ReadFamilyState(ctx context.Context, tx pgx.Tx, familyID string) (FamilyState, error)
type Refs struct { IDs, StoppedTimerIDs, SessionIDs, TimerBabyIDs, CascadeBabyIDs, MutationIDs []string; DeviceID string; CountLiveBabies bool }
type StoredReceipt struct { SameFamily bool; Hash [32]byte; Outcome protocol.Outcome }
type Fetched struct { Family FamilyState; Rows map[string]Row; SessionMembers, LiveTimers, CascadeTargets []Row; LiveBabies int; Receipts map[string]StoredReceipt }
func Fetch(ctx context.Context, tx pgx.Tx, familyID string, refs Refs) (Fetched, error) // ONE pipelined round trip; family lock first
type ReceiptRow struct { DeviceID, MutationID, AccountID string; Hash [32]byte; Outcome protocol.Outcome; AcceptedAt time.Time }
type CounterDelta struct { Records, ChargedBytes, Entities, WireBytes int64 }
type Writes struct { FamilyID string; Base FamilyState; Sequence uint64; Rows []Row; Receipts []ReceiptRow; Delta CounterDelta }
func QueueWrites(b *pgx.Batch, w Writes) (check func(pgx.BatchResults) error)
type ChangeRow struct { Seq uint64; Kind, ID string; Version uint64; ActorAccountID, DeviceID, Operation string; AcceptedAt time.Time; Wire []byte }
func (c ChangeRow) Encode(server protocol.ServerInfo, familyID string) (json.RawMessage, error)
func QueueChanges(b *pgx.Batch, familyID string, after, through uint64, limit int) (read func(pgx.BatchResults) ([]ChangeRow, error))
func ReadPull(ctx context.Context, tx pgx.Tx, familyID string, after uint64, limit int) (highWater uint64, rows []ChangeRow, err error) // one statement
type SnapshotKey struct { Kind, ID string }
var FirstSnapshotKey = SnapshotKey{Kind: "baby", ID: "00000000-0000-0000-0000-000000000000"}
type SnapshotRow struct { Key SnapshotKey; Wire []byte }
func QueueSnapshotPage(b *pgx.Batch, familyID string, after SnapshotKey, limit int) (read func(pgx.BatchResults) ([]SnapshotRow, error))
func Get(ctx context.Context, tx pgx.Tx, familyID, id string) (Row, error)
func GetMany(ctx context.Context, tx pgx.Tx, familyID string, ids []string) (map[string]Row, error)
func Stream(ctx context.Context, tx pgx.Tx, familyID string, visit func(Row) error) error            // (kind,id) order, tombstones included
func StreamChanges(ctx context.Context, tx pgx.Tx, familyID string, through uint64, visit func(ChangeRow) error) error
func MatchBabies(ctx context.Context, tx pgx.Tx, familyID string, hash [32]byte) ([]Row, error)  // live babies by name hash, id order
func ChargeImport(ctx context.Context, tx pgx.Tx, familyID string, bytes int64, limits protocol.Limits) error

package caretest
func TenantTx(t *testing.T, pool *pgxpool.Pool, accountID, familyID string) pgx.Tx
func Write(t *testing.T, h *testkit.Harness, rows ...care.Row) error // tenant tx: QueueWrites + commit, counters from rows
func Rows(t *testing.T, h *testkit.Harness) map[string]care.Row     // admin read of every family row

package testkit
type Wire struct{ /* counts writes after reads, as authority/roundtrip_test.go:20-95 */ }
func CountedPool(t *testing.T, base *pgxpool.Pool) (*pgxpool.Pool, *Wire) // MaxConns 1, no ping on acquire
func (w *Wire) Reset(); func (w *Wire) Trips() int; func (w *Wire) Statements() []string
```
Key SQL:
```sql
-- Fetch (one pgx.Batch; statements 4-7 only when the refs ask):
-- 1 SELECT next_sequence,care_record_count,care_charged_bytes,care_entity_count,care_wire_bytes FROM families WHERE id=$1 FOR NO KEY UPDATE
-- 2 SELECT mutation_id::text, family_id=$2, semantic_hash, outcome FROM care_receipts WHERE device_id=$1 AND mutation_id=ANY($3::uuid[])
-- 3 SELECT <rowColumns> FROM care_entities WHERE family_id=$1 AND (id=ANY($2::uuid[]) OR id IN
--      (SELECT completed_record_id FROM care_entities WHERE family_id=$1 AND id=ANY($3::uuid[]) AND completed_record_id IS NOT NULL))
-- 4 SELECT <rowColumns minus wire>, octet_length(wire) FROM care_entities WHERE family_id=$1 AND tombstoned_at IS NULL AND session_id=ANY($2::uuid[])
-- 5 SELECT <rowColumns> FROM care_entities WHERE family_id=$1 AND kind='timer' AND tombstoned_at IS NULL AND baby_id=ANY($2::uuid[])
-- 6 SELECT <rowColumns minus wire>, octet_length(wire) FROM care_entities WHERE family_id=$1 AND tombstoned_at IS NULL AND baby_id=ANY($2::uuid[])
-- 7 SELECT count(*) FROM care_entities WHERE family_id=$1 AND kind='baby' AND tombstoned_at IS NULL
-- QueueWrites:
INSERT INTO care_entities (family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,tombstoned_at,
  started_at,ended_at,session_id,timer_kind,completed_record_id,sort_order,label,name_hash,breast_seconds,bottle_volume_ml,bottle_observations,bottle_missing,shortest_part_seconds,wire)
SELECT $1, v.* FROM unnest($2::uuid[],$3::text[]::app.care_kind[],$4::uuid[],$5::bigint[],$6::bigint[],$7::uuid[],$8::uuid[],$9::uuid[],$10::text[]::app.care_operation[],
  $11::timestamptz[],$12::timestamptz[],$13::timestamptz[],$14::timestamptz[],$15::uuid[],$16::text[]::app.timer_kind[],$17::uuid[],$18::integer[],$19::text[],$20::bytea[],
  $21::integer[],$22::numeric[],$23::smallint[],$24::smallint[],$25::integer[],$26::bytea[]) AS v
ON CONFLICT (family_id, id) DO UPDATE SET baby_id=EXCLUDED.baby_id, entity_version=EXCLUDED.entity_version, changed_seq=EXCLUDED.changed_seq,
  actor_account_id=EXCLUDED.actor_account_id, device_id=EXCLUDED.device_id, operation=EXCLUDED.operation, accepted_at=EXCLUDED.accepted_at,
  tombstoned_at=EXCLUDED.tombstoned_at, started_at=EXCLUDED.started_at, ended_at=EXCLUDED.ended_at, session_id=EXCLUDED.session_id,
  completed_record_id=EXCLUDED.completed_record_id, sort_order=EXCLUDED.sort_order, label=EXCLUDED.label, name_hash=EXCLUDED.name_hash,
  breast_seconds=EXCLUDED.breast_seconds, bottle_volume_ml=EXCLUDED.bottle_volume_ml, bottle_observations=EXCLUDED.bottle_observations,
  bottle_missing=EXCLUDED.bottle_missing, shortest_part_seconds=EXCLUDED.shortest_part_seconds, wire=EXCLUDED.wire
WHERE care_entities.entity_version = EXCLUDED.entity_version - 1 AND care_entities.kind = EXCLUDED.kind;   -- check: rows affected == len(rows)
INSERT INTO care_receipts (device_id,mutation_id,family_id,account_id,semantic_hash,outcome,accepted_at)
SELECT $1, v.m, $2, $3, v.h, v.o, $4 FROM unnest($5::uuid[],$6::bytea[],$7::jsonb[]) AS v(m,h,o);
UPDATE families SET next_sequence=$2, care_record_count=care_record_count+$3, care_charged_bytes=care_charged_bytes+$4,
  care_entity_count=care_entity_count+$5, care_wire_bytes=care_wire_bytes+$6 WHERE id=$1 AND next_sequence=$7;   -- check: 1 row
-- ReadPull (high water and rows from one statement snapshot):
WITH hw AS (SELECT next_sequence FROM families WHERE id=$1)
SELECT hw.next_sequence, c.changed_seq, c.kind::text, c.id::text, c.entity_version, c.actor_account_id::text, c.device_id::text, c.operation::text, c.accepted_at, c.wire
FROM hw LEFT JOIN LATERAL (SELECT * FROM care_entities WHERE family_id=$1 AND changed_seq > $2 AND changed_seq <= hw.next_sequence ORDER BY changed_seq LIMIT $3) c ON true;
-- QueueSnapshotPage:
SELECT kind::text, id::text, wire FROM care_entities WHERE family_id=$1 AND (kind, id) > ($2::app.care_kind, $3::uuid) ORDER BY kind, id LIMIT $4;
```
- [ ] **Step 1: Write the failing tests** `care/store_test.go`:
```go
func TestCareEntitiesSecondLineChecks(t *testing.T) {
	h := testkit.New(t)
	baby := caretest.BabyRow(t, h, "11000000-0000-4000-8000-000000000001", 1)
	if err := caretest.Write(t, h, baby); err != nil { t.Fatal(err) }
	for _, c := range []struct{ name string; edit func(*care.Row); code string }{
		{"eight-day feed", func(r *care.Row) { end := r.StartedAt.Add(8 * 24 * time.Hour); r.EndedAt = &end }, "23514"},
		{"zero-second part", func(r *care.Row) { zero := int32(0); r.Derived.ShortestPartSeconds = &zero }, "23514"},
		{"negative bottle", func(r *care.Row) { r.Derived.BottleVolumeML, r.Derived.BottleObservations = protocol.MustParseDecimal("-1"), 1 }, "23514"},
		{"feed without baby", func(r *care.Row) { r.BabyID = nil }, "23514"},
		{"timer kind on a feed", func(r *care.Row) { k := "feed"; r.TimerKind = &k }, "23514"},
		{"unknown baby", func(r *care.Row) { other := "11000000-0000-4000-8000-0000000000ff"; r.BabyID = &other }, "23503"},
		{"reused sequence", func(r *care.Row) { r.ChangedSeq = baby.ChangedSeq }, "23505"},
	} {
		row := caretest.FeedRow(t, h, baby, "21000000-0000-4000-8000-000000000001", 2)
		c.edit(&row)
		var pgErr *pgconn.PgError
		if err := caretest.Write(t, h, row); !errors.As(err, &pgErr) || pgErr.Code != c.code { t.Fatalf("%s: %v, want %s", c.name, err, c.code) }
	}
}
func TestCareQueriesUseOnePartitionAndIndexes(t *testing.T) {
	// as the runtime in a tenant tx, EXPLAIN (FORMAT JSON) each named query (Fetch 3-7, ReadPull, snapshot page, MatchBabies, history candidate):
	// exactly one care_entities_pNN relation appears and the expected index name appears (care_entities_seq_key for ReadPull, …_snapshot_idx, …_history_idx, …_session_idx, …_baby_match_idx, …_one_live_timer)
}
func TestFetchIsOneRoundTrip(t *testing.T) { /* CountedPool; warm once; Reset; Fetch with all refs kinds set -> wire.Trips()==1 */ }
func TestReadPullIsOneStatementSnapshot(t *testing.T) { /* a writer commits rows at seq 5..6 between two ReadPull calls; each call's rows all have seq <= its returned high water, and every row <= hw is returned */ }
func TestSnapshotPageKeyOrder(t *testing.T) { /* one row per kind with ids descending; pages of 2 follow baby,feed,sleep,diaper,timer,supplement,growth_profile,measurement then id */ }
func TestChargeImportEnforcesQuota(t *testing.T) { /* charged bytes at limit-10; ChargeImport(+20) -> ErrQuotaExceeded; +5 ok */ }
func TestMatchBabiesUsesNameHash(t *testing.T) { /* " élie " and "Élie" same birth date match; a tombstoned baby does not */ }
```
`care/rls_test.go`, `TestCareRLSIsolatesFamilies`:
- family A's tenant reads none of family B's rows on all four tables;
- an insert with family B's id raises `42501`;
- the account sees its own receipt from a previous family, but not another account's.

- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/care' BT_TEST_RUN='TestCare|TestFetch|TestReadPull|TestSnapshotPage|TestChargeImport|TestMatchBabies' bash Server/scripts/test-db.sh`. Expected: `undefined: care.Fetch` and similar.
- [ ] **Step 3: Implement** `store.go` with the SQL above (one `rowColumns` constant and one scan function) and the caretest helpers. Copy the counted pool from `authority/roundtrip_test.go:76-122` into exported `testkit.CountedPool`.
- [ ] **Step 4: Run green:** the Step 2 command.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/care Server/internal/testkit/roundtrips.go && rtk git commit -m "feat(care): add the care store with one-round-trip fetch and pipelined writes"`

### Task 5: Set-based batch apply — records, receipts, LWW, quotas

**Files:**
- Create `Server/internal/care/{apply.go,plan.go,records.go,apply_test.go,roundtrip_test.go,records_test.go}`.
- Move `Server/internal/records/validation.go` to `Server/internal/care/validate.go` (and `validation_test.go` to `care/validate_test.go`); `records` keeps `func ValidateMutation(m protocol.Mutation, l protocol.Limits) error { return care.ValidateMutation(m, l) }` until Task 9.
- Port into `care/records_test.go` the cases of `records/components_test.go`, `sleep_sessions_test.go`, `growth_test.go` and `repository_test.go`, driven through `ApplyBatch`.

**Interfaces:** Consumes Task 4. Produces:
```go
package care
type Policy func(role, action string) error                       // nil: no per-mutation role check (server-driven batches)
type View interface { Lookup(id string) (Row, bool) }               // working set: fetched rows + earlier writes of this batch
type ImportAdmission interface {
	AdmitRecord(ctx context.Context, tx pgx.Tx, g access.Grant, deviceID string, m protocol.Mutation, view View) (protocol.Outcome, bool, error)
}
type Publisher interface { Publish(ctx context.Context, familyID string, cursor protocol.Cursor) error }
type BatchCommand struct {
	Grant access.Grant; DeviceID string; Mutations []protocol.Mutation; AcceptedAt time.Time
	Server protocol.ServerInfo; Limits protocol.Limits; Policy Policy; Imports ImportAdmission
}
type PullRequest struct { After uint64; Limit int }
type BatchResult struct {
	Outcomes        []protocol.Outcome // the written prefix, in mutation order
	Base, HighWater uint64             // families.next_sequence before and after
	Changes         []ChangeRow        // the pull read (After, HighWater], when requested, After <= HighWater and Fatal == nil
	Fatal           error              // the first refusing error; everything before it is written
}
var ErrMutationIDReused = errors.New("mutation_id_reused")
var ErrFeatureUnavailable = errors.New("feature_unavailable")
func ActionFor(m protocol.Mutation) (string, error)                // sync/mutation.go:94-112 moved
func ApplyBatch(ctx context.Context, tx pgx.Tx, cmd BatchCommand, pull *PullRequest) (BatchResult, error) // exactly two round trips
```
Semantics are ported 1:1 from `records/repository.go:88-305` against the working set:
- **Kind probe.** Done in the working set (`entity_exists`, `entity_kind_collision`, `entity_not_found`).
- **Parent checks.** `record_parent_mismatch` uses the stored `baby_id` of a tombstone, replacing the `storedBabyID` read. `baby_not_found` and `baby_deleted` (with the baby entity) also apply.
- **Sessions.** `sessionOwnerMismatch` (`records/sessions.go:12-23`) is evaluated over fetched session members, fetched rows whose id is the session, and pending rows.
- **Measurements and supplements.** Measurements are checked against `measured_at`, acceptance time and birth date (`invalid_timestamp`). `record_logged_at_mismatch` applies to measurements and supplements.
- **Tombstoned targets.** Return `superseded/entity_deleted`.
- **Size.** `mutation_too_large` covers both the canonical bytes and `entityFitsMinimumPage`, computed with `IncrementalPageEncodedBytesRaw`/`SnapshotPageEncodedBytesRaw`.
- **Live babies.** `max_live_babies` counts fetched plus pending, otherwise `ErrQuotaExceeded`.
- **Versions.** LWW: version = existing+1, creator kept, `overwrote_newer_version = observed < existing`.
- **Quotas.** Per mutation, using running counters (deletions exempt).
- **Receipts.**
  - Read from `Fetched.Receipts` plus receipts of this batch.
  - Same family and same hash → `already_applied` with `original_status`.
  - Another family or another hash → Fatal `ErrMutationIDReused`.
  - Every non-replayed outcome gets a receipt.
- **Sequences.** Each applied change takes `ws.next()`; overflow past 2^53-1 is fatal.
- **Policy.** `cmd.Policy(cmd.Grant.Role, ActionFor(m))` runs before anything else for each mutation.
- **Imports.** `m.ImportID != nil` with nil `Imports` is Fatal `ErrFeatureUnavailable`.
- **Timers and baby deletes.** In this task `case m.EntityKind == "timer" || (m.EntityKind == "baby" && m.Operation == "delete")` returns `errTimerAndCascadeArriveInTask6`, an internal error; there are no callers yet.

- [ ] **Step 1: Write the failing tests:**
```go
func TestApplyBatchIsTwoRoundTripsAtAnySize(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	pool, wire := testkit.CountedPool(t, h.Runtime)
	for _, size := range []int{1, 20, 100} {
		batch := caretest.MixedBatch(caretest.BabyA, size) // feeds (some in one session), sleeps, diapers, supplements, measurements, one update of an earlier feed
		run := func(measure bool) care.BatchResult {
			tx := caretest.TenantTx(t, pool, h.Owner.AccountID, h.FamilyID)
			defer tx.Rollback(t.Context())
			if measure { wire.Reset() }
			result, err := care.ApplyBatch(t.Context(), tx, caretest.Command(h, h.Owner, batch), &care.PullRequest{After: 0, Limit: 201})
			if err != nil || result.Fatal != nil { t.Fatalf("size %d: %v %v", size, err, result.Fatal) }
			return result
		}
		run(false) // warms the per-connection statement cache with every statement this shape queues
		if result := run(true); wire.Trips() != 2 || len(result.Outcomes) != size { t.Fatalf("size %d: %d round trips, %d outcomes", size, wire.Trips(), len(result.Outcomes)) }
	}
}
func TestBatchFatalErrorCommitsPrefix(t *testing.T) { // Review Focus 2
	h := testkit.New(t)
	h.Limits.MaxLiveBabies = 1
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	batch := []protocol.Mutation{caretest.Feed(caretest.BabyA, 1), caretest.Baby(caretest.BabyB), caretest.Feed(caretest.BabyA, 2)}
	result := caretest.ApplyCommitted(t, h, h.Owner, batch) // tenant tx, ApplyBatch, commit
	if !errors.Is(result.Fatal, care.ErrQuotaExceeded) || len(result.Outcomes) != 1 { t.Fatalf("%v %d", result.Fatal, len(result.Outcomes)) }
	rows := caretest.Rows(t, h)
	if _, ok := rows[batch[0].EntityID]; !ok { t.Fatal("prefix feed not committed") }
	if _, ok := rows[batch[2].EntityID]; ok { t.Fatal("feed after the refusal committed") }
	replay := caretest.ApplyCommitted(t, h, h.Owner, batch[:1])
	if replay.Outcomes[0].Status != "already_applied" { t.Fatalf("retry %+v", replay.Outcomes[0]) }
	// viewer: first mutation refused read_only_caregiver by Policy; no row, no receipt, sequence unchanged
}
func TestBatchChainsMutationsOnOneEntity(t *testing.T) { // Review Focus 5
	// [feed create F, feed update F (observed 1), same mutation id as the update again] in one batch:
	// outcomes v1@base+1, v2@base+2, already_applied(original applied); row F v2 changed_seq base+2; HighWater base+2.
	// the same mutation id with a different payload in the same batch -> Fatal ErrMutationIDReused after two outcomes.
}
func TestLastWriterWinsAndOverwroteNewer(t *testing.T) { /* version 3, update observed 1 -> version 4, overwrote_newer_version true; observed 3 -> false */ }
func TestRecordRulesMatchLegacy(t *testing.T) {
	// table, one batch per case, each outcome compared field-for-field with records/repository.go:
	// entity_exists, entity_kind_collision, entity_not_found, record_parent_mismatch (live and tombstoned), baby_not_found, baby_deleted,
	// superseded entity_deleted, invalid_timestamp (measurement before birth / in the future), record_logged_at_mismatch (supplement, measurement),
	// record_parent_mismatch for a feeding session owned by another baby (pending in the same batch and committed), mutation_too_large
}
func TestReceiptReplayAndCrossFamilyReuse(t *testing.T) { /* device reuses a mutation id after leaving family A for B -> Fatal ErrMutationIDReused before any write */ }
func TestCountersMatchRows(t *testing.T) {
	// after 200 random record mutations: families.care_* == aggregates over care_entities
	// (records: live non-baby non-timer; charged: sum(wire+256)+4096*live; entities: count; wire: sum(octet_length))
}
```
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/care' BT_TEST_RUN='TestApplyBatch|TestBatch|TestLastWriter|TestRecordRules|TestReceipt|TestCounters|TestValidate|TestComponents|TestSleepSessions|TestGrowth' bash Server/scripts/test-db.sh`. Expected: `undefined: care.ApplyBatch`.
- [ ] **Step 3: Implement** (key shape):
```go
func ApplyBatch(ctx context.Context, tx pgx.Tx, cmd BatchCommand, pull *PullRequest) (BatchResult, error) {
	refs, err := collectRefs(cmd) // ids (entity, payload baby, timer completion, session ids), sessions, timer babies, cascade babies, mutation ids
	if err != nil { return BatchResult{}, err }
	fetched, err := Fetch(ctx, tx, cmd.Grant.FamilyID, refs) // round trip 1
	if err != nil { return BatchResult{}, err }
	ws := newWorkingSet(cmd, fetched)
	result := BatchResult{Base: fetched.Family.Sequence}
	for _, m := range cmd.Mutations {
		outcome, fatal, err := ws.apply(ctx, tx, m)
		if err != nil { return BatchResult{}, err } // infrastructure: the caller rolls back
		if fatal != nil { result.Fatal = fatal; break }
		result.Outcomes = append(result.Outcomes, outcome)
	}
	result.HighWater = ws.sequence
	batch := &pgx.Batch{}
	check := QueueWrites(batch, ws.writes()) // upsert of final rows, receipts, family counters + sequence
	var readPull func(pgx.BatchResults) ([]ChangeRow, error)
	if pull != nil && result.Fatal == nil && pull.After <= result.HighWater {
		readPull = QueueChanges(batch, cmd.Grant.FamilyID, pull.After, result.HighWater, pull.Limit)
	}
	if batch.Len() == 0 { return result, nil }
	results := tx.SendBatch(ctx, batch) // round trip 2
	defer results.Close()
	if err := check(results); err != nil { return BatchResult{}, err }
	if readPull != nil { if result.Changes, err = readPull(results); err != nil { return BatchResult{}, err } }
	return result, results.Close()
}
```
- [ ] **Step 4: Run green:** the Step 2 command, then `rtk proxy env BT_TEST_PACKAGES='./internal/records ./internal/sync' bash Server/scripts/test-db.sh` to confirm legacy callers still pass with the moved validation.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/care Server/internal/records && rtk git commit -m "feat(care): apply a sync batch set-based in two round trips"`

### Task 6: Timers and the baby-delete cascade in the planner

**Files:**
- Create `Server/internal/care/{timers.go,cascade.go,timers_test.go,cascade_test.go}`.
- Move the pure helpers `completedTimerPayload`, `cleanShortBreastComponents`, `cleanedFeedKind`, `parseTimerTime`, `timerHasComponent` and `breastPartCount` from `records/timers.go:504-588,677-723` into `care/timers.go`; `records` calls the care versions until Task 9.
- Port into `care/timers_test.go` the cases of `records/{timers_test.go,timer_edit_test.go,timed_bottle_test.go,timer_stop_cleanup_test.go,read_timer_test.go}`.
- Port into `care/cascade_test.go` the cases of `records/{child_delete_test.go,growth_cascade_test.go}`.

**Interfaces:** Consumes Task 5's working set. Produces no new exported names; it removes `errTimerAndCascadeArriveInTask6`.

Semantics are ported 1:1 from `records/timers.go:97-621`:
- **Start.**
  - Same id and same live payload → `superseded` at the row's sequence.
  - Same id otherwise → `entity_exists` or `entity_kind_collision`.
  - Live timer of that kind on the baby → `superseded` with it.
  - Session owner check applies.
  - Charges the reserve.
- **Finish bottle.** `invalid_timer_kind` and `invalid_timer_time` (1..604800 s, no duplicate part).
- **Edit.** The `editTimer` rules; a no-op is `applied` at the current cursor with no sequence.
- **Stop.**
  - `active_bottle_unfinished`; `invalid_timer_time` (7 days); `completed_record_exists` (working-set probe of the completion id).
  - Tombstone (operation `timer_stop`, `completed_record_id` set), then the completion (operation `timer_stop`, version 1) as two consecutive sequences.
  - Outcome: tombstone plus `related_entities: [completion]`, cursor = completion sequence.
- **Ended timer.** Finish, edit or stop → `superseded/timer_ended` with the completion; the cursor is the completion's `changed_seq`, from the row fetched through `StoppedTimerIDs`.
- **Overwrote newer.** `observed < version-1`.

The cascade is ported from `records/child_delete.go`:
- Lock-free inside the family lock.
- Targets are fetched cascade rows plus pending live rows of that baby.
- Sequence order: root first, then feed, sleep, diaper, supplement, growth_profile, measurement, timer, each by id.
- Each child becomes a tombstone row via `Encode(tombstone, &target, Change{Operation: "delete"})` with version+1, creator kept and the deleting account as last editor.
- Counters: records minus non-timer children; charged minus the reserve plus the wire delta for each row.
- Timers are tombstoned without a completion.
- Outcome: the root tombstone with the final cursor.

- [ ] **Step 1: Write the failing tests:**
```go
func TestTimerLifecycleInOneBatch(t *testing.T) { // Review Focus 5
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	start, finish, edit, stop := caretest.FeedTimerLifecycle(caretest.BabyA, caretest.TimerA, caretest.CompletionA)
	result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{start, finish, edit, stop})
	base := result.Base
	want := []uint64{base + 1, base + 2, base + 3, base + 5} // start, finish, edit, stop's final cursor (tombstone base+4, completion base+5)
	for i, o := range result.Outcomes { if o.Status != "applied" || o.Cursor.Sequence != want[i] { t.Fatalf("outcome %d %+v", i, o) } }
	rows := caretest.Rows(t, h)
	timer, completion := rows[caretest.TimerA], rows[caretest.CompletionA]
	if timer.ChangedSeq != base+4 || timer.TombstonedAt == nil || *timer.CompletedRecordID != caretest.CompletionA || timer.Operation != "timer_stop" { t.Fatalf("timer %+v", timer) }
	if completion.ChangedSeq != base+5 || completion.Kind != "feed" || completion.Operation != "timer_stop" || completion.Derived.BottleObservations != 1 { t.Fatalf("completion %+v", completion) }
	late := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{caretest.Stop(caretest.BabyA, caretest.TimerA, "21000000-0000-4000-8000-0000000000aa")})
	if o := late.Outcomes[0]; o.Status != "superseded" || *o.Code != "timer_ended" || o.Cursor.Sequence != base+5 || len(o.RelatedEntities) != 1 { t.Fatalf("late stop %+v", o) }
}
func TestBabyDeleteCascadeIsSetBasedAndOrdered(t *testing.T) { // Review Focus 4
	// seed baby B with 5,000 children (feeds, sleeps, diapers, supplements, measurements, growth profile, one running feed timer) via ApplyBatch batches;
	// counted pool: warm with a rolled-back delete, then measure the real delete -> wire.Trips() == 2;
	// sequences: root base+1, then children in (feed,sleep,diaper,supplement,growth_profile,measurement,timer; id) order, consecutive;
	// counters: care_record_count -= non-timer children, care_charged_bytes released (TestCountersMatchRows invariant holds);
	// a later feed update under baby B -> rejected baby_deleted carrying the baby tombstone.
}
func TestCascadeIncludesRowsCreatedEarlierInBatch(t *testing.T) { /* [feed create under B, baby B delete, feed create under B] -> 2nd tombstones the 1st feed; 3rd rejected baby_deleted */ }
func TestTimerStartRules(t *testing.T) { /* superseded existing live timer of the kind; second kind allowed; one_live_timer index never violated */ }
// + the ported records timer/cascade cases, unchanged in expectation.
```
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/care' BT_TEST_RUN='TestTimer|TestBabyDelete|TestCascade|TestTimed|TestReadTimer' bash Server/scripts/test-db.sh`. Expected: `timer and baby-delete planning arrive in Task 6`.
- [ ] **Step 3: Implement** `timers.go` and `cascade.go` against the working set.
- [ ] **Step 4: Run green:** `rtk proxy env BT_TEST_PACKAGES='./internal/care ./internal/records' bash Server/scripts/test-db.sh`.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/care Server/internal/records && rtk git commit -m "feat(care): plan timers and baby deletes on the working set"`

### Task 7: Reader paths on care_entities, parity-tested before the switch

**Files:**
- Create `Server/internal/familyread/{reads.go,care_reads.go,care_reads_test.go}` and `familyread/export_test.go` (hook).
- Modify `Server/internal/familyread/{service.go,history.go,summary.go,summary_sql.go}`: today's SQL moves unchanged behind `legacyReads`.
- Modify `Server/internal/export/repository.go`: `collectArchive` takes a `careSource`.
- Create `Server/internal/export/care_source_test.go`.
- Add `caretest.Mirror` to `Server/internal/care/caretest/caretest.go`.

**Interfaces:** Consumes `care.Get`/`GetMany`/`Stream`/`StreamChanges` and Task 5/6 `ApplyBatch`. Produces:
```go
package familyread
type reads interface {
	babies(ctx context.Context, tx pgx.Tx, familyID string, limit int) ([]protocol.CanonicalEntity, error)
	liveBaby(ctx context.Context, tx pgx.Tx, familyID, babyID string) error
	candidates(ctx context.Context, tx pgx.Tx, familyID string, q historyQuery, after *position) ([]position, error)
	entries(ctx context.Context, tx pgx.Tx, familyID, babyID string, picked []position) ([]HistoryEntry, error)
	sessions(ctx context.Context, tx pgx.Tx, familyID, babyID string, entries []HistoryEntry) error
	timers(ctx context.Context, tx pgx.Tx, familyID, babyID string) ([]ActiveTimer, error)
	summaryDays(ctx context.Context, tx pgx.Tx, familyID string, q summaryQuery) ([]SummaryDay, error)
	supplements(ctx context.Context, tx pgx.Tx, familyID string, q summaryQuery) ([]SupplementDoses, error)
	measurements(ctx context.Context, tx pgx.Tx, familyID string, q summaryQuery, max int) ([]MeasurementObservation, error)
}
type legacyReads struct{ records *records.Repository } // today's code, verbatim
type careReads struct{}                                  // care_entities; the default from Task 8
// export_test.go: func NewWithReads(guard Guard, key [32]byte, now func() time.Time, careData bool) *Service

package export
type careSource interface {
	entities(ctx context.Context, tx pgx.Tx, familyID string, visit func(protocol.CanonicalEntity) error) error
	journal(ctx context.Context, tx pgx.Tx, familyID string, through uint64, identity protocol.ServerInfo, visit func(protocol.JournalChange) error) error
}
func collectArchive(ctx context.Context, tx pgx.Tx, grant access.Grant, identity protocol.ServerInfo, now time.Time, audit auditPager, source careSource) (ArchiveV1, error)

package caretest
func Mirror(t *testing.T, h *testkit.Harness, subject access.Principal, mutations ...protocol.Mutation) // h.Service.ApplyAndPull (legacy tables) AND care.ApplyBatch (care_entities), both must succeed
```
The `careReads` SQL (`care_reads.go`) runs on `care_entities` with `kind` predicates:
- **Candidates.** Each branch is `SELECT started_at AS event_start, '<kind>'::text AS kind, id FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND kind='<kind>' AND tombstoned_at IS NULL AND …range… ORDER BY started_at, id LIMIT $5`, using the history index. The sleep overlap uses `started_at > $3 - interval '7 days' AND started_at < $4 AND (started_at >= $3 OR ended_at > $3)`. The outer `ORDER BY event_start, kind COLLATE "C", page.id LIMIT $5` is unchanged.
- **Entries.** One `GetMany` then a decode, instead of N per-kind getters.
- **Sessions.** `sessionMembersSQL` over `care_entities` with `kind=` and `session_id`.
- **Timers.** `kind='timer' AND tombstoned_at IS NULL AND baby_id=$2 ORDER BY timer_kind, id LIMIT 3`.
- **Babies.** `kind='baby' AND tombstoned_at IS NULL ORDER BY sort_order, id LIMIT $2`.
- **Summary.** The aggregate replaces the `feed_components` LATERAL with `bottle_observations`, `bottle_volume_ml` and `bottle_missing`, keeps `bottle_days … WHERE bottle_observations + bottle_missing > 0`, and groups supplements `BY label ORDER BY label COLLATE "C"`.

- [ ] **Step 1: Write the failing tests** `familyread/care_reads_test.go`:
```go
func TestCareReadsMatchLegacy(t *testing.T) {
	f := newFixture(t)
	for _, step := range testkit.GoldenSteps(t, f.h) { caretest.Mirror(t, f.h, f.h.Principal(step.Subject), step.Mutations...) } // both stores hold the same family
	legacy, care := NewWithReads(f.guard, f.key, f.clock, false), NewWithReads(f.guard, f.key, f.clock, true)
	queries := []HistoryQuery{{BabyID: fixtureBab, Start: day0, End: day0.Add(30 * 24 * time.Hour)}, {BabyID: fixtureBab, Start: day0, End: day0.Add(48 * time.Hour), Kinds: []string{"sleep", "feed"}, Limit: 2}}
	for _, q := range queries { compareJSON(t, must(legacy.History(t.Context(), f.credential, q)), must(care.History(t.Context(), f.credential, q))) } // NextCursor compared after unsealing
	compareJSON(t, must(legacy.ListBabies(t.Context(), f.credential)), must(care.ListBabies(t.Context(), f.credential)))
	compareJSON(t, must(legacy.Summary(t.Context(), f.credential, SummaryQuery{BabyID: fixtureBab, Start: day0, End: day0.Add(72 * time.Hour), Timezone: "Europe/Zurich"})),
		must(care.Summary(t.Context(), f.credential, SummaryQuery{BabyID: fixtureBab, Start: day0, End: day0.Add(72 * time.Hour), Timezone: "Europe/Zurich"})))
}
func TestCareQueryPlansStayBounded(t *testing.T) { /* plan_test.go:161-260 against careReads SQL on seedLargeHistory mirrored into care_entities: buffers bounded, history/session/baby-match indexes used */ }
```
`export/care_source_test.go`, `TestCareArchiveMatchesLegacy`: mirror the golden steps, then compare every archive section between the legacy and care sources except `journal`. The care `journal` must equal `LatestPerEntity(legacy journal)` with cursors normalised, because dual seeding interleaves sequences.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/familyread ./internal/export' BT_TEST_RUN='TestCare' bash Server/scripts/test-db.sh`. Expected: `undefined: NewWithReads` and `undefined: careSource`.
- [ ] **Step 3: Implement** both sources.
- [ ] **Step 4: Run green:** `rtk proxy env BT_TEST_PACKAGES='./internal/familyread ./internal/export ./internal/mcpapi' bash Server/scripts/test-db.sh`. The legacy default is still wired, so every existing test passes.
- [ ] **Step 5: Commit:** `rtk git add Server/internal/familyread Server/internal/export Server/internal/care/caretest && rtk git commit -m "feat(familyread,export): read care entities behind a parity-tested source"`

### Task 8 (big-bang switch): sync, importer, delivery, readers and composition onto care_entities

**Files:**
- **8a:**
  - Modify `Server/internal/sync/{mutation.go,paging.go,snapshot.go}`; create `Server/internal/sync/{exchange.go,roundtrip_test.go,convergence_test.go}`.
  - Port `sync/{integration_test.go,paging_test.go,snapshot_test.go,family_lock_test.go,encoding_test.go,growth_records_test.go,supplements_test.go,timer_readback_test.go,interim_client_test.go}`: responses are decoded through `testkit.Decoded`; legacy table assertions become `care_entities` assertions.
  - Modify `Server/internal/importer/{resolver.go,conflict.go,manifest.go,repository.go}` and its tests.
  - Modify `Server/internal/delivery/observer.go`; rewrite `observer_test.go`.
  - Modify `Server/internal/testkit/{fixtures.go,postgres.go}` and add the golden comparisons.
  - Modify `Server/cmd/baby-tracker-server/{main.go,performance_composition_identitytest_test.go}`, `Server/internal/authority/locks_test.go` (`records.LockFamilyForUpdate` → `care.LockFamily`), and `httpapi` tests where they read legacy tables.
- **8b:**
  - Modify `Server/internal/familyread/service.go` (default `careReads`, delete `legacyReads` and the old SQL) and `Server/internal/export/repository.go` (default care source, delete the legacy source).
  - Fix legacy-table SQL in `mcpapi/*_test.go`, `tests/protocol/{harness_test.go,import_test.go,reauth_test.go,viewer_test.go}`, `internal/capacity/{fixture_identitytest.go,mcp_test.go}` and `cmd/baby-tracker-server/mcp_test.go`.
  - Turn `TestLegacy*Corpus` into the comparison tests below.

**Interfaces:** Consumes everything above plus `authority.FamilyGuard.WithFamilyTx`. Produces:
```go
package sync
type Options struct {
	Guard authority.FamilyGuard; Server protocol.ServerInfo; Limits protocol.Limits; Now func() time.Time
	Resolver access.FamilyResolver; Publisher care.Publisher; PageKeys identity.DigestKeyring; Allowed func(role, action string) bool
}
func New(o Options) *Service                                         // replaces NewService/NewServiceWithResolver/NewServiceWithPaging
func (s *Service) SetImportAdmission(admission care.ImportAdmission)
func (s *Service) Apply(ctx context.Context, p access.Principal, familyID string, m protocol.Mutation) (protocol.Outcome, error) // one-mutation batch, no pull
func (s *Service) ApplyAndPull(ctx context.Context, p access.Principal, request protocol.SyncRequest) (protocol.SyncResponse, error)
func (s *Service) FullSnapshot(ctx context.Context, p access.Principal, request protocol.SnapshotRequest) (protocol.SnapshotResponse, error)
// Phase 3 seam: everything one request does inside an already guarded transaction.
type Exchanged struct { Response protocol.SyncResponse; Publish *protocol.Cursor; Fatal error }
func (s *Service) Exchange(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, request protocol.SyncRequest) (Exchanged, error)
func (s *Service) SnapshotPage(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, request protocol.SnapshotRequest) (protocol.SnapshotResponse, error)
var ErrReadOnlyCaregiver = care.ErrReadOnlyCaregiver   // care defines it; Policy returns it, authority.ErrOwnerRequired, or authority.ErrMembershipRevoked

package importer
func NewService(guard authority.FamilyGuard, resolver access.FamilyResolver, info protocol.ServerInfo, limits protocol.Limits, pageKeys identity.DigestKeyring, now func() time.Time) *Service
func (s *Service) SetPublisher(p care.Publisher)
func (s *Service) AdmitRecord(ctx context.Context, tx pgx.Tx, g access.Grant, device string, m protocol.Mutation, view care.View) (protocol.Outcome, bool, error)

package delivery
func NewObserver(publisher HintPublisher) *Observer
func (o *Observer) Publish(ctx context.Context, familyID string, cursor protocol.Cursor) error // Schedule, Repository, Queue removed

package testkit
func Decoded(t *testing.T, response protocol.SyncResponse) protocol.SyncResponse          // marshal + protocol.SyncResponse.UnmarshalJSON
func DecodedSnapshot(t *testing.T, response protocol.SnapshotResponse) protocol.SnapshotResponse
func (h *Harness) RebuildSyncService(t *testing.T, limits protocol.Limits)             // small page sizes for paging tests
func (h *Harness) AuthorityOn(t *testing.T, pool *pgxpool.Pool) *authority.Service
```

Flow:
- **`ApplyAndPull`.**
  - Validation is as today (`paging.go:159-187`), and `resolveFamily` is kept; Phase 3 removes it.
  - It runs `s.guard.WithFamilyTx(ctx, p, "read", …)` with timeout `SyncTransactionSeconds`, checks the grant family, then `Exchange`.
  - After the guarded transaction it publishes (if `Publish != nil`), then returns `Fatal` or the response.
- **`Exchange`.**
  - A page token goes to the continuation: one statement joins `sync_page_bindings` (by digest) laterally to `care_entities (resume_sequence, high_water_sequence]` with `LIMIT item_limit+1`, followed by `validatePageBinding` (`paging.go:361-373`, unchanged).
  - No mutations: `care.ReadPull` (no lock); `after > hw` gives `ErrSnapshotRequired`.
  - Mutations: `care.ApplyBatch` with `Policy` from `Options.Allowed` and `PullRequest{After, SyncPageSize+1}`. `HighWater > Base` sets `Publish`. `Fatal`, or `after > HighWater` (which becomes Fatal `ErrSnapshotRequired`), returns after commit.
- **Paging.**
  - Each `ChangeRow.Encode` is computed once.
  - The page is the longest prefix within the item and byte limits, sized with `IncrementalPageEncodedBytesRaw`. The token is counted in the size iff a fetched row is left after the prefix; more = a row is left over.
  - With more: insert a binding `{kind sync, resume = last seq, high water, limits, created/expires}`. For a first page the lookup is random; for a continuation it is `successorLookup(presented, binding{resume: end})` (`paging.go:121-137`). A conflict keeps `ON CONFLICT DO NOTHING` plus equality of the stored binding.
  - The response uses `EncodedChanges`.
- **`SnapshotPage` capture.**
  - One round trip pipelines: admission counts (`SELECT count(*) FILTER (WHERE account_id=$1), count(*) FILTER (WHERE family_id=$2) FROM sync_snapshot_sessions WHERE (account_id=$1 OR family_id=$2) AND expires_at > $3`), `ReadFamilyState`, and `QueueSnapshotPage(FirstSnapshotKey, SyncPageSize+1)`.
  - Limits: `ErrSnapshotLimit` for the account/family cap, when `care_entity_count > SnapshotMaxEntities`, or when `SnapshotPageEncodedBytesRaw(empty)+care_wire_bytes+count-1 > SnapshotMaxBytes`.
  - Then one round trip inserts the session row and, when there is more, a binding `{kind snapshot, snapshot_id, resume_kind/resume_id = last key}`.
- **Snapshot continuation.** One statement joins the binding, the session row and the page.
- **Importer.**
  - `LockFamily`, `MatchBabies(BabyNameHash(name, birth))` (replacing `normalized_name=$2 AND birth_date=$3`), `care.Get` (replacing `FindEntityKind`/`GetCanonical`/`GetBaby`), and `ChargeImport`.
  - Baby publication is one `ApplyBatch` of all publication creates, with nil `Policy`; each mapping's `shared_baby_cursor` is the baby's `changed_seq`.
  - `AdmitRecord` reads entities only through `view`.
  - The post-commit `Publish` fires when `next_sequence` advanced.

- [ ] **Step 1: Write the failing tests.**
  - `sync/golden_test.go`, rewriting `TestLegacyWireCorpus` into `TestCareWireMatchesLegacyCorpus`, which compares against the committed legacy corpus:
    - outcomes and import responses are byte-equal;
    - `full_pull.changes == LatestPerEntity(legacy.full_pull.changes)`;
    - `mid_pull.changes == LatestPerEntity(legacy.mid_pull.changes)`;
    - high waters are equal;
    - `snapshot.entities` are byte-equal.
  - `familyread` and `export`: `TestFamilyReadMatchesLegacyCorpus` (byte-equal) and `TestExportMatchesLegacyCorpus` (byte-equal except `journal == LatestPerEntity(legacy journal)`).
  - `sync/roundtrip_test.go`:
```go
func TestSyncRequestRoundTrips(t *testing.T) {
	h := testkit.New(t)
	pool, wire := testkit.CountedPool(t, h.Runtime)
	service := testkit.SyncServiceOn(t, h, h.AuthorityOn(t, pool))
	measure := func(request protocol.SyncRequest) int { // guard (BEGIN+authority) + Exchange + COMMIT; warmed once in a rolled-back tx first
		wire.Reset()
		err := h.AuthorityOn(t, pool).WithFamilyTx(t.Context(), h.Owner, "read", func(tx pgx.Tx, g access.Grant) error { _, err := service.Exchange(t.Context(), tx, h.Owner, g, request); return err })
		if err != nil { t.Fatal(err) }
		return wire.Trips()
	}
	// mutation batches of 1, 20, 100 with a pull that fits one page -> 4; pull-only -> 3; first snapshot page with more -> 4; continuation with more -> 4, last continuation -> 3
}
```
  - `TestSyncContinuationReplayAfterConcurrentEdit` (Review Focus 1):
    1. Page size 3; seed 9 feeds; the caregiver pulls page 1 and keeps token T.
    2. The owner edits feed 5.
    3. The caregiver requests T twice: both succeed, neither returns feed 5, and neither is `invalid_page_token`.
    4. After paging, a pull from the high water returns feed 5 at version 2, and the device map equals the server rows.
  - `TestEmptiedRangeEndsWithoutToken`: every row in the token's range is edited, the continuation returns `changes: []` with no `next_page_token`, and the next pull delivers them.
  - `TestSnapshotWithoutStagingConvergesUnderConcurrentWrites`: during snapshot paging, a row is created with a key before the resume, a paged row is updated, and an unpaged row is deleted. After the snapshot plus one pull the device equals the server rows. Admission caps hold, and a family over `snapshot_max_entities` is refused before any page.
  - `sync/convergence_test.go`, `TestPullAndSnapshotConvergeUnderAnyInterleaving`:
    - 40 seeds with `math/rand/v2` PCG and 120 steps each, page size 3.
    - Step types: a random write batch of 1–5 mutations (creates, updates, deletes, timer start/stop, baby create/delete) by the owner or caregiver; one page of an in-flight pull or snapshot on one of three simulated devices; or a replay of a device's last continuation token.
    - After the steps, every device drains and pulls to completion. Each device map, built by applying a change iff `entity_version >= current` (as BabyStore+RemoteApply.swift:159) and recording the cursor only on a final page (:57-93), must equal `{id: (entity_version, wire)}` of `care_entities`.
  - `TestConcurrentWritersAndPagersConverge`: the same simulation with a writer goroutine racing two pagers, under `-race`.
  - `delivery/observer_test.go`, `TestWriteTransactionSchedulesNoWakes`: a hosted harness write leaves `pg_stat_activity`'s NOTIFY queue empty (`pg_notification_queue_usage() = 0`) and inserts no wake row; `Publish` runs once after commit with the high-water cursor.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/sync ./internal/importer ./internal/delivery' bash Server/scripts/test-db.sh`. Expected: compile errors (`undefined: sync.Options`, `undefined: (*Service).Exchange`).
- [ ] **Step 3: Implement 8a, then 8b**, following the flow above. Commit 8a and 8b separately if useful: `rtk git commit -m "feat(sync): serve sync and snapshots from care entities in one transaction"` and `rtk git commit -m "feat(familyread,export): read care entities by default"`.
- [ ] **Step 4: Gate:**
  - `rtk proxy env BT_TEST_PACKAGES='./internal/care ./internal/sync ./internal/importer ./internal/delivery ./internal/familyread ./internal/export ./internal/mcpapi ./internal/httpapi ./internal/realtime ./internal/testkit ./internal/authority ./internal/capacity' bash Server/scripts/test-db.sh`
  - `(cd Server && rtk go build ./... && rtk go vet ./... && rtk go vet -tags identitytest ./...)`
  - `rtk proxy env BT_TEST_PACKAGES='./tests/protocol/...' BT_TEST_TAGS=identitytest bash Server/scripts/test-db.sh`. If `test-db.sh` has no tag knob, use `cd Server && BABY_TRACKER_REQUIRE_DATABASE=1 go test -tags identitytest -race -count=1 ./tests/protocol/...` with the disposable DSN, as protocol-e2e.sh:73 does.
- [ ] **Step 5: Commit** (if not already committed in parts): `rtk git add -A Server/internal Server/cmd Server/tests && rtk git commit -m "refactor(care): switch every care-data caller to care entities"`

### Task 9: Remove the legacy care storage; freeze 000030

**Files:**
- Modify `Server/migrations/000030_care_entities.sql` (append section 8; move the manifest and compatibility statements to the end); update the `compatibility.json` digest.
- Delete `Server/internal/records/**` after moving `format_cleanup_migration_test.go` and `format_cleanup_join_test.go` to `Server/internal/database/legacy_format_cleanup_test.go` and `legacy_format_cleanup_join_test.go` (`package database`, with a local copy `legacySnapshotEntitiesUnion` of `records/snapshot.go:60-175`).
- Delete `Server/internal/sync/{journal.go,receipts.go,format_cleanup_interim_test.go}` and `Server/internal/delivery/{repository.go,queue.go,queue_test.go}`.
- Delete testkit `NewThrough`, `newCareHarness`, `tenantGuard`, `careResolver`, `careSeed` and `PendingWake`.
- Replace `Server/internal/database/{records_schema_test.go,snapshot_schema_test.go,timer_schema_test.go,delivery_schema_test.go}` with `care_schema_test.go`; update `import_schema_test.go` for the FK repoint.
- Modify the scripts:
  - `check-principals.sh:52-92,314-330` — care grants instead of babies/journal/receipts/sync_*/delivery.
  - `smoke.sh:199-212` — the `>=30` branch asserts the `care_*` grants and the absence of `delivery_pending`/`schedule_family_wakes`.
  - `protocol-e2e.sh:22-27,46-63,96-121` — queue assertions become "no `delivery_pending` relation"; `completed_feed` and the owner-reinstall ledger are read from `care_entities`.
  - `mcp-e2e.sh:352-356` — `care_entities` with `kind=`.
  - The `scripts/*_test.go` expectations.
- Modify `docs/protocol/v1.md`:
  - L169-184: "journaled" becomes "changed"; a cascade is "tombstoned with consecutive sequences".
  - L234: the wake paragraph — no durable wake intent until the background worker.
  - L322-331: continuations return the current state of the bound range; snapshots page current rows and are consistent after the following pull.
  - L343-347: charged storage is current rows plus deletion headroom plus retained import staging; receipts are kept 30 days.
  - Add a "Latest-state pulls" note.

**Interfaces:** Produces the final 000030. Appended SQL (section 8, placed before the manifest):
```sql
-- 8. Remove the legacy care storage. Fresh installs only: refuse when any
-- legacy care row exists (Phase 4's importer owns existing data). FORCE RLS is
-- lifted for the check only, inside this transaction, as 000029 does.
ALTER TABLE public.babies NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.mutation_journal NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.mutation_receipts NO FORCE ROW LEVEL SECURITY;
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM public.babies) OR EXISTS (SELECT 1 FROM public.mutation_journal) OR EXISTS (SELECT 1 FROM public.mutation_receipts) THEN
    RAISE EXCEPTION 'legacy care data present; use baby-tracker-admin import-legacy' USING ERRCODE='55000';
  END IF;
END $$;
ALTER TABLE public.import_baby_mappings DROP CONSTRAINT import_mappings_shared_baby_fk,
  ADD CONSTRAINT import_mappings_shared_baby_fk FOREIGN KEY (family_id, shared_baby_id) REFERENCES public.care_entities(family_id, id);
DROP TRIGGER sync_snapshots_stage_admission ON public.sync_snapshots;
DROP FUNCTION app.stage_sync_snapshot_admission(), app.sync_snapshot_admission_counts(uuid,uuid,timestamptz),
  app.save_mutation_receipt(uuid,uuid,uuid,uuid,bytea,jsonb,timestamptz,bigint), app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint);
-- No CASCADE: a dependency outside this list fails the migration loudly.
DROP TABLE public.sync_page_sessions, public.sync_snapshot_entities, public.sync_snapshot_admissions, public.sync_snapshots,
  public.feed_components, public.timer_components, public.active_timers, public.feed_entries, public.sleep_entries, public.diaper_entries,
  public.supplement_entries, public.measurement_entries, public.growth_profiles, public.mutation_receipts, public.family_record_usage,
  public.delivery_pending, public.mutation_journal, public.babies;
-- Manifest 30 now copies 29 minus the dropped objects:
--   … WHERE schema_version=29
--     AND NOT (object_kind IN ('table','column') AND object_identity ~ '^public\.(babies|feed_entries|sleep_entries|diaper_entries|supplement_entries|growth_profiles|measurement_entries|active_timers|feed_components|timer_components|mutation_receipts|mutation_journal|family_record_usage|sync_page_sessions|sync_snapshots|sync_snapshot_entities|sync_snapshot_admissions|delivery_pending)(\.|$)')
--     AND NOT (object_kind='function' AND object_identity ~ '^app\.(schedule_family_wakes|save_mutation_receipt|sync_snapshot_admission_counts|stage_sync_snapshot_admission)\(');
```
Test disposition table (no skip survives this task):

| Suite | Disposition |
|---|---|
| `records/validation_test.go` | Moved to `care/validate_test.go` (Task 5) |
| `records/{components,sleep_sessions,growth,repository}_test.go` | Ported to `care/records_test.go` (Task 5) |
| `records/{timers,timer_edit,timed_bottle,timer_stop_cleanup,read_timer}_test.go` | Ported to `care/timers_test.go` (Task 6) |
| `records/{child_delete,growth_cascade}_test.go` | Ported to `care/cascade_test.go` (Task 6) |
| `records/rls_test.go` | Rewritten as `care/rls_test.go` (Task 4) |
| `records/journal_test.go` | Deleted: no journal; replaced by `TestCountersMatchRows` and the sequence assertions of Tasks 5–6 |
| `records/format_cleanup_{migration,join}_test.go` | Moved to `database/legacy_format_cleanup_*_test.go` (pure SQL against schema 26→27) |
| `sync/format_cleanup_interim_test.go` | Deleted: it ran the runtime against schema 26, which no runtime supports |
| `delivery/queue_test.go` | Deleted: no in-transaction queue; `observer_test.go` rewritten (Task 8) |
| `database/{records,snapshot,timer,delivery}_schema_test.go` | Replaced by `care_schema_test.go` assertions (absence of legacy objects included) |

- [ ] **Step 1: Write the failing tests** in `database/care_schema_test.go`:
  - `TestLegacyCareStorageIsGone`: `to_regclass` is null for all 18 tables, and the four functions are absent from `pg_proc`.
  - `TestMigrationThirtyRefusesLegacyCareRows`: `testpg.NewThrough(t, 29)`; insert one `babies` row as the owner; `MigrateThrough(30)` fails with SQLSTATE `55000`.
- [ ] **Step 2: Run to see the failure:** `rtk proxy env BT_TEST_PACKAGES='./internal/database' BT_TEST_RUN='TestLegacyCareStorage|TestMigrationThirty' bash Server/scripts/test-db.sh`. Expected: `legacy table babies survived`.
- [ ] **Step 3: Implement.** Amend 000030, recompute the digest (`shasum -a 256 Server/migrations/000030_care_entities.sql`), delete the files, move the tests, and update the scripts and docs. `rtk grep -rn "feed_entries\|mutation_journal\|sync_snapshot_entities\|delivery_pending\|schedule_family_wakes\|records\." Server --include='*.go' --include='*.sh' --include='*.py'` must return only `migrations/`, `cmd/baby-tracker-overlap-check` (historical; Phase 5) and the moved legacy format-cleanup tests.
- [ ] **Step 4: Run green:** `rtk make server-test`, then `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`. Write "000030 FROZEN" in its header comment.
- [ ] **Step 5: Commit:** `rtk git add -A Server docs/protocol/v1.md && rtk git commit -m "chore(care): remove the journal, snapshot staging and per-kind care tables"`

### Task 10: Fresh-install gates and the iOS behaviour verification

**Files:** Script fixes only, if the gates expose any. No product change is expected; any change is committed with its reason.

**iOS E2E policy:** The client suites are SwiftPM tests on the macOS host. `protocol-e2e.sh:85-91` runs `swift test --filter FamilySyncClientE2ETests` against the TLS front, which covers:
- TwoCaregiverSync, OwnerReinstall, OnePartTimerSync and FamilySyncClient;
- RefreshRecovery under `BT_RECOVERY_E2E_MODE`;
- the non-live CapacityMatrix and ForegroundLatency cases.

They need no simulator. If any `xcodebuild` step is added, it must use only `platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853`, one run at a time, and must never create, reset or erase a simulator. The live CapacityMatrix cells need `sync-performance.sh`, which still reads the retired `security_projection_head` (line 210), so they are a Phase 5 obligation.

- [ ] **Step 1: Client contract review.** Record in the task report, with line citations:
  - `Packages/BabyTracker/Sources/BabyStore/BabyStore+RemoteApply.swift:57-93` applies each change by entity version and records the cursor only when `next_page_token` is nil.
  - `:104-112` commits a snapshot by rematerialising the received keys and recording its high water, without deleting absent entities.
  - `:159` applies only when `entityVersion >= currentVersion`.
  - So latest-state pulls and non-frozen snapshots meet the shipped client's contract.
- [ ] **Step 2: Server gates.** `rtk make server-build`; `rtk make server-test`; `(cd Server && rtk go vet ./... && rtk go vet -tags identitytest ./...)`.
- [ ] **Step 3: Protocol E2E (hosted), with the iOS suite.** Run `rtk make server-protocol-e2e`. Expected:
  - exit 0;
  - `two-caregiver proof: step 18:` is present;
  - `owner-reinstall proof:` shows the phone ledger equal to the `care_entities` ledger;
  - the queue line reports no `delivery_pending` relation.
- [ ] **Step 4: Protocol E2E (self-hosted, recovery):** `rtk env BT_RECOVERY_E2E_MODE=self_hosted make server-protocol-e2e`. Expected: exit 0 and the refresh-recovery phases pass. One rerun is allowed for the recorded Swift flake (Phase 1 ledger, Task 16); a second failure blocks.
- [ ] **Step 5: MCP and smoke.** `rtk bash Server/scripts/mcp-e2e.sh` (four clients, 0 credential matches; the database proof reads `care_entities`); `rtk make server-smoke` (runtime grants match manifest 30, `authority verify` clean).
- [ ] **Step 6: Commit** any script fixes: `rtk git commit -am "test(e2e): verify care entities and latest-state sync on a fresh install"`

### Carries out of Phase 2
- **Phase 3:**
  - `resolveFamily` and `httpapi` still call `family.Service.Current` twice per sync request. Collapse each request onto `WithAuthenticatedFamilyTx` plus `Service.Exchange`/`SnapshotPage`.
  - The wake worker needs per-device acknowledged cursors.
  - Retention sweeps use the provided indexes (`care_receipts` 30 days, `sync_page_bindings` and `sync_snapshot_sessions` by TTL) and need autovacuum tuning.
  - Delete `realtime.PostgresListener`. Its resyncer pulls from cursor 0 just to learn the high water.
- **Phase 4:** The importer must load `care_entities` through `care.Encode`, the only encoder, and set the four family counters and `next_sequence`. It must also replace the 000029 and 000030 fresh-install guards.
- **Phase 5:** Live CapacityMatrix runs via `sync-performance.sh`; `check-migration-overlap.sh`.

### Critical Files for Implementation
- <repo>/.worktrees/postgres-authority/Server/migrations/000030_care_entities.sql (new)
- <repo>/.worktrees/postgres-authority/Server/internal/care/apply.go and store.go (new)
- <repo>/.worktrees/postgres-authority/Server/internal/records/repository.go, timers.go, child_delete.go (behaviour source)
- <repo>/.worktrees/postgres-authority/Server/internal/sync/paging.go and snapshot.go (rewritten around `Exchange`)
- <repo>/.worktrees/postgres-authority/Server/internal/familyread/summary_sql.go and history.go (reader SQL)

