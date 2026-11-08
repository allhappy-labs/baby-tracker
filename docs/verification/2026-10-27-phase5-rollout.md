# Phase 5 rollout — 2026-10-27

**Status: deployed** on 2026-10-27 at 19:17 CET (`670dbba5f8a990ccecfa12a5257a89245506b275`; `Server/` tree `fc9dcbe725d06d3ee412e712a74009dd874a871c`). The user approved the deploy ("deploy"). The record is under "Deploy" below; the plan as prepared follows it.

## Deploy

All times are CET; the host's clock reads PDT.

- **Preflight:** `eaddcef0…` running at schema 34 (`schema_migrations` 32, 33, 34). Three containers were healthy with 0 restarts, `deployment.env` named `eaddcef0`, and 658 GB was free. The database is 43 MB; care rows per kind were `1:2, 2:625, 3:311, 4:190, 5:821, 6:23, 7:1, 8:18, 9:1, 10:3`. Audit: 2,074 rows from 2026-09-26, in partitions `security_audit_events_2026`, `_2027_01` and `_default`. The 2026 partition's range ends 2027-01-01, so retention can drop nothing before 2028-02.
- **Source:** `git archive 670dbba5 Server` (`source-670dbba.tar`, mode 600) has SHA-256 `f8e042d2aa2be5ee5ee9aec1da271cfc46782b403d352280d3424bc085b1810e`, the same on both ends. Against the running source: 212 differing paths, none removed. Of the compose, Dockerfile and deploy files, only `compose.yaml` differs, by the `BT_ADMISSION_AUTH_MAX` pass-through.
- **Backups** (mode 600; 1063 `pg_restore --list` entries each):

  | File | SHA-256 |
  |---|---|
  | `backup.pre-670dbba.dump` (app running) | `56e38637d5e567f2d62cf173c873a805bd2accbdfbea7a59894495d85049ff49` |
  | `backup.cutover-670dbba.dump` (app stopped) | `08d15879c66698e5a5981fb51e11584c288e2be87e8d19c3e93aabd9ebd32f7b` |

- **Rollback set:** `source.eaddcef-rollback-670dbba` and `deployment.env.eaddcef-rollback-670dbba`, both checked identical to what was running. The `eaddcef0` image (`8781d2b64efb`) is kept. The running source was moved aside as `source.eaddcef-displaced-670dbba`.
- **Config:** `provision-unraid-secrets.sh` with `BT_ALLOW_SOURCE_UPDATE=1` changed only `BT_SOURCE_SHA` and `BT_IMAGE_NAME`; `compose config -q` passed. It also printed "Tailscale enrollment remains pending": the one-use auth key file is empty, as it is after enrollment, and the node is enrolled. The unrelated-container baseline was re-captured (61 lines); the previous one is `unrelated-containers.before.prior-670dbba.tsv`.
- **Image:** `baby-tracker-server:670dbba5…` is `sha256:5af1d14d179278834a675dfd919d41a73166b9ae3183650bdcfb51584f9fb2a4`, revision label `670dbba5…`. It was built while the old app ran.
- **Cutover** (app down 19:17:32–19:17:41, 9 seconds):
  - stopped only the app, then took the cutover dump;
  - `compose run --rm --no-deps migrate` exited 0 with no error lines;
  - schema 34 → 37, and `schema_migrations` holds 32–37;
  - care rows per kind were identical before and after;
  - `migrate authority verify`: `scopes=3 rows=2074 failed=0 result=verified`;
  - `compose up -d --wait app`: three containers healthy, the app on `670dbba5…`.
- **After:**
  - `verify-unraid-deployment.sh verify` passed: private HTTPS health and readiness 200, the live socket gives 401 for a bogus bearer, no host ports, unrelated containers unchanged.
  - Startup log: `memory limit` (`source=cgroup_unlimited`), `gc percent` (`gogc=100 source=no_memory_limit`, as planned), and `known refresh tokens loaded` (`digests=11`, 10 ms).
  - All lines INFO, 0 restarts.
  - No phone traffic for the first 13 minutes (only health checks); the phones connect when the app opens or wakes.
  - One phone opened the app at 19:30:53. In order: `GET /v1/server` 200; `/v1/live` 401 (the token had lapsed while the app was closed, as at launch in earlier rollouts); `refresh-recoverable` 200; `sync` 200; `refresh-ack` 204; two more `sync` 200; `family/invitations` 200; then `/v1/live` 101, a socket held 25 s. All 418 log lines INFO; 0 restarts.

## Incident: Live Activity push off after the cutover

Found 2026-10-28 after the owner reported that a Live Activity set on one phone did not update on the other until the app was opened.

- **Cause:** the cutover script composed the app with only `compose.yaml` and `deploy/unraid/compose.yaml`. It left out `deploy/unraid/push.yaml`, which every deploy since 2026-10-10 had included. The app started healthy with no `BT_LIVE_ACTIVITY_PUSH_ENABLED` or `BT_APNS_*` settings, so it sent no pushes. The phones still synced over the live socket whenever the app was open.
- **Evidence:** `live_activity_history` ends at 19:01:21 CET, before the cutover, with every earlier send `accepted`/200. The app's compose `config_files` label names two files, and its environment has no APNs keys. `verify-unraid-deployment.sh` passed because it never checked push.
- **Guard:** the verifier now fails unless the app was composed with `source/Server/deploy/unraid/push.yaml` and runs with `BT_LIVE_ACTIVITY_PUSH_ENABLED=true`. Run read-only against the broken container, the check exits 1.
- **Every `compose` command in this rollout** uses `--project-name baby-tracker-sync --env-file "$ROOT/deployment.env" -f source/Server/compose.yaml -f source/Server/deploy/unraid/compose.yaml -f source/Server/deploy/unraid/push.yaml`.
- **Fix** (owner approved): on 2026-10-28 the app was recreated with all three files, down 07:32:43–07:32:49 CET. Same image `sha256:5af1d14d…`, no migration, 0 restarts. The environment now has `BT_LIVE_ACTIVITY_PUSH_ENABLED=true` and the `BT_APNS_*` settings. The startup log has `live activity sweep`. The new verifier was uploaded (the old copy kept as `verify-unraid-deployment.sh.pre-0fbe1d7`), and `verify` passed with "app composed with push.yaml, Live Activity push on".

## Tailscale sidecar v1.98.3 → v1.104.4 (2026-11-01)

The owner approved it ("Sure update it"). Why: the phones' Tailscale moved to 1.104 around 2026-10-13. From 10-14 the pinned v1.98.3 sidecar logged about 100 TSMP disco-key re-advertisements a day (about 0 before), with DERP "does not know about peer" bursts. Phones' tunnels went dead until Tailscale was toggled. The host's Unraid plugin had updated itself to 1.104.2.

- **Change:** only the two `tailscale/tailscale` image lines in `source/Server/deploy/unraid/compose.yaml` (repo `ff4dffaa`), now `v1.104.4@sha256:ed6480b3…`. The host file then matched `main`'s copy byte for byte. `BT_SOURCE_SHA` and the app image are unchanged; the rest of `source/` is still `670dbba5`.
- **Backups:** `compose.yaml.pre-ts1104` next to it, and `tailscale-state.pre-ts1104.tgz` (mode 600).
- **Recreate:** `compose pull tailscale`, then `compose up -d --no-deps --wait tailscale` with all three files. The sidecar was down 21:30:52–21:31:00 CET. The app and db were untouched (the app is not in the sidecar's network namespace).
- **After:**
  - `tailscale version` 1.104.4, same node `baby-sync` 100.64.0.7; Serve still proxies to `baby-tracker-sync-app-1:8080`.
  - The verifier first failed on the unrelated-container snapshot only: `bet`, `notetaker` and `plainpost-sync-node` had changed since the 10-27 baseline. The baseline was re-captured (previous kept as `unrelated-containers.before.prior-ts1104.tsv`), and `verify` then passed in full.
  - Both phones synced and opened `/v1/live` at 21:32 CET over direct paths.
- **Known, not new:** each sidecar start logs `health(warnable=router)`, because iptables in the container is denied the filter table. v1.98.3 logged the same at every start, and Serve does not depend on it.
- **Rollback:** restore `compose.yaml.pre-ts1104` and run the same `up -d --no-deps tailscale`.

## The plan as prepared

**What ships.** Phase 5 (PostgreSQL authority load measurement and tuning) merged into `main` as `0ab03016628740cbfa1fc565683c3a34205672aa`. Server only: Phase 5 changed no app code, only iOS test files, so the phones need no new build. `Server/` at `0ab03016` is identical to the Phase 5 branch's final code. That code passed the final gates and the 2x 30-minute PASS soak on the load node ([report](2026-10-26-phase5-load.md)).

### Scope

- **Running (last record, to confirm):** `eaddcef0` at schema 34 ([pumping rollout](2026-10-16-pumping-rollout.md)). `git log eaddcef0..0ab03016 -- Server` is 203 commits, all Phase 5. Most are the load harness, generator and seeder, which do not ship in the app image.
- **Source:** deploy `main`'s head at deploy time. Its `Server/` tree must be `fc9dcbe725d06d3ee412e712a74009dd874a871c` (`git rev-parse <head>:Server`): `0ab03016`'s server code plus the README's migrations paragraph (`cf2b8fb6`). A different tree means `Server/` changed after this plan, and the plan needs re-scoping. `git archive` embeds the commit ID, so its hash is recorded at upload, not here.
- **Compose and image:** the Dockerfile and `deploy/unraid/compose.yaml` are unchanged since `eaddcef0`. The base `compose.yaml` only passes through the new optional `BT_ADMISSION_AUTH_MAX`. **No config change is needed:** `provision-unraid-secrets.sh` with `BT_ALLOW_SOURCE_UPDATE=1` should change only `BT_SOURCE_SHA` and `BT_IMAGE_NAME`.
- **Migrations 34 → 37, one way.** `compatibility.json` is `{"min":37,"max":37}`, so the new binary runs only on schema 37 and the `eaddcef0` binary does not start on it. A rollback means restoring the cutover dump.
  - `000035_audit_retention.sql`: audit partitions are dropped after 13 months, behind a retention anchor; routine refresh rotation writes no audit row (D1 (c), confirmed by the owner 2026-10-27). The homelab's audit rows were created at or after the 2026-10-10 cutover (51 imported then), so none should be old enough to drop: step 1 checks the oldest audit timestamp.
  - `000036_request_authority_family_key.sql`: `request_authority` finds the family by key.
  - `000037_family_delete_cascade.sql`: family-owned import mappings cascade on a family delete; RLS context helpers become inlinable; the Live Activity work sweep is paged. It applied to the 10,000-family load database in 401 ms; the homelab database is about 26 MB.

### What changes on the homelab

| Change | Homelab effect |
|---|---|
| `BT_ADMISSION_WAIT` default 0 → 250 ms | A request that finds no free slot waits up to 250 ms instead of an immediate 503. Never reached at homelab load. |
| Auth lane, `BT_ADMISSION_AUTH_MAX` default 8 | Sign-in, and refreshes with tokens the node did not issue, use 8 slots of their own. At startup the app loads the live sessions' refresh digests (log line `known refresh tokens loaded`), so the phones' refreshes after the restart stay in the normal class. |
| `BT_SOCKET_PROCESS_LIMIT` default 3,700 → 1,700 | Not reached: the homelab has two phones (`BT_SOCKET_DEVICE_LIMIT` 2 stays). |
| GOGC 400 under a memory limit | Applies only when the app has a cgroup memory limit or `GOMEMLIMIT`. The Unraid compose sets neither, so expect `gc percent` `gogc=100 source=no_memory_limit`, and no change. |
| Hub ping-race fix | A live socket retired after a token refresh now closes with the right code. Previously a ping in flight could make it close 1013 or drop. |
| Fewer refresh round trips (12 → 4 recoverable, 9 → 2 ack); paged pulls re-authorize; bearer checked before the body; stored records served as stored | No change on the wire for current phones (protocol v1; both protocol e2e modes passed). |

### Steps (the pumping rollout's sequence)

Every `compose` below means `docker compose --project-name baby-tracker-sync --env-file "$ROOT/deployment.env" -f source/Server/compose.yaml -f source/Server/deploy/unraid/compose.yaml -f source/Server/deploy/unraid/push.yaml`. Without `push.yaml`, the app runs with Live Activity push off.

1. **Preflight (read-only).** Confirm the running revision label, `deployment.env`'s `BT_SOURCE_SHA`, schema version 34 and `schema_migrations` 32–34, three healthy containers with 0 restarts, disk space, and the oldest audit row's timestamp (anything older than 13 months would be dropped by 000035's retention). If any of these differ from the record, stop and re-scope.
2. **Source.** Check the `Server/` tree above, upload `git archive <head> Server` next to the running source, and check its SHA-256 after upload against the one computed before it. Diff it against the running source: only Phase 5 files may differ.
3. **Backups**, mode 600, each read back by `pg_restore --list`: `backup.pre-<short>.dump` (`pg_dump -Fc`, app running).
4. **Rollback set:** `source.eaddcef-rollback-<short>` and `deployment.env.eaddcef-rollback-<short>`. Keep the `eaddcef0` image.
5. **Config:** `provision-unraid-secrets.sh` with `BT_ALLOW_SOURCE_UPDATE=1`; only `BT_SOURCE_SHA` and `BT_IMAGE_NAME` may change; `compose config -q`. Re-capture the unrelated-container baseline (`verify-unraid-deployment.sh capture`), keeping the previous one.
6. **Image:** `compose build app migrate` while the old app runs. Record the image ID and the revision label, which is the deployed head.
7. **Cutover** (the app is down for seconds):
   - stop only the app, then take `backup.cutover-<short>.dump`;
   - `compose run --rm --no-deps migrate` exits 0 with no error lines;
   - schema 37, `schema_migrations` 32–37;
   - care rows per kind identical before and after;
   - `compose run --rm --no-deps migrate authority verify` passes;
   - `compose up -d --wait app`: three containers healthy, the app on the deployed head.
8. **After:**
   - `verify-unraid-deployment.sh verify` passes;
   - the startup log has `memory limit`, `gc percent` and `known refresh tokens loaded` (with the phones' session count, about 2–4);
   - the first minutes of logs are INFO only, with 0 restarts;
   - both phones sync and their live sockets reconnect.

### Rollback

Before any phone writes, or on the owner's decision after:
1. Stop the app.
2. Recreate the database from `backup.cutover-<short>.dump` (`pg_restore --single-transaction --exit-on-error`, as the 2026-10-10 runbook does).
3. Put back the `eaddcef` rollback source and `deployment.env`.
4. `compose up -d --wait app`, check schema 34, then run `verify`.

Writes made after the cutover are lost. The phones need nothing: they are on protocol v1 either way.

## Still open

- Never run a schema-34 binary against this database again: the rollback is the cutover dump, and it loses the writes made since.
