# History Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace History with the approved one-row-per-session list, a session sheet with Join / Split here / Add part, and edit modes on the logging sheets, so the generic editor can go.

**Architecture:**
- The store gains a few session calls: add a part to a session, delete a session, find the previous session, and check a completed part's start.
- LoggingFeature gains edit modes on its sheets, plus a new session sheet. Both are reached through new `LogSheet` cases.
- HistoryFeature becomes a list that emits `HistoryAction`. `RootView` maps each action to the matching sheet, the same pattern as Home's ruling R4.
- No data-format, sync or server change.

**Tech Stack:** Swift 6, SwiftUI, SwiftData, Swift Testing (package tests), XCTest (UI tests), String Catalogs.

**Spec:** `docs/superpowers/specs/2026-10-03-history-redesign-design.md`. Read its "Amendments from the code survey" section too. Mockups: `mockups/2026-10-03-history-redesign/`.

## Global Constraints

- **Workspace.** Work in a worktree on branch `feat/history-redesign`, cut from `main` at `add33b7` or later.
- **Simulator.** Use only `platform=iOS Simulator,id=<simulator-udid>`. Never create, clone or erase simulators.
- **Copy rule.**
  - Times only: no "ago", and no "just now" on these screens.
  - No title words that repeat the icon ("Feeding session", "Diaper", "Med").
  - Every new user-facing string goes in the module's `Localizable.xcstrings` with de, fr, es, it and uk.
- **Localization gate.**
  - Before Task 1, generate the baseline from the branch start: `python3 scripts/verify-localizations.py > work/loc-baseline.txt 2>&1 || true`.
  - The gate is `comm -13 <(sort work/loc-baseline.txt) <(python3 scripts/verify-localizations.py 2>&1 | sort)`, and it must print nothing.
- **Module boundaries.**
  - HistoryFeature must not import LoggingFeature or HomeFeature.
  - LoggingFeature must not import HistoryFeature or HomeFeature.
- **Shared icons.** Use `BabyIconView` / `TrackerIcon` and `Palette` colours. Use `.sheetHeight(_:)` for sheet heights.
- **Gates.** Run every gate through `rtk proxy`, write output to a log file, and report the real exit code. Never pipe a gate through `tail`. Keep `EXPECTED_TESTS` in the Makefile in step; it is 1247 at the branch start.
- **Commits.** Use conventional messages ending with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.
- **Production.** Never touch production.

## Rulings (controller, from the code survey)

| # | Ruling | Cost if wrong |
|---|---|---|
| H-R1 | Growth rows are merged from `store.measurements(for:)` by `measuredAt`, and the filter gains Growth. A Growth row opens `MeasurementSheet(mode: .edit)`, which gains Delete. | One more filter toggle |
| H-R2 | A part is a `FeedComponent`, or a whole sleep. "Split here" shows only between parts of different member entries. Deleting a component calls the store's component removal; deleting the last component of an entry deletes the entry. | Old one-entry combos cannot be split, which is the existing limitation |
| H-R3 | No field is lost. Diaper edit mode offers Dry. The sleep editor has Nap / Night / Auto. Feed part and sleep editors have a Note in Details. Part start and end are editable. | Some extra edit-mode UI |
| H-R4 | A store check covers completed feed parts. A part's new start before the previous part's end throws `PartError.startBeforePreviousPart`. Editors show `"The start can't be before the previous part ended, at %@."`, a key duplicated into LoggingFeature with its translations copied. | — |
| H-R5 | Running row tap → `HistoryAction.runningTimer(id)`. `RootView` presents `RunningTimerEditSheet`, made `public` in HomeFeature. | — |
| H-R6 | View-only caregivers (`!store.canWriteInActiveRealm`) get non-interactive rows: no tap target, no swipe. | — |
| H-R7 | Time rows in edit mode include the date (`[.date, .hourAndMinute]`), limited to `...store.now`. | — |
| H-R8 | `EntryEditModel` / `EntryEditSheet` are deleted in Task 6. The E2E tests that used them switch to the new edit models or direct store calls, keeping the same assertions. | — |
| H-R9 | `HistoryFocus(entryID:)` from Ask keeps working. It opens the entry's edit sheet through `HistoryAction`. | — |
| H-R10 | Day titles are "Today", "Yesterday", then `.abbreviated` date. | — |

## Review Focus

These are inputs no task's happy path exercises. Each one has a pinned test in the task named.

1. **A session that started before midnight and ended after it.** Shown once, on the first part's day, with the end time under the start. Pinned in Task 5.
2. **Deleting a part while its session sheet is open.**
   - Deleting the middle part keeps one session.
   - Deleting the last remaining part closes the sheet and removes the row.
   - Deleting the first part makes the next part the new root, with no orphaned links.
   
   Pinned in Task 4.
3. **The Join row when the previous session is solids or the baby's other tracker.** Solids are never offered as a join target, and the row is absent when no earlier same-kind session exists. Pinned in Tasks 1 and 4.
4. **An edit that moves a part's start before the previous part's end.** The clear message shows, and nothing is saved. Pinned in Tasks 1 and 3.
5. **The other phone edits or deletes the entry while its sheet is open.** The sheet reloads on `store.revision`. If the entry is gone, the sheet closes without a crash. Pinned in Tasks 2 and 4.

---

### Task 1: Store session operations

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyStore/BabyStore+SessionEditing.swift`
- Create: `Packages/BabyTracker/Sources/BabyStore/EntryRef.swift`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/SessionEditingTests.swift`

**Interfaces:**
- Consumes:
  - `combineFeeds`, `separateFeed`, `combineSleeps`, `separateSleep`
  - `feedingSessions(for:from:to:)`, `sleepSessions(for:from:to:)`
  - `logBreast`, `logBottle`, `logSleep`
  - `update(_:)`, `softDelete(_:)`
  - `PartError`
- Produces:

```swift
public enum EntryRef: Hashable, Sendable {
    case feed(UUID), sleep(UUID), diaper(UUID), supplement(UUID), measurement(UUID)
    public var id: UUID { get }
}
public enum SessionKind: Hashable, Sendable { case feed, sleep }

/// A completed part added to an existing session from History.
public enum CompletedFeedPart: Equatable, Sendable {
    case breast(side: BreastSide, from: Date, to: Date)
    case bottle(volumeML: Double, contents: BottleContents, at: Date)
}

public struct PreviousSession: Equatable, Sendable {
    public let rootID: UUID      // first member's id
    public let lastMemberID: UUID
    public let startedAt: Date
    public let endedAt: Date
}

public extension BabyStore {
    /// Logs `part` and links it to the session whose root is `rootID`,
    /// whatever the break limit says (manual add).
    @discardableResult
    func addFeedPart(_ part: CompletedFeedPart, toSession rootID: UUID, for babyID: UUID) throws -> FeedEntry
    @discardableResult
    func addSleepPart(from start: Date, to end: Date, toSession rootID: UUID, for babyID: UUID) throws -> SleepEntry
    /// Tombstones every member of the session in one write.
    func deleteSession(_ kind: SessionKind, rootID: UUID, for babyID: UUID) throws
    /// Removes one component from a feed entry; removes the entry if it was the last.
    func removeComponent(_ componentID: UUID, fromFeed feedID: UUID, for babyID: UUID) throws
    /// The newest earlier session of the same kind (feeds: never a solids-only session).
    func previousSession(_ kind: SessionKind, before rootID: UUID, for babyID: UUID) throws -> PreviousSession?
    /// The end of the part just before `feedID` in its session, if any.
    func previousPartEnd(ofFeed feedID: UUID, for babyID: UUID) throws -> Date?
    /// Throws PartError.startBeforePreviousPart when `start` is before previousPartEnd(ofFeed:).
    func checkPartStart(_ start: Date, ofFeed feedID: UUID, for babyID: UUID) throws
}
```

- [ ] **Step 1: Write failing tests.** Use the fixture pattern from `Tests/LoggingFeatureTests/Support/PartTestHelpers.swift` (in-memory store, `TestDateProvider`, `breakLimits = { .standard }`). Add these tests:

```swift
@Test func addFeedPartJoinsTheGivenSessionEvenPastTheBreakLimit() throws {
    let (store, baby, clock) = try fixture()
    let left = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-7200), to: clock.now.addingTimeInterval(-6600), for: baby)
    let bottle = try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: clock.now), toSession: left.id, for: baby)
    #expect(bottle.effectiveFeedingSessionID == left.effectiveFeedingSessionID)
    #expect(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).count == 1)
}
@Test func deleteSessionTombstonesEveryMember() throws { /* 3 joined parts → deleteSession(.feed) → 0 sessions, 0 live feeds */ }
@Test func deletingTheFirstPartPromotesTheNextAsRoot() throws {
    /* L,R,bottle joined; softDelete(L) → one session of R+bottle, every live member's effectiveFeedingSessionID == R.id */
}
@Test func removeComponentDropsOneAndDeletesTheEntryWithTheLast() throws { /* 2-component entry → remove one → 1 left; remove last → entry tombstoned */ }
@Test func previousSessionSkipsSolidsAndIsNilWhenNoneEarlier() throws { /* feed 07:30, solids 09:00, feed 10:23 → previous of 10:23 is 07:30; previous of 07:30 is nil */ }
@Test func previousSleepSessionFindsTheEarlierSleep() throws { }
@Test func checkPartStartRefusesAStartBeforeThePreviousPartEnds() throws {
    /* L 10:00-10:12, R joined 10:15-10:30: checkPartStart(10:10, ofFeed: R) throws .startBeforePreviousPart; 10:12 passes */
}
```

  Fill in each commented test with real assertions in the style of the first. Bodies must assert exactly what the comment says.
- [ ] **Step 2: Run them and confirm they fail.**

```bash
cd Packages/BabyTracker && rtk proxy swift test --disable-sandbox --filter SessionEditing > ../../work/t1-red.log 2>&1; echo $?
```

  The expected failure is compile errors (missing API).
- [ ] **Step 3: Implement `EntryRef`, `SessionKind`, `CompletedFeedPart`, `PreviousSession` and the extension.**
  - `addFeedPart` and `addSleepPart` log through the existing `logBreast` / `logBottle` / `logSleep`, then set `feedingSessionID` / `sleepSessionID` to the session's effective root id and call `update(_:)`. Wrap both steps in one write if the store offers `withRealmWrite`; otherwise do two records, and explain why in the code.
  - Deleting the first member must relink the remaining members to the new first member. Check whether `softDelete` already does this: the redesign's tombstone path (`separateFeed` semantics) may handle it. If it doesn't, `deleteSession` and the root promotion live here, and the `softDelete` of a root goes through a new `deleteFeedPart(_:for:)` that promotes. In that case also produce `func deleteFeedPart(_ feedID: UUID, for babyID: UUID) throws` and `func deleteSleepPart(_ sleepID: UUID, for babyID: UUID) throws`, and list them in the report.
  - `removeComponent` keeps the entry's other components and their ids.
- [ ] **Step 4: Run the tests and confirm they pass, then run `rtk proxy make test`.** Update `EXPECTED_TESTS` and expect exit 0.
- [ ] **Step 5: Commit.** Message: `feat(store): add, delete and check parts of a session from History`.

### Task 2: Edit modes for the Diaper, Meds and Solids sheets

**Files:**
- Modify: `Sources/LoggingFeature/DiaperSheet.swift`, `SupplementSheet.swift`, `SolidsSheet.swift`
- Create: `Sources/LoggingFeature/EditChrome.swift`: shared Save/Delete footer, `EditTimeRow` (date and time, `...now`), and a `deleteConfirmation` modifier
- Modify: `Sources/LoggingFeature/Localizable.xcstrings`
- Test: `Tests/LoggingFeatureTests/DiaperEditTests.swift`, `SupplementEditTests.swift`, `SolidsEditTests.swift`

**Interfaces:**
- Consumes: `EntryRef` (Task 1); `store.entry(id:for:) -> HistoryItem?`, `update(_:)`, `editSupplement(...)`, `softDelete(_:)`.
- Produces:
  - `DiaperSheet(store:babyID:editing: UUID? = nil, onDone:)`
  - `SupplementSheet(store:babyID:editing: UUID? = nil, onDone:)`
  - `SolidsSheet(store:babyID:editing: UUID? = nil, onDone:)`
  - Each sheet's edit logic sits in a testable `@MainActor @Observable` model: `DiaperEditModel(store:babyID:diaperID:)`, `SupplementSheetModel(store:babyID:editing:)` (extended) and `SolidsEditModel(store:babyID:feedID:)`.
  - Each model has `save() throws`, `delete() throws` and `isGone: Bool`. `isGone` is true when the entry vanished, for example after a remote delete.

- [ ] **Step 1: Write failing tests.**
  - Prefill: a dirty diaper with note "x" opens with `.dirty` and the note.
  - Save writes the new contents and time and records an update.
  - Dry is offered only in edit mode (`choices(editing: true)` contains `.dry`; create mode does not).
  - Delete tombstones the entry.
  - `isGone` turns true after the entry is soft-deleted elsewhere and the model reloads on revision.
  - Meds: rename and move the time.
  - Solids: change foods and amount, keeping the component id.
- [ ] **Step 2: Run the tests and confirm they fail** (`--filter "DiaperEdit|SupplementEdit|SolidsEdit"`).
- [ ] **Step 3: Implement.**
  - In edit mode, the title row shows the entry's time as an `EditTimeRow`. The tap-to-save choice buttons become select-then-Save. The footer shows **Save** (primary fill) and **Delete** (destructive, with a confirmation).
  - Create mode is unchanged.
  - Heights: use the existing `.sheetHeight`, plus room for the footer.
- [ ] **Step 4: Run the tests until they pass, then run `make test` and `make build SIMULATOR='platform=iOS Simulator,id=<simulator-udid>'`.**
- [ ] **Step 5: Commit.** Message: `feat(logging): edit modes for diaper, meds and solids`.

### Task 3: Part editors — bottle, breast and sleep (with Join row), and growth Delete

**Files:**
- Modify: `Sources/LoggingFeature/BottleSheet.swift` (new mode `.edit(feedID: UUID, componentID: UUID)`)
- Create: `Sources/LoggingFeature/PartEditSheet.swift`, a breast or sleep part editor in the Past entry layout
- Create: `Sources/LoggingFeature/JoinRow.swift`
- Modify: `Sources/LoggingFeature/MeasurementSheet.swift` (Delete in edit mode)
- Modify: `Sources/LoggingFeature/Localizable.xcstrings`; copy the key `"The start can't be before the previous part ended, at %@."` and all five translations from `Sources/HomeFeature/Localizable.xcstrings`
- Test: `Tests/LoggingFeatureTests/BottleEditTests.swift`, `PartEditTests.swift`, `JoinRowTests.swift`, `MeasurementSheetTests.swift` (extend)

**Interfaces:**
- Consumes: Task 1's `previousSession`, `checkPartStart`, `removeComponent`, `combineFeeds`/`combineSleeps`, and `update`.
- Produces:

```swift
public enum PartEditTarget: Hashable, Sendable {
    case breast(feedID: UUID, componentID: UUID)
    case bottle(feedID: UUID, componentID: UUID)
    case sleep(sleepID: UUID)
}
@MainActor @Observable public final class PartEditModel {
    public init(store: BabyStore, babyID: UUID, target: PartEditTarget, unit: VolumeUnit)
    public var start: Date; public var end: Date        // breast: end = start + duration
    public var side: BreastSide                          // breast only
    public var sleepKind: SleepKind?                     // nil = Auto (sleep only)
    public var note: String
    public private(set) var validationMessage: String?   // includes the H-R4 message
    public var isGone: Bool { get }
    public func save() throws -> Bool                    // false + validationMessage when refused
    public func delete() throws                          // component removal or entry delete per H-R2
}
@MainActor @Observable public final class JoinRowModel {
    public init(store: BabyStore, babyID: UUID, kind: SessionKind, rootID: UUID)
    public private(set) var previous: PreviousSession?   // nil hides the row
    public var label: String { get }                     // "Join the 07:30 feed" / "Join the 08:40 sleep"
    public var gapText: String { get }                   // "2h 8m before"
    public func join() throws                            // combineFeeds/combineSleeps([previous.lastMemberID, rootID])
}
public struct PartEditSheet: View { public init(model: PartEditModel, showsJoinRow: Bool = true, onDone: @escaping () -> Void) }
public struct JoinRow: View { public init(model: JoinRowModel) }
```

- [ ] **Step 1: Write failing tests.**
  - `PartEditModel` breast:
    - prefill side, start and duration;
    - save a new side and duration;
    - a start before the previous part's end gives `validationMessage` "The start can't be before the previous part ended, at 10:12." and `save()` returns false, saving nothing (Review Focus 4);
    - the note round-trips.
  - Sleep: Nap / Night / Auto sets `kindOverride`.
  - Bottle edit: prefill volume, contents and time from the component; save changes only that component.
  - `delete()` on a single-component entry tombstones it. On a two-component entry it removes one.
  - `JoinRowModel`:
    - the label and gap for the previous feed at 07:30 are correct;
    - `previous == nil` when the only earlier feed is solids (Review Focus 3);
    - `join()` puts both in one session.
  - `MeasurementSheet` edit `delete()` tombstones the check-in.
- [ ] **Step 2: Run the tests and confirm they fail.**
- [ ] **Step 3: Implement.**
  - **PartEditSheet:**
    - layout: side picker (breast), `EditTimeRow` start, duration stepper or end `EditTimeRow` (sleep);
    - for sleep, a Nap / Night / Auto segmented picker;
    - a Details disclosure with the Note;
    - the `JoinRow` at the top only when opened standalone (`showsJoinRow: Bool` init flag, default true);
    - the Save and Delete footer from `EditChrome`.
  - **BottleSheet `.edit`:** chips with unit, contents, time row, Save, Delete.
  - **Delete in `MeasurementSheet` edit mode.**
- [ ] **Step 4: Run the tests until they pass, then run `make test` and `make build`.**
- [ ] **Step 5: Commit.** Message: `feat(logging): part editors with Join, bottle edit, growth delete`.

### Task 4: Session sheet

**Files:**
- Create: `Sources/LoggingFeature/SessionSheet.swift` (view) and `SessionSheetModel.swift`
- Modify: `Sources/LoggingFeature/Localizable.xcstrings`
- Test: `Tests/LoggingFeatureTests/SessionSheetModelTests.swift`

**Interfaces:**
- Consumes: Tasks 1 and 3 (`PartEditModel`, `PartEditTarget`, `JoinRowModel`, `addFeedPart`, `addSleepPart`, `deleteSession`, `separateFeed`/`separateSleep`).
- Produces:

```swift
public struct SessionPart: Identifiable, Equatable {
    public let id: UUID                   // component id, or sleep id
    public let target: PartEditTarget
    public let icon: BabyIcon
    public let label: String              // "Left", "Right", "Formula 60 ml", "1h 2m"
    public let detail: String             // "10:23 · 12m"
    public let gapBefore: String?         // "52m later"; nil for the first part
    public let canSplitBefore: Bool       // H-R2: true only when the previous part is a different entry
}
@MainActor @Observable public final class SessionSheetModel {
    public init(store: BabyStore, babyID: UUID, kind: SessionKind, rootID: UUID, unit: VolumeUnit)
    public private(set) var title: String         // "10:23 – 11:48"
    public private(set) var totals: String        // "1h 25m · 27m breast · 60 ml" / "2h 9m · 1 wake"
    public private(set) var parts: [SessionPart]
    public let join: JoinRowModel
    public var isGone: Bool { get }               // session no longer exists → sheet closes
    public func split(before partID: UUID) throws
    public func addPart(_ kind: AddPartKind) throws -> PartEditTarget   // .left/.right/.bottle/.sleep, starts at session end
    public func deletePart(_ partID: UUID) throws
    public func deleteSession() throws
    public func reload()
}
public enum AddPartKind: CaseIterable { case left, right, bottle, sleep }
public struct SessionSheet: View { public init(model: SessionSheetModel, unit: VolumeUnit, onDone: @escaping () -> Void) }
```

- [ ] **Step 1: Write failing tests.**
  - Parts, gaps, title and totals are right for an L, R, bottle session spread over three entries.
  - In an old single-entry combo with three components, `canSplitBefore` is false for all.
  - `split(before: R)` makes two sessions.
  - Join merges with the previous feed.
  - `addPart(.bottle)` adds a part starting at the session's end, in this session.
  - Deleting parts (Review Focus 2):
    - delete the middle part: still one session;
    - delete the first part: the next becomes root;
    - delete the last remaining part: `isGone`.
  - `deleteSession` tombstones all members.
  - A remote delete followed by `reload()` sets `isGone` (Review Focus 5).
  - A sleep session shows "· 1 wake".
- [ ] **Step 2: Run the tests and confirm they fail.**
- [ ] **Step 3: Implement.**
  - **SessionSheet (mockup G2):**
    - header: title, totals, and Done;
    - the `JoinRow`;
    - part rows that push `PartEditSheet(showsJoinRow: false)` inside a `NavigationStack`;
    - between parts: the grey `gapBefore` on the left and **Split here** on the right, only when `canSplitBefore`;
    - an "Add part" menu with `AddPartKind` filtered by kind;
    - **Delete feed / Delete sleep** with a confirmation;
    - swipe-to-delete on part rows;
    - close on `isGone`;
    - reload on `store.revision`.
  - No scissors and no dashed lines.
- [ ] **Step 4: Run the tests until they pass, then run `make test` and `make build`.**
- [ ] **Step 5: Commit.** Message: `feat(logging): session sheet with Join, Split here and Add part`.

### Task 5: History list model

**Files:**
- Rewrite: `Sources/HistoryFeature/HistoryViewModel.swift`
- Modify: `Sources/HistoryFeature/Localizable.xcstrings`
- Rewrite tests: `Tests/HistoryFeatureTests/HistoryViewModelTests.swift`. Keep every still-valid assertion from the current 31 tests; delete only those for removed behaviour (menus, chips, summary line), and list them.

**Interfaces:**
- Consumes: `feedingSessions`, `sleepSessions`, `entries(for:from:to:)`, `measurements(for:)`, `activeTimers(for:in:)`, `canWriteInActiveRealm`, and `EntryRef` / `SessionKind`.
- Produces:

```swift
public enum HistoryRowTarget: Equatable { case entry(EntryRef), session(SessionKind, rootID: UUID), runningTimer(UUID) }
public struct HistoryChip: Equatable, Identifiable { public let id: Int; public let icon: BabyIcon?; public let text: String }  // "L 12m", "60 ml"
public struct HistoryListRow: Identifiable, Equatable {
    public let id: UUID
    public let tracker: Tracker
    public let start: String                 // "10:23"
    public let end: String?                  // "11:48" or "now"
    public let chips: [HistoryChip]          // feeds
    public let text: String?                 // sleeps "2h 9m · 1 wake", diapers "Wet", meds name, growth "4.2 kg · 54 cm"
    public let isRunning: Bool
    public let target: HistoryRowTarget
    public let deleteConfirmation: Bool      // sessions ask, singles do not
}
public struct HistoryDayTotals: Equatable { public let feeds: Int; public let sleep: String; public let diapers: Int }
public struct HistoryDay: Identifiable, Equatable { public let id: DayKey; public let title: String; public let totals: HistoryDayTotals; public let rows: [HistoryListRow] }
@MainActor @Observable public final class HistoryViewModel {
    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres)
    public var filter: Set<Tracker>          // default [.feed, .sleep, .diaper, .supplement, .growth]
    public var isFiltered: Bool { get }
    public private(set) var days: [HistoryDay]
    public private(set) var canEdit: Bool    // H-R6
    public private(set) var emptyState: HistoryEmptyState?   // .noData / .nothingMatches
    public func load(dayCount: Int) throws
    public func delete(_ row: HistoryListRow) throws          // single: softDelete; session: deleteSession
}
```

- [ ] **Step 1: Write failing tests.**
  - A2 rows:
    - an L, R, bottle session gives one row with chips `L 12m`, `R 15m`, `60 ml` and start/end times;
    - a sleep session gives `2h 9m · 1 wake`;
    - diaper Wet; med name; growth values (H-R1).
  - A running feed shows at the top of today with end `now` and `isRunning`.
  - Midnight: a session from 23:40 to 00:25 appears once, on the first day, with end "00:25" (Review Focus 1).
  - Totals count sessions once, and sleep comes from `sleepSessions`.
  - Titles: Today, Yesterday, then the short date.
  - Filters: diapers only; `isFiltered`; `.nothingMatches`.
  - `canEdit == false` for a read-only realm.
  - `delete` of a session row tombstones all members.
- [ ] **Step 2: Run the tests and confirm they fail.**
- [ ] **Step 3: Implement.** Build rows from the session projections, so there is no per-feed or per-sleep duplication. Use the entry's time zone for clock text, and `Volume.formatted(as:)` for bottle volumes.
- [ ] **Step 4: Run `make test` and confirm it passes.** HistoryView won't compile against the new model until Task 6, so Tasks 5 and 6 may land as one commit pair: commit 5 with a minimal adapter in `HistoryView`, or with HistoryView excluded from the build. Say which in the report.
- [ ] **Step 5: Commit.** Message: `feat(history): one-row-per-session list model`.

### Task 6: History list UI, wiring and removal of the old editor

**Files:**
- Rewrite: `Sources/HistoryFeature/HistoryView.swift`
- Delete: `Sources/HistoryFeature/EntryEditModel.swift` and `EntryEditSheet.swift`; delete their tests `Tests/HistoryFeatureTests/EntryEditTests.swift` after porting every assertion still meaningful into the Task 2–4 test files, and list them
- Modify: `Sources/LoggingFeature/LogCoordinator.swift`, adding `LogSheet` cases and routing
- Modify: `Sources/HomeFeature/RunningTimerEditSheet.swift`, making it `public`
- Modify: `Apps/BabyTrackerApp/RootView.swift`
- Modify: `Tests/FamilySyncClientE2ETests/TwoCaregiverSyncE2ETests.swift` and `OwnerReinstallE2ETests.swift`, replacing `EntryEditModel` with the new models or store calls and keeping the assertions (H-R8)

**Interfaces:**
- Consumes: Tasks 1–5.
- Produces:

```swift
// HistoryFeature
public enum HistoryAction: Equatable { case edit(EntryRef), session(SessionKind, rootID: UUID), runningTimer(UUID) }
public init(store: BabyStore, babyID: UUID, babyName: String = "", volumeUnit: VolumeUnit = .millilitres,
            focus: HistoryFocus? = nil, onAction: @escaping (HistoryAction) -> Void)
// LoggingFeature: new LogSheet cases
case editEntry(EntryRef)                  // id "edit-<kind>-<uuid>"
case session(SessionKind, rootID: UUID)   // id "session-<kind>-<uuid>"
```

- [ ] **Step 1: Rewrite HistoryView to the spec's section 1** (mockups `history-layout.html` A and `history-times.html` A2).
  - Header: the name, plus the round filter button (`history.filter`), with a dot while filtered.
  - The pinned "Showing: … ✕" chip (`history.filter.clear`).
  - The filter sheet: icon toggles (`history.filter.<tracker>`).
  - `List` with sections and pinned headers (`.listStyle(.plain)` plus `pinnedViews`, or `Section` headers with `.listSectionSpacing`, whichever pins on iOS 26). Headers show the icon totals.
  - Rows (`history.row.<n>`): start over end, icon, chips or text.
  - Tap → `onAction(row.target)`, mapping `.entry` → `.edit` and `.session` → `.session`.
  - `.swipeActions` Delete with a confirmation for sessions.
  - Empty states, H-R6 gating, and `openFocusedEntry` → `onAction(.edit(ref))` (H-R9).
  - Remove the eyebrow, chips, menus and alert.
- [ ] **Step 2: Route the new cases in LoggingFeature and the app.**
  - `LogSheetView` routes `.editEntry(.diaper(id))` → `DiaperSheet(editing:)`. Likewise `.supplement` → `SupplementSheet(editing:)`, and solids feeds → `SolidsSheet(editing:)`.
  - Single-part feeds → `BottleSheet(.edit)` or `PartEditSheet(.breast)`. Sleep → `PartEditSheet(.sleep)`. Measurement → `MeasurementSheet(.edit)`.
  - `.session` → `SessionSheet`.
  - `RootView` passes `onAction`: `.edit`/`.session` → `sheet = .editEntry/.session`; `.runningTimer(id)` → present `RunningTimerEditSheet` built from `RunningTimerEditModel(timer:store:unit:)`.
- [ ] **Step 3: Port the E2E tests and delete the old editor.** Check that nothing references `EntryEditModel` or `EntryEditSheet`:

```bash
rtk proxy grep -rn "EntryEdit" Packages Apps UITests
```

  Comments must be updated too.
- [ ] **Step 4: Run gates.** Run `make test` (update `EXPECTED_TESTS`), `make build SIMULATOR=…`, and `make server-protocol-e2e`. Expect all exit 0.
- [ ] **Step 5: Commit.** Message: `feat(history): new list, sheets through RootView, old editor removed`.

### Task 7: UI tests, audits, Ukrainian XXXL, copy and localization

**Files:**
- Modify: `UITests/BabyTrackerUITests/` — the History-touching suites listed in the survey: CompositeFeedFlowTests, FeedingHistoryLayoutTests, EditFlowTests, LoggingFlowTests, HomeRedesignWalkthroughTests, CopyReadThroughTests, SupplementFlowTests, RunningTimerLifecycleTests, ScreenshotTourTests, AskTabTests, ReadOnlyCaregiverUITests
- Modify: `AccessibilityAuditTests.swift` (History list, the session sheet, each edit mode, plus XXXL) and `UkrainianLayoutSmokeTests.swift` (a History XXXL case)
- Create: `UITests/BabyTrackerUITests/HistoryRedesignTests.swift`
- Modify: `Apps/BabyTrackerApp/UITestSupport.swift` and `UITestHarness.swift`, adding a seed `-uiTestHistorySessions`: a joined L+R+bottle feed, a sleep with one wake, a diaper, a med, a growth check-in, and a session crossing midnight

**Interfaces:**
- Consumes: the accessibility identifiers from Task 6.

- [ ] **Step 1: Write `HistoryRedesignTests`.**
  - Open History, tap the session row, tap **Split here** before the bottle: two rows.
  - Tap the bottle row, then **Join the 10:23 feed**: one row.
  - Swipe-delete a diaper.
  - Swipe-delete a session, with its confirmation.
  - Filter to Diapers, then clear it.
  - Open each edit sheet once and save.
  - A view-only family: rows aren't hittable.
- [ ] **Step 2: Update the listed suites** to the new identifiers and flows, keeping their intent. Replace the Dry and sleep-override flows in EditFlowTests with the edit-mode equivalents.
- [ ] **Step 3: Audits and Ukrainian XXXL.**
  - Add audits; known issues must be named narrowly, as in ruling R23.
  - Add the History XXXL Ukrainian case: rows and chips wrap without mid-word hyphenation, and the session sheet stays usable.
- [ ] **Step 4: Run all gates.**
  - `make test`, `make build`.
  - `make test-ui SIMULATOR='platform=iOS Simulator,id=<simulator-udid>'`, with 0 failures.
  - `make server-protocol-e2e`.
  - The localization gate (Global Constraints), printing nothing.
  - Save the History screenshots to `docs/verification/2026-10-03-history-redesign/`, and write `docs/verification/2026-10-03-history-redesign.md` with the gate results.
- [ ] **Step 5: Commit.** Message: `test(history): redesign flows, audits, Ukrainian XXXL, verification`.
