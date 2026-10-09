# Supplements verification — 2026-09-26

Initial live acceptance source: `a65b0e83df4d6c9685e9e7e61bee4493eeb7466b` plus the Task 4 acceptance-test changes committed with this document. The subsequent old-client compatibility fix is `4f4fea30a946811ce72b5a54d277d77c6af89901`; its verification is below. Task 4 began at `9fa8e8c`.

## Live local two-caregiver acceptance

`rtk make server-protocol-e2e > /path/to/artifact 2>&1` — **exit 0**.

This gate builds a disposable server image and PostgreSQL database, migrates to schema 18, runs server restart/protocol/race checks, and executes the real Swift family composition over pinned local TLS. The Apple identity adapter supplies the test proof; no Apple production sign-in or deployed server is involved.

Image source label: `a65b0e83df4d6c9685e9e7e61bee4493eeb7466b`; server binary SHA-256: `d34d8a43a819b63c44b3a0ce75f730fbf3ef7832ac1a49b29ad560ab7e522929`; architecture `arm64`; schema `18`.

The existing `TwoCaregiverSyncE2ETests` sequence now proves:

- After family creation/import/join, the owner creates Vitamin D in the shared realm and the second caregiver receives the same ID/name/logging time through outbox and remote apply.
- The second caregiver reuses ` vitamin d `; the canonical option remains Vitamin D, then logs Iron. Both phones derive the same reusable options.
- The second caregiver renames the older Vitamin D row, backdates its event time by a day, and edits its note. The server acknowledges the update and the owner sees each correction.
- Later edit acceptance does not replace immutable `logged_at` or make the older log the last-used choice. Both phones still select Iron.
- A new empty store receives actual server snapshot pages, with no preexisting base/overlay. It preserves both original logging times, the corrected event time/note, and the same Iron selection/options.
- Deletes travel owner-to-joiner and joiner-to-owner. Options disappear after their last surviving logs are deleted, only Iron remains, and both outboxes drain.

The supplement proof steps 16 and 17 precede the existing relaunch/live-hint steps, leaving step 15 as the final completion marker checked by `Server/scripts/protocol-e2e.sh`. The gate exits nonzero for any Swift assertion failure or missing final marker, and checks disposal of its containers and volumes. RTK abbreviated successful output after the server race output; the final gate exit was captured as 0. Individual successful proof lines are not retained in that abbreviated log.

## Regression found and fixed by live acceptance

The first live run failed seven edit/snapshot expectations. A diagnostic rerun happened to pass with a different fractional clock. Task 4 then fixed the test clock fraction to `.008` to reproduce the failure deterministically.

`rtk make server-protocol-e2e > /path/to/artifact 2>&1` — **RED, exit 2** before the precision fix. The edit receipt was `.superseded`, code `record_logged_at_mismatch`, observed entity version `1`. Original logging timestamp: `2026-09-26T20:00:32.007Z`; the second caregiver re-emitted `2026-09-26T20:00:32.006Z`. The Foundation fractional parser/formatter had lost a millisecond. The gate reported zero remaining containers and volumes after failure.

Full deterministic red output: `~/Library/Application Support/rtk/tee/1790454158_make_server-protocol-e2e.log`. First red output: `~/Library/Application Support/rtk/tee/1790453845_make_server-protocol-e2e.log`.

Task 2 fixed supplement parsing/formatting in `a65b0e8`, using canonical microsecond precision. The same deterministic live regression then passed. The server's immutable timestamp rule was retained.

The earliest test-first run, `rtk swift test --package-path Packages/BabyTracker --filter TwoCaregiverSyncE2ETests`, also failed compilation because the new `Phone.supplements` and `Phone.freshSnapshot` helpers were not yet implemented (`/path/to/artifact`). The later live receipt failure supplies behavioral red evidence beyond that scaffold failure.

## Other gates

| Command | Result | Evidence / scope |
| --- | --- | --- |
| `rtk make test` | **785 tests, exit 0** | Final source with precision fix; `/path/to/artifact`. The Makefile runs the full Swift package suite and checks its literal count. |
| `rtk go test ./...` (from `Server`) | **1,656 passed in 35 packages, exit 0** | `/path/to/artifact`. Run before the Swift-only precision fix; no later Go changes. Without a DB DSN, database-dependent tests skip; the live gate supplies real PostgreSQL coverage for its selected packages. |
| `rtk xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -derivedDataPath /path/to/artifact build` | **exit 0** | Final source; `/path/to/artifact`. |
| `rtk xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -derivedDataPath /path/to/artifact -resultBundlePath /path/to/artifact -only-testing:BabyTrackerUITests/SupplementFlowTests test` | **2 passed, 0 failed/skipped, exit 0** | `xcresulttool get test-results summary` verified iPhone 17 Pro, iOS 26.5. Run before precision fix; UI code unchanged afterward and final app build rerun. |
| `rtk git diff --check` and `rtk git diff --check 8425a4b HEAD` | **exit 0** | Working change and committed feature diff. |

## Old-client compatibility fix and final local gate

The whole-branch review found that pre-feature Swift clients silently skip unknown supplement records while advancing their sync cursor. Commit `4f4fea3` adds an optional `X-Baby-Tracker-Supplements: v1` HTTP request header to current Swift sync and snapshot calls. On a family with supplement history, the server returns `unsupported_capability` to an unmarked request before giving it a successful page or new cursor. It retains the boundary after deletion through the mutation journal. Families without supplement history continue to accept old requests. The server checks under the family lock before mutations and page capture, including continuations and a racing first supplement. The request JSON and strict capability list remain compatible with pre-feature servers.

The frozen pre-feature HTTP request, marked current request, snapshot/sync continuations, tombstones, and first-supplement race/idempotent retry are covered by new database-backed tests. Focused PostgreSQL race tests passed. After the fix, the full Swift package suite passed, 1,656 Go unit tests passed across 35 packages, and `rtk make server-test` passed every PostgreSQL-backed race package (exit 0). The scoped independent re-review cleared the prior Important finding and found no new blocking defect.

`rtk make server-protocol-e2e` was rerun on `4f4fea3` and passed with **exit 0**. Its image build embedded source version `4f4fea30a946811ce72b5a54d277d77c6af89901`; the disposable schema-18 TLS server and Swift two-caregiver flow completed. This proves local server/client interoperability with the marker. It does not establish behavior of an installed old app or deployed proxy.

The final `rtk xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -derivedDataPath /path/to/artifact build` also passed with **exit 0** after the transport change.

Compiler/simulator caches, Docker access, and local test listeners required escalated execution. No dependency/lint gate beyond the repository's applicable test/build commands was added.

## Review and limits

Reviewed supplement routing, adaptive sheet/validation, History edit/delete, SwiftData V4 migration, mutation projection, immutable timestamp mapping, content hash/fixture, import parent remapping, realm movement/purge, remote required-kind handling, server migration/RLS/grants, quota/deletion/snapshot switches, and compatibility boundary. The live timestamp defect was corrected and reverified; no additional integration blocker was found.

The server requires schema 18 with coordinated migration/restart; this document is not deployment evidence. Existing old-client unsupported-required-resource handling is covered at protocol/store level, not by installing an old app. Two caregivers here are two application object graphs with separate stores/sessions, not two physical iPhones. No production deployment, push, or merge occurred.

Local store logging can accept a tombstoned baby, as the preexisting diaper path also does; this existing parent-existence policy was not changed. Failed-save UI behavior is covered by model tests with a real realm transition, not simulator fault injection. Existing compiler warnings remain.
