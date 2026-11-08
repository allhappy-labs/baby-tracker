# Phase 5 handoff: paused in Wave D (2026-10-25)

The owner paused Phase 5 on 2026-10-25 ("pause everything, we'll continue next
week"). This note is enough to resume without the session's history. The
detailed ledger is `.superpowers/sdd/2026-10-16-postgres-authority-phase5/progress.md`
(git-excluded, in this worktree); the plan is
`docs/superpowers/plans/2026-10-16-postgres-authority-phase5.md`.

## Where things are

- **Branch:** `feat/postgres-authority-phase5`, worktree `.worktrees/phase5`. Nothing is
  pushed, merged or deployed. Use `/usr/bin/git` in the worktree.
- **Done:** Waves A–C (Tasks 1–13, 27), and in Wave D:
  - Task 14 code: GOMEMLIMIT at 85% of the cgroup, socket bound 65,536. The socket
    default is still 1,000 because the live-only memory cell has not run.
  - Task 15 observation (sweep timing, closes). The chunked sweep is on side branch
    `p5-w1-a-chunk2` (a1ce3089) and is applied only if the rule fires.
  - Task 16: hosted profile measured.
    - Pool 48 / admission 96 (7c355169).
    - shared_buffers 5GB (9638a3d1). This deviates from the plan's literal 3GB: the
      rule still fired at 3GB, and the VM keeps no file cache for the db.
    - wal_compression lz4 (e30a519a).
  - Task 17 code: AdaptiveBatch grow-back, per-pass sweep log, retention by ctid.
  - Task 18 code:
    - sink latency;
    - SendBudget;
    - settle quarantine;
    - wake/LA observability;
    - final tracker flush after the HTTP drain.
  - Task 22: 000037 holds the family-delete cascade plus the measured tuning DDL:
    - inlinable RLS helpers;
    - paged LA work sweep;
    - request_authority single read with `plan_cache_mode`.
  - Task 23 Step 1: flush on stop.
  - Task 21, four iterations:
    - H1, 000036;
    - C1/C2/C5;
    - lz4 and shared buffers;
    - R1/R3/R4/R5a round trips;
    - pull with COMMIT;
    - no JSON re-compaction;
    - generator decode.
  - Load tooling:
    - runner `upgrade [--recreate-db]`;
    - restart cell;
    - hot-family seed;
    - backlog/APNs/hub-wake samplers;
    - hot maintenance;
    - live-only override;
    - generator profiling and `generator_runtime`;
    - plan time / WAL / blocker instrumentation;
    - the burn bound that reads cgroup CPU.
- **Phase head:** `7d842728`, with W4 merged, plus this note. `make server-test` is
  green at 7d842728 (40 packages ok, 0 FAIL).
- **Load stack:** down (`load-node.sh down --keep-volume`).
  - Volume `bt-load_database` is kept: the seeded R10k database (31.5 GB data,
    ~36 GB volume), schema 37.
  - Its 000037 digest is W3's (`2d656b03…`). W4 did not change 000037, so `upgrade`
    accepts the head.
  - The lock is released. No script is running.
- **Run directory:** `/path/to/bt-load/d1-20261024-1613`. It holds runs/1–13
  with result.json, audit, pg_stat_statements, cluster deltas and profiles.
  Helper scripts are in `/path/to/bt-p5d` (`block*.sh`, the A/B SQL).
  `/private/tmp` survives until a reboot only.

## What the measurements say (10,000 families, 2x, W5 M10–15)

| Run | Code / config | Host | Unexpected | Note |
|---|---|---|---|---|
| Task 13 2x | pre-H1 | contaminated | 80.6% | request_authority 84% of db time |
| noiseA/B | H1 (000036), pool 32 | contaminated | 44% / 22% | noise baseline unusable on this machine |
| w2A / c1old / w2A2 | + C1/C2/C5 (ABA on C1) | contaminated | 44 / 35 / 26% | C1: LA registration sweep 1.8× faster, authority unchanged |
| walz | + lz4 | contaminated | 26.8% | WAL −32%, FPI −27% |
| w3 | + R1/R3/R4/R5a, sb 3GB | contaminated | 29.3% | authority 0.74 ms (was 1.39) |
| **sb5** | + shared_buffers 5GB | **clean** | **13.3%** | hit ratio 98.4%; pull 1.47 ms |
| **p48a96** | + pool 48 / admission 96 | **clean** | **6.33%** | best so far |
| p64a128 / p24a48 | pool sweep | mild / contaminated | 6.8% / 38% | 48/96 chosen |

- **Audits:** every run's audit found zero lost, cross-family or diverged writes.
- **Not yet measured live:** W4 (pull + COMMIT, no JSON re-compaction, generator
  light decode). The generator's own scheduler lag (34–67 ms p99 against the 20 ms
  gate) should fall well under 20 ms by the benchmarks (0.9–2.3 ms on 2 cores).
- **The node's limits:**
  - db CPU near its 4-CPU cap, 57% of it kernel time;
  - WAL fsync 6–9 ms on OrbStack's virtual disk;
  - no guest file cache, so reads cost ~1.6–1.8 ms;
  - connections idle in transaction between round trips.
- **Contamination:** other sessions' Xcode/Swift/Go builds touched most runs. Only
  sb5 and p48a96 are clean. Compare runs by mechanistic metrics (per-statement means,
  reads, fsyncs, pool waits) rather than the unexpected ratio alone.

## Resume, in order

1. Check `/path/to/bt-p5d` and the run directory still exist (a reboot clears
   `/private/tmp`). If gone, re-seed (step 4's fresh seed): `down` (volumes too),
   `preflight`, `up`, `seed --families 10000 --seed 1 --history r10k --backlog-load 2`
   (add `--hot-family` for Task 20), `start`.
2. If the run directory exists, take the lock and move the stack to the head:
   `preflight --families 10000` with `BT_LOAD_RUN=/path/to/bt-load/d1-20261024-1613`,
   then `load-node.sh up`. If up refuses an existing run, use `upgrade --run … --recreate-db`.
   When a later commit edits the undeployed 000037 again, `upgrade` refuses with
   `migration_checksum_mismatch`; apply that change's apply-SQL with the new digest
   first (the W3 pattern: `w3-g-apply.sql -v digest=1`).
3. Run one 2x soak at the head (the W4 A/B against p48a96). Read `generator_runtime`
   in result.json: high lag with a low generator scheduling latency means host CPU
   contention, not the generator.
4. Then the cells `block5.sh` lists (Tasks 14, 15, 16 Step 4, 19, 20, 23, 24), each
   with an audit:
   - ramp 0.5–2x (the ceiling);
   - live-only memory (sockets 4k/8k; Task 14 sets the socket default from it);
   - storm (Task 15's chunking rule);
   - restart;
   - mass snapshot;
   - hot maintenance;
   - flood;
   - export;
   - import.
   - Hot family needs a seed with `--hot-family`: fold it into Task 25's fresh seed.
5. Task 21 stops when one of its rules holds: the 2x soak passes, no candidate is
   ≥5%, or six iterations are done. Four are done.
6. Task 25: a fresh seed, then the 30-minute 2x PASS soak, the tightened thresholds
   and the identity evidence. The report keeps every failed soak.
7. Task 26: the iOS capacity matrix, foreground latency and protocol-e2e. These use
   the shared simulator <simulator-udid>, one run at a time.
   Other sessions use it for another app; wait for `pgrep xcodebuild` to be empty.
8. Task 28: the report (docs/verification/…-phase5-load.md is in progress since
   66de046e), the carries close-out and the final gates (including `make test`).

## Decisions taken in the pause window (for the owner to confirm at the end)

- **Kept:**
  - `synchronous_commit` stays on.
  - Authority is never cached across requests.
  - The charge-before-care precedence stays, so the pull is not folded into a single
    statement (R2 was rejected).
- **request_authority (R1):** if the caller's transaction already holds non-uuid text
  in `app.family_id`/`app.account_id`, a non-matching lookup now raises 22P02 (it
  was matches=0). No code path sets such a context; the case is pinned in a test.
- **Paged pulls:** a pull page with more pages is refused, with the code a new request
  would get, if the session, account or family changes mid-response (docs/protocol/v1.md
  ruled changes). This is stricter than before.
- **Stored wires:** sync now serves them verbatim. A corrupt stored wire, which only an
  out-of-band SQL write could produce, now yields invalid JSON with a 200 instead of a
  500. Carry: an offline wire verifier or a sampled check.
- **D1 wording:** the owner still has to confirm the D1 (c) spec wording at phase end
  (Task 27).
- **Not done yet:** the 000037 homelab apply (with the app stopped) and the migration
  number at merge (Task 22's rename rule). main has no 000035–37.

## Housekeeping

- **Agent worktrees:** these can go once their commits are confirmed on the branch
  (all were cherry-picked; check with `git cherry`):
  - under `.claude/worktrees/`: `agent-a0dc0502…`, `agent-a35f535a…`,
    `agent-a638907a…`, `agent-a7601517…`, `agent-a7eb59a3…`, `agent-aade3ef7…`,
    `agent-ab0fb693…`, `agent-ab7af396…`, `agent-ac0980ea…`, `agent-af62a378…`,
    `p5-authority-join`, `p5-tooling-fixes`;
  - `agent-af2c8a62…` is not this session's; leave it.
- **Side branch:** keep `p5-w1-a-chunk2`; it is conditional.
- **Disk:** the kept volume holds ~36 GB in OrbStack's VM. Drop it
  (`docker volume rm bt-load_database`) only if a fresh seed is accepted.
- **Leftover:** the empty run directory `/path/to/bt-load/smoke-20261023-0053` from
  Wave C is still there for the owner to delete.

## Plan agreed after the pause: a dedicated load VM

The owner keeps the Mac busy with other work, so a quiet Mac cannot be
guaranteed. Proposed split (pending the owner's VM details):

- **Load runs move to a dedicated Linux VM** on the owner's Windows machine, reached
  over SSH. That covers every remaining Wave D/E cell, the ramp and the Task 25 PASS
  soak. The reference node label changes accordingly (an owner decision); the Mac
  runs stay in the report as development evidence.
- **Mac keeps:**
  - code, agents and unit/DB tests;
  - Task 26 and the final `make test`, on the shared simulator, scheduled when no
    other `xcodebuild` runs, with no whole-machine idle gate for those.
- **VM spec asked for:**
  - Ubuntu 24.04 server with Docker Engine (apt), plus git, jq, curl and rsync;
  - ≥ 12 vCPU, ≥ 24 GB RAM, 150 GB fixed SSD/NVMe disk;
  - SSH key login with the user in the `docker` group;
  - the Windows host kept quiet during runs (Windows Update and AV scans paused).
- **SSH key:** `~/.ssh/bt_loadnode_ed25519` (public key `claude-phase5-loadnode`)
  was created for it.
- **Code reaches the VM by `rsync` over SSH** (no GitHub push). The stack makes its
  own throwaway secrets; no real credentials go there.
- **On the VM, `load-node.sh` needs a Linux variant of its host checks:** the idle
  check (it reads macOS `sysctl`/`ps`), the host facts and OrbStack's `orb config`.
  Make that a small change, and keep the macOS path.
