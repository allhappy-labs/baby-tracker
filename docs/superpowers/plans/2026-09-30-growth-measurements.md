# Growth Measurements and WHO Insights Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let parents log three growth measurements from Home and inspect dated trends and on-device WHO percentiles in Insights and Growth details.

**Architecture:** Two new synced entities hold one child's growth reference profile and dated measurement check-ins. A pure calculator consumes bundled WHO reference data and stored dates/values; Growth UI reads its results without syncing derived percentiles. The server extends the existing canonical-record, capability-boundary, cascade, and export paths.

**Tech Stack:** Swift 6.2, SwiftData, SwiftUI, Swift Charts, Swift Testing, Go, PostgreSQL 17.

**Spec:** `docs/superpowers/specs/2026-09-30-growth-measurements-design.md`

## Global Constraints

- Preserve unrelated dirty work. Create an isolated worktree with `superpowers:using-git-worktrees` at execution time; reconcile the branch against the current main checkout before implementation. Use `rtk` before shell commands and Conventional Commits.
- First release: weight in kg, length/height and head circumference in cm; one check-in holds any nonempty combination. No custom types, alerts, clinical labels, or chart for fewer than two readings of that type.
- New-baby creation requires the WHO girls' or boys' reference choice; legacy babies remain usable without a profile. Optional due date corrects age through the third chronological birthday. Values/percentiles are calculated locally and never synced.
- Home's **Measure** control is the only create route. Insights adds one weight card and an **Open growth details** button below it; keep the existing insight stamp focused on its current insight. Keep the approved dark card design, localize all new copy in every supported app language, and make VoiceOver and Dynamic Type usable.
- Local-only baby deletion retains Growth records; shared-family owner deletion tombstones them with the existing atomic child cascade. Do not deploy, push, or merge without the user's separate authorization.
- Do not package WHO tables for distribution until the exact source and reuse rights are documented and any required permission is secured. Preserve attribution and a version/hash manifest.
- PostgreSQL migration `000023_growth.sql` follows the current `000022_feed_sessions.sql`; recheck the latest migration number in the execution worktree before creating it.

## File map and interfaces

- `Packages/BabyTracker/Sources/BabyCore/GrowthTypes.swift`: `GrowthReferenceSex { girls, boys }`, `LengthPosition { lying, standing }`, `GrowthMetric { weight, lengthHeight, headCircumference }`, and `MeasurementValues(weightKG: Double?, lengthCM: Double?, headCircumferenceCM: Double?, lengthPosition: LengthPosition?)`; no storage or UI dependency.
- `Packages/BabyTracker/Sources/BabyCore/GrowthProfileID.swift`: `GrowthProfileID.forBaby(_ babyID: UUID) -> UUID`, a stable UUIDv5 with the baby UUID as namespace and `growth-profile-v1` as the name, so two devices filling a legacy profile address the same entity.
- `Packages/BabyTracker/Sources/BabyStore/Models/{GrowthProfile,MeasurementCheckIn,SchemaV7}.swift`, `BabyStore+Growth.swift`: storage and atomic CRUD; `GrowthProfile` holds `babyID`, `referenceSexRaw`, `expectedDueDateISO`; `MeasurementCheckIn` holds `babyID`, `measuredAt`, `timeZoneID`, immutable `loggedAt`, three optional values, and `lengthPositionRaw`.
- `Packages/BabyTracker/Sources/GrowthFeature/{GrowthReferenceCalculator,GrowthViewModel,GrowthView,GrowthCardView}.swift` plus `Resources/WHO/` and a localization catalog: pure percentile/reference calculation and presentation. `GrowthFeature` depends on BabyCore, BabyStore, BabyUI, and LoggingFeature for its edit sheet; InsightsFeature depends on GrowthFeature, never the reverse.
- `Packages/BabyTracker/Sources/LoggingFeature/MeasurementSheet.swift`: input and edit sheet. Home/RootView dispatch `.measurement` without adding a case to `Tracker`, which also controls History filters and widget routing.
- `Server/migrations/000023_growth.sql`, `Server/internal/{protocol,records,sync,export,httpapi}` and `Server/api/openapi.yaml`: two canonical kinds, `growth_profile` and `measurement`, plus `X-Baby-Tracker-Growth: v1` marker and advertised `growth-v1` support. The marker follows the existing supplement boundary because sync route capability arrays have an exact-match contract.
- Wire payload v1: profile `{baby_id, reference_sex, expected_due_date}`; measurement `{baby_id, measured_at, logged_at, time_zone_id, weight_kg, length_cm, head_circumference_cm, length_position}`. Optional fields are explicit JSON null on a full-state update. Profile ID is deterministic; check-in ID is a fresh UUID. Server enforces one profile per family/baby and immutability of `logged_at` on edit.

## Review Focus

- Same measurement viewed in another time zone: the stored time zone determines its calendar date and percentile; Task 1 pins this in a test.
- Two readings only for different types: neither type gets a line chart; Task 7 tests per-type counts.
- Clearing one value on an edited check-in: the other phone receives JSON null and removes that value; Task 4 tests remote apply.
- Two caregivers filling a legacy profile offline: one deterministic entity ID converges under normal version/conflict rules; Tasks 2 and 4 test it.
- A shared-family deletion racing a growth edit: the atomic cascade wins or the late edit receives `baby_deleted`, while local-only deletion retains rows; Tasks 3 and 4 test both.

---

### Task 1: WHO source, rights, and reference calculator

**Files:** Create `BabyCore/{GrowthTypes,GrowthProfileID}.swift`, `GrowthFeature/GrowthReferenceCalculator.swift`, `GrowthFeature/Resources/WHO/*`, `GrowthFeature/WHOReferenceManifest.swift`, `Tests/BabyCoreTests/GrowthProfileIDTests.swift`, `Tests/GrowthFeatureTests/GrowthReferenceCalculatorTests.swift`, and `docs/research/2026-09-30-who-growth-reference-sources.md`; modify `Package.swift`.

**Interfaces:** `GrowthProfileID.forBaby(_ babyID: UUID) -> UUID`; `GrowthReferenceCalculator.result(metric: GrowthMetric, value: Double, position: LengthPosition?, measuredAt: Date, timeZoneID: String, birthDate: LocalDate, expectedDueDate: LocalDate?, referenceSex: GrowthReferenceSex) -> GrowthReferenceResult`. `GrowthReferenceResult` has `available(percentile: Double, median: Double, p3: Double, p97: Double, correctedAge: Bool)` and `unavailable(GrowthReferenceUnavailableReason)` cases. Input values remain unchanged.

- [ ] **Step 1: Record source and rights.** Identify the exact official WHO weight, length/height, and head circumference reference tables for girls and boys, their versions and SHA-256 hashes; document attribution and distribution rights. If rights cannot be confirmed for the intended distribution, stop before bundling tables and report the blocker.
- [ ] **Step 2: Write failing golden tests.** Assert published WHO median/percentile examples for each metric and sex; test the cross-device UUIDv5 profile ID, birth, corrected age before due date, third chronological birthday, 24-month position conversion of ±0.7 cm, 60-month limit, invalid table rows, and measured date stability across viewer time zones.
- [ ] **Step 3: Run `rtk swift test --filter 'GrowthReferenceCalculatorTests|GrowthProfileIDTests'` from `Packages/BabyTracker`.** Expect the missing calculator/target failure.
- [ ] **Step 4: Add the core types, stable ID, table loader, and calculator.** Bundle only the six required series; derive age in days from the measured timestamp's stored time zone, interpolate reference parameters as the chosen WHO source requires, use its LMS z-score and normal CDF for percentile, and return unavailable for unsupported ages/data.
- [ ] **Step 5: Run the focused tests until green; commit only Task 1 files** with `feat(growth): calculate WHO reference percentiles`.

### Task 2: Server growth records and profile/check-in contract

**Files:** Create `Server/migrations/000023_growth.sql`, `Server/internal/protocol/growth_profile_id.go`, `Server/internal/records/{growth_profiles,measurements}.go`, fixtures `Server/api/fixtures/v1/{growth-profile,measurement}.json`, and focused protocol/record/importer tests. Modify `Server/internal/protocol/{auth,entities,mutations,canonical,sync}.go`, `Server/internal/records/{repository,validation,snapshot}.go`, `Server/internal/importer/{manifest,resolver}.go`, and `Server/api/openapi.yaml`.

**Interfaces:** `protocol.GrowthProfileID(babyID string) string` matches Task 1's UUIDv5 result. Canonical kinds `growth_profile` and `measurement` use payload version 1 and the File map's fields. `reference_sex` is `girls|boys`; `length_position` is `lying|standing` when and only when `length_cm` is present; numeric values are finite and positive; at least one measurement is non-null.

- [ ] **Step 1: Write failing protocol, record, and import tests.** Cover exact JSON/hash fixtures, null clearing, invalid type/sex/position/date/value, wrong-family baby, immutable `logged_at`, one profile per baby, two clients addressing the same profile ID, and local-to-shared import with a baby mapped to a different UUID.
- [ ] **Step 2: Run `rtk go test ./internal/protocol -run 'TestGrowth|TestMeasurement' -count=1` from `Server`.** Expect missing-kind/shape failures.
- [ ] **Step 3: Implement the v1 wire contract and migration.** Extend both entity switches and PostgreSQL RLS/privilege manifest, family/baby indexes, journal/snapshot/import kind checks, and version 23; add repository get/put/validation/snapshot paths. Growth content hashes exclude `baby_id`, as for other imported records. Import admission derives the profile ID from the mapped baby ID using the same UUIDv5 rule, compares an existing shared profile, and returns a resolver conflict for differing values rather than overwriting them.
- [ ] **Step 4: Run focused protocol tests and `rtk proxy env BT_TEST_RUN='TestGrowth|TestMeasurement' BT_TEST_PACKAGES='./internal/records ./internal/sync ./internal/importer' bash Server/scripts/test-db.sh` from repo root.** Expect all new tests green in disposable PostgreSQL.
- [ ] **Step 5: Commit scoped files** with `feat(growth): persist synced growth records`.

### Task 3: Server compatibility, cascade, and archive

**Files:** Create `Server/internal/sync/growth_boundary.go` and tests; modify `Server/internal/{httpapi/sync.go,sync/paging.go,sync/snapshot.go,sync/mutation.go,records/child_delete.go,export/archive.go,export/repository.go}`, `Server/api/openapi.yaml`, and relevant tests.

**Interfaces:** A legacy client missing `X-Baby-Tracker-Growth: v1` receives `409 unsupported_capability` after the first profile or check-in exists, before a cursor advances. The marker is checked on each sync and snapshot page, including continuation races. The owner archive contains `growth_profiles` and `measurements` arrays of raw canonical entities.

- [ ] **Step 1: Write failing tests** for first growth write racing response capture, old-client sync/snapshot continuations, new-client readback, shared-family child cascade ordering and rollback, late mutation rejection, and archive inclusion/tombstones.
- [ ] **Step 2: Run focused tests under disposable PostgreSQL** with `rtk proxy env BT_TEST_RUN='TestGrowth' BT_TEST_PACKAGES='./internal/httpapi ./internal/sync ./internal/records ./internal/export' bash Server/scripts/test-db.sh`; expect the new cases to fail.
- [ ] **Step 3: Add marker parsing and locked capability checks** following `supplement_boundary.go`; extend snapshot, journal, cascade, usage accounting, archive size accounting, and OpenAPI. Preserve the existing exact-match sync capability arrays while advertising `growth-v1` in server info.
- [ ] **Step 4: Run the focused disposable-DB tests until green; commit scoped files** with `feat(growth): protect and export growth sync data`.

### Task 4: SwiftData growth store and family sync

**Files:** Create `BabyStore/Models/{GrowthProfile,MeasurementCheckIn,SchemaV7}.swift`, `BabyStore/BabyStore+Growth.swift`, and matching `BabyProtocolTests`, `BabyStoreTests`, `BabySyncTests`, `FamilySyncClientE2ETests`. Modify `BabyStore/Models/{Schema,SchemaV2,SchemaV3,SchemaV4,SchemaV5,SchemaV6,BabyStoreMigrationPlan}.swift`, `BabyStore/{EntryPayload,MutationFactory,BabyStore+Babies,BabyStore+Outbox,BabyStore+RemoteApply,BabyStore+Import,ImportManifestSnapshot}.swift`, `BabyProtocol/{WireModels,ContentHash}.swift`, and `BabySync/FamilySyncHTTPClient.swift`.

**Interfaces:** `BabyStore.addBaby(name: String, birthDate: Date, referenceSex: GrowthReferenceSex, expectedDueDate: LocalDate?, expectedWriteContext: RealmWriteContext? = nil) throws -> Baby`; `growthProfile(for: UUID) throws -> GrowthProfile?`; `setGrowthProfile(for: UUID, referenceSex: GrowthReferenceSex, expectedDueDate: LocalDate?) throws`; `updateBabyAndGrowthProfile(_ baby: Baby, name: String, birthDate: Date, referenceSex: GrowthReferenceSex?, expectedDueDate: LocalDate?) throws` saves both atomically, with nil sex leaving a legacy baby's absent profile untouched; `logMeasurement(for: UUID, at: Date, timeZoneID: String, values: MeasurementValues) throws -> MeasurementCheckIn`; `editMeasurement(_ entry: MeasurementCheckIn, at: Date, timeZoneID: String, values: MeasurementValues) throws`; `measurements(for: UUID) throws -> [MeasurementCheckIn]`. Preserve the `loggedAt` assigned at creation.

- [ ] **Step 1: Write failing model/store/sync tests.** Cover V6→V7 migration with absent legacy profile and unchanged V2–V6 model lists, atomic baby/profile creation and combined baby/profile edit rollback, realm isolation, offline CRUD, same-day ordering, invalid/partial values, immutable logging time, deterministic profile ID, remote null clearing, snapshot/outbox replay, local-only baby hiding retaining growth rows, and local-to-shared import remapping a profile ID and baby ID without changing measurement IDs.
- [ ] **Step 2: Run `rtk swift test --filter 'Growth|Measurement'` from `Packages/BabyTracker`;** expect missing models/APIs.
- [ ] **Step 3: Freeze the V2–V6 model lists and add SchemaV7.** Adding Growth models must not retroactively change a historical schema's `models` array; pass the pre-growth store migration test before continuing.
- [ ] **Step 4: Add models, store APIs, payloads, and remote materialization.** Include both growth kinds in import/realm transition and shared-realm purge paths; after baby mapping, rederive the profile ID for the canonical baby before sending the imported mutation, while preserving check-in IDs. Send the growth marker in `FamilySyncHTTPClient`. Update every `addBaby` call site and fixture rather than leaving a default sex that bypasses the required creation choice.
- [ ] **Step 5: Run focused Swift tests until green and a focused two-client sync test** using `rtk make server-protocol-e2e` from repo root; commit scoped files with `feat(growth): sync local profiles and measurements`.

### Task 5: Required growth reference in baby creation and settings

**Files:** Modify `BabyFeature/AddBabySheet.swift`, `SettingsFeature/BabyEditSheet.swift`, their localization catalogs and tests; create `UITests/BabyTrackerUITests/GrowthFlowUITests.swift`; update test/preview baby factories in `Apps/BabyTrackerApp/AppEnvironment.swift`, `Apps/BabyTrackerApp/UITestSupport.swift`, and affected test fixtures.

**Interfaces:** New baby Save remains disabled until name and `GrowthReferenceSex` are selected. Optional due date is date-only. Existing babies without a profile show an explicit reference-choice prompt in Growth and an editable field in Baby settings; measurements remain readable without percentiles.

- [ ] **Step 1: Write failing creation/edit tests** for required choice, legacy baby, optional due-date input, and failed save remaining open.
- [ ] **Step 2: Run `rtk swift test --filter 'BabyFeatureTests|SettingsFeatureTests'` from `Packages/BabyTracker`;** expect missing controls/APIs.
- [ ] **Step 3: Add the selection and due-date controls** with localized, clear wording and update all creation call sites. Save name, birth date, and profile edits through Task 4's atomic combined store API; an existing baby without a profile may still save unrelated edits. Keep old records when a legacy baby has no profile.
- [ ] **Step 4: Run focused tests and an iOS simulator build** with `rtk make build`; commit scoped files with `feat(growth): require reference at baby creation`.

### Task 6: Home Measure entry and check-in editing

**Files:** Create `LoggingFeature/MeasurementSheet.swift` and tests; modify `LoggingFeature/LogCoordinator.swift`, `HomeFeature/{QuickLogDock,StopDock,HomeView}.swift`, `Apps/BabyTrackerApp/RootView.swift`, their localization catalogs, and `UITests/BabyTrackerUITests/GrowthFlowUITests.swift`.

**Interfaces:** `MeasurementSheetMode { create(babyID: UUID), edit(entry: MeasurementCheckIn) }` selects the same sheet's save action. `.measurement` defaults to now, accepts any nonempty combination of three values in kg/cm, captures lying/standing for length, and calls Task 4 store APIs. `QuickLogDock` gets a fourth secondary action and `StopDock` gets the same Measure menu action. `Tracker` remains unchanged, so History filters and widgets do not acquire a phantom measurement kind.

- [ ] **Step 1: Write failing form and rendered-dock tests** for no values, one or multiple values, future/pre-birth date, backdating, invalid numeric text, lying/standing requirement, failed save staying open, and fourth-action accessibility at compact/Dynamic Type sizes.
- [ ] **Step 2: Run `rtk swift test --filter 'Measurement|QuickLogDock'` from `Packages/BabyTracker`;** expect missing sheet/action failures.
- [ ] **Step 3: Implement sheet, localized copy, and Home routing.** Reuse the sheet for editing from Growth details; keep create entry out of Insights/Growth details. Do not modify `Tracker.allCases`.
- [ ] **Step 4: Run focused tests, `rtk make build`, and `rtk xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests/GrowthFlowUITests` from repo root;** commit scoped files with `feat(growth): log measurement check-ins from Home`.

### Task 7: Growth summaries and chart state

**Files:** Create `GrowthFeature/{GrowthViewModel,GrowthView,GrowthCardView}.swift`, `GrowthFeature/Localizable.xcstrings`, `Tests/GrowthFeatureTests/{GrowthViewModelTests,GrowthViewTests}.swift`; modify `InsightsFeature/{InsightsView,InsightsViewModel}.swift`, `Package.swift`, `UITests/BabyTrackerUITests/GrowthFlowUITests.swift`, and focused Insights tests.

**Interfaces:** `GrowthViewModel` consumes Task 4's profile/check-ins and Task 1's calculator; it exposes per-metric latest value, previous change, percentile, and `chartPoints` only when that metric has at least two readings. `GrowthCardView` displays weight only and calls `onOpenDetails`; `GrowthView` displays each metric's own chart/history and offers edit/delete on existing check-ins, never Add.

- [ ] **Step 1: Write failing state and rendered-view tests** for 0/1/2 readings per metric, weight-only Insights chart, date/value/percentile accessibility, unavailable-percentile states, recalculation after a birth/due/reference edit, and details navigation from the button below the card.
- [ ] **Step 2: Run `rtk swift test --filter 'GrowthView|GrowthViewModel|InsightsFeatureTests'` from `Packages/BabyTracker`;** expect missing view/model behavior.
- [ ] **Step 3: Implement the approved card and Growth details** with Swift Charts, source-attributed WHO median/band when available, and store-revision refresh. Ensure a reading with one point has no chart and a metric with zero points has no fabricated data.
- [ ] **Step 4: Run focused tests, `rtk make build`, and targeted simulator UI tests;** commit scoped files with `feat(growth): show charts and WHO insights`.

### Task 8: Whole-feature verification and review

**Files:** Update `TASKS.md` and create `docs/verification/2026-09-30-growth-measurements.md` with commands, outcomes, source/rights status, and remaining device/release limits.

- [ ] **Step 1: Run full project gates once assembled:** `rtk make test` (update `EXPECTED_TESTS` only for the actual new test count), `rtk make server-test`, `rtk make server-build`, `rtk make server-protocol-e2e`, `rtk make build`, and the targeted then full simulator UI suite when available.
- [ ] **Step 2: Exercise a local and shared two-caregiver scenario** from baby creation through two check-ins, Insights→Growth details, edit/delete, offline replay, archive export, and legacy-client rejection. Inspect a pre-growth store migration and compact/Dynamic Type screens. Try an installed-iPhone build and flow when a phone is available; record any check that cannot run.
- [ ] **Step 3: Review the complete diff** against the spec, five Review Focus cases, localization catalogs, protocol fixtures, data rights, migration order, and protected dirty work. Resolve findings and rerun only affected gates.
- [ ] **Step 4: Record evidence and commit scoped verification files** with `docs(growth): record feature verification`; present branch handoff without deployment, push, or merge.
