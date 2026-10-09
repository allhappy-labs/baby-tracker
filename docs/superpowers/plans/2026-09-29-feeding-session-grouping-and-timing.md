# Feeding Session Grouping and Step Timing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let caregivers correct start and end times for breast and bottle steps and choose whether adjacent feed records form one feeding session.

**Architecture:** Preserve each `FeedEntry` and component ID. An optional `feedingSessionID` links entries; an unlinked entry uses its own ID as its effective session ID. Store optional step boundaries on breast and bottle components, carry both features through feed and timer protocol v4 and server schema 22, and derive display and counts from a shared session projection.

**Tech Stack:** Swift 6.2, SwiftData, SwiftUI, Foundation Models context, Go, PostgreSQL, existing family sync protocol.

**Spec:** `docs/superpowers/specs/2026-09-29-feeding-session-grouping-and-timing-design.md`

## Global Constraints

- Suggest Combine only after a feed was saved by Stop, when the previous session ended at most **60 minutes** before this feed started. Keep separate by default; diaper and supplement do not suppress, sleep does.
- Preserve feed/component IDs, notes, durations, measured bottle volume, and diaper records. Breastfeeding has no measured volume. Never infer old multi-step boundaries.
- Manual Combine may exceed 60 minutes. History day and daily feed count use the first member's start; rolling Ask count uses any member in the window. Attribute volume to the timed bottle step's day or the parent feed's day when untimed.
- Old clients may neither mutate a v4 feed/timer nor advance sync cursors beyond an unsupported shape. Local linking is atomic and sends ordinary feed update mutations.
- Keep the current zero-duration breast rejection, family/baby ownership checks, and no-future edited-time rule.

## Review Focus

- A diaper in a 20-minute feed gap leaves Combine available; a sleep in the same gap removes the suggestion (Task 3 test).
- A three-member session crossing midnight counts on its first feed day while a later bottle's volume counts on the bottle step day (Task 5 test).
- A v3 client attempting to edit a v4 feed or resume past one cannot erase links or times (Task 2 test).
- A legacy multi-step feed with only aggregate start/end keeps every step's bounds unknown after migration and a note edit (Tasks 1 and 4 tests).
- Two caregivers grouping and correcting the same feed concurrently get a conflict or coherent winner, never deleted or duplicated feeds (Task 7 test).

---

## File structure

- `BabyCore/FeedComponent.swift`: optional component boundaries and stable IDs.
- `BabyStore/Models/FeedEntry.swift`, `SchemaV6.swift`, migration plan, `BabyStore+Feeding.swift`: persisted link/bounds and timer-created bounds.
- New `BabyStore/FeedingSessions.swift`: effective IDs, ordered grouping, candidate selection, atomic Combine/Separate; shared by Home, History, Insights, Ask.
- `BabyProtocol/WireModels.swift`, `ContentHash.swift`, `BabyStore/MutationFactory.swift`, remote apply and sync: v4 JSON and canonicalization.
- `Server/internal/protocol`, `records`, `sync`, and `migrations/000022_feed_sessions.sql`: v4 storage, validation, readback, and compatibility boundary.
- `HistoryFeature` and `HomeFeature`: editing, expanded grouped rows, post-Stop suggestion.
- `InsightsFeature` and `AskFeature`: session counts, correctly dated volume, original source links.

### Task 1: Persist component times and session links locally

**Files:** Modify `Packages/BabyTracker/Sources/BabyCore/FeedComponent.swift`, `Packages/BabyTracker/Sources/BabyStore/Models/FeedEntry.swift`, `Schema.swift`, `BabyStoreMigrationPlan.swift`, and component constructors/pattern matches in `BabyStore`; create `Packages/BabyTracker/Sources/BabyStore/Models/SchemaV6.swift`; test in `BabyCoreTests/FeedComponentCodableTests.swift` and `BabyStoreTests/SchemaMigrationTests.swift`.

**Interfaces:** `FeedComponent.breast(id:side:seconds:startedAt:endedAt:)` and `.bottle(id:volumeML:contents:seconds:startedAt:endedAt:)` add optional `Date` bounds; `FeedComponent.startedAt`/`.endedAt` expose them. `FeedEntry.feedingSessionID: UUID?`; `FeedEntry.effectiveFeedingSessionID: UUID` returns `feedingSessionID ?? id`.

- [ ] Add tests: encode/decode known and absent bounds; migrate V5 store to V6 with unlinked feeds and legacy multi-step bounds still nil; a note edit does not populate those bounds.
- [ ] Run `swift test --package-path Packages/BabyTracker --filter 'FeedComponentCodableTests|SchemaMigrationTests'`; confirm the new tests fail.
- [ ] Add optional associated values and a lightweight V5→V6 migration; update all existing enum pattern matches/constructors without changing their old behavior.
- [ ] Run the same filtered tests; expect pass, then commit `feat(feeding): persist step times and session links`.

### Task 2: Add feed and timer protocol v4, server schema 22, and legacy protection

**Files:** Modify `Packages/BabyTracker/Sources/BabyProtocol/{WireModels,ContentHash}.swift`, `Packages/BabyTracker/Sources/BabyStore/{MutationFactory,BabyStore+RemoteApply,BabyStore+RemoteTimers}.swift` and sync boundary code; modify `Server/internal/protocol/{entities,canonical,mutations}.go`, `Server/internal/records/{feeds,timers,validation,snapshot,repository}.go`, `Server/internal/sync/timed_bottle_boundary.go`; create `Server/migrations/000022_feed_sessions.sql`; update `Server/internal/migrate/compatibility.json`, schema privilege manifests, and protocol fixtures with the repository's generators. Test `BabyProtocolTests/CanonicalProtocolTests.swift`, `BabySyncTests/FamilySyncEngineTests.swift`, `Server/internal/{protocol,records,sync}` tests.

**Interfaces:** v4 feed JSON always includes `feeding_session_id` (nullable) and may include component `started_at`/`ended_at` (nullable); v4 timer completed components carry the same bounds. New client feed and timer writes use payload version 4, including when a link is cleared. Server `FeedPayload.FeedingSessionID *string`, `FeedComponent.StartedAt/EndedAt *string`; server readback retains exact component order and IDs.

- [ ] Add failing Swift and Go tests for v4 canonical bytes, mutation/readback, v3→v4 migration, time validation, a v3 write against a v4 entity, and unsupported cursor advancement. Cover a v4 feed whose link is cleared and whose bounds remain known.
- [ ] Run the focused Swift protocol/sync tests and `go test ./internal/protocol ./internal/records ./internal/sync ./internal/migrate` from `Server`; confirm the new tests fail.
- [ ] Implement v4 feed/timer encoding, canonicalization, DB columns, migration, readback, ownership/time validation, and one-way capability checks. Preserve v1–v3 projection and avoid assigning timestamps to old components.
- [ ] Run the same focused suites and migration/privilege checks; expect pass, then commit `feat(sync): preserve feeding sessions and step times in v4`.

### Task 3: Suggest and manage logical sessions

**Files:** Create `Packages/BabyTracker/Sources/BabyStore/FeedingSessions.swift`; modify `BabyStore+Feeding.swift`, `HomeFeature/{HomeViewModel,HomeView}.swift` and Home localization; test `BabyStoreTests/FeedingSessionsTests.swift`, `HomeFeatureTests/FeedActionTests.swift`.

**Interfaces:** `FeedingSession` represents ordered same-baby member feeds with a stable effective ID. `BabyStore.combineFeeds(_ feedIDs: [UUID], for babyID: UUID) throws`, `BabyStore.separateFeed(at feedID: UUID, for babyID: UUID) throws`, and `BabyStore.combineCandidate(after savedFeedID: UUID, within: TimeInterval = 3_600) throws -> FeedingSession?`. `HomeViewModel` exposes a pending suggestion only after `stopFeed` returns a saved feed; dismissing it does not mutate records.

- [ ] Add failing tests for a second breast/bottle/mixed feed within 60 minutes, exactly 60 minutes, 61 minutes, diaper/supplement/sleep in the gap, cancelled Stop, Keep separate, failed Combine, manual Combine outside the window, and Separate at a middle member.
- [ ] Run `swift test --package-path Packages/BabyTracker --filter 'FeedingSessionsTests|FeedActionTests'`; confirm the new tests fail.
- [ ] Implement deterministic same-baby candidate selection, atomic link updates with normal mutation logs, and the post-Stop suggestion with Keep separate as the default action. Never retry a dismissed suggestion on refresh.
- [ ] Run the focused tests; expect pass, then commit `feat(feeding): suggest and manage linked sessions`.

### Task 4: Edit individual breast and bottle boundaries

**Files:** Modify `Packages/BabyTracker/Sources/BabyStore/BabyStore+Feeding.swift`, `Packages/BabyTracker/Sources/HistoryFeature/{EntryEditModel,EntryEditSheet}.swift` and History localization; test `BabyStoreTests/ComboFeedTests.swift`, `HistoryFeatureTests/EntryEditTests.swift`.

**Interfaces:** `EntryEditModel` exposes `componentStartAt: Date?`, `componentEndAt: Date?`, and setters for the selected component ID. Saving recomputes that component's `seconds` only when both bounds are known; a manual bottle may keep `endedAt == nil`.

- [ ] Add failing tests for exact boundaries from side switch, bottle finish and Stop; independent start/end correction; overlap/reverse/future/zero-breast rejection; manual bottle's optional end; preserving unknown old multi-step bounds and untouched step volume/duration.
- [ ] Run `swift test --package-path Packages/BabyTracker --filter 'ComboFeedTests|EntryEditTests'`; confirm new tests fail.
- [ ] Populate timer component bounds at the event that closes each step, expose sleep-style date/time controls per selected component, validate edited bounds, and adjust the parent envelope without filling breaks.
- [ ] Run focused tests and `make build`; expect pass, then commit `feat(history): edit breast and bottle step times`.

### Task 5: Render groups and count them consistently

**Files:** Modify `Packages/BabyTracker/Sources/BabyStore/{FeedingSessions,BabyStore+Queries}.swift`, `HistoryFeature/{HistoryViewModel,HistoryView,Localizable.xcstrings}`, `InsightsFeature/InsightsViewModel.swift`, `AskFeature/AskFactBuilder.swift`; test `HistoryFeatureTests/HistoryViewModelTests.swift`, `InsightsFeatureTests/InsightsViewModelTests.swift`, `AskFeatureTests/{AskFactBuilderTests,AskLogToolTests}.swift`, `HomeFeatureTests/HomeViewModelTests.swift`.

**Interfaces:** `FeedingSession` contains ordered source `FeedEntry` IDs, earliest start, latest end, summed active seconds, and summed measured bottle volume; `BabyStore.feedingSessions(for:from:to:) throws -> [FeedingSession]` includes a group when any member is in the window. Daily callers bucket by earliest member; Ask rolling callers use window membership and retain individual `AskSource.entry` links.

- [ ] Add failing tests for two- and three-feed rows, expandable ordered steps and break-aware clock span, Combine/Separate count reversibility, midnight first-feed anchoring, step-day bottle volume, same-baby isolation, and rolling Ask count/source links.
- [ ] Run focused History, Insights, Ask and Home suites; confirm new tests fail.
- [ ] Build the shared projection and use it for feed counts while retaining original feeds for details and source links. History Combine/Separate controls call Task 3 store APIs; diapers remain independent rows.
- [ ] Run focused suites; expect pass, then commit `feat(feeding): show sessions across history and insights`.

### Task 6: Verify two-caregiver sync and complete the branch

**Files:** Modify `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/TwoCaregiverSyncE2ETests.swift`, `Server/internal/sync/integration_test.go`, `Server/internal/migrate/compatibility_test.go`, the protocol fixture corpus/manifest, `Makefile` expected test count, and `TASKS.md`.

**Interfaces:** No new product interface. Use the existing two-caregiver harness and conflict semantics; no silent last-writer flattening.

- [ ] Add failing E2E assertions that two caregivers receive the same grouped feeds and corrected component times; simultaneous edits resolve coherently; an old client cannot erase either field or skip it with its cursor.
- [ ] Run the new E2E and server sync tests; confirm new assertions fail before completing any remaining wiring.
- [ ] Finish client/server sync wiring, regenerate and verify protocol fixtures, set `Makefile`'s `EXPECTED_TESTS` to the verified count, and update `TASKS.md` with actual results.
- [ ] Run `make test`, `make server-test`, `make server-protocol-e2e`, and `make build`; run the schema-21→22 migration overlap script with verified refs; build a signed device app and exercise edit and suggestion flows on an installed iPhone. Inspect the final diff and perform whole-branch review before merge.
- [ ] Commit `test(feeding): verify grouped sessions across caregivers`; merge to local `main` only after review defects are fixed. Treat server migration/deployment and phone installation as separate external rollouts requiring their own live evidence and authorization.
