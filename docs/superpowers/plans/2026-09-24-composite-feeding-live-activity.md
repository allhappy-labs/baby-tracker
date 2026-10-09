# Composite Feeding and Live Activity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve an ordered multi-component feeding session and make the running screen and Live Activity accurate, legible, and resilient across app switching.

**Architecture:** `FeedEntry` remains one stable session/sync entity and owns ordered `FeedComponent` values. `ActiveTimer` owns completed components plus one optional timed breast component; SwiftData V3 and protocol payload V2 migrate legacy parallel fields without loss. Views render derived state only, and ActivityKit receives an explicit current-component presentation.

**Tech Stack:** Swift 6, SwiftData, SwiftUI, ActivityKit, App Intents, Swift Testing, Go 1.24+, PostgreSQL migrations and protocol fixtures.

**Spec:** `docs/superpowers/specs/2026-09-24-composite-feeding-live-activity-design.md`

**Status (2026-09-25):** Tasks 1–7 are implemented and ticked on evidence, not
on commit titles — each was checked against the files, interfaces and tests it
names (commits `6bed779`, `e622e82`, `afbb312`, `871eb01`, `1fc2d1c`,
`d5d9df5`, `3875dc7`, `0226448`). Task 7 had shipped without the second half of
its Step 1; that test was added on 2026-09-25 through `ActivityPresentation`.
Task 8 is recorded in `docs/verification/2026-09-24-composite-feeding-live-activity.md`,
including which of its steps need a person holding the phone.

## Global Constraints

- A session preserves ordered components and stable component UUIDs.
- Adding a later bottle never overwrites an earlier bottle.
- Session counts, breast duration, and bottle volume aggregate at session level.
- Legacy V1/V2 local data and protocol V1 data migrate without loss.
- Timer values derive from timestamps and resume after scene reactivation.
- Live Activity foregrounds must remain readable against the fixed dark surface.
- Run `make test`, not bare `swift test`; update `EXPECTED_TESTS` with new tests.
- Preserve the existing uncommitted signing change in `BabyTracker.xcodeproj/project.pbxproj`.

## Review Focus

- A feed alternating breast, bottle, then breast again retains exact order and duration.
- Replaying a V1 timer attach cannot silently collapse two V2 bottles.
- A stale attach sheet creates a standalone bottle without corrupting the stopped session.
- Background/foreground and tab-away/tab-back both restart ticking without duplicate publishers.
- Live Activity remains legible with dark/light system appearance, increased contrast, and accessibility text sizes.

---

### Task 1: Ordered component domain and SwiftData V3 migration

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyCore/FeedComponent.swift`
- Create: `Packages/BabyTracker/Sources/BabyStore/Models/SchemaV3.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/Models/FeedEntry.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/Models/ActiveTimer.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/Models/BabyStoreMigrationPlan.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/Models/Schema.swift`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/FeedComponentMigrationTests.swift`

**Interfaces:**
- Produces: `FeedComponent`, `FeedEntry.components`, `ActiveTimer.completedComponents`, `ActiveTimer.currentBreastSide`, and V2-to-V3 migration.
- Consumes: `BreastSide`, `BottleContents`, `SolidsAmount`, and existing V2 rows.

- [x] **Step 1: Write migration and projection failures**

Add tests that open a V2 fixture containing two sides plus a formula bottle and assert V3 yields three ordered components with stable IDs, plus standalone bottle and solids cases. Assert `totalBreastSeconds`, `totalBottleVolumeML`, `lastSideUsed`, and `isCombo` project from components.

- [x] **Step 2: Run the focused tests and capture RED**

Run: `rtk make test TEST_FILTER=FeedComponentMigrationTests`  
Expected: compile failure because `FeedComponent`, `SchemaV3`, and `components` do not exist.

- [x] **Step 3: Add the component type and compatibility projections**

Implement the exact enum from the spec, including computed `id`, `breastSide`, `durationSeconds`, `volumeML`, and `bottleContents`. Replace stored feed fields with `components`, retaining source-compatible read-only projections while downstream code migrates.

- [x] **Step 4: Add Schema V3 and deterministic migration**

Capture V2 feed and timer rows in `willMigrate`; in `didMigrate`, append legacy side intervals first, then the legacy bottle, and preserve standalone solids. Derive deterministic component UUIDs from the entry/timer UUID plus position so retrying a migration produces the same result.

- [x] **Step 5: Run focused and full store tests**

Run: `rtk make test TEST_FILTER='FeedComponent|Migration'`  
Expected: PASS with no lost legacy values.

- [x] **Step 6: Commit the domain migration**

```bash
rtk git add Packages/BabyTracker/Sources/BabyCore/FeedComponent.swift Packages/BabyTracker/Sources/BabyStore/Models Packages/BabyTracker/Tests/BabyStoreTests/FeedComponentMigrationTests.swift Makefile
rtk git commit -m "feat(feeding): model ordered feed components"
```

### Task 2: Multi-component timer lifecycle

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Feeding.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/TimerIntentReducer.swift`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/BottleSheet.swift`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/ComboFeedTests.swift`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/SharedTimerTests.swift`

**Interfaces:**
- Produces: `appendBottle(volumeML:contents:to:)`, `beginBreast(side:on:)`, and component-preserving `stopFeed`.
- Consumes: Task 1 component storage.

- [x] **Step 1: Add failing sequential-feed tests**

Assert `left 8m -> right 6m -> breast milk 50ml -> formula 30ml` yields four ordered components; the first bottle remains after the second; bottle append closes the current breast interval; and stopping an empty feed is rejected.

- [x] **Step 2: Run RED**

Run: `rtk make test TEST_FILTER='ComboFeed|SharedTimer'`  
Expected: the second bottle replaces the first or the new API is missing.

- [x] **Step 3: Implement append semantics and timer reducer intents**

Replace the single pending bottle with component append. Update reducer operations so remote timer state carries the same component order and stable IDs. Keep stale-sheet fallback as a standalone bottle session.

- [x] **Step 4: Run focused tests GREEN**

Run: `rtk make test TEST_FILTER='ComboFeed|SharedTimer'`  
Expected: PASS, including replay/idempotency cases.

- [x] **Step 5: Commit timer lifecycle**

```bash
rtk git add Packages/BabyTracker/Sources/BabyStore Packages/BabyTracker/Sources/LoggingFeature/BottleSheet.swift Packages/BabyTracker/Tests/BabyStoreTests Makefile
rtk git commit -m "feat(feeding): preserve sequential feed components"
```

### Task 3: Component rendering, editing, and aggregates

**Files:**
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeViewModel.swift`
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/HistoryViewModel.swift`
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/EntryEditModel.swift`
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/EntryEditSheet.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Queries.swift`
- Modify: `Packages/BabyTracker/Sources/InsightsFeature/InsightsViewModel.swift`
- Test: corresponding Home, History, and Insights test targets.

**Interfaces:**
- Produces: ordered component rows and component-ID edit/remove operations.
- Consumes: Task 1 components and Task 2 lifecycle.

- [x] **Step 1: Add failing copy, edit, and aggregate tests**

Pin exact component ordering/copy and assert a four-component session counts as one feed, 80ml total volume, and 14m breast duration. Edit only the formula component and prove the breast-milk component is unchanged.

- [x] **Step 2: Run RED**

Run: `rtk make test TEST_FILTER='HomeFeatureTests|HistoryFeatureTests|InsightsFeatureTests'`

- [x] **Step 3: Migrate readers and editor to components**

Render one session with ordered child rows. Address mutations by component UUID. Remove the session only when no components remain and after the existing confirmation path.

- [x] **Step 4: Run focused tests GREEN**

Run the command from Step 2 and require PASS.

- [x] **Step 5: Commit presentation and editing**

```bash
rtk git add Packages/BabyTracker/Sources/HomeFeature Packages/BabyTracker/Sources/HistoryFeature Packages/BabyTracker/Sources/InsightsFeature Packages/BabyTracker/Sources/BabyStore Packages/BabyTracker/Tests Makefile
rtk git commit -m "feat(feeding): render and edit feed components"
```

### Task 4: Swift protocol payload V2

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyProtocol/WireModels.swift`
- Modify: `Packages/BabyTracker/Sources/BabySync/MutationEncoder.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/EntryPayload.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+RemoteApply.swift`
- Test: `Packages/BabyTracker/Tests/BabyProtocolTests/CanonicalProtocolTests.swift`
- Test: `Packages/BabyTracker/Tests/BabySyncTests/SyncCoordinatorTests.swift`

**Interfaces:**
- Produces: payload-version-2 component DTOs and V1 upgrade decoder.
- Consumes: local `FeedComponent`.

- [x] **Step 1: Add canonical V2 vectors and V1-upgrade failures**

Pin exact JSON field names, component UUID casing, decimal normalization, array order, and forbidden-field combinations per component kind.

- [x] **Step 2: Run RED**

Run: `rtk make test TEST_FILTER='CanonicalProtocol|SyncCoordinator'`

- [x] **Step 3: Implement V2 encode/decode and V1 upgrade**

Emit `payload_version:2` for feed/timer mutations. Decode V1 sides/scalar bottle into deterministic components. Reject unknown component kinds and invalid mixed fields.

- [x] **Step 4: Run focused tests GREEN and commit**

```bash
rtk make test TEST_FILTER='CanonicalProtocol|SyncCoordinator'
rtk git add Packages/BabyTracker/Sources Packages/BabyTracker/Tests Makefile
rtk git commit -m "feat(sync): add feed component payloads"
```

### Task 5: Server compatibility migration and component persistence

**Files:**
- Create: `Server/migrations/000017_feed_components.sql`
- Modify: `Server/internal/migrate/compatibility.json`
- Modify: `Server/deploy/postgres/init-roles.sql`
- Modify: `Server/internal/database/records_schema_test.go`
- Modify: `Server/internal/records/feeds.go`
- Modify: `Server/internal/records/timers.go`
- Modify: `Server/internal/records/snapshot.go`
- Modify: `Server/internal/records/repository.go`
- Test: server protocol, records, overlap, snapshot, and RLS tests.

**Interfaces:**
- Produces: schema 17 `feed_components`/`timer_components` persistence readable by old and new server revisions during overlap.
- Consumes: V2 wire components from Task 4.

- [x] **Step 1: Add the compatibility predecessor tests**

Test schema-16 upgrade, exact privilege snapshot, deterministic backfill order, rollback refusal, and old-server/new-schema overlap before feature code references schema 17.

- [x] **Step 2: Run RED against the migration gate**

Run: `rtk proxy env BT_TEST_RUN='TestFeedComponent|TestMigrationOverlap|TestPrivilege' make server-test`

- [x] **Step 3: Add migration 17 and deployable compatibility**

Create component tables with family-scoped keys, positions, per-kind constraints, forced RLS, and deterministic backfill from sides/scalar bottle/solids. Update manifest to `[16,17]` until feature code lands.

- [x] **Step 4: Prove the predecessor and commit it separately**

```bash
rtk proxy env BT_TEST_RUN='TestFeedComponent|TestMigrationOverlap|TestPrivilege' make server-test
rtk git add Server/migrations/000017_feed_components.sql Server/internal/migrate/compatibility.json Server/deploy/postgres/init-roles.sql Server/internal/database
rtk git commit -m "feat(server): prepare feed component compatibility"
```

- [x] **Step 5: Implement V2 protocol and persistence**

Update canonical validation, records, timers, snapshot/import/export, deletion, storage charging, fixtures, OpenAPI, and receipts to preserve ordered components. Retain V1 decoding and return explicit version errors where downgrade is lossy.

- [x] **Step 6: Run the complete server gate and set manifest `[17,17]`**

Run: `rtk make server-test`  
Run: `rtk make server-smoke`  
Expected: PASS without database skips; schema 17 contiguous; V1/V2 overlap assertions pass.

- [x] **Step 7: Commit server feature**

```bash
rtk git add Server
rtk git commit -m "feat(server): synchronize ordered feed components"
```

### Task 6: Running UI lifecycle, copy, size, and single-surface stop

**Files:**
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeView.swift`
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeViewModel.swift`
- Modify: `Packages/BabyTracker/Sources/HomeFeature/StopDock.swift`
- Modify: `Packages/BabyTracker/Sources/HomeFeature/TimerDial.swift`
- Modify: `Packages/BabyTracker/Sources/BabyUI/Typography.swift`
- Test: `Packages/BabyTracker/Tests/HomeFeatureTests/HomeViewModelTests.swift`
- Test: `UITests/BabyTrackerUITests/RunningTimerLifecycleTests.swift`

**Interfaces:**
- Produces: explicit breast labels, 82pt hero numerals, reconnecting cadence, and one stop surface.

- [x] **Step 1: Add failing state/copy and lifecycle tests**

Assert `Feeding · Left breast`/`Right breast`, background the UI test app for at least three seconds, reactivate it, and require the displayed timestamp-derived value to advance. Add an accessibility-tree assertion that stop exposes one button element.

- [x] **Step 2: Run RED**

Run focused package tests, then the single UI lifecycle test on simulator.

- [x] **Step 3: Implement lifecycle and visual fixes**

Call `reload()` then `applyCadence(cadence)` on appearance and active scene transition; cancel on inactive/background/disappearance. Make the stop button plain inside `GlassDock`, leaving glass as the only material/stroke. Increase `HeroNumeral` to 82.

- [x] **Step 4: Run focused tests GREEN and commit**

```bash
rtk make test TEST_FILTER=HomeFeatureTests
rtk make test-ui TEST_FILTER=RunningTimerLifecycleTests
rtk git add Packages/BabyTracker/Sources/HomeFeature Packages/BabyTracker/Sources/BabyUI/Typography.swift Packages/BabyTracker/Tests/HomeFeatureTests UITests/BabyTrackerUITests Makefile
rtk git commit -m "fix(home): keep running timers live and legible"
```

### Task 7: Component-aware, high-contrast Live Activity

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/TimerActivityAttributes.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/LiveActivityController.swift`
- Modify: `Apps/BabyTrackerWidgets/FeedActivityWidget.swift`
- Modify: `Apps/SharedIntents/TimerIntents.swift`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/LiveActivityControllerTests.swift`
- Test: widget copy/mapping tests.

**Interfaces:**
- Produces: `FeedActivityKind` and one label/symbol/tint mapping used by app and widget.

- [x] **Step 1: Add failing mapping and update tests**

Assert distinct symbols/labels for left breast, right breast, pumped milk, formula, mixed bottle, and sleep; assert switch and bottle append push new ActivityKit content state.

- [x] **Step 2: Run RED**

Run: `rtk make test TEST_FILTER='LiveActivity|ActivityCopy'`

- [x] **Step 3: Implement state and explicit contrast**

Add `FeedActivityKind` to content state. Apply explicit primary/secondary foreground colors and `activitySystemActionForegroundColor` against `Palette.ground`; keep system timer rendering from `startedAt`.

- [x] **Step 4: Run focused tests and build both app targets**

Run the focused test command and `rtk make build`; require PASS.

- [x] **Step 5: Commit Live Activity changes**

```bash
rtk git add Packages/BabyTracker/Sources/BabyStore Apps/BabyTrackerWidgets Apps/SharedIntents Packages/BabyTracker/Tests Makefile
rtk git commit -m "fix(widgets): distinguish and contrast feeding activities"
```

### Task 8: Full verification and device acceptance

**Files:**
- Modify: `docs/verification/2026-09-24-composite-feeding-live-activity.md`

**Interfaces:**
- Consumes: Tasks 1–7.
- Produces: reproducible automated and physical-device evidence.

- [x] **Step 1: Run all automated gates**

Run `rtk make test`, `rtk make server-test`, `rtk make server-smoke`, `rtk make build`, and `rtk make test-ui`. Record exact totals and failures.

- [ ] **Step 2: Install on `Phone A` and reproduce the lifecycle flow** _(installed 2026-09-25; taps pending — see verification doc)_

Start a left feed, wait, switch to another app, return, switch right, add pumped milk, add formula, and stop. Verify timestamps, order, totals, and editing.

- [ ] **Step 3: Inspect Lock Screen and Dynamic Island in dark mode**

Capture evidence for each component label/icon, contrast, elapsed timer, switch action, and stop action.

- [x] **Step 4: Record completion honestly and commit evidence**

```bash
rtk git add docs/verification/2026-09-24-composite-feeding-live-activity.md
rtk git commit -m "docs: verify composite feeding on device"
```

