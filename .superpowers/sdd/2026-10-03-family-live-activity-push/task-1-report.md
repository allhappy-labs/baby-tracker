# Task 1 report: versioned Live Activity contracts

Status: complete. Task base: `3dac705`. Scoped commit message: `feat: define live activity push contracts`.

## Scope and interface rulings

- Shared `LiveActivityScope` and `LiveActivityRevision` live in BabyProtocol, preserving BabyStore's dependency direction. BabyStore exposes type aliases and realm/target comparison helpers.
- Registration API fields use snake_case. ActivityKit attributes, content, scope and revision metadata use camelCase. Private Swift registration-scope and distinct Go payload adapters keep these representations explicit.
- Both registrations carry `installation_id`; authenticated authority is established by later server handlers, never by client claims.
- Counters use the existing safe integer ceiling `9007199254740991`. Epoch and registration/token/presentation versions start at one; family sequence and APNs Unix timestamp permit zero. Swift rejects fractional numeric values and out-of-range values without adding a custom lexical parser.
- `sideLabel` remains the saved wire field. `localizedCaption` derives new remote captions from phase/kind on-device; Task 9 owns widget wiring.
- Task 1 adds reusable OpenAPI schemas and the optional capability registry entry. Actual path entries land with their Task 3 handlers to preserve `TestOpenAPIRouteSetMatchesRegisteredRoutes`. Default server capabilities remain unchanged until optional runtime configuration is ready.
- The controller cleared Swift work after the 1247-test baseline while retaining Server/ read-only until its full disposable baseline completed.
- Optional Go generation/timer/session UUIDs are `*string`: absent/null remains unknown, while a present empty string fails UUID validation, matching Swift.

## Changes and files

- `Packages/BabyTracker/Sources/BabyProtocol/LiveActivityDTOs.swift`: validated scope/revision value types, device/token registrations, authoritative generation mappings and receipt; canonical lowercase UUID encoding; safe integer, token, payload-version, state and preference bounds.
- `Packages/BabyTracker/Sources/BabyStore/LiveActivityIdentity.swift`: public aliases plus entire-realm and baby/kind target comparisons.
- `Packages/BabyTracker/Sources/BabyStore/TimerActivityAttributes.swift`: optional immutable attributes scope, optional remote content revision, legacy-default `omittedParts`, and on-device typed caption helper preserving saved `sideLabel`.
- `Packages/BabyTracker/Sources/BabySync/EndpointIdentity.swift`: `advertisesLiveActivityPush` capability probe helper.
- `Server/internal/protocol/live_activity.go`: matching registration/storage contracts and validation using existing request envelopes and protocol error categories.
- `Server/internal/liveactivity/types.go`: ActivityKit camelCase payload types, explicit registration/payload revision adapters, Foundation reference-date conversion and legacy decoding defaults.
- `Server/internal/protocol/auth.go`, `Server/internal/protocol/types.go`: register the optional capability and permit a configured advertisement, while leaving the active default capability list untouched.
- `Server/api/openapi.yaml`: reusable registration, receipt, scope, revision, token, counter and content-state schemas; no unimplemented paths.
- `Server/api/fixtures/live-activity-v1/{device-registration,token-registration,registration-receipt,content-states,legacy-content}.json`: shared golden corpus. Eight typed variants cover left/right breast, timed bottle, pause, mixed break with a truncated summary count, sleep, stopped sleep references, and an edited fractional start time. These are payload contracts, not APNs delivery or projection acceptance.
- `Packages/BabyTracker/Tests/BabyProtocolTests/LiveActivityDTOTests.swift`, `Packages/BabyTracker/Tests/BabyStoreTests/LiveActivityPayloadTests.swift`, `Server/internal/protocol/live_activity_test.go`: real decoding/encoding, validation, realm isolation, legacy defaults and fixture parity checks.
- `Makefile`: expected Swift total increased from 1247 to 1258 for 11 new tests.

Downstream Go consumers use `liveactivity.NewRevision(protocol.LiveActivityRevision)` for APNs content, `Revision.Protocol()` for validation/storage identity, and `FoundationDate(unixSeconds)` only for content dates. `PresentationTimestamp` remains server Unix seconds. Receipt `generations` must be an array, including `[]` for no mappings.

## RED/GREEN evidence

All commands run from the isolated family-live-activity-push worktree through `rtk`. Full outputs are retained beside this report.

| Command | Result | Log |
| --- | --- | --- |
| `rtk swift test --package-path Packages/BabyTracker --filter LiveActivityDTOTests` | RED exit 1: registration/scope/revision symbols absent | `task-1-swift-red.log` |
| `rtk swift test --package-path Packages/BabyTracker --filter LiveActivityPayloadTests` | RED exit 1: `remote`, `omittedParts`, `localizedCaption` absent | `task-1-payload-red.log` |
| `rtk swift test --package-path Packages/BabyTracker --filter LiveActivityDTOTests` | GREEN exit 0: 4 tests | `task-1-swift-green.log` |
| `rtk swift test --package-path Packages/BabyTracker --filter LiveActivityPayloadTests` | GREEN exit 0: 4 tests | `task-1-payload-green.log` |
| `rtk swift test --package-path Packages/BabyTracker --filter LiveActivityDTOTests` | RED exit 1: receipt symbol absent | `task-1-receipt-red.log` |
| `rtk swift test --package-path Packages/BabyTracker --filter LiveActivityDTOTests` | GREEN exit 0: `Test run with 5 tests in 1 suite passed` | `task-1-receipt-green.log` |
| `rtk make test` | GREEN exit 0: `OK: 1256 tests` before shared fixture tests were added | `task-1-swift-full.log` |
| `rtk proxy env BT_TEST_PACKAGES='./internal/protocol' BT_TEST_RUN='LiveActivity' bash Server/scripts/test-db.sh` | RED exit 1: `package babytracker/server/internal/liveactivity is not in std` because the new package was not yet implemented; disposable database setup succeeded | `task-1-go-red.log` |
| `rtk swift test --package-path Packages/BabyTracker --filter 'LiveActivity(DTO\|Payload)Tests'` | RED exit 1: both fixtureParity tests reported missing new JSON corpus files | `task-1-fixtures-red.log` |
| `rtk proxy env BT_TEST_PACKAGES='./internal/protocol' BT_TEST_RUN='LiveActivity' bash Server/scripts/test-db.sh` | GREEN exit 0: `ok babytracker/server/internal/protocol 1.394s` | `task-1-go-green.log` |
| `rtk swift test --package-path Packages/BabyTracker --filter 'LiveActivity(DTO\|Payload)Tests'` | GREEN exit 0: 6 DTO tests and 5 payload tests passed | `task-1-fixtures-green.log` |
| `rtk proxy env BT_TEST_PACKAGES='./internal/protocol' BT_TEST_RUN='LiveActivity' bash Server/scripts/test-db.sh` | RED exit 1: `TestLiveActivityTokenRegistrationBounds: accepted invalid known_generation_id=""` | `task-1-optional-uuid-red.log` |
| `rtk proxy env BT_TEST_PACKAGES='./internal/protocol' BT_TEST_RUN='LiveActivity' bash Server/scripts/test-db.sh` | GREEN exit 0: `ok babytracker/server/internal/protocol 1.403s` after nullable UUID correction | `task-1-optional-uuid-green.log` |
| `rtk make test` | Final GREEN exit 0: `OK: 1258 tests` | `task-1-swift-final.log` |
| `rtk proxy env BT_TEST_PACKAGES='./internal/protocol ./internal/liveactivity' bash Server/scripts/test-db.sh` | Final GREEN exit 0: `ok babytracker/server/internal/protocol 1.914s`; `? babytracker/server/internal/liveactivity [no test files]`, its contract behavior is exercised by the protocol fixture tests | `task-1-go-full.log` |
| `rtk git diff --check` | GREEN exit 0, no output | final self-review |

Go's owned disposable runner uses `go test -p=1 -race -count=1` and rejects an externally supplied database URL. Each run completed its owned resource cleanup without a cleanup error. All initial RED logs and the additional optional-UUID regression RED remain retained. The shared fixtures are compared against actual Swift and Go JSON encodings, including omission of nil fields and Foundation date seconds, rather than only accepting a successful decode.

## Existing warnings

The initial GREEN rebuild surfaced existing warnings in `Sources/BabyStore/BabyStore.swift:130` (`nonisolated(unsafe)`), `Sources/AskFeature/AskModel.swift:184` (unreachable branch), `Tests/SettingsFeatureTests/ReauthenticationWiringTests.swift:148` (mutation after sendable capture), and `Tests/SettingsFeatureTests/CaregiversScreenCopyTests.swift:75` (actor-isolated helper). These files remain unchanged versus `3dac705`; they are outside Task 1.

## Self-review and concerns

Self-review inspected the complete scoped source/fixture/schema diff, checked legacy defaults and dependency direction, and found/fixed the explicit-empty optional UUID discrepancy through another RED/GREEN cycle. No outstanding Task 1 blocker or defect found.

No simulator, phone, production, deployment or external account actions performed. The iOS-only ActivityKit attributes have not been compiled in this task; the controller owns later shared-simulator app builds. Runtime authorization/monotonic upserts, actual network routes, canonical payload-size projection, widget use of localizedCaption, ordering and physical delivery remain later-task acceptance gates. No claim of implemented push delivery is made.

## Review fix: exact Swift JSON integer decoding

The review found that Foundation's `Int64` decoding rounded fractional JSON values into valid counters. A shared Decimal-based keyed decoding helper now checks integrality and the protocol's safe integer range before conversion. It covers camel-case scope/revision counters, snake-case registration scope/version counters, receipt counters including optional `accepted_token_version`, and the device's integer payload version and feed preference. The regression exercises `9007199254740991.1`, `9007199254740990.1`, and `1.0000000000000001` across those fields, plus the valid maximum and absent optional token version. `Makefile` now expects 1259 tests.

| Command | Result | Log |
| --- | --- | --- |
| `rtk swift test --package-path Packages/BabyTracker --filter LiveActivityDTOTests` | RED exit 1: 32 fractional values were accepted after integer coercion | `task-1-fractional-red.log` |
| `rtk swift test --package-path Packages/BabyTracker --filter LiveActivityDTOTests` | GREEN exit 0: 7 tests | `task-1-fractional-green.log` |
| `rtk swift test --package-path Packages/BabyTracker --filter LiveActivityPayloadTests` | GREEN exit 0: 5 tests | `task-1-payload-fix-green.log` |
| `rtk make test` | Exit 2, count 881/1259: `BabyStoreTests` runner aborted with signal 6 | `task-1-fractional-full.log` |
| `rtk make test` | Same runner abort and count on retry | `task-1-fractional-full-retry.log` |
| `rtk proxy env TMPDIR=/tmp/btlap.A29gWK make test` | GREEN exit 0: `OK: 1259 tests` | `task-1-fractional-full-shorttmp.log` |
| `rtk git diff --check` | GREEN exit 0, no output | self-review |

The two abort reports are `~/Library/Logs/DiagnosticReports/swiftpm-testing-helper-2026-10-03-171116.ips` and `...171156.ips`. Both identify `SIGABRT` in the existing BabyStore SwiftData write path during `LiveCardSequenceTests`/`TimerTests`; the crash reports do not establish the underlying cause. The same full command passed with an owned, short `TMPDIR`, so no Task 1 source failure is indicated by the final gate. Swift `JSONDecoder` does not expose the original number text: values beyond Decimal's representable precision cannot be proven integral by this helper. Incoming Go requests retain strict lexical validation, and the server emits integer counters within the safe bound.
