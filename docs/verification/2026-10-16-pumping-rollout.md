# Pumping rollout — 2026-10-16

The user approved the merge ("Merge") and then the server deploy ("deploy"). The server goes before the phones: a phone on this build that syncs a pump to a schema-33 server has that mutation quarantined for good, and the new server binary runs only on schema 34.

## Merge

`feat/pumping` (`594cf1de`) merged into `main` as `eaddcef0`, a real merge because `main` had gained `120f559e` (a bottle's End picker built with the sheet). Both edited `TimeSpanSection`'s End row; the merge keeps `120f559e`'s always-built picker with the branch's stacking and size cap at accessibility sizes.

- `make test` on the merged tree: OK, 1931 tests.
- UI on the merged tree: `PumpingFlowTests`, `LoggingFlowTests`, `AccessibilityAuditTests`, `UkrainianLayoutSmokeTests` — 27 tests, 0 failures.
- Server suite, build and protocol e2e (six pumping proofs) passed on the branch; `Server/` at `eaddcef0` is identical to the branch's.

## Server (Unraid `baby-tracker-sync`)

**Scope.** `431dc6cc..eaddcef0` under `Server/` is pumping only: five commits (protocol, care kind 10 and migration `000034_pumping.sql`, server rules and export, the e2e, the review fixes). No other undeployed server work.

**Preflight.** Revision `431dc6cc`, schema 33, three containers healthy, 0 restarts.

**Source.** `git archive eaddcef0 Server` has SHA-256 `1c9f966a6ad636482006be3384a350460ca4923a3a7d971dd99ad53f2f4e1b19`; it matched after upload. Compatibility `{"min":34,"max":34}`. Against the running source only pumping files differed; the compose files and the Dockerfile were identical.

**Backups** (mode 600; 1030 `pg_restore --list` entries each)

| File | SHA-256 |
|---|---|
| `backup.pre-eaddcef.dump` (app running) | `384086dfe0055e9deebb163eb85c920da5faf4d39b8a42c0b9f30f39b9087888` |
| `backup.cutover-eaddcef.dump` (app stopped) | `26b82a86898978d98db6aee4375a36055104251fa9e24ee5f8911c4aa69d916e` |

**Rollback set:** `source.431dc6c-rollback-eaddcef` and `deployment.env.431dc6c-rollback-eaddcef`; the `431dc6cc` image is retained. The old binary does not start on schema 34, so a rollback means restoring the cutover dump first.

**Config.** `provision-unraid-secrets.sh` with `BT_ALLOW_SOURCE_UPDATE=1`: only `BT_SOURCE_SHA` and `BT_IMAGE_NAME` changed; `compose config -q` passed. Unrelated-container baseline re-captured (61 lines); the previous one is `unrelated-containers.before.prior-eaddcef.tsv`.

**Image.** `baby-tracker-server:eaddcef0…` is `sha256:8781d2b64efbd5f291372998993cfae926195a02d5658d39657425a364b76dcc`, revision label `eaddcef0c6c932ed818bc9a570ceea368ffe27d6`. Built while the old app ran.

**Cutover** (app down 12:05:50–12:05:58 host time, 8 seconds)

- Stopped only the app, then took the cutover dump.
- `compose run --rm --no-deps migrate` exited 0, no error lines.
- Schema 33 → 34; `schema_migrations` holds 32, 33, 34.
- Care rows per kind identical before and after: `1:2, 2:396, 3:198, 4:126, 5:503, 6:15, 7:1, 8:14`.
- `care_entities_kind_check` allows kind 10 on the parent and all 16 partitions (17 of 17); the privilege manifest has 177 rows at 33 and 177 at 34; `care_entities_one_open_pump` exists.
- `compose up -d --wait app`: all three containers healthy, the app on `eaddcef0…`.

**After the deploy**

- `verify-unraid-deployment.sh verify` passed: private HTTPS health and readiness 200, the live socket gives 401 for a bogus bearer, no host ports, unrelated containers unchanged.
- First minutes: 58 log lines, all INFO; 55×200 and 1×401 (the verifier). 0 restarts.

## Phones

The user approved the installs ("phones").

- **Build:** signed Debug of `2ea985de` from a clean detached worktree, `xcodebuild -scheme BabyTracker -configuration Debug -destination 'generic/platform=iOS'`. `codesign --verify --deep --strict` passes; team `TEAM123456`; `scripts/live-activity-push-signing.sh check --app` shows the profile "BabyTracker Development - TEAM123456" with `aps-environment: development` ok. App executable SHA-256 `cfe9c21506e9c2b479c35aacdcbc712b2cd70f7b540325c5acd543456ec56a63`.
- **Phone B** (`00000000-…0000`): installed in place and launched. In the next minutes the server logged 80 lines, all INFO: 75×200, 2 live-socket upgrades (101), 1×204, 1×401 (a token refresh at launch). 0 restarts.
- **Phone A** (`00000000-…0000`): locked through about 45 minutes of retries; installed in place and launched once the user unlocked it. In the next minutes the server logged 112 lines, all INFO: 107×200, 1×101, 1×204, 2×401 — `/v1/live` and `PUT /v1/live-activities/device` at launch, before the token refresh. 0 restarts. No pump exists yet (`kind=10` count 0).

## Still open

- The user's check on the phones: the Lock Screen card and its Stop when locked, a pump on the other phone's strip, the card ending when the other phone stops, the widget Pump tile.
- Never run a schema-33 server binary against this database again.
- The worktree `.worktrees/pumping` and branch `feat/pumping` can be deleted after the phones.
