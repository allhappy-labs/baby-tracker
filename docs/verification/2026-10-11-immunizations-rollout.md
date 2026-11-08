# Immunizations rollout — 2026-10-11

The user approved the deploy, the merge and the phone installs ("All approved, proceed"), confirmed the deploy scope ("Ship both"), and asked for the remaining steps to be completed. The server went first: a phone on this build that syncs an `immunization` with a schema-32 server has that mutation quarantined for good, and the new server binary runs only on schema 33.

## Pre-deploy evidence (branch `immunizations`, head `339f655f`, on `main` at `43788f4f`)

- `make test`: OK, 1856 tests.
- `make server-test` (race, disposable PostgreSQL): 39 packages ok, none failed.
- `make server-protocol-e2e`: exit 0 against schema 33, the vaccination steps and the owner-reinstall ledger (kind 9) included.
- UI classes on the shared simulator: `LoggingFlowTests` (14) and `SettingsFlowTests` (4) on the final code. `GrowthFlowUITests`, `AccessibilityAuditTests` and `UkrainianLayoutSmokeTests` passed earlier on the branch and were not re-run after the rebase and the last fixes.
- Whole-branch review by a fresh reviewer: no critical findings; one important (Ask under-counted doses for a birth-day shot) and the minors fixed.
- Details and screenshots: [verification notes](2026-10-11-immunizations/README.md).

## Server (Unraid `baby-tracker-sync`)

**Scope.** `3e586cce..339f655f` under `Server/` is immunizations (care kind 9, migration `000033_immunizations.sql`) plus server commits `main` already had but the homelab did not: the post-cutover cleanup `b474c4b0` (the one-time legacy importer and its admin subcommand, the verifier's importer-only mode, the two unused ledger volumes in the Unraid compose file) and the cutover-script fixes it then deleted. No other migration.

**Preflight.** Revision `3e586cce`, schema 32, three containers healthy, 0 restarts. Compose runs with `Server/compose.yaml`, `deploy/unraid/compose.yaml` and `deploy/unraid/push.yaml`.

**Source.** `git archive 339f655f Server` has SHA-256 `7ce6ba7fbcc3668b4ffe57ea81d35b5e813a7d959acf7678a219d51ba7eeee01`; it matched after upload. The staged tree has compatibility `{"min":33,"max":33}`. Against the running source only the expected files differed (the new migration, the removed legacy scripts, the trimmed Unraid compose file, README and test scripts); `compose.yaml`, `push.yaml` and the Dockerfile were identical.

**Backups** (mode 600; 1030 `pg_restore --list` entries each)

| File | SHA-256 |
|---|---|
| `backup.pre-339f655.dump` (app running) | `7e6bfb61bd1ac1ae11491dded113973f402dbcac8fe9d1185319fa8915f0c773` |
| `backup.cutover-339f655.dump` (app stopped) | `4e8b3bb5ed61a8becee16a441b7bfbc02e3ccaa3ed6986148246cac1c6f0c233` |

**Rollback set:** `source.3e586cc-rollback-339f655` and `deployment.env.3e586cc-rollback-339f655`. The `3e586cce` image is retained. After the migration the old binary no longer starts (its window is exactly 32), so a rollback means restoring the cutover dump first.

**Config.** `provision-unraid-secrets.sh` ran with `BT_ALLOW_SOURCE_UPDATE=1`. Only `BT_SOURCE_SHA` and `BT_IMAGE_NAME` changed; `compose config -q` passed. The unrelated-container baseline was re-captured before the deploy (61 lines); the previous one is kept as `unrelated-containers.before.prior-339f655.tsv`.

**Image.** `baby-tracker-server:339f655f…` is `sha256:88907cc2189e624529fee7936cc4a56a1318ccf36928ade33edfbd95dec44a5a`, label revision `339f655f1fb62fe9312d834fe5d4659ca63122c9`. Built while the old app kept running.

**Cutover** (app down 16:27:30–16:27:38 UTC, 8 seconds)

- Stopped only the app, then took the cutover dump.
- `compose run --rm --no-deps migrate` exited 0; the migrate log has no error lines.
- Schema 32 → 33 (`000033_immunizations.sql`); `schema_migrations` holds 32 and 33.
- Care rows per kind were identical before and after: `1:2, 2:297, 3:152, 4:97, 5:356, 6:11, 7:1, 8:11`.
- `care_entities_kind_check` is widened on the parent and all 16 partitions (17 of 17); the privilege manifest has 177 rows at 32 and 177 at 33.
- `compose up -d --wait app`: all three containers healthy, the app on `339f655f…`.

**After the deploy**

- `verify-unraid-deployment.sh verify` passed: private HTTPS health and readiness 200, the live socket gives 401 for a bogus bearer, no host ports, unrelated containers unchanged.
- First minutes: 58 log lines, all INFO; 55×200 and 1×401 (the verifier). 0 restarts.
- Not run: `authority verify` as a separate step. The app's own readiness, which the verifier checks, passed.

**Process note.** The permission system refused the production commands on the first attempt, although the user had approved in conversation. Nothing was worked around; the uploaded archive and the staged directory sat unused until the user asked again for the remaining steps to be completed.

## Merge

`main` was fast-forwarded from `43788f4f` to `339f655f`. The main checkout held two untracked files, this feature's spec and plan, identical to the branch's copies; they were moved aside (`/tmp/imm-main-untracked/`) so the merge could bring the tracked ones.

## Phones

- **Build:** signed Debug of `339f655f1fb62fe9312d834fe5d4659ca63122c9` from the clean worktree, `xcodebuild -scheme BabyTracker -configuration Debug -destination 'generic/platform=iOS'`. `codesign --verify --deep --strict` passes; team `TEAM123456`; `scripts/live-activity-push-signing.sh check --app` reports `result: ready`, profile "BabyTracker Development - TEAM123456", `aps-environment: development`. App executable SHA-256 `efad85670d22de8ace821be2edf88f1971cfaea7e7e9fc9a4f730b4e47fc70bb`.
- **Install:** `devicectl device install app` in place, no uninstall, on Phone A (`00000000-…0000`) and Phone B (`00000000-…0000`). Local data is kept; the store migrates V10 → V11 on first launch.
- **Launch:** both apps were launched with `devicectl`. In the two minutes after, the server logged 98 lines, all INFO: 93×200 and 5 live-socket upgrades (101). 0 restarts. No immunization exists yet (`kind=9` count 0).

## Still open

- The user's check on the phones, from the verification notes:
  1. A shot logged on one phone appears on the other, in History and on the Immunizations screen.
  2. Vaccine moved up in Settings → Home buttons reaches the Quick Log widget, and its tile opens the vaccine panel.
  3. Ask answers "when was her last vaccine?" and "how many Prevenar doses has she had?".
- Never install a pre-V11 build on these phones again, and never run a schema-32 server binary against this database.
- Pumping's plan still names migration `000030` and `SchemaV11`; it must take `000034` and `SchemaV12`.
- The branch `immunizations-pre-rebase` and the worktree `.worktrees/immunizations` can be deleted.
