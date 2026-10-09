# Family Live Activity push — local verification, 2026-10-04

- **Branch:** `feat/family-live-activity-push` (worktree `.worktrees/family-live-activity-push`).
- **Spec:** `docs/superpowers/specs/2026-10-03-family-live-activity-push-design.md`, section "Verification and rollout".
- **Plan:** `docs/superpowers/plans/2026-10-03-family-live-activity-push.md`, Task 11.

This is **local** evidence only:

- Disposable Postgres and containers, synthetic tokens, a disposable P-256 key.
- The identitytest recorder stands where Apple stands.
- Simulator work ran only on the shared `platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853` (iPhone 17 Pro). No simulator was created, cloned or erased.

What was **not** done:

- No signed build was installed on a phone.
- No APNs request reached Apple.
- No Apple account, key or provisioning was changed.
- Nothing was pushed, deployed or run against production data.

Phone and production acceptance are in the Task 12 runbook, [`docs/runbooks/family-live-activity-push.md`](../runbooks/family-live-activity-push.md), and need a separate rollout decision. The runbook has not been executed; see "Task 12" below.

## Source identity

| Commit | What |
| --- | --- |
| `e4e01a5` | Server fixes from the first Task 11 run: updates and ends get an APNs expiration, stale leases are released, a first-part side switch continues the card. These were reviewed separately. |
| `ee05ab7` | `test(scripts)`: the protocol gate's script test expected two-caregiver step 18, but the script requires 24. This was already broken on `main`. |
| `0b79773` | `test: verify family live activity push lifecycle`: the tests, the identitytest-only APNs recorder hook, the harness push profile, UI tests, the Makefile count and UI flags. |
| `e974fa1` | `test(server)`: expect schema 28 in the overlap-check and tenant suites. Their stale 27 literals failed the first all-server gate. |
| `00455ac` | `docs`: the first version of this document. |
| `9f27098` | `fix(ios): retire registered cards the app ends` (review fix round 1, F1). In a push-managed realm every end the app makes on a realm card — own pass or not — is recorded as a retirement before the card goes, and a card whose retirement cannot be recorded stays up. Two Swift regressions; `EXPECTED_TESTS` 1564 → 1566. |
| `a73528c` | `test`: the Go probe `AppEndsAndDismissals` and the review's assertion fixes (F3) in the composition test and the Swift harness test. |
| `d1309e1` | Final review fix wave, server: a phone's own timer-bound card is adopted on the pass that sees its start (the next lifecycle's card is never taken for the previous end); the server's own retirement stays sticky through a later phone report; per-attempt APNs outcomes (class, HTTP status, allowlisted reason) in `live_activity_history` and `last_reason`, plus one bounded log line per non-accepted send; topic failures are configuration failures that clear no tokens; per-job failures are parked instead of stopping the worker; runners drain up to the batch limit; priority 5 for cosmetic updates. Migration 28 amended in place (unshipped). |
| `3956902` | Final review fix wave, iOS: the card controller publishes each realm token card's timer and effective session and the coordinator registers them for a card no push has reached; a card that lost to a registered smaller-ID duplicate is retired, not reported; a card refused again after device renewal is left pending while the others register; app ends of realm cards are retired whenever a retire handler exists; dead `end(activityID:)` removed. |
| `d9677e7` | Final review fix wave, tests: own-card adoption in the client's exact registration shape (Go composition and Swift e2e proof step 13), sticky retirement, recorded outcomes, parking, draining, priorities. `EXPECTED_TESTS` 1566 → 1571. |
| `cf9b263` | `docs`: runbook STOP gates on the recorded APNs classes; README push behaviour. |
| `d7bbcea` | `fix(server)`: quote the OpenAPI DELETE description (the reworded plain scalar broke YAML parsing in every OpenAPI contract test). **Final code commit; all gates below ran on it.** |

`e974fa1` differs from `0b79773` only in `Server/cmd/baby-tracker-overlap-check/main_test.go` and `Server/internal/tenant/context_test.go`.

- **Run on `0b79773`:** the Swift, protocol, smoke, build, simulator and UI gates. None of them builds or runs those two files.
- **Rerun on `e974fa1`:** the all-server suite and `go vet`.
- **Rerun on `a73528c`** (fix round 1): the Swift suite, the race suite of the one changed Go package, its `go vet`, the protocol gate and the simulator build; see "Fix round 1 gates". Since `e974fa1` the only server change is the test file `Server/cmd/baby-tracker-server/live_activity_composition_test.go`; no server source outside `_test.go` files changed.
- **Run on `d7bbcea`** (final review fix wave): the complete gate set, every gate; see "Final review fix wave gates". These results supersede the earlier tables for the final source.
- **Later commits:** any commit after `d7bbcea` is documentation only.

**What the logs record.** The nine logs written by `task-11-gates-runner.sh.txt` — `task-11-gate-{swift-test,server-test,server-protocol-e2e,server-smoke,server-build,go-vet,ios-build,test-ui}.log` and `task-11-gate-server-test-rerun.log` — record the `HEAD` they ran on and, in their header line, that no tracked file other than `TASKS.md` was modified when the gate started. `task-11-gate-go-vet-rerun.log` and `task-11-rollout-image.log` record only the `HEAD`. The fix-round logs (`task-11-fix1-gate-*.log`, runner `task-11-fix1-gates-runner.sh.txt`) record that check before and after each gate. Three of those logs record a modified tracked file; see "Fix round 1 gates".

## Gates

All gates ran through `rtk proxy`, sequentially, with raw receipts in `.superpowers/sdd/2026-10-03-family-live-activity-push/task-11-gate-*.log` (untracked). The runner is kept as `task-11-gates-runner.sh.txt`. Each exit code is the command's own. Swift runs used an owned `TMPDIR=$(mktemp -d /tmp/btlap.XXXXXX)`.

| Command | Commit | Result | Time |
| --- | --- | --- | --- |
| `make test` | `0b79773` | exit 0, `OK: 1564 tests` (`EXPECTED_TESTS` 1563 → 1564) | 19 s |
| `make server-test` | `0b79773` | exit 2. The packages `cmd/baby-tracker-overlap-check` (5 tests) and `internal/tenant` (1 test) still expected schema 27; every other package was ok. Fixed in `e974fa1`. | 1270 s |
| `make server-test` | `e974fa1` | exit 0: 34 packages ok, no failures (`cmd/baby-tracker-server` 416.6 s, `internal/liveactivity` 139.7 s, `cmd/baby-tracker-overlap-check` 33.9 s, `internal/tenant` 2.3 s) | 1223 s |
| `make server-protocol-e2e` | `0b79773` | exit 0 (see below) | 106 s |
| `make server-smoke` | `0b79773` | exit 0: push-disabled composition, `contiguous schema 28`, cleanup proofs | 29 s |
| `make server-build` | `0b79773` | exit 0: multi-arch `cacheonly` build (no image produced, by design) | 21 s |
| `sh -c 'cd Server && go vet ./... && go vet -tags identitytest ./...'` | `0b79773`, `e974fa1` | exit 0 both times | cached |
| `make build SIMULATOR='platform=iOS Simulator,id=D17101C9-…'` | `0b79773` | exit 0, `** BUILD SUCCEEDED **` (compiled the changed `AppEnvironment.swift`) | 8 s, incremental |
| `make test-ui SIMULATOR='platform=iOS Simulator,id=D17101C9-…'` | `0b79773` | exit 0, `Executed 167 tests, with 0 failures`, `** TEST SUCCEEDED **`; ran with `-parallel-testing-enabled NO -maximum-concurrent-test-simulator-destinations 1` | 4061 s |

### Fix round 1 gates (`a73528c`)

Sequential, through `task-11-fix1-gates-runner.sh.txt`, each Swift or harness gate with a fresh owned `TMPDIR=$(mktemp -d /tmp/btlap.XXXXXX)`. Summary: `task-11-fix1-gates-summary.log`.

| Command | Result | Time |
| --- | --- | --- |
| `make test` | exit 0, `OK: 1566 tests` (`EXPECTED_TESTS` 1564 → 1566) | 22 s |
| `env BT_TEST_PACKAGES='./cmd/baby-tracker-server' bash Server/scripts/test-db.sh` (race, disposable database; the only Go package changed) | exit 0, `ok babytracker/server/cmd/baby-tracker-server 467.331s` | 471 s |
| `sh -c 'cd Server && go vet ./cmd/baby-tracker-server/ && go vet -tags identitytest ./cmd/baby-tracker-server/'` | exit 0 | cached |
| `make server-protocol-e2e` | exit 0. Image proof `source_sha=a73528c…`, `schema=28`, `live_activity_push=true`; `✔ Test run with 15 tests in 8 suites passed`; 12 phone proof lines; ledger line derived from the phones' ledgers `registrations=2 sandbox=2 cards=3`, Postgres agrees with `delivered=9`; no APNs token in 532 server log lines; cleanup proof | 109 s |
| `make build SIMULATOR='platform=iOS Simulator,id=D17101C9-…'` | exit 0, `** BUILD SUCCEEDED **`, recompiled `LiveActivityController.swift` and `LiveActivityController+Push.swift` | 12 s |

All commands ran through `rtk proxy`.

Three fix-round logs record a modified `docs/verification/2026-10-03-family-live-activity-push.md` (this document, being edited):

| Log | Start line | After line |
| --- | --- | --- |
| `task-11-fix1-gate-server-affected.log` (the Go race run) | `none` | the modified document |
| `task-11-fix1-gate-go-vet.log` | the modified document | the modified document |
| `task-11-fix1-gate-server-protocol-e2e.log` | the modified document | `none` |

- The document was set aside seconds after the protocol gate started.
- That gate builds its image from `Server/` only.
- The Swift and simulator-build logs read `none` on both lines.
- No gate reads that file.

**Not rerun in fix round 1:** the all-server suite (only one test file in one package changed), `make server-smoke` and `make server-build` (no server source changed), and `make test-ui` (the product change is Live Activity card reconciliation, which XCUITest cannot drive). Their results above stand for `0b79773` / `e974fa1`.

### Final review fix wave gates (`d7bbcea`)

The complete gate set, sequential, through `final-fix-gates-runner.sh.txt`, which refuses to start unless `HEAD` is `d7bbcea` and records in each log that no tracked file other than `TASKS.md` was modified, before and after the gate (every log reads `none` on both lines). Swift and harness gates used a fresh owned `TMPDIR`. Summary: `final-fix-gates-summary.log`; receipts `final-fix-gate-*.log`.

| Command | Result | Time |
| --- | --- | --- |
| `make test` | exit 0, `OK: 1571 tests` | 20 s |
| `make server-test` | exit 0, every package ok | 1255 s |
| `make server-protocol-e2e` | exit 0 | 100 s |
| `make server-smoke` | exit 0 | 27 s |
| `make server-build` | exit 0 | 17 s |
| `sh -c 'cd Server && go vet ./... && go vet -tags identitytest ./...'` | exit 0 | 2 s, cached |
| `make build SIMULATOR='platform=iOS Simulator,id=D17101C9-…'` | exit 0, `** BUILD SUCCEEDED **` | 11 s, incremental |
| `make test-ui SIMULATOR='platform=iOS Simulator,id=D17101C9-…'` | exit 0, `Executed 167 tests, with 0 failures`, `** TEST SUCCEEDED **` (parallel testing disabled) | 4056 s |

An earlier run of the same runner on `cf9b263` (`final-fix-gates-run1/`) passed `make test` and failed `make server-test` only in the `internal/protocol` OpenAPI tests (`yaml: line 60: mapping values are not allowed in this context`); `d7bbcea` fixed the quoting.

**Rollout image (P0, local only, never pushed):** `baby-tracker-server:rollout-d7bbcea39e03`, image `sha256:9e7a747c90d59f1dc6bd59a116ab08edaa2ef65ca6ac8615f48292b31214a5fc`, `linux/amd64`, revision label `d7bbcea39e0351743d0bfbcf19d6d4709f7a80aa`, binary SHA-256 `3732daa9f5e78c05baeac1e5a2344a2ebb6655918d2258d54fc455a665c81df7`; `__identitytest` 0, `identitytest.invalid` 0, `api.sandbox.push.apple.com` 1, `api.push.apple.com` 1 (`final-fix-rollout-image.log`, built with the runbook's P0-3 commands on OrbStack). It supersedes the `e974fa1` image `sha256:9a98587f…`, which must not be deployed.

### What the protocol gate proved (`0b79773`)

- **Image proof:** `source_sha=0b79773…`, `schema=28`, `manifest=28:28:000028_live_activity_push.sql`, `live_activity_push=true`.
- The existing suites still pass:
  - black-box restart proof;
  - Go protocol tests;
  - the identitytest recorder test;
  - `✔ Test run with 15 tests in 8 suites passed`, including two-caregiver step 24 and owner-reinstall.
- **Live Activity proof:** 12 phone proof lines. Postgres agrees: `registrations=2 sandbox=2 cards=3 delivered=9`, and no APNs token appears in the 532 server log lines.
- **Cleanup proof:** zero containers and zero volumes remain.

## What the new tests cover

### Server composition (`Server/cmd/baby-tracker-server/live_activity_composition_test.go`, runs in `make server-test`)

**Setup.** The production `configureLiveActivityWorkerWithWait` runs behind the production `httpapi.New` router, on a fully migrated disposable database.

- Phones speak authenticated HTTP: sync, live-activity registration, refresh, logout, family and account commands.
- Apple's transport is a recorder of the worker's requests; the harness separately runs the production APNs transport.
- Steps wait on conditions, never on elapsed time:
  - each send must arrive within **10 s**;
  - "nothing else" is asserted only after a database idle check (no live lease, no due work, no unswept family sequence).

| Test | Proves |
| --- | --- |
| `TwoCaregiverLifecycle` | A's own start is suppressed. B's delayed registration is pushed the running feed. B's card is confirmed by an update on the same generation. An edit updates both cards. **A first-part side switch (Left stop + Right start in one request) is one update per card: no end, no start, same generation.** A worker restart delivers a pause committed while no worker ran. The break ends on its own deadline with no write, within 10 s of the deadline. Sleep starts only on B and ends on both. An in-session pause and resume keeps one card. Nothing is ever sent to A's push-to-start token. |
| `AuthorityProfiles` | **Refresh rotation** keeps delivering; **logout** stops it. **Remove**: nothing sent; **rejoin** with re-registration resumes current content without a start. **Owner → viewer**: updates carry `canWrite=false`, and the viewer's Stop is refused with HTTP 403 and code `read_only_caregiver` (not retryable), with no journal entry and nothing pushed. **Account deletion** and **recovery-epoch bump** stop delivery. **A queued retry after a 503** goes only to the replacement token. For revocations, the test waits up to 90 s for the registration to be revoked and its delivery rows deleted, asserting **zero sends** throughout: 55–57 s for logout, removal and account deletion (the minute authority sweep); the epoch bump's prior-epoch registration was already retired when the wait began (the worker retires stale-epoch registrations on every claim). |
| `SelfHostedWithoutCapability` | Push disabled: no capability, no push services, registration refused with 503, sync unaffected, nothing sent. |
| `InFlightRevocationBoundary` | One payload already in transport when a removal committed is still delivered. This is the **unavoidable boundary**: its dispatch check preceded the revocation. The removal is not delayed by the send, and nothing is sent afterwards. |
| `LostAcknowledgment` | Apple took a start but the answer was lost. Nothing repeats it; the device receipt lists it; a replay is not proof. A foreground absence proof yields exactly one retry with a new attempt; a second proof yields nothing. A card that really was delivered confirms by registering. |
| `RetiredAndNeverPushedCards` | Task 9 ledger probe (c), and the server's reading of a timer-less card registered `ended`; see below. |
| `AppEndsAndDismissals` | Ledger probe (b) after `9f27098`. **App retirement:** A's adopted, timer-bound card is ended by the app while the feed runs; the phone records a retirement and registers nothing. A's registration keeps receiving the generation's sends; the server does not mark it dismissed. How ActivityKit treats those sends on a real ended activity is unverified. With the phone step of `00455ac` instead (the end registered `ended`), the edit never reached A (RED receipt `task-11-fix1-go-red.log`). **Parent's swipe:** a registered `ended` or `dismissed` suppresses that generation on that phone only: B keeps the edit and the pause, and A's next feed — a new generation — reaches A's new card. |

### Protocol harness (`make server-protocol-e2e`)

**Exact schema.** The disposable composition asserts an exact schema of 28, which must agree with the embedded compatibility manifest and the database's privilege manifest. There is no `>=`.

**Push profile.** The ordinary gate runs a push profile:

- the production worker with its role-separated `baby_tracker_live_activity_app` login;
- a disposable P-256 key;
- `BT_IDENTITYTEST_APNS_SINK=1`. In identitytest builds only, this keeps the production `APNSTransport` (provider token, headers, payload, classification) but hands each request to an in-process recorder exposed at `/__identitytest/apns`. Production builds construct only `NewAPNSTransport`.

The refresh-recovery modes and the smoke gate keep push disabled.

**`LiveActivityPushE2ETests`.** Three phones built with the app's own `FamilySyncComposition.assemble`. Each has the production driver, coordinator, registration ledger and `LiveActivityPushClient` over pinned TLS; only ActivityKit is scripted. Every recorded payload is decoded with `TimerActivityContentState`.

1. The owner signs in and creates the family.
2. The joiner and a third phone join.
3. The owner registers at launch with no settings step. The phone that treats this server as self-hosted makes no push probe and no registration.
4. The owner starts a feed and registers its local card.
5. The joiner's first probe fails (private network down), and the driver's retry registers it.
6. The push-to-start is decoded (`breastLeft`, `running`). It carries a valid ES256 provider token (`kid E2EKEY0001`, `iss E2ETEAM001`), topic `….push-type.liveactivity`, priority 10 and expiration 0.
7. The pushed card registers and is confirmed by an update, not a start.
8. The break end is checked against the joiner's own 5-minute limit.
9. The other side in the same session updates the card in place.
10. A lost acknowledgment. When the joiner comes to the foreground the recorder still holds only that one start (asserted); after the joiner's foreground inventory proves the card absent, exactly one retry with a new `apns-id`. Nothing waits for a quiet period before that check: the Go composition's `LostAcknowledgment` proves "never repeated blindly" against an idle queue.
11. The sleep end (`dismissal-date` = timestamp − 1) and a two-part break suggesting the left side.
12. Negatives: nothing to the owner's start token, nothing from the self-hosted phone, and **the joiner never synced**.

The script cross-checks the phones' ledgers against Postgres — the registration, environment and card counts in the ledger line are read from the owner's and joiner's real registration ledgers since `a73528c` (before, they were constants that Postgres was checked against) — and greps the server log for every synthetic token.

**Rollout artefact (not pushed).**

- **Build:** `docker buildx build --platform linux/amd64 --load --build-arg BT_SOURCE_SHA=e974fa1… -t baby-tracker-server:rollout-e974fa1bb2ad -f Server/Dockerfile Server`.
- **Image:** `sha256:9a98587f166c0e0d19825d32b5a12b0706ed0e2f4359fa82ffa968f57d869450`, `linux/amd64`, label `org.opencontainers.image.revision=e974fa1bb2ad03af6eb57c91b5185562d2f53c4e`.
- **Binary:** sha256 `be86bab5…3ab94`. It contains no `__identitytest` route and no identitytest APNs host; it does contain Apple's sandbox host.
- **Location:** retained locally; receipt in `task-11-rollout-image.log`.
- **After fix round 1:** not rebuilt. No server source outside `_test.go` files changed after `e974fa1`, so the binary's sources are those of the branch head; its revision label still names `e974fa1`.
- **Not for rollout.** This image was built from `e974fa1`, which is not the final reviewed source. The runbook (P0, R1-5) requires a fresh `linux/amd64` image built from the final reviewed commit, with its ID, platform, revision label and binary SHA-256 recorded before any rollout.

This is the deployment-architecture artefact. The protocol gate's image is a native arm64 identitytest image that the harness removes.

### UI (`UITests/BabyTrackerUITests/LiveActivityPushUITests.swift`)

- **No new setting:** a signed-in family owner sees no Live Activity or push setting in Settings or Caregivers.
- **Stale-action handoff:** a handed-off card action shows the exact Task 10 copy once, starts nothing, and is not shown again on the next launch. It is seeded by a DEBUG-only `-uiTestParkedActivityHandoff`, as an intent parks it.
- **Viewer Home controls:** still covered by `ReadOnlyCaregiverUITests`.

## Ledger probes

| Item | Outcome |
| --- | --- |
| (a) a removal refused non-retryably stays pending and fails passes closed (Task 8) | **Not feasible** through the real server: a valid family session cannot provoke a non-retryable DELETE refusal without forging ledger state. Remains a deferred limitation. |
| (b) a managed-regime non-own end of a never-pushed registered card (Task 9) | **Defect confirmed by review, fixed in `9f27098`, probed.** The app ended registered cards unretired on non-own passes (for example another caregiver's sleep ending this phone's feed break), and the coordinator then registered ActivityKit's `ended`, which the server reads as the parent dismissing that lifecycle. Now every app end of a realm card in a push-managed realm is retired first (Swift: `aNonUserPassRetiresTheRegisteredCardItEnds`, `aNonOwnEndOfARegisteredCardIsNeverReportedToTheServer`). `AppEndsAndDismissals` shows the server side: after a retired end, that phone's registration keeps receiving the generation's sends, and the server does not mark it dismissed; a registered `ended`/`dismissed` suppresses that generation on that phone only. `RetiredAndNeverPushedCards` still shows that a timer-less card registered `ended` suppresses nothing. |
| (c) push-to-start retry after the phone retires an unregistered server-started card (Task 9) | **Covered.** Exactly one retry follows an absence proof; a second proof for the same generation sends nothing. |
| (d) Task 7 smoke not rerun after `cbfc6a4` | **Covered.** `make server-smoke` exit 0 on `0b79773`. |

## Found by these tests and fixed in `e4e01a5`

The fixes were made and reviewed separately; the tests here now pass against them.

| Defect | What happened | Status |
| --- | --- | --- |
| Updates and ends never sent | Updates to running cards and ends without a card expiry failed local APNs validation (zero expiration) and retried forever. | Fixed |
| Stale leases | A concurrent re-dirty left a lease stranded for 30 s, delaying delivery. | Fixed; steps now complete within 10 s |
| Side switch | A first-part side switch replaced the other phone's card with a new start alert. | Fixed |

## Not proven here

- Delivery by Apple, and ActivityKit behaviour on a real phone: background, locked, terminated, dismissal, `continueInForeground` from the Lock Screen.
- Signing and entitlements, the production APNs key and environment, and outbound connectivity from the server.
- Any production data or deployment.
- **Revocation latency.** A revoked registration stops receiving immediately, because the dispatch check refuses every send. Its rows are retired only by the minute sweep (observed 55–57 s). That is intended behaviour, not a delivery SLA.
- **The disposable push profile's `0600` secrets.** The harness ran on OrbStack: the rollout receipt's buildx line reads `building with "orbstack" instance`, and `task-11-fix1-docker-runtime.log` records the active context `orbstack` (`docker info`: OrbStack, 29.4.0). The earlier harness logs do not name the runtime. The secrets were readable by the container's non-root user through OrbStack's bind-mount ownership mapping on macOS. Docker Desktop and Linux hosts are untested; on Linux the files would need to be owned by (or readable for) uid 65532.

## Known limits

These are carried into the runbook's "Known limits".

- **App-ended cards keep receiving sends.** Retirement is client-side only. APNs sends continue to an app-ended card's token until that generation ends. This is a ruled trade-off.
- **Revocation completes at the minute sweep.** Expect about 60 s or less; zero sends were observed meanwhile.
- **Sign-out sends no client DELETE.** It relies on `/v1/auth/logout` plus the server sweep.
- **Apple-credential revocation** relies on Apple server notifications. `/v1/auth/apple/notifications` is registered only in hosted mode, so the self-hosted production deployment does not receive them.
- **Enabled startup can take about 25 s in the worst case** when it is ready at boot.
- **The key-arrival tools have not met Apple or Unraid.** The readiness plan's Task 2 added `baby-tracker-server live-activity-preflight` and `Server/scripts/provision-live-activity-secrets.sh`. The preflight is proven against an in-process HTTP/2 TLS server for every answer class (`TestLiveActivityPreflightClassifiesAppleAnswers`) and against the disposable database for the worker checks (`TestLiveActivityPreflightVerifiesWorkerDatabase`); the script against temporary directories without root (`TestLiveActivitySecrets*`, intended owners asserted in `--dry-run` output). Neither has sent a request to Apple or written to the Unraid host; the runbook's R2-2, R2-4 and R2-6 are their first real runs. The preflight's PASS proves the key, Team ID and Key ID; it cannot prove the topic (Apple does not document whether a dummy token is refused before or after the topic check), so the first accepted real send in R2-7 is the topic proof.
- **The upgrade rehearsal has only run synthetically, with one secrets directory.** `Server/scripts/rehearse-live-activity-upgrade.sh --synthetic` (readiness Task 3) seeds its own schema-27 deployment and then upgrades a copy of it, and both sides read the same generated secrets (`pepper_file="$secrets/actor_id_pepper"`): every pepper matched by construction. Dump mode's shape — only `actor_id_pepper` is the production deployment's, every other pepper is fresh for the disposable copy, and the new app must still become ready against the restored dump and ledger — is therefore first proven by R1-3b on Unraid. It fails closed: a mismatch ends in `new app never became ready; security verify: …` and `STOP: rehearsal`, before R1-4 changes production configuration.

Three limits recorded here at the time were closed by the readiness plan's Task 1 (`docs/superpowers/plans/2026-10-04-live-activity-push-readiness.md`) on this branch:

- **Own-card adoption picking a stale row.** `adoptOwnCard` now adopts only an `active` row with a token, and an `ended`/`dismissed` row joins a lifecycle by reference only before that lifecycle has a generation. Regressions: `TestLiveActivityAdoptOwnCardRequiresALiveToken`, `TestLiveActivitySwipedBreakCardIsNotAdoptedTwoLifecyclesLater`.
- **Unregistering while push is disabled.** `DELETE /v1/live-activities/device/{installation_id}` uses the repository-only `liveactivity.Unregistration`, composed in every mode, and succeeds with push disabled or the worker stopped; `PUT` stays 503 and the capability unadvertised. Regression: `TestLiveActivityRegistrationRoutes`.
- **An enabled-mode boot that is not yet ready exits the process.** Activation (schema, privileges, shared database, budget, readiness, initial pass) is attempted at boot and otherwise deferred to supervision; only malformed configuration exits. Regressions: `TestLiveActivityRuntimeDefersActivationUntilReady`, `TestLiveActivityRuntimeConfigurationFailsFastDatabaseChecksDefer`, `TestServerExitsOnMalformedLiveActivityConfiguration`.

### Swift runner aborts under the ordinary temporary directory (Task 1)

This is preserved evidence; the root cause is unproven.

- **The aborts.** During Task 1, two full `rtk make test` runs with the ordinary `TMPDIR` aborted. The `BabyStoreTests` runner exited with signal 6 at 881 of 1259 tests (`task-1-fractional-full.log`, `task-1-fractional-full-retry.log`).
- **The crash reports.** `~/Library/Logs/DiagnosticReports/swiftpm-testing-helper-2026-10-03-171116.ips` and `…171156.ips` place the `SIGABRT` in the existing BabyStore SwiftData write path, during `LiveCardSequenceTests` and `TimerTests`.
- **The pass.** The same command passed with an owned short `TMPDIR` (`OK: 1259 tests`, `task-1-fractional-full-shorttmp.log`). Every later Swift gate used `TMPDIR=$(mktemp -d /tmp/btlap.XXXXXX)`.
- **What this does not show.** It is neither a known source defect that was fixed nor a proven environmental cause.

## Task 12: rollout and two-phone acceptance runbook

[`docs/runbooks/family-live-activity-push.md`](../runbooks/family-live-activity-push.md) was written on 2026-10-04 against the tree at `87c6f50` and corrected in the final review fix wave (`cf9b263`: STOP gates on recorded APNs classes, new migration 28 digest). The final reviewed code commit is `d7bbcea`.

It is documentation only. Nothing in it was executed: no Apple account, profile or key change, no phone build or install, no SSH, deployment or image push.

**What it covers:**

- **P0 Local evidence:** clean-tree gates and a fresh local `linux/amd64` image of the final commit, with its label, binary SHA-256 and identitytest-string checks.
- **P1 Apple provisioning:** Push Notifications on `com.olhapi.babytracker`, the regenerated development profile with `aps-environment`, and the APNs key.
- **P2 Project switch:** device configurations sign with `Apps/BabyTrackerLiveActivityPush.entitlements`, and the pinning Swift test is updated.
- **R1 Server upgrade:** schema 27 → 28 with push off, after the old binary's exact-schema check, a backup, the cutover dump plus ledger/head tar, and the roles installed by recreating db and rerunning `init-users.sh`.
- **D Phone builds:** the `codesign` and embedded-profile check against the resolver's input.
- **R2 Enable push:** the three secret files with their owners and modes, the overlay, outbound Apple reachability, and the capability.
- **A Acceptance:** 22 two-phone rows, from the brief, the spec and the Task 12 controller additions.
- **X Disable and rollback:** disabling delivery by dropping the overlay keeps schema 28. A binary rollback is not proposed, because the old binary's manifest is exactly 27.

**Local check:** `rtk git diff --check` is clean.

**Open before execution:**

- **P1** is blocked on an approved Apple account change. The current profiles lack `aps-environment`.
- **No 27 → 28 dry-run tool.** No in-tree tool rehearses 27 → 28 with the `init-users.sh` rerun on a restored production dump.
- **The production schema** must be confirmed as 27 in R1-1.

## Readiness before the APNs key, and key day (2026-10-04)

Plan: [`docs/superpowers/plans/2026-10-04-live-activity-push-readiness.md`](../superpowers/plans/2026-10-04-live-activity-push-readiness.md). Every task was reviewed independently; one whole-branch review (server and client halves) and one fix wave followed.

**What landed (817482b..0b3aca6):**

- Server: own-card adoption only from live rows with a token; unregistration works while push is disabled; enabled boot never exits on readiness/schema/role/budget (activation deferred under supervision); a start rejected before any network I/O is released, not left ambiguous; stale-lease release failures logged; duplicate ordering pinned to byte order; deterministic worker tests.
- Key-arrival tooling: `baby-tracker-server live-activity-preflight` (Apple dummy-token check through the production transport and JWT, worker-database checks), `Server/scripts/provision-live-activity-secrets.sh` (the three push secret files with the owners and modes the containers need), `Server/scripts/rehearse-live-activity-upgrade.sh` (R1-3b: the 27 → 28 cutover on a restored dump in a disposable project; synthetic run PASS), `scripts/live-activity-push-signing.sh` (profile/identity `check`, `check --app`, guarded `enable`).
- iOS: viewers never see write controls; dedicated Lock Screen card intents (Siri/widget intents unchanged); removal wedge, cancelled re-probe, alert/sheet sequencing, identity-session leak fixed; pending removals sent while push is unavailable; coordinator/controller split.
- Main `e6c2166` merged (`10f91ec`). Its whole-sleep counting (`e736df5`) is now mirrored by the server projection (`0b3aca6`), pinned by the shared fixture's `joined-sleep` case through the phone's production builder.

**Gates** (runner `readiness-gates-runner.sh.txt`, receipts `readiness-gate-*.log`, each logging `HEAD` and a clean tree):

| Commit | Gates | Result |
| --- | --- | --- |
| `10f91ec` | all eight | exit 0: `OK: 1602 tests`; all-server 34 packages; protocol e2e; smoke; server build; `go vet` (both tags); simulator build; UI `Executed 38 tests, with 0 failures` |
| `0b3aca6` | all but UI (its diff is server code and tests only) | exit 0: `OK: 1602 tests`; all-server 34 packages (1302 s); protocol e2e; smoke; server build; `go vet`; simulator build |

**Reference image (P0, local only, never pushed):** `baby-tracker-server:rollout-0b3aca68940f`, `sha256:7a08399187051c216bf660221d29555174c341d5ccd461d7f68235010a2279f2`, `linux/amd64`, label `0b3aca68940f1a83e74fb5daa01f56d8a5c807c7`, binary SHA-256 `2c67b8ce9f47463ca9ee2983e18e3f6fa44aa2930fb021f4f681504dbd5d4a95`, no identitytest strings. R1-5 compares the Unraid-built binary with this hash.

**Key day (P1/P2), 2026-10-04:**

- Apple account: Example Developer's team `TEAM123456` (the bundle IDs keep the historical `com.olhapi.*` names). Push Notifications enabled on `com.olhapi.babytracker`; the widget App ID unchanged.
- Profile `BabyTracker Development - TEAM123456` regenerated: UUID `00000000-0000-0000-0000-000000000000`, `aps-environment = development`, 4 devices, expires 2027-10-04. Installed; the two stale copies moved aside (not deleted).
- `scripts/live-activity-push-signing.sh check`: `result: ready`; signing identity `Apple Development: developer@example.com (CERT123456)` present and valid.
- APNs key `KEY1234567` (stored in the operator's credentials folder, mode 0600): local sandbox preflight `RESULT: PASS` (`400 BadDeviceToken` — key, team and key ID accepted; topic not contradicted). One provider JWT minted at 19:01 CEST.
- P2 `enable` applied (`aec1cfc`): both app configurations sign with `Apps/BabyTrackerLiveActivityPush.entitlements`; `make test` `OK: 1594` at the time.

**Still not proven:** delivery to a phone, the server's outbound Apple connectivity, the production upgrade and the two-phone acceptance (runbook R1, D, R2, A).
