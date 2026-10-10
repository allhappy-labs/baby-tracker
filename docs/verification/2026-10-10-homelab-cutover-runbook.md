> **Record of what ran** at the homelab cutover on 2026-10-10 ([`2026-10-10-homelab-cutover.md`](2026-10-10-homelab-cutover.md)); moved here from `docs/operations/legacy-import.md` by Phase 4 Task 29, the commit that deleted the importer and every tool this runbook names.

# Homelab cutover: the legacy import runbook

**Status: not executed.** Written with the tooling at branch `feat/postgres-authority` (Phase 4, Task 21, fix round 1). Nothing in it has been run against the homelab. R5 to R9 were rehearsed on a disposable synthetic stack on the final tree (Task 28; see "Rehearsed").

This runbook moves the homelab (Unraid, project `baby-tracker-sync`) from the legacy server to this one, once:

- **From:** `fce422d5` at schema 29, with the file security ledger, Live Activity push through `main`'s `live-activity-push.yaml`, and its worker login.
- **To:** the merged branch, through `baby-tracker-admin import-legacy` (one transaction: verify the ledger, cross-check the projection, load the new schema, verify, drop the legacy tables).
- **Then:** the legacy roles dropped (R6) and the ledger directories deleted (R7).

The tooling:

- `Server/scripts/legacy-cutover.sh <subcommand>` runs every homelab step.
- `Server/scripts/rehearse-legacy-import.sh` runs the same steps on a disposable local stack (R3).
- `Server/deploy/unraid/push.yaml` replaces `main`'s `live-activity-push.yaml`: Live Activity on, background wakes per C5, the one APNs key, no worker login.

## Approvals

**Every step that touches the homelab starts only after the owner says yes to that step in the conversation.** Nothing runs from this document, from a plan or from a test. Approving this document approves no step.

**The gate.** `legacy-cutover.sh` identifies its target before it acts, from four sources:

- the instance in the project's app container;
- the database's `server_state`;
- `deployment.env`;
- the ledger's `identity.json`.

Every readable source must agree, and at least one of the two container sources must be readable. If any source names the homelab instance `<instance-uuid>`, the subcommand runs only when `BT_CUTOVER_APPROVED` equals that instance. The coordinator types the variable by hand, on the one command the owner just approved, and never exports it. Without it, every subcommand exits 2 with `refusing the homelab instance`. For the project `baby-tracker-sync`, `BT_DEPLOY_ROOT` and `BT_ENV_FILE` cannot be redirected.

| Step | What it touches | Approval |
| --- | --- | --- |
| R0 | Nothing: the questions | — |
| R1 | The homelab, read-only | Yes, for R1 |
| R2 | The homelab's source tree, `deployment.env`, the image; Apple (one zero-token push per type) | Yes, for R2 |
| R3 | A short stop of the legacy app for a fresh backup; the rehearsal where D4 says | Yes, for R3 |
| R4 | The phones (the owner), then the legacy app | Yes, for R4 |
| R5 | The homelab database: backup, dry run, import, verify | Yes, for R5 |
| R-abort | Back to the legacy app after a STOP from R2 to R5.3, before any commit; the database is not touched | Yes, for R-abort |
| R8 | The phones (the owner), right after R5.4 | — |
| R6 | The cluster's legacy roles (superuser) | **Its own yes** |
| R7 | The ledger directories, after R8 | **Its own yes** |
| R9 | Rollback: after a commit and before R8 on a STOP from R5.4; after R8 only on the owner's decision (never for an R6 or R7 STOP) | Yes, for R9; after R8, the owner's explicit decision |
| R10 | The record | — |

**Where a STOP goes:**

- **R2 up to R5.3, before the import committed:** to R-abort. The legacy database is untouched; a failed run or a dry run leaves it as it was.
- **After the import committed and before R8** (`import` printed `result=imported`, or a `--verify-only` says `start=imported`; no `phones-back.txt` and no `device_sync_cursors` rows, which rollback checks itself): to R9. No phone has written, so the phones need nothing.
- **R6 or R7:** they stay put. These steps are cleanup, and a STOP there changes nothing: R6's drop is one transaction (its post-drop readiness check, step 6, is the one STOP after a change: see R6), and R7 deletes only after every check. The new server keeps running. Report, fix the cause, and retry that step. **Never R9.**
- **After R8** (`phones-back.txt` exists, or `device_sync_cursors` has rows; rollback decides this from the database): R9 only on the owner's explicit decision. Writes made since R8 are lost; R9 below says how.
- **When in doubt:** run `verify`, whose `--verify-only` tells which.

**Reading the output:**

- Every subcommand prints `key=value` lines, then one summary line.
- A STOP is one `STOP: <subcommand>: <reason>` line on stderr and exit 1. Do not continue past a STOP; report it to the owner.
- Exit 2 is a refusal to start (usage, the homelab gate).
- Exit 3 is preflight's compose-only mismatch (R2.2).
- For the importer, `legacy-cutover.sh` prints `import-legacy`'s own output. On a failure, **read its last stderr line**, `import-legacy: <code>: <detail>`.

**Do not:**

- **Rerun the new tree's `init-users.sh` against the legacy schema-29 database**, before or after any step. It knows only the runtime and maintenance groups (Task 27) and compares them against the current privilege manifest, so on the legacy database it fails with `effective privileges do not match schema privilege manifest`. The legacy database is only ever rebuilt by R9, which uses `fce422d5`'s own statements. Init scripts never rerun on an existing volume by themselves.

## Duration targets

- **Window:** about 15 minutes, with the image built beforehand (R2):
  - backup about 1 minute (26 MB database);
  - dry run plus import about 30 seconds;
  - verify and restart about 5 minutes.
- **The import itself:** under 10 seconds on the homelab-shaped fixture (P: 1 family, 2 accounts, 4 devices, 1,500 journal rows, 2 Live Activity registrations with cards, about 5,000 ledger events), verification included.
- **Measured** (Task 22, `Report.Phases` of `TestImportScale`, recorded in `docs/verification/2026-10-09-legacy-import-scale.md`; a shared Apple M4 Mac under OrbStack, not the homelab):
  - **P** (1 family, 2 accounts, 4 devices, 1,443 journal rows, 1,109 care entities, 4 Live Activity registrations with 3 cards, a 5,007-event ledger): dry run 1.99 s, import 2.77 s on a quiet machine, verification included. Under a load average of 7.5–15 from other sessions: 4.1–5.3 s, with one import at 7.2 s. Target ≤ 10 s.
  - **L10k** (10,000 families, 2,010,000 care entities, a 550,000-event ledger, 670,000 audit rows): dry run 11m20s, import 8m22s (run 5, load average mean 7.3). Target ≤ 15 minutes. Every rate floor held: ledger walk 5,552–6,324 events/s, re-walk 4,851–5,790 events/s (≥ 4,000), audit COPY 51,619–75,042 rows/s (≥ 10,000), care 5,880–9,737 entities/s (≥ 5,000).
  - The homelab (26 MB, 1,421 journal rows) is about P's size. The reference node was not measured (a Phase 5 carry).

## Rehearsed

**Synthetic, on the final tree** (Phase 4 Task 28, 2026-10-10): `Server/scripts/rehearse-legacy-import.sh --synthetic --legacy-sha fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6`, on a Mac with OrbStack, new image built from `d292b39e` (the frozen `000032_baseline`). Last line:

```
rehearsal proof: imported=25 verified=1 pulled=3 icons=1 card_updated=1 roles_dropped=6 post_r6_ready=1 ledger_deleted=1 rolled_back=1 legacy_ready=1 post_r8_detected=db new_db_dumped=1 sign_in_required=1 cleanup=0/0
```

What it ran, on a disposable `fce422d5` stack seeded by `TestCaptureLegacyFixture` (its own instance, throwaway keys, the identitytest APNs recorder in place of Apple), with `legacy-cutover.sh` itself:

- R5: `backup` (schema 29, 694 dump entries), `dry-run` and `import` (`start=homelab_29`, 25 care entities, 51 audit rows; the import 0.7 s), `verify` (`verify_only=passed authority_failed=0 readyz=200`, `live-activity-push-v1` and `push-token-v1` advertised);
- R8: `phones-back`, then the three captured phones (owner, caregiver, viewer: Go clients holding their captured legacy tokens, not the iOS app) over HTTPS through the new app:
  - each pulled from zero, with no `snapshot_required`, exactly the entities the legacy server had served it, at their latest versions and up to the same high water, and nothing from its captured cursor (`pulled=3`);
  - the icon baby came back with its icon (`icons=1`);
  - a rename of the card's baby from the caregiver's phone reached the captured card's carried update token as an update under its carried generation, with no push-to-start (`card_updated=1`);
  - each then refreshed through `refresh-recoverable` with its captured refresh token;
- R6: all six legacy roles dropped in one transaction, the app restarted, ready, Live Activity worker passing and no maintenance failure for 25 s (`post_r6_ready=1`);
- R7: `delete-ledger` after its `--verify-only`;
- R9 after R8: with `phones-back.txt` removed, the rollback found the phones from `device_sync_cursors` and refused without `--owner-accepts-loss-of-phone-writes`; with it, it dumped the new database first, restored the legacy app at schema 29 with the backup's ledger head, and ended on the sign-out/sign-in line.

What it did not show: R1 and R2 against the homelab (preflight, staging, `apns-preflight` with the real key, `verify-unraid-deployment.sh`, which reported `unraid_verify=skipped`), R3 on real data (`--dump`, where D4 says), the actor-key binding against the homelab's own `actor-id-key.json`, file ownership (see R3), and the real phones and Apple.

## Shell setup (on the Unraid host, once per session)

Pasted after the owner approves the first host step. Every later block relies on it.

```sh
export BT_DEPLOY_ROOT=/mnt/user/appdata/baby-tracker-sync
umask 077
set -o pipefail
# deployment.env is read through --env-file only, never exported (main's runbooks);
# legacy-cutover.sh also removes every exported BT_* variable from its compose calls.
unset BT_SOURCE_SHA BT_IMAGE_NAME BT_ENV_FILE
merged_sha=<40-character approved merged SHA>; short=${merged_sha:0:7}
src=$BT_DEPLOY_ROOT/source
staged=$BT_DEPLOY_ROOT/source.$short-staged
rollback_src=$BT_DEPLOY_ROOT/source.fce422d-rollback-$short
rollback_env=$BT_DEPLOY_ROOT/deployment.env.fce422d-rollback-$short
export BT_BACKUP_DIR=$BT_DEPLOY_ROOT/backup.cutover-$short
homelab=<instance-uuid>
cutover=$src/Server/scripts/legacy-cutover.sh
# The new composition (push.yaml) and the legacy one (main's overlay), as legacy-cutover.sh runs them.
compose_new() {
  (cd "$src/Server" && docker compose -p baby-tracker-sync --env-file "$BT_DEPLOY_ROOT/deployment.env" \
    -f compose.yaml -f deploy/unraid/compose.yaml -f deploy/unraid/push.yaml "$@")
}
compose_legacy() {
  (cd "$rollback_src/Server" && docker compose -p baby-tracker-sync --env-file "$rollback_env" \
    -f compose.yaml -f deploy/unraid/compose.yaml -f deploy/unraid/live-activity-push.yaml "$@")
}
revision() { docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$1"; }
```

`legacy-cutover.sh`'s defaults are these paths:

- `BT_COMPOSE_FILES` is `-f compose.yaml -f deploy/unraid/compose.yaml -f deploy/unraid/push.yaml`, under `$src/Server`.
- `BT_LEGACY_COMPOSE_FILES` is the same with `deploy/unraid/live-activity-push.yaml`, under the one `source.fce422d-rollback-*`.
- The project is `baby-tracker-sync`.

`$BT_BACKUP_DIR` (0700) holds:

- the backup: `legacy.dump`, `ledger.tar`, `receipt.json`, each 0600;
- `preflight.txt`, `verify.txt`;
- the R6 records `roles-dropped.txt` and `retired-secrets/`, which has a `manifest.tsv` of owners and modes;
- `out/`, the importer's reports (`dry-run-report.json`, `import-report.json`, `verify-report.json`) and its temporary files. The importer gets the backup read-only and only `out/` writable.

## R0 Approvals and choices

Nothing runs. One approval per homelab step from here on. The coordinator asks the owner:

- **D4:** where the real-data rehearsal (R3) runs. The script needs a checkout of this repository with Go, git and local Docker, which the Unraid host does not have today. So either D4 provides that on the host, and no file leaves it, or the two backup files (never a secret) go to the D4 machine for the rehearsal and are deleted afterwards.
- **D5:** the cutover window.
- **C4:** in R6, also drop `main`'s Live Activity worker role and login (`baby_tracker_live_activity`, `baby_tracker_live_activity_app`) and its two secrets (`live_activity_password`, `live_activity_database_url`)? Default: yes.
- **C5:** background wake pushes on at the cutover (`BT_APNS_WAKES_ENABLED=true`), or later? Default: on. They do nothing until the phones run a merged app build that registers a push token.
- **C6:** when to install a merged app build on the phones. It must come from Task 23's merge or later (V10). Default: after the cutover; the wire is unchanged, so the current app works with the new server.
- **The D2 re-pin rule:** hold every other homelab deploy until R10. If the homelab moves, the plan is re-pinned before any step here runs.

The merged SHA must include Task 27's baseline (it grants the projector and Apple-revoker roles nothing), so that R6 can drop them. It must also include main's `98029498` (Serve proxies to the container name), which R2.3 checks.

## R1 Preflight (read-only)

From the Mac, after the owner's yes for R1: Task 1's `homelab-preflight.sh`. It reads over SSH, read-only, and prints no secret.

```sh
BT_PREFLIGHT_APPROVED=yes Server/scripts/homelab-preflight.sh | tee /tmp/r1-preflight.txt; echo "exit=${PIPESTATUS[0]}"
```

Expected: `exit=0`, and the last line is

```
preflight: schema=29 digests=match head=<g>:<r> projection=equal mode=self_hosted source=fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6 source_match=match image_revision=fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6 image_revision_match=match apns=set app=running compose=source/Server/compose.yaml,source/Server/deploy/unraid/compose.yaml,source/Server/deploy/unraid/live-activity-push.yaml compose_match=match identity_match=match household=equal
```

The expected schema digests come from the vendored manifest (`legacytest/testdata/legacy-chain/manifest.json`), never a literal.

**STOP unless:**

- the summary has every field above (`household=grew` is informational);
- `identities_material_missing=0`, `identities_key_mismatch=0` and `devices_foreign_instance=0` (the importer would refuse);
- `ledger_actor_id_key=present`: `ledger-head/actor-id-key.json` exists, and the importer checks the actor key against it.

Any other `source` or schema is a STOP and the D2 re-pin rule: no cutover step runs against a state the plan does not pin.

**Compose.** `compose_match=mismatch` with every other gate passing is inspected by eye, until the live label has been seen once. Continue only if `compose=` names exactly `source/Server/compose.yaml`, `source/Server/deploy/unraid/compose.yaml` and `source/Server/deploy/unraid/live-activity-push.yaml`; record what it printed.

**Record:**

- `babies_live` and `babies_with_icon` (expected 2 and 1);
- the informational `prekap_lineages`, `recovery_active_rotated_count`, `recovery_receipt_present_count` and `recovery_ledger_refresh_attempt_count` (whether the pre-cap and recovery-child import paths are live here);
- `database_size_bytes` and `mount_available_kb`.

## R2 Stage the source and the overlay

After the owner's yes for R2. The legacy app keeps running. A STOP anywhere in R2 goes to R-abort; if `source/` was not swapped yet, nothing needs restoring.

1. **Source.** On the Mac, from the clean worktree at `$merged_sha`, as `main:docs/verification/2026-10-09-baby-icons-rollout.md` did:

   ```sh
   git archive --format=tar "$merged_sha" Server | gzip -n > "/tmp/bt-$short.tar.gz" && shasum -a 256 "/tmp/bt-$short.tar.gz"
   ```

   Upload it. On the host, check the SHA-256 again, then unpack it into `$staged`. **STOP** if the digests differ.

2. **The preflight on the host, from the staged tree, before anything changes.** It runs R1's checks locally (no SSH), while `deployment.env` still names `fce422d5`. It saves `$BT_BACKUP_DIR/preflight.txt` (0600), but only once every check has passed, and records the verdict that `verify` requires.

   ```sh
   BT_CUTOVER_APPROVED=$homelab "$staged/Server/scripts/legacy-cutover.sh" preflight; echo "exit=$?"
   ```

   - Expected: `preflight_verdict=pass schema=29 source=fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6`, then `exit=0`.
   - `exit=3` (`preflight_verdict=inspect_compose`) is R1's compose-only case. Inspect `compose=` the same way, then rerun with `BT_PREFLIGHT_ACCEPT_COMPOSE=1` to record `preflight_verdict=pass_compose_acknowledged`.
   - **STOP** on exit 1, or if `babies_with_icon` differs from R1's.

3. **Serve.** Unraid's rc.docker drops the `app` alias at boot, so Serve must proxy to the container name (main's `98029498`). A reboot between R2 and R5 would otherwise start tailscale on the staged tree's `serve.json`.

   ```sh
   grep -q '"Proxy": "http://baby-tracker-sync-app-1:8080"' "$staged/Server/deploy/unraid/tailscale/serve.json" && echo 'SERVE: OK'
   ```

   **STOP** otherwise. The branch carries the fix since Task 23's merge of `main`; `TestUnraidServeConfigTerminatesHTTPSOnlyForBabySync` refuses the `app` alias.

4. **The rollback set, then the new source and config.** R-abort and R9 restore `source.fce422d-rollback-$short` and `deployment.env.fce422d-rollback-$short`. The older `source.998501b-rollback-fce422d` set stays where it is.

   ```sh
   [ ! -e "$rollback_src" ] && [ ! -e "$rollback_env" ] &&
   mv "$src" "$rollback_src" && cp -p "$BT_DEPLOY_ROOT/deployment.env" "$rollback_env" &&
   cp -a "$staged" "$src" &&
   BT_SOURCE_SHA="$merged_sha" BT_ALLOW_SOURCE_UPDATE=1 "$src/Server/scripts/provision-unraid-secrets.sh" &&
   grep -qx "BT_SOURCE_SHA=$merged_sha" "$BT_DEPLOY_ROOT/deployment.env" && echo 'STAGED'
   ```

   `provision-unraid-secrets.sh` adds the maintenance credential (`secrets/maintenance_database_url`) that the new composition mounts. Expected: `STAGED`.

5. **`deployment.env`.**
   - Set `BT_APNS_WAKES_ENABLED` per C5.
   - Remove any `BT_LIVE_ACTIVITY_DATABASE_URL` or `BT_LIVE_ACTIVITY_DATABASE_URL_FILE` line: the new server refuses either at startup (the rollback copy keeps it).
   - `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`, `BT_APNS_BUNDLE_ID` and `secrets/apns_key.p8` stay as they are.

   ```sh
   sed -i '/^BT_LIVE_ACTIVITY_DATABASE_URL\(_FILE\)\?=/d; /^BT_APNS_WAKES_ENABLED=/d' "$BT_DEPLOY_ROOT/deployment.env" &&
   echo 'BT_APNS_WAKES_ENABLED=<true per C5, else false>' >> "$BT_DEPLOY_ROOT/deployment.env" &&
   ! grep -q '^BT_LIVE_ACTIVITY_DATABASE_URL' "$BT_DEPLOY_ROOT/deployment.env" &&
   [ "$(grep -c '^BT_APNS_\(TEAM_ID\|KEY_ID\|BUNDLE_ID\)=' "$BT_DEPLOY_ROOT/deployment.env")" = 3 ] &&
   compose_new config -q && echo 'CONFIG: OK'
   ```

   Expected: `CONFIG: OK`. **STOP** otherwise.

6. **Image.** The app keeps running the old image.

   ```sh
   compose_new build app migrate && [ "$(revision "baby-tracker-server:$merged_sha")" = "$merged_sha" ] && echo 'IMAGE: OK'
   ```

7. **APNs, once per push type.** The homelab key must also cover the Live Activity topic. `apns-preflight` sends one push to an all-zero device token and prints no secret:

   ```sh
   for type in background liveactivity; do
     compose_new run -T --rm --no-deps app apns-preflight --environment sandbox --push-type "$type"
   done
   ```

   Expected, one line each:

   ```
   apns_preflight environment=sandbox push_type=background outcome=pass status=400 reason=BadDeviceToken
   apns_preflight environment=sandbox push_type=liveactivity outcome=pass status=400 reason=BadDeviceToken
   ```

   Run both again with `--environment production` if the phones run TestFlight builds. **STOP** unless every line reads `outcome=pass`.

## R3 Rehearse

After the owner's yes for R3. The rehearsal uses a fresh backup taken by this tool on the host, and nothing else.

1. **The fresh backup.** The `backup` subcommand refuses a running app, so this needs a short stop: ask R4's question, stop the legacy app, take the backup into its own directory, then start the legacy app again.

   ```sh
   compose_legacy stop app &&
   BT_BACKUP_DIR=$BT_DEPLOY_ROOT/backup.rehearsal-$short BT_CUTOVER_APPROVED=$homelab "$cutover" backup;
   compose_legacy start app
   ```

   The backup stays on the host, under the deploy root, mode 0700/0600.

2. **The rehearsal, using only `legacy.dump` and `ledger.tar`.** It never uses a secret:
   - it never reads the secrets directory or the APNs key;
   - the instance and epoch come from the tar's `identity.json`;
   - the disposable stack keeps its own throwaway keys;
   - its private ledger copy binds the stack's own actor-ID key in `ledger-head/actor-id-key.json`, in place of the homelab's (`actor_key=rehearsal_binding`): the importer checks that binding, and the rolled-back `fce422d5` server's recovery gate refuses a ledger without a matching one (its `/readyz` stays 503 and its Live Activity worker never resumes);
   - its database gets `fce422d5`'s database-level grants (CONNECT revoked from PUBLIC, granted to each group), as `init-users.sh` leaves the homelab's.

   Where D4 says: a checkout of this repository at the merged SHA, with Go, git, local Docker and ≥ 10 GB free.
   - **On the host,** if D4 provides that there. Then no file leaves the host.
   - **Elsewhere:** only those two files may leave, over the tailnet, and they are deleted afterwards (they hold every token digest). Never the secrets directory, never `deployment.env`, never the APNs key; copy the two files by name, never the directory.

   ```sh
   BT_COMPOSE_PROJECT=rehearsal-homelab BT_CUTOVER_APPROVED=$homelab \
     Server/scripts/rehearse-legacy-import.sh --dump "$BT_DEPLOY_ROOT/backup.rehearsal-$short/legacy.dump" \
     --ledger-tar "$BT_DEPLOY_ROOT/backup.rehearsal-$short/ledger.tar"
   ```

   It refuses any project not named `rehearsal-*`. It runs backup, dry run, import, verify, `drop-legacy-roles --with-live-activity`, `delete-ledger` and `rollback` on the disposable stack. The phones' proofs need a capture and are skipped.

   Expected last line:

   ```
   rehearsal proof: imported=<n> verified=1 pulled=skipped icons=<R1's babies_with_icon> card_updated=skipped roles_dropped=6 post_r6_ready=1 ledger_deleted=1 rolled_back=1 legacy_ready=1 legacy_live_activity=1 legacy_db_acl=revoked_from_public actor_key=rehearsal_binding post_r8_detected=marker new_db_dumped=1 sign_in_required=1 cleanup=0/0
   ```

   The rollback is rehearsed after R8: the stack's phones are marked back, the rollback is refused without `--owner-accepts-loss-of-phone-writes`, then runs with it, dumps the new database first (`new_db_dumped=1`) and ends on the sign-out/sign-in requirement (`sign_in_required=1`). No phone pulls in this mode, so `device_sync_cursors` stays empty and the refusal comes from the `phones-back.txt` marker (`post_r8_detected=marker`); `--synthetic`, whose phones pull, removes the marker and proves the detection from the database (`post_r8_detected=db`). The rolled-back legacy server must be ready with its Live Activity worker resumed (`legacy_ready=1 legacy_live_activity=1`), on a database carrying `fce422d5`'s database-level grants (`legacy_db_acl=revoked_from_public`).

   **STOP** on any other result. `roles_dropped=6` is R6 dropping all six legacy roles (C2's four and C4's pair) in one transaction; any other count, or an R6 STOP on a dependency, means the staged tree is not Task 27's baseline or later.

`--synthetic` is the same procedure on a seeded disposable `fce422d5` stack. It is the evidence that the tooling works, not a substitute for R3. Its phones pull over HTTPS through the new app (`pulled=3`), and a rename of the captured card's baby reaches the card's carried update token (`card_updated=1`).

**What no rehearsal shows: file ownership.** Docker Desktop's file sharing ignores owners, so the synthetic rehearsal on a Mac cannot show that:

- the backup directory is handed to the importer's user;
- each retired secret comes back with its owner and mode (65532 0600, 70 0600, root 0444).

`TestLegacyCutoverRestoresOwnersOnLinux` covers that, as root, in a disposable Linux container; the command is in its comment.

## R4 Stop the phones, then the server (D3)

There is no in-flight request handling; the owner quiesces the phones. The coordinator asks:

> "Ready to migrate? Please open the app on both phones, check each shows no pending changes (the sync indicator is idle), then force-quit both apps, and tell me when done."

Only after the owner confirms, stop the legacy app from the legacy tree:

```sh
compose_legacy stop app && [ -z "$(docker ps -q --filter label=com.docker.compose.project=baby-tracker-sync --filter label=com.docker.compose.service=app)" ] && echo 'APP: STOPPED'
```

Expected: `APP: STOPPED`. The app stays stopped from here until R5.4 starts the new one, or R-abort restarts the legacy one.

## R5 Cutover

After the owner's yes for R5. Each command is run only if the previous one printed its expected line. A STOP in R5.1 to R5.3, before a commit, goes to R-abort.

**R5.1 Backup.** Taken with the server stopped:

- `pg_dump -Fc` as the cluster superuser, read back by `pg_restore --list`;
- `tar` of `security-ledger` and `ledger-head`;
- `legacy_state_sha256`: `legacyimport.LegacyStateSQL`, byte for byte (pinned by `legacy_cutover_test.go`), fed on stdin to `psql -f -` as `postgres` in a read-only session.

It writes `legacy.dump`, `ledger.tar` and `receipt.json` in one 0700 directory, each file 0600, with the digests and `dump_bytes` over the final files. It hands the directory to the importer's user (65532).

```sh
BT_CUTOVER_APPROVED=$homelab "$cutover" backup
```

Expected: `backup: dir=<BT_BACKUP_DIR> schema=29 dump_entries=<n> dump_bytes=<n> dump_sha256=<…> ledger_tar_sha256=<…> legacy_state_sha256=<…> head=<g>:<r> created_at=<…>`.

**STOP** if:

- the app is running, or Docker cannot say whether it is;
- free space is below 3 × `pg_database_size`;
- the backup directory already holds a backup;
- the app started during the backup.

**R5.2 Dry run.**

- Mounts: the ledger directories and the backup read-only (`/legacy/ledger:ro`, `/legacy/head:ro`, `/legacy/backup:ro`); `out/` writable for `--report` and the temporary files.
- Environment: the app's own instance, epoch (3 on the homelab), mode and family limits, as compose resolves them from `deployment.env`, plus the runtime URL file. The command runs `compose run -T`, so stdout and stderr stay apart.

```sh
BT_CUTOVER_APPROVED=$homelab "$cutover" dry-run
```

Expected:

- `import-legacy`'s line `result=dry_run start=homelab_29 …`;
- then `dry-run: result=dry_run start=homelab_29 report=<BT_BACKUP_DIR>/out/dry-run-report.json`.

On `server_running`: the db container's own health check (`pg_isready` as the superuser) can show up as a second session. The first `server_running` is rerun once, automatically, after 5 seconds (`rerun=1`). A second one is a **STOP**; its line names each session's role and `application_name` for the owner.

On `legacy_tables_busy`: the lock step waits up to 5 s for the legacy tables, and a lock held longer than that is refused with nothing changed. It is also rerun once, automatically (`rerun=1`); a second one is a **STOP**. A routine autovacuum is cancelled by the waiting import after `deadlock_timeout`, but an anti-wraparound autovacuum does not yield: before retrying after that STOP, check `pg_stat_progress_vacuum` (read-only) and wait for the vacuum to finish.

Any other refusal is a **STOP**: read the last stderr line, then R-abort.

**R5.3 Import.**

```sh
BT_CUTOVER_APPROVED=$homelab "$cutover" import
```

Expected: `result=imported start=homelab_29 … runtime_privileges=verified`, then `import: result=imported start=homelab_29 report=<BT_BACKUP_DIR>/out/import-report.json`.

It refuses, before running anything:

- a receipt older than 24 hours, or for another ledger head (take the backup again);
- no passing dry-run report newer than the receipt;
- an existing import report.

On failure:

- `import-legacy: import_commit_outcome_unknown: …` means the COMMIT returned an error. Run R5.4: its `--verify-only` says whether the import committed.
  - If it committed, R5.4 continues, and writes `out/verify-report.json` with the `import_id`. R6 and R7 accept that report when no import report was written.
  - If it did not commit, R5.4 STOPs (`--verify-only did not pass on an imported database`). Rerunning `import` is safe, or go to R-abort.
- `server_running` and `legacy_tables_busy` are rerun once, automatically, as in R5.2. A second one is a **STOP**; nothing was changed.
- Any other failure leaves the legacy database exactly as it was: **STOP**, read the last stderr line, report, and go to R-abort.

**R5.4 Verify, then start the new app.**

```sh
BT_CUTOVER_APPROVED=$homelab "$cutover" verify
```

It refuses to start the app if:

- `preflight.txt` records no passing preflight;
- `deployment.env` sets `BT_LIVE_ACTIVITY_DATABASE_URL[_FILE]`;
- `BT_COMPOSE_FILES` names `live-activity-push.yaml`. `main`'s overlay leaves the composition here; its copy stays in the rollback source for R9.

Then, in order:

1. `import-legacy --verify-only` (read-only), writing `out/verify-report.json` and naming the `import_id`;
2. `baby-tracker-admin authority verify`: `failed=0`;
3. `docker compose … up -d --wait app` with `push.yaml`;
4. `verify-unraid-deployment.sh verify` (see below);
5. `/readyz` 200, and `/v1/server` advertising `live-activity-push-v1` (after the worker's first pass), and `push-token-v1` when wakes are on;
6. `babies_with_icon` from the reports equal to `preflight.txt`'s.

Expected last line:

```
verify: verify_only=passed import_id=<id> authority_failed=0 app=started readyz=200 unraid_verify=passed capabilities=[…] babies_with_icon=1 preflight_babies_with_icon=1
```

It writes `verify.txt`, which R6 and R7 require. Then R8: the phones come back.

**`unraid_verify=warn`.** `verify-unraid-deployment.sh` also diffs every unrelated container against the deploy-time snapshot, and ordinary drift in another app would fail it. So a failure there is a WARN, not a STOP. Read its output:

- **Accept** drift in unrelated containers only, and record it.
- **STOP** on anything else: private HTTPS health or readiness, the live socket's 401, a published host port, the number of project containers.
- **Before R9, rerun `verify` once.** `--verify-only` tolerates the running app.

**STOP** on any other result. A STOP here comes after the commit, so it goes to R9.

## R-abort Back to the legacy app before any commit

For a STOP from R2 to R5.3, before the import committed. It brings back the legacy source, its `deployment.env` and its app, on the untouched legacy database. It refuses when:

- `out/import-report.json` or `verify.txt` exists (that is R9's case);
- the database is not the legacy schema 29 (`mutation_journal` present). Run `verify` first; if the import committed, use R9.

```sh
BT_CUTOVER_APPROVED=$homelab "$cutover" abort-before-commit
```

If R2.4 has not swapped `source/` yet, there is no rollback set. Then nothing needs restoring, and the legacy app is normally still running: before R2.4 only R3.1 stops it, and R3.1 starts it again itself.

If it is stopped, `abort-before-commit` restarts it from the unchanged `source/` and `deployment.env`. It requires that `deployment.env` still names `fce422d5`, and prints `source=not_swapped`. The setup's `compose_legacy` helper cannot be used here, because it points at `$rollback_src`, which does not exist yet.

What it does:

1. Restores `source/` from `source.fce422d-rollback-*` (the new one kept as `source.failed-<time>`) and `deployment.env` from its rollback copy.
2. Runs `compose $BT_LEGACY_COMPOSE_FILES up -d --wait app`.
3. Runs `verify-unraid-deployment.sh` (WARN rule above).
4. Checks that `/readyz` is 200, and, when Live Activity push is enabled, that `/v1/server` advertises `live-activity-push-v1` (`legacy_live_activity=advertised`), as R9 step 6 does.

It never runs `dropdb`, `pg_restore` or any write, and it can be rerun.

Expected: `abort-before-commit: legacy_ready=1 schema=29 database=untouched`.

## R8 Phones

After R5.4 passed, first record that the phones come back. From this point a rollback loses their writes, and the script knows it from this marker, not from anyone's memory:

```sh
BT_CUTOVER_APPROVED=$homelab "$cutover" phones-back
```

It requires a passing `verify` of the committed import and the new app running. Expected: `phones-back: recorded (<BT_BACKUP_DIR>/phones-back.txt) import_id=<id>; …`.

Then the owner opens the app on both phones. R6 and R7 follow, each with its own yes.

After R-abort, the phones come back to the legacy server and need nothing.

- **Sync.** Both pull without a snapshot: no `snapshot_required` for their devices in the server log. Both still show the family and its history.
- **Cards.** A running feed or sleep card on one phone updates within a minute of a change made on the other phone, and no new card appears.
- **Icons.** Both phones show the same icons as before:
  - the baby with `boy.1` keeps it, and the other keeps the neutral face;
  - a rename on one phone keeps the icon on the other;
  - the server log shows no `invalid_request` or `invalid_mutation` for a baby.

  These are the rollout record's open checks, repeated on the new server.
- **Cursors.** `device_sync_cursors` rows reappear within 2 minutes:

  ```sh
  docker exec -e PGOPTIONS=-cdefault_transaction_read_only=on baby-tracker-sync-db-1 psql -XAtq -U postgres -d babytracker -c 'SELECT count(*) FROM device_sync_cursors'
  ```

## R6 Drop the legacy roles (C2, C4)

After its own yes. A one-time superuser step, in one transaction. It needs:

- the committed import's report (or, after an unknown commit outcome, the verify-only report that names its `import_id`);
- a `verify.txt` of that import;
- no rollback since.

```sh
BT_CUTOVER_APPROVED=$homelab "$cutover" drop-legacy-roles --with-live-activity   # without the flag if C4 is no
```

What it does:

1. Lists which of the roles exist.
2. Counts `pg_shdepend` rows in `babytracker` for them; this must be 0 after the import.
3. Copies `projector_password`, `projector_database_url`, `apple_revocation_password` and (C4) `live_activity_password`/`live_activity_database_url` into `$BT_BACKUP_DIR/retired-secrets/` (0600), and records each one's exact owner and mode (`stat -c '%u:%g %a'`) in `retired-secrets/manifest.tsv`. R9 restores exactly those.
4. Lists shared dependencies (database grants), then, in one `BEGIN`/`COMMIT`:
   - the count again;
   - `REVOKE ALL ON DATABASE babytracker FROM <role>` for each role that holds a database privilege;
   - `DROP OWNED BY` the roles;
   - `DROP ROLE`, logins before their groups.
5. Removes each retired secret the new composition no longer mounts, and keeps the rest.
6. Restarts the new app (`up -d --wait --force-recreate --no-deps app`, a few seconds of outage), so its readiness audit runs at once on the cluster without the roles: the maintenance pool's login and role checks included. Then it requires:
   - `/readyz` 200;
   - `/v1/server` advertising `live-activity-push-v1`: the Live Activity worker's first pass, which runs on the maintenance pool;
   - no `maintenance sweep failed` or `export worker iteration failed` in the app's log for `BT_CUTOVER_MAINTENANCE_SECONDS` (25 s: two retention passes);
   - `/readyz` 200 again after that window.

   The restart loses at most the wake worker's last unflushed tick (2 s) of phone pull progress; the phones' next pull records it again, and `phones-back.txt` (R8) already marks that they came back.

The roles are `baby_tracker_projector_app`, `baby_tracker_projector`, `baby_tracker_apple_revoker_app` and `baby_tracker_apple_revoker`. C4 adds `baby_tracker_live_activity_app` and `baby_tracker_live_activity`.

Expected: `roles_dropped=6 roles=…` (4 without C4), `secrets_retired=…` (every retired secret: the new composition mounts none of them), `post_drop: app=restarted readyz=200 capabilities=[…"live-activity-push-v1"…] maintenance_window=25s maintenance_failures=0`, then `drop-legacy-roles: done …`.

**STOP** if the dependency count is not 0. The `dependent=` lines name each role, catalog and count, and nothing was dropped. With Task 27's baseline the count is 0 (`TestLegacyRolesDropCleanlyAfterAnImport` runs these statements after an import, and the rehearsal drops all six roles), so a STOP here is real: report it; there is no partial or other flag to reach for.

**Every R6 STOP before the drop changes nothing.** The line says `nothing was changed and the new server keeps running … (not a rollback trigger)`. Leave the roles, keep the new server, report, fix, and retry R6. Never R9.

**A STOP in step 6 comes after the drop committed.** The line says `the roles are already dropped (roles-dropped.txt) …`. The new server keeps its data, and the cause is the server, not the roles: read the app's log, report it, fix, and rerun `drop-legacy-roles`, which then finds no role to drop and repeats only step 6. Not a rollback trigger on its own; if the server cannot be made ready, the owner decides on R9 as after R8, and R9 recreates the roles.

**When `verify` recorded `unraid_verify=warn`,** R6 and R7 refuse until the owner has accepted the drift. Rerun with `BT_CUTOVER_ACCEPT_UNRAID_WARN=1`; the script then prints `unraid_verify=warn accepted_by_owner=1`.

## R7 Delete the ledger directories (C3)

After its own yes, and only after `verify` passed. It comes after R8: the phones are back and have written, and R7's `--verify-only` re-proves the imported database as it is then (the rehearsal writes from a phone before its R7).

```sh
BT_CUTOVER_APPROVED=$homelab "$cutover" delete-ledger
```

It re-proves its precondition at deletion time:

- no rollback has happened (`rolled-back.txt`);
- `ledger.tar`'s SHA-256 is the receipt's;
- `verify.txt` is of the committed import;
- `--verify-only` passes on the current database with `start=imported` and that same `import_id`;
- `tar -df ledger.tar` finds no difference (the importer mounted the directories read-only).

Then it runs `rm -rf -- "$BT_DEPLOY_ROOT/security-ledger" "$BT_DEPLOY_ROOT/ledger-head"`.

Expected: `delete-ledger: ledger_deleted=1 import_id=<id> kept=<BT_BACKUP_DIR>/ledger.tar,<BT_BACKUP_DIR>/legacy.dump`.

**STOP** on any failed check: nothing is deleted, and the new server keeps running. Report, fix, and retry R7. **Never R9.** The dump and `ledger.tar` stay in the backup directory.

## R9 Rollback

R9 returns the homelab to `fce422d5` at schema 29 with `live-activity-push.yaml`, as it was at R1. It never answers an R6 or R7 STOP; those stay put.

**How rollback decides, read-only, before anything changes:**

1. **Which import.** It runs `--verify-only` on the database. The `import_id` there must be the committed one this backup directory recorded, or rollback refuses (exit 2): `this backup is not the one the database was imported from`. A directory with no committed import, such as R3's `backup.rehearsal-*`, is refused too: `records no committed import`.
2. **Whether the phones came back.** It counts as after R8 when either holds:
   - `phones-back.txt` exists;
   - `device_sync_cursors` has any row. The importer leaves the table empty, and a phone's pull fills it, so this does not depend on R8's command having been run.

   When cursor rows are what shows it, rollback writes the marker itself (`detected_by=device_sync_cursors`).
3. **The record.** Before the app stops, it writes both answers to `rollback-started.txt`. A rerun, after `dropdb` for instance, uses that file and derives nothing again.

**Before R8** (neither the marker nor cursor rows):
- R9 answers a STOP after the import committed (R5.4), after the owner's yes.
- No phone has written to the new server. The phones run V10, which the legacy server serves, so they need nothing.

```sh
BT_CUTOVER_APPROVED=$homelab "$cutover" rollback
```

**After R8** (the marker, or cursor rows), R9 runs only on the owner's explicit decision:

1. The coordinator states the loss: every write the phones made since R8 is lost from the server. The rollback keeps them only in a dump, for manual recovery.
2. The owner confirms in the conversation.
3. The owner force-quits both phones (D3's question), and they stay closed until step 6 below.
4. Without the flag, `rollback` refuses (exit 2). With it:

   ```sh
   BT_CUTOVER_APPROVED=$homelab "$cutover" rollback --owner-accepts-loss-of-phone-writes
   ```

5. Right after the app stops, and before any role step or `dropdb`, it dumps the new database to `$BT_BACKUP_DIR/rolled-back-from-<import_id>.dump`. It does this before R8 too, which costs about 26 MB on the homelab. The dump is 0600, read back with `pg_restore --list`, and recorded in `rollback-manifest.tsv`. A rerun keeps that first dump.
6. Its marker records `phones_must_sign_out_and_in=1`, and its **last line** is:

   ```
   rollback: REQUIRED: phones_must_sign_out_and_in=1 -- before any phone uses the legacy server, sign out and sign in again on every phone …
   ```

   The owner signs out and in again on every phone before using it with the legacy server. Each phone's cursor is past the legacy high water, so a phone that pulls without a fresh legacy snapshot silently misses the other phone's legacy writes. `next_sequence` is not adjusted. This step cannot be skipped.

   A phone opened after the rollback gets 401: its tokens were minted by the new server and are unknown to the legacy database. That is expected and safe; it is the cue for the step below.

   **How, on each phone** (fce422d5's app):
   - **Sign out:** Settings › Family › **Disconnect this device**, then sign in again.
     - Disconnect runs `AccountCoordinator.revokeDeviceBindingAndSessions`. It sends `client.logout()` (`POST /v1/auth/logout`) **before** `finishFamilyCleanup`, which deletes the family keychain entries and purges the shared realm with its sync cursor (`purgeSharedRealm`).
     - A phone whose tokens were minted by the new server after R8 is unknown to the restored legacy database. So `logout` can fail with 401, the `try` throws, and the purge never runs.
     - The app has no other local reset: `deleteAccount` deletes the account on the server, and the only other reset is UI-test-only.
   - **If Disconnect fails** (an error, or the family is still shown): **delete the app and reinstall it**, then sign in.
     - Deleting the app removes the realm with its cursor, and the `UserDefaults` session record.
     - Without that record, `restoreFamilySession` restores nothing, even though the keychain entries survive.
     - The phone's own data is on the legacy server up to R4, and in the dump after it.

It is staged. Everything that can fail without changing anything runs first:

- the receipt;
- `legacy.dump`'s and `ledger.tar`'s digests against the receipt;
- `ledger.tar` lists `ledger-head/actor-id-key.json`: `fce422d5`'s recovery gate (`internal/recovery/gate.go`, `ValidateActorIDKey`) refuses a ledger without it, so the legacy server would never become ready;
- the one `source.fce422d-rollback-*` and its `deployment.env.fce422d-rollback-*`;
- the retired passwords;
- `pg_restore --list` reading the dump back;
- the decision above (which import; whether after R8), recorded in `rollback-started.txt`;
- `compose $BT_LEGACY_COMPOSE_FILES build app migrate`;
- the retired secrets put back with their recorded owner and mode, then checked.

A failure among these prints `nothing was changed`. Then:

1. Stops the app. It then counts `device_sync_cursors` again. A phone may have come back since the decision; if rows appeared, it records `post_r8=1` in `rollback-started.txt` and writes the R8 marker. Without `--owner-accepts-loss-of-phone-writes` it then starts the app again on the new database and refuses (exit 2): nothing was dropped. It then dumps the new database (step 5 above), before R8 too.
2. Recreates every legacy role and login that is missing from `pg_roles`, whatever `roles-dropped.txt` says. It does this in one transaction, creating each only when missing, with `fce422d5`'s `init-roles.sql`/`init-users.sh` attributes and the retired passwords.
3. Runs `dropdb --if-exists --force` and `createdb -O baby_tracker_migrator`, restores `fce422d5`'s database-level grants, then `pg_restore --single-transaction --exit-on-error` from `legacy.dump`.
4. If R7 removed the ledger directories, extracts them from `ledger.tar` (as root, owners kept); otherwise checks them against it.
5. Restores `source/` and `deployment.env` from the rollback set (left alone if already the legacy ones).
6. Runs `up -d --wait app`; checks that `/readyz` is 200 and `schema_compatibility.current_version` is the receipt's 29. When the legacy composition enables Live Activity push, it then waits (as long as for `/readyz`) until `/v1/server` advertises `live-activity-push-v1`, which `fce422d5` does only while its Live Activity worker runs, and prints `legacy_live_activity=advertised` (`legacy_live_activity=off` otherwise); a worker that never resumes is a STOP.
7. Writes `rolled-back.txt` and moves `verify.txt` and the import and verify reports aside. Nothing of this cutover runs again on the rolled-back homelab, rollback included.

After a STOP in steps 1–6, **rerun `rollback`**: each step is rerunnable.

Expected: `rollback: legacy_ready=1 schema=29 head=<R1's head> ledger_restored=<0|1>`. After R8 the `REQUIRED` line above follows it, as the last line.

Then run `"$src/Server/scripts/verify-unraid-deployment.sh" verify` (WARN rule above).

Writes the phones made to the new server after R8 are lost from the server by a rollback; they are kept only in the dump. That is why R9 after R8 is the owner's decision alone, and why R6 and R7 STOPs never lead to it.

## R10 Confirmation

The owner confirms the cutover in the conversation. The coordinator records it in `docs/verification/<date>-homelab-cutover.md`, which unlocks Task 29. The record holds:

- R1's summary;
- every expected line above, as printed;
- the backup's digests;
- the owner's R8 observations.

The backup directories (dump, ledger tar, receipt, reports, `preflight.txt`, `verify.txt`, `roles-dropped.txt`, retired secrets, and the R3 backup) are kept 30 days, then deleted with the owner's approval.
