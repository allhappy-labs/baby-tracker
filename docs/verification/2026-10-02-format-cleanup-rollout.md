# Format cleanup rollout (follow-up B) — template

**Status: template.** Fill in every `<…>` on the day and delete the guidance in *italics*. This follows the runbook in [the spec](../superpowers/specs/2026-10-02-format-cleanup-design.md#runbook) and uses the evidence style of [the 2026-10-02 rollout](2026-10-02-home-redesign-rollout.md).

- **Phase S** deploys the server track now. Migration `000027_format_cleanup.sql` takes the schema from 26 to 27 and is not overlap-safe, so the app stays stopped while `migrate` runs. During the interim, Marta's iPhone keeps logging on the installed `2961ca1` build.
- **Phase C** happens days later: both phones delete the app and fresh-install the final client.

Tooling, all read-only against the database except `migrate`:

| Script | What it does |
|---|---|
| `Server/scripts/format-cleanup-preflight.sql` | Counts and must-be-0 checks at schema 26, the join replay, and a closing `FORMAT_CLEANUP_BEFORE …` line |
| `Server/scripts/format-cleanup-postflight.sql` | Every S-4 invariant as PASS/FAIL against that line, plus a 14-day session listing; `-v listing_only=1` prints only the listing |
| `Server/scripts/format-cleanup-dry-run.sh <dump> <image>` | Restores a dump into a disposable container on the Unraid host, then runs preflight, `migrate` and postflight, and always cleans up |

The scripts print counts, PASS/FAIL lines and session times only. They never print names, notes, food text, ids or volumes. The dry-run keeps the family data on the Unraid host.

Shell setup on Unraid. Paste it once per SSH session; every later block relies on it.

```sh
export BT_DEPLOY_ROOT=/mnt/user/appdata/baby-tracker-sync
umask 077          # dumps and .out files are created 0600
set -o pipefail    # a pipeline fails when any command in it fails
new_sha=<40-character reviewed sha>; short=<short-sha>
old_sha=2961ca137fae3fbcf0d3e784ec8bee737754c093
src="$BT_DEPLOY_ROOT/source"                 # the deployed source tree compose runs from (confirm the path)
staged="$BT_DEPLOY_ROOT/source.$short-staged" # the reviewed source, unpacked in S-2
# deployment.env is deliberately NOT exported into this shell. Compose reads
# it through --env-file only, so the BT_SOURCE_SHA/BT_IMAGE_NAME that
# provisioning rewrites take effect on the next compose call. Exported shell
# variables would override --env-file, and S-5 would then tag the new build as
# the 2961ca1 rollback image.
unset BT_SOURCE_SHA BT_IMAGE_NAME
compose() {
  docker compose --project-name baby-tracker-sync \
    --env-file "$BT_DEPLOY_ROOT/deployment.env" \
    -f "$src/Server/compose.yaml" -f "$src/Server/deploy/unraid/compose.yaml" "$@"
}
db_psql() { docker exec -i baby-tracker-sync-db-1 psql -X -q -v ON_ERROR_STOP=1 -U postgres -d babytracker "$@"; }
# gate LINE FILE: succeeds only if FILE holds exactly LINE; otherwise prints STOP.
gate() { grep -qxF "$1" "$2" 2>/dev/null || { echo "STOP: $2 does not contain '$1'" >&2; return 1; }; }
revision() { docker image inspect --format '{{with .Config.Labels}}{{index . "org.opencontainers.image.revision"}}{{end}}' "$1"; }
# The new image carries the new sha, the retained 2961ca1 image still exists
# with its own label, and the running app still uses it.
images_ok() {
  [ "$(revision "baby-tracker-server:$new_sha")" = "$new_sha" ] &&
  [ "$(revision "baby-tracker-server:$old_sha")" = "$old_sha" ] &&
  [ "$(docker inspect --format '{{.Config.Image}}' baby-tracker-sync-app-1)" = "baby-tracker-server:$old_sha" ] ||
  { echo 'STOP: image proof failed (new label, retained 2961ca1 image, or running app image)' >&2; return 1; }
}
rollback_src="$BT_DEPLOY_ROOT/source.2961ca1-rollback-$short"
rollback_env="$BT_DEPLOY_ROOT/deployment.env.2961ca1-rollback-$short"
cutover="$BT_DEPLOY_ROOT/backup.cutover-$short.dump"
pre="$BT_DEPLOY_ROOT/format-cleanup-preflight-cutover.out"
post="$BT_DEPLOY_ROOT/format-cleanup-postflight-production.out"
```

Every gated step below is a single `&&` chain that begins with its gate. A failed gate prints `STOP` and skips the commands it guards, even when blocks are pasted together. Do not continue past a `STOP`.

## Pre-deploy evidence

- Reviewed server track on `main` at `<sha>`. It must contain `79aa8f0` (the Task 6 head) and the Task 7 tooling commit; check with `git merge-base --is-ancestor 79aa8f0 <sha>`.
- From a clean tree at `<sha>`:
  - `make server-test`: `<exit 0>`;
  - `make server-smoke`: `<exit 0>`;
  - `make server-protocol-e2e`: `<exit 0>`. This exercises the interim `2961ca1` client against the B server.
- Local rehearsal (Task 7, 2026-10-03), done before the reviewed sha existed:
  - **Database.** The dry-run ran against a seeded schema-26 dump. The dump was built the production way: postgres by digest, init-roles/init-users, migrations 1–26 as `baby_tracker_migrator`.
  - **Seed.** It holds every production shape class. Feeds v2/v3/v4, linked and unlinked, on both sides of the B-R9 cutoff. Sleeps v1/v2. Timer histories with `timer_switch_side`, `timer_start_bottle`, `timer_pause` and `timer_attach_bottle`. Receipts with related entities. Tombstones, a root with a linked member that B-R13 moves whole, and a second baby with a running sleep timer.
  - **Image.** Built from `1313f62` (B-R13, Task 4 fix round 2); the label showed that revision. Task 7 first ran it on `1eec4cd` under B-R10 (373 heads, 39/39); the numbers below replace those.
  - **Preflight:** 11/11 PASS. It expected 358 converted entities, 144 joined (64 feeds, 80 sleeps; 16 of them not converted), 1 relinked and 375 appended heads.
  - **Postflight:** 41/41 PASS, and the 14-day listing had 279 sessions.
  - **Cleanup.** No container, network or volume was left. No canary name, note or food string, and no id, appeared in the output.
  - **Failure paths.** Planted defects (a v1 feed, a solids part without `foods`, an unfinished import) failed the preflight and stopped the run before `migrate`. A server image without the admin binary failed at `migrate`. A truncated dump failed at `pg_restore` and printed only an error count and a data-free first line. SIGTERM mid-run exited 143. Each of these runs cleaned up fully.

## Phase S — deploy now

### S-1 Communicate

- [ ] Owner phone (pre-redesign build): no logging. Prefer airplane mode until the install.
- [ ] No timers running on either phone.
- [ ] **The second caregiver's phone shows "Up to date" just before the cutover.** Rows from before `2961ca1` that are still unconfirmed would be refused by the B server, which would cause a short stall. Re-check this right before `compose stop app` in S-5.

### S-2 Read-only preflight on production

- [ ] Stage the reviewed source. Upload `git archive --format=tar <sha> Server` as `$BT_DEPLOY_ROOT/source-<short-sha>.tar`, record its SHA-256 (`<…>`), and confirm it matches the local one:

```sh
sha256sum "$BT_DEPLOY_ROOT/source-$short.tar"
mkdir "$staged" && tar -x -C "$staged" -f "$BT_DEPLOY_ROOT/source-$short.tar"
```

- [ ] Run the verifier, read `/v1/server` and the schema, then the preflight:

```sh
"$src/Server/scripts/verify-unraid-deployment.sh" verify
curl -fsS https://baby-sync.example-tailnet.ts.net/v1/server | jq -r .source_version    # must be $old_sha
db_psql -At -c 'SELECT current_version FROM schema_compatibility'                 # must be 26
db_psql < "$staged/Server/scripts/format-cleanup-preflight.sql" > "$BT_DEPLOY_ROOT/format-cleanup-preflight-s2.out" 2>&1
cat "$BT_DEPLOY_ROOT/format-cleanup-preflight-s2.out"
gate 'PREFLIGHT: PASS' "$BT_DEPLOY_ROOT/format-cleanup-preflight-s2.out" && echo 'S-2: PASS'
```

- Result: `PREFLIGHT: <PASS>`. Every check must PASS: schema 26, v1 feeds and timers, scalar or `completed_sides` payloads in the journal and receipts, solids parts without `foods` (B-R12), unfinished imports, running timers below v5, and live feeds without parts.
- Records per version:

  | Feeds v2 / v3 / v4 | Sleeps v1 / v2 | Timers v2 / v3 / v4 / v5 (live) | Journal | Receipts |
  |---|---|---|---|---|
  | `<…>` | `<…>` | `<…>` | `<…>` | `<…>` |

- Expected changes: `<converted>` converted, `<joined>` joined, and `<appended>` heads to append (the union of converted and joined).
- *If anything is FAIL: stop. Migration 27 would refuse to run (B-R2), or would migrate a B-R12 solids part it cannot fix. Fix the cause first, for example by stopping a running timer or letting an import expire.*

### S-3 Backup

```sh
dump="$BT_DEPLOY_ROOT/backup.pre-format-cleanup-<yyyymmdd>.dump"
docker exec baby-tracker-sync-db-1 pg_dump -Fc -U postgres -d babytracker > "$dump" &&
sha256sum "$dump" &&
docker exec -i baby-tracker-sync-db-1 pg_restore --list < "$dump" | wc -l
```

| Dump | When | SHA-256 | `pg_restore --list` entries |
|---|---|---|---|
| `backup.pre-format-cleanup-<yyyymmdd>.dump` | Before the dry-run | `<…>` | `<…>` |

### S-4 Dry-run on a restored copy (on the Unraid host)

- [ ] Build the B image from the staged source under its own tag, check its revision label, run the dry-run, then remove the image:

```sh
docker build --build-arg BT_SOURCE_SHA="$new_sha" -t "bt-format-cleanup:$short" "$staged/Server" &&
[ "$(revision "bt-format-cleanup:$short")" = "$new_sha" ] && echo 'IMAGE LABEL: OK'
dry="$BT_DEPLOY_ROOT/format-cleanup-dry-run.out"; rm -f "$dry"
[ "$(revision "bt-format-cleanup:$short")" = "$new_sha" ] &&
"$staged/Server/scripts/format-cleanup-dry-run.sh" "$dump" "bt-format-cleanup:$short" > "$dry" 2>&1; cat "$dry"
gate 'DRY-RUN: PASS' "$dry" && echo 'S-4: PASS'
docker ps -a --filter label=org.babytracker.format-cleanup-dry-run -q   # must print nothing
docker image rm "bt-format-cleanup:$short"
```

- Image proof: `<image id>`, revision `<sha>`. *It must be the full reviewed sha, at or after `79aa8f0`. The Task 6 gates ran on an uncommitted tree and their image showed `8ffb262`, so check the label rather than assume it.*
- Result: `PREFLIGHT: <PASS>`, `POSTFLIGHT: <PASS>` (`<41>`/`<41>`), `DRY-RUN: <PASS>`, `CLEANUP: removed …`.
- Key invariants:

  | Invariant | Expected | Got |
  |---|---|---|
  | Rows unchanged (feeds, sleeps, diapers, meds, growth, measurements, timers, babies) | `<…>` | `<…>` |
  | Journal rows after = before + appended | `<…>` | `<…>` |
  | Appended heads = converted ∪ joined (count and digest) | `<…>` | `<…>` |
  | Joined by 27 = the preflight's join replay (count and digest) | `<…>` | `<…>` |
  | `next_sequence` advanced by the appended count | `<…>` | `<…>` |
  | Manifest rows for 27 = rows for 26 − 3 | `<…>` | `<…>` |

- [ ] **The user spot-checks the session listing.** Joins should appear only where the gap is at most 30 min (feeds) or 15 min (sleeps). For feeds, no sleep may fall in between, and solids are never joined. Sessions marked `already linked` were linked live or with Combine. Result: `<ok / notes>`.
- [ ] Risk 11: on a Mac with a paired phone, check that the `devicectl` App Group copy (C-2) works: `<works / not supported>`.
- *If anything FAILs, do not deploy. Keep `format-cleanup-dry-run.out` for the fix.*

### S-5 Deploy (the 2026-10-02 procedure)

- [ ] **Rollback set, then the new source and config.** The 2961ca1 source moves aside and its config is copied. Nothing below moves or edits these rollback copies again:

```sh
[ ! -e "$rollback_src" ] && [ ! -e "$rollback_env" ] &&
mv "$src" "$rollback_src" && cp -p "$BT_DEPLOY_ROOT/deployment.env" "$rollback_env" &&
cp -a "$staged" "$src" &&
BT_SOURCE_SHA="$new_sha" BT_ALLOW_SOURCE_UPDATE=1 "$src/Server/scripts/provision-unraid-secrets.sh" &&
grep -qx "BT_SOURCE_SHA=$new_sha" "$BT_DEPLOY_ROOT/deployment.env" &&
grep -qx "BT_IMAGE_NAME=baby-tracker-server:$new_sha" "$BT_DEPLOY_ROOT/deployment.env" &&
diff <(grep -v '^BT_SOURCE_SHA=\|^BT_IMAGE_NAME=' "$rollback_env" | sort) \
     <(grep -v '^BT_SOURCE_SHA=\|^BT_IMAGE_NAME=' "$BT_DEPLOY_ROOT/deployment.env" | sort) &&
compose config -q && echo 'CONFIG: OK'
```

- [ ] **Build, then the image proof.** It must hold before the app stops: the new image carries the new sha, the retained 2961ca1 image still exists with its own label, and the running app still uses it.

```sh
compose build app migrate &&
images_ok && docker image inspect --format '{{.Id}} {{.RepoTags}}' "baby-tracker-server:$new_sha" "baby-tracker-server:$old_sha"
```

  Image proof: new `<image id>` with label `<sha>`; retained `<image id>` with label `2961ca1…`.
- [ ] Re-check S-1: The second caregiver's phone shows "Up to date" and no timers are running.
- [ ] **Cutover, step 1: stop the app, take the cutover dump, run the preflight.** Earlier outputs are removed first, so a stale PASS can never open a later gate.

```sh
rm -f "$pre" "$post"
images_ok &&
compose stop app &&
docker exec baby-tracker-sync-db-1 pg_dump -Fc -U postgres -d babytracker > "$cutover" &&
sha256sum "$cutover" &&
docker exec -i baby-tracker-sync-db-1 pg_restore --list < "$cutover" | wc -l &&
db_psql < "$src/Server/scripts/format-cleanup-preflight.sql" > "$pre" 2>&1; cat "$pre"
```

- [ ] **Step 2: migrate, then the production postflight.** These run only on a cutover `PREFLIGHT: PASS`. This is spec S-6's production postflight, moved before `up` so the before/after comparisons are exact.

```sh
gate 'PREFLIGHT: PASS' "$pre" &&
compose run --rm --no-deps migrate &&
db_psql -v before="$(grep '^FORMAT_CLEANUP_BEFORE ' "$pre")" \
  < "$src/Server/scripts/format-cleanup-postflight.sql" > "$post" 2>&1; cat "$post"
```

- [ ] **Step 3: start the app, only on `POSTFLIGHT: PASS`.**

```sh
gate 'POSTFLIGHT: PASS (every invariant holds)' "$post" && compose up -d --wait app
```

- *On a `STOP`:*
  - *At step 1, before `migrate`: nothing was migrated. Do S-7 steps 2, 4 and 5; skip the database restore.*
  - *When `migrate` itself failed: 27 runs as one transaction (the runner wraps each file and its `schema_migrations` row in one `BEGIN … COMMIT`), so a failure leaves schema 26 untouched and needs no database restore. Confirm it first:*

    ```sh
    [ "$(db_psql -At -c 'SELECT current_version FROM schema_compatibility')" = 26 ] &&
    [ "$(db_psql -At -c 'SELECT count(*) FROM schema_migrations WHERE version = 27')" = 0 ] &&
    echo 'SCHEMA 26 UNTOUCHED'
    ```

    *On `SCHEMA 26 UNTOUCHED`, do S-7 steps 2, 4 and 5 and skip step 3. If it does not print, do all of S-7.*
  - *At step 3, a postflight `STOP` after `migrate` succeeded: the app is still stopped. Do all of S-7.*

| Dump | When | SHA-256 | `pg_restore --list` entries |
|---|---|---|---|
| `backup.cutover-<short-sha>.dump` | After stopping only the app | `<…>` | `<…>` |

- Cutover preflight: `<PASS>`; the numbers match the dry-run's apart from rows written since: `<…>`.
- `migrate`: exit `<0>`; schema 26 → 27.
- Production postflight: `POSTFLIGHT: <PASS>` (`<41>`/`<41>`).
- Record totals at cutover:

  | Families | Memberships | Feeds | Sleeps | Diapers | Meds | Timer rows | Journal before → after |
  |---|---|---|---|---|---|---|---|
  | `<…>` | `<…>` | `<…>` | `<…>` | `<…>` | `<…>` | `<…>` | `<…>` → `<…>` |

- [ ] Verify:
  - `curl -fsS https://baby-sync.example-tailnet.ts.net/v1/server | jq '{source_version, instance_id, recovery_epoch, capabilities}'` shows source `$new_sha`, the unchanged instance id and recovery epoch 1;
  - the capabilities no longer include `timed-bottle-v1`, `growth-v1` or `feeding-session-v1`;
  - public `/healthz` and `/readyz` return 200;
  - `"$src/Server/scripts/verify-unraid-deployment.sh" verify` passes;
  - the bounded log check over the first 10 minutes reads only level counts and HTTP status counts: `<…>`.

### S-6 Post-deploy

- [ ] The second caregiver opens the app and her next sync pulls the appended heads. Joined sessions should appear in History; spot-check 2–3 against the dry-run listing: `<…>`.
- [ ] She logs normally. Optionally, log and delete one test entry to exercise the v2 feed delete.
- [ ] Status counts show no 400 responses from her device: `<…>`.
- For a later comparison with a phone, run `db_psql -v listing_only=1 < "$src/Server/scripts/format-cleanup-postflight.sql"`. It prints the listing only; the invariants apply only to the quiet database right after `migrate`.

### S-7 Rollback

**Only before the second caregiver has written anything. After she has written entries, fix forward only.**

The database restore is the fresh-database path the dry-run takes (init-users, then `pg_restore`). Task 7 rehearsed it locally on an initialised cluster with the seeded dump. It exited 0, restored schema 26 with every row, and the migrator kept ownership of the database and all its tables. The app comes back on the retained 2961ca1 image; nothing is rebuilt.

After a restore, the second caregiver's phone may already have pulled the heads 27 appended. Its cursor is then above the restored high water, so the server answers `snapshot_required` (`sync/paging.go`) and 2961ca1 takes a snapshot (`SyncRetryPolicy.swift`). The joined sessions disappear from her phone with no error. This is expected and safe: she has written nothing, and the snapshot is the restored state.

```sh
# 1. Stop the app.
compose stop app
# 2. Source and config back to 2961ca1. The B source moves aside; the rollback
#    copies are copied, not moved, so they stay intact. Run the next block only
#    after this one prints CONFIG RESTORED.
[ -d "$rollback_src" ] && [ -f "$rollback_env" ] && [ ! -e "$BT_DEPLOY_ROOT/source.$short-failed" ] &&
mv "$src" "$BT_DEPLOY_ROOT/source.$short-failed" && cp -a "$rollback_src" "$src" &&
cp -p "$rollback_env" "$BT_DEPLOY_ROOT/deployment.env" &&
grep -qx "BT_SOURCE_SHA=$old_sha" "$BT_DEPLOY_ROOT/deployment.env" &&
grep -qx "BT_IMAGE_NAME=baby-tracker-server:$old_sha" "$BT_DEPLOY_ROOT/deployment.env" &&
[ "$(revision "baby-tracker-server:$old_sha")" = "$old_sha" ] && echo 'CONFIG RESTORED'
```

```sh
# 3. Database (only if migrate ran): the cutover dump into a fresh database.
#    pg_restore's messages go to a 0600 file, because a failure can quote row data.
[ -s "$cutover" ] &&
docker exec baby-tracker-sync-db-1 psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres \
  -c 'DROP DATABASE babytracker WITH (FORCE)' -c 'CREATE DATABASE babytracker' &&
compose exec -T -u postgres db /docker-entrypoint-initdb.d/20-init-users.sh &&
docker exec -i baby-tracker-sync-db-1 pg_restore -U postgres -d babytracker --single-transaction --exit-on-error \
  < "$cutover" > "$BT_DEPLOY_ROOT/rollback-restore.log" 2>&1 &&
[ "$(db_psql -At -c 'SELECT current_version FROM schema_compatibility')" = 26 ] && echo 'DATABASE RESTORED (schema 26)'
```

```sh
# 4. Start the app on the retained image (no build).
grep -qx "BT_IMAGE_NAME=baby-tracker-server:$old_sha" "$BT_DEPLOY_ROOT/deployment.env" &&
[ "$(db_psql -At -c 'SELECT current_version FROM schema_compatibility')" = 26 ] &&
compose up -d --no-build --wait app
# 5. Post-rollback check: the verifier, the source and the schema.
"$src/Server/scripts/verify-unraid-deployment.sh" verify &&
[ "$(curl -fsS https://baby-sync.example-tailnet.ts.net/v1/server | jq -r .source_version)" = "$old_sha" ] &&
[ "$(db_psql -At -c 'SELECT current_version FROM schema_compatibility')" = 26 ] &&
[ "$(docker inspect --format '{{.Config.Image}}' baby-tracker-sync-app-1)" = "baby-tracker-server:$old_sha" ] &&
echo 'ROLLBACK: OK (2961ca1, schema 26)'
```

- Rollback result: `<ROLLBACK: OK …>`; verifier `<pass>`; `/v1/server` source `<2961ca1…>`; schema `<26>`.

## Phase C — installs, days later

### C-1 Build

- [ ] Merge the latest `main` into the branch, then from a **clean tree** at the merged `<sha>` run every gate: `make test`, `make build`, `make test-ui`, `make server-protocol-e2e`. Record exit codes: `<…>`.
- [ ] Fast-forward `main` to that `<sha>` and build the signed Debug app from the same clean tree. Record the `codesign --verify --deep --strict` result and the executable SHA-256: `<…>`.

### C-2 Before uninstalling, on each phone

- [ ] The server is healthy: `/readyz` 200 and the verifier passes. Both phones are on Tailscale and reach `baby-sync`.
- [ ] No timer is running on either phone. Stop any running feed or sleep first, then let it sync.
- [ ] The phone shows "Up to date".
- [ ] Its last 3 days match the server listing (`db_psql -v listing_only=1 < "$src/Server/scripts/format-cleanup-postflight.sql"`): `<…>`.
- [ ] Archive the App Group container, mandatory for the owner phone:

```sh
xcrun devicectl device copy from --device <device-id> \
  --domain-type appGroupDataContainer --domain-identifier group.com.olhapi.babytracker \
  --source / --destination ~/BabyTrackerArchive/<phone>-<yyyymmdd>/
```

  *If the App Group copy is not supported (risk 11), rely on "Up to date" plus the 3-day comparison, and note it here.*

### C-3 Owner phone first, proven, then second caregiver

**The second caregiver's phone stays on its current build, as the family's working logger, until the owner phone passes every check below.**

1. [ ] **Phone A: uninstall, install, sign in.**
   - Sign in on the first screen before adding anything. An entry logged before sign-in stays on the phone and is never uploaded.
   - The reinstall is a new device, so it takes a snapshot (Task 11 rehearsed this against a schema-27 server).
   - *Uninstall first; never install over the old build. Installing over it keeps the defaults and runs the owner import path instead. Task 11 covers that path, but the plan is a clean install.*
2. [ ] **Counts.** Per-day counts on the phone match the server listing: `<…>`.
3. [ ] **Round trip.**
   - Log a test diaper, see it on the server: `<…>`.
   - Delete it, and see the tombstone on the server.
   - The second caregiver's phone (still on the current build) sees both changes.
4. [ ] **Relaunch check (B-R14 on a real device).**
   - Force-quit the app and relaunch it.
   - The data is still there and there is no sign-in prompt.
   - `xcrun devicectl device info files --device <id> --domain-type appGroupDataContainer --domain-identifier group.com.olhapi.babytracker` shows only `default.store`, `default.store-wal` and `default.store-shm` under `Library/Application Support`, with **no `default.store.incompatible-*`**: `<…>`.
5. [ ] **Phone B: uninstall, install, sign in.** Only after steps 1–4 pass. She joins and takes a snapshot. Repeat steps 2 and 4 for her phone: `<…>`.

**If sign-in or the first sync fails:**
- Do not log anything on the new install, and do not reinstall again.
- Check Tailscale on the phone, `/readyz`, and the server's status counts (level and status only).
- The server allows 2 snapshots per account per 15 minutes (`snapshot_limit`, HTTP 409). After a failed attempt, wait 15 minutes before retrying.
- The second caregiver's phone keeps logging meanwhile. Fix forward. The server data is untouched by a phone install.

### C-4 Physical checks

- [ ] Live Activity on each phone: start Left, lock, stop it and start Right from the lock screen.
- [ ] Joined sessions in History match the dry-run listing (spot-check 2–3, e.g. 09-26 10:23 and 09-28 02:15).
- [ ] Widgets show the current state; one Siri/App Shortcut works.
- [ ] Settings → Export produces a backup.
- [ ] Server logs, level and status counts only, show no 4xx from either phone: `<…>`.

## Still open

- `<…>`
