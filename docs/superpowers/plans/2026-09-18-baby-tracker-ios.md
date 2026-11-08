# Baby Tracker (iOS) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A native iOS 26 app for logging a baby's feeding, sleep, and diaper changes, fast enough to use one-handed in the dark.

**Architecture:** A thin Xcode project (app target + widget extension) over a local Swift package. `BabyCore` holds pure value types and time math with no SwiftData or SwiftUI. `BabyStore` owns the only `ModelContext` in the system and funnels every mutation through a single `record` choke point that also appends a change-log entry. `BabySync` is a protocol with a no-op implementation, built now so the future self-hosted sync node drops in without touching feature code. Feature modules depend on `BabyStore` and on each other never.

**Tech Stack:** Swift 6.4, SwiftUI, SwiftData, Swift Testing, WidgetKit, ActivityKit, App Intents. No third-party dependencies.

**Spec:** `docs/superpowers/specs/2026-09-18-baby-tracker-ios-design.md`

## Global Constraints

- iOS 26 minimum. Package also declares macOS 26 so `BabyCore`, `BabyStore`, and `BabySync` run under `swift test` on the Mac without a simulator — this is the fast test loop and tasks 2–8 rely on it.
- Swift 6 language mode with strict concurrency enabled. No `@preconcurrency` escapes.
- **No third-party dependencies.** Not for testing, not for formatting, not for anything.
- Volumes stored in millilitres (`Double`), durations in seconds (`Int`), timestamps as UTC `Date` plus the entry's own `timeZoneID` string.
- Deletes are tombstones (`isTombstoned = true`). Rows are never removed. All queries filter `isTombstoned == false` by default.
- **The tombstone column MUST NOT be named `isDeleted`.** `isDeleted` is reserved by
  `NSManagedObject`, which backs SwiftData: a `@Model` property with that name is
  silently reset to `false` on every `save()`, so tombstones never persist. Verified
  by repro on this toolchain. The column is `isTombstoned` everywhere — model
  properties, `Touchable`, `#Predicate` filters, and change-log payload keys.
- Every mutation appends a `ChangeLogEntry` in the same transaction. No exceptions.
- Enums persist as raw `String` columns (`kindRaw`, `contentsRaw`, …) because `#Predicate` cannot traverse computed properties. Typed accessors are computed on top.
- No feature may import another feature module.
- No predictions, forecasts, percentile curves, or progress-toward-a-target UI anywhere.
- Dynamic Type throughout, no fixed font sizes, usable at AX5. All tap targets ≥ 44pt.
- Custom `.glassEffect` appears in exactly one place per screen (the dock, or the stop control that replaces it), inside one `GlassEffectContainer`. Glass never overlaps glass.
- Commit after every task. Conventional commit prefixes (`feat:`, `test:`, `chore:`).
- **Test fixtures must not straddle local midnight.** Several tasks use a fixed epoch
  and then `advance(by:)` across hours. A bare epoch like `1_700_000_000` is 23:13 UTC,
  which is already the next local day in CEST, so "today" assertions fail depending on
  the runner's timezone. Any test that logs entries and asserts on same-day totals must
  build its base instant as **noon local time** via `DateComponents` against
  `Calendar.current`, not as a raw `timeIntervalSince1970`. This applies to Tasks 10,
  16 and 17.

## File Structure

```
BabyTracker.xcodeproj
Apps/
  BabyTrackerApp/
    BabyTrackerApp.swift          @main, injects BabyStore, three-tab TabView
    AppGroup.swift                shared container identifier, single source
  BabyTrackerWidgets/
    BabyTrackerWidgets.swift      widget bundle entry
    StatusWidget.swift            small: time since last feed/sleep/diaper
    QuickLogWidget.swift          medium: App Intent buttons
    FeedActivityWidget.swift      Live Activity UI
    LogIntents.swift              App Intents calling BabyStore

Packages/BabyTracker/Sources/
  BabyCore/
    DateProvider.swift            DateProvider protocol, SystemDateProvider, TestDateProvider
    Volume.swift                  Volume value type, VolumeUnit
    ElapsedFormatter.swift        compact ("2h 14m") and timer ("18:42") formatting
    DayBucket.swift               timezone-anchored day keys
    SleepClassifier.swift         nap/night derivation
    Enums.swift                   FeedKind, BreastSide, SideInterval, DiaperContents, …
  BabyStore/
    Models/                       Baby, FeedEntry, SleepEntry, DiaperEntry, ActiveTimer, ChangeLogEntry
    BabyStore.swift               container ownership + record() choke point
    BabyStore+Feeding.swift       feed mutations and timer transitions
    BabyStore+Sleep.swift         sleep mutations
    BabyStore+Diaper.swift        diaper mutations
    BabyStore+Queries.swift       last-of-kind, today totals, history pages
  BabySync/
    SyncClient.swift              protocol
    LocalOnlySync.swift           no-op implementation
  BabyUI/
    Palette.swift                 Night Shift colors as semantic tokens
    Typography.swift              rounded numeral styles, tabular figures
    GlassDock.swift               the one custom-glass container
  Features/
    HomeFeature/                  HomeView, IdleStateView, RunningTimerView, TimerDial
    LoggingFeature/               bottle sheet, diaper sheet, feed start, edit sheet
    HistoryFeature/               grouped list, day headers, filter chips
    InsightsFeature/              four-figure comparison
    BabyFeature/                  baby creation, switcher, first-run empty state

Packages/BabyTracker/Tests/
  BabyCoreTests/  BabyStoreTests/  BabySyncTests/
UITests/BabyTrackerUITests/       the four two-tap flows
```

---

### Task 1: Project scaffold

**Files:**
- Create: `Packages/BabyTracker/Package.swift`
- Create: `Packages/BabyTracker/Sources/BabyCore/DateProvider.swift`
- Test: `Packages/BabyTracker/Tests/BabyCoreTests/DateProviderTests.swift`

**Interfaces:**
- Consumes: nothing
- Produces: `DateProvider` protocol with `var now: Date { get }`; `SystemDateProvider()`; `TestDateProvider(_ start: Date)` with `advance(by: TimeInterval)`

The Xcode project and widget extension are created in Task 16. Everything through Task 15 is package code testable with `swift test`, which is the whole point of putting it in a package.

- [ ] **Step 1: Create the package manifest**

`Packages/BabyTracker/Package.swift`:

```swift
// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "BabyTracker",
    platforms: [.iOS(.v26), .macOS(.v26)],
    products: [
        .library(name: "BabyCore", targets: ["BabyCore"]),
        .library(name: "BabyStore", targets: ["BabyStore"]),
        .library(name: "BabySync", targets: ["BabySync"]),
        .library(name: "BabyUI", targets: ["BabyUI"]),
        .library(name: "HomeFeature", targets: ["HomeFeature"]),
        .library(name: "LoggingFeature", targets: ["LoggingFeature"]),
        .library(name: "HistoryFeature", targets: ["HistoryFeature"]),
        .library(name: "InsightsFeature", targets: ["InsightsFeature"]),
        .library(name: "BabyFeature", targets: ["BabyFeature"]),
    ],
    targets: [
        .target(name: "BabyCore"),
        .target(name: "BabyStore", dependencies: ["BabyCore"]),
        .target(name: "BabySync", dependencies: ["BabyCore"]),
        .target(name: "BabyUI", dependencies: ["BabyCore"]),
        .target(name: "HomeFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI"]),
        .target(name: "LoggingFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI"]),
        .target(name: "HistoryFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI"]),
        .target(name: "InsightsFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI"]),
        .target(name: "BabyFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI"]),
        .testTarget(name: "BabyCoreTests", dependencies: ["BabyCore"]),
        .testTarget(name: "BabyStoreTests", dependencies: ["BabyStore"]),
        .testTarget(name: "BabySyncTests", dependencies: ["BabySync"]),
    ],
    swiftLanguageModes: [.v6]
)
```

- [ ] **Step 2: Write the failing test**

`Tests/BabyCoreTests/DateProviderTests.swift`:

```swift
import Testing
import Foundation
@testable import BabyCore

@Test func testClockAdvancesOnDemandOnly() {
    let start = Date(timeIntervalSince1970: 1_000_000)
    let clock = TestDateProvider(start)

    #expect(clock.now == start)

    clock.advance(by: 90)
    #expect(clock.now == start.addingTimeInterval(90))
}

@Test func systemClockMovesForward() {
    let clock = SystemDateProvider()
    let first = clock.now
    #expect(clock.now >= first)
}
```

- [ ] **Step 3: Run the test and confirm it fails**

Run: `cd Packages/BabyTracker && swift test --filter DateProviderTests`
Expected: FAIL — `cannot find 'TestDateProvider' in scope`

- [ ] **Step 4: Implement**

`Sources/BabyCore/DateProvider.swift`:

```swift
import Foundation

/// Every read of "now" in the app goes through this, so timer behaviour is
/// testable without waiting. Named DateProvider rather than Clock to avoid
/// colliding with the standard library's Clock protocol.
public protocol DateProvider: Sendable {
    var now: Date { get }
}

public struct SystemDateProvider: DateProvider {
    public init() {}
    public var now: Date { Date() }
}

/// Test double. Time moves only when the test says so.
public final class TestDateProvider: DateProvider, @unchecked Sendable {
    private let lock = NSLock()
    private var current: Date

    public init(_ start: Date) {
        self.current = start
    }

    public var now: Date {
        lock.withLock { current }
    }

    public func advance(by interval: TimeInterval) {
        lock.withLock { current += interval }
    }
}
```

- [ ] **Step 5: Run the test and confirm it passes**

Run: `cd Packages/BabyTracker && swift test --filter DateProviderTests`
Expected: PASS, 2 tests

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add package scaffold and injectable date provider"
```

---

### Task 2: Core enums and the side-interval type

**Files:**
- Create: `Sources/BabyCore/Enums.swift`
- Test: `Tests/BabyCoreTests/EnumsTests.swift`

**Interfaces:**
- Consumes: nothing
- Produces: `FeedKind` (`.breast`, `.bottle`, `.solids`), `BreastSide` (`.left`, `.right`), `SideInterval(side:seconds:)`, `BottleContents` (`.breastMilk`, `.formula`, `.mixed`), `SolidsAmount` (`.taste`, `.some`, `.lots`), `DiaperContents` (`.wet`, `.dirty`, `.mixed`, `.dry`), `SleepKind` (`.nap`, `.night`), `TimerKind` (`.feed`, `.sleep`). All are `String`-raw-valued, `Codable`, `Sendable`, `CaseIterable`.

Raw values are persisted, so they are frozen contract. Changing one is a data migration.

- [ ] **Step 1: Write the failing test**

`Tests/BabyCoreTests/EnumsTests.swift`:

```swift
import Testing
import Foundation
@testable import BabyCore

@Test func rawValuesAreStableContract() {
    // These strings are written to the store and will be written to the wire
    // by the future sync node. Changing one is a migration, not a rename.
    #expect(FeedKind.breast.rawValue == "breast")
    #expect(FeedKind.bottle.rawValue == "bottle")
    #expect(FeedKind.solids.rawValue == "solids")
    #expect(BreastSide.left.rawValue == "left")
    #expect(DiaperContents.mixed.rawValue == "mixed")
    #expect(TimerKind.feed.rawValue == "feed")
}

@Test func sideIntervalRoundTripsThroughJSON() throws {
    let intervals = [
        SideInterval(side: .left, seconds: 620),
        SideInterval(side: .right, seconds: 480),
    ]
    let data = try JSONEncoder().encode(intervals)
    let decoded = try JSONDecoder().decode([SideInterval].self, from: data)

    #expect(decoded == intervals)
    #expect(decoded.last?.side == .right)
}

@Test func unknownRawValueDecodesToNil() {
    #expect(FeedKind(rawValue: "telepathy") == nil)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter EnumsTests`
Expected: FAIL — `cannot find 'FeedKind' in scope`

- [ ] **Step 3: Implement**

`Sources/BabyCore/Enums.swift`:

```swift
import Foundation

public enum FeedKind: String, Codable, Sendable, CaseIterable {
    case breast, bottle, solids
}

public enum BreastSide: String, Codable, Sendable, CaseIterable {
    case left, right

    public var opposite: BreastSide {
        self == .left ? .right : .left
    }
}

/// One continuous stretch on one breast. A feed is a list of these, so
/// switching sides mid-feed appends rather than overwrites.
public struct SideInterval: Codable, Sendable, Equatable {
    public var side: BreastSide
    public var seconds: Int

    public init(side: BreastSide, seconds: Int) {
        self.side = side
        self.seconds = seconds
    }
}

public enum BottleContents: String, Codable, Sendable, CaseIterable {
    case breastMilk, formula, mixed
}

public enum SolidsAmount: String, Codable, Sendable, CaseIterable {
    case taste, some, lots
}

public enum DiaperContents: String, Codable, Sendable, CaseIterable {
    case wet, dirty, mixed, dry

    /// The three offered in the two-tap fast path. `.dry` is reachable only
    /// from the edit sheet.
    public static var fastPath: [DiaperContents] { [.wet, .dirty, .mixed] }
}

public enum SleepKind: String, Codable, Sendable, CaseIterable {
    case nap, night
}

public enum TimerKind: String, Codable, Sendable, CaseIterable {
    case feed, sleep
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `swift test --filter EnumsTests`
Expected: PASS, 3 tests

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add core domain enums and side intervals"
```

---

### Task 3: Volume and elapsed-time formatting

**Files:**
- Create: `Sources/BabyCore/Volume.swift`
- Create: `Sources/BabyCore/ElapsedFormatter.swift`
- Test: `Tests/BabyCoreTests/VolumeTests.swift`
- Test: `Tests/BabyCoreTests/ElapsedFormatterTests.swift`

**Interfaces:**
- Consumes: nothing
- Produces: `Volume(millilitres:)`, `Volume(ounces:)`, `.millilitres`, `.ounces`, `.formatted(as: VolumeUnit)`; `VolumeUnit` (`.millilitres`, `.ounces`); `ElapsedFormatter.compact(_ seconds: Int) -> String`, `ElapsedFormatter.timer(_ seconds: Int) -> String`

- [ ] **Step 1: Write the failing tests**

`Tests/BabyCoreTests/VolumeTests.swift`:

```swift
import Testing
@testable import BabyCore

@Test func ouncesRoundTripWithoutDrift() {
    let original = Volume(ounces: 4)
    let roundTripped = Volume(millilitres: original.millilitres)

    #expect(abs(roundTripped.ounces - 4) < 0.0001)
}

@Test func millilitresAreCanonical() {
    #expect(abs(Volume(ounces: 1).millilitres - 29.5735295625) < 0.0001)
}

@Test func formattingDropsPointlessDecimals() {
    #expect(Volume(millilitres: 120).formatted(as: .millilitres) == "120 ml")
    #expect(Volume(millilitres: 118.294).formatted(as: .ounces) == "4 oz")
    #expect(Volume(millilitres: 100).formatted(as: .ounces) == "3.4 oz")
}
```

`Tests/BabyCoreTests/ElapsedFormatterTests.swift`:

```swift
import Testing
@testable import BabyCore

@Test func compactFormatsRelativeAges() {
    #expect(ElapsedFormatter.compact(0) == "just now")
    #expect(ElapsedFormatter.compact(45) == "just now")
    #expect(ElapsedFormatter.compact(60) == "1m")
    #expect(ElapsedFormatter.compact(8_040) == "2h 14m")
    #expect(ElapsedFormatter.compact(7_200) == "2h")
}

@Test func compactClampsNegativeToJustNow() {
    // Clock skew or an edit dated in the future must not render "-3m".
    #expect(ElapsedFormatter.compact(-180) == "just now")
}

@Test func timerFormatsRunningCounters() {
    #expect(ElapsedFormatter.timer(0) == "0:00")
    #expect(ElapsedFormatter.timer(1_122) == "18:42")
    #expect(ElapsedFormatter.timer(3_725) == "1:02:05")
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `swift test --filter VolumeTests && swift test --filter ElapsedFormatterTests`
Expected: FAIL — `cannot find 'Volume' in scope`

- [ ] **Step 3: Implement Volume**

`Sources/BabyCore/Volume.swift`:

```swift
import Foundation

public enum VolumeUnit: String, Codable, Sendable, CaseIterable {
    case millilitres, ounces

    public var shortName: String {
        switch self {
        case .millilitres: "ml"
        case .ounces: "oz"
        }
    }
}

/// Millilitres are canonical everywhere. Ounces exist only at the display edge.
public struct Volume: Codable, Sendable, Equatable {
    private static let mlPerOunce = 29.5735295625

    public let millilitres: Double

    public init(millilitres: Double) {
        self.millilitres = millilitres
    }

    public init(ounces: Double) {
        self.millilitres = ounces * Self.mlPerOunce
    }

    public var ounces: Double {
        millilitres / Self.mlPerOunce
    }

    public func formatted(as unit: VolumeUnit) -> String {
        let value = unit == .millilitres ? millilitres : ounces
        let rounded = (value * 10).rounded() / 10
        let text = rounded == rounded.rounded()
            ? String(Int(rounded))
            : String(format: "%.1f", rounded)
        return "\(text) \(unit.shortName)"
    }
}
```

- [ ] **Step 4: Implement ElapsedFormatter**

`Sources/BabyCore/ElapsedFormatter.swift`:

```swift
import Foundation

public enum ElapsedFormatter {
    /// Relative age of a past event: "2h 14m", "55m", "just now".
    public static func compact(_ seconds: Int) -> String {
        let total = max(0, seconds)
        let hours = total / 3600
        let minutes = (total % 3600) / 60

        if hours > 0 {
            return minutes > 0 ? "\(hours)h \(minutes)m" : "\(hours)h"
        }
        if minutes > 0 {
            return "\(minutes)m"
        }
        return "just now"
    }

    /// A running counter: "0:00", "18:42", "1:02:05".
    public static func timer(_ seconds: Int) -> String {
        let total = max(0, seconds)
        let hours = total / 3600
        let minutes = (total % 3600) / 60
        let secs = total % 60

        if hours > 0 {
            return String(format: "%d:%02d:%02d", hours, minutes, secs)
        }
        return String(format: "%d:%02d", minutes, secs)
    }
}
```

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `swift test --filter VolumeTests && swift test --filter ElapsedFormatterTests`
Expected: PASS, 6 tests

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add volume conversion and elapsed-time formatting"
```

---

### Task 4: Timezone-anchored day bucketing

**Files:**
- Create: `Sources/BabyCore/DayBucket.swift`
- Test: `Tests/BabyCoreTests/DayBucketTests.swift`

**Interfaces:**
- Consumes: nothing
- Produces: `DayKey` (`Hashable`, `Comparable`, with `year`, `month`, `day`); `DayBucket.key(for: Date, timeZoneID: String) -> DayKey`; `DayBucket.bounds(of: DayKey, timeZoneID: String) -> (start: Date, end: Date)`

An entry logged at 2am in Berlin must still bucket to that Berlin day after the phone moves to New York. This is why the entry carries its own `timeZoneID` rather than reading the device's.

- [ ] **Step 1: Write the failing test**

`Tests/BabyCoreTests/DayBucketTests.swift`:

```swift
import Testing
import Foundation
@testable import BabyCore

@Test func dayIsAnchoredToTheEntrysOwnTimezone() {
    // 2026-09-18 02:00 Berlin == 2026-09-18 00:00 UTC == 2026-09-17 20:00 New York.
    var components = DateComponents()
    components.year = 2026; components.month = 9; components.day = 18
    components.hour = 0; components.minute = 0
    var utc = Calendar(identifier: .gregorian)
    utc.timeZone = TimeZone(identifier: "UTC")!
    let instant = utc.date(from: components)!

    let berlin = DayBucket.key(for: instant, timeZoneID: "Europe/Berlin")
    let newYork = DayBucket.key(for: instant, timeZoneID: "America/New_York")

    #expect(berlin == DayKey(year: 2026, month: 9, day: 18))
    #expect(newYork == DayKey(year: 2026, month: 9, day: 17))
}

@Test func springForwardDayIsStillOneDay() {
    // 2026-03-29 is the European spring-forward: that local day is 23 hours long.
    let key = DayKey(year: 2026, month: 3, day: 29)
    let bounds = DayBucket.bounds(of: key, timeZoneID: "Europe/Berlin")
    let length = bounds.end.timeIntervalSince(bounds.start)

    #expect(length == 23 * 3600)
}

@Test func autumnBackDayIsStillOneDay() {
    // 2026-10-25 is the European fall-back: that local day is 25 hours long.
    let key = DayKey(year: 2026, month: 10, day: 25)
    let bounds = DayBucket.bounds(of: key, timeZoneID: "Europe/Berlin")

    #expect(bounds.end.timeIntervalSince(bounds.start) == 25 * 3600)
}

@Test func keysSortChronologically() {
    let earlier = DayKey(year: 2026, month: 9, day: 17)
    let later = DayKey(year: 2026, month: 9, day: 18)
    let nextYear = DayKey(year: 2027, month: 1, day: 1)

    #expect(earlier < later)
    #expect(later < nextYear)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter DayBucketTests`
Expected: FAIL — `cannot find 'DayBucket' in scope`

- [ ] **Step 3: Implement**

`Sources/BabyCore/DayBucket.swift`:

```swift
import Foundation

/// A calendar day in some specific timezone. Not an instant — two DayKeys with
/// the same fields from different timezones describe different stretches of time.
public struct DayKey: Hashable, Sendable, Comparable, Codable {
    public let year: Int
    public let month: Int
    public let day: Int

    public init(year: Int, month: Int, day: Int) {
        self.year = year
        self.month = month
        self.day = day
    }

    public static func < (lhs: DayKey, rhs: DayKey) -> Bool {
        (lhs.year, lhs.month, lhs.day) < (rhs.year, rhs.month, rhs.day)
    }
}

public enum DayBucket {
    /// Falls back to UTC rather than the device timezone when an identifier is
    /// unknown, so a corrupt value produces a stable wrong answer rather than
    /// one that changes as the user travels.
    static func calendar(for timeZoneID: String) -> Calendar {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: timeZoneID) ?? TimeZone(identifier: "UTC")!
        return calendar
    }

    public static func key(for date: Date, timeZoneID: String) -> DayKey {
        let parts = calendar(for: timeZoneID).dateComponents([.year, .month, .day], from: date)
        return DayKey(year: parts.year!, month: parts.month!, day: parts.day!)
    }

    /// Start of the local day and start of the next local day. Uses calendar
    /// arithmetic rather than adding 86,400 seconds, so DST days come out 23 or
    /// 25 hours long as they actually are.
    public static func bounds(of key: DayKey, timeZoneID: String) -> (start: Date, end: Date) {
        let calendar = calendar(for: timeZoneID)
        var components = DateComponents()
        components.year = key.year
        components.month = key.month
        components.day = key.day

        let start = calendar.date(from: components)!
        let end = calendar.date(byAdding: .day, value: 1, to: start)!
        return (start, end)
    }
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `swift test --filter DayBucketTests`
Expected: PASS, 4 tests

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add timezone-anchored day bucketing with DST-correct bounds"
```

---

### Task 5: Nap/night classification

**Files:**
- Create: `Sources/BabyCore/SleepClassifier.swift`
- Test: `Tests/BabyCoreTests/SleepClassifierTests.swift`

**Interfaces:**
- Consumes: `SleepKind` (Task 2), `DayBucket.calendar(for:)` (Task 4)
- Produces: `SleepClassifier.classify(startedAt: Date, timeZoneID: String) -> SleepKind`

The user is never asked nap-or-night at log time. A sleep that *starts* at or after 19:00 local, or before 06:00 local, is night; anything else is a nap. The user can override in the edit sheet, which is why the model stores an optional override rather than relying on this function at read time.

- [ ] **Step 1: Write the failing test**

`Tests/BabyCoreTests/SleepClassifierTests.swift`:

```swift
import Testing
import Foundation
@testable import BabyCore

private func berlinInstant(hour: Int, minute: Int = 0) -> Date {
    var calendar = Calendar(identifier: .gregorian)
    calendar.timeZone = TimeZone(identifier: "Europe/Berlin")!
    var components = DateComponents()
    components.year = 2026; components.month = 9; components.day = 18
    components.hour = hour; components.minute = minute
    return calendar.date(from: components)!
}

@Test func lateEveningStartsAreNight() {
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 19), timeZoneID: "Europe/Berlin") == .night)
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 23), timeZoneID: "Europe/Berlin") == .night)
}

@Test func smallHoursAreNight() {
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 2), timeZoneID: "Europe/Berlin") == .night)
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 5, minute: 59), timeZoneID: "Europe/Berlin") == .night)
}

@Test func daytimeStartsAreNaps() {
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 6), timeZoneID: "Europe/Berlin") == .nap)
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 13), timeZoneID: "Europe/Berlin") == .nap)
    #expect(SleepClassifier.classify(startedAt: berlinInstant(hour: 18, minute: 59), timeZoneID: "Europe/Berlin") == .nap)
}

@Test func classificationFollowsTheEntrysTimezoneNotTheDevices() {
    // 19:00 Berlin is 13:00 New York: night there, nap here.
    let instant = berlinInstant(hour: 19)

    #expect(SleepClassifier.classify(startedAt: instant, timeZoneID: "Europe/Berlin") == .night)
    #expect(SleepClassifier.classify(startedAt: instant, timeZoneID: "America/New_York") == .nap)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter SleepClassifierTests`
Expected: FAIL — `cannot find 'SleepClassifier' in scope`

- [ ] **Step 3: Implement**

`Sources/BabyCore/SleepClassifier.swift`:

```swift
import Foundation

public enum SleepClassifier {
    /// Local hour at or after this is night.
    public static let nightStartHour = 19
    /// Local hour at or after this is daytime again.
    public static let dayStartHour = 6

    /// Derived from when the sleep *started*, never asked at log time. A user
    /// override is stored separately on the entry and wins over this.
    public static func classify(startedAt: Date, timeZoneID: String) -> SleepKind {
        let calendar = DayBucket.calendar(for: timeZoneID)
        let hour = calendar.component(.hour, from: startedAt)

        return (hour >= nightStartHour || hour < dayStartHour) ? .night : .nap
    }
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `swift test --filter SleepClassifierTests`
Expected: PASS, 4 tests

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: derive nap/night from local start hour"
```

---

### Task 6: SwiftData models

**Files:**
- Create: `Sources/BabyStore/Models/Baby.swift`
- Create: `Sources/BabyStore/Models/FeedEntry.swift`
- Create: `Sources/BabyStore/Models/SleepEntry.swift`
- Create: `Sources/BabyStore/Models/DiaperEntry.swift`
- Create: `Sources/BabyStore/Models/ActiveTimer.swift`
- Create: `Sources/BabyStore/Models/ChangeLogEntry.swift`
- Create: `Sources/BabyStore/Models/Schema.swift`
- Test: `Tests/BabyStoreTests/ModelTests.swift`

**Interfaces:**
- Consumes: all of `BabyCore` Task 2
- Produces: the six `@Model` classes above; `BabySchema.all: [any PersistentModel.Type]`; `BabySchema.container(inMemory: Bool) throws -> ModelContainer`. Each entry exposes a typed computed accessor over its raw column (`FeedEntry.kind`, `DiaperEntry.contents`, `SleepEntry.kindOverride`).

Enums persist as raw `String`. `#Predicate` cannot traverse computed properties, so every property a query filters on must be a stored column.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/ModelTests.swift`:

```swift
import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@Test func inMemoryContainerAcceptsEveryModel() throws {
    let container = try BabySchema.container(inMemory: true)
    let context = ModelContext(container)

    let baby = Baby(name: "Mira", birthDate: .now)
    context.insert(baby)

    let feed = FeedEntry(babyID: baby.id, kind: .breast, startedAt: .now, timeZoneID: "Europe/Berlin")
    feed.sides = [SideInterval(side: .left, seconds: 1_080)]
    context.insert(feed)

    try context.save()

    let feeds = try context.fetch(FetchDescriptor<FeedEntry>())
    #expect(feeds.count == 1)
    #expect(feeds[0].kind == .breast)
    #expect(feeds[0].sides.first?.seconds == 1_080)
}

@Test func typedAccessorsReadAndWriteTheRawColumn() {
    let feed = FeedEntry(babyID: UUID(), kind: .bottle, startedAt: .now, timeZoneID: "UTC")

    #expect(feed.kindRaw == "bottle")

    feed.kind = .solids
    #expect(feed.kindRaw == "solids")
    #expect(feed.kind == .solids)
}

@Test func comboFeedIsDetectedByFieldsNotByKind() {
    let feed = FeedEntry(babyID: UUID(), kind: .breast, startedAt: .now, timeZoneID: "UTC")
    #expect(feed.isCombo == false)

    feed.sides = [SideInterval(side: .left, seconds: 300)]
    feed.volumeML = 60
    #expect(feed.isCombo)
    // kind still records how it started; it does not gate which fields are set.
    #expect(feed.kind == .breast)
}

@Test func entriesStartUndeletedAndStampTheirOwnTimezone() {
    let sleep = SleepEntry(babyID: UUID(), startedAt: .now, timeZoneID: "America/New_York")

    #expect(sleep.isTombstoned == false)
    #expect(sleep.timeZoneID == "America/New_York")
    #expect(sleep.endedAt == nil)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter ModelTests`
Expected: FAIL — `cannot find 'BabySchema' in scope`

- [ ] **Step 3: Implement Baby and the entry models**

`Sources/BabyStore/Models/Baby.swift`:

```swift
import Foundation
import SwiftData

@Model
public final class Baby {
    public var id: UUID = UUID()
    public var name: String = ""
    public var birthDate: Date = Date()
    public var photoData: Data?
    public var sortOrder: Int = 0
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    public init(id: UUID = UUID(), name: String, birthDate: Date, sortOrder: Int = 0) {
        self.id = id
        self.name = name
        self.birthDate = birthDate
        self.sortOrder = sortOrder
        self.lastModifiedAt = Date()
        self.isTombstoned = false
    }
}
```

`Sources/BabyStore/Models/FeedEntry.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

@Model
public final class FeedEntry {
    public var id: UUID = UUID()
    public var babyID: UUID = UUID()
    public var startedAt: Date = Date()
    public var endedAt: Date?
    public var timeZoneID: String = "UTC"
    public var note: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    /// How the feed started. Drives icon and default edit sheet only — never
    /// which fields are populated. Aggregates must read the fields themselves.
    public var kindRaw: String = FeedKind.breast.rawValue
    public var sides: [SideInterval] = []
    public var volumeML: Double?
    public var contentsRaw: String?
    public var foods: [String] = []
    public var amountRaw: String?

    public init(
        id: UUID = UUID(),
        babyID: UUID,
        kind: FeedKind,
        startedAt: Date,
        timeZoneID: String,
        createdBy: String = ""
    ) {
        self.id = id
        self.babyID = babyID
        self.kindRaw = kind.rawValue
        self.startedAt = startedAt
        self.timeZoneID = timeZoneID
        self.createdBy = createdBy
        self.lastModifiedAt = startedAt
        self.isTombstoned = false
    }

    public var kind: FeedKind {
        get { FeedKind(rawValue: kindRaw) ?? .breast }
        set { kindRaw = newValue.rawValue }
    }

    public var contents: BottleContents? {
        get { contentsRaw.flatMap(BottleContents.init(rawValue:)) }
        set { contentsRaw = newValue?.rawValue }
    }

    public var amount: SolidsAmount? {
        get { amountRaw.flatMap(SolidsAmount.init(rawValue:)) }
        set { amountRaw = newValue?.rawValue }
    }

    /// A combination feed has both breast intervals and a bottle volume.
    public var isCombo: Bool {
        !sides.isEmpty && volumeML != nil
    }

    public var totalBreastSeconds: Int {
        sides.reduce(0) { $0 + $1.seconds }
    }

    public var lastSideUsed: BreastSide? {
        sides.last?.side
    }
}
```

`Sources/BabyStore/Models/SleepEntry.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

@Model
public final class SleepEntry {
    public var id: UUID = UUID()
    public var babyID: UUID = UUID()
    public var startedAt: Date = Date()
    public var endedAt: Date?
    public var timeZoneID: String = "UTC"
    public var note: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    /// Set only when the user overrides the derived classification.
    public var kindOverrideRaw: String?

    public init(
        id: UUID = UUID(),
        babyID: UUID,
        startedAt: Date,
        timeZoneID: String,
        createdBy: String = ""
    ) {
        self.id = id
        self.babyID = babyID
        self.startedAt = startedAt
        self.timeZoneID = timeZoneID
        self.createdBy = createdBy
        self.lastModifiedAt = startedAt
        self.isTombstoned = false
    }

    public var kindOverride: SleepKind? {
        get { kindOverrideRaw.flatMap(SleepKind.init(rawValue:)) }
        set { kindOverrideRaw = newValue?.rawValue }
    }

    /// The override wins; otherwise derive from the local start hour.
    public var kind: SleepKind {
        kindOverride ?? SleepClassifier.classify(startedAt: startedAt, timeZoneID: timeZoneID)
    }

    public var durationSeconds: Int? {
        endedAt.map { Int($0.timeIntervalSince(startedAt)) }
    }
}
```

`Sources/BabyStore/Models/DiaperEntry.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

@Model
public final class DiaperEntry {
    public var id: UUID = UUID()
    public var babyID: UUID = UUID()
    public var startedAt: Date = Date()
    public var endedAt: Date?
    public var timeZoneID: String = "UTC"
    public var note: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    public var contentsRaw: String = DiaperContents.wet.rawValue
    public var consistency: String?
    public var color: String?

    public init(
        id: UUID = UUID(),
        babyID: UUID,
        contents: DiaperContents,
        startedAt: Date,
        timeZoneID: String,
        createdBy: String = ""
    ) {
        self.id = id
        self.babyID = babyID
        self.contentsRaw = contents.rawValue
        self.startedAt = startedAt
        self.timeZoneID = timeZoneID
        self.createdBy = createdBy
        self.lastModifiedAt = startedAt
        self.isTombstoned = false
    }

    public var contents: DiaperContents {
        get { DiaperContents(rawValue: contentsRaw) ?? .wet }
        set { contentsRaw = newValue.rawValue }
    }
}
```

- [ ] **Step 4: Implement ActiveTimer, ChangeLogEntry, and the schema**

`Sources/BabyStore/Models/ActiveTimer.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

/// A running timer is data, not view state. This is what makes a feed survive
/// a force-quit, a reboot, and the Live Activity.
@Model
public final class ActiveTimer {
    public var id: UUID = UUID()
    public var babyID: UUID = UUID()
    public var kindRaw: String = TimerKind.feed.rawValue
    public var startedAt: Date = Date()
    public var timeZoneID: String = "UTC"
    public var liveActivityID: String?

    /// Breast intervals completed before the current one, for feed timers.
    public var completedSides: [SideInterval] = []
    /// Side currently running, for feed timers.
    public var currentSideRaw: String?
    /// When the current side started. Differs from startedAt after a switch.
    public var currentSideStartedAt: Date = Date()

    public init(
        id: UUID = UUID(),
        babyID: UUID,
        kind: TimerKind,
        startedAt: Date,
        timeZoneID: String,
        currentSide: BreastSide? = nil
    ) {
        self.id = id
        self.babyID = babyID
        self.kindRaw = kind.rawValue
        self.startedAt = startedAt
        self.currentSideStartedAt = startedAt
        self.timeZoneID = timeZoneID
        self.currentSideRaw = currentSide?.rawValue
    }

    public var kind: TimerKind {
        get { TimerKind(rawValue: kindRaw) ?? .feed }
        set { kindRaw = newValue.rawValue }
    }

    public var currentSide: BreastSide? {
        get { currentSideRaw.flatMap(BreastSide.init(rawValue:)) }
        set { currentSideRaw = newValue?.rawValue }
    }

    /// Always computed from stored timestamps, never accumulated in memory.
    public func elapsedSeconds(now: Date) -> Int {
        max(0, Int(now.timeIntervalSince(startedAt)))
    }

    public func currentSideSeconds(now: Date) -> Int {
        max(0, Int(now.timeIntervalSince(currentSideStartedAt)))
    }
}
```

`Sources/BabyStore/Models/ChangeLogEntry.swift`:

```swift
import Foundation
import SwiftData

public enum ChangeOperation: String, Codable, Sendable, CaseIterable {
    case create, update, delete
}

/// Append-only outbox. Nothing drains it in v1; the future sync node will.
@Model
public final class ChangeLogEntry {
    public var id: UUID = UUID()
    public var entityID: UUID = UUID()
    public var entityType: String = ""
    public var operationRaw: String = ChangeOperation.create.rawValue
    public var payload: Data = Data()
    public var timestamp: Date = Date()
    public var deviceID: String = ""
    public var syncedAt: Date?

    public init(
        entityID: UUID,
        entityType: String,
        operation: ChangeOperation,
        payload: Data,
        timestamp: Date,
        deviceID: String
    ) {
        self.entityID = entityID
        self.entityType = entityType
        self.operationRaw = operation.rawValue
        self.payload = payload
        self.timestamp = timestamp
        self.deviceID = deviceID
    }

    public var operation: ChangeOperation {
        get { ChangeOperation(rawValue: operationRaw) ?? .create }
        set { operationRaw = newValue.rawValue }
    }
}
```

`Sources/BabyStore/Models/Schema.swift`:

```swift
import Foundation
import SwiftData

public enum BabySchema {
    public static let all: [any PersistentModel.Type] = [
        Baby.self,
        FeedEntry.self,
        SleepEntry.self,
        DiaperEntry.self,
        ActiveTimer.self,
        ChangeLogEntry.self,
    ]

    /// The App Group is what lets the app, the widget extension, and Live
    /// Activity intents read one database instead of three.
    public static let appGroupID = "group.com.example.babytracker"

    public static func container(inMemory: Bool) throws -> ModelContainer {
        let schema = Schema(all)
        let configuration: ModelConfiguration = if inMemory {
            ModelConfiguration(schema: schema, isStoredInMemoryOnly: true)
        } else {
            ModelConfiguration(schema: schema, groupContainer: .identifier(appGroupID))
        }
        return try ModelContainer(for: schema, configurations: configuration)
    }
}
```

- [ ] **Step 5: Run the test and confirm it passes**

Run: `swift test --filter ModelTests`
Expected: PASS, 4 tests

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add SwiftData models with raw-column enum storage"
```

---

### Task 7: BabyStore and the change-log choke point

**Files:**
- Create: `Sources/BabyStore/BabyStore.swift`
- Create: `Sources/BabyStore/EntryPayload.swift`
- Test: `Tests/BabyStoreTests/ChangeLogTests.swift`

**Interfaces:**
- Consumes: Task 6 models, `DateProvider` (Task 1)
- Produces: `BabyStore(container:dateProvider:deviceID:)`; `BabyStore.inMemory(dateProvider:)` test helper; `store.context: ModelContext`; `store.record(_ operation: ChangeOperation, on entity: any ChangeLoggable) throws`; `store.changeLog() throws -> [ChangeLogEntry]`; protocol `ChangeLoggable` with `var entityID: UUID`, `var entityTypeName: String`, `func payloadData() throws -> Data`

This is the single most important task in the plan. Every later mutation goes through `record`, and the invariant that no mutation exists without a matching change-log entry is what makes the future sync node possible.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/ChangeLogTests.swift`:

```swift
import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@Test func everyMutationAppendsExactlyOneChangeLogEntry() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)

    let baby = Baby(name: "Mira", birthDate: .now)
    store.context.insert(baby)
    try store.record(.create, on: baby)

    let log = try store.changeLog()
    #expect(log.count == 1)
    #expect(log[0].entityID == baby.id)
    #expect(log[0].entityType == "Baby")
    #expect(log[0].operation == .create)
    #expect(log[0].timestamp == clock.now)
}

@Test func changeLogPayloadCarriesTheEntitysFields() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))

    let feed = FeedEntry(babyID: UUID(), kind: .bottle, startedAt: .now, timeZoneID: "UTC")
    feed.volumeML = 120
    feed.contents = .formula
    store.context.insert(feed)
    try store.record(.create, on: feed)

    let payload = try store.changeLog()[0].payload
    let decoded = try JSONSerialization.jsonObject(with: payload) as! [String: Any]

    #expect(decoded["volumeML"] as? Double == 120)
    #expect(decoded["contentsRaw"] as? String == "formula")
    #expect(decoded["kindRaw"] as? String == "bottle")
}

@Test func operationsAccumulateInOrder() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)

    let baby = Baby(name: "Mira", birthDate: .now)
    store.context.insert(baby)
    try store.record(.create, on: baby)

    clock.advance(by: 60)
    baby.name = "Mira R"
    try store.record(.update, on: baby)

    clock.advance(by: 60)
    try store.record(.delete, on: baby)

    let log = try store.changeLog()
    #expect(log.map(\.operation) == [.create, .update, .delete])
    #expect(log[2].timestamp.timeIntervalSince(log[0].timestamp) == 120)
}

@Test func changeLogStampsTheOriginatingDevice() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now), deviceID: "device-abc")

    let baby = Baby(name: "Mira", birthDate: .now)
    store.context.insert(baby)
    try store.record(.create, on: baby)

    #expect(try store.changeLog()[0].deviceID == "device-abc")
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter ChangeLogTests`
Expected: FAIL — `cannot find 'BabyStore' in scope`

- [ ] **Step 3: Implement the payload protocol**

`Sources/BabyStore/EntryPayload.swift`:

```swift
import Foundation

/// Anything the change log can carry. Payloads are plain JSON dictionaries of
/// stored columns, so the future sync node can read them without linking
/// SwiftData or this package's types.
public protocol ChangeLoggable {
    var entityID: UUID { get }
    var entityTypeName: String { get }
    func payloadFields() -> [String: Any]
}

public extension ChangeLoggable {
    func payloadData() throws -> Data {
        try JSONSerialization.data(withJSONObject: payloadFields(), options: [.sortedKeys])
    }
}

extension Baby: ChangeLoggable {
    public var entityID: UUID { id }
    public var entityTypeName: String { "Baby" }
    public func payloadFields() -> [String: Any] {
        [
            "id": id.uuidString,
            "name": name,
            "birthDate": birthDate.timeIntervalSince1970,
            "sortOrder": sortOrder,
            "isTombstoned": isTombstoned,
        ]
    }
}

extension FeedEntry: ChangeLoggable {
    public var entityID: UUID { id }
    public var entityTypeName: String { "FeedEntry" }
    public func payloadFields() -> [String: Any] {
        var fields: [String: Any] = [
            "id": id.uuidString,
            "babyID": babyID.uuidString,
            "startedAt": startedAt.timeIntervalSince1970,
            "timeZoneID": timeZoneID,
            "kindRaw": kindRaw,
            "sides": sides.map { ["side": $0.side.rawValue, "seconds": $0.seconds] },
            "foods": foods,
            "isTombstoned": isTombstoned,
        ]
        fields["endedAt"] = endedAt?.timeIntervalSince1970
        fields["volumeML"] = volumeML
        fields["contentsRaw"] = contentsRaw
        fields["amountRaw"] = amountRaw
        fields["note"] = note
        return fields
    }
}

extension SleepEntry: ChangeLoggable {
    public var entityID: UUID { id }
    public var entityTypeName: String { "SleepEntry" }
    public func payloadFields() -> [String: Any] {
        var fields: [String: Any] = [
            "id": id.uuidString,
            "babyID": babyID.uuidString,
            "startedAt": startedAt.timeIntervalSince1970,
            "timeZoneID": timeZoneID,
            "isTombstoned": isTombstoned,
        ]
        fields["endedAt"] = endedAt?.timeIntervalSince1970
        fields["kindOverrideRaw"] = kindOverrideRaw
        fields["note"] = note
        return fields
    }
}

extension DiaperEntry: ChangeLoggable {
    public var entityID: UUID { id }
    public var entityTypeName: String { "DiaperEntry" }
    public func payloadFields() -> [String: Any] {
        var fields: [String: Any] = [
            "id": id.uuidString,
            "babyID": babyID.uuidString,
            "startedAt": startedAt.timeIntervalSince1970,
            "timeZoneID": timeZoneID,
            "contentsRaw": contentsRaw,
            "isTombstoned": isTombstoned,
        ]
        fields["consistency"] = consistency
        fields["color"] = color
        fields["note"] = note
        return fields
    }
}
```

- [ ] **Step 4: Implement BabyStore**

`Sources/BabyStore/BabyStore.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

/// Owns the only ModelContext in the system. Nothing outside this module
/// touches SwiftData directly — features call intent-named methods instead.
@MainActor
@Observable
public final class BabyStore {
    public let container: ModelContainer
    public let context: ModelContext
    let dateProvider: DateProvider
    let deviceID: String

    public init(container: ModelContainer, dateProvider: DateProvider, deviceID: String) {
        self.container = container
        self.context = ModelContext(container)
        self.dateProvider = dateProvider
        self.deviceID = deviceID
    }

    public static func inMemory(
        dateProvider: DateProvider,
        deviceID: String = "test-device"
    ) throws -> BabyStore {
        BabyStore(
            container: try BabySchema.container(inMemory: true),
            dateProvider: dateProvider,
            deviceID: deviceID
        )
    }

    public var now: Date { dateProvider.now }

    /// The choke point. Writes the change-log entry and saves in one
    /// transaction, so a mutation can never exist without its log entry.
    public func record(_ operation: ChangeOperation, on entity: some ChangeLoggable) throws {
        let entry = ChangeLogEntry(
            entityID: entity.entityID,
            entityType: entity.entityTypeName,
            operation: operation,
            payload: try entity.payloadData(),
            timestamp: now,
            deviceID: deviceID
        )
        context.insert(entry)
        try context.save()
    }

    public func changeLog() throws -> [ChangeLogEntry] {
        var descriptor = FetchDescriptor<ChangeLogEntry>()
        descriptor.sortBy = [SortDescriptor(\.timestamp, order: .forward)]
        return try context.fetch(descriptor)
    }

    /// Unsynced entries, oldest first. Nothing calls this in v1 — it exists so
    /// the seam is real rather than imagined.
    public func pendingChanges() throws -> [ChangeLogEntry] {
        var descriptor = FetchDescriptor<ChangeLogEntry>(
            predicate: #Predicate { $0.syncedAt == nil }
        )
        descriptor.sortBy = [SortDescriptor(\.timestamp, order: .forward)]
        return try context.fetch(descriptor)
    }
}
```

Note: tests calling `@MainActor` code need `@MainActor` on the test function. Add `@MainActor` to each `@Test` in `ChangeLogTests.swift` and every later store test.

- [ ] **Step 5: Run the test and confirm it passes**

Run: `swift test --filter ChangeLogTests`
Expected: PASS, 4 tests

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add BabyStore with change-log choke point"
```

---

### Task 8: Entry mutations and tombstones

**Files:**
- Create: `Sources/BabyStore/BabyStore+Diaper.swift`
- Create: `Sources/BabyStore/BabyStore+Sleep.swift`
- Create: `Sources/BabyStore/BabyStore+Babies.swift`
- Test: `Tests/BabyStoreTests/MutationTests.swift`

**Interfaces:**
- Consumes: Task 7 `BabyStore`, `record`
- Produces: `addBaby(name:birthDate:) throws -> Baby`; `babies() throws -> [Baby]`; `logDiaper(_ contents: DiaperContents, for babyID: UUID, at date: Date? = nil) throws -> DiaperEntry`; `logSleep(from:to:for:) throws -> SleepEntry`; `update(_ entry: some ChangeLoggable & Touchable) throws`; `softDelete(_ entry: some ChangeLoggable & Touchable) throws`; protocol `Touchable`. Deletion sets `isTombstoned` and logs `.delete`; it never removes the row.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/MutationTests.swift`:

```swift
import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@MainActor
@Test func loggingADiaperStampsNowAndTheDeviceTimezone() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let entry = try store.logDiaper(.wet, for: baby.id)

    #expect(entry.startedAt == clock.now)
    #expect(entry.contents == .wet)
    #expect(entry.timeZoneID == TimeZone.current.identifier)
}

@MainActor
@Test func loggingAcceptsABackdatedTime() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let fortyMinutesAgo = clock.now.addingTimeInterval(-2_400)
    let entry = try store.logDiaper(.dirty, for: baby.id, at: fortyMinutesAgo)

    #expect(entry.startedAt == fortyMinutesAgo)
}

@MainActor
@Test func softDeleteKeepsTheRowAndLogsTheDeletion() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    let entry = try store.logDiaper(.wet, for: baby.id)

    try store.softDelete(entry)

    // The row survives — a sync node cannot replicate a deletion it never saw.
    let all = try store.context.fetch(FetchDescriptor<DiaperEntry>())
    #expect(all.count == 1)
    #expect(all[0].isTombstoned)

    let log = try store.changeLog()
    #expect(log.last?.operation == .delete)
}

@MainActor
@Test func everyMutationTypeProducesALogEntry() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))

    let baby = try store.addBaby(name: "Mira", birthDate: .now)      // 1 create
    let entry = try store.logDiaper(.wet, for: baby.id)              // 2 create
    entry.contents = .mixed
    try store.update(entry)                                          // 3 update
    try store.softDelete(entry)                                      // 4 delete

    #expect(try store.changeLog().count == 4)
}

@MainActor
@Test func updateRefreshesLastModified() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    let entry = try store.logDiaper(.wet, for: baby.id)
    let originalModified = entry.lastModifiedAt

    clock.advance(by: 300)
    entry.contents = .dirty
    try store.update(entry)

    #expect(entry.lastModifiedAt == clock.now)
    #expect(entry.lastModifiedAt > originalModified)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter MutationTests`
Expected: FAIL — `value of type 'BabyStore' has no member 'addBaby'`

- [ ] **Step 3: Implement baby management and shared mutation helpers**

`Sources/BabyStore/BabyStore+Babies.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

public extension BabyStore {
    func addBaby(name: String, birthDate: Date) throws -> Baby {
        let existing = try babies()
        let baby = Baby(name: name, birthDate: birthDate, sortOrder: existing.count)
        context.insert(baby)
        try record(.create, on: baby)
        return baby
    }

    func babies() throws -> [Baby] {
        var descriptor = FetchDescriptor<Baby>(predicate: #Predicate { !$0.isTombstoned })
        descriptor.sortBy = [SortDescriptor(\.sortOrder, order: .forward)]
        return try context.fetch(descriptor)
    }

    /// Re-stamps lastModifiedAt and logs the update. Callers mutate the object
    /// first, then call this.
    func update(_ entry: some ChangeLoggable & Touchable) throws {
        entry.lastModifiedAt = now
        try record(.update, on: entry)
    }

    /// Tombstone. The row is never removed.
    func softDelete(_ entry: some ChangeLoggable & Touchable) throws {
        entry.isTombstoned = true
        entry.lastModifiedAt = now
        try record(.delete, on: entry)
    }
}

/// Anything carrying the common mutable bookkeeping columns.
public protocol Touchable: AnyObject {
    var lastModifiedAt: Date { get set }
    var isTombstoned: Bool { get set }
}

extension Baby: Touchable {}
extension FeedEntry: Touchable {}
extension SleepEntry: Touchable {}
extension DiaperEntry: Touchable {}
```

- [ ] **Step 4: Implement diaper and sleep logging**

`Sources/BabyStore/BabyStore+Diaper.swift`:

```swift
import Foundation
import BabyCore

public extension BabyStore {
    /// `at` defaults to now. Every logging call accepts a correction, because
    /// remembering a change forty minutes late is the normal case.
    @discardableResult
    func logDiaper(
        _ contents: DiaperContents,
        for babyID: UUID,
        at date: Date? = nil
    ) throws -> DiaperEntry {
        let entry = DiaperEntry(
            babyID: babyID,
            contents: contents,
            startedAt: date ?? now,
            timeZoneID: TimeZone.current.identifier,
            createdBy: deviceID
        )
        context.insert(entry)
        try record(.create, on: entry)
        return entry
    }
}
```

`Sources/BabyStore/BabyStore+Sleep.swift`:

```swift
import Foundation
import BabyCore

public extension BabyStore {
    /// A completed sleep, entered after the fact. Live sleeps go through the
    /// timer API in Task 9.
    @discardableResult
    func logSleep(from start: Date, to end: Date?, for babyID: UUID) throws -> SleepEntry {
        let entry = SleepEntry(
            babyID: babyID,
            startedAt: start,
            timeZoneID: TimeZone.current.identifier,
            createdBy: deviceID
        )
        entry.endedAt = end
        context.insert(entry)
        try record(.create, on: entry)
        return entry
    }
}
```

- [ ] **Step 5: Run the test and confirm it passes**

Run: `swift test --filter MutationTests`
Expected: PASS, 5 tests

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add entry mutations with tombstone deletes"
```

---

### Task 9: Timer lifecycle

**Files:**
- Create: `Sources/BabyStore/BabyStore+Feeding.swift`
- Create: `Sources/BabyStore/BabyStore+Timers.swift`
- Test: `Tests/BabyStoreTests/TimerTests.swift`

**Interfaces:**
- Consumes: Task 7 `BabyStore`, Task 6 `ActiveTimer`
- Produces: `startFeed(side: BreastSide?, for babyID: UUID) throws -> ActiveTimer`; `switchSide(on timer: ActiveTimer) throws`; `stopFeed(_ timer: ActiveTimer) throws -> FeedEntry`; `startSleep(for babyID: UUID) throws -> ActiveTimer`; `stopSleep(_ timer: ActiveTimer) throws -> SleepEntry`; `activeTimers(for babyID: UUID) throws -> [ActiveTimer]`; `activeTimer(kind: TimerKind, for babyID: UUID) throws -> ActiveTimer?`; `logBottle(volumeML:contents:for:at:) throws -> FeedEntry`; `suggestedSide(for babyID: UUID) throws -> BreastSide`

Elapsed time is always `now − startedAt` read from the persisted timer. Nothing accumulates in memory, which is why the force-quit test below can pass.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/TimerTests.swift`:

```swift
import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@MainActor
private func makeStore(_ clock: TestDateProvider) throws -> (BabyStore, UUID) {
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    return (store, baby.id)
}

@MainActor
@Test func elapsedSurvivesTheAppBeingKilledMidFeed() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let timer = try store.startFeed(side: .left, for: babyID)

    // Two hours pass with the process dead. A fresh store over the same
    // container is exactly what a relaunch looks like.
    clock.advance(by: 7_200)
    let relaunched = BabyStore(
        container: store.container,
        dateProvider: clock,
        deviceID: "test-device"
    )
    let recovered = try relaunched.activeTimer(kind: .feed, for: babyID)

    #expect(recovered != nil)
    #expect(recovered!.elapsedSeconds(now: clock.now) == 7_200)
    #expect(recovered!.id == timer.id)
}

@MainActor
@Test func switchingSideAppendsAnIntervalRatherThanResetting() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let timer = try store.startFeed(side: .left, for: babyID)
    clock.advance(by: 600)
    try store.switchSide(on: timer)
    clock.advance(by: 480)
    let feed = try store.stopFeed(timer)

    #expect(feed.sides.count == 2)
    #expect(feed.sides[0] == SideInterval(side: .left, seconds: 600))
    #expect(feed.sides[1] == SideInterval(side: .right, seconds: 480))
    #expect(feed.totalBreastSeconds == 1_080)
    #expect(feed.lastSideUsed == .right)
}

@MainActor
@Test func stoppingAFeedClearsTheTimerAndCreatesTheEntry() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let timer = try store.startFeed(side: .left, for: babyID)
    clock.advance(by: 1_080)
    let feed = try store.stopFeed(timer)

    #expect(feed.endedAt == clock.now)
    #expect(feed.startedAt == clock.now.addingTimeInterval(-1_080))
    #expect(try store.activeTimer(kind: .feed, for: babyID) == nil)
}

@MainActor
@Test func nextFeedSuggestsTheOppositeSide() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    // No history yet: left is as good a guess as any.
    #expect(try store.suggestedSide(for: babyID) == .left)

    let timer = try store.startFeed(side: .left, for: babyID)
    clock.advance(by: 600)
    _ = try store.stopFeed(timer)

    #expect(try store.suggestedSide(for: babyID) == .right)
}

@MainActor
@Test func sleepAndFeedCanRunAtTheSameTime() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    _ = try store.startSleep(for: babyID)
    clock.advance(by: 120)
    _ = try store.startFeed(side: .left, for: babyID)

    let running = try store.activeTimers(for: babyID)
    #expect(running.count == 2)
    // Most recently started first — this is what the dial shows.
    #expect(running[0].kind == .feed)
    #expect(running[1].kind == .sleep)
}

@MainActor
@Test func startingASecondTimerOfTheSameKindReusesTheRunningOne() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let first = try store.startFeed(side: .left, for: babyID)
    clock.advance(by: 60)
    let second = try store.startFeed(side: .right, for: babyID)

    // The invariant is at most one per (baby, kind). A double-tap must not
    // silently discard the running feed.
    #expect(first.id == second.id)
    #expect(try store.activeTimers(for: babyID).count == 1)
}

@MainActor
@Test func stoppingSleepWritesTheSleepEntry() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let timer = try store.startSleep(for: babyID)
    clock.advance(by: 6_600)
    let sleep = try store.stopSleep(timer)

    #expect(sleep.durationSeconds == 6_600)
    #expect(try store.activeTimer(kind: .sleep, for: babyID) == nil)
    #expect(try store.changeLog().last?.entityType == "SleepEntry")
}

@MainActor
@Test func bottleFeedsAreLoggedWithoutATimer() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let (store, babyID) = try makeStore(clock)

    let feed = try store.logBottle(volumeML: 120, contents: .formula, for: babyID)

    #expect(feed.kind == .bottle)
    #expect(feed.volumeML == 120)
    #expect(feed.contents == .formula)
    #expect(feed.endedAt == clock.now)
    #expect(try store.activeTimers(for: babyID).isEmpty)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter TimerTests`
Expected: FAIL — `value of type 'BabyStore' has no member 'startFeed'`

- [ ] **Step 3: Implement timer queries**

`Sources/BabyStore/BabyStore+Timers.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

public extension BabyStore {
    /// Running timers, most recently started first. The dial shows the first;
    /// any others appear as live rows with their own stop controls.
    func activeTimers(for babyID: UUID) throws -> [ActiveTimer] {
        var descriptor = FetchDescriptor<ActiveTimer>(
            predicate: #Predicate { $0.babyID == babyID }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .reverse)]
        return try context.fetch(descriptor)
    }

    func activeTimer(kind: TimerKind, for babyID: UUID) throws -> ActiveTimer? {
        let raw = kind.rawValue
        let descriptor = FetchDescriptor<ActiveTimer>(
            predicate: #Predicate { $0.babyID == babyID && $0.kindRaw == raw }
        )
        return try context.fetch(descriptor).first
    }
}
```

- [ ] **Step 4: Implement feeding and sleep timers**

`Sources/BabyStore/BabyStore+Feeding.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

public extension BabyStore {
    /// Pre-selects the side opposite the last completed breast feed.
    func suggestedSide(for babyID: UUID) throws -> BreastSide {
        let breast = FeedKind.breast.rawValue
        var descriptor = FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.babyID == babyID && !$0.isTombstoned && $0.kindRaw == breast }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .reverse)]
        descriptor.fetchLimit = 1

        guard let last = try context.fetch(descriptor).first, let side = last.lastSideUsed else {
            return .left
        }
        return side.opposite
    }

    /// Idempotent per (baby, feed): tapping Feed while a feed runs returns the
    /// running timer rather than discarding it.
    @discardableResult
    func startFeed(side: BreastSide?, for babyID: UUID) throws -> ActiveTimer {
        if let running = try activeTimer(kind: .feed, for: babyID) {
            return running
        }
        let timer = ActiveTimer(
            babyID: babyID,
            kind: .feed,
            startedAt: now,
            timeZoneID: TimeZone.current.identifier,
            currentSide: side ?? (try suggestedSide(for: babyID))
        )
        context.insert(timer)
        try context.save()
        return timer
    }

    /// Closes the current interval and opens one on the other side.
    func switchSide(on timer: ActiveTimer) throws {
        guard let side = timer.currentSide else { return }
        timer.completedSides.append(
            SideInterval(side: side, seconds: timer.currentSideSeconds(now: now))
        )
        timer.currentSide = side.opposite
        timer.currentSideStartedAt = now
        try context.save()
    }

    @discardableResult
    func stopFeed(_ timer: ActiveTimer) throws -> FeedEntry {
        let entry = FeedEntry(
            babyID: timer.babyID,
            kind: .breast,
            startedAt: timer.startedAt,
            timeZoneID: timer.timeZoneID,
            createdBy: deviceID
        )
        var intervals = timer.completedSides
        if let side = timer.currentSide {
            intervals.append(
                SideInterval(side: side, seconds: timer.currentSideSeconds(now: now))
            )
        }
        entry.sides = intervals
        entry.endedAt = now

        context.insert(entry)
        context.delete(timer)   // ActiveTimer is scratch state, not a record
        try record(.create, on: entry)
        return entry
    }

    /// Bottles have no timer — volume and contents are known at log time.
    @discardableResult
    func logBottle(
        volumeML: Double,
        contents: BottleContents,
        for babyID: UUID,
        at date: Date? = nil
    ) throws -> FeedEntry {
        let stamp = date ?? now
        let entry = FeedEntry(
            babyID: babyID,
            kind: .bottle,
            startedAt: stamp,
            timeZoneID: TimeZone.current.identifier,
            createdBy: deviceID
        )
        entry.volumeML = volumeML
        entry.contents = contents
        entry.endedAt = stamp

        context.insert(entry)
        try record(.create, on: entry)
        return entry
    }

    @discardableResult
    func startSleep(for babyID: UUID) throws -> ActiveTimer {
        if let running = try activeTimer(kind: .sleep, for: babyID) {
            return running
        }
        let timer = ActiveTimer(
            babyID: babyID,
            kind: .sleep,
            startedAt: now,
            timeZoneID: TimeZone.current.identifier
        )
        context.insert(timer)
        try context.save()
        return timer
    }

    @discardableResult
    func stopSleep(_ timer: ActiveTimer) throws -> SleepEntry {
        let entry = SleepEntry(
            babyID: timer.babyID,
            startedAt: timer.startedAt,
            timeZoneID: timer.timeZoneID,
            createdBy: deviceID
        )
        entry.endedAt = now

        context.insert(entry)
        context.delete(timer)
        try record(.create, on: entry)
        return entry
    }
}
```

- [ ] **Step 5: Run the test and confirm it passes**

Run: `swift test --filter TimerTests`
Expected: PASS, 8 tests

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add persisted timer lifecycle for feeds and sleep"
```

---

### Task 10: Home and history queries

**Files:**
- Create: `Sources/BabyStore/BabyStore+Queries.swift`
- Create: `Sources/BabyStore/HomeSnapshot.swift`
- Test: `Tests/BabyStoreTests/QueryTests.swift`

**Interfaces:**
- Consumes: Tasks 6–9
- Produces: `HomeSnapshot` (`lastFeed`, `lastSleep`, `lastDiaper`, `runningTimers`, `todayFeedCount`, `todaySleepSeconds`, `todayDiaperCount`, `todayVolumeML`); `homeSnapshot(for babyID: UUID) throws -> HomeSnapshot`; `lastFeed(for:) throws -> FeedEntry?`; `lastSleep(for:) throws -> SleepEntry?`; `lastDiaper(for:) throws -> DiaperEntry?`; `entries(for babyID: UUID, on day: DayKey) throws -> [HistoryItem]`; `HistoryItem` enum with `.feed`, `.sleep`, `.diaper` cases exposing `id`, `startedAt`, `timeZoneID`

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/QueryTests.swift`:

```swift
import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@MainActor
@Test func homeSnapshotReportsTheLastOfEachKind() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby.id)
    clock.advance(by: 3_600)
    _ = try store.logDiaper(.wet, for: baby.id)
    clock.advance(by: 3_600)
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)

    let snapshot = try store.homeSnapshot(for: baby.id)

    #expect(snapshot.lastFeed?.volumeML == 120)
    #expect(snapshot.lastDiaper?.contents == .wet)
    #expect(snapshot.lastSleep == nil)
    #expect(snapshot.todayFeedCount == 2)
    #expect(snapshot.todayVolumeML == 210)
    #expect(snapshot.todayDiaperCount == 1)
}

@MainActor
@Test func deletedEntriesAreInvisibleToQueries() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let diaper = try store.logDiaper(.wet, for: baby.id)
    try store.softDelete(diaper)

    let snapshot = try store.homeSnapshot(for: baby.id)
    #expect(snapshot.lastDiaper == nil)
    #expect(snapshot.todayDiaperCount == 0)
}

@MainActor
@Test func snapshotIsScopedToOneBaby() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let mira = try store.addBaby(name: "Mira", birthDate: .now)
    let theo = try store.addBaby(name: "Theo", birthDate: .now)

    _ = try store.logDiaper(.wet, for: mira.id)
    _ = try store.logDiaper(.dirty, for: theo.id)
    _ = try store.logDiaper(.mixed, for: theo.id)

    #expect(try store.homeSnapshot(for: mira.id).todayDiaperCount == 1)
    #expect(try store.homeSnapshot(for: theo.id).todayDiaperCount == 2)
}

@MainActor
@Test func runningTimersAppearInTheSnapshotMostRecentFirst() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    _ = try store.startSleep(for: baby.id)
    clock.advance(by: 300)
    _ = try store.startFeed(side: .left, for: baby.id)

    let snapshot = try store.homeSnapshot(for: baby.id)
    #expect(snapshot.runningTimers.count == 2)
    #expect(snapshot.runningTimers.first?.kind == .feed)
}

@MainActor
@Test func historyMergesAllKindsNewestFirst() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby.id)
    clock.advance(by: 600)
    _ = try store.logDiaper(.wet, for: baby.id)

    let day = DayBucket.key(for: clock.now, timeZoneID: TimeZone.current.identifier)
    let items = try store.entries(for: baby.id, on: day)

    #expect(items.count == 2)
    #expect(items[0].startedAt > items[1].startedAt)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter QueryTests`
Expected: FAIL — `cannot find 'HomeSnapshot' in scope`

- [ ] **Step 3: Implement the snapshot type**

`Sources/BabyStore/HomeSnapshot.swift`:

```swift
import Foundation
import BabyCore

/// Everything Home needs, assembled in one pass so the view does no querying.
@MainActor
public struct HomeSnapshot {
    public let lastFeed: FeedEntry?
    public let lastSleep: SleepEntry?
    public let lastDiaper: DiaperEntry?
    public let runningTimers: [ActiveTimer]
    public let todayFeedCount: Int
    public let todayVolumeML: Double
    public let todaySleepSeconds: Int
    public let todayDiaperCount: Int

    public var feedTimer: ActiveTimer? {
        runningTimers.first { $0.kind == .feed }
    }

    public var sleepTimer: ActiveTimer? {
        runningTimers.first { $0.kind == .sleep }
    }

    /// The timer the dial shows: the most recently started one.
    public var primaryTimer: ActiveTimer? {
        runningTimers.first
    }

    public var isAsleep: Bool {
        sleepTimer != nil
    }
}

/// One row in History, whichever kind it is.
@MainActor
public enum HistoryItem: Identifiable {
    case feed(FeedEntry)
    case sleep(SleepEntry)
    case diaper(DiaperEntry)

    public var id: UUID {
        switch self {
        case .feed(let entry): entry.id
        case .sleep(let entry): entry.id
        case .diaper(let entry): entry.id
        }
    }

    public var startedAt: Date {
        switch self {
        case .feed(let entry): entry.startedAt
        case .sleep(let entry): entry.startedAt
        case .diaper(let entry): entry.startedAt
        }
    }

    public var timeZoneID: String {
        switch self {
        case .feed(let entry): entry.timeZoneID
        case .sleep(let entry): entry.timeZoneID
        case .diaper(let entry): entry.timeZoneID
        }
    }
}
```

- [ ] **Step 4: Implement the queries**

`Sources/BabyStore/BabyStore+Queries.swift`:

```swift
import Foundation
import SwiftData
import BabyCore

public extension BabyStore {
    func lastFeed(for babyID: UUID) throws -> FeedEntry? {
        var descriptor = FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.babyID == babyID && !$0.isTombstoned }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .reverse)]
        descriptor.fetchLimit = 1
        return try context.fetch(descriptor).first
    }

    func lastSleep(for babyID: UUID) throws -> SleepEntry? {
        var descriptor = FetchDescriptor<SleepEntry>(
            predicate: #Predicate { $0.babyID == babyID && !$0.isTombstoned }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .reverse)]
        descriptor.fetchLimit = 1
        return try context.fetch(descriptor).first
    }

    func lastDiaper(for babyID: UUID) throws -> DiaperEntry? {
        var descriptor = FetchDescriptor<DiaperEntry>(
            predicate: #Predicate { $0.babyID == babyID && !$0.isTombstoned }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .reverse)]
        descriptor.fetchLimit = 1
        return try context.fetch(descriptor).first
    }

    func homeSnapshot(for babyID: UUID) throws -> HomeSnapshot {
        let today = DayBucket.key(for: now, timeZoneID: TimeZone.current.identifier)
        let bounds = DayBucket.bounds(of: today, timeZoneID: TimeZone.current.identifier)
        let (start, end) = (bounds.start, bounds.end)

        let feeds = try context.fetch(FetchDescriptor<FeedEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        ))
        let sleeps = try context.fetch(FetchDescriptor<SleepEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        ))
        let diapers = try context.fetch(FetchDescriptor<DiaperEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        ))

        return HomeSnapshot(
            lastFeed: try lastFeed(for: babyID),
            lastSleep: try lastSleep(for: babyID),
            lastDiaper: try lastDiaper(for: babyID),
            runningTimers: try activeTimers(for: babyID),
            todayFeedCount: feeds.count,
            todayVolumeML: feeds.compactMap(\.volumeML).reduce(0, +),
            todaySleepSeconds: sleeps.compactMap(\.durationSeconds).reduce(0, +),
            todayDiaperCount: diapers.count
        )
    }

    /// One local day of history, newest first, all kinds merged.
    func entries(for babyID: UUID, on day: DayKey) throws -> [HistoryItem] {
        let bounds = DayBucket.bounds(of: day, timeZoneID: TimeZone.current.identifier)
        let (start, end) = (bounds.start, bounds.end)

        let feeds = try context.fetch(FetchDescriptor<FeedEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        )).map(HistoryItem.feed)

        let sleeps = try context.fetch(FetchDescriptor<SleepEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        )).map(HistoryItem.sleep)

        let diapers = try context.fetch(FetchDescriptor<DiaperEntry>(
            predicate: #Predicate {
                $0.babyID == babyID && !$0.isTombstoned && $0.startedAt >= start && $0.startedAt < end
            }
        )).map(HistoryItem.diaper)

        return (feeds + sleeps + diapers).sorted { $0.startedAt > $1.startedAt }
    }
}
```

- [ ] **Step 5: Run the test and confirm it passes**

Run: `swift test --filter QueryTests`
Expected: PASS, 5 tests

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add home snapshot and merged history queries"
```

---

### Task 11: The sync seam

**Files:**
- Create: `Sources/BabySync/SyncClient.swift`
- Create: `Sources/BabySync/LocalOnlySync.swift`
- Test: `Tests/BabySyncTests/SyncClientTests.swift`

**Interfaces:**
- Consumes: nothing from `BabyStore` — deliberately. `BabySync` speaks in `PendingChange` value types so the future node never links SwiftData.
- Produces: `PendingChange(id:entityID:entityType:operation:payload:timestamp:deviceID:)`; `protocol SyncClient` with `func push(_ changes: [PendingChange]) async throws -> [UUID]` and `func pull(since: Date?) async throws -> [PendingChange]`; `LocalOnlySync()`

Nothing wires this into the app in v1. It exists, is exercised by tests, and defines the shape the Go/Rust node must satisfy. A seam you've never run anything through is a seam you've only imagined.

- [ ] **Step 1: Write the failing test**

`Tests/BabySyncTests/SyncClientTests.swift`:

```swift
import Testing
import Foundation
@testable import BabySync

@Test func localOnlyAcknowledgesEverythingAndReturnsNothing() async throws {
    let client = LocalOnlySync()
    let change = PendingChange(
        entityID: UUID(),
        entityType: "DiaperEntry",
        operation: "create",
        payload: Data("{}".utf8),
        timestamp: Date(timeIntervalSince1970: 1_700_000_000),
        deviceID: "device-abc"
    )

    let acknowledged = try await client.push([change])
    #expect(acknowledged == [change.id])

    let pulled = try await client.pull(since: nil)
    #expect(pulled.isEmpty)
}

@Test func pushingNothingIsNotAnError() async throws {
    let client = LocalOnlySync()
    #expect(try await client.push([]).isEmpty)
}

@Test func pendingChangeRoundTripsThroughJSON() throws {
    let change = PendingChange(
        entityID: UUID(),
        entityType: "FeedEntry",
        operation: "update",
        payload: Data(#"{"volumeML":120}"#.utf8),
        timestamp: Date(timeIntervalSince1970: 1_700_000_000),
        deviceID: "device-abc"
    )

    let encoded = try JSONEncoder().encode(change)
    let decoded = try JSONDecoder().decode(PendingChange.self, from: encoded)

    #expect(decoded == change)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter SyncClientTests`
Expected: FAIL — `cannot find 'LocalOnlySync' in scope`

- [ ] **Step 3: Implement**

`Sources/BabySync/SyncClient.swift`:

```swift
import Foundation

/// The wire shape. Deliberately free of SwiftData and of BabyStore's types, so
/// the future self-hosted node can be written against this alone.
public struct PendingChange: Codable, Sendable, Equatable, Identifiable {
    public let id: UUID
    public let entityID: UUID
    public let entityType: String
    public let operation: String
    public let payload: Data
    public let timestamp: Date
    public let deviceID: String

    public init(
        id: UUID = UUID(),
        entityID: UUID,
        entityType: String,
        operation: String,
        payload: Data,
        timestamp: Date,
        deviceID: String
    ) {
        self.id = id
        self.entityID = entityID
        self.entityType = entityType
        self.operation = operation
        self.payload = payload
        self.timestamp = timestamp
        self.deviceID = deviceID
    }
}

public protocol SyncClient: Sendable {
    /// Returns the ids the server durably accepted. Anything absent stays pending.
    func push(_ changes: [PendingChange]) async throws -> [UUID]

    /// Changes from other devices since the given watermark.
    func pull(since: Date?) async throws -> [PendingChange]
}
```

`Sources/BabySync/LocalOnlySync.swift`:

```swift
import Foundation

/// v1's implementation. Accepts everything so the outbox drains, returns
/// nothing because there are no other devices yet.
public struct LocalOnlySync: SyncClient {
    public init() {}

    public func push(_ changes: [PendingChange]) async throws -> [UUID] {
        changes.map(\.id)
    }

    public func pull(since: Date?) async throws -> [PendingChange] {
        []
    }
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `swift test --filter SyncClientTests`
Expected: PASS, 3 tests

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add sync protocol seam with local-only implementation"
```

---

### Task 12: BabyUI design system

**Files:**
- Create: `Sources/BabyUI/Palette.swift`
- Create: `Sources/BabyUI/Typography.swift`
- Create: `Sources/BabyUI/GlassDock.swift`
- Test: `Tests/BabyUITests/PaletteTests.swift`
- Modify: `Package.swift` — add `.testTarget(name: "BabyUITests", dependencies: ["BabyUI"])`

**Interfaces:**
- Consumes: `BabyCore` enums
- Produces: `Palette.feed`, `.sleep`, `.diaper`, `.ground`, `.primaryText`, `.secondaryText`; `Tracker` enum (`.feed`, `.sleep`, `.diaper`) with `.color`, `.symbolName`, `.title`; `View.numeralStyle(_ size: NumeralSize)`; `GlassDock<Content>` view

Colors come from the approved Night Shift palette in the spec. Each tracker also has a distinct SF Symbol, because color is never the only carrier of meaning.

- [ ] **Step 1: Write the failing test**

`Tests/BabyUITests/PaletteTests.swift`:

```swift
import Testing
import SwiftUI
@testable import BabyUI

@Test func everyTrackerHasADistinctColorAndSymbol() {
    let colors = Tracker.allCases.map(\.color.description)
    let symbols = Tracker.allCases.map(\.symbolName)

    #expect(Set(colors).count == Tracker.allCases.count)
    #expect(Set(symbols).count == Tracker.allCases.count)
    // Color is never the only carrier of meaning.
    #expect(symbols.allSatisfy { !$0.isEmpty })
}

@Test func trackerTitlesAreSentenceCase() {
    #expect(Tracker.feed.title == "Feed")
    #expect(Tracker.sleep.title == "Sleep")
    #expect(Tracker.diaper.title == "Diaper")
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter PaletteTests`
Expected: FAIL — no such module `BabyUI` test target

- [ ] **Step 3: Implement the palette**

`Sources/BabyUI/Palette.swift`:

```swift
import SwiftUI

/// Night Shift. Amber leads because it is the accent least hostile to
/// dark-adapted eyes, and this app's peak usage is at 3am.
public enum Palette {
    public static let feed = Color(red: 0.949, green: 0.690, blue: 0.380)    // #F2B061
    public static let sleep = Color(red: 0.541, green: 0.608, blue: 0.941)   // #8A9BF0
    public static let diaper = Color(red: 0.373, green: 0.827, blue: 0.769)  // #5FD3C4
    public static let ground = Color(red: 0.051, green: 0.071, blue: 0.141)  // #0D1224
    public static let primaryText = Color(red: 0.953, green: 0.961, blue: 0.992)
    public static let secondaryText = Color(red: 0.604, green: 0.643, blue: 0.788)
}

public enum Tracker: String, CaseIterable, Sendable {
    case feed, sleep, diaper

    public var color: Color {
        switch self {
        case .feed: Palette.feed
        case .sleep: Palette.sleep
        case .diaper: Palette.diaper
        }
    }

    public var symbolName: String {
        switch self {
        case .feed: "drop.fill"
        case .sleep: "moon.fill"
        case .diaper: "shippingbox.fill"
        }
    }

    public var title: String {
        switch self {
        case .feed: "Feed"
        case .sleep: "Sleep"
        case .diaper: "Diaper"
        }
    }
}
```

- [ ] **Step 4: Implement typography and the glass dock**

`Sources/BabyUI/Typography.swift`:

```swift
import SwiftUI

public enum NumeralSize {
    case hero      // the running dial
    case display   // idle status rows
    case compact   // totals

    var textStyle: Font.TextStyle {
        switch self {
        case .hero: .largeTitle
        case .display: .title
        case .compact: .title3
        }
    }
}

public extension View {
    /// Rounded numerals with tabular figures, so a live-updating timer does not
    /// jitter as digits change width. Scales with Dynamic Type — never fixed.
    func numeralStyle(_ size: NumeralSize) -> some View {
        self
            .font(.system(size.textStyle, design: .rounded, weight: .bold))
            .monospacedDigit()
            .foregroundStyle(Palette.primaryText)
    }
}
```

`Sources/BabyUI/GlassDock.swift`:

```swift
import SwiftUI

/// The one place custom Liquid Glass appears. Anchored outside the scroll view
/// so it never collides with toolbar or tab-bar glass — glass cannot sample
/// glass, and overlapping produces artifacts.
public struct GlassDock<Content: View>: View {
    private let content: Content

    public init(@ViewBuilder content: () -> Content) {
        self.content = content()
    }

    public var body: some View {
        GlassEffectContainer {
            content
                .padding(9)
                .glassEffect(.regular, in: .rect(cornerRadius: 30))
        }
        .padding(.horizontal, 16)
    }
}
```

- [ ] **Step 5: Run the test and confirm it passes**

Run: `swift test --filter PaletteTests`
Expected: PASS, 2 tests

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add Night Shift design tokens and glass dock"
```

---

### Task 13: Home view model

**Files:**
- Create: `Sources/HomeFeature/HomeViewModel.swift`
- Test: `Tests/HomeFeatureTests/HomeViewModelTests.swift`
- Modify: `Package.swift` — add `.testTarget(name: "HomeFeatureTests", dependencies: ["HomeFeature"])`

**Interfaces:**
- Consumes: `BabyStore.homeSnapshot(for:)`, `ElapsedFormatter`, `Tracker`
- Produces: `HomeViewModel(store:babyID:)`; `.refresh()`; `.mode: HomeMode` (`.idle(IdleState)` or `.running(RunningState)`); `IdleState` with `feedRow`, `sleepRow`, `diaperRow`, `todayFeeds`, `todaySleep`, `todayDiapers`; `StatusRow(title:detail:value:tracker:isLive:)`; `RunningState` with `tracker`, `elapsedText`, `secondsIntoMinute`, `sideLabel`, `perSideText`, `secondaryRows`

All display strings are computed here so the views hold no logic and this task is fully testable without a simulator. The sleep row's stateful wording lives here.

- [ ] **Step 1: Write the failing test**

`Tests/HomeFeatureTests/HomeViewModelTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
@testable import HomeFeature

@MainActor
private func freshStore() throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    return (store, baby.id, clock)
}

@MainActor
@Test func idleModeWhenNothingIsRunning() throws {
    let (store, babyID, _) = try freshStore()
    let model = HomeViewModel(store: store, babyID: babyID)
    try model.refresh()

    guard case .idle = model.mode else {
        Issue.record("expected idle mode")
        return
    }
}

@MainActor
@Test func idleRowsReportRelativeAges() throws {
    let (store, babyID, clock) = try freshStore()
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: babyID)
    clock.advance(by: 8_040)   // 2h 14m

    let model = HomeViewModel(store: store, babyID: babyID)
    try model.refresh()

    guard case .idle(let state) = model.mode else {
        Issue.record("expected idle mode"); return
    }
    #expect(state.feedRow.value == "2h 14m")
    #expect(state.feedRow.title == "Last feed")
    #expect(state.feedRow.detail == "Bottle · 120 ml · formula")
}

@MainActor
@Test func sleepRowWordsItselfDifferentlyAwakeVersusAsleep() throws {
    let (store, babyID, clock) = try freshStore()

    // Asleep: live count, present tense.
    _ = try store.startSleep(for: babyID)
    clock.advance(by: 2_520)   // 42m
    let asleepModel = HomeViewModel(store: store, babyID: babyID)
    try asleepModel.refresh()

    guard case .running(let running) = asleepModel.mode else {
        Issue.record("expected running mode"); return
    }
    #expect(running.tracker == .sleep)

    // Awake: reports how long awake, with the previous sleep's length beneath.
    let timer = try store.activeTimer(kind: .sleep, for: babyID)!
    _ = try store.stopSleep(timer)
    clock.advance(by: 2_400)   // 40m awake

    let awakeModel = HomeViewModel(store: store, babyID: babyID)
    try awakeModel.refresh()

    guard case .idle(let state) = awakeModel.mode else {
        Issue.record("expected idle mode"); return
    }
    #expect(state.sleepRow.title == "Awake")
    #expect(state.sleepRow.value == "40m")
    #expect(state.sleepRow.detail == "Slept 42m before that")
}

@MainActor
@Test func emptyRowsSayNoneYetRatherThanShowingZero() throws {
    let (store, babyID, _) = try freshStore()
    let model = HomeViewModel(store: store, babyID: babyID)
    try model.refresh()

    guard case .idle(let state) = model.mode else {
        Issue.record("expected idle mode"); return
    }
    #expect(state.feedRow.value == "—")
    #expect(state.feedRow.detail == "Nothing logged yet")
}

@MainActor
@Test func runningStateShowsPerSideAndSecondsIntoTheMinute() throws {
    let (store, babyID, clock) = try freshStore()
    _ = try store.startFeed(side: .left, for: babyID)
    clock.advance(by: 1_122)   // 18:42

    let model = HomeViewModel(store: store, babyID: babyID)
    try model.refresh()

    guard case .running(let running) = model.mode else {
        Issue.record("expected running mode"); return
    }
    #expect(running.tracker == .feed)
    #expect(running.elapsedText == "18:42")
    #expect(running.sideLabel == "Feeding · left")
    #expect(running.perSideText == "18:42")
    // The ring is a seconds hand, never progress toward a target.
    #expect(running.secondsIntoMinute == 42)
}

@MainActor
@Test func theDialFollowsTheMostRecentlyStartedTimer() throws {
    let (store, babyID, clock) = try freshStore()
    _ = try store.startSleep(for: babyID)
    clock.advance(by: 600)
    _ = try store.startFeed(side: .left, for: babyID)
    clock.advance(by: 60)

    let model = HomeViewModel(store: store, babyID: babyID)
    try model.refresh()

    guard case .running(let running) = model.mode else {
        Issue.record("expected running mode"); return
    }
    #expect(running.tracker == .feed)
    // The other running timer stays visible with its own stop control.
    #expect(running.secondaryRows.count == 1)
    #expect(running.secondaryRows[0].tracker == .sleep)
    #expect(running.secondaryRows[0].isLive)
}

@MainActor
@Test func todayTotalsUseThePreferredVolumeUnit() throws {
    let (store, babyID, _) = try freshStore()
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: babyID)
    _ = try store.logBottle(volumeML: 90, contents: .formula, for: babyID)

    let model = HomeViewModel(store: store, babyID: babyID, volumeUnit: .millilitres)
    try model.refresh()

    guard case .idle(let state) = model.mode else {
        Issue.record("expected idle mode"); return
    }
    #expect(state.todayFeeds == "2")
    #expect(state.todayVolume == "210 ml")
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter HomeViewModelTests`
Expected: FAIL — `cannot find 'HomeViewModel' in scope`

- [ ] **Step 3: Implement the display types**

`Sources/HomeFeature/HomeViewModel.swift` (first half):

```swift
import Foundation
import Observation
import BabyCore
import BabyStore
import BabyUI

public struct StatusRow: Sendable, Equatable {
    public let title: String
    public let detail: String
    public let value: String
    public let tracker: Tracker
    public let isLive: Bool
}

public struct IdleState: Sendable, Equatable {
    public let feedRow: StatusRow
    public let sleepRow: StatusRow
    public let diaperRow: StatusRow
    public let todayFeeds: String
    public let todayVolume: String
    public let todaySleep: String
    public let todayDiapers: String
}

public struct RunningState: Sendable, Equatable {
    public let tracker: Tracker
    public let elapsedText: String
    public let secondsIntoMinute: Int
    public let sideLabel: String
    public let perSideText: String?
    public let startedAtText: String
    public let secondaryRows: [StatusRow]
}

public enum HomeMode: Sendable, Equatable {
    case idle(IdleState)
    case running(RunningState)
}
```

- [ ] **Step 4: Implement the view model**

`Sources/HomeFeature/HomeViewModel.swift` (second half):

```swift
@MainActor
@Observable
public final class HomeViewModel {
    private let store: BabyStore
    private let babyID: UUID
    private let volumeUnit: VolumeUnit

    public private(set) var mode: HomeMode = .idle(.empty)

    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres) {
        self.store = store
        self.babyID = babyID
        self.volumeUnit = volumeUnit
    }

    public func refresh() throws {
        let snapshot = try store.homeSnapshot(for: babyID)
        let now = store.now

        if let primary = snapshot.primaryTimer {
            mode = .running(runningState(primary, snapshot: snapshot, now: now))
        } else {
            mode = .idle(idleState(snapshot, now: now))
        }
    }

    // MARK: - Idle

    private func idleState(_ snapshot: HomeSnapshot, now: Date) -> IdleState {
        IdleState(
            feedRow: feedRow(snapshot.lastFeed, now: now),
            sleepRow: sleepRow(snapshot.lastSleep, now: now),
            diaperRow: diaperRow(snapshot.lastDiaper, now: now),
            todayFeeds: String(snapshot.todayFeedCount),
            todayVolume: Volume(millilitres: snapshot.todayVolumeML).formatted(as: volumeUnit),
            todaySleep: ElapsedFormatter.compact(snapshot.todaySleepSeconds),
            todayDiapers: String(snapshot.todayDiaperCount)
        )
    }

    private func age(_ date: Date, now: Date) -> String {
        ElapsedFormatter.compact(Int(now.timeIntervalSince(date)))
    }

    private func feedRow(_ feed: FeedEntry?, now: Date) -> StatusRow {
        guard let feed else { return .empty(.feed, title: "Last feed") }
        return StatusRow(
            title: "Last feed",
            detail: Self.describe(feed, unit: volumeUnit),
            value: age(feed.endedAt ?? feed.startedAt, now: now),
            tracker: .feed,
            isLive: false
        )
    }

    /// Stateful wording. "Last sleep 2h 7m ago" is ambiguous at 3am; "Awake 40m"
    /// with the previous sleep's length beneath is not.
    private func sleepRow(_ sleep: SleepEntry?, now: Date) -> StatusRow {
        guard let sleep, let ended = sleep.endedAt else {
            return .empty(.sleep, title: "Last sleep")
        }
        let slept = ElapsedFormatter.compact(sleep.durationSeconds ?? 0)
        return StatusRow(
            title: "Awake",
            detail: "Slept \(slept) before that",
            value: age(ended, now: now),
            tracker: .sleep,
            isLive: false
        )
    }

    private func diaperRow(_ diaper: DiaperEntry?, now: Date) -> StatusRow {
        guard let diaper else { return .empty(.diaper, title: "Last diaper") }
        return StatusRow(
            title: "Last diaper",
            detail: diaper.contents.rawValue.capitalized,
            value: age(diaper.startedAt, now: now),
            tracker: .diaper,
            isLive: false
        )
    }

    // MARK: - Running

    private func runningState(
        _ timer: ActiveTimer,
        snapshot: HomeSnapshot,
        now: Date
    ) -> RunningState {
        let elapsed = timer.elapsedSeconds(now: now)
        let tracker: Tracker = timer.kind == .feed ? .feed : .sleep

        var secondary: [StatusRow] = snapshot.runningTimers
            .filter { $0.id != timer.id }
            .map { other in
                StatusRow(
                    title: other.kind == .sleep ? "Asleep" : "Feeding",
                    detail: "Started \(Self.clockTime(other.startedAt, zone: other.timeZoneID))",
                    value: ElapsedFormatter.compact(other.elapsedSeconds(now: now)),
                    tracker: other.kind == .feed ? .feed : .sleep,
                    isLive: true
                )
            }
        if let diaper = snapshot.lastDiaper {
            secondary.append(diaperRow(diaper, now: now))
        }

        return RunningState(
            tracker: tracker,
            elapsedText: ElapsedFormatter.timer(elapsed),
            secondsIntoMinute: elapsed % 60,
            sideLabel: timer.currentSide.map { "Feeding · \($0.rawValue)" } ?? "Asleep",
            perSideText: timer.currentSide.map { _ in
                ElapsedFormatter.timer(timer.currentSideSeconds(now: now))
            },
            startedAtText: "Started \(Self.clockTime(timer.startedAt, zone: timer.timeZoneID))",
            secondaryRows: secondary
        )
    }

    // MARK: - Formatting helpers

    static func describe(_ feed: FeedEntry, unit: VolumeUnit) -> String {
        var parts: [String] = []
        if !feed.sides.isEmpty {
            let minutes = max(1, feed.totalBreastSeconds / 60)
            parts.append("Breast")
            if let side = feed.lastSideUsed { parts.append(side.rawValue) }
            parts.append("\(minutes) min")
        }
        if let ml = feed.volumeML {
            if parts.isEmpty { parts.append("Bottle") }
            parts.append(Volume(millilitres: ml).formatted(as: unit))
            if let contents = feed.contents {
                parts.append(contents == .breastMilk ? "breast milk" : contents.rawValue)
            }
        }
        if parts.isEmpty { parts.append("Solids") }
        return parts.joined(separator: " · ")
    }

    static func clockTime(_ date: Date, zone: String) -> String {
        var formatter = Date.FormatStyle(date: .omitted, time: .shortened)
        formatter.timeZone = TimeZone(identifier: zone) ?? .gmt
        return date.formatted(formatter)
    }
}

extension StatusRow {
    static func empty(_ tracker: Tracker, title: String) -> StatusRow {
        StatusRow(
            title: title,
            detail: "Nothing logged yet",
            value: "—",
            tracker: tracker,
            isLive: false
        )
    }
}

extension IdleState {
    static let empty = IdleState(
        feedRow: .empty(.feed, title: "Last feed"),
        sleepRow: .empty(.sleep, title: "Last sleep"),
        diaperRow: .empty(.diaper, title: "Last diaper"),
        todayFeeds: "0",
        todayVolume: "0 ml",
        todaySleep: "just now",
        todayDiapers: "0"
    )
}
```

- [ ] **Step 5: Run the test and confirm it passes**

Run: `swift test --filter HomeViewModelTests`
Expected: PASS, 8 tests

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add home view model with idle and running states"
```

---

### Task 14: Home views

**Files:**
- Create: `Sources/HomeFeature/HomeView.swift`
- Create: `Sources/HomeFeature/IdleStateView.swift`
- Create: `Sources/HomeFeature/RunningTimerView.swift`
- Create: `Sources/HomeFeature/TimerDial.swift`
- Create: `Sources/HomeFeature/QuickLogDock.swift`

**Interfaces:**
- Consumes: `HomeViewModel`, `HomeMode`, `Palette`, `Tracker`, `GlassDock`, `numeralStyle`
- Produces: `HomeView(store:babyID:onQuickLog:onOpenEntry:)` where `onQuickLog: (Tracker) -> Void`

Views hold no logic — every string comes from `HomeViewModel`. There is no unit test here; correctness is covered by Task 13's tests and Task 22's UI tests.

- [ ] **Step 1: Implement the dial**

`Sources/HomeFeature/TimerDial.swift`:

```swift
import SwiftUI
import BabyUI

/// Shown only while a timer runs. The ring is a seconds hand for the current
/// minute — it sweeps 0→60 and resets. It must never represent progress toward
/// a target, because this app makes no predictions.
struct TimerDial: View {
    let elapsedText: String
    let secondsIntoMinute: Int
    let eyebrow: String
    let caption: String
    let tint: Color

    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private var sweep: Double { Double(secondsIntoMinute) / 60 }

    var body: some View {
        ZStack {
            Circle()
                .stroke(Color.white.opacity(0.07), lineWidth: 9)

            Circle()
                .trim(from: 0, to: sweep)
                .stroke(tint, style: StrokeStyle(lineWidth: 9, lineCap: .round))
                .rotationEffect(.degrees(-90))
                .animation(reduceMotion ? nil : .linear(duration: 1), value: secondsIntoMinute)

            VStack(spacing: 6) {
                Text(eyebrow)
                    .font(.caption.weight(.bold))
                    .textCase(.uppercase)
                    .kerning(1.2)
                    .foregroundStyle(tint)

                Text(elapsedText)
                    .numeralStyle(.hero)

                Text(caption)
                    .font(.subheadline)
                    .foregroundStyle(Palette.secondaryText)
            }
            .padding(28)
        }
        .aspectRatio(1, contentMode: .fit)
        .frame(maxWidth: 260)
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(eyebrow), \(elapsedText) elapsed. \(caption)")
    }
}
```

- [ ] **Step 2: Implement the idle state**

`Sources/HomeFeature/IdleStateView.swift`:

```swift
import SwiftUI
import BabyUI

struct IdleStateView: View {
    let state: IdleState

    var body: some View {
        VStack(spacing: 24) {
            VStack(spacing: 11) {
                StatusRowView(row: state.feedRow)
                StatusRowView(row: state.sleepRow)
                StatusRowView(row: state.diaperRow)
            }

            VStack(alignment: .leading, spacing: 13) {
                Text("Today so far")
                    .font(.caption.weight(.bold))
                    .textCase(.uppercase)
                    .kerning(1.1)
                    .foregroundStyle(Palette.secondaryText)
                    .frame(maxWidth: .infinity, alignment: .leading)

                ViewThatFits(in: .horizontal) {
                    HStack(spacing: 10) { totals }
                    VStack(spacing: 10) { totals }
                }
            }
        }
    }

    @ViewBuilder
    private var totals: some View {
        TotalCell(value: state.todayFeeds, label: "feeds", tint: Palette.feed)
        TotalCell(value: state.todaySleep, label: "sleep", tint: Palette.sleep)
        TotalCell(value: state.todayDiapers, label: "diapers", tint: Palette.diaper)
    }
}

struct StatusRowView: View {
    let row: StatusRow

    var body: some View {
        HStack(spacing: 15) {
            Image(systemName: row.tracker.symbolName)
                .font(.title3)
                .foregroundStyle(row.tracker.color)
                .frame(width: 44, height: 44)
                .background(row.tracker.color.opacity(0.17), in: .rect(cornerRadius: 14))

            VStack(alignment: .leading, spacing: 3) {
                Text(row.title).font(.body.weight(.semibold))
                Text(row.detail)
                    .font(.subheadline)
                    .foregroundStyle(Palette.secondaryText)
            }

            Spacer(minLength: 8)

            VStack(alignment: .trailing, spacing: 4) {
                Text(row.value).numeralStyle(.display)
                if !row.isLive {
                    Text("ago")
                        .font(.caption2.weight(.semibold))
                        .textCase(.uppercase)
                        .foregroundStyle(Palette.secondaryText)
                }
            }
        }
        .padding(18)
        .background(.white.opacity(0.055), in: .rect(cornerRadius: 24))
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(row.title). \(row.value)\(row.isLive ? "" : " ago"). \(row.detail)")
    }
}

struct TotalCell: View {
    let value: String
    let label: String
    let tint: Color

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(value)
                .numeralStyle(.compact)
                .foregroundStyle(tint)
            Text(label)
                .font(.caption)
                .foregroundStyle(Palette.secondaryText)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(15)
        .background(.white.opacity(0.04), in: .rect(cornerRadius: 20))
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(value) \(label) today")
    }
}
```

- [ ] **Step 3: Implement the running state and the dock**

`Sources/HomeFeature/RunningTimerView.swift`:

```swift
import SwiftUI
import BabyUI

struct RunningTimerView: View {
    let state: RunningState
    let onSwitchSide: () -> Void
    let onStop: () -> Void

    var body: some View {
        VStack(spacing: 14) {
            TimerDial(
                elapsedText: state.elapsedText,
                secondsIntoMinute: state.secondsIntoMinute,
                eyebrow: state.sideLabel,
                caption: state.startedAtText,
                tint: state.tracker.color
            )
            .padding(.bottom, 10)

            if let perSide = state.perSideText {
                Button(action: onSwitchSide) {
                    VStack(spacing: 4) {
                        Text("Switch side").font(.body.weight(.semibold))
                        Text("This side \(perSide)")
                            .font(.caption)
                            .foregroundStyle(Palette.secondaryText)
                    }
                    .frame(maxWidth: .infinity, minHeight: 44)
                    .padding(.vertical, 11)
                }
                .buttonStyle(.bordered)
                .tint(state.tracker.color)
            }

            Button(action: onStop) {
                Text("Stop \(state.tracker.title.lowercased())")
                    .font(.headline)
                    .frame(maxWidth: .infinity, minHeight: 44)
                    .padding(.vertical, 8)
            }
            .buttonStyle(.borderedProminent)
            .tint(state.tracker.color)

            VStack(spacing: 9) {
                ForEach(state.secondaryRows, id: \.title) { row in
                    StatusRowView(row: row)
                }
            }
            .padding(.top, 6)
        }
    }
}
```

`Sources/HomeFeature/QuickLogDock.swift`:

```swift
import SwiftUI
import BabyUI

struct QuickLogDock: View {
    let onTap: (Tracker) -> Void

    var body: some View {
        GlassDock {
            HStack(spacing: 9) {
                ForEach(Tracker.allCases, id: \.self) { tracker in
                    Button { onTap(tracker) } label: {
                        VStack(spacing: 7) {
                            Image(systemName: tracker.symbolName).font(.title2)
                            Text(tracker.title).font(.footnote.weight(.semibold))
                        }
                        .frame(maxWidth: .infinity, minHeight: 60)
                        .padding(.vertical, 10)
                    }
                    .buttonStyle(.bordered)
                    .tint(tracker.color)
                    .accessibilityLabel("Log \(tracker.title.lowercased())")
                }
            }
        }
    }
}
```

- [ ] **Step 4: Implement HomeView**

`Sources/HomeFeature/HomeView.swift`:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public struct HomeView: View {
    @State private var model: HomeViewModel
    private let store: BabyStore
    private let babyID: UUID
    private let babyName: String
    private let onQuickLog: (Tracker) -> Void
    private let onSwitchBaby: () -> Void

    /// Drives the seconds ring and relative ages. One second is the coarsest
    /// tick that keeps the ring honest.
    private let tick = Timer.publish(every: 1, on: .main, in: .common).autoconnect()

    public init(
        store: BabyStore,
        babyID: UUID,
        babyName: String,
        volumeUnit: VolumeUnit = .millilitres,
        onQuickLog: @escaping (Tracker) -> Void,
        onSwitchBaby: @escaping () -> Void
    ) {
        self.store = store
        self.babyID = babyID
        self.babyName = babyName
        self.onQuickLog = onQuickLog
        self.onSwitchBaby = onSwitchBaby
        _model = State(wrappedValue: HomeViewModel(store: store, babyID: babyID, volumeUnit: volumeUnit))
    }

    public var body: some View {
        NavigationStack {
            ZStack(alignment: .bottom) {
                ScrollView {
                    Group {
                        switch model.mode {
                        case .idle(let state):
                            IdleStateView(state: state)
                        case .running(let state):
                            RunningTimerView(
                                state: state,
                                onSwitchSide: switchSide,
                                onStop: stopPrimary
                            )
                        }
                    }
                    .padding(.horizontal, 20)
                    .padding(.top, 6)
                    // Clears the dock, which floats outside the scroll view so
                    // its glass never collides with the tab bar's.
                    .padding(.bottom, 130)
                }

                if case .idle = model.mode {
                    QuickLogDock(onTap: onQuickLog)
                        .padding(.bottom, 12)
                }
            }
            .background(backgroundGradient)
            .navigationTitle(babyName)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button(action: onSwitchBaby) {
                        Image(systemName: "person.crop.circle")
                    }
                    .accessibilityLabel("Switch baby")
                }
            }
        }
        .task { try? model.refresh() }
        .onReceive(tick) { _ in try? model.refresh() }
    }

    private var backgroundGradient: some View {
        RadialGradient(
            colors: [Color(red: 0.106, green: 0.145, blue: 0.278), Palette.ground],
            center: .top,
            startRadius: 0,
            endRadius: 700
        )
        .ignoresSafeArea()
    }

    private func switchSide() {
        guard let timer = try? store.activeTimer(kind: .feed, for: babyID) else { return }
        try? store.switchSide(on: timer)
        try? model.refresh()
    }

    private func stopPrimary() {
        guard let timer = try? store.activeTimers(for: babyID).first else { return }
        switch timer.kind {
        case .feed: _ = try? store.stopFeed(timer)
        case .sleep: _ = try? store.stopSleep(timer)
        }
        try? model.refresh()
    }
}
```

- [ ] **Step 5: Confirm the package still builds**

Run: `swift build`
Expected: build succeeds with no warnings

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add home views for idle and running states"
```

---

### Task 15: Logging sheets

**Files:**
- Create: `Sources/LoggingFeature/LogCoordinator.swift`
- Create: `Sources/LoggingFeature/BottleSheet.swift`
- Create: `Sources/LoggingFeature/DiaperSheet.swift`
- Create: `Sources/LoggingFeature/FeedStartSheet.swift`
- Create: `Sources/LoggingFeature/LogDefaults.swift`
- Test: `Tests/LoggingFeatureTests/LogDefaultsTests.swift`
- Modify: `Package.swift` — add `.testTarget(name: "LoggingFeatureTests", dependencies: ["LoggingFeature"])`

**Interfaces:**
- Consumes: `BabyStore` mutations, `Tracker`
- Produces: `LogDefaults(store:)` with `mostUsedBottleVolumeML(for:) throws -> Double` and `lastBottleContents(for:) throws -> BottleContents`; `LogSheet` enum (`.bottle`, `.diaper`, `.feedStart`); `LogSheetView(sheet:store:babyID:onDone:)`

The two-tap requirement is met by pre-filling from history: the bottle stepper opens on the most-used volume, contents on the last used. Diaper saves immediately on contents selection with no confirmation screen.

- [ ] **Step 1: Write the failing test**

`Tests/LoggingFeatureTests/LogDefaultsTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
@testable import LoggingFeature

@MainActor
@Test func bottleDefaultsToTheMostFrequentVolume() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
    _ = try store.logBottle(volumeML: 90, contents: .breastMilk, for: baby.id)

    let defaults = LogDefaults(store: store)
    #expect(try defaults.mostUsedBottleVolumeML(for: baby.id) == 120)
    // Contents follows the most recent, not the most frequent — it changes
    // more often than volume does.
    #expect(try defaults.lastBottleContents(for: baby.id) == .breastMilk)
}

@MainActor
@Test func bottleDefaultsAreSensibleWithNoHistory() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    let defaults = LogDefaults(store: store)

    #expect(try defaults.mostUsedBottleVolumeML(for: baby.id) == 120)
    #expect(try defaults.lastBottleContents(for: baby.id) == .formula)
}

@MainActor
@Test func breastFeedsDoNotPolluteBottleDefaults() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let timer = try store.startFeed(side: .left, for: baby.id)
    clock.advance(by: 900)
    _ = try store.stopFeed(timer)

    let defaults = LogDefaults(store: store)
    #expect(try defaults.mostUsedBottleVolumeML(for: baby.id) == 120)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter LogDefaultsTests`
Expected: FAIL — `cannot find 'LogDefaults' in scope`

- [ ] **Step 3: Implement the defaults**

`Sources/LoggingFeature/LogDefaults.swift`:

```swift
import Foundation
import SwiftData
import BabyCore
import BabyStore

/// Pre-fills that make bottle logging two taps. Reads history rather than
/// asking the user to configure anything.
@MainActor
public struct LogDefaults {
    public static let fallbackVolumeML: Double = 120
    public static let fallbackContents: BottleContents = .formula

    private let store: BabyStore

    public init(store: BabyStore) {
        self.store = store
    }

    private func recentBottles(for babyID: UUID, limit: Int = 30) throws -> [FeedEntry] {
        let bottle = FeedKind.bottle.rawValue
        var descriptor = FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.babyID == babyID && !$0.isTombstoned && $0.kindRaw == bottle }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .reverse)]
        descriptor.fetchLimit = limit
        return try store.context.fetch(descriptor)
    }

    /// Volume is habitual, so the mode beats the most recent value.
    public func mostUsedBottleVolumeML(for babyID: UUID) throws -> Double {
        let volumes = try recentBottles(for: babyID).compactMap(\.volumeML)
        guard !volumes.isEmpty else { return Self.fallbackVolumeML }

        let counts = volumes.reduce(into: [Double: Int]()) { $0[$1, default: 0] += 1 }
        // Most frequent wins; ties break toward the larger volume for determinism.
        return counts.sorted { ($0.value, $0.key) > ($1.value, $1.key) }
            .first?.key ?? Self.fallbackVolumeML
    }

    /// Contents changes more often than volume, so the most recent wins.
    public func lastBottleContents(for babyID: UUID) throws -> BottleContents {
        try recentBottles(for: babyID, limit: 1).first?.contents ?? Self.fallbackContents
    }
}
```

- [ ] **Step 4: Implement the sheets**

`Sources/LoggingFeature/DiaperSheet.swift`:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

/// Two taps: open, choose. Saves immediately — no confirmation screen.
/// `.dry` is deliberately absent; it lives in the edit sheet.
public struct DiaperSheet: View {
    let store: BabyStore
    let babyID: UUID
    let onDone: () -> Void

    @State private var when: Date

    public init(store: BabyStore, babyID: UUID, onDone: @escaping () -> Void) {
        self.store = store
        self.babyID = babyID
        self.onDone = onDone
        _when = State(wrappedValue: store.now)
    }

    public var body: some View {
        NavigationStack {
            VStack(spacing: 14) {
                ForEach(DiaperContents.fastPath, id: \.self) { contents in
                    Button {
                        _ = try? store.logDiaper(contents, for: babyID, at: when)
                        onDone()
                    } label: {
                        Text(contents.rawValue.capitalized)
                            .font(.title3.weight(.semibold))
                            .frame(maxWidth: .infinity, minHeight: 64)
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(Palette.diaper)
                }

                DatePicker("Time", selection: $when, displayedComponents: [.hourAndMinute])
                    .padding(.top, 10)

                Spacer()
            }
            .padding(20)
            .navigationTitle("Diaper")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel", action: onDone)
                }
            }
        }
    }
}
```

`Sources/LoggingFeature/BottleSheet.swift`:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public struct BottleSheet: View {
    let store: BabyStore
    let babyID: UUID
    let unit: VolumeUnit
    let onDone: () -> Void

    @State private var volumeML: Double = LogDefaults.fallbackVolumeML
    @State private var contents: BottleContents = LogDefaults.fallbackContents
    @State private var when: Date

    public init(
        store: BabyStore,
        babyID: UUID,
        unit: VolumeUnit = .millilitres,
        onDone: @escaping () -> Void
    ) {
        self.store = store
        self.babyID = babyID
        self.unit = unit
        self.onDone = onDone
        _when = State(wrappedValue: store.now)
    }

    private var step: Double { unit == .millilitres ? 10 : 29.5735295625 / 2 }

    public var body: some View {
        NavigationStack {
            Form {
                Section {
                    Stepper(
                        value: $volumeML,
                        in: 0...400,
                        step: step
                    ) {
                        Text(Volume(millilitres: volumeML).formatted(as: unit))
                            .numeralStyle(.compact)
                    }
                    .accessibilityLabel("Volume")
                }

                Section {
                    Picker("Contents", selection: $contents) {
                        Text("Breast milk").tag(BottleContents.breastMilk)
                        Text("Formula").tag(BottleContents.formula)
                        Text("Mixed").tag(BottleContents.mixed)
                    }
                    .pickerStyle(.segmented)

                    DatePicker("Time", selection: $when, displayedComponents: [.hourAndMinute])
                }
            }
            .navigationTitle("Bottle")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel", action: onDone)
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") {
                        _ = try? store.logBottle(
                            volumeML: volumeML,
                            contents: contents,
                            for: babyID,
                            at: when
                        )
                        onDone()
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(Palette.feed)
                }
            }
            .task {
                let defaults = LogDefaults(store: store)
                volumeML = (try? defaults.mostUsedBottleVolumeML(for: babyID)) ?? volumeML
                contents = (try? defaults.lastBottleContents(for: babyID)) ?? contents
            }
        }
    }
}
```

`Sources/LoggingFeature/FeedStartSheet.swift`:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

/// Breast starts immediately on the suggested side — the sheet exists only to
/// offer the bottle and solids alternatives, so breast stays two taps.
public struct FeedStartSheet: View {
    let store: BabyStore
    let babyID: UUID
    let onStartedBreast: () -> Void
    let onChooseBottle: () -> Void
    let onDone: () -> Void

    @State private var side: BreastSide = .left

    public init(
        store: BabyStore,
        babyID: UUID,
        onStartedBreast: @escaping () -> Void,
        onChooseBottle: @escaping () -> Void,
        onDone: @escaping () -> Void
    ) {
        self.store = store
        self.babyID = babyID
        self.onStartedBreast = onStartedBreast
        self.onChooseBottle = onChooseBottle
        self.onDone = onDone
    }

    public var body: some View {
        NavigationStack {
            VStack(spacing: 14) {
                Text("Starts on \(side.rawValue) — tap the other side to change")
                    .font(.footnote)
                    .foregroundStyle(Palette.secondaryText)
                    .multilineTextAlignment(.center)

                HStack(spacing: 12) {
                    ForEach(BreastSide.allCases, id: \.self) { candidate in
                        Button {
                            _ = try? store.startFeed(side: candidate, for: babyID)
                            onStartedBreast()
                        } label: {
                            Text(candidate.rawValue.capitalized)
                                .font(.title3.weight(.semibold))
                                .frame(maxWidth: .infinity, minHeight: 72)
                        }
                        .buttonStyle(candidate == side ? .borderedProminent : .bordered)
                        .tint(Palette.feed)
                    }
                }

                Button("Bottle instead", action: onChooseBottle)
                    .buttonStyle(.bordered)
                    .frame(maxWidth: .infinity, minHeight: 44)

                Spacer()
            }
            .padding(20)
            .navigationTitle("Feed")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel", action: onDone)
                }
            }
            .task {
                side = (try? store.suggestedSide(for: babyID)) ?? .left
            }
        }
    }
}
```

`Sources/LoggingFeature/LogCoordinator.swift`:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public enum LogSheet: Identifiable {
    case feedStart
    case bottle
    case diaper

    public var id: String {
        switch self {
        case .feedStart: "feedStart"
        case .bottle: "bottle"
        case .diaper: "diaper"
        }
    }
}

public struct LogSheetView: View {
    let sheet: LogSheet
    let store: BabyStore
    let babyID: UUID
    let unit: VolumeUnit
    let onDone: () -> Void
    let onSwitchTo: (LogSheet) -> Void

    public init(
        sheet: LogSheet,
        store: BabyStore,
        babyID: UUID,
        unit: VolumeUnit = .millilitres,
        onDone: @escaping () -> Void,
        onSwitchTo: @escaping (LogSheet) -> Void
    ) {
        self.sheet = sheet
        self.store = store
        self.babyID = babyID
        self.unit = unit
        self.onDone = onDone
        self.onSwitchTo = onSwitchTo
    }

    public var body: some View {
        switch sheet {
        case .feedStart:
            FeedStartSheet(
                store: store,
                babyID: babyID,
                onStartedBreast: onDone,
                onChooseBottle: { onSwitchTo(.bottle) },
                onDone: onDone
            )
            .presentationDetents([.medium])
        case .bottle:
            BottleSheet(store: store, babyID: babyID, unit: unit, onDone: onDone)
                .presentationDetents([.medium])
        case .diaper:
            DiaperSheet(store: store, babyID: babyID, onDone: onDone)
                .presentationDetents([.medium])
        }
    }
}
```

- [ ] **Step 5: Run the tests and build**

Run: `swift test --filter LogDefaultsTests && swift build`
Expected: PASS, 3 tests; build succeeds

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add logging sheets with history-based prefills"
```

---

### Task 16: History

**Files:**
- Create: `Sources/HistoryFeature/HistoryViewModel.swift`
- Create: `Sources/HistoryFeature/HistoryView.swift`
- Test: `Tests/HistoryFeatureTests/HistoryViewModelTests.swift`
- Modify: `Package.swift` — add `.testTarget(name: "HistoryFeatureTests", dependencies: ["HistoryFeature"])`

**Interfaces:**
- Consumes: `store.entries(for:on:)`, `store.softDelete(_:)`, `DayBucket`
- Produces: `HistoryViewModel(store:babyID:)` with `.days: [HistorySection]`, `.filter: Set<Tracker>`, `.load(dayCount:)`, `.delete(_ item: HistoryItem)`; `HistorySection(day:title:rows:totals:)`; `HistoryRow(id:time:title:detail:tracker:)`

Day headers carry that day's totals, which is what makes History answer "did I already log that?" without arithmetic.

- [ ] **Step 1: Write the failing test**

`Tests/HistoryFeatureTests/HistoryViewModelTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
import BabyUI
@testable import HistoryFeature

@MainActor
private func makeStore(at start: TimeInterval) throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(Date(timeIntervalSince1970: start))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    return (store, baby.id, clock)
}

@MainActor
@Test func entriesGroupIntoDaysNewestFirst() throws {
    let (store, babyID, clock) = try makeStore(at: 1_700_000_000)
    _ = try store.logDiaper(.wet, for: babyID)
    clock.advance(by: 86_400 * 2)
    _ = try store.logDiaper(.dirty, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 7)

    #expect(model.days.count >= 2)
    #expect(model.days[0].day > model.days[1].day)
}

@MainActor
@Test func dayHeadersCarryThatDaysTotals() throws {
    let (store, babyID, clock) = try makeStore(at: 1_700_000_000)
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: babyID)
    clock.advance(by: 3_600)
    _ = try store.logBottle(volumeML: 90, contents: .formula, for: babyID)
    clock.advance(by: 3_600)
    _ = try store.logDiaper(.wet, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    #expect(model.days[0].totals == "2 feeds · 210 ml · 1 diaper")
}

@MainActor
@Test func filterHidesOtherKinds() throws {
    let (store, babyID, clock) = try makeStore(at: 1_700_000_000)
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: babyID)
    clock.advance(by: 600)
    _ = try store.logDiaper(.wet, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    model.filter = [.diaper]
    try model.load(dayCount: 1)

    #expect(model.days[0].rows.count == 1)
    #expect(model.days[0].rows[0].tracker == .diaper)
}

@MainActor
@Test func deletingRemovesTheRowButKeepsTheRecord() throws {
    let (store, babyID, _) = try makeStore(at: 1_700_000_000)
    _ = try store.logDiaper(.wet, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)
    let item = model.days[0].items[0]
    try model.delete(item)

    #expect(model.days.allSatisfy { $0.rows.isEmpty })
    #expect(try store.changeLog().last?.operation == .delete)
}

@MainActor
@Test func emptyDaysAreOmittedRatherThanShownBlank() throws {
    let (store, babyID, clock) = try makeStore(at: 1_700_000_000)
    _ = try store.logDiaper(.wet, for: babyID)
    clock.advance(by: 86_400 * 3)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 7)

    #expect(model.days.allSatisfy { !$0.rows.isEmpty })
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter HistoryViewModelTests`
Expected: FAIL — `cannot find 'HistoryViewModel' in scope`

- [ ] **Step 3: Implement the view model**

`Sources/HistoryFeature/HistoryViewModel.swift`:

```swift
import Foundation
import Observation
import BabyCore
import BabyStore
import BabyUI

public struct HistoryRow: Identifiable, Sendable {
    public let id: UUID
    public let time: String
    public let title: String
    public let detail: String
    public let tracker: Tracker
}

@MainActor
public struct HistorySection: Identifiable {
    public let day: DayKey
    public let title: String
    public let totals: String
    public let rows: [HistoryRow]
    public let items: [HistoryItem]

    public var id: DayKey { day }
}

@MainActor
@Observable
public final class HistoryViewModel {
    private let store: BabyStore
    private let babyID: UUID
    private let volumeUnit: VolumeUnit

    public var filter: Set<Tracker> = Set(Tracker.allCases)
    public private(set) var days: [HistorySection] = []

    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres) {
        self.store = store
        self.babyID = babyID
        self.volumeUnit = volumeUnit
    }

    public func load(dayCount: Int) throws {
        let zone = TimeZone.current.identifier
        let today = DayBucket.key(for: store.now, timeZoneID: zone)
        var sections: [HistorySection] = []

        for offset in 0..<dayCount {
            guard let date = Calendar.current.date(
                byAdding: .day, value: -offset, to: store.now
            ) else { continue }

            let day = DayBucket.key(for: date, timeZoneID: zone)
            let items = try store.entries(for: babyID, on: day)
                .filter { filter.contains(tracker(for: $0)) }

            guard !items.isEmpty else { continue }

            sections.append(
                HistorySection(
                    day: day,
                    title: Self.title(for: day, today: today, zone: zone),
                    totals: totals(for: try store.entries(for: babyID, on: day)),
                    rows: items.map(row(for:)),
                    items: items
                )
            )
        }

        days = sections
    }

    public func delete(_ item: HistoryItem) throws {
        switch item {
        case .feed(let entry): try store.softDelete(entry)
        case .sleep(let entry): try store.softDelete(entry)
        case .diaper(let entry): try store.softDelete(entry)
        }
        try load(dayCount: max(days.count, 1))
    }

    private func tracker(for item: HistoryItem) -> Tracker {
        switch item {
        case .feed: .feed
        case .sleep: .sleep
        case .diaper: .diaper
        }
    }

    private func row(for item: HistoryItem) -> HistoryRow {
        let time = Self.clockTime(item.startedAt, zone: item.timeZoneID)
        switch item {
        case .feed(let entry):
            return HistoryRow(
                id: entry.id,
                time: time,
                title: entry.isCombo ? "Combo feed" : entry.kind.rawValue.capitalized,
                detail: describe(entry),
                tracker: .feed
            )
        case .sleep(let entry):
            let length = entry.durationSeconds.map(ElapsedFormatter.compact) ?? "in progress"
            return HistoryRow(
                id: entry.id,
                time: time,
                title: entry.kind == .night ? "Night sleep" : "Nap",
                detail: length,
                tracker: .sleep
            )
        case .diaper(let entry):
            return HistoryRow(
                id: entry.id,
                time: time,
                title: "Diaper",
                detail: entry.contents.rawValue.capitalized,
                tracker: .diaper
            )
        }
    }

    private func describe(_ entry: FeedEntry) -> String {
        var parts: [String] = []
        if !entry.sides.isEmpty {
            parts.append("\(max(1, entry.totalBreastSeconds / 60)) min")
            if let side = entry.lastSideUsed { parts.append(side.rawValue) }
        }
        if let ml = entry.volumeML {
            parts.append(Volume(millilitres: ml).formatted(as: volumeUnit))
        }
        return parts.isEmpty ? "—" : parts.joined(separator: " · ")
    }

    private func totals(for items: [HistoryItem]) -> String {
        var feeds = 0, diapers = 0, volume = 0.0, sleepSeconds = 0
        for item in items {
            switch item {
            case .feed(let entry):
                feeds += 1
                volume += entry.volumeML ?? 0
            case .diaper:
                diapers += 1
            case .sleep(let entry):
                sleepSeconds += entry.durationSeconds ?? 0
            }
        }

        var parts: [String] = []
        if feeds > 0 { parts.append("\(feeds) feed\(feeds == 1 ? "" : "s")") }
        if volume > 0 { parts.append(Volume(millilitres: volume).formatted(as: volumeUnit)) }
        if sleepSeconds > 0 { parts.append(ElapsedFormatter.compact(sleepSeconds) + " sleep") }
        if diapers > 0 { parts.append("\(diapers) diaper\(diapers == 1 ? "" : "s")") }
        return parts.joined(separator: " · ")
    }

    static func title(for day: DayKey, today: DayKey, zone: String) -> String {
        if day == today { return "Today" }
        let bounds = DayBucket.bounds(of: day, timeZoneID: zone)
        var style = Date.FormatStyle(date: .abbreviated, time: .omitted)
        style.timeZone = TimeZone(identifier: zone) ?? .gmt
        return bounds.start.formatted(style)
    }

    static func clockTime(_ date: Date, zone: String) -> String {
        var style = Date.FormatStyle(date: .omitted, time: .shortened)
        style.timeZone = TimeZone(identifier: zone) ?? .gmt
        return date.formatted(style)
    }
}
```

- [ ] **Step 4: Implement the view**

`Sources/HistoryFeature/HistoryView.swift`:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public struct HistoryView: View {
    @State private var model: HistoryViewModel

    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres) {
        _model = State(wrappedValue: HistoryViewModel(
            store: store, babyID: babyID, volumeUnit: volumeUnit
        ))
    }

    public var body: some View {
        NavigationStack {
            List {
                ForEach(model.days) { section in
                    Section {
                        ForEach(Array(section.rows.enumerated()), id: \.element.id) { index, row in
                            HistoryRowView(row: row)
                                .swipeActions(edge: .trailing) {
                                    Button("Delete", role: .destructive) {
                                        try? model.delete(section.items[index])
                                    }
                                }
                        }
                    } header: {
                        VStack(alignment: .leading, spacing: 3) {
                            Text(section.title).font(.headline)
                            Text(section.totals)
                                .font(.caption)
                                .foregroundStyle(Palette.secondaryText)
                        }
                        .textCase(nil)
                    }
                }
            }
            .listStyle(.insetGrouped)
            .navigationTitle("History")
            .safeAreaInset(edge: .top) {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 8) {
                        ForEach(Tracker.allCases, id: \.self) { tracker in
                            let isOn = model.filter.contains(tracker)
                            Button {
                                if isOn { model.filter.remove(tracker) }
                                else { model.filter.insert(tracker) }
                                try? model.load(dayCount: 30)
                            } label: {
                                Label(tracker.title, systemImage: tracker.symbolName)
                                    .font(.subheadline.weight(.semibold))
                                    .padding(.horizontal, 6)
                                    .frame(minHeight: 44)
                            }
                            .buttonStyle(isOn ? .borderedProminent : .bordered)
                            .tint(tracker.color)
                            .accessibilityAddTraits(isOn ? [.isSelected] : [])
                        }
                    }
                    .padding(.horizontal, 16)
                }
                .background(.bar)
            }
            .overlay {
                if model.days.isEmpty {
                    ContentUnavailableView(
                        "Nothing logged yet",
                        systemImage: "list.bullet",
                        description: Text("Feeds, sleeps, and diaper changes will appear here.")
                    )
                }
            }
        }
        .task { try? model.load(dayCount: 30) }
    }
}

struct HistoryRowView: View {
    let row: HistoryRow

    var body: some View {
        HStack(spacing: 13) {
            Image(systemName: row.tracker.symbolName)
                .foregroundStyle(row.tracker.color)
                .frame(width: 30)

            VStack(alignment: .leading, spacing: 2) {
                Text(row.title).font(.body.weight(.medium))
                Text(row.detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }

            Spacer()

            Text(row.time)
                .font(.subheadline)
                .monospacedDigit()
                .foregroundStyle(.secondary)
        }
        .frame(minHeight: 44)
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(row.title), \(row.detail), at \(row.time)")
    }
}
```

- [ ] **Step 5: Run the tests and build**

Run: `swift test --filter HistoryViewModelTests && swift build`
Expected: PASS, 5 tests; build succeeds

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add history with day totals and filtering"
```

---

### Task 17: Insights

**Files:**
- Create: `Sources/InsightsFeature/InsightsViewModel.swift`
- Create: `Sources/InsightsFeature/InsightsView.swift`
- Test: `Tests/InsightsFeatureTests/InsightsViewModelTests.swift`
- Modify: `Package.swift` — add `.testTarget(name: "InsightsFeatureTests", dependencies: ["InsightsFeature"])`

**Interfaces:**
- Consumes: `store.entries(for:on:)`, `DayBucket`
- Produces: `InsightsViewModel(store:babyID:)` with `.figures: [InsightFigure]` and `.load()`; `InsightFigure(title:todayValue:averageValue:comparison:tracker:)`

Four figures, today versus the trailing 7-day average. No curves, no percentiles, no predictions. The comparison string is factual ("2 more than usual"), never evaluative — nothing here tells a parent they are doing badly.

- [ ] **Step 1: Write the failing test**

`Tests/InsightsFeatureTests/InsightsViewModelTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
@testable import InsightsFeature

@MainActor
@Test func figuresCompareTodayAgainstTheTrailingAverage() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    // Seven days back: two feeds each day.
    for dayOffset in 1...7 {
        let when = clock.now.addingTimeInterval(-86_400 * Double(dayOffset))
        _ = try store.logBottle(volumeML: 100, contents: .formula, for: baby.id, at: when)
        _ = try store.logBottle(volumeML: 100, contents: .formula, for: baby.id, at: when)
    }
    // Today: four feeds.
    for _ in 0..<4 {
        _ = try store.logBottle(volumeML: 100, contents: .formula, for: baby.id)
    }

    let model = InsightsViewModel(store: store, babyID: baby.id)
    try model.load()

    let feeds = model.figures.first { $0.title == "Feeds" }!
    #expect(feeds.todayValue == "4")
    #expect(feeds.averageValue == "2")
    #expect(feeds.comparison == "2 more than usual")
}

@MainActor
@Test func comparisonIsFactualNotEvaluative() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    for dayOffset in 1...7 {
        let when = clock.now.addingTimeInterval(-86_400 * Double(dayOffset))
        _ = try store.logDiaper(.wet, for: baby.id, at: when)
        _ = try store.logDiaper(.wet, for: baby.id, at: when)
        _ = try store.logDiaper(.wet, for: baby.id, at: when)
    }
    _ = try store.logDiaper(.wet, for: baby.id)

    let model = InsightsViewModel(store: store, babyID: baby.id)
    try model.load()

    let diapers = model.figures.first { $0.title == "Diapers" }!
    #expect(diapers.comparison == "2 fewer than usual")
    // No language that could be read as a judgement or a warning.
    for figure in model.figures {
        #expect(!figure.comparison.lowercased().contains("low"))
        #expect(!figure.comparison.lowercased().contains("should"))
    }
}

@MainActor
@Test func noHistoryMeansNoComparison() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    _ = try store.logDiaper(.wet, for: baby.id)

    let model = InsightsViewModel(store: store, babyID: baby.id)
    try model.load()

    #expect(model.figures.allSatisfy { $0.comparison == "Not enough history yet" })
}

@MainActor
@Test func thereAreExactlyFourFigures() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let model = InsightsViewModel(store: store, babyID: baby.id)
    try model.load()

    #expect(model.figures.map(\.title) == ["Feeds", "Volume", "Sleep", "Diapers"])
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter InsightsViewModelTests`
Expected: FAIL — `cannot find 'InsightsViewModel' in scope`

- [ ] **Step 3: Implement**

`Sources/InsightsFeature/InsightsViewModel.swift`:

```swift
import Foundation
import Observation
import BabyCore
import BabyStore
import BabyUI

public struct InsightFigure: Identifiable, Sendable {
    public let title: String
    public let todayValue: String
    public let averageValue: String
    public let comparison: String
    public let tracker: Tracker

    public var id: String { title }
}

@MainActor
@Observable
public final class InsightsViewModel {
    private let store: BabyStore
    private let babyID: UUID
    private let volumeUnit: VolumeUnit

    public private(set) var figures: [InsightFigure] = []

    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres) {
        self.store = store
        self.babyID = babyID
        self.volumeUnit = volumeUnit
    }

    private struct DayTotals {
        var feeds = 0
        var volumeML = 0.0
        var sleepSeconds = 0
        var diapers = 0
    }

    public func load() throws {
        let zone = TimeZone.current.identifier
        let today = try totals(on: DayBucket.key(for: store.now, timeZoneID: zone))

        var priorDays: [DayTotals] = []
        for offset in 1...7 {
            guard let date = Calendar.current.date(
                byAdding: .day, value: -offset, to: store.now
            ) else { continue }
            priorDays.append(try totals(on: DayBucket.key(for: date, timeZoneID: zone)))
        }

        let hasHistory = priorDays.contains {
            $0.feeds > 0 || $0.diapers > 0 || $0.sleepSeconds > 0
        }
        let divisor = Double(max(priorDays.count, 1))

        let averageFeeds = Double(priorDays.map(\.feeds).reduce(0, +)) / divisor
        let averageVolume = priorDays.map(\.volumeML).reduce(0, +) / divisor
        let averageSleep = Double(priorDays.map(\.sleepSeconds).reduce(0, +)) / divisor
        let averageDiapers = Double(priorDays.map(\.diapers).reduce(0, +)) / divisor

        figures = [
            InsightFigure(
                title: "Feeds",
                todayValue: String(today.feeds),
                averageValue: String(Int(averageFeeds.rounded())),
                comparison: countComparison(Double(today.feeds), averageFeeds, hasHistory: hasHistory),
                tracker: .feed
            ),
            InsightFigure(
                title: "Volume",
                todayValue: Volume(millilitres: today.volumeML).formatted(as: volumeUnit),
                averageValue: Volume(millilitres: averageVolume).formatted(as: volumeUnit),
                comparison: volumeComparison(today.volumeML, averageVolume, hasHistory: hasHistory),
                tracker: .feed
            ),
            InsightFigure(
                title: "Sleep",
                todayValue: ElapsedFormatter.compact(today.sleepSeconds),
                averageValue: ElapsedFormatter.compact(Int(averageSleep)),
                comparison: durationComparison(
                    today.sleepSeconds, Int(averageSleep), hasHistory: hasHistory
                ),
                tracker: .sleep
            ),
            InsightFigure(
                title: "Diapers",
                todayValue: String(today.diapers),
                averageValue: String(Int(averageDiapers.rounded())),
                comparison: countComparison(
                    Double(today.diapers), averageDiapers, hasHistory: hasHistory
                ),
                tracker: .diaper
            ),
        ]
    }

    private func totals(on day: DayKey) throws -> DayTotals {
        var result = DayTotals()
        for item in try store.entries(for: babyID, on: day) {
            switch item {
            case .feed(let entry):
                result.feeds += 1
                result.volumeML += entry.volumeML ?? 0
            case .sleep(let entry):
                result.sleepSeconds += entry.durationSeconds ?? 0
            case .diaper:
                result.diapers += 1
            }
        }
        return result
    }

    /// Factual, never evaluative. This app does not tell a parent how they are doing.
    private func countComparison(_ today: Double, _ average: Double, hasHistory: Bool) -> String {
        guard hasHistory else { return "Not enough history yet" }
        let delta = Int((today - average).rounded())
        if delta == 0 { return "Same as usual" }
        return delta > 0 ? "\(delta) more than usual" : "\(-delta) fewer than usual"
    }

    private func volumeComparison(_ today: Double, _ average: Double, hasHistory: Bool) -> String {
        guard hasHistory else { return "Not enough history yet" }
        let delta = today - average
        if abs(delta) < 1 { return "Same as usual" }
        let amount = Volume(millilitres: abs(delta)).formatted(as: volumeUnit)
        return delta > 0 ? "\(amount) more than usual" : "\(amount) less than usual"
    }

    private func durationComparison(_ today: Int, _ average: Int, hasHistory: Bool) -> String {
        guard hasHistory else { return "Not enough history yet" }
        let delta = today - average
        if abs(delta) < 60 { return "Same as usual" }
        let amount = ElapsedFormatter.compact(abs(delta))
        return delta > 0 ? "\(amount) more than usual" : "\(amount) less than usual"
    }
}
```

`Sources/InsightsFeature/InsightsView.swift`:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public struct InsightsView: View {
    @State private var model: InsightsViewModel

    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres) {
        _model = State(wrappedValue: InsightsViewModel(
            store: store, babyID: babyID, volumeUnit: volumeUnit
        ))
    }

    public var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 13) {
                    ForEach(model.figures) { figure in
                        VStack(alignment: .leading, spacing: 7) {
                            Text(figure.title)
                                .font(.caption.weight(.bold))
                                .textCase(.uppercase)
                                .kerning(1.1)
                                .foregroundStyle(figure.tracker.color)

                            Text(figure.todayValue).numeralStyle(.display)

                            Text("\(figure.comparison) · 7-day average \(figure.averageValue)")
                                .font(.footnote)
                                .foregroundStyle(Palette.secondaryText)
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(19)
                        .background(.white.opacity(0.055), in: .rect(cornerRadius: 24))
                        .accessibilityElement(children: .combine)
                        .accessibilityLabel(
                            "\(figure.title) today \(figure.todayValue). \(figure.comparison)."
                        )
                    }
                }
                .padding(20)
            }
            .background(Palette.ground.ignoresSafeArea())
            .navigationTitle("Insights")
        }
        .task { try? model.load() }
    }
}
```

- [ ] **Step 4: Run the tests and build**

Run: `swift test --filter InsightsViewModelTests && swift build`
Expected: PASS, 4 tests; build succeeds

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add insights comparing today to the trailing average"
```

---

### Task 18: Baby management

**Files:**
- Create: `Sources/BabyFeature/BabyListViewModel.swift`
- Create: `Sources/BabyFeature/AddBabySheet.swift`
- Create: `Sources/BabyFeature/BabySwitcherSheet.swift`
- Create: `Sources/BabyFeature/SelectedBabyStore.swift`
- Test: `Tests/BabyFeatureTests/SelectedBabyStoreTests.swift`
- Modify: `Package.swift` — add `.testTarget(name: "BabyFeatureTests", dependencies: ["BabyFeature"])`

**Interfaces:**
- Consumes: `store.addBaby(name:birthDate:)`, `store.babies()`
- Produces: `SelectedBabyStore(defaults:)` with `.selectedID: UUID?`, `.select(_ id: UUID)`, `.resolve(from babies: [Baby]) -> Baby?`; `AddBabySheet(store:onAdded:)`; `BabySwitcherSheet(store:selection:onDone:)`

Selection persists in the App Group's `UserDefaults` so widget taps log against the same baby the app is showing.

- [ ] **Step 1: Write the failing test**

`Tests/BabyFeatureTests/SelectedBabyStoreTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
@testable import BabyFeature

private func emptyDefaults() -> UserDefaults {
    let name = "test-\(UUID().uuidString)"
    let defaults = UserDefaults(suiteName: name)!
    defaults.removePersistentDomain(forName: name)
    return defaults
}

@MainActor
@Test func selectionPersistsAcrossInstances() throws {
    let defaults = emptyDefaults()
    let id = UUID()

    SelectedBabyStore(defaults: defaults).select(id)

    #expect(SelectedBabyStore(defaults: defaults).selectedID == id)
}

@MainActor
@Test func resolveFallsBackToTheFirstBabyWhenSelectionIsStale() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let mira = try store.addBaby(name: "Mira", birthDate: .now)
    let babies = try store.babies()

    let selection = SelectedBabyStore(defaults: emptyDefaults())
    selection.select(UUID())   // points at a baby that no longer exists

    #expect(selection.resolve(from: babies)?.id == mira.id)
}

@MainActor
@Test func resolveReturnsNilWhenThereAreNoBabies() {
    let selection = SelectedBabyStore(defaults: emptyDefaults())
    #expect(selection.resolve(from: []) == nil)
}

@MainActor
@Test func resolveHonoursAValidSelection() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    _ = try store.addBaby(name: "Mira", birthDate: .now)
    let theo = try store.addBaby(name: "Theo", birthDate: .now)
    let babies = try store.babies()

    let selection = SelectedBabyStore(defaults: emptyDefaults())
    selection.select(theo.id)

    #expect(selection.resolve(from: babies)?.name == "Theo")
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter SelectedBabyStoreTests`
Expected: FAIL — `cannot find 'SelectedBabyStore' in scope`

- [ ] **Step 3: Implement selection**

`Sources/BabyFeature/SelectedBabyStore.swift`:

```swift
import Foundation
import Observation
import BabyStore

/// Lives in the App Group so a widget tap logs against the baby the app shows.
@MainActor
@Observable
public final class SelectedBabyStore {
    private static let key = "selectedBabyID"
    private let defaults: UserDefaults

    public init(defaults: UserDefaults? = nil) {
        self.defaults = defaults
            ?? UserDefaults(suiteName: BabySchema.appGroupID)
            ?? .standard
    }

    public var selectedID: UUID? {
        defaults.string(forKey: Self.key).flatMap(UUID.init(uuidString:))
    }

    public func select(_ id: UUID) {
        defaults.set(id.uuidString, forKey: Self.key)
    }

    /// A stale selection falls back to the first baby rather than showing an
    /// empty screen — a deleted baby must never strand the app.
    public func resolve(from babies: [Baby]) -> Baby? {
        if let id = selectedID, let match = babies.first(where: { $0.id == id }) {
            return match
        }
        return babies.first
    }
}
```

- [ ] **Step 4: Implement the sheets**

`Sources/BabyFeature/AddBabySheet.swift`:

```swift
import SwiftUI
import BabyStore
import BabyUI

public struct AddBabySheet: View {
    let store: BabyStore
    let onAdded: (Baby) -> Void
    let onCancel: (() -> Void)?

    @State private var name = ""
    @State private var birthDate = Date()

    public init(store: BabyStore, onAdded: @escaping (Baby) -> Void, onCancel: (() -> Void)? = nil) {
        self.store = store
        self.onAdded = onAdded
        self.onCancel = onCancel
    }

    private var canSave: Bool {
        !name.trimmingCharacters(in: .whitespaces).isEmpty
    }

    public var body: some View {
        NavigationStack {
            Form {
                TextField("Name", text: $name)
                    .textInputAutocapitalization(.words)
                DatePicker("Born", selection: $birthDate, displayedComponents: [.date])
            }
            .navigationTitle("Add baby")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                if let onCancel {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Cancel", action: onCancel)
                    }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") {
                        guard let baby = try? store.addBaby(
                            name: name.trimmingCharacters(in: .whitespaces),
                            birthDate: birthDate
                        ) else { return }
                        onAdded(baby)
                    }
                    .disabled(!canSave)
                }
            }
        }
    }
}
```

`Sources/BabyFeature/BabySwitcherSheet.swift`:

```swift
import SwiftUI
import BabyStore
import BabyUI

public struct BabySwitcherSheet: View {
    let store: BabyStore
    let selection: SelectedBabyStore
    let onDone: () -> Void

    @State private var babies: [Baby] = []
    @State private var isAdding = false

    public init(store: BabyStore, selection: SelectedBabyStore, onDone: @escaping () -> Void) {
        self.store = store
        self.selection = selection
        self.onDone = onDone
    }

    public var body: some View {
        NavigationStack {
            List {
                ForEach(babies, id: \.id) { baby in
                    Button {
                        selection.select(baby.id)
                        onDone()
                    } label: {
                        HStack {
                            Text(baby.name)
                            Spacer()
                            if baby.id == selection.selectedID {
                                Image(systemName: "checkmark")
                                    .foregroundStyle(Palette.feed)
                            }
                        }
                        .frame(minHeight: 44)
                    }
                }

                Button("Add baby") { isAdding = true }
                    .frame(minHeight: 44)
            }
            .navigationTitle("Babies")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done", action: onDone)
                }
            }
            .sheet(isPresented: $isAdding) {
                AddBabySheet(
                    store: store,
                    onAdded: { baby in
                        selection.select(baby.id)
                        isAdding = false
                        onDone()
                    },
                    onCancel: { isAdding = false }
                )
            }
        }
        .task { babies = (try? store.babies()) ?? [] }
    }
}
```

- [ ] **Step 5: Run the tests and build**

Run: `swift test --filter SelectedBabyStoreTests && swift build`
Expected: PASS, 4 tests; build succeeds

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add baby creation, switching, and persisted selection"
```

---

### Task 19: Xcode project and app wiring

**Files:**
- Create: `BabyTracker.xcodeproj` (via Xcode)
- Create: `Apps/BabyTrackerApp/BabyTrackerApp.swift`
- Create: `Apps/BabyTrackerApp/RootView.swift`
- Create: `Apps/BabyTrackerApp/AppEnvironment.swift`

**Interfaces:**
- Consumes: every feature module
- Produces: a launchable app; `AppEnvironment` holding `store`, `selection`, and `volumeUnit`

This is the first task that needs a simulator. Everything before it ran under `swift test`.

- [ ] **Step 1: Create the project in Xcode**

Create a new iOS App project named `BabyTracker`, interface SwiftUI, at the repository root. Then:

1. Delete the generated `ContentView.swift`.
2. `File > Add Package Dependencies… > Add Local…` and select `Packages/BabyTracker`.
3. In the app target's **General > Frameworks, Libraries, and Embedded Content**, add `BabyCore`, `BabyStore`, `BabySync`, `BabyUI`, `HomeFeature`, `LoggingFeature`, `HistoryFeature`, `InsightsFeature`, `BabyFeature`.
4. **Signing & Capabilities >  + Capability > App Groups**, add `group.com.example.babytracker`. The identifier must match `BabySchema.appGroupID` exactly or the widget reads an empty database.
5. Set the deployment target to iOS 26.0.
6. **Build Settings > Swift Language Version** to 6, and **Strict Concurrency Checking** to Complete.

- [ ] **Step 2: Implement the app environment**

`Apps/BabyTrackerApp/AppEnvironment.swift`:

```swift
import Foundation
import Observation
import BabyCore
import BabyStore
import BabyFeature

@MainActor
@Observable
final class AppEnvironment {
    let store: BabyStore
    let selection: SelectedBabyStore
    var volumeUnit: VolumeUnit = .millilitres

    init() {
        let container = try! BabySchema.container(inMemory: false)
        self.store = BabyStore(
            container: container,
            dateProvider: SystemDateProvider(),
            deviceID: Self.deviceID
        )
        self.selection = SelectedBabyStore()
    }

    /// Stable per install. Identifies the origin of change-log entries so the
    /// future sync node can resolve concurrent edits.
    private static var deviceID: String {
        let defaults = UserDefaults(suiteName: BabySchema.appGroupID) ?? .standard
        if let existing = defaults.string(forKey: "deviceID") { return existing }
        let fresh = UUID().uuidString
        defaults.set(fresh, forKey: "deviceID")
        return fresh
    }
}
```

- [ ] **Step 3: Implement the root view**

`Apps/BabyTrackerApp/RootView.swift`:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI
import BabyFeature
import HomeFeature
import HistoryFeature
import InsightsFeature
import LoggingFeature

struct RootView: View {
    @State private var environment = AppEnvironment()
    @State private var babies: [Baby] = []
    @State private var sheet: LogSheet?
    @State private var isSwitchingBaby = false

    var body: some View {
        Group {
            if let baby = environment.selection.resolve(from: babies) {
                tabs(for: baby)
            } else {
                AddBabySheet(store: environment.store) { baby in
                    environment.selection.select(baby.id)
                    reload()
                }
            }
        }
        .preferredColorScheme(.dark)
        .task { reload() }
    }

    private func tabs(for baby: Baby) -> some View {
        TabView {
            Tab("Home", systemImage: "house.fill") {
                HomeView(
                    store: environment.store,
                    babyID: baby.id,
                    babyName: baby.name,
                    volumeUnit: environment.volumeUnit,
                    onQuickLog: quickLog,
                    onSwitchBaby: { isSwitchingBaby = true }
                )
            }
            Tab("History", systemImage: "list.bullet") {
                HistoryView(
                    store: environment.store,
                    babyID: baby.id,
                    volumeUnit: environment.volumeUnit
                )
            }
            Tab("Insights", systemImage: "chart.bar.fill") {
                InsightsView(
                    store: environment.store,
                    babyID: baby.id,
                    volumeUnit: environment.volumeUnit
                )
            }
        }
        .sheet(item: $sheet) { current in
            LogSheetView(
                sheet: current,
                store: environment.store,
                babyID: baby.id,
                unit: environment.volumeUnit,
                onDone: { sheet = nil },
                onSwitchTo: { sheet = $0 }
            )
        }
        .sheet(isPresented: $isSwitchingBaby) {
            BabySwitcherSheet(
                store: environment.store,
                selection: environment.selection,
                onDone: {
                    isSwitchingBaby = false
                    reload()
                }
            )
        }
    }

    /// Sleep toggles directly — there is nothing to ask. Feed and diaper open
    /// their sheets.
    private func quickLog(_ tracker: Tracker) {
        guard let baby = environment.selection.resolve(from: babies) else { return }
        switch tracker {
        case .feed:
            sheet = .feedStart
        case .diaper:
            sheet = .diaper
        case .sleep:
            if let running = try? environment.store.activeTimer(kind: .sleep, for: baby.id) {
                _ = try? environment.store.stopSleep(running)
            } else {
                _ = try? environment.store.startSleep(for: baby.id)
            }
        }
    }

    private func reload() {
        babies = (try? environment.store.babies()) ?? []
    }
}
```

`Apps/BabyTrackerApp/BabyTrackerApp.swift`:

```swift
import SwiftUI

@main
struct BabyTrackerApp: App {
    var body: some Scene {
        WindowGroup {
            RootView()
        }
    }
}
```

- [ ] **Step 4: Build and run**

Run: `xcrun simctl list devices available | grep iPhone` to pick a device, then
`xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=<DEVICE>' build`
Expected: BUILD SUCCEEDED. Launch in the simulator, add a baby, log a diaper, confirm it appears in History.

- [ ] **Step 5: Commit**

```bash
git add .
git commit -m "feat: add app target wiring features into three tabs"
```

---

### Task 20: Widgets and App Intents

**Files:**
- Create: `Apps/BabyTrackerWidgets/BabyTrackerWidgets.swift`
- Create: `Apps/BabyTrackerWidgets/LogIntents.swift`
- Create: `Apps/BabyTrackerWidgets/StatusWidget.swift`
- Create: `Apps/BabyTrackerWidgets/QuickLogWidget.swift`
- Create: `Apps/BabyTrackerWidgets/WidgetStore.swift`

**Interfaces:**
- Consumes: `BabySchema.container(inMemory: false)`, `BabyStore` mutations, `SelectedBabyStore`
- Produces: `LogDiaperIntent`, `StartFeedIntent`, `ToggleSleepIntent` App Intents; `StatusWidget`; `QuickLogWidget`

Widget buttons call the same `BabyStore` methods the app does. There is exactly one code path for logging a diaper.

- [ ] **Step 1: Add the widget extension target**

In Xcode: `File > New > Target > Widget Extension`, named `BabyTrackerWidgets`, **with** "Include Live Activity" checked (Task 21 uses it). Add the same App Group capability, then link `BabyCore`, `BabyStore`, `BabyUI`, and `BabyFeature`.

- [ ] **Step 2: Implement the shared widget store accessor**

`Apps/BabyTrackerWidgets/WidgetStore.swift`:

```swift
import Foundation
import BabyCore
import BabyStore
import BabyFeature

@MainActor
enum WidgetStore {
    /// One container per process. The widget extension is short-lived, so this
    /// is created on demand rather than held by a long-lived owner.
    static func make() throws -> BabyStore {
        let defaults = UserDefaults(suiteName: BabySchema.appGroupID) ?? .standard
        return BabyStore(
            container: try BabySchema.container(inMemory: false),
            dateProvider: SystemDateProvider(),
            deviceID: defaults.string(forKey: "deviceID") ?? "widget"
        )
    }

    static func currentBaby(in store: BabyStore) throws -> Baby? {
        SelectedBabyStore().resolve(from: try store.babies())
    }
}
```

- [ ] **Step 3: Implement the intents**

`Apps/BabyTrackerWidgets/LogIntents.swift`:

```swift
import AppIntents
import WidgetKit
import BabyCore
import BabyStore

struct LogDiaperIntent: AppIntent {
    static var title: LocalizedStringResource = "Log a diaper"
    static var description = IntentDescription("Records a wet diaper for the selected baby.")

    @MainActor
    func perform() async throws -> some IntentResult {
        let store = try WidgetStore.make()
        guard let baby = try WidgetStore.currentBaby(in: store) else { return .result() }

        _ = try store.logDiaper(.wet, for: baby.id)
        WidgetCenter.shared.reloadAllTimelines()
        return .result()
    }
}

struct StartFeedIntent: AppIntent {
    static var title: LocalizedStringResource = "Start a feed"
    static var description = IntentDescription("Starts a breast feed on the suggested side.")

    @MainActor
    func perform() async throws -> some IntentResult {
        let store = try WidgetStore.make()
        guard let baby = try WidgetStore.currentBaby(in: store) else { return .result() }

        _ = try store.startFeed(side: nil, for: baby.id)
        WidgetCenter.shared.reloadAllTimelines()
        return .result()
    }
}

struct ToggleSleepIntent: AppIntent {
    static var title: LocalizedStringResource = "Start or stop sleep"
    static var description = IntentDescription("Starts sleep, or ends it if already asleep.")

    @MainActor
    func perform() async throws -> some IntentResult {
        let store = try WidgetStore.make()
        guard let baby = try WidgetStore.currentBaby(in: store) else { return .result() }

        if let running = try store.activeTimer(kind: .sleep, for: baby.id) {
            _ = try store.stopSleep(running)
        } else {
            _ = try store.startSleep(for: baby.id)
        }
        WidgetCenter.shared.reloadAllTimelines()
        return .result()
    }
}
```

- [ ] **Step 4: Implement the widgets**

`Apps/BabyTrackerWidgets/StatusWidget.swift`:

```swift
import WidgetKit
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

struct StatusEntry: TimelineEntry {
    let date: Date
    let babyName: String
    let feedAge: String
    let sleepText: String
    let diaperAge: String
}

struct StatusProvider: TimelineProvider {
    func placeholder(in context: Context) -> StatusEntry {
        StatusEntry(
            date: .now, babyName: "Baby",
            feedAge: "2h 14m", sleepText: "Awake 40m", diaperAge: "55m"
        )
    }

    func getSnapshot(in context: Context, completion: @escaping (StatusEntry) -> Void) {
        completion(placeholder(in: context))
    }

    func getTimeline(in context: Context, completion: @escaping (Timeline<StatusEntry>) -> Void) {
        Task { @MainActor in
            let entry = (try? Self.currentEntry()) ?? placeholder(in: context)
            // Relative ages drift, so refresh every few minutes.
            let next = Date().addingTimeInterval(300)
            completion(Timeline(entries: [entry], policy: .after(next)))
        }
    }

    @MainActor
    private static func currentEntry() throws -> StatusEntry {
        let store = try WidgetStore.make()
        guard let baby = try WidgetStore.currentBaby(in: store) else {
            throw CocoaError(.fileNoSuchFile)
        }
        let snapshot = try store.homeSnapshot(for: baby.id)
        let now = store.now

        func age(_ date: Date?) -> String {
            guard let date else { return "—" }
            return ElapsedFormatter.compact(Int(now.timeIntervalSince(date)))
        }

        let sleepText: String = if let timer = snapshot.sleepTimer {
            "Asleep \(ElapsedFormatter.compact(timer.elapsedSeconds(now: now)))"
        } else if let ended = snapshot.lastSleep?.endedAt {
            "Awake \(age(ended))"
        } else {
            "—"
        }

        return StatusEntry(
            date: now,
            babyName: baby.name,
            feedAge: age(snapshot.lastFeed?.endedAt ?? snapshot.lastFeed?.startedAt),
            sleepText: sleepText,
            diaperAge: age(snapshot.lastDiaper?.startedAt)
        )
    }
}

struct StatusWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: "StatusWidget", provider: StatusProvider()) { entry in
            VStack(alignment: .leading, spacing: 7) {
                Label(entry.feedAge, systemImage: Tracker.feed.symbolName)
                    .foregroundStyle(Tracker.feed.color)
                Label(entry.sleepText, systemImage: Tracker.sleep.symbolName)
                    .foregroundStyle(Tracker.sleep.color)
                Label(entry.diaperAge, systemImage: Tracker.diaper.symbolName)
                    .foregroundStyle(Tracker.diaper.color)
            }
            .font(.caption.weight(.semibold))
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
            .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Status")
        .description("Time since the last feed, sleep, and diaper.")
        .supportedFamilies([.systemSmall])
    }
}
```

`Apps/BabyTrackerWidgets/QuickLogWidget.swift`:

```swift
import WidgetKit
import SwiftUI
import AppIntents
import BabyUI

struct QuickLogWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: "QuickLogWidget", provider: StatusProvider()) { entry in
            VStack(spacing: 9) {
                Text(entry.babyName)
                    .font(.caption.weight(.bold))
                    .frame(maxWidth: .infinity, alignment: .leading)

                HStack(spacing: 8) {
                    Button(intent: StartFeedIntent()) {
                        Label("Feed", systemImage: Tracker.feed.symbolName)
                            .frame(maxWidth: .infinity, minHeight: 44)
                    }
                    .tint(Tracker.feed.color)

                    Button(intent: ToggleSleepIntent()) {
                        Label("Sleep", systemImage: Tracker.sleep.symbolName)
                            .frame(maxWidth: .infinity, minHeight: 44)
                    }
                    .tint(Tracker.sleep.color)

                    Button(intent: LogDiaperIntent()) {
                        Label("Diaper", systemImage: Tracker.diaper.symbolName)
                            .frame(maxWidth: .infinity, minHeight: 44)
                    }
                    .tint(Tracker.diaper.color)
                }
                .buttonStyle(.borderedProminent)
                .font(.caption2.weight(.semibold))
            }
            .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Quick log")
        .description("Log a feed, sleep, or diaper without opening the app.")
        .supportedFamilies([.systemMedium])
    }
}
```

`Apps/BabyTrackerWidgets/BabyTrackerWidgets.swift` — the bundle. `FeedActivityWidget` does not exist until Task 21, so it is deliberately absent here; Task 21 adds that line.

```swift
import WidgetKit
import SwiftUI

@main
struct BabyTrackerWidgets: WidgetBundle {
    var body: some Widget {
        StatusWidget()
        QuickLogWidget()
    }
}
```

- [ ] **Step 5: Verify on the simulator**

Build and run, add both widgets to the Home Screen, tap the diaper button, and confirm the entry appears in the app's History. This verifies the App Group is wired correctly — if History stays empty, the group identifier does not match `BabySchema.appGroupID`.

- [ ] **Step 6: Commit**

```bash
git add .
git commit -m "feat: add status and quick-log widgets with App Intents"
```

---

### Task 21: Live Activity

**Files:**
- Create: `Apps/BabyTrackerWidgets/FeedActivityWidget.swift`
- Create: `Packages/BabyTracker/Sources/BabyStore/TimerActivityAttributes.swift`
- Create: `Apps/BabyTrackerApp/LiveActivityController.swift`
- Modify: `Apps/BabyTrackerApp/Info.plist` — add `NSSupportsLiveActivities` = `YES`

**Interfaces:**
- Consumes: `ActiveTimer`, `Tracker`
- Produces: `TimerActivityAttributes` with `ContentState(startedAt:sideLabel:)`; `LiveActivityController.start(for:)`, `.end(_ timer:)`

The Lock Screen counts up without the app running, using SwiftUI's range-based timer text. Stopping from the Lock Screen goes through the same intents as Task 20.

- [ ] **Step 1: Implement the attributes**

`Sources/BabyStore/TimerActivityAttributes.swift`:

```swift
import Foundation
import ActivityKit

public struct TimerActivityAttributes: ActivityAttributes {
    public struct ContentState: Codable, Hashable {
        public let startedAt: Date
        public let sideLabel: String

        public init(startedAt: Date, sideLabel: String) {
            self.startedAt = startedAt
            self.sideLabel = sideLabel
        }
    }

    public let babyName: String
    public let kindRaw: String

    public init(babyName: String, kindRaw: String) {
        self.babyName = babyName
        self.kindRaw = kindRaw
    }
}
```

- [ ] **Step 2: Implement the Live Activity UI**

`Apps/BabyTrackerWidgets/FeedActivityWidget.swift`:

```swift
import ActivityKit
import WidgetKit
import SwiftUI
import BabyStore
import BabyUI

struct FeedActivityWidget: Widget {
    var body: some WidgetConfiguration {
        ActivityConfiguration(for: TimerActivityAttributes.self) { context in
            HStack(spacing: 14) {
                Image(systemName: context.attributes.kindRaw == "sleep"
                      ? Tracker.sleep.symbolName : Tracker.feed.symbolName)
                    .font(.title2)
                    .foregroundStyle(tint(for: context.attributes.kindRaw))

                VStack(alignment: .leading, spacing: 2) {
                    Text(context.state.sideLabel)
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(.secondary)
                    // Counts up on the Lock Screen with the app not running.
                    Text(context.state.startedAt, style: .timer)
                        .font(.title2.weight(.bold).monospacedDigit())
                }

                Spacer()

                Button(intent: stopIntent(for: context.attributes.kindRaw)) {
                    Text("Stop").frame(minWidth: 64, minHeight: 44)
                }
                .buttonStyle(.borderedProminent)
                .tint(tint(for: context.attributes.kindRaw))
            }
            .padding()
            .activityBackgroundTint(Palette.ground)
        } dynamicIsland: { context in
            DynamicIsland {
                DynamicIslandExpandedRegion(.leading) {
                    Image(systemName: context.attributes.kindRaw == "sleep"
                          ? Tracker.sleep.symbolName : Tracker.feed.symbolName)
                        .foregroundStyle(tint(for: context.attributes.kindRaw))
                }
                DynamicIslandExpandedRegion(.center) {
                    Text(context.state.startedAt, style: .timer)
                        .font(.title3.weight(.bold).monospacedDigit())
                }
                DynamicIslandExpandedRegion(.bottom) {
                    Button(intent: stopIntent(for: context.attributes.kindRaw)) {
                        Text("Stop").frame(maxWidth: .infinity, minHeight: 44)
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(tint(for: context.attributes.kindRaw))
                }
            } compactLeading: {
                Image(systemName: context.attributes.kindRaw == "sleep"
                      ? Tracker.sleep.symbolName : Tracker.feed.symbolName)
                    .foregroundStyle(tint(for: context.attributes.kindRaw))
            } compactTrailing: {
                Text(context.state.startedAt, style: .timer)
                    .monospacedDigit()
                    .frame(maxWidth: 52)
            } minimal: {
                Image(systemName: context.attributes.kindRaw == "sleep"
                      ? Tracker.sleep.symbolName : Tracker.feed.symbolName)
            }
        }
    }

    private func tint(for kindRaw: String) -> Color {
        kindRaw == "sleep" ? Tracker.sleep.color : Tracker.feed.color
    }

    private func stopIntent(for kindRaw: String) -> any AppIntent {
        kindRaw == "sleep" ? ToggleSleepIntent() : StopFeedIntent()
    }
}
```

Add `StopFeedIntent` to `LogIntents.swift`:

```swift
struct StopFeedIntent: AppIntent {
    static var title: LocalizedStringResource = "Stop the feed"
    static var description = IntentDescription("Ends the running feed and saves it.")

    @MainActor
    func perform() async throws -> some IntentResult {
        let store = try WidgetStore.make()
        guard let baby = try WidgetStore.currentBaby(in: store),
              let timer = try store.activeTimer(kind: .feed, for: baby.id)
        else { return .result() }

        _ = try store.stopFeed(timer)
        WidgetCenter.shared.reloadAllTimelines()
        return .result()
    }
}
```

- [ ] **Step 3: Implement the controller**

`Apps/BabyTrackerApp/LiveActivityController.swift`:

```swift
import Foundation
import ActivityKit
import BabyCore
import BabyStore

@MainActor
enum LiveActivityController {
    /// Silently does nothing when the user has Live Activities disabled —
    /// a running timer must never depend on one existing.
    static func start(for timer: ActiveTimer, babyName: String) {
        guard ActivityAuthorizationInfo().areActivitiesEnabled else { return }

        let attributes = TimerActivityAttributes(
            babyName: babyName,
            kindRaw: timer.kindRaw
        )
        let state = TimerActivityAttributes.ContentState(
            startedAt: timer.startedAt,
            sideLabel: timer.currentSide.map { "Feeding · \($0.rawValue)" } ?? "Asleep"
        )

        let activity = try? Activity.request(
            attributes: attributes,
            content: .init(state: state, staleDate: nil)
        )
        timer.liveActivityID = activity?.id
    }

    static func update(for timer: ActiveTimer) async {
        guard let id = timer.liveActivityID,
              let activity = Activity<TimerActivityAttributes>.activities
                .first(where: { $0.id == id })
        else { return }

        await activity.update(.init(
            state: .init(
                startedAt: timer.startedAt,
                sideLabel: timer.currentSide.map { "Feeding · \($0.rawValue)" } ?? "Asleep"
            ),
            staleDate: nil
        ))
    }

    static func end(activityID: String?) async {
        guard let activityID,
              let activity = Activity<TimerActivityAttributes>.activities
                .first(where: { $0.id == activityID })
        else { return }

        await activity.end(nil, dismissalPolicy: .immediate)
    }
}
```

Wire it into `RootView.quickLog` and `HomeView.stopPrimary`: call `LiveActivityController.start(for:babyName:)` after starting a timer, `update(for:)` after `switchSide`, and `end(activityID:)` with the timer's `liveActivityID` captured *before* stopping, since `stopFeed` deletes the timer.

- [ ] **Step 4: Verify on device or simulator**

Start a feed, lock the screen, confirm the timer counts up on the Lock Screen and that tapping Stop ends the feed and writes the entry. Switch sides mid-feed and confirm the label updates.

- [ ] **Step 5: Commit**

```bash
git add .
git commit -m "feat: add Live Activity for running feed and sleep timers"
```

---

### Task 22: Accessibility pass and UI tests

**Files:**
- Create: `UITests/BabyTrackerUITests/LoggingFlowTests.swift`
- Modify: any view failing the checks below

**Interfaces:**
- Consumes: the whole app
- Produces: passing UI tests for the four two-tap flows

- [ ] **Step 1: Write the UI tests**

`UITests/BabyTrackerUITests/LoggingFlowTests.swift`:

```swift
import XCTest

final class LoggingFlowTests: XCTestCase {
    private var app: XCUIApplication!

    override func setUp() {
        continueAfterFailure = false
        app = XCUIApplication()
        app.launchArguments = ["-uiTestResetStore", "-uiTestSeedBaby"]
        app.launch()
    }

    func testDiaperIsTwoTaps() {
        app.buttons["Log diaper"].tap()
        app.buttons["Wet"].tap()

        app.buttons["History"].tap()
        XCTAssertTrue(app.staticTexts["Diaper"].waitForExistence(timeout: 2))
    }

    func testSleepStartsAndStopsFromHome() {
        app.buttons["Log sleep"].tap()
        XCTAssertTrue(app.buttons["Stop sleep"].waitForExistence(timeout: 2))

        app.buttons["Stop sleep"].tap()
        XCTAssertTrue(app.buttons["Log sleep"].waitForExistence(timeout: 2))
    }

    func testBreastFeedStartsOnTheSuggestedSide() {
        app.buttons["Log feed"].tap()
        app.buttons["Left"].tap()

        XCTAssertTrue(app.buttons["Stop feed"].waitForExistence(timeout: 2))
        app.buttons["Stop feed"].tap()

        app.buttons["History"].tap()
        XCTAssertTrue(app.staticTexts["Breast"].waitForExistence(timeout: 2))
    }

    func testBottleSavesWithPrefilledDefaults() {
        app.buttons["Log feed"].tap()
        app.buttons["Bottle instead"].tap()
        app.buttons["Save"].tap()

        app.buttons["History"].tap()
        XCTAssertTrue(app.staticTexts["Bottle"].waitForExistence(timeout: 2))
    }

    func testQuickLogRemainsReachableAtLargestTextSize() {
        // AX5: buttons must reflow rather than truncate.
        XCUIDevice.shared.appearance = .dark
        app.launchArguments += ["-UIPreferredContentSizeCategoryName",
                                "UICTContentSizeCategoryAccessibilityXXXL"]
        app.terminate()
        app.launch()

        XCTAssertTrue(app.buttons["Log feed"].exists)
        XCTAssertTrue(app.buttons["Log sleep"].exists)
        XCTAssertTrue(app.buttons["Log diaper"].exists)
    }
}
```

- [ ] **Step 2: Add the test launch arguments to the app**

In `AppEnvironment.init`, before creating the container:

```swift
if ProcessInfo.processInfo.arguments.contains("-uiTestResetStore") {
    let defaults = UserDefaults(suiteName: BabySchema.appGroupID) ?? .standard
    defaults.removePersistentDomain(forName: BabySchema.appGroupID)
}
```

And after creating `store`, seed a baby when `-uiTestSeedBaby` is present, so tests do not start on the add-baby screen:

```swift
if ProcessInfo.processInfo.arguments.contains("-uiTestSeedBaby"),
   (try? store.babies())?.isEmpty ?? true,
   let baby = try? store.addBaby(name: "Test Baby", birthDate: .now) {
    selection.select(baby.id)
}
```

- [ ] **Step 3: Run the UI tests**

Run: `xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=<DEVICE>' -only-testing:BabyTrackerUITests`
Expected: 5 tests pass

- [ ] **Step 4: Run the accessibility audit**

For each of Home (idle), Home (running), History, and Insights:

1. Run Xcode's Accessibility Inspector audit and fix every reported issue.
2. Set the simulator to AX5 (`Settings > Accessibility > Display & Text Size > Larger Text`) and confirm nothing truncates and the quick-log buttons reflow vertically.
3. Turn on VoiceOver and confirm each status row reads as one element with its title, value, and detail, and that the running dial announces elapsed time.
4. Turn on Reduce Motion and confirm the dial ring stops animating and the dial's breathing glow is disabled.
5. Check all three tracker colors at 4.5:1 against the dark ground using Accessibility Inspector's contrast tool. Darken or lighten any that fail and update `Palette`.

- [ ] **Step 5: Run the full test suite**

Run: `cd Packages/BabyTracker && swift test` then
`xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=<DEVICE>'`
Expected: all package tests and all UI tests pass

- [ ] **Step 6: Commit**

```bash
git add .
git commit -m "test: add logging flow UI tests and accessibility fixes"
```

---

### Task 23: Entry editing

**Files:**
- Create: `Sources/HistoryFeature/EntryEditSheet.swift`
- Modify: `Sources/HistoryFeature/HistoryView.swift` — present the sheet on row tap
- Test: `Tests/HistoryFeatureTests/EntryEditTests.swift`

**Interfaces:**
- Consumes: `store.update(_:)`, `HistoryItem`
- Produces: `EntryEditSheet(item:store:unit:onDone:)`

The spec requires every entry to be editable after creation, because the common
real-world failure is logging at the right moment with the wrong detail. This is
also the only route to `.dry` for diapers and to the nap/night override.

- [ ] **Step 1: Write the failing test**

`Tests/HistoryFeatureTests/EntryEditTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
@testable import HistoryFeature

@MainActor
@Test func editingADiaperLogsAnUpdateNotACreate() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    let entry = try store.logDiaper(.wet, for: baby.id)

    clock.advance(by: 120)
    entry.contents = .dry
    entry.startedAt = clock.now.addingTimeInterval(-2_400)
    try store.update(entry)

    let log = try store.changeLog()
    #expect(log.last?.operation == .update)
    #expect(entry.contents == .dry)
    #expect(entry.lastModifiedAt == clock.now)
}

@MainActor
@Test func overridingSleepKindBeatsTheDerivedValue() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    // 13:00 Berlin derives as a nap.
    var calendar = Calendar(identifier: .gregorian)
    calendar.timeZone = TimeZone(identifier: "Europe/Berlin")!
    var parts = DateComponents()
    parts.year = 2026; parts.month = 9; parts.day = 18; parts.hour = 13
    let afternoon = calendar.date(from: parts)!

    let sleep = try store.logSleep(from: afternoon, to: afternoon.addingTimeInterval(7_200), for: baby.id)
    sleep.timeZoneID = "Europe/Berlin"
    #expect(sleep.kind == .nap)

    sleep.kindOverride = .night
    try store.update(sleep)
    #expect(sleep.kind == .night)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter EntryEditTests`
Expected: FAIL — no test target member `EntryEditTests` until the file compiles; once it does, the assertions drive the sheet below.

- [ ] **Step 3: Implement the sheet**

`Sources/HistoryFeature/EntryEditSheet.swift`:

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public struct EntryEditSheet: View {
    let item: HistoryItem
    let store: BabyStore
    let unit: VolumeUnit
    let onDone: () -> Void

    @State private var when = Date()
    @State private var note = ""
    @State private var diaperContents: DiaperContents = .wet
    @State private var volumeML: Double = 0
    @State private var sleepOverride: SleepKind?

    public init(
        item: HistoryItem,
        store: BabyStore,
        unit: VolumeUnit = .millilitres,
        onDone: @escaping () -> Void
    ) {
        self.item = item
        self.store = store
        self.unit = unit
        self.onDone = onDone
    }

    public var body: some View {
        NavigationStack {
            Form {
                Section {
                    DatePicker("Time", selection: $when)
                }

                switch item {
                case .diaper:
                    Section {
                        // The full set, including .dry, which the fast path omits.
                        Picker("Contents", selection: $diaperContents) {
                            ForEach(DiaperContents.allCases, id: \.self) {
                                Text($0.rawValue.capitalized).tag($0)
                            }
                        }
                    }
                case .feed(let entry):
                    if entry.volumeML != nil {
                        Section {
                            Stepper(value: $volumeML, in: 0...400, step: 10) {
                                Text(Volume(millilitres: volumeML).formatted(as: unit))
                            }
                        }
                    }
                case .sleep:
                    Section {
                        Picker("Classified as", selection: $sleepOverride) {
                            Text("Automatic").tag(SleepKind?.none)
                            Text("Nap").tag(SleepKind?.some(.nap))
                            Text("Night").tag(SleepKind?.some(.night))
                        }
                    }
                }

                Section {
                    TextField("Note", text: $note, axis: .vertical)
                }
            }
            .navigationTitle("Edit entry")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel", action: onDone)
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save", action: save)
                }
            }
            .task(load)
        }
    }

    private func load() {
        when = item.startedAt
        switch item {
        case .diaper(let entry):
            diaperContents = entry.contents
            note = entry.note ?? ""
        case .feed(let entry):
            volumeML = entry.volumeML ?? 0
            note = entry.note ?? ""
        case .sleep(let entry):
            sleepOverride = entry.kindOverride
            note = entry.note ?? ""
        }
    }

    private func save() {
        let trimmed = note.trimmingCharacters(in: .whitespacesAndNewlines)
        switch item {
        case .diaper(let entry):
            entry.startedAt = when
            entry.contents = diaperContents
            entry.note = trimmed.isEmpty ? nil : trimmed
            try? store.update(entry)
        case .feed(let entry):
            entry.startedAt = when
            if entry.volumeML != nil { entry.volumeML = volumeML }
            entry.note = trimmed.isEmpty ? nil : trimmed
            try? store.update(entry)
        case .sleep(let entry):
            entry.startedAt = when
            entry.kindOverride = sleepOverride
            entry.note = trimmed.isEmpty ? nil : trimmed
            try? store.update(entry)
        }
        onDone()
    }
}
```

- [ ] **Step 4: Present it from History**

In `HistoryView`, add `@State private var editing: HistoryItem?`, wrap each row in a
`Button { editing = section.items[index] } label: { HistoryRowView(row: row) }`, and attach:

```swift
.sheet(item: $editing) { item in
    EntryEditSheet(item: item, store: store, unit: volumeUnit) {
        editing = nil
        try? model.load(dayCount: 30)
    }
}
```

`HistoryView` must retain `store` and `volumeUnit` as stored properties for this.

- [ ] **Step 5: Run the tests and build**

Run: `swift test --filter EntryEditTests && swift build`
Expected: PASS, 2 tests; build succeeds

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -m "feat: add entry editing with time correction and overrides"
```

---

## Verification

The app is done when all of these hold:

- `swift test` passes in `Packages/BabyTracker` with no skipped tests
- `xcodebuild test -scheme BabyTracker` passes
- Every logging flow in Task 7's table is achievable in the stated number of taps
- Killing the app mid-feed and relaunching shows the correct elapsed time
- A widget-logged diaper appears in the app's History
- Stopping a feed from the Lock Screen writes the entry
- Accessibility Inspector reports no issues on all four screens
- `store.changeLog()` contains one entry per mutation after a manual session
- Every entry kind can be edited after the fact, including its time
