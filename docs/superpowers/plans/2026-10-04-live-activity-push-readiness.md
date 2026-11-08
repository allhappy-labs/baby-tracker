# Live Activity push — readiness before the APNs key

**Goal:** close every locally fixable issue left after the family Live Activity push implementation, and add the tooling that turns "the APNs key and push-capable profiles exist" into a short, checked enablement. Spec: [design](../specs/2026-10-03-family-live-activity-push-design.md). Predecessor plan: [implementation](2026-10-03-family-live-activity-push.md). Evidence of what is open: the verification note `docs/verification/2026-10-03-family-live-activity-push.md` ("Known limits"), the runbook `docs/runbooks/family-live-activity-push.md`, and the previous ledger `.superpowers/sdd/2026-10-03-family-live-activity-push/progress.md` (lines tagged `minor (deferred)` and `parked`).

**Requested by the user (2026-10-04):** "Fix all the issues and immediate improvements so when I'll provide the key we are fully ready to go."

## Global constraints

- Same as the predecessor plan: automatic behaviour on the built-in server only; self-hosted unchanged; no settings UI; no per-second pushes; no public ports; shared simulator `platform=iOS Simulator,id=<simulator-udid>` only, never create/clone/erase simulators.
- No Apple account, profile or key changes; no deployment, SSH, image push, merge to main or phone install. Tools that would touch Apple or production are written and tested locally (dry-run / disposable) but not executed against real systems.
- Migration 28 is still unshipped: amend in place, regenerate the manifest with the existing command, keep historical hashes, keep privilege inventories exact.
- Never log or store keys, tokens, baby names, care content or raw payloads.
- TDD with RED/GREEN receipts; deterministic tests (no new wall-clock waits); explicit-path commits with Conventional Commit messages ending `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.

## Task 1: Server correctness and robustness

Files: `Server/internal/liveactivity/*`, `Server/cmd/baby-tracker-server/*`, `Server/internal/httpapi/live_activity.go`, migration 28 + manifest, tests, README/runbook lines that describe changed behaviour.

1. `adoptOwnCard` (worker.go) adopts only `status = 'active'` rows with a token; a stale phone-reported ended/dismissed row from an older lifecycle is never adopted. Regression: same feed session resumed across two sleeps after a swipe.
2. Unregistration works while push is disabled: `DELETE /v1/live-activities/device/{installation_id}` uses a repository-only path that needs no worker, keys or APNs config, with the same authentication/authorization and idempotency. PUT routes stay unavailable while disabled; capability stays unadvertised. Tests for disabled and stopped-worker cases. Update README/runbook/verification "Known limits".
3. Enabled-mode boot no longer exits when recovery/readiness is not yet ready: the HTTP server starts (not ready, exactly like disabled mode) and push activation is deferred to the existing supervision, which activates once local checks and readiness pass. Malformed configuration (bad key file, bad env) still fails fast at startup. Tests for not-ready-at-boot → later activation, and malformed config → exit.
4. A start rejected before any network I/O (local validation or configuration failure raised before the request is written) is known-unsent and released for a later attempt rather than recorded `startUnconfirmed`; a start that may have reached Apple keeps the ambiguity rules. Regression for both.
5. `releaseStaleLease` errors are logged with a safe code instead of being silently swallowed.
6. Make `TestLiveActivityRunDrainsABacklogWithinOnePoll` and `TestLiveActivityHistoryRecordsResponseClasses` deterministic (injected clock/jitter or explicit completion signals; no sub-second wall-clock assertions).
7. Duplicate ordering: pin the server's card ordering to byte order (`COLLATE "C"`) so it provably matches the client's `String <` winner rule; regression with mixed-case IDs.
8. Task 7 cleanups: remove the duplicate `liveActivityFamilyResolver` (reuse `syncFamilyResolver`), drop the unneeded `started` channel, move the `FamilyService`/`Readiness` nil checks before opening the worker pool.

## Task 2: Key-arrival preflight and secret provisioning

Files: `Server/cmd/baby-tracker-server` (new subcommand), `Server/internal/liveactivity` (preflight helper), `Server/scripts/provision-unraid-secrets.sh` (or a focused sibling script), tests, README/runbook.

1. `baby-tracker-server live-activity-preflight` (exact name may follow existing operator command conventions): loads the same configuration as startup (`BT_APNS_*`, `BT_LIVE_ACTIVITY_DATABASE_URL_FILE`), validates the key file (mode, P-256 PKCS#8 parse), team/key ID format, bundle ID and topic, signs a provider JWT, and for each selected environment opens an HTTP/2 connection to Apple and sends one Live Activity push to a syntactically valid all-zero dummy device token. Expected classification: `400 BadDeviceToken` ⇒ key, team and topic accepted; `403 InvalidProviderToken`/`ExpiredProviderToken` ⇒ key/team wrong; `400 TopicDisallowed`/`DeviceTokenNotForTopic`/`BadTopic` ⇒ bundle/topic wrong; network errors ⇒ connectivity. It also verifies the worker database login (exact role inventory, same database) when a database URL is configured. Output is a short human summary plus exit code; never prints the key, JWT or URL secrets. Tested against an httptest HTTP/2 TLS server for every class (Apple hosts injectable only in tests), and against the disposable database for the role check.
2. Secret provisioning: extend the existing Unraid provisioning script (or add `provision-live-activity-secrets.sh` following its conventions) to create `live_activity_password` (db-owned, 0600) and `live_activity_database_url` (app uid 65532, 0600) from a generated password, and to install a provided `.p8` as `apns_key.p8` (uid 65532, 0600) from a path argument — never from a command-line secret value. Idempotent, refuses to overwrite without an explicit flag, supports a dry-run that prints only actions. Tests run it against a temp directory (ownership checks skipped or simulated where not root).
3. Runbook R2 uses the preflight before enabling and the provisioning script for the files; document exact invocations.

## Task 3: Upgrade rehearsal

Files: `Server/scripts/rehearse-live-activity-upgrade.sh` (name may follow conventions), test, runbook R1.

A script that takes a PostgreSQL custom-format dump (or, with `--synthetic`, builds a schema-27 database from the `8032e79` tree in a disposable container), restores it into a disposable PostgreSQL, runs the new image's migration and the `init-users.sh` rerun exactly as R1 prescribes, verifies schema 28, the exact role/privilege inventories, record counts unchanged, readiness 200 with push disabled, and the old binary's refusal to run against 28, then removes every owned resource. Run it end to end with `--synthetic` and record the receipt. Runbook R1 requires running it against the cutover dump before touching production.

## Task 4: iOS fixes and isolation

Files: `Packages/BabyTracker/Sources/{BabyStore,BabySync,SettingsFeature,BabyFeature}`, `Apps/{BabyTrackerApp,SharedIntents,BabyTrackerWidgets}`, tests, Makefile count.

1. Viewer devices never show write controls on any card, including token cards they requested locally before the first push (local content carries `canWrite=false` from current family authority).
2. Lock Screen card buttons use dedicated Live Activity intent types carrying the card target, so the Siri/Shortcuts/Home Screen widget intents keep their original `supportedModes` and expose no "Activity" parameter. Behaviour of the card buttons is unchanged.
3. Removal wedge: a removal refused non-retryably for this installation is resolved (finished when the server's sweep or tombstone makes it moot, or bounded) so it can never block later passes indefinitely; regression.
4. A cancelled background evaluation re-arms the event-driven re-probe; regression.
5. The Lock Screen handoff alert and the bottle "Set amount" sheet never collide on one activation (sequence them); regression in the RootView model layer.
6. The intent's identity client URLSession is invalidated after the bounded catch-up; regression or leak check.
7. Close the Task 10 test gaps: write-boundary refusal branches, start on a pushed break card, baby/kind mismatch handoff, offline scope-only ready, successful catch-up through the real composition.
8. Split `LiveActivityPushCoordinator.swift` (registration of activities/removals/inventory) and `LiveActivityController.swift` push section into focused files without behaviour change.

## Task 5: Signing readiness

Files: `scripts/live-activity-push-signing.sh` (or similar), project test, runbook P1/P2.

A script with `check` and `enable` modes. `check` decodes the installed provisioning profiles referenced by `project.pbxproj` for the app and widget, reports each profile's expiry and `aps-environment`, and inspects a built app's effective entitlements with `codesign -d --entitlements` when a path is given. `enable` refuses unless every device profile used by the app contains `aps-environment`, then performs the documented project switch (device configurations sign with `Apps/BabyTrackerLiveActivityPush.entitlements`) and updates the pinning test so it asserts the new state. Test the script logic against fixture profiles (synthetic plists, no real profile mutation); run `check` against the real local profiles and record that it reports the expected blocker.

## Task 6: Final verification

Full gate set on the final commit (same runner as the previous plan), fresh local rollout image, verification note and runbook updated, whole-branch review of the readiness range, one fix wave if needed.
