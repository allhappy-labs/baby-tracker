# Live Activity check and the replaced-card stall — 2026-10-12

The owner asked whether Live Activities still work after the recent updates, the two-caregiver sync of cards above all, then to fix what was found and deploy. The fix is `431dc6cc`, merged to `main` and deployed to the homelab on 2026-10-12 (the owner: "merge and deploy"). The first deploy attempt was refused by the permission system and nothing was worked around; the owner asked again.

## What production showed (read-only, server `339f655f`, schema 33)

- Both phones are registered (sandbox, not revoked, authority valid). Apple accepts deliveries every day (HTTP 200).
- Two feed jobs, one per phone, never complete: `completed_version` behind `dirty_version`, `not_before` always now, 22 356 and 106 028 attempts. Each writes an `end` history row with no result about every two seconds: 43 294 on 10 October, 54 084 on 11 October (97 711 rows, 20 MB).
- Their saved state is the same on both: `status=terminal`, `pending_event=end`, an `activity_id` still registered active with a token, and `dismissed_generation` equal to `generation_id`. Another card of the same generation is registered `dismissed` by the phone.
- Registration `55d8b250` (Phone B) has no accepted feed delivery in the history at all; its job has spun since 2026-10-09 23:57 UTC. Registration `b848ed15` (Phone A) has spun since 2026-10-11 06:12 UTC. Sleep cards deliver on both.
- Eleven feed cards on `55d8b250` and one on `b848ed15` are still `active` with a token although their feeds ended. The app does not report cards it retires itself; the server's own end retires them, and that end is what was stuck.

## Cause

`Worker.snapshot` marked a generation dismissed as soon as any of its cards was reported ended or dismissed by the phone, even while another card of that generation was still registered active. `Reconcile` then sends nothing to a dismissed generation, so the surviving card froze. When the lifecycle ended, `Reconcile` decided `end` (the job still had an activity), `dispatch` wrote the intent and abandoned it at the re-read because the generation was dismissed, and rescheduled a second later, forever. A terminal job that still holds an activity also takes the `end` branch before any new lifecycle, so no later feed could start or be adopted on that phone.

## Fix

A generation is dismissed only when none of its cards is left, and a saved dismissal gives way to a card registered after it (`Server/internal/liveactivity/worker.go`). Two tests, both failing before the change: `TestLiveActivityReplacedCardLeavesTheSurvivorLive` (ended and dismissed) and `TestLiveActivityCardRegisteredAfterADismissalIsLive`. No migration, no wire change, no app change.

## Gates

| Check | Result |
| --- | --- |
| `make test` (Swift, unchanged by the fix) | `OK: 1856 tests` |
| UI: `LiveActivityPushUITests`, `RunningTimerLifecycleTests`, `WidgetTourTests` (unchanged by the fix) | 8 tests, 0 failures |
| `go vet ./...`, both tags | clean |
| `Server/scripts/test-db.sh` (race, disposable PostgreSQL) on the fix | 39 packages ok, none failed |
| `Server/scripts/protocol-e2e.sh` on the fix | exit 0; `live-activity proof` steps 1–13; Postgres agrees: registrations=2 sandbox=2 cards=3 delivered=14 |

## Phones (read-only)

Both phones' registration ledgers were copied from the app group container: device accepted, sandbox, not revoked, no pending removals, no unconfirmed starts, and one active sleep card each, the same two the server holds for the running sleep.

## Rollout (Unraid `baby-tracker-sync`, 2026-10-12)

- **Merge.** `main` fast-forwarded from `1bf8b151` to `90468e2a`.
- **Source.** `git archive 431dc6cc Server`, SHA-256 `e8d8a93c5a87a13a88377481ec92813f7c5b65957c8fe4bc7fa2a807ebb726c3`, matched after upload. Against the running source only `worker.go` and `worker_outcomes_test.go` differed.
- **Backups** (mode 600, 1030 `pg_restore --list` entries each): `backup.pre-431dc6c.dump` `36fcbc0b1d53e1b106220ef622105729da2ecb9d82b6131fcf061f8f3a3a9c59`; `backup.cutover-431dc6c.dump` `4fe9f6b5fb186b7b548633b15de27e27b0853aa629abc7215539caad781943f0`.
- **Rollback set.** `source.339f655-rollback-431dc6c`, `deployment.env.339f655-rollback-431dc6c`; the `339f655f` image is retained. Schema stays 33, so the old binary still starts.
- **Config.** Only `BT_SOURCE_SHA` and `BT_IMAGE_NAME` changed; `compose config -q` passed; baseline re-captured (61 lines).
- **Image.** `baby-tracker-server:431dc6cc…` is `sha256:6c19945b7d7d9a25c2278f0918520c285b5874bc306e0cad7d2d5a54381ca418`.
- **Cutover.** App down 05:32:37–05:32:45 UTC (8 seconds). No migration. `verify-unraid-deployment.sh verify` passed. 0 restarts.

## After the deploy (read 05:33 UTC)

- Each stuck job sent its overdue `end` (410 `ExpiredToken`, the card retired) and completed. All eight jobs have `completed_version = dirty_version`.
- The ten other leftover feed cards on `55d8b250` were ended as duplicates (one 200, ten 410 `ExpiredToken`) within 15 seconds.
- A feed was running: both phones' feed cards took an accepted `update` (200) right after.
- Active cards left: one feed card per phone and one sleep card, none stale by the server's state.
- Log since start: 18 INFO, 13 WARN, all 13 the expected "push not accepted" lines for the expired tokens.

## Not done

- **History cleanup.** The owner asked for the loop's empty `end` rows to be deleted. The permission system refused the `DELETE`; nothing was worked around. The rows are inert and the worker's own retention removes history older than seven days, so the last of them is gone by 2026-10-19.
- **The phones' Lock Screen check** is the owner's.
