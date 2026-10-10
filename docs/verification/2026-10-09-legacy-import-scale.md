# Legacy import duration: homelab shape (P) and 10,000 families (L10k)

Phase 4 Task 22. `TestImportScale` (`Server/internal/legacyimport/scale_test.go`)
builds a legacy install at schema 29 with `legacytest.Build`, then runs
`legacyimport.Run` twice on it: a dry run, then the import with a backup
receipt (`legacy_state_sha256` computed by `LegacyStateSQL` as the cluster
superuser). The timings below are `Report.Phases`, as the plan's duration target
asks. The test also records each run's wall clock, the peak Go heap, and the
database size before and after.

**Result** (final code, after the owner's rulings; run 5 for L10k):

- **P meets its target.** The dry run takes 2.0 s and the import 2.8 s, against
  a limit of 10 s.
- **L10k meets its 15-minute target**, at 11m20s for the dry run and 8m22s for
  the import. The dry run's care phase ran under a load average of up to 17
  from another session.
- **Every L10k rate floor holds:**

  | Floor | Phase | Dry run | Import |
  |---|---|---:|---:|
  | ledger walk ≥ 4,000 events/s | `ledger_verify` | 6,324 | 5,552 |
  | ledger walk ≥ 4,000 events/s | `audit_rewalk` | 4,851 | 5,790 |
  | care ≥ 5,000 entities/s | `care` | 5,880 | 9,737 |
  | audit ≥ 10,000 rows/s | `audit_copy` | 51,619 | 75,042 |

- **The audit floor now measures the audit COPY.** By the owner's ruling, the
  former `authority` phase is split into `audit_rewalk` (the in-transaction
  ledger re-walk, held to the ledger-walk floor) and `audit_copy` (the audit
  rows' COPY, held to the audit floor); see "The audit phase split" below.
  Before the split, the audit floor read 5,900–7,600 rows/s, because the phase
  included a full ledger walk. No floor was lowered.

## Machine and harness

| | |
|---|---|
| Host | Apple M4, 10 cores, 24 GiB (`hw.memsize` 25769803776), macOS 26.6.2 |
| Docker | OrbStack 29.4.0, `NCPU` 10, `MemTotal` 12588724224 (11.7 GiB) |
| PostgreSQL | `postgres:17.6-alpine@sha256:ef257d85…eee94` with the harness defaults (`Server/scripts/test-db.sh`: no tuning, so `shared_buffers` 128 MB and `max_wal_size` 1 GB) |
| Go | go1.26.2 darwin/arm64; test binary without `-race` |
| Command | `rtk proxy env GOCACHE=… BT_TEST_RACE=0 BT_TEST_TIMEOUT=120m BT_TEST_VERBOSE=1 BT_LEGACY_SCALE=L10k\|P BT_TEST_PACKAGES='./internal/legacyimport' BT_TEST_RUN='^TestImportScale$' bash Server/scripts/test-db.sh` |

The machine was shared with other agent sessions, so the numbers carry noise:

- **L10k (run 4, 14:29–14:54 CEST).** The one-minute load average was 2.6–8.9
  (mean 4.0) on 10 cores. Free disk fell from 31 GiB to 16 GiB during the run,
  and was back afterwards.
- **P.** Another session's `test-db.sh` ran at the same time.
- **The first diagnostic L10k run** (before the fixes) overlapped a 10-process
  Swift build and another `test-db.sh`. Its numbers are kept only as the
  "before" column.

This is not the reference node: **measuring on the reference node is a Phase 5 carry.**

## P: homelab-shaped fixture

`legacytest.Spec{Families: 1, Babies: 2, Caregivers: 1, DevicesPerMember: 2, EntitiesPerFamily: 1100, VersionsPerEntity: 1, UpdateEvery: 4, Timers: true, RotationsPerDevice: 1250, LiveActivity: true, NotCarried: true}`

The fixture holds:

- 1 family, 2 accounts and 4 devices (the plan's duration target says 4 devices).
- 2 babies, one with an icon.
- **1,443 `mutation_journal` rows** and 1,109 care entities.
- A ledger of **5,007 events**, close to the plan's "about 5,000".
- 4 Live Activity registrations, with 3 card tokens and 16 delivery jobs. All
  four devices register, because they all hold family-scope sessions; the
  plan's text says 2 registrations.

| Phase | Dry run | Import |
|---|---:|---:|
| ledger_verify | 348 ms | 343 ms |
| lock | 0.9 ms | 1.0 ms |
| crosscheck | 36 ms | 33 ms |
| stage | 422 ms | 479 ms |
| unforce | 12 ms | 12 ms |
| authority (with audit, 5,013 rows) | 647 ms | 821 ms |
| care (1,109 entities) | 158 ms | 163 ms |
| liveactivity | 7 ms | 6 ms |
| restore | 11 ms | 10 ms |
| verify | 130 ms | 129 ms |
| drop | 167 ms | 255 ms |
| analyze | 46 ms | 44 ms |
| commit | — | 15 ms |
| **Total (phases)** | **1.98 s** | **2.31 s** |
| Wall clock of `Run` | 2.00 s | 2.38 s |

Other measurements for P:

- **Build:** 2.3 s, with a peak Go heap of 36 MiB and a process max RSS of 67 MiB.
- **Database size:** 37.9 MiB before the import and 34.1 MiB after.
- **Sensitivity run:** the plan's comment says 2,500 rotations per device. With
  that, the 4 devices write a 10,007-event ledger, and the run takes 2.80 s
  (dry) and 3.32 s (import), still well inside 10 s.
- **Before the Task 22 fixes** (commit `cc04574c`), P took 2.5 s and 3.0 s.

## L10k: 10,000 families

`legacytest.Spec{Families: 10000, Caregivers: 2, Viewers: 1, EntitiesPerFamily: 199, VersionsPerEntity: 1, UpdateEvery: 3, RotationsPerDevice: 11, NotCarried: true}`

The fixture holds:

- **Accounts and devices:** 40,000 accounts and 40,000 devices.
- **Care:** 2,010,000 care entities from 2,810,000 `mutation_journal` rows.
- **Ledger and audit:** a ledger of 550,000 events, which becomes 670,000 audit rows.

It also writes the not-carried rows. The skipped counts include 440,000
`session_refresh_history` rows, 550,000 `security_command_receipts` and
480,000 `auth_exchange_material` rows.

### The audit phase split (run 5, final code)

The owner ruled that the 10,000 rows/s audit floor measures the audit rows'
COPY, not the ledger walk that produces the rows. `authorityStep` now reports
its time as three phases, which add up to the step:

- `authority`: the authority tables, sessions and the skipped counts;
- `audit_rewalk`: the in-transaction re-walk of the ledger and the audit rows
  built from it. That is the walk's time, less the COPYs made during it. This
  is a ledger walk, so it is held to the ledger-walk floor (4,000 events/s);
- `audit_copy`: every audit row COPY (in batches of 5,000, during and after the
  walk), the `legacy_imported` anchors and the chain heads. This is held to the
  audit floor (10,000 rows/s).

The COPY clears its floor by a wide margin, so the audit COPY was not moved
onto a goroutine. That would have been possible: the walk reads files only, so
the transaction's connection is free for a COPY while it runs.

| Phase | Dry run | Import |
|---|---:|---:|
| ledger_verify (550,000 events) | 1m27.0s (6,324/s) | 1m39.1s (5,552/s) |
| lock | 6 ms | 4 ms |
| crosscheck | 6.5 s | 8.1 s |
| stage | 0.6 s | 0.6 s |
| unforce | 14 ms | 14 ms |
| authority | 6.0 s | 7.9 s |
| audit_rewalk (550,000 events) | 1m53.4s (4,851/s) | 1m35.0s (5,790/s) |
| audit_copy (670,000 rows) | 13.0 s (51,619/s) | 8.9 s (75,042/s) |
| care (2,010,000 entities) | 5m41.8s (5,880/s) | 3m26.4s (9,737/s) |
| liveactivity | 36 ms | 32 ms |
| restore | 28 ms | 21 ms |
| verify | 1m47.7s | 1m13.0s |
| drop | 0.2 s | 0.2 s |
| analyze | 3.5 s | 3.0 s |
| commit | — | 75 ms |
| **Total (phases)** | **11m19.7s** | **8m22.3s** |
| Wall clock of `Run` | 11m19.9s | 8m28.6s |

Run 5 ran from 15:57 to 16:25 CEST:

- **Machine load.** The one-minute load average was 2.2–17.1 (mean 7.3) on 10
  cores, from other sessions (a headless Chromium and Docker workloads).
- **Free disk.** The run started with 30 GiB free; free disk dipped to 13 GiB
  during the run.
- **Effect.** The dry run's care phase (5m42s, against 3m26s in the import) and
  the walks ran slower than in run 4.
- **Fixture build:** 4m59s.
- **Database size:** 6,690.2 MiB before the import and 3,633.9 MiB after.

P with the split (dry run / import):

| Phase | Dry run | Import |
|---|---:|---:|
| `audit_rewalk` | 0.47 s (10,700 events/s) | 0.48 s (10,500 events/s) |
| `audit_copy` (5,013 rows) | 0.10 s | 0.46 s |
| **Total** | **1.99 s** | **2.77 s** |

**P after review M1 (envelopes opened as the sidecars are).** P was rerun under a
load average of 7.5–15. A headless Chromium from another session was using
about 4–7 cores.

- **Totals.** Four runs, each with a dry run and an import, gave 4.1–5.3 s,
  plus one import at 7.2 s. All are under the 10 s limit; every phase, not only
  the walk, ran about twice as long as in the quiet runs above.
- **A/B under the same load.** The final code was compared with `readCandidate`
  on the old unguarded read:
  - `ledger_verify`: 0.66–0.87 s with the final code, against 0.82–0.92 s with
    the old read.
  - `audit_rewalk`: 0.74–1.18 s, against 0.84–0.89 s.

  So the extra `fstat` and `lstat` per envelope do not show at this size.

### Run 4 (before the split)

| Phase | Dry run | Import |
|---|---:|---:|
| ledger_verify (550,000 events) | 1m13.0s (7,539/s) | 1m12.8s (7,558/s) |
| lock | 3 ms | 1 ms |
| crosscheck | 6.0 s | 6.2 s |
| stage | 0.6 s | 0.4 s |
| unforce | 11 ms | 13 ms |
| authority (with 670,000 audit rows) | 1m28.1s (**7,603 rows/s**) | 1m53.0s (**5,931 rows/s**) |
| care (2,010,000 entities) | 3m14.6s (10,331/s) | 3m31.0s (9,527/s) |
| liveactivity | 27 ms | 24 ms |
| restore | 24 ms | 20 ms |
| verify | 1m13.4s | 1m17.6s |
| drop | 0.1 s | 0.2 s |
| analyze | 19.7 s | 26.1 s |
| commit | — | 71 ms |
| **Total (phases)** | **7m35.5s** | **8m27.3s** |
| Wall clock of `Run` | 7m35.7s | 8m34.1s (includes the post-commit `VACUUM (ANALYZE)`) |

The rest of run 4:

- **Fixture build:** 3m57s, plus 38 s for the `VACUUM (ANALYZE)` that settles
  the fixture (see the findings).
- **Database size:** 6,690.1 MiB before the import and 3,633.9 MiB after.
- **Memory.** The peak Go heap was 3.1 GB during the build and 2.3 GB during
  each run; that figure includes the fixture, which the test still holds. The
  test process's max RSS was 3.4 GiB.
- **Task 18 verify** (about 50 batched family round trips, then
  `VerifyChainsTx` over 50,000 scopes and 670,000 rows) took **68–78 s**. The
  query log of a diagnostic run puts about 60 s in the family reads and about
  40 s in the chain verification.
- **Receipt.** At L10k, `LegacyStateSQL` took roughly 2 minutes as superuser.
  This is an estimate derived from the test's total, not a measured phase. The
  owner accepted the receipt's cost as negligible at the homelab's size (26 MB).

### Before and after the Task 22 changes

| Run | Code | Dry total | Import total | Walk | Care | Audit |
|---|---|---:|---:|---:|---:|---:|
| 2 (diagnostic, heavily loaded) | `cc04574c` | 17m35s | 17m39s | 3,374–3,424/s | 3,469–3,728/s | 3,158–3,526/s |
| 3 (quiet, load ~2.7) | + care key | 9m34s | 9m41s | 4,097 / 3,826/s | 10,783 / 10,723/s | 4,193 / 4,247/s |
| 4 | + read-ahead | 7m35s | 8m27s | 7,539 / 7,558/s | 10,331 / 9,527/s | 7,603 / 5,931/s (whole `authority`) |
| 5 (recorded, load mean 7.3) | + audit split, no `NOWAIT` | 11m20s | 8m22s | 6,324 / 5,552/s; re-walk 4,851 / 5,790/s | 5,880 / 9,737/s | 51,619 / 75,042/s (`audit_copy`) |

## What was changed, and why

Phases were timed directly, and A/B experiments were run on a 1,000-family
copy of the L10k shape. The Go CPU profiles taken on macOS were not used. They
attributed most samples to `syscall.Open`, `pread` and `pwrite`, but buffering
the walk's index-file I/O showed no change, so that attribution was wrong. The
timing experiments located the real costs.

### 1. Care: the self-referencing baby key

`care_entities_baby_fk` references `care_entities(family_id, id)` on the
hash-partitioned table, and its per-row check was about 110 µs.

- **On 201,000 rows,** the COPY took 30 s with the key, 7.5 s without it, and
  adding the key back took 0.8 s.
- **Dropping the six non-unique secondary indexes** for the load made no
  measurable difference.

`careStep` now does the following (`dropBabyKey`, commit `8bd6c507`):

1. Reads the key's definition with `pg_get_constraintdef`.
2. Drops the key.
3. Copies the care rows.
4. Adds the key back under the same name and definition, which validates every
   row in one pass.

Results:

- A record whose baby is missing is refused with the same 23503 from
  `care_entities_baby_fk`, pinned by `TestCareLoadRefusesARecordWithoutItsBaby`.
- `TestImportedCatalogEqualsAFreshInstall` still holds.
- **At L10k the care phase went from about 9 minutes to about 3m15s**, and its
  rate from about 3,600/s to about 10,000/s, against a floor of 5,000.

### 2. Ledger walk: reading ahead

On a copy of the L10k ledger, reading and decoding the envelopes across both
passes took 114 s, while replaying the state took 9 s. The cause is two
sequential `open`, `read` and strict-decode operations per event, in a
directory of 550,000 files.

`WalkCommitted` now reads ahead in each pass (`readahead.go`, commit `e6268aab`):

- Each pass keeps a window of 512 envelopes, read by 8 workers.
- A pass takes a read-ahead result only for the path it names itself. The
  backward pass's guesses come from one directory listing, and the forward
  pass's from its index.
- Any other path is read exactly as before, so the checks, errors and their
  order are unchanged.
- The tests pin a stray envelope beside a revision, a missing own envelope, and
  that `stop` returns only after every read-ahead goroutine has.
- **Opening envelopes (review M1).** A backward guess can name a file the chain
  does not name, so envelopes are now opened as the sidecars are (`readEnvelope`):
  - `O_NOFOLLOW|O_NONBLOCK`;
  - the opened file and the path must be the same regular file;
  - the 68 KiB bound.

  A FIFO or symlink is refused as `ledger_corrupt` and never waited on.
- **Memory.** What waits in memory is bounded only for envelope bodies, at 512 ×
  68 KiB. The guesses grow with the chain, as the replayed State does:
  - one hash and two flags per revision, about 18 MB at 550,000;
  - the directory listing is read 4,096 names at a time.

Results on the L10k ledger copy:

- Read and decode went from 114 s to 40 s with 8 workers. 4 workers gave 49 s
  and 16 gave 39 s.
- **In the import, `ledger_verify` went from 2m14–2m24s to 1m13s, about 7,500
  events/s.**

### 3. Lock: autovacuum and `NOWAIT` (changed by the owner's ruling)

The first L10k attempt failed at `lockStep` with:

```
LOCK TABLE public.accounts, public.families, public.sessions, public.mutation_journal IN ACCESS EXCLUSIVE MODE NOWAIT:
could not obtain lock on relation "public.mutation_journal" (SQLSTATE 55P03)
```

The fixture had just been written, so autovacuum was processing
`mutation_journal`. Autovacuum is not a client backend, so `checkAlone` does
not see it, and `NOWAIT` refused at once instead of waiting.

The test now settles the fixture before it measures, so that autovacuum has
nothing to do, as on a database at rest:

- It runs a superuser `VACUUM (ANALYZE)`.
- It waits until no autovacuum worker runs before each `Run`.

**Ruling and change.** `lockStep` no longer uses `NOWAIT`:

- **Order.** `checkAlone` still runs first, unchanged. The advisory migrate
  lock is still taken without waiting.
- **Waiting.** The `LOCK TABLE … IN ACCESS EXCLUSIVE MODE` then waits up to the
  existing `lock_timeout` of 5 s.
  - **A routine autovacuum worker** that blocks the lock is cancelled by the
    waiting import once the wait passes `deadlock_timeout` (1 s by default). The
    lock is then granted.
  - **An anti-wraparound autovacuum** ("to prevent wraparound") is never
    cancelled.
- **Refusal.** A lock that another session still holds after 5 s is refused
  with SQLSTATE 55P03. The importer reports it with the code
  **`legacy_tables_busy`** (`ErrLegacyTablesBusy`) and this message:

  > legacy_tables_busy: another session held a lock on public.accounts,
  > families, sessions or mutation_journal for more than 5s. A client session is
  > refused before this, and a routine autovacuum is cancelled by the waiting
  > import after deadlock_timeout; an anti-wraparound autovacuum does not yield.
  > Nothing was changed: run the import again. If this repeats, check
  > pg_stat_progress_vacuum and wait for the vacuum to finish: \<PostgreSQL error\>

  On the young homelab database, an anti-wraparound vacuum is unlikely. The
  real-autovacuum cancel path is PostgreSQL's own; the tests use a client lock.
- **Tests.**
  - `TestLockStepWaitsForABriefLock`: another session holds a conflicting lock
    for 1 s after `checkAlone`. The run waits and succeeds.
  - `TestLockStepRefusesALockHeldPastTheTimeout`: the lock is held past 5 s.
    - A real import, with its receipt, is refused with `legacy_tables_busy`.
    - The lock step itself waited at least 5 s.
    - The catalog, the FORCE flags and the row checksums are unchanged.
    - Once the lock is gone, the same import commits.
  - The all-or-nothing fault loop now includes `lock-tables` and the care
    load's points between the baby key's drop and its re-add
    (`care-key-dropped`, `care-key-readd`).

The scale test still settles its fixture before measuring.

## Why the audit phase was split (the analysis behind the ruling)

The owner's ruling was to split the phase rather than lower the floor or speed
up the re-walk, so the proposals at the end of this section were not pursued.
Before the split, `authority` did the following, all in one phase:

- loads the authority tables;
- **walks the whole ledger again** inside the transaction (the transaction
  ruling's re-walk);
- builds the 670,000 audit rows;
- copies them in batches of 5,000, synchronously with the walk.

The re-walk alone costs what `ledger_verify` costs: 73 s at L10k. That gives
these bounds:

- With an instant COPY and instant loads, the phase could reach at most 670,000
  rows / 73 s, about 9,200 rows/s.
- **The floor of 10,000 rows/s needs a walk of about 8,200 events/s with no
  other cost.**

The rest of the phase is the audit COPY plus the other loads: 15 s on the dry
run and 40 s on the loaded import run.

Inside the walk, the remaining cost is in the consumer, which runs one event at
a time after each envelope is read:

- `validateCandidate`: a SHA-256, a strict decode and a canonical re-encode
  compared with the bytes;
- a second `decodeEventStrict`;
- `State.apply`. Its `applyAccountRegistered` scans every account for an
  identity-key clash, which is quadratic and cost 7 s of the 8.5 s replay at
  40,000 accounts;
- the importer's visit.

Proposals made to the owner, before the ruling. Each would change verification
code; none was made:

1. **Re-walk forward only, along the verified hashes.**
   - Preflight already holds every verified event hash (`ledgerFacts.hashes`),
     and `visitAudit` already refuses any event whose hash differs.
   - The audit walk could therefore read the envelopes forward along those
     hashes: re-read `head.json`, refuse a moved head, and run
     `validateCandidate` per envelope.
   - It would skip the backward pass and the State replay. That roughly halves
     the re-walk.
2. **Move `validateCandidate`'s prior-independent checks into the read-ahead
   workers.** These are the hash, the strict decode and the canonical
   re-encode; the workers would hand the decoded event to the pass.
3. **Copy the audit batches on a goroutine while the walk continues.** The walk
   does not touch the transaction.
4. **Replace the identity-key scan in `applyAccountRegistered` with an index**
   kept with the account status. This saves about 7 s per walk at 40,000
   accounts and nothing at homelab size.

Proposals 1 and 3 together would have put `authority` at about 45–55 s, which
is 12,000–15,000 rows/s. With the split, the re-walk is a ledger walk and is
held to the ledger-walk floor, and the COPY alone runs at 51,000–75,000 rows/s.

## Reproducing

- `BT_TEST_RACE=0` drops `-race`, `BT_TEST_TIMEOUT` sets `-timeout`, and
  `BT_TEST_VERBOSE=1` adds `-v` (`test-db.sh`, timing runs only; the defaults
  are unchanged).
- `BT_LEGACY_SCALE_PROFILE_DIR=<dir>` writes a CPU profile for each `Run`. On
  macOS, read these with the syscall-attribution caveat above.
- Disk: at L10k, PostgreSQL's volume peaks at about 15 GB (the 6.7 GB legacy
  database plus the new schema and WAL). Run it with at least 30 GB free.
