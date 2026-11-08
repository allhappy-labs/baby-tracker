# Arrangeable Home Buttons Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Each baby gets an ordered list of Home actions on this phone that decides the dock's big button, its three tiles, the ··· sheet and the Quick Log widget; the bottle amount screen gains contents and starts from the previous bottle's amount.

**Architecture:** A pure `HomeLayout` value in BabyCore turns an order into the dock, the ··· sheet contents and the widget slots. The order is stored per baby by `TrackingPreferences` in the App Group defaults, and reaches Home the way `volumeUnit` does: `SettingsViewModel` is the observation seam, `RootView` hands the order to `HomeView` and `LogSheetView`. The widget reads `TrackingPreferences` directly.

**Tech Stack:** Swift 6, SwiftUI (iOS 26), SwiftData `BabyStore`, WidgetKit + App Intents, Swift Testing, XCUITest.

**Spec:** `docs/superpowers/specs/2026-10-11-home-buttons-order-design.md`

## Global Constraints

- iOS 26 / macOS 26 package platforms; Swift 6 language mode. Anything iOS-only in the package is wrapped in `#if os(iOS)`, as `SettingsView` already does.
- No schema, sync-payload or server change. No deploy.
- Actions and their stored raw values: `breast`, `bottle`, `sleep`, `diaper`, `solids`, `meds`, `growth`. Never rename a raw value.
- Default order: Breast, Bottle, Sleep, Diaper, Solids, Meds, Growth. With it, every Home state must equal today's.
- Storage key: `tracking.homeOrder.<babyID uuidString>` in the App Group defaults.
- Position 1 is the big button, 2–4 are tiles, 5 and later are in ···. ··· and Past entry are not part of the order.
- Default bottle amount: the previous bottle's amount; 120 ml with no earlier bottle or when that bottle was saved at 0 ml.
- Existing accessibility identifiers stay: `home.start.left`, `home.start.right`, `home.bottle`, `home.sleep`, `home.diaper`, `home.more`, `home.feed`, `home.stop`, `home.wake`, `more.solids`, `more.growth`, `more.pastEntry`, `more.med.N`, `more.addMed`, `bottle.contents`.
- Strings never live in a view. Every new user-facing string goes through the target's copy enum and gets `de`, `fr`, `es`, `it`, `uk` translations in that target's `Localizable.xcstrings` via `scripts/add-catalog-strings.py`. Before adding a key, `grep` the catalog; a key that already exists is left alone.
- Tests assert logic, never layout geometry.
- `make test` asserts a literal total (`EXPECTED_TESTS` in `Makefile`). Each task says how many tests it adds; bump the number by that much in the same commit. If the number on the branch differs from the one this plan assumes, add the delta to whatever is there.
- Simulator: only `platform=iOS Simulator,id=<simulator-udid>`. Run `pgrep xcodebuild` first; one run at a time.
- UI tests: only the named tests with `-only-testing:`, never the full suite.
- Work in worktree `.worktrees/home-buttons` on branch `home-buttons`; use `/usr/bin/git` there (RTK's rewrite trips the worktree guard). Main carries another session's uncommitted History edits; do not touch them.
- End every commit message with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.
- No new Swift file under `Apps/` or `UITests/` (they would need Xcode project membership). New files go in the package only.

## Review Focus

1. **The previous bottle was saved at 0 ml** — the default must fall back to 120 ml, or a Lock Screen stop saves another 0 ml bottle that no total counts. Test in Task 3 (`aZeroVolumeLastBottleFallsBackToTheDefault`).
2. **A stored order this version cannot fully read** — an unknown kind from a later version, a duplicate, or an empty array must read as a complete seven-action order. Tests in Task 1 (`storedOrdersAreCompletedAndCleaned`) and Task 2 (`aStoredOrderWithUnknownKindsReadsComplete`).
3. **No feed action on the dock while a sleep runs** — the Feed chooser tile must not appear, and Breast and Bottle must stay reachable in ···. Tests in Task 1 (`runningTilesDropWhatConflicts`) and Task 7 (`duringASleepTheSheetKeepsFeedActions`).
4. **The bottle's timer is gone when Done is tapped** — stopped from the other phone while the amount screen is open with changed contents: the save is refused and nothing is written. Test in Task 4 (`doneAfterTheTimerIsGoneSavesNothing`).
5. **A widget link the app cannot act on** — a baby deleted since the widget drew, a malformed link, or a link arriving before the store is open: Home shows, no sheet, no crash. Tests in Task 9 (`LogLinkTests`), the parked-link path in Task 9 Step 6, and the manual check in Task 11.

---

### Task 0: Worktree

**Files:** none changed.

- [ ] **Step 1: Create the worktree and bring the spec and plan into it**

```bash
cd <repo>
/usr/bin/git worktree add .worktrees/home-buttons -b home-buttons
cp docs/superpowers/specs/2026-10-11-home-buttons-order-design.md .worktrees/home-buttons/docs/superpowers/specs/
cp docs/superpowers/plans/2026-10-11-home-buttons-order.md .worktrees/home-buttons/docs/superpowers/plans/
cd .worktrees/home-buttons
/usr/bin/git add docs/superpowers/specs/2026-10-11-home-buttons-order-design.md docs/superpowers/plans/2026-10-11-home-buttons-order.md
/usr/bin/git commit -m "docs(home): arrangeable Home buttons spec and plan

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

The two files stay untracked in the main checkout. Tell the user at the end; they must be removed there before `home-buttons` can be merged.

- [ ] **Step 2: Confirm the baseline**

Run: `cd <repo>/.worktrees/home-buttons && make test`
Expected: `OK` with the `EXPECTED_TESTS` total in the `Makefile` (1755 when this plan was written). Note the number; every later bump is relative to it.

All later paths are relative to `.worktrees/home-buttons`.

---

### Task 1: `HomeActionKind` and `HomeLayout`

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyCore/HomeLayout.swift`
- Test: `Packages/BabyTracker/Tests/BabyCoreTests/HomeLayoutTests.swift`

**Interfaces:**
- Produces:
  - `enum HomeActionKind: String, CaseIterable, Hashable, Sendable` with cases `breast, bottle, sleep, diaper, solids, meds, growth`; `static let defaultOrder: [HomeActionKind]`; `static func order(fromStored raw: [String]) -> [HomeActionKind]`.
  - `struct HomeLayout: Equatable, Sendable` with `init(order: [HomeActionKind] = HomeActionKind.defaultOrder)`, `order`, `big`, `tiles`, `docked`, `overflow`, `tilesWhileFeedRuns: [HomeActionKind]`, `tilesWhileSleepRuns: [RunningTile]`, `func overflow(feedRunning: Bool, sleepRunning: Bool) -> [HomeActionKind]`, `func widgetSlots(_ count: Int = 4) -> [WidgetSlot]`, `static func role(at index: Int) -> Role`.
  - `HomeLayout.RunningTile: Hashable, Sendable` = `.feed | .action(HomeActionKind)`.
  - `HomeLayout.WidgetSlot: Hashable, Sendable` = `.breastLeft | .breastRight | .action(HomeActionKind)`.
  - `HomeLayout.Role` = `.big | .tile | .overflow`.

- [ ] **Step 1: Write the failing tests**

```swift
import Testing
import Foundation
@testable import BabyCore

@Suite struct HomeLayoutTests {
    @Test func theDefaultOrderIsTodaysHome() {
        let layout = HomeLayout()
        #expect(layout.big == .breast)
        #expect(layout.tiles == [.bottle, .sleep, .diaper])
        #expect(layout.overflow == [.solids, .meds, .growth])
        #expect(layout.tilesWhileFeedRuns == [.diaper])
        #expect(layout.tilesWhileSleepRuns == [.feed, .action(.diaper)])
        #expect(layout.overflow(feedRunning: false, sleepRunning: false) == [.solids, .meds, .growth])
        #expect(layout.overflow(feedRunning: true, sleepRunning: false) == [.solids, .meds, .growth])
    }

    @Test func storedOrdersAreCompletedAndCleaned() {
        #expect(HomeActionKind.order(fromStored: []) == HomeActionKind.defaultOrder)
        // "pump" is a kind a later version may write; "bottle" is repeated.
        #expect(HomeActionKind.order(fromStored: ["bottle", "pump", "bottle", "solids"])
                == [.bottle, .solids, .breast, .sleep, .diaper, .meds, .growth])
    }

    @Test func anIncompleteOrderHandedToALayoutIsCompleted() {
        #expect(HomeLayout(order: [.solids]).order == [.solids, .breast, .bottle, .sleep, .diaper, .meds, .growth])
    }

    @Test func aNonBreastActionCanBeBig() {
        let layout = HomeLayout(order: [.bottle, .breast, .solids, .diaper, .sleep, .meds, .growth])
        #expect(layout.big == .bottle)
        #expect(layout.tiles == [.breast, .solids, .diaper])
        #expect(layout.overflow == [.sleep, .meds, .growth])
    }

    @Test func runningTilesDropWhatConflicts() {
        let layout = HomeLayout(order: [.solids, .diaper, .meds, .growth, .breast, .bottle, .sleep])
        #expect(layout.tilesWhileFeedRuns == [.solids, .diaper, .meds])
        // No feed action is docked, so no Feed tile; three tiles at most.
        #expect(layout.tilesWhileSleepRuns == [.action(.solids), .action(.diaper), .action(.meds)])
        #expect(layout.overflow(feedRunning: true, sleepRunning: false) == [])
        #expect(layout.overflow(feedRunning: false, sleepRunning: true) == [.breast, .bottle])
    }

    @Test func theFeedTileLeadsWhileSleepRunsWhenAFeedActionIsDocked() {
        let layout = HomeLayout(order: [.bottle, .solids, .diaper, .growth, .breast, .sleep, .meds])
        #expect(layout.tilesWhileSleepRuns == [.feed, .action(.solids), .action(.diaper)])
    }

    @Test func rolesFollowPosition() {
        #expect(HomeLayout.role(at: 0) == .big)
        #expect(HomeLayout.role(at: 1) == .tile)
        #expect(HomeLayout.role(at: 3) == .tile)
        #expect(HomeLayout.role(at: 4) == .overflow)
        #expect(HomeLayout.role(at: 6) == .overflow)
    }

    @Test func widgetSlotsWalkTheOrderAndBreastTakesTwo() {
        #expect(HomeLayout().widgetSlots() == [.breastLeft, .breastRight, .action(.bottle), .action(.sleep)])
        #expect(HomeLayout(order: [.bottle, .breast, .sleep, .diaper, .solids, .meds, .growth]).widgetSlots()
                == [.action(.bottle), .breastLeft, .breastRight, .action(.sleep)])
        // Breast does not fit the one slot left, so it is skipped.
        #expect(HomeLayout(order: [.sleep, .diaper, .bottle, .breast, .solids, .meds, .growth]).widgetSlots()
                == [.action(.sleep), .action(.diaper), .action(.bottle), .action(.solids)])
    }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd Packages/BabyTracker && swift test --filter HomeLayoutTests`
Expected: FAIL to compile, `cannot find 'HomeLayout' in scope`.

- [ ] **Step 3: Implement**

`Packages/BabyTracker/Sources/BabyCore/HomeLayout.swift`:

```swift
import Foundation

/// An action Home can offer. The raw values are stored
/// (`TrackingPreferences.homeOrder`), so one is never renamed.
public enum HomeActionKind: String, CaseIterable, Hashable, Sendable {
    case breast, bottle, sleep, diaper, solids, meds, growth

    /// Home as it was before the order could be changed: L and R, then
    /// Bottle, Sleep and Diaper, with the rest behind ···.
    public static let defaultOrder: [HomeActionKind] = allCases

    /// A complete order from stored raw values: the kinds this version knows,
    /// once each in their stored sequence, then every kind the stored value
    /// lacks in default order. A kind a later version adds therefore lands at
    /// the end, and a value a later version wrote still reads.
    public static func order(fromStored raw: [String]) -> [HomeActionKind] {
        var seen = Set<HomeActionKind>()
        let stored = raw.compactMap(HomeActionKind.init(rawValue:)).filter { seen.insert($0).inserted }
        return stored + defaultOrder.filter { !seen.contains($0) }
    }
}

/// What an order means on each surface: the dock, the ··· sheet and the
/// Quick Log widget. Pure, so the three never disagree.
public struct HomeLayout: Equatable, Sendable {
    /// The small tiles beside ···.
    public static let tileCount = 3

    public enum Role: Equatable, Sendable { case big, tile, overflow }

    /// A tile in a running dock. `feed` is the L / R / Bottle chooser a
    /// running sleep offers.
    public enum RunningTile: Hashable, Sendable {
        case feed
        case action(HomeActionKind)
    }

    public enum WidgetSlot: Hashable, Sendable {
        case breastLeft, breastRight
        case action(HomeActionKind)
    }

    /// Always all seven kinds, once each.
    public let order: [HomeActionKind]

    public init(order: [HomeActionKind] = HomeActionKind.defaultOrder) {
        self.order = HomeActionKind.order(fromStored: order.map(\.rawValue))
    }

    public var big: HomeActionKind { order[0] }
    public var tiles: [HomeActionKind] { Array(order[1...Self.tileCount]) }
    /// The big action and the tiles, in order.
    public var docked: [HomeActionKind] { Array(order.prefix(Self.tileCount + 1)) }
    public var overflow: [HomeActionKind] { Array(order.dropFirst(Self.tileCount + 1)) }

    public static func role(at index: Int) -> Role {
        index == 0 ? .big : index <= tileCount ? .tile : .overflow
    }

    /// Actions with a timer of their own: none of them is a tile in a
    /// running dock, where Stop or Wake up is the one timer control.
    private static let timed: Set<HomeActionKind> = [.breast, .bottle, .sleep]

    public var tilesWhileFeedRuns: [HomeActionKind] {
        Array(docked.filter { !Self.timed.contains($0) }.prefix(Self.tileCount))
    }

    /// A baby can fall asleep at the breast, so a feed may start while the
    /// sleep keeps running — when a feed action is on the dock at all.
    public var tilesWhileSleepRuns: [RunningTile] {
        let feed: [RunningTile] = docked.contains(.breast) || docked.contains(.bottle) ? [.feed] : []
        let rest = docked.filter { !Self.timed.contains($0) }.map(RunningTile.action)
        return Array((feed + rest).prefix(Self.tileCount))
    }

    /// What the ··· sheet offers. A running feed leaves out every timed
    /// action; a running sleep leaves out only Sleep.
    public func overflow(feedRunning: Bool, sleepRunning: Bool) -> [HomeActionKind] {
        if feedRunning { return overflow.filter { !Self.timed.contains($0) } }
        if sleepRunning { return overflow.filter { $0 != .sleep } }
        return overflow
    }

    /// Fills `count` slots by walking the order. Breast takes two, L and R;
    /// an action that does not fit the slots left is skipped.
    public func widgetSlots(_ count: Int = 4) -> [WidgetSlot] {
        var slots: [WidgetSlot] = []
        for kind in order where slots.count < count {
            if kind == .breast {
                if slots.count + 2 <= count { slots += [.breastLeft, .breastRight] }
            } else {
                slots.append(.action(kind))
            }
        }
        return slots
    }
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd Packages/BabyTracker && swift test --filter HomeLayoutTests`
Expected: PASS, 8 tests.

- [ ] **Step 5: Commit**

Bump `EXPECTED_TESTS` in `Makefile` by 8. Run `make test`; expected `OK`.

```bash
/usr/bin/git add Makefile Packages/BabyTracker/Sources/BabyCore/HomeLayout.swift Packages/BabyTracker/Tests/BabyCoreTests/HomeLayoutTests.swift
/usr/bin/git commit -m "feat(home): HomeLayout turns an order into dock, More and widget (+8 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Store the order per baby

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/TrackingPreferences.swift`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/TrackingPreferencesTests.swift`

**Interfaces:**
- Consumes: `HomeActionKind.order(fromStored:)`, `HomeActionKind.defaultOrder`, `HomeLayout(order:)` (Task 1).
- Produces on `TrackingPreferences`:
  - `static func homeOrderKey(for babyID: UUID) -> String`
  - `func homeOrder(for babyID: UUID) -> [HomeActionKind]`
  - `func setHomeOrder(_ order: [HomeActionKind], for babyID: UUID)`
  - `func resetHomeOrder(for babyID: UUID)`

- [ ] **Step 1: Write the failing tests**

Add inside `TrackingPreferencesTests`:

```swift
    @Test func withNothingStoredTheOrderIsTheDefault() {
        #expect(TrackingPreferences(defaults: defaults()).homeOrder(for: UUID()) == HomeActionKind.defaultOrder)
    }

    @Test func anOrderIsKeptPerBaby() {
        let prefs = TrackingPreferences(defaults: defaults())
        let mira = UUID(), tomas = UUID()
        prefs.setHomeOrder([.bottle, .breast, .sleep, .diaper, .solids, .meds, .growth], for: mira)
        #expect(prefs.homeOrder(for: mira).first == .bottle)
        #expect(prefs.homeOrder(for: tomas) == HomeActionKind.defaultOrder)
    }

    @Test func aStoredOrderWithUnknownKindsReadsComplete() {
        let d = defaults()
        let baby = UUID()
        d.set(["solids", "pump", "solids"], forKey: TrackingPreferences.homeOrderKey(for: baby))
        #expect(TrackingPreferences(defaults: d).homeOrder(for: baby)
                == [.solids, .breast, .bottle, .sleep, .diaper, .meds, .growth])
    }

    @Test func resetRemovesTheStoredOrder() {
        let d = defaults()
        let prefs = TrackingPreferences(defaults: d)
        let baby = UUID()
        prefs.setHomeOrder([.solids, .breast, .bottle, .sleep, .diaper, .meds, .growth], for: baby)
        prefs.resetHomeOrder(for: baby)
        #expect(d.object(forKey: TrackingPreferences.homeOrderKey(for: baby)) == nil)
        #expect(prefs.homeOrder(for: baby) == HomeActionKind.defaultOrder)
    }

    /// The default is "nothing stored", so a kind a later version adds to the
    /// default order is not pinned to the end for a baby left at the default.
    @Test func storingTheDefaultOrderStoresNothing() {
        let d = defaults()
        let baby = UUID()
        TrackingPreferences(defaults: d).setHomeOrder(HomeActionKind.defaultOrder, for: baby)
        #expect(d.object(forKey: TrackingPreferences.homeOrderKey(for: baby)) == nil)
    }
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd Packages/BabyTracker && swift test --filter TrackingPreferencesTests`
Expected: FAIL to compile, `value of type 'TrackingPreferences' has no member 'homeOrder'`.

- [ ] **Step 3: Implement**

In `TrackingPreferences`, after `limits`:

```swift
    // MARK: - Home buttons

    public static func homeOrderKey(for babyID: UUID) -> String {
        "tracking.homeOrder.\(babyID.uuidString)"
    }

    /// The order of Home's actions for one baby, as this phone arranged it.
    /// Always complete — see `HomeActionKind.order(fromStored:)`.
    public func homeOrder(for babyID: UUID) -> [HomeActionKind] {
        HomeActionKind.order(fromStored: defaults.stringArray(forKey: Self.homeOrderKey(for: babyID)) ?? [])
    }

    /// The default order is stored as nothing at all, so a baby left at the
    /// default follows whatever the default becomes.
    public func setHomeOrder(_ order: [HomeActionKind], for babyID: UUID) {
        let complete = HomeLayout(order: order).order
        guard complete != HomeActionKind.defaultOrder else { return resetHomeOrder(for: babyID) }
        defaults.set(complete.map(\.rawValue), forKey: Self.homeOrderKey(for: babyID))
    }

    public func resetHomeOrder(for babyID: UUID) {
        defaults.removeObject(forKey: Self.homeOrderKey(for: babyID))
    }
```

Update the type's doc comment: replace "How long a break may last before the next part starts a new feed or sleep session, as the person using this device set it." with "How this phone tracks: the break limits, and the order of each baby's Home buttons."

- [ ] **Step 4: Run to verify it passes**

Run: `cd Packages/BabyTracker && swift test --filter TrackingPreferencesTests`
Expected: PASS, 8 tests (3 existing, 5 new).

- [ ] **Step 5: Commit**

Bump `EXPECTED_TESTS` by 5. Run `make test`; expected `OK`.

```bash
/usr/bin/git add Makefile Packages/BabyTracker/Sources/BabyStore/TrackingPreferences.swift Packages/BabyTracker/Tests/BabyStoreTests/TrackingPreferencesTests.swift
/usr/bin/git commit -m "feat(home): store each baby's Home order in the App Group (+5 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: The default bottle amount is the previous bottle's

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/LogDefaults.swift`
- Modify (callers): `Packages/BabyTracker/Sources/BabyStore/BabyStore+Parts.swift:55`, `Packages/BabyTracker/Sources/BabyFeature/OutOfAppFeedActions.swift:70`, `Packages/BabyTracker/Sources/LoggingFeature/BottleSheet.swift:82`, `Packages/BabyTracker/Sources/LoggingFeature/SessionSheetModel.swift:213`, `Packages/BabyTracker/Sources/LoggingFeature/PartFormModel.swift:65`
- Test: `Packages/BabyTracker/Tests/LoggingFeatureTests/LogDefaultsTests.swift`, `Packages/BabyTracker/Tests/BabyStoreTests/OnePartTimerTests.swift:187`

**Interfaces:**
- Produces: `LogDefaults.lastBottleVolumeML(for babyID: UUID) throws -> Double`. `mostUsedBottleVolumeML` is deleted.

- [ ] **Step 1: Rewrite the tests**

Replace the whole of `LogDefaultsTests.swift` with:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
@testable import LoggingFeature

@MainActor
@Test func bottleDefaultsToThePreviousBottle() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
    clock.advance(by: 60)
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
    clock.advance(by: 60)
    _ = try store.logBottle(volumeML: 90, contents: .breastMilk, for: baby.id)

    let defaults = LogDefaults(store: store)
    // The previous bottle, not the most common one.
    #expect(try defaults.lastBottleVolumeML(for: baby.id) == 90)
    #expect(try defaults.lastBottleContents(for: baby.id) == .breastMilk)
}

@MainActor
@Test func bottleDefaultsAreSensibleWithNoHistory() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let defaults = LogDefaults(store: store)

    #expect(try defaults.lastBottleVolumeML(for: baby.id) == 120)
    #expect(try defaults.lastBottleContents(for: baby.id) == .formula)
}

@MainActor
@Test func breastFeedsDoNotPolluteBottleDefaults() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    let timer = try store.startPart(.breast(.left), for: baby.id)
    clock.advance(by: 900)
    _ = try store.stopFeed(timer)

    #expect(try LogDefaults(store: store).lastBottleVolumeML(for: baby.id) == 120)
}

/// A bottle saved at 0 ml is never the default: every total leaves a
/// zero-volume bottle out, and a Lock Screen stop would save another.
@MainActor
@Test func aZeroVolumeLastBottleFallsBackToTheDefault() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby.id)
    clock.advance(by: 60)
    _ = try store.logBottle(volumeML: 0, contents: .formula, for: baby.id)

    #expect(try LogDefaults(store: store).lastBottleVolumeML(for: baby.id) == 120)
}

/// An older phone's feed can hold several bottles: the last one is the
/// previous bottle.
@MainActor
@Test func theLastBottleOfAFeedWithSeveralIsThePreviousBottle() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000)))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let feed = FeedEntry(babyID: baby.id, kind: .bottle, startedAt: store.now, timeZoneID: "UTC")
    feed.components = [
        .bottle(id: UUID(), volumeML: 40, contents: .formula),
        .bottle(id: UUID(), volumeML: 70, contents: .breastMilk),
    ]
    feed.endedAt = store.now
    store.context.insert(feed)
    try store.record(.create, on: feed)

    #expect(try LogDefaults(store: store).lastBottleVolumeML(for: baby.id) == 70)
}
```

If `logBottle(volumeML: 0, …)` throws in `aZeroVolumeLastBottleFallsBackToTheDefault`, build that bottle directly instead, exactly as the last test builds its feed, with one `.bottle(id: UUID(), volumeML: 0, contents: .formula)` component and `startedAt: store.now` after the clock advance.

In `OnePartTimerTests.swift:187`, change `mostUsedBottleVolumeML` to `lastBottleVolumeML`.

- [ ] **Step 2: Run to verify it fails**

Run: `cd Packages/BabyTracker && swift test --filter LogDefaultsTests`
Expected: FAIL to compile, `has no member 'lastBottleVolumeML'`.

- [ ] **Step 3: Implement**

In `LogDefaults.swift`, change the type's doc comment's first line to "Pre-fills that make bottle logging two taps: what the previous bottle was." Delete `recentBottles(for:limit:)` and `mostUsedBottleVolumeML(for:)`, and replace `lastBottleContents(for:)` with:

```swift
    /// The baby's most recent bottle feed.
    ///
    /// Fetched ascending and read from the end rather than fetched descending
    /// and read from the front: SwiftData's sort is stable, and ties (e.g. two
    /// bottles logged in the same test tick) keep their original insertion
    /// order regardless of the requested sort direction. Reading the tail of
    /// an ascending fetch means the most-recently-inserted entry wins even
    /// when timestamps tie; reading the head of a descending fetch does not.
    private func lastBottle(for babyID: UUID) throws -> FeedEntry? {
        let bottle = FeedKind.bottle.rawValue
        let realmKey = store.activeRealm.encodedKey
        var descriptor = FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.babyID == babyID && $0.realmKey == realmKey && !$0.isTombstoned && $0.kindRaw == bottle }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .forward)]
        return try store.context.fetch(descriptor).last
    }

    /// What the previous bottle held. Never a bottle saved at 0 ml: every
    /// total leaves a zero-volume bottle out, so it is not an amount to
    /// repeat.
    public func lastBottleVolumeML(for babyID: UUID) throws -> Double {
        guard let volume = try lastBottle(for: babyID)?.bottleComponents.last?.volumeML, volume > 0 else {
            return Self.fallbackVolumeML
        }
        return volume
    }

    public func lastBottleContents(for babyID: UUID) throws -> BottleContents {
        try lastBottle(for: babyID)?.bottleComponents.last?.bottleContents ?? Self.fallbackContents
    }
```

Then rename the call in each of the five caller files listed above from `mostUsedBottleVolumeML` to `lastBottleVolumeML`. In `BabyStore+Parts.swift` and `OutOfAppFeedActions.swift`, where a nearby comment says "usual amount" or "habitual volume", change the wording to "the previous bottle's amount".

Run: `grep -rn "mostUsedBottleVolumeML" Packages Apps`
Expected: no output.

- [ ] **Step 4: Run to verify it passes**

Run: `cd Packages/BabyTracker && swift test --filter "LogDefaultsTests|OnePartTimerTests|OutOfAppFeedActionsTests|BottleSheetModelTests"`
Expected: PASS. If a test in those suites asserted the most-common amount, change its expectation to the previous bottle's amount and say so in the commit message.

- [ ] **Step 5: Commit**

Bump `EXPECTED_TESTS` by 2 (five tests where there were three). Run `make test`; expected `OK`.

```bash
/usr/bin/git add -A Makefile Packages/BabyTracker
/usr/bin/git commit -m "feat(bottle): the default amount is the previous bottle's (+2 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Contents on the amount screen

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Parts.swift:85-94, 143-160`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/BottleSheet.swift` (model `init`, `done()`, `startBody`, `amountBody`)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/OnePartTimerTests.swift`, `Packages/BabyTracker/Tests/BabyStoreTests/StoreRevisionTests.swift:79`, `Packages/BabyTracker/Tests/BabyStoreTests/SingleFormatWriteTests.swift:67`, `Packages/BabyTracker/Tests/LoggingFeatureTests/BottleSheetModelTests.swift`

**Interfaces:**
- Produces on `BabyStore`:
  - `func stopBottle(_ timer: ActiveTimer, volumeML: Double, contents: BottleContents? = nil, expectedWriteContext: RealmWriteContext? = nil) throws -> FeedEntry?`
  - `func setBottle(volumeML: Double, contents: BottleContents, onFeed feedID: UUID) throws` (replaces `setBottleVolume(_:onFeed:)`)
  - `func bottlePart(onFeed feedID: UUID) throws -> (volumeML: Double, contents: BottleContents)?`

- [ ] **Step 1: Write the failing tests**

In `OnePartTimerTests.swift`, replace `setBottleVolumeRewritesTheBottleComponent` and update the three refusal tests to the new call. The refusals keep their expectations:

```swift
    @Test func setBottleRewritesTheVolumeAndContentsInOneWrite() throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        let before = store.revision
        try store.setBottle(volumeML: 90, contents: .breastMilk, onFeed: feed.id)
        #expect(feed.bottleComponents.map(\.volumeML) == [90])
        #expect(feed.bottleComponents.map(\.bottleContents) == [.breastMilk])
        #expect(store.revision == before + 1)
    }

    @Test func aChosenContentsBeatsTheTimersWhenABottleStops() throws {
        let (store, baby, clock) = try fixture()
        let timer = try store.startPart(.bottle(.formula), for: baby)
        clock.advance(by: 120)
        let entry = try #require(try store.stopBottle(timer, volumeML: 90, contents: .mixed))
        #expect(entry.bottleComponents.first?.bottleContents == .mixed)
    }

    @Test func bottlePartReadsTheSingleBottleOfAFeed() throws {
        let (store, baby, _) = try fixture()
        let bottle = try store.logBottle(volumeML: 60, contents: .breastMilk, for: baby)
        let solids = try store.logSolids(foods: ["pear"], amount: .some, for: baby)
        let part = try #require(try store.bottlePart(onFeed: bottle.id))
        #expect(part.volumeML == 60)
        #expect(part.contents == .breastMilk)
        #expect(try store.bottlePart(onFeed: solids.id) == nil)
    }
```

In the three refusal tests change `try store.setBottleVolume(80, onFeed: feed.id)` to `try store.setBottle(volumeML: 80, contents: .formula, onFeed: feed.id)` and `setBottleVolume(0, …)` to `setBottle(volumeML: 0, contents: .formula, onFeed: feed.id)`, and rename the tests' `setBottleVolume` prefix to `setBottle`. Make the same call change at `StoreRevisionTests.swift:79` (keep the label string `"setBottleVolume"` → `"setBottle"`) and `SingleFormatWriteTests.swift:67`.

If `store.revision == before + 1` does not hold for the existing single-write path either (check by running the old `setBottleVolume` test with that assertion first), drop that one line; the single `update(feed)` call in the implementation is what makes it one write.

In `BottleSheetModelTests`, add:

```swift
    @Test func theAmountScreenStartsFromTheBottlesOwnContents() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logBottle(volumeML: 80, contents: .formula, for: baby)
        clock.advance(by: 3_600)
        let timer = try store.startPart(.bottle(.breastMilk), for: baby)
        let stopped = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .stopped(timerID: timer.id))
        #expect(stopped.contents == .breastMilk)
        // The previous bottle's amount.
        #expect(stopped.volumeML == 80)
    }

    @Test func doneAfterATimedBottleSavesTheChosenContents() async throws {
        let (store, baby, clock) = try fixture()
        let timer = try store.startPart(.bottle(.breastMilk), for: baby)
        clock.advance(by: 400)
        let model = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .stopped(timerID: timer.id))
        model.contents = .formula
        model.volumeML = 70
        try await model.done()
        let saved = try #require(try store.lastFeed(for: baby))
        #expect(saved.bottleComponents.first?.bottleContents == .formula)
        #expect(saved.totalBottleVolumeML == 70)
    }

    @Test func setAmountStartsFromTheSavedBottleAndSavesContentsWithIt() async throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logBottle(volumeML: 60, contents: .breastMilk, for: baby)
        let model = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .amount(feedID: feed.id))
        #expect(model.contents == .breastMilk)
        #expect(model.volumeML == 60)
        model.contents = .mixed
        model.volumeML = 100
        try await model.done()
        #expect(feed.bottleComponents.first?.bottleContents == .mixed)
        #expect(feed.totalBottleVolumeML == 100)
    }

    /// Stopped from the other phone while this screen was open: nothing is
    /// saved, and the screen says so rather than writing a second bottle.
    @Test func doneAfterTheTimerIsGoneSavesNothing() async throws {
        let (store, baby, clock) = try fixture()
        let timer = try store.startPart(.bottle(.breastMilk), for: baby)
        clock.advance(by: 400)
        let model = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .stopped(timerID: timer.id))
        model.contents = .formula
        _ = try store.stopBottle(timer, volumeML: 50)
        await #expect(throws: BottleTimerError.timerChanged) { try await model.done() }
        let saved = try #require(try store.lastFeed(for: baby))
        #expect(saved.bottleComponents.first?.bottleContents == .breastMilk)
        #expect(saved.totalBottleVolumeML == 50)
    }
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd Packages/BabyTracker && swift test --filter "OnePartTimerTests|BottleSheetModelTests"`
Expected: FAIL to compile, `has no member 'setBottle'`.

- [ ] **Step 3: Implement the store**

In `BabyStore+Parts.swift`, replace `stopBottle` with:

```swift
    /// `contents`: what the caregiver chose on the amount screen. Without one
    /// it is what the bottle was started with — and a bottle this device
    /// didn't start carries none (`startBottle` sets none), so the best guess
    /// is what was logged last, not the generic fallback: the same reasoning
    /// `LogDefaults` already applies to a fresh Bottle sheet.
    @discardableResult
    func stopBottle(_ timer: ActiveTimer, volumeML: Double, contents chosen: BottleContents? = nil,
                    expectedWriteContext: RealmWriteContext? = nil) throws -> FeedEntry? {
        let contents = try chosen ?? timer.bottleContents ?? LogDefaults(store: self).lastBottleContents(for: timer.babyID)
        try finishBottle(volumeML: volumeML, contents: contents, on: timer, expectedWriteContext: expectedWriteContext)
        return try stopFeed(timer, expectedWriteContext: expectedWriteContext)
    }
```

Replace `setBottleVolume(_:onFeed:)` with:

```swift
    /// The single bottle of a feed logged earlier, or nil when the feed is
    /// gone or does not hold exactly one.
    func bottlePart(onFeed feedID: UUID) throws -> (volumeML: Double, contents: BottleContents)? {
        let realmKey = activeRealm.encodedKey
        guard let feed = try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate {
            $0.id == feedID && $0.realmKey == realmKey && !$0.isTombstoned
        })).first else { return nil }
        let bottles = feed.components.filter { $0.bottleContents != nil }
        guard bottles.count == 1, let volume = bottles[0].volumeML, let contents = bottles[0].bottleContents else {
            return nil
        }
        return (volume, contents)
    }

    /// Rewrites the single bottle component's volume and contents on a feed
    /// logged earlier, in one write. Refuses a feed with no bottle component
    /// or more than one — which one would these belong to? — and a volume
    /// that is not positive.
    func setBottle(volumeML: Double, contents: BottleContents, onFeed feedID: UUID) throws {
        guard volumeML > 0 else { throw PartError.invalidVolume }
        let realmKey = activeRealm.encodedKey
        guard let feed = try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate {
            $0.id == feedID && $0.realmKey == realmKey && !$0.isTombstoned
        })).first else { throw PartError.missingFeed }
        guard feed.components.filter({ $0.bottleContents != nil }).count == 1 else {
            throw PartError.notASingleBottle
        }
        feed.components = feed.components.map { component in
            guard case let .bottle(id, _, _, seconds, startedAt, endedAt) = component else { return component }
            return .bottle(id: id, volumeML: volumeML, contents: contents, seconds: seconds, startedAt: startedAt, endedAt: endedAt)
        }
        try update(feed)
    }
```

- [ ] **Step 4: Implement the model and the view**

In `BottleSheetModel.init`, replace the two lines that set `contents` and `volumeML` with:

```swift
        let defaults = LogDefaults(store: store)
        var contents = (try? defaults.lastBottleContents(for: babyID)) ?? LogDefaults.fallbackContents
        var volumeML = (try? defaults.lastBottleVolumeML(for: babyID)) ?? LogDefaults.fallbackVolumeML
        // The amount screens start from the bottle they finish, not from
        // history: what it was started with, or what it was saved as.
        switch mode {
        case .start, .edit:
            break
        case .stopped(let timerID):
            if let started = (try? store.activeTimer(id: timerID))?.bottleContents { contents = started }
        case .amount(let feedID):
            if let saved = try? store.bottlePart(onFeed: feedID) {
                contents = saved.contents
                if saved.volumeML > 0 { volumeML = saved.volumeML }
            }
        }
        self.contents = contents
        self.volumeML = volumeML
```

In `done()`, pass the contents through:

```swift
        case .stopped(let timerID):
            guard let timer = try store.activeTimer(id: timerID) else { throw BottleTimerError.timerChanged }
            let volumeML = volumeML
            let contents = contents
            try await LiveActivityController.stopping(timer, in: store) {
                _ = try store.stopBottle(timer, volumeML: volumeML, contents: contents)
            }
        case .amount(let feedID):
            try store.setBottle(volumeML: volumeML, contents: contents, onFeed: feedID)
```

Update the doc comment above `done()`: "`.amount` only sets the volume" becomes "`.amount` sets the volume and contents".

In `BottleSheet`, extract the picker from `startBody` so both bodies share it:

```swift
    /// Breast milk, Formula or Mixed. On the amount screens it is what the
    /// bottle held, corrected before it is saved.
    private var contentsPicker: some View {
        Picker(BottleSheetCopy.contentsLabel, selection: $model.contents) {
            Text(BottleSheetCopy.breastMilk).tag(BottleContents.breastMilk)
            Text(BottleSheetCopy.formula).tag(BottleContents.formula)
            Text(BottleSheetCopy.mixed).tag(BottleContents.mixed)
        }
        .pickerStyle(.segmented)
        .accessibilityIdentifier("bottle.contents")
    }
```

`startBody` uses `contentsPicker` in place of its inline `Picker`. In `amountBody`, insert `contentsPicker` directly after the caption `Text` and before the volume `HStack`.

- [ ] **Step 5: Run to verify it passes**

Run: `cd Packages/BabyTracker && swift test --filter "OnePartTimerTests|BottleSheetModelTests|StoreRevisionTests|SingleFormatWriteTests"`
Expected: PASS.

Run: `grep -rn "setBottleVolume" Packages Apps`
Expected: no output.

- [ ] **Step 6: Commit**

Bump `EXPECTED_TESTS` by 6 (two new store tests net of the renamed one, four new model tests). Run `make test`; expected `OK`. If the total is off, count `@Test` lines added minus removed in this task's diff and use that.

```bash
/usr/bin/git add -A Makefile Packages/BabyTracker
/usr/bin/git commit -m "feat(bottle): contents on the amount screen, saved with the amount (+6 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Each action's icon, colour and name

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyUI/HomeActionKind+Display.swift`
- Modify: `Packages/BabyTracker/Sources/BabyUI/Localizable.xcstrings`
- Test: `Packages/BabyTracker/Tests/BabyUITests/HomeActionDisplayTests.swift`

**Interfaces:**
- Consumes: `HomeActionKind` (Task 1).
- Produces: `extension HomeActionKind { var icon: BabyIcon; var color: Color; var title: String }` in BabyUI.

- [ ] **Step 1: Write the failing test**

```swift
import Testing
import BabyCore
@testable import BabyUI

@Suite struct HomeActionDisplayTests {
    @Test func everyActionHasItsOwnName() {
        let titles = HomeActionKind.allCases.map(\.title)
        #expect(titles.allSatisfy { !$0.isEmpty })
        #expect(Set(titles).count == HomeActionKind.allCases.count)
    }

    @Test func actionsUseTheirTrackersLook() {
        #expect(HomeActionKind.breast.icon == .feed)
        #expect(HomeActionKind.bottle.icon == .bottle)
        #expect(HomeActionKind.meds.icon == Tracker.supplement.icon)
        #expect(HomeActionKind.meds.title == Tracker.supplement.title)
        #expect(HomeActionKind.growth.title == Tracker.growth.title)
    }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd Packages/BabyTracker && swift test --filter HomeActionDisplayTests`
Expected: FAIL to compile, `has no member 'title'`.

- [ ] **Step 3: Implement**

`HomeActionKind+Display.swift`:

```swift
import SwiftUI
import BabyCore

/// How a Home action looks wherever it is offered: the dock, the ··· sheet,
/// the settings list and the Quick Log widget.
public extension HomeActionKind {
    var icon: BabyIcon {
        switch self {
        case .breast: .feed
        case .bottle: .bottle
        case .sleep: .sleep
        case .diaper: .diaper
        case .solids: .solids
        case .meds: .meds
        case .growth: .growth
        }
    }

    var color: Color {
        switch self {
        case .breast, .bottle: Palette.feed
        case .sleep: Palette.sleep
        case .diaper: Palette.diaper
        case .solids: Palette.solids
        case .meds: Palette.meds
        case .growth: Palette.growth
        }
    }

    var title: String {
        switch self {
        case .breast: String(localized: "Breast", bundle: .module)
        case .bottle: String(localized: "Bottle", bundle: .module)
        case .sleep: Tracker.sleep.title
        case .diaper: Tracker.diaper.title
        case .solids: Tracker.solids.title
        case .meds: Tracker.supplement.title
        case .growth: Tracker.growth.title
        }
    }
}
```

Add the two strings. First read what HomeFeature already calls Bottle, so the two catalogs agree:

```bash
python3 -c "import json;c=json.load(open('Packages/BabyTracker/Sources/HomeFeature/Localizable.xcstrings'));print({l:v['stringUnit']['value'] for l,v in c['strings']['Bottle']['localizations'].items()})"
```

Write `/tmp/home-buttons-babyui.json`, using HomeFeature's values for `Bottle` if they differ from these:

```json
{
  "Breast": {"de": "Brust", "fr": "Sein", "es": "Pecho", "it": "Seno", "uk": "Груди"},
  "Bottle": {"de": "Flasche", "fr": "Biberon", "es": "Biberón", "it": "Biberon", "uk": "Пляшечка"}
}
```

Run: `python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/BabyUI/Localizable.xcstrings /tmp/home-buttons-babyui.json`

- [ ] **Step 4: Run to verify it passes**

Run: `cd Packages/BabyTracker && swift test --filter HomeActionDisplayTests`
Expected: PASS, 2 tests.

- [ ] **Step 5: Commit**

Bump `EXPECTED_TESTS` by 2. Run `make test`; expected `OK`.

```bash
/usr/bin/git add Makefile Packages/BabyTracker/Sources/BabyUI Packages/BabyTracker/Tests/BabyUITests/HomeActionDisplayTests.swift
/usr/bin/git commit -m "feat(home): each Home action's icon, colour and name (+2 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: The dock follows the order

**Files:**
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeScreen.swift:82-88, 94, 142-145`
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeViewModel.swift`
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeView.swift` (`HomeAction`, `init`, `onChange`)
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeDock.swift`
- Test: `Packages/BabyTracker/Tests/HomeFeatureTests/HomeScreenTests.swift`, `Packages/BabyTracker/Tests/HomeFeatureTests/HomeViewModelTests.swift`

**Interfaces:**
- Consumes: `HomeLayout`, `HomeActionKind` (Task 1); `icon`, `color`, `title` (Task 5).
- Produces:
  - `Dock.start(suggested: BreastSide, big: HomeActionKind, tiles: [HomeActionKind])`, `Dock.feedRunning(timerID: UUID, isBottle: Bool, tiles: [HomeActionKind])`, `Dock.sleepRunning(timerID: UUID, tiles: [HomeLayout.RunningTile])`, `Dock.readOnly`.
  - `HomeScreen.make(snapshot:suggested:canLog:layout:now:)` with `layout: HomeLayout = HomeLayout()`.
  - `HomeViewModel.homeOrder: [HomeActionKind]` (settable), and `init(store:babyID:volumeUnit:homeOrder:)`.
  - `HomeView.init(…, homeOrder: [HomeActionKind] = HomeActionKind.defaultOrder, …)`.
  - `HomeAction` gains `.solids`, `.meds`, `.growth`.

- [ ] **Step 1: Write the failing tests**

Add to `HomeScreenTests`:

```swift
    @Test func theFirstActionOfTheOrderIsTheBigButton() throws {
        let (store, baby, clock) = try fixture()
        let layout = HomeLayout(order: [.bottle, .breast, .solids, .diaper, .sleep, .meds, .growth])
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left,
                                     canLog: true, layout: layout, now: clock.now)
        #expect(screen.dock == .start(suggested: .left, big: .bottle, tiles: [.breast, .solids, .diaper]))
    }

    @Test func runningDocksCarryTheOrdersTiles() throws {
        let (store, baby, clock) = try fixture()
        let layout = HomeLayout(order: [.bottle, .solids, .diaper, .growth, .breast, .sleep, .meds])
        let sleep = try store.startSleep(for: baby)
        var screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left,
                                     canLog: true, layout: layout, now: clock.now)
        #expect(screen.dock == .sleepRunning(timerID: sleep.id, tiles: [.feed, .action(.solids), .action(.diaper)]))
        let left = try store.startPart(.breast(.left), for: baby)
        screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .right,
                                 canLog: true, layout: layout, now: clock.now)
        // Feed and sleep together: the feed's dock, as before.
        #expect(screen.dock == .feedRunning(timerID: left.id, isBottle: false, tiles: [.solids, .diaper, .growth]))
    }
```

Add to `HomeViewModelTests` (use that file's existing way of building a store and baby; if it has none, use `fixture()` from `Support/PartTestHelpers.swift`):

```swift
    @Test func changingTheOrderReshapesTheDockOnTheNextRefresh() throws {
        let (store, baby, _) = try fixture()
        let model = HomeViewModel(store: store, babyID: baby)
        try model.refresh()
        #expect(model.screen.dock == .start(suggested: try store.suggestedSide(for: baby), big: .breast,
                                            tiles: [.bottle, .sleep, .diaper]))
        model.homeOrder = [.solids, .breast, .bottle, .sleep, .diaper, .meds, .growth]
        try model.refresh()
        #expect(model.screen.dock == .start(suggested: try store.suggestedSide(for: baby), big: .solids,
                                            tiles: [.breast, .bottle, .sleep]))
    }
```

Update the existing expectations to the new cases. Find them:

```bash
grep -rn "\.start(suggested\|\.feedRunning(timerID\|\.sleepRunning(timerID" Packages/BabyTracker Apps
```

With the default order, each becomes:
- `.start(suggested: X)` → `.start(suggested: X, big: .breast, tiles: [.bottle, .sleep, .diaper])`
- `.feedRunning(timerID: X, isBottle: Y)` → `.feedRunning(timerID: X, isBottle: Y, tiles: [.diaper])`
- `.sleepRunning(timerID: X)` → `.sleepRunning(timerID: X, tiles: [.feed, .action(.diaper)])`

Pattern matches that bind (`case .feedRunning(let id, _)`) gain one more `_`.

- [ ] **Step 2: Run to verify it fails**

Run: `cd Packages/BabyTracker && swift test --filter "HomeScreenTests|HomeViewModelTests"`
Expected: FAIL to compile, `extra argument 'layout' in call`.

- [ ] **Step 3: Implement `HomeScreen` and `HomeViewModel`**

`HomeScreen.swift`, the enum:

```swift
public enum Dock: Equatable, Sendable {
    /// Idle or in a break: the order's first action as the big button — Left
    /// and Right when that is Breast — then its three tiles, then More.
    case start(suggested: BreastSide, big: HomeActionKind, tiles: [HomeActionKind])
    /// Stop, then the tiles that do not conflict with a running feed.
    case feedRunning(timerID: UUID, isBottle: Bool, tiles: [HomeActionKind])
    /// Wake up, then the Feed chooser and the tiles that do not conflict.
    case sleepRunning(timerID: UUID, tiles: [HomeLayout.RunningTile])
    case readOnly
}
```

`make` gains the parameter and uses it:

```swift
    static func make(snapshot s: HomeSnapshot, suggested: BreastSide, canLog: Bool,
                     layout: HomeLayout = HomeLayout(), now: Date) -> HomeScreen {
```

```swift
        let dock: Dock = !canLog ? .readOnly
            : feedTimer.map {
                .feedRunning(timerID: $0.id, isBottle: $0.currentBottleStartedAt != nil, tiles: layout.tilesWhileFeedRuns)
            }
            ?? sleepTimer.map { .sleepRunning(timerID: $0.id, tiles: layout.tilesWhileSleepRuns) }
            ?? .start(suggested: suggested, big: layout.big, tiles: layout.tiles)
```

`HomeViewModel.swift`:

```swift
    /// A preference, like `volumeUnit`: the order of this baby's Home
    /// buttons can change while this screen is alive.
    public var homeOrder: [HomeActionKind]
```

```swift
    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres,
                homeOrder: [HomeActionKind] = HomeActionKind.defaultOrder) {
        self.store = store
        self.babyID = babyID
        self.volumeUnit = volumeUnit
        self.homeOrder = homeOrder
    }
```

```swift
    public func screen(at now: Date) -> HomeScreen {
        guard let snapshot else { return canLog ? .empty : .emptyReadOnly }
        return HomeScreen.make(snapshot: snapshot, suggested: suggestedSide, canLog: canLog,
                               layout: HomeLayout(order: homeOrder), now: now)
    }
```

`HomeScreen.empty` becomes `dock: .start(suggested: .left, big: .breast, tiles: HomeLayout().tiles)`.

- [ ] **Step 4: Implement `HomeAction`, `HomeView` and `HomeDock`**

`HomeView.swift`, `HomeAction`:

```swift
public enum HomeAction: Equatable, Sendable {
    /// The Bottle sheet: start a timed bottle, or log what was drunk.
    case bottle
    case diaper
    case solids
    /// The Meds panel: the saved meds as chips, and "+".
    case meds
    case growth
    /// The ··· sheet: whatever the dock has no room for, and Past entry.
    case more
    /// History at today, from the today strip.
    case history

    /// The ··· control's name, for whatever the app presents behind it.
    public static var moreTitle: String { HomeCopy.more }
}
```

`HomeView`: add the stored property `private let homeOrder: [HomeActionKind]` beside `volumeUnit`, the `init` parameter `homeOrder: [HomeActionKind] = HomeActionKind.defaultOrder` after `volumeUnit:`, assign it, and pass it to the model: `HomeViewModel(store: store, babyID: babyID, volumeUnit: volumeUnit, homeOrder: homeOrder)`. After the `.onChange(of: volumeUnit)` modifier add:

```swift
        .onChange(of: homeOrder) { _, order in
            model.homeOrder = order
            reload()
        }
```

`HomeDock.swift`: replace the `body`, and the `diaper` helper, with:

```swift
    @State private var isChoosingFeed = false
    @State private var isChoosingBreast = false

    var body: some View {
        switch dock {
        case .readOnly:
            Text(HomeCopy.readOnly)
                .font(.footnote)
                .multilineTextAlignment(.center)
                .foregroundStyle(Palette.secondaryText)
                .padding(.horizontal, 32)
                .accessibilityIdentifier("home.readOnly")
        case let .start(suggested, big, tiles):
            GlassDock {
                VStack(spacing: 8) {
                    if big == .breast {
                        HStack(spacing: 8) {
                            side(.left, suggested: suggested == .left)
                            side(.right, suggested: suggested == .right)
                        }
                    } else {
                        bigAction(big)
                    }
                    HStack(spacing: 8) {
                        ForEach(tiles, id: \.self) { actionTile($0) }
                        more
                    }
                }
            }
        case .feedRunning(_, _, let tiles):
            GlassDock {
                VStack(spacing: 8) {
                    primary(HomeCopy.stop, "home.stop", action: onStopFeed)
                    HStack(spacing: 8) {
                        ForEach(tiles, id: \.self) { actionTile($0) }
                        more
                    }
                }
            }
        case .sleepRunning(_, let tiles):
            GlassDock {
                VStack(spacing: 8) {
                    primary(HomeCopy.wakeUp, "home.wake", action: onWake)
                    HStack(spacing: 8) {
                        ForEach(tiles, id: \.self) { slot in
                            switch slot {
                            case .feed: feedChooser
                            case .action(let kind): actionTile(kind)
                            }
                        }
                        more
                    }
                }
            }
        }
    }

    /// A baby can fall asleep at the breast, so a feed may start while the
    /// sleep keeps running.
    private var feedChooser: some View {
        tile(.feed, Palette.feed, Tracker.feed.title, "home.feed") { isChoosingFeed = true }
            .confirmationDialog(Tracker.feed.title, isPresented: $isChoosingFeed) {
                Button(HomeL10n.breast(.left)) { onStartBreast(.left) }
                Button(HomeL10n.breast(.right)) { onStartBreast(.right) }
                Button(HomeCopy.bottle) { onAction(.bottle) }
            }
    }

    /// One action as a small tile. Breast has two sides, so as a tile it
    /// asks which.
    @ViewBuilder
    private func actionTile(_ kind: HomeActionKind) -> some View {
        let button = tile(kind.icon, kind.color, kind.title, "home.\(kind.rawValue)") { run(kind) }
        if kind == .breast {
            button.confirmationDialog(kind.title, isPresented: $isChoosingBreast) {
                Button(HomeL10n.breast(.left)) { onStartBreast(.left) }
                Button(HomeL10n.breast(.right)) { onStartBreast(.right) }
            }
        } else {
            button
        }
    }

    /// The order's first action when it is not Breast: one wide button with
    /// its name, the height of Left and Right.
    private func bigAction(_ kind: HomeActionKind) -> some View {
        Button { run(kind) } label: {
            HStack(spacing: 8) {
                BabyIconView(kind.icon, size: 24)
                Text(kind.title)
                    .font(.title3.weight(.bold))
                    .foregroundStyle(Palette.primaryText)
                    .lineLimit(1)
                    .minimumScaleFactor(0.7)
            }
        }
        .buttonStyle(DockTileStyle(tint: kind.color, height: 64))
        .accessibilityIdentifier("home.\(kind.rawValue)")
    }

    private func run(_ kind: HomeActionKind) {
        switch kind {
        case .breast: isChoosingBreast = true
        case .sleep: onStartSleep()
        case .bottle: onAction(.bottle)
        case .diaper: onAction(.diaper)
        case .solids: onAction(.solids)
        case .meds: onAction(.meds)
        case .growth: onAction(.growth)
        }
    }
```

Update the type's doc comment to: "The thumb zone. Start what the baby's order puts first while idle or in a break; Stop or Wake up while a part runs. Every control is at least 44 pt and every icon-only control has a spoken label."

`Apps/BabyTrackerApp/RootView.swift` must still compile: in `handleHomeAction` add the three cases now, mapped to sheets that already exist (`.meds` gets its own sheet in Task 7):

```swift
        case .solids: sheet = .solids
        case .growth: sheet = .measurement
        case .meds: sheet = .more
```

- [ ] **Step 5: Run to verify it passes**

Run: `cd Packages/BabyTracker && swift test --filter "HomeScreenTests|HomeViewModelTests|RunningTimerEditTests"`
Expected: PASS.

Run (after `pgrep xcodebuild` prints nothing): `xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build`
Expected: `BUILD SUCCEEDED`.

- [ ] **Step 6: Commit**

Bump `EXPECTED_TESTS` by 3. Run `make test`; expected `OK`.

```bash
/usr/bin/git add -A Makefile Packages/BabyTracker Apps/BabyTrackerApp/RootView.swift
/usr/bin/git commit -m "feat(home): the dock is drawn from the baby's order (+3 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: The ··· sheet and the Meds panel

**Files:**
- Create: `Packages/BabyTracker/Sources/LoggingFeature/MedChipsRow.swift`
- Create: `Packages/BabyTracker/Sources/LoggingFeature/MedsSheet.swift`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/MoreSheet.swift`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/LogCoordinator.swift` (`LogSheet`, `LogSheetView`)
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/Localizable.xcstrings`
- Modify: `Apps/BabyTrackerApp/RootView.swift` (`handleHomeAction`)
- Test: `Packages/BabyTracker/Tests/LoggingFeatureTests/MoreSheetModelTests.swift`

**Interfaces:**
- Consumes: `HomeLayout.overflow(feedRunning:sleepRunning:)` (Task 1); `icon`, `color`, `title` (Task 5).
- Produces:
  - `MoreSheetModel.init(store:babyID:order:)` with `order: [HomeActionKind] = HomeActionKind.defaultOrder`; `tiles: [HomeActionKind]`; `showsMeds: Bool`; `func startBreast(_ side: BreastSide) async throws`; `func startSleep() async throws`.
  - `MoreSheet.init(store:babyID:order:onSwitchTo:onEditHomeButtons:onDone:)`.
  - `LogSheet.meds`.
  - `LogSheetView.init(sheet:store:babyID:unit:homeOrder:onDone:onSwitchTo:onEditHomeButtons:)` with `homeOrder: [HomeActionKind] = HomeActionKind.defaultOrder` and `onEditHomeButtons: (() -> Void)? = nil`.

- [ ] **Step 1: Write the failing tests**

Add to `MoreSheetModelTests`:

```swift
    @Test func theDefaultOrderShowsSolidsGrowthAndTheMedChips() throws {
        let (store, baby, _) = try fixture()
        let model = MoreSheetModel(store: store, babyID: baby)
        #expect(model.tiles == [.solids, .growth])
        #expect(model.showsMeds)
    }

    @Test func whatTheDockHasNoRoomForIsATile() throws {
        let (store, baby, _) = try fixture()
        let model = MoreSheetModel(store: store, babyID: baby,
                                   order: [.solids, .meds, .growth, .diaper, .breast, .bottle, .sleep])
        #expect(model.tiles == [.breast, .bottle, .sleep])
        // Meds is on the dock, where its own panel carries the chips.
        #expect(!model.showsMeds)
    }

    @Test func duringASleepTheSheetKeepsFeedActions() throws {
        let (store, baby, _) = try fixture()
        _ = try store.startSleep(for: baby)
        let model = MoreSheetModel(store: store, babyID: baby,
                                   order: [.solids, .meds, .growth, .diaper, .breast, .bottle, .sleep])
        #expect(model.tiles == [.breast, .bottle])
    }

    @Test func duringAFeedTheSheetLeavesOutEveryTimedAction() throws {
        let (store, baby, _) = try fixture()
        _ = try store.startPart(.breast(.left), for: baby)
        let model = MoreSheetModel(store: store, babyID: baby,
                                   order: [.solids, .meds, .growth, .diaper, .breast, .bottle, .sleep])
        #expect(model.tiles.isEmpty)
    }

    @Test func aBreastSideStartsFromTheSheet() async throws {
        let (store, baby, _) = try fixture()
        let model = MoreSheetModel(store: store, babyID: baby,
                                   order: [.solids, .meds, .growth, .diaper, .breast, .bottle, .sleep])
        try await model.startBreast(.right)
        #expect(try store.activeTimer(kind: .feed, for: baby)?.currentSide == .right)
        try await model.startSleep()
        #expect(try store.activeTimer(kind: .sleep, for: baby) != nil)
    }
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd Packages/BabyTracker && swift test --filter MoreSheetModelTests`
Expected: FAIL to compile, `extra argument 'order' in call`.

- [ ] **Step 3: Implement the model**

In `MoreSheet.swift`, add to `MoreSheetCopy`:

```swift
    public static let editHomeButtons = LoggingL10n.text("Edit Home buttons")
    public static func breast(_ side: BreastSide) -> String {
        LoggingL10n.text(side == .left ? "Left breast" : "Right breast")
    }
```

Replace `MoreSheetModel` with:

```swift
/// Everything the dock has no room for, by the baby's order, and the saved
/// meds while Meds itself is not on the dock.
@MainActor @Observable
public final class MoreSheetModel {
    private let store: BabyStore
    private let babyID: UUID

    /// Saved med names, most recent first.
    public private(set) var meds: [String] = []
    /// The actions offered as tiles, in order. Never Meds — its chips are
    /// the row below — and never an action that conflicts with a running
    /// timer.
    public let tiles: [HomeActionKind]
    public let showsMeds: Bool

    public init(store: BabyStore, babyID: UUID, order: [HomeActionKind] = HomeActionKind.defaultOrder) {
        self.store = store
        self.babyID = babyID
        self.meds = (try? store.supplementNames(for: babyID)) ?? []
        let timers = (try? store.activeTimers(for: babyID)) ?? []
        let overflow = HomeLayout(order: order).overflow(feedRunning: timers.contains { $0.kind == .feed },
                                                         sleepRunning: timers.contains { $0.kind == .sleep })
        self.tiles = overflow.filter { $0 != .meds }
        self.showsMeds = overflow.contains(.meds)
    }

    /// Logs `name` right now and refreshes the row, so a second tap before
    /// the sheet dismisses sees the same order the store would report.
    public func logMed(_ name: String) throws {
        try store.logSupplement(name: name, for: babyID)
        meds = (try? store.supplementNames(for: babyID)) ?? meds
    }

    /// Home's own start, for a baby whose order keeps Breast off the dock.
    public func startBreast(_ side: BreastSide) async throws {
        let runningID = try store.activeTimer(kind: .feed, for: babyID)?.id
        let timer = try store.startPart(.breast(side), for: babyID)
        guard timer.id != runningID else { return }
        await LiveActivityController.adoptOrStart(for: timer, in: store)
    }

    public func startSleep() async throws {
        guard try store.activeTimer(kind: .sleep, for: babyID) == nil else { return }
        let timer = try store.startSleep(for: babyID)
        await LiveActivityController.adoptOrStart(for: timer, in: store)
    }
}
```

- [ ] **Step 4: Run to verify the model passes**

Run: `cd Packages/BabyTracker && swift test --filter MoreSheetModelTests`
Expected: PASS, 7 tests.

- [ ] **Step 5: Implement the views**

`MedChipsRow.swift` — the chips moved out of `MoreSheet` unchanged, so the Meds panel can show the same row:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

/// The saved meds as horizontally scrolling chips, "+" always last. A tap
/// logs that med now and closes the sheet.
struct MedChipsRow: View {
    let model: MoreSheetModel
    /// "more" in the ··· sheet, "meds" in the Meds panel.
    let idPrefix: String
    let onAdd: () -> Void
    let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 8) {
                ForEach(Array(model.meds.enumerated()), id: \.offset) { index, name in
                    medChip(name, index: index)
                }
                addMedChip
            }
        }
    }

    private func medChip(_ name: String, index: Int) -> some View {
        Button {
            do {
                try model.logMed(name)
                errorReporter.clear()
                onDone()
            } catch {
                errorReporter.report(ErrorBannerCopy.saveFailed(what: ErrorSubject.supplement), error: error, category: AppLog.store)
            }
        } label: {
            HStack(spacing: 6) {
                BabyIconView(.meds, size: 17)
                Text(name).lineLimit(1)
            }
            .padding(.horizontal, 14)
            .frame(minHeight: 44)
        }
        .buttonStyle(.tracker(Palette.meds))
        .accessibilityIdentifier("\(idPrefix).med.\(index)")
    }

    private var addMedChip: some View {
        Button(action: onAdd) {
            BabyIconView(.plus, size: 17)
                .frame(width: 44, height: 44)
        }
        .buttonStyle(.tracker(Palette.secondaryText))
        .accessibilityLabel(SupplementSheetCopy.addName)
        .accessibilityIdentifier("\(idPrefix).addMed")
    }
}
```

`MedsSheet.swift`:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

/// Meds from the dock: the saved meds as chips, and "+" for a new one.
public struct MedsSheet: View {
    @State private var model: MoreSheetModel
    private let onSwitchTo: (LogSheet) -> Void
    private let onDone: () -> Void

    public init(store: BabyStore, babyID: UUID, onSwitchTo: @escaping (LogSheet) -> Void, onDone: @escaping () -> Void) {
        _model = State(initialValue: MoreSheetModel(store: store, babyID: babyID))
        self.onSwitchTo = onSwitchTo
        self.onDone = onDone
    }

    /// Presented with `fittedSheet`, so the title and Cancel are drawn here.
    public var body: some View {
        VStack(alignment: .leading, spacing: 20) {
            SheetHeader(Tracker.supplement.title, cancelTitle: MoreSheetCopy.cancel, onCancel: onDone)
                .padding(.horizontal, -20)
            MedChipsRow(model: model, idPrefix: "meds", onAdd: { onSwitchTo(.supplement) }, onDone: onDone)
        }
        .padding(.horizontal, 20)
        .padding(.bottom, 12)
    }
}
```

`MoreSheet`: replace the view's stored properties, `init`, `body` and `tiles`, and delete its `medChip` and `addMedChip` (now in `MedChipsRow`). Keep `tile(title:icon:color:id:action:)` as it is.

```swift
public struct MoreSheet: View {
    @State private var model: MoreSheetModel
    private let onSwitchTo: (LogSheet) -> Void
    private let onEditHomeButtons: (() -> Void)?
    private let onDone: () -> Void
    @State private var isChoosingBreast = false
    @Environment(\.errorReporter) private var errorReporter
    /// Three tiles share a row at the ordinary sizes; at the accessibility
    /// sizes a third of the sheet split "Solids" mid-word, so they stack.
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(
        store: BabyStore,
        babyID: UUID,
        order: [HomeActionKind] = HomeActionKind.defaultOrder,
        onSwitchTo: @escaping (LogSheet) -> Void,
        onEditHomeButtons: (() -> Void)? = nil,
        onDone: @escaping () -> Void
    ) {
        _model = State(initialValue: MoreSheetModel(store: store, babyID: babyID, order: order))
        self.onSwitchTo = onSwitchTo
        self.onEditHomeButtons = onEditHomeButtons
        self.onDone = onDone
    }

    private var columns: [GridItem] {
        Array(repeating: GridItem(.flexible(), spacing: 10), count: typeSize.isAccessibilitySize ? 1 : 3)
    }

    /// Presented with `fittedSheet`: the sheet is as tall as this, so the
    /// title and Cancel are drawn here rather than in a navigation bar.
    public var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                SheetHeader(MoreSheetCopy.title, cancelTitle: MoreSheetCopy.cancel, onCancel: onDone)
                    .padding(.horizontal, -20)

                LazyVGrid(columns: columns, spacing: 10) { tiles }
                    .confirmationDialog(HomeActionKind.breast.title, isPresented: $isChoosingBreast) {
                        Button(MoreSheetCopy.breast(.left)) { start(ErrorSubject.feed) { try await model.startBreast(.left) } }
                        Button(MoreSheetCopy.breast(.right)) { start(ErrorSubject.feed) { try await model.startBreast(.right) } }
                    }

                if model.showsMeds {
                    VStack(alignment: .leading, spacing: 8) {
                        Text(Tracker.supplement.title)
                            .font(.footnote)
                            .foregroundStyle(Palette.secondaryText)
                        MedChipsRow(model: model, idPrefix: "more", onAdd: { onSwitchTo(.supplement) }, onDone: onDone)
                    }
                }

                if let onEditHomeButtons {
                    Button(MoreSheetCopy.editHomeButtons, action: onEditHomeButtons)
                        .font(.footnote.weight(.semibold))
                        .frame(maxWidth: .infinity, minHeight: 44)
                        .accessibilityIdentifier("more.editHomeButtons")
                }
            }
            .padding(.horizontal, 20)
            .padding(.bottom, 12)
        }
        // Past the screen (the largest text sizes) it scrolls rather than
        // clips; otherwise it does not bounce.
        .scrollBounceBehavior(.basedOnSize)
    }

    @ViewBuilder
    private var tiles: some View {
        ForEach(model.tiles, id: \.self) { kind in
            tile(title: kind.title, icon: kind.icon, color: kind.color, id: "more.\(kind.rawValue)") { run(kind) }
        }
        tile(title: MoreSheetCopy.pastEntry, icon: .clock, color: Palette.secondaryText, id: "more.pastEntry") {
            onSwitchTo(.pastEntry)
        }
    }

    private func run(_ kind: HomeActionKind) {
        switch kind {
        case .breast: isChoosingBreast = true
        case .sleep: start(ErrorSubject.sleep) { try await model.startSleep() }
        case .bottle: onSwitchTo(.bottle)
        case .diaper: onSwitchTo(.diaper)
        case .solids: onSwitchTo(.solids)
        case .growth: onSwitchTo(.measurement)
        // Never a tile: its chips are the row below.
        case .meds: onSwitchTo(.meds)
        }
    }

    /// Starts a timer and closes the sheet. A failure says so and leaves the
    /// sheet up.
    private func start(_ subject: String, _ work: @escaping () async throws -> Void) {
        Task {
            do {
                try await work()
                errorReporter.clear()
                onDone()
            } catch {
                errorReporter.report(ErrorBannerCopy.saveFailed(what: subject), error: error, category: AppLog.store)
            }
        }
    }
}
```

Update the comment above `MoreSheetCopy`'s model, formerly "Everything else the dock doesn't have room for: Solids, Growth and Past entry up top…": it is replaced by the model's new doc comment above.

`LogCoordinator.swift`:
- In `LogSheet`, after `case supplement`, add `/// Meds from the dock: the saved meds as chips.` and `case meds`. In `id`, add `case .meds: "meds"`. Change the `.more` comment to "The ··· sheet: what the dock has no room for, and Past entry."
- In `LogSheetView`, add stored properties `let homeOrder: [HomeActionKind]` and `let onEditHomeButtons: (() -> Void)?`, and `init` parameters `homeOrder: [HomeActionKind] = HomeActionKind.defaultOrder` (after `unit:`) and `onEditHomeButtons: (() -> Void)? = nil` (last). In `body`:

```swift
        case .meds:
            MedsSheet(store: store, babyID: babyID, onSwitchTo: onSwitchTo, onDone: onDone)
        case .more:
            MoreSheet(store: store, babyID: babyID, order: homeOrder, onSwitchTo: onSwitchTo,
                      onEditHomeButtons: onEditHomeButtons, onDone: onDone)
```

`RootView.handleHomeAction`: change `case .meds: sheet = .more` to `case .meds: sheet = .meds`.

Strings. Check which keys LoggingFeature's catalog lacks:

```bash
grep -c '"Left breast"\|"Right breast"\|"Edit Home buttons"' Packages/BabyTracker/Sources/LoggingFeature/Localizable.xcstrings
```

Write `/tmp/home-buttons-logging.json` with only the missing keys. For `Left breast` and `Right breast`, copy the five values from HomeFeature's catalog (same one-liner as Task 5 Step 3, with the key changed):

```json
{
  "Edit Home buttons": {"de": "Home-Tasten bearbeiten", "fr": "Modifier les boutons d’accueil", "es": "Editar botones de inicio", "it": "Modifica pulsanti Home", "uk": "Змінити кнопки на головній"},
  "Left breast": {"de": "Linke Brust", "fr": "Sein gauche", "es": "Pecho izquierdo", "it": "Seno sinistro", "uk": "Ліва грудь"},
  "Right breast": {"de": "Rechte Brust", "fr": "Sein droit", "es": "Pecho derecho", "it": "Seno destro", "uk": "Права грудь"}
}
```

Run: `python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/LoggingFeature/Localizable.xcstrings /tmp/home-buttons-logging.json`

- [ ] **Step 6: Verify**

Run: `cd Packages/BabyTracker && swift test --filter "MoreSheetModelTests|LoggingFeatureTests"`
Expected: PASS.

Run (after `pgrep xcodebuild` prints nothing): `xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build`
Expected: `BUILD SUCCEEDED`.

- [ ] **Step 7: Commit**

Bump `EXPECTED_TESTS` by 5. Run `make test`; expected `OK`.

```bash
/usr/bin/git add -A Makefile Packages/BabyTracker Apps/BabyTrackerApp/RootView.swift
/usr/bin/git commit -m "feat(home): the More sheet follows the order; Meds gets its own panel (+5 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: The Home buttons settings screen

**Files:**
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/SettingsViewModel.swift`
- Create: `Packages/BabyTracker/Sources/SettingsFeature/HomeButtonsView.swift`
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/SettingsView.swift:167-184` (Tracking section)
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/Localizable.xcstrings`
- Test: `Packages/BabyTracker/Tests/SettingsFeatureTests/SettingsViewModelTests.swift`

**Interfaces:**
- Consumes: `TrackingPreferences.homeOrder(for:)`, `setHomeOrder(_:for:)`, `resetHomeOrder(for:)` (Task 2); `HomeLayout.role(at:)` (Task 1); `icon`, `color`, `title` (Task 5).
- Produces:
  - On `SettingsViewModel`: `func homeOrder(for babyID: UUID) -> [HomeActionKind]`, `func setHomeOrder(_ order: [HomeActionKind], for babyID: UUID)`, `func moveHomeAction(at index: Int, by offset: Int, for babyID: UUID)`, `func resetHomeOrder(for babyID: UUID)`.
  - `public struct HomeButtonsView: View` with `init(settings: SettingsViewModel, babyID: UUID, babyName: String)` and `static var doneTitle: String`.
  - `enum HomeButtonsCopy` with `rowTitle`, `title(_ babyName:)`, `heading(at:)`, `breastFooter`, `reset`, `moveUp`, `moveDown`, `done`.

- [ ] **Step 1: Write the failing tests**

Add to `SettingsViewModelTests` (it needs `import BabyCore` and `import BabyStore` if it lacks them), building the model the way that file's other tests do (an isolated `UserDefaults` suite passed to `SettingsViewModel(defaults:)`):

```swift
    @Test func aHomeOrderIsWrittenThroughAndReadBack() {
        let d = isolatedDefaults()
        let model = SettingsViewModel(defaults: d)
        let baby = UUID()
        #expect(model.homeOrder(for: baby) == HomeActionKind.defaultOrder)
        model.setHomeOrder([.bottle, .breast, .sleep, .diaper, .solids, .meds, .growth], for: baby)
        #expect(model.homeOrder(for: baby).first == .bottle)
        // Another model on the same defaults — the widget's view of it.
        #expect(TrackingPreferences(defaults: d).homeOrder(for: baby).first == .bottle)
        model.resetHomeOrder(for: baby)
        #expect(model.homeOrder(for: baby) == HomeActionKind.defaultOrder)
    }

    @Test func movingAnActionByOneStepSwapsItWithItsNeighbour() {
        let model = SettingsViewModel(defaults: isolatedDefaults())
        let baby = UUID()
        model.moveHomeAction(at: 1, by: -1, for: baby)
        #expect(model.homeOrder(for: baby).prefix(2) == [.bottle, .breast])
        // Off either end: nothing moves.
        model.moveHomeAction(at: 0, by: -1, for: baby)
        model.moveHomeAction(at: 6, by: 1, for: baby)
        #expect(model.homeOrder(for: baby).prefix(2) == [.bottle, .breast])
    }

    @Test func theListsHeadingsSitAtFixedPositions() {
        #expect(HomeButtonsCopy.heading(at: 0) == HomeButtonsCopy.bigButton)
        #expect(HomeButtonsCopy.heading(at: 1) == HomeButtonsCopy.tiles)
        #expect(HomeButtonsCopy.heading(at: 2) == nil)
        #expect(HomeButtonsCopy.heading(at: 4) == HomeButtonsCopy.inMore)
        #expect(HomeButtonsCopy.heading(at: 5) == nil)
    }
```

If the file has no `isolatedDefaults()` helper, add this one at the top of the suite:

```swift
    private func isolatedDefaults() -> UserDefaults {
        let name = "settings-\(UUID().uuidString)"
        let d = UserDefaults(suiteName: name)!
        d.removePersistentDomain(forName: name)
        return d
    }
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd Packages/BabyTracker && swift test --filter SettingsViewModelTests`
Expected: FAIL to compile, `has no member 'homeOrder'`.

- [ ] **Step 3: Implement the view model**

In `SettingsViewModel`, beside the other stored observation seams:

```swift
    /// The observation seam for the Home orders, as `storedVolumeUnit` is for
    /// the unit: the orders live in `TrackingPreferences`, which a view
    /// cannot watch. Reading an order reads this; writing one bumps it.
    private var homeOrderRevision = 0
```

and after the break-limit accessors:

```swift
    // MARK: - Home buttons

    public func homeOrder(for babyID: UUID) -> [HomeActionKind] {
        _ = homeOrderRevision
        return tracking.homeOrder(for: babyID)
    }

    public func setHomeOrder(_ order: [HomeActionKind], for babyID: UUID) {
        tracking.setHomeOrder(order, for: babyID)
        homeOrderRevision += 1
    }

    /// One step up (`-1`) or down (`1`): VoiceOver's way to reorder.
    public func moveHomeAction(at index: Int, by offset: Int, for babyID: UUID) {
        var order = tracking.homeOrder(for: babyID)
        let target = index + offset
        guard order.indices.contains(index), order.indices.contains(target) else { return }
        order.swapAt(index, target)
        setHomeOrder(order, for: babyID)
    }

    /// Also what a deleted baby's order becomes: nothing.
    public func resetHomeOrder(for babyID: UUID) {
        tracking.resetHomeOrder(for: babyID)
        homeOrderRevision += 1
    }
```

- [ ] **Step 4: Implement the screen**

`HomeButtonsView.swift`:

```swift
import SwiftUI
import BabyCore
import BabyUI

enum HomeButtonsCopy {
    static let rowTitle = String(localized: "Home buttons", bundle: .module)
    static let bigButton = String(localized: "Big button", bundle: .module)
    static let tiles = String(localized: "Tiles", bundle: .module)
    static let inMore = String(localized: "In More (···)", bundle: .module)
    static let breastFooter = String(localized: "Shown as Left and Right.", bundle: .module)
    static let reset = String(localized: "Reset to default", bundle: .module)
    static let moveUp = String(localized: "Move up", bundle: .module)
    static let moveDown = String(localized: "Move down", bundle: .module)
    static let done = String(localized: "Done", bundle: .module)

    /// "Home buttons · Mira": the order belongs to one baby.
    static func title(_ babyName: String) -> String { "\(rowTitle) · \(babyName)" }

    /// The heading drawn above a row. They belong to positions, not to
    /// actions, so they stay put while rows move under them.
    static func heading(at index: Int) -> String? {
        switch index {
        case 0: bigButton
        case 1: tiles
        case HomeLayout.tileCount + 1: inMore
        default: nil
        }
    }
}

/// One list, dragged into order: the first action is Home's big button, the
/// next three its tiles, the rest live behind ···. Every change is saved as
/// it is made.
public struct HomeButtonsView: View {
    private let settings: SettingsViewModel
    private let babyID: UUID
    private let babyName: String

    public init(settings: SettingsViewModel, babyID: UUID, babyName: String) {
        self.settings = settings
        self.babyID = babyID
        self.babyName = babyName
    }

    private var order: [HomeActionKind] { settings.homeOrder(for: babyID) }

    public var body: some View {
        List {
            Section {
                ForEach(Array(order.enumerated()), id: \.element) { index, kind in
                    row(kind, at: index)
                }
                .onMove { from, to in
                    var next = order
                    next.move(fromOffsets: from, toOffset: to)
                    settings.setHomeOrder(next, for: babyID)
                }
            } footer: {
                if order.first == .breast { Text(HomeButtonsCopy.breastFooter) }
            }
            Section {
                Button(HomeButtonsCopy.reset) { settings.resetHomeOrder(for: babyID) }
                    .disabled(order == HomeActionKind.defaultOrder)
                    .accessibilityIdentifier("homeButtons.reset")
            }
        }
        // Always reorderable: there is nothing else to do on this screen.
        #if os(iOS)
        .environment(\.editMode, .constant(.active))
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .navigationTitle(HomeButtonsCopy.title(babyName))
    }

    private func row(_ kind: HomeActionKind, at index: Int) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            if let heading = HomeButtonsCopy.heading(at: index) {
                Text(heading)
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(Palette.secondaryText)
            }
            HStack(spacing: 10) {
                BabyIconView(kind.icon, size: 22)
                    .foregroundStyle(kind.color)
                Text(kind.title)
            }
            .frame(minHeight: 44)
        }
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("homeButtons.row.\(kind.rawValue)")
        .accessibilityAction(named: HomeButtonsCopy.moveUp) {
            settings.moveHomeAction(at: index, by: -1, for: babyID)
        }
        .accessibilityAction(named: HomeButtonsCopy.moveDown) {
            settings.moveHomeAction(at: index, by: 1, for: babyID)
        }
    }
}

public extension HomeButtonsView {
    /// The title of the Done button the app adds when it presents this
    /// screen as a sheet, from the ··· sheet's last row.
    static var doneTitle: String { HomeButtonsCopy.done }
}
```

In `SettingsView`, at the end of the Tracking `Section`, after the sleep-break `Stepper`:

```swift
                    if let baby = selection.resolve(from: babies) {
                        NavigationLink {
                            HomeButtonsView(settings: settings, babyID: baby.id, babyName: baby.name)
                        } label: {
                            Text(HomeButtonsCopy.rowTitle)
                                .frame(minHeight: 44)
                        }
                        .accessibilityIdentifier("settings.homeButtons")
                    }
```

Strings. Check for existing keys first:

```bash
grep -c '"Done"\|"Move up"\|"Move down"' Packages/BabyTracker/Sources/SettingsFeature/Localizable.xcstrings
```

Write `/tmp/home-buttons-settings.json`, leaving out any key the catalog already has:

```json
{
  "Home buttons": {"de": "Home-Tasten", "fr": "Boutons d’accueil", "es": "Botones de inicio", "it": "Pulsanti Home", "uk": "Кнопки на головній"},
  "Big button": {"de": "Große Taste", "fr": "Grand bouton", "es": "Botón grande", "it": "Pulsante grande", "uk": "Велика кнопка"},
  "Tiles": {"de": "Kleine Tasten", "fr": "Petits boutons", "es": "Botones pequeños", "it": "Pulsanti piccoli", "uk": "Малі кнопки"},
  "In More (···)": {"de": "Unter Mehr (···)", "fr": "Dans Plus (···)", "es": "En Más (···)", "it": "In Altro (···)", "uk": "У «Ще» (···)"},
  "Shown as Left and Right.": {"de": "Wird als Links und Rechts angezeigt.", "fr": "Affiché comme Gauche et Droite.", "es": "Se muestra como Izquierdo y Derecho.", "it": "Mostrato come Sinistra e Destra.", "uk": "Показується як Ліва і Права."},
  "Reset to default": {"de": "Auf Standard zurücksetzen", "fr": "Rétablir par défaut", "es": "Restablecer valores predeterminados", "it": "Ripristina predefiniti", "uk": "Скинути до типових"},
  "Move up": {"de": "Nach oben", "fr": "Monter", "es": "Subir", "it": "Sposta su", "uk": "Вгору"},
  "Move down": {"de": "Nach unten", "fr": "Descendre", "es": "Bajar", "it": "Sposta giù", "uk": "Вниз"},
  "Done": {"de": "Fertig", "fr": "Terminé", "es": "Listo", "it": "Fine", "uk": "Готово"}
}
```

For "In More (···)", match what the app already calls the ··· control in each language: read `More` from HomeFeature's catalog with the Task 5 one-liner and use that word inside the phrase.

Run: `python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/SettingsFeature/Localizable.xcstrings /tmp/home-buttons-settings.json`

- [ ] **Step 5: Run to verify it passes**

Run: `cd Packages/BabyTracker && swift test --filter SettingsFeatureTests`
Expected: PASS.

- [ ] **Step 6: Commit**

Bump `EXPECTED_TESTS` by 3. Run `make test`; expected `OK`.

```bash
/usr/bin/git add -A Makefile Packages/BabyTracker
/usr/bin/git commit -m "feat(settings): Home buttons, one list dragged into order (+3 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Wire the app, and the widget's app link

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyCore/LogLink.swift`
- Test: `Packages/BabyTracker/Tests/BabyCoreTests/LogLinkTests.swift`
- Modify: `Apps/BabyTrackerApp/RootView.swift`
- Modify: `UITests/BabyTrackerUITests/UITestHarness.swift` (`Launch`)

**Interfaces:**
- Consumes: `SettingsViewModel.homeOrder(for:)`, `resetHomeOrder(for:)`, `setHomeOrder(_:for:)` (Task 8); `HomeView(homeOrder:)` (Task 6); `LogSheetView(homeOrder:onEditHomeButtons:)`, `LogSheet.meds` (Task 7); `HomeButtonsView` (Task 8).
- Produces: `struct LogLink: Equatable, Sendable` with `init(kind: HomeActionKind, babyID: UUID)`, `init?(url: URL)`, `var url: URL`, `static func isLogLink(_ url: URL) -> Bool`. `UITestHarness.Launch.homeOrder: String?`.

- [ ] **Step 1: Write the failing tests**

```swift
import Testing
import Foundation
@testable import BabyCore

@Suite struct LogLinkTests {
    private let baby = UUID(uuidString: "0F1E2D3C-4B5A-6978-8796-A5B4C3D2E1F0")!

    @Test func aLinkRoundTripsThroughItsURL() throws {
        for kind in HomeActionKind.allCases {
            let link = LogLink(kind: kind, babyID: baby)
            #expect(LogLink.isLogLink(link.url))
            #expect(LogLink(url: link.url) == link)
        }
    }

    @Test func aLinkTheAppCannotReadIsStillALogLink() throws {
        // An action a later widget knows, a broken baby, nothing at all:
        // recognised as a log link, so the app opens Home rather than
        // treating it as an invitation, and parsed as nothing.
        for text in ["babytracker://log?kind=pump&baby=\(baby.uuidString)",
                     "babytracker://log?kind=solids&baby=not-a-uuid",
                     "babytracker://log"] {
            let url = try #require(URL(string: text))
            #expect(LogLink.isLogLink(url))
            #expect(LogLink(url: url) == nil)
        }
    }

    @Test func anInvitationIsNotALogLink() throws {
        let url = try #require(URL(string: "babytracker://invite?server=https%3A%2F%2Fexample.org&token=abc"))
        #expect(!LogLink.isLogLink(url))
        #expect(LogLink(url: url) == nil)
    }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd Packages/BabyTracker && swift test --filter LogLinkTests`
Expected: FAIL to compile, `cannot find 'LogLink' in scope`.

- [ ] **Step 3: Implement `LogLink`**

```swift
import Foundation

/// `babytracker://log?kind=solids&baby=<id>`: a Quick Log widget tile whose
/// action needs the app. The widget builds it; the app opens that baby's
/// panel.
public struct LogLink: Equatable, Sendable {
    public let kind: HomeActionKind
    public let babyID: UUID

    public init(kind: HomeActionKind, babyID: UUID) {
        self.kind = kind
        self.babyID = babyID
    }

    /// Nil for anything this version cannot act on, including a kind a later
    /// version's widget names.
    public init?(url: URL) {
        guard Self.isLogLink(url),
              let items = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems,
              let kind = items.first(where: { $0.name == "kind" })?.value.flatMap(HomeActionKind.init(rawValue:)),
              let babyID = items.first(where: { $0.name == "baby" })?.value.flatMap(UUID.init(uuidString:))
        else { return nil }
        self.init(kind: kind, babyID: babyID)
    }

    public var url: URL {
        var components = URLComponents()
        components.scheme = "babytracker"
        components.host = "log"
        components.queryItems = [URLQueryItem(name: "kind", value: kind.rawValue),
                                 URLQueryItem(name: "baby", value: babyID.uuidString)]
        // Built from fixed parts and a UUID: it always forms a URL.
        return components.url!
    }

    /// Whether `url` is addressed to this route at all, readable or not.
    public static func isLogLink(_ url: URL) -> Bool {
        url.scheme == "babytracker" && url.host == "log"
    }
}
```

Run: `cd Packages/BabyTracker && swift test --filter LogLinkTests`
Expected: PASS, 3 tests.

- [ ] **Step 4: Hand the order to Home and the ··· sheet**

In `RootView.swift`:

`HomeView(…)` in `tabs(for:store:)` gains, after `volumeUnit:`:

```swift
                    homeOrder: environment.settings.homeOrder(for: baby.id),
```

Add state beside `isSwitchingBaby`:

```swift
    /// The Home buttons screen, opened from the ··· sheet's last row. Queued
    /// while that sheet closes: two sheets cannot change places in one step.
    @State private var isEditingHomeButtons = false
    @State private var editsHomeButtonsAfterDismiss = false
    /// A widget's link that arrived before the store was open.
    @State private var parkedLogLink: LogLink?
```

The fitted log sheet's `LogSheetView(…)` (the first of the two) gains `homeOrder:` after `unit:` and `onEditHomeButtons:` last:

```swift
                    homeOrder: environment.settings.homeOrder(for: baby.id),
```

```swift
                    onEditHomeButtons: {
                        editsHomeButtonsAfterDismiss = true
                        sheet = nil
                    }
```

and that `.fittedSheet`'s `onDismiss: presentQueuedInvitation` becomes `onDismiss: logSheetDismissed`, with:

```swift
    /// The log sheet has closed. The Home buttons screen it asked for comes
    /// up now; otherwise a queued invitation gets its turn, as before.
    private func logSheetDismissed() {
        if editsHomeButtonsAfterDismiss {
            editsHomeButtonsAfterDismiss = false
            isEditingHomeButtons = true
        } else {
            presentQueuedInvitation()
        }
    }
```

After the `.sheet(isPresented: $isSwitchingBaby, …)` modifier add:

```swift
        .sheet(isPresented: $isEditingHomeButtons, onDismiss: presentQueuedInvitation) {
            NavigationStack {
                HomeButtonsView(settings: environment.settings, babyID: baby.id, babyName: baby.name)
                    .toolbar {
                        ToolbarItem(placement: .confirmationAction) {
                            Button(HomeButtonsView.doneTitle) { isEditingHomeButtons = false }
                                .accessibilityIdentifier("homeButtons.done")
                        }
                    }
            }
        }
        // The widget draws the same order.
        .onChange(of: environment.settings.homeOrder(for: baby.id)) { _, _ in
            WidgetCenter.shared.reloadAllTimelines()
        }
```

In the `SettingsView(…)` call, extend `onBabyDeleted`:

```swift
                    onBabyDeleted: { id in
                        environment.settings.resetHomeOrder(for: id)
                        do { try environment.askConversations.removeBaby(id) }
                        catch { errorReporter.report("Could not remove Ask chat", error: error, category: AppLog.store) }
                    }
```

- [ ] **Step 5: Route the widget's link**

At the top of the `.onOpenURL` closure, before `guard let familyModel`:

```swift
            // A Quick Log widget tile whose action needs the app. Checked
            // before the invitation routing, which needs a family model that
            // a phone outside a family does not have.
            if LogLink.isLogLink(url) {
                openLogLink(LogLink(url: url))
                return
            }
```

Update the comment above `.onOpenURL` to start: "`babytracker://log?…` from the Quick Log widget, or `babytracker://invite?…`".

Add beside `presentPendingRequest`:

```swift
    /// A widget tile opened the app: Home, then that baby's panel. A link
    /// this version cannot read, a baby deleted since the widget drew, a
    /// caregiver who may not write, or another sheet already up: Home alone.
    private func openLogLink(_ link: LogLink?) {
        selectedTab = .home
        guard let link else { return }
        guard let store = environment.store else {
            parkedLogLink = link
            return
        }
        guard (try? store.baby(link.babyID)) != nil, store.canWriteInActiveRealm,
              timerEditor == nil, !isSwitchingBaby, !isEditingHomeButtons else { return }
        environment.selection.select(link.babyID)
        reload()
        switch link.kind {
        case .bottle: sheet = .bottle
        case .diaper: sheet = .diaper
        case .solids: sheet = .solids
        case .meds: sheet = .meds
        case .growth: sheet = .measurement
        // Both log from the widget itself; a link for them only opens Home.
        case .breast, .sleep: break
        }
    }
```

- [ ] **Step 6: Open a parked link once the store is up**

In the `.task` closure, directly after `consumePendingQuickLog()`:

```swift
            // A widget link can arrive before the store is open on a cold
            // launch; `retry()` above has opened it by now.
            if let link = parkedLogLink {
                parkedLogLink = nil
                openLogLink(link)
            }
```

- [ ] **Step 7: A launch argument that sets the order, for UI tests**

In `RootView.openRequestedLogSheetIfNeeded()` (inside `#if DEBUG`), after the `guard`:

```swift
        // `-uiTestHomeOrder bottle,breast`: the baby's Home order, set
        // before the first frame. A `-key value` pair, read through the
        // argument domain.
        if let order = UserDefaults.standard.string(forKey: "uiTestHomeOrder") {
            environment.settings.setHomeOrder(
                HomeActionKind.order(fromStored: order.split(separator: ",").map(String.init)), for: baby.id)
        } else if arguments.contains("-uiTestResetStore") {
            environment.settings.resetHomeOrder(for: baby.id)
        }
```

Move the `let arguments = ProcessInfo.processInfo.arguments` line above this block.

In `UITestHarness.swift`, add to `Launch` beside `runningBottle`:

```swift
        /// The baby's Home order, as comma-separated action names.
        var homeOrder: String?
```

and in `arguments`, directly after the `language` line (pairs go before the bare flags — see the comment there):

```swift
            if let homeOrder { result += ["-uiTestHomeOrder", homeOrder] }
```

- [ ] **Step 8: Verify**

Run (after `pgrep xcodebuild` prints nothing): `xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build`
Expected: `BUILD SUCCEEDED`.

- [ ] **Step 9: Commit**

Bump `EXPECTED_TESTS` by 3. Run `make test`; expected `OK`.

```bash
/usr/bin/git add -A Makefile Packages/BabyTracker Apps/BabyTrackerApp/RootView.swift UITests/BabyTrackerUITests/UITestHarness.swift
/usr/bin/git commit -m "feat(home): the app hands the order to Home and More, and opens widget links (+3 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: The Quick Log widget follows the order

**Files:**
- Modify: `Apps/BabyTrackerWidgets/QuickLogWidget.swift`
- Modify: the String Catalog the widget target reads (`Apps/SharedIntents/Localizable.xcstrings`; confirm with `grep -l "Log a feed, sleep, or diaper without opening the app." Apps -r`)

**Interfaces:**
- Consumes: `HomeLayout.widgetSlots(_:)` (Task 1); `TrackingPreferences.homeOrder(for:)` (Task 2); `icon`, `color`, `title` (Task 5); `LogLink` (Task 9); the existing `StartBreastIntent`, `ToggleSleepIntent`, `LogDiaperIntent`, `StartBottleIntent`.

- [ ] **Step 1: Check the intents' initialisers**

Run: `grep -n "init(babyID" Apps/SharedIntents/LogIntents.swift`
Expected: `LogDiaperIntent.init(babyID: String)`, `StartBottleIntent.init(babyID: UUID)`, and the initialisers of `StartBreastIntent` and `ToggleSleepIntent`. The code below passes a `String` to the two the widget already calls that way and a `UUID` to `StartBottleIntent`; adjust an argument if an initialiser differs.

- [ ] **Step 2: Implement**

Replace the widget's doc comment, its `HStack` and the description:

```swift
/// The first four slots of the baby's Home order — the same order the dock
/// follows — each a direct write through the same store calls as the app
/// where one tap is the whole entry.
///
/// L and R start that breast part (joining the feed in its break, as on
/// Home); Sleep toggles the nap; Diaper logs a wet diaper; Bottle starts a
/// timed bottle. None of those opens the app. Solids, Meds and Growth need
/// more than a tap, so they open the app at their panel.
struct QuickLogWidget: Widget {
    var body: some WidgetConfiguration {
        AppIntentConfiguration(kind: "QuickLogWidget", intent: BabyWidgetConfiguration.self, provider: StatusProvider()) { entry in
            VStack(spacing: 9) {
                Text(entry.message ?? entry.summary.babyName)
                    .font(.caption.weight(.bold))
                    .frame(maxWidth: .infinity, alignment: .leading)

                if let babyID = entry.babyID, let baby = UUID(uuidString: babyID) {
                    HStack(spacing: 8) {
                        ForEach(HomeLayout(order: TrackingPreferences().homeOrder(for: baby)).widgetSlots(), id: \.self) { slot in
                            QuickLogButton(slot: slot, babyID: babyID, baby: baby)
                        }
                    }
                    .buttonStyle(.plain)
                }
            }
            .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Quick log")
        .description("Log with the first actions from your Home buttons.")
        .supportedFamilies([.systemMedium])
    }
}

/// One slot. The four that log in a tap are buttons on an intent; the rest
/// are links into the app.
private struct QuickLogButton: View {
    let slot: HomeLayout.WidgetSlot
    let babyID: String
    let baby: UUID

    var body: some View {
        switch slot {
        case .breastLeft:
            Button(intent: StartBreastIntent(babyID: babyID, side: BreastSide.left.rawValue)) {
                QuickLogTile(icon: .breastLeft, title: String(localized: "Left"), tint: Palette.feed)
            }
            .accessibilityLabel(String(localized: "Start left breast"))
        case .breastRight:
            Button(intent: StartBreastIntent(babyID: babyID, side: BreastSide.right.rawValue)) {
                QuickLogTile(icon: .breastRight, title: String(localized: "Right"), tint: Palette.feed)
            }
            .accessibilityLabel(String(localized: "Start right breast"))
        case .action(.sleep):
            Button(intent: ToggleSleepIntent(babyID: babyID)) { tile(.sleep) }
        case .action(.diaper):
            Button(intent: LogDiaperIntent(babyID: babyID)) { tile(.diaper) }
        case .action(.bottle):
            Button(intent: StartBottleIntent(babyID: baby)) { tile(.bottle) }
        case .action(let kind):
            Link(destination: LogLink(kind: kind, babyID: baby).url) { tile(kind) }
        }
    }

    private func tile(_ kind: HomeActionKind) -> QuickLogTile {
        QuickLogTile(icon: kind.icon, title: kind.title, tint: kind.color)
    }
}
```

Add `import BabyStore` to the file's imports (for `TrackingPreferences`). `QuickLogTile` is unchanged.

Add the description to the widget's catalog. Write `/tmp/home-buttons-widget.json`:

```json
{
  "Log with the first actions from your Home buttons.": {"de": "Protokolliere mit den ersten Aktionen deiner Home-Tasten.", "fr": "Enregistrez avec les premières actions de vos boutons d’accueil.", "es": "Registra con las primeras acciones de tus botones de inicio.", "it": "Registra con le prime azioni dei tuoi pulsanti Home.", "uk": "Записуйте першими діями з кнопок головної."}
}
```

Run: `python3 scripts/add-catalog-strings.py <the catalog found above> /tmp/home-buttons-widget.json`

Remove the old key "Log a feed, sleep, or diaper without opening the app." from that catalog if nothing else uses it:

```bash
grep -rn "Log a feed, sleep, or diaper without opening the app." Apps Packages --include="*.swift"
```

Expected: no output; then delete the key's entry from the catalog JSON.

- [ ] **Step 3: Verify**

Run (after `pgrep xcodebuild` prints nothing): `xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build`
Expected: `BUILD SUCCEEDED`.

- [ ] **Step 4: Commit**

```bash
/usr/bin/git add -A Apps
/usr/bin/git commit -m "feat(widget): Quick Log follows the baby's Home order; Bottle starts without the app

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 11: UI tests, own end-to-end checks, tracker

**Files:**
- Modify: `UITests/BabyTrackerUITests/LoggingFlowTests.swift`
- Modify: `TASKS.md`

- [ ] **Step 1: Write the UI tests**

Add to `LoggingFlowTests`, following that file's existing launch and wait helpers:

```swift
    /// The order decides the dock: Bottle first is the big button and opens
    /// the bottle panel; Breast, now a tile, asks which side.
    func testABottleFirstOrderLeadsHomeWithBottle() {
        let app = UITestHarness.launch(.init(homeOrder: "bottle,breast,sleep,diaper"))
        XCTAssertTrue(app.homeMarker.waitForExistence(timeout: 15))
        XCTAssertFalse(app.buttons["home.start.left"].exists, "Left is still a big button")

        XCTAssertTrue(app.tap(app.buttons["home.breast"], until: app.buttons["Left breast"]),
                      "the Breast tile did not ask which side")
        app.buttons["Left breast"].tap()
        XCTAssertTrue(app.buttons["home.stop"].waitForExistence(timeout: 8), "Left did not start")
        app.buttons["home.stop"].tap()

        XCTAssertTrue(app.tap(app.buttons["home.bottle"], until: app.buttons["bottle.start"]),
                      "the big Bottle button did not open the bottle panel")
    }

    /// The amount screen offers contents, and what is chosen there is what
    /// History shows.
    func testContentsChosenOnTheAmountScreenAreSaved() {
        let app = UITestHarness.launch(.init(runningBottle: true))
        XCTAssertTrue(app.homeMarker.waitForExistence(timeout: 15))
        let contents = app.segmentedControls["bottle.contents"]
        XCTAssertTrue(app.tap(app.buttons["home.stop"], until: contents), "Stop did not ask for the amount")

        // Whichever contents the bottle was not started with.
        let other = contents.buttons.allElementsBoundByIndex.first { !$0.isSelected }
        guard let other else { return XCTFail("no other contents to choose") }
        let chosen = other.label
        other.tap()
        app.buttons["bottle.done"].tap()
        XCTAssertTrue(app.buttons["home.start.left"].waitForExistence(timeout: 8), "the bottle did not stop")

        app.goToTab("History")
        XCTAssertTrue(app.visibleStrings().contains { $0.localizedCaseInsensitiveContains(chosen) },
                      "History does not show \(chosen)")
    }
```

If `goToTab` in `UITestHarness.swift` takes more than the tab's name, call it the way the file's other tests do.

- [ ] **Step 2: Run them**

Run (after `pgrep xcodebuild` prints nothing):

```bash
xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' \
  -parallel-testing-enabled NO -maximum-concurrent-test-simulator-destinations 1 \
  -only-testing:BabyTrackerUITests/LoggingFlowTests/testABottleFirstOrderLeadsHomeWithBottle \
  -only-testing:BabyTrackerUITests/LoggingFlowTests/testContentsChosenOnTheAmountScreenAreSaved
```

Expected: `** TEST SUCCEEDED **`, 2 tests.

Then the existing tests whose screens changed, one run:

```bash
grep -ln "home.bottle\|home.more\|more.solids\|bottle.done\|home.feed" UITests/BabyTrackerUITests/*.swift
```

Run every class that prints, each with `-only-testing:BabyTrackerUITests/<Class>`, in a single `xcodebuild test` invocation. Expected: `** TEST SUCCEEDED **`. A failure here is a regression in the default order; fix it before going on.

- [ ] **Step 3: Simulator screenshots**

Build and install on the shared simulator, then for each state launch with the arguments shown and save a screenshot with `xcrun simctl io <simulator-udid> screenshot <path>` under `docs/verification/2026-10-11-home-buttons/`:

| File | Launch arguments |
| --- | --- |
| `default-idle.png` | `-uiTestResetStore -uiTestSeedBaby -uiTestSeedFixture` |
| `bottle-first-idle.png` | `-uiTestHomeOrder bottle,breast,sleep,diaper -uiTestResetStore -uiTestSeedBaby -uiTestSeedFixture` |
| `bottle-first-sleep.png` | same, plus `-uiTestRunningSleep` |
| `bottle-first-feed.png` | same as idle, plus `-uiTestRunningFeed` |
| `solids-first-idle.png` | `-uiTestHomeOrder solids,meds,growth,diaper -uiTestResetStore -uiTestSeedBaby -uiTestSeedFixture` |
| `solids-first-more.png` | same, plus `-uiTestOpenMore` |
| `amount-contents.png` | `-uiTestResetStore -uiTestSeedBaby -uiTestRunningBottle -uiTestOpenBottleStopped` |
| `bottle-first-idle-uk-ax.png` | `-UIPreferredContentSizeCategoryName UICTContentSizeCategoryAccessibilityXXXL -AppleLanguages "(uk)" -uiTestHomeOrder bottle,breast,sleep,diaper -uiTestResetStore -uiTestSeedBaby` |

Launch with `xcrun simctl launch <simulator-udid> <bundle id> <arguments>`; the bundle id is `PRODUCT_BUNDLE_IDENTIFIER` in the app target's build settings (`xcodebuild -scheme BabyTracker -showBuildSettings | grep PRODUCT_BUNDLE_IDENTIFIER`).

Open the settings screen by hand (Settings → Tracking → Home buttons) and save `settings-list.png`.

Read every screenshot. Check: the default states match `mockups/2026-10-01-home-redesign/` as today's build does; the big non-breast button shows its icon and name untruncated; no tile row holds more than four; the Ukrainian accessibility-size dock scrolls with the content and nothing is clipped.

- [ ] **Step 4: The widget**

On the simulator's Home Screen add the Quick Log widget (medium) for the seeded baby. Check, and record each result in `docs/verification/2026-10-11-home-buttons/README.md`:

1. Default order: the widget shows L · R · Bottle · Sleep.
2. Tap Bottle on the widget: the app does not open; opening it by hand shows a running bottle on Home.
3. In the app set the order to Solids, Meds, Growth, Diaper: within a few seconds the widget shows those four.
4. Tap Solids on the widget: the app opens on Home with the solids panel up.
5. Force-quit the app, then tap Growth on the widget: the app cold-launches into the growth panel (the parked-link path).

If the widget cannot be added or tapped in the simulator, say so in the README rather than marking the items passed, and list them under "needs a phone".

- [ ] **Step 5: Full gates**

Run: `make test`
Expected: `OK` with the bumped total (baseline + 37).

Run: `xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build`
Expected: `BUILD SUCCEEDED`.

- [ ] **Step 6: Record in the tracker and commit**

Add a section at the top of `TASKS.md`, in the file's existing style:

```markdown
# Arrangeable Home buttons (2026-10-11)

Spec `docs/superpowers/specs/2026-10-11-home-buttons-order-design.md`, plan `docs/superpowers/plans/2026-10-11-home-buttons-order.md`, branch `home-buttons`.

- [x] Order per baby on this phone; dock, More sheet and Quick Log widget follow it.
- [x] Settings → Tracking → Home buttons, and "Edit Home buttons" in the More sheet.
- [x] Bottle: contents on the amount screen; the default amount is the previous bottle's.
- [x] Package suite, build, UI subset, simulator screenshots (`docs/verification/2026-10-11-home-buttons/`).
- [ ] Phone check: widget order, widget Bottle without opening the app, widget Solids opening its panel.
```

Tick or untick each line to match what Steps 2–5 actually showed.

```bash
/usr/bin/git add -A UITests TASKS.md docs/verification/2026-10-11-home-buttons
/usr/bin/git commit -m "test(home): UI subset, screenshots and widget checks for arrangeable Home buttons

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

Nothing is merged, pushed or installed on a phone without the user's say-so.
