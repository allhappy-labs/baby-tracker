# Home & tracking redesign rollout — 2026-10-02

The user approved the merge and the production deploy. Local `main` was fast-forwarded from `19071bf` to `2961ca137fae3fbcf0d3e784ec8bee737754c093`, which is the reviewed `feat/home-redesign` head and includes the sync reliability work. No Git remote is configured.

## Pre-merge evidence

- Final fix wave (C1, I1–I5, M1–M9) at `64b48b6`, from a clean tree:
  - `make test` passed: `OK: 1242 tests`.
  - `make server-test`, `make server-smoke`, `make server-protocol-e2e` and `make build` all exited 0.
  - The localization gate found no new problems against the main baseline.
- `make test-ui`: 142 of 143 tests passed at `64b48b6`. The remaining test, `testAuditBottleStartSheet`, was updated in `2961ca1` and then passed alone on the shared simulator.
- The scoped re-review of the fix wave was clean:
  - all 15 findings addressed;
  - no new Critical or Important issues;
  - 3 optional minors, recorded in [the rulings](2026-10-01-home-tracking-redesign-rulings.md).
- The fix wave changed only one server file, a test (`sleep_sessions_test.go`), so the deployed server code is final.

## Server (Unraid `baby-tracker-sync`)

**Preflight**
- Revision `842ab4440e8326b5711a6522ebc934d33b68dfca`, schema 22, instance `<instance-uuid>`.
- The existing verifier passed against the stored unrelated-container baseline: private health and readiness 200, live socket 401 for a bogus bearer, no host ports, baseline unchanged.
- Exports are configured (`BT_EXPORT_DATABASE_URL_FILE`), so migration 25's recovery cleanup reuses the export maintenance pool and no new secret was needed.

**Backups** (mode 600; both passed `pg_restore --list` with 608 entries)

| Dump | When | SHA-256 |
|---|---|---|
| `backup.pre-home-redesign-20261002.dump` | Before the deploy | `614eb1a075bead5f97533b7c98bad256a64d1b0da67192c2cd9f1a3b967614b7` |
| `backup.cutover-2961ca1.dump` | After stopping only the app | `9cfd3193aa5be101db7f143f25bc98fb1886f34f6d7df49812c44053be8bc5f5` |

**Source and image**
- The exact `git archive` of `Server/` at `2961ca1` has SHA-256 `a1b7c5f085c313101e056240db7eb45e946734356ea4dc9ab64158168f40b7bb`. It matched after upload.
- Retained for rollback:
  - the previous source, as `source.842ab44-rollback-2961ca1`;
  - the previous config, as `deployment.env.842ab44-rollback-2961ca1`.
- `provision-unraid-secrets.sh` was run with `BT_ALLOW_SOURCE_UPDATE=1`. It changed only `BT_SOURCE_SHA`/`BT_IMAGE_NAME`; every other line was verified unchanged. `compose config -q` passed.
- Built image: `sha256:a07e4baedaf52f53015bab37c411c10e5cbdf693841e03bf4987b26f181a7cce`, labelled `org.opencontainers.image.revision=2961ca1…`.

**Cutover**
- Stopped only the app, took the cutover dump, then ran `compose run --rm --no-deps migrate`, which exited 0. Schema went 22 → 26, applying migrations 23, 24, 25 and 26.
- Record totals at cutover:

  | Families | Memberships | Feeds | Sleeps | Diapers | Meds | Timer rows | Journal |
  |---|---|---|---|---|---|---|---|
  | 1 | 2 | 96 | 53 | 31 | 3 | 98 | 519 |

- `compose up -d --wait app`: all three containers healthy.

**After the deploy**
- `/v1/server` reports source `2961ca1…`, the unchanged instance ID and recovery epoch 1.
- Advertised capabilities now also include `growth-v1` and `refresh-recovery-v1`.
- Public `/healthz` and `/readyz` returned 200.
- The verifier passed again: health and readiness, live socket, no host ports, unrelated containers unchanged.
- The phones synced right away. Your phone uploaded the history the server had been missing:

  | | Feeds | Sleeps | Diapers | Timer rows | Journal |
  |---|---|---|---|---|---|
  | At cutover | 96 | 53 | 31 | 98 | 519 |
  | Shortly after | 114 | 64 | 35 | 99 | 565 |

  46 journal rows were accepted after the deploy.
- **Bounded log check:** in the first 10 minutes after start (16:37:51Z–16:47:51Z) the app wrote 319 lines, all `INFO` and none `WARN`/`ERROR`. At 16:49:38Z the container had 0 restarts and was healthy, and the cumulative status counts were 366×200, 1×101 (live socket upgrade) and 5×401 (the verifier's fake bearer plus token refreshes). Only level counts and HTTP status counts were read, never raw log lines.

## Phones

- Signed Debug build of `2961ca1`, built from the clean worktree with `xcodebuild -scheme BabyTracker -configuration Debug -destination 'generic/platform=iOS'`:
  - exit 0; log at `/path/to/bt-redesign-phone-build.log`;
  - `codesign --verify --deep --strict` passes;
  - app executable SHA-256 `7f1d4aa131cc71b412b36776e7415e86153f5d856575aad0506e8b12ef91b4c9`.
- **Phone B:** a clean reinstall, as the user approved. `devicectl device uninstall app com.example.babytracker` and then `install app` both exited 0. The old local history was discarded on purpose; the app rejoins and pulls shared data from the server.
- **Phone A:** `unavailable` to `devicectl` during the rollout, as it was not accessible for immediate update. This phone still runs the pre-redesign build against the new server. Plain logging works, as its post-deploy sync shows. Switch, Pause and bottle-inside-a-timer commands from the old build are refused, so the user was told to avoid them until the update.

## Still open

- Install `2961ca1`, or a later build, on Phone A.
- The user's locked-iPhone Live Activity check, and physical UI checks on both phones.
- Follow-up B: full format cleanup and joining past entries into sessions. Its server deploy needs both phones to update at the same time.
