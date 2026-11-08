# Insights Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the Insights screen with one scroll of Feeding, Sleep, Diapers and Growth. The sections show per-day averages over Week, Month or All, with neutral comparisons, readable charts and feeding/sleep detail rows. The separate Growth details screen merges into Insights.

**Architecture:**
- **Calculator.** A pure `InsightsCalculator` turns plain value inputs (feed sessions, bottle steps, sleep sessions, diaper times) into an `InsightsSummary`, using day indexes relative to today in the device's time zone.
- **Presentation.** `InsightsPresentation` formats the summary into strings and chart bars, so the SwiftUI views only lay things out.
- **View model.** `InsightsViewModel` adapts `BabyStore` into calculator inputs and keeps owning a `GrowthViewModel`.
- **Growth.** The Growth section becomes `GrowthSectionView` in GrowthFeature. The old Growth details screen and weight card are deleted.

**Tech Stack:** Swift 6.2, SwiftUI and Swift Charts (iOS 26 / macOS 26), SwiftData-backed `BabyStore`, Swift Testing, XCTest UI tests, `.xcstrings` catalogs.

**Spec:** `docs/superpowers/specs/2026-10-04-insights-redesign-design.md`. Read its "Amendments from the code survey (2026-10-06)". They drop all Ask work except hiding the old Insights links in saved chats, remove Insights' focus mechanism, make today's bar a faint fill, and limit the per-day growth rate to weight.

## Global Constraints

- **Worktree.** Work in `.worktrees/insights-redesign` on branch `feat/insights-redesign`, created from `main` with superpowers:using-git-worktrees. Other sessions edit `main` in place, so never commit there.
- **Package tests.** Run them with `make test` from the worktree root. It asserts the literal total in `Makefile` (`EXPECTED_TESTS`).
  - Every task that adds or removes tests sets `EXPECTED_TESTS` to the count `make test` reports ("counted N"), in the same commit.
  - When the total drops, the commit message says which tests were deleted and why.
- **Focused runs.** From `Packages/BabyTracker`, run one module with `swift test --filter <TestTarget>`.
- **Simulator.** Use only `platform=iOS Simulator,id=<simulator-udid>`. Never create or clone simulators.
  - Check `pgrep -fl xcodebuild` first and wait if another run is active.
  - Never use `make build` or `make test-ui`, which select the simulator by name.
- **Comparisons are factual.** Comparison copy is never evaluative and never coloured: ▲/▼/≈ in `Palette.secondaryText`.
- **Section titles.** Sections show the tracker icon and colour only, with no title word. Words appear only in row labels, units and captions.
- **Strings.** All user-visible strings go through the module's `Localizable.xcstrings`, with translations in en, de, es, fr, it and uk.
  - GrowthFeature's catalog test demands state `translated` for all six locales on every key.
  - Remove strings that are no longer referenced.
- **Data model.** No change to the data model, the sync protocol or the server.
- **Accessibility.** Every control is at least 44 pt. Layouts switch at `dynamicTypeSize.isAccessibilitySize`, as the existing code does, rather than using `ViewThatFits`.
- **Text styles.** Use `.footnote` or larger for scaled text. `.caption`/`.caption2` fail the Dynamic Type audit (see the `InsightsView.swift` comments).
- **Commits** end with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.

## Review Focus

1. **DST day inside the period.** Europe/Zurich, 29 March 2026: Week still has 7 daily bars plus today, and a diaper at 23:30 on the short day lands on that day, not the next. *Task 1, `dstWeekKeepsOneBarPerDay`.*
2. **Opening Insights at 00:05.** Today is almost empty and yesterday is complete. Averages and comparisons use yesterday and earlier only, and today's bar is not counted. *Task 1, `justAfterMidnightTodayIsNotCounted`.*
3. **Overlapping sleeps from two caregivers.** "Awake between sleeps" skips negative gaps instead of reporting them. *Task 3, `overlappingSleepsSkipNegativeGaps`.*
4. **Months of history under All.** A year of data (about 3,650 feeds) still summarises in well under a second on the main actor. *Task 2, `aYearOfFeedsSummarisesQuickly`.*
5. **Ounces near zero.** In fl oz, a 1 ml difference reads "≈", not "▲ 0 fl oz", and bottle values keep one decimal. *Task 5, `ounceComparisonsRoundToTheDisplayedTenth`.*

---

## File Structure

| File | Responsibility |
|---|---|
| Create `Packages/BabyTracker/Sources/InsightsFeature/InsightsCalculator.swift` | `InsightRange`, input structs, summary structs, `InsightsCalculator` (pure) |
| Create `Packages/BabyTracker/Sources/InsightsFeature/InsightsPresentation.swift` | Strings, chart bars, rows and accessibility text from a summary |
| Create `Packages/BabyTracker/Sources/InsightsFeature/InsightSectionCard.swift` | One section card and its chart |
| Rewrite `Packages/BabyTracker/Sources/InsightsFeature/InsightsViewModel.swift` | Store → inputs → summary; owns `GrowthViewModel` |
| Rewrite `Packages/BabyTracker/Sources/InsightsFeature/InsightsView.swift` | Header, pinned range picker, section list, growth section, focus, reload |
| Create `Packages/BabyTracker/Sources/GrowthFeature/GrowthPresentation.swift` | `GrowthCopy`, `GrowthMetricPresentation`, `GrowthReferenceBand` (moved), new `GrowthSectionPresentation` |
| Create `Packages/BabyTracker/Sources/GrowthFeature/GrowthSectionView.swift` | Chips, change line, chart with selection, reference link, All check-ins, reference choice |
| Delete `GrowthFeature/GrowthView.swift`, `GrowthFeature/GrowthCardView.swift` | Old details screen and weight card |
| Modify `GrowthFeature/GrowthViewModel.swift` | Add `birthDate` and `checkInCount` |
| Modify `HistoryFeature/HistoryView.swift`, `HistoryFeature/HistoryViewModel.swift` | `HistoryFocus` filter, `apply(_:)` |
| Modify `Apps/BabyTrackerApp/RootView.swift` | Insights callbacks; Insights links from Ask are ignored; `insightFocus` removed |
| Modify `AskFeature/AskView.swift`, `AskFeature/AskViewModel.swift`, `Package.swift` | Hide old Insights source chips; drop the InsightsFeature dependency |
| Tests | `InsightsFeatureTests/InsightsCalculatorTests.swift`, `InsightsPresentationTests.swift`, `InsightsViewModelTests.swift` (rewritten), `InsightsGrowthTests.swift` (updated), `InsightsViewRenderTests.swift`; `AskFeatureTests/AskViewModelTests.swift` (updated); `GrowthFeatureTests/GrowthViewTests.swift` → `GrowthSectionTests.swift`; `HistoryFeatureTests/HistoryFocusTests.swift`; UI: `GrowthFlowUITests.swift`, `UITestHarness.swift` |

---

### Task 1: Periods, per-day averages, comparisons and bars

**Files:**
- Create: `Packages/BabyTracker/Sources/InsightsFeature/InsightsCalculator.swift`
- Test: `Packages/BabyTracker/Tests/InsightsFeatureTests/InsightsCalculatorTests.swift`

**Interfaces:**
- Produces (all `internal` unless marked):
  - `public enum InsightRange: String, CaseIterable, Identifiable, Sendable { case week, month, all }`, with `days: Int?` and `title: String`.
  - `struct FeedSessionInput`, `struct BottleInput`, `struct SleepSessionInput`, `struct InsightsInput`.
  - `enum InsightComparison { case change(Double), coverage(days: Int), since(Date) }`.
  - `struct InsightAverage { value: Double; comparison: InsightComparison }`.
  - `struct InsightBar { start: Date; value: Double?; secondary: Double?; isToday: Bool }`.
  - `struct FeedingInsights`, `struct SleepInsights`, `struct DiaperInsights`, and `struct InsightsSummary { range; feeding; sleep; diapers; static let empty }`.
  - `struct InsightsCalculator { init(timeZone: TimeZone); func summary(_ input: InsightsInput, range: InsightRange, now: Date) -> InsightsSummary }`.

  Tasks 2 and 3 fill the feeding and sleep fields. This task computes `perDay` and `bars` for all three sections.

- [ ] **Step 0: Set up the worktree and baselines**

```bash
cd <repo>
git worktree add .worktrees/insights-redesign -b feat/insights-redesign main
cd .worktrees/insights-redesign
mkdir -p work
python3 scripts/verify-localizations.py > work/insights-loc-baseline.txt 2>&1 || true
make test   # expect "OK: <EXPECTED_TESTS> tests"; record the number in the task notes
```

- [ ] **Step 1: Write the failing tests**

Create `Packages/BabyTracker/Tests/InsightsFeatureTests/InsightsCalculatorTests.swift`:

```swift
import Foundation
import Testing
import BabyCore
@testable import InsightsFeature

/// Wall-clock times in Zurich, counted back from Monday 15 June 2026.
enum Clock {
    static let zone = TimeZone(identifier: "Europe/Zurich")!
    static var calendar: Calendar { var c = Calendar(identifier: .gregorian); c.timeZone = zone; return c }
    static func at(_ daysAgo: Int, _ hour: Int, _ minute: Int = 0,
                   from base: DateComponents = DateComponents(year: 2026, month: 6, day: 15)) -> Date {
        let day = calendar.date(byAdding: .day, value: -daysAgo, to: calendar.date(from: base)!)!
        return calendar.date(bySettingHour: hour, minute: minute, second: 0, of: day)!
    }
    static func startOfDay(_ daysAgo: Int) -> Date { at(daysAgo, 0) }
    static let now = at(0, 12)
}

private func summary(_ input: InsightsInput, _ range: InsightRange = .week, now: Date = Clock.now) -> InsightsSummary {
    InsightsCalculator(timeZone: Clock.zone).summary(input, range: range, now: now)
}

private func diapers(_ perDay: [Int: Int]) -> InsightsInput {
    var input = InsightsInput()
    for (daysAgo, count) in perDay {
        for i in 0..<count { input.diapers.append(Clock.at(daysAgo, 8 + i)) }
    }
    return input
}

@Suite struct InsightsPeriodTests {
    @Test func weekAveragesCompleteDaysOnlyAndShowsTodayApart() {
        var perDay = Dictionary(uniqueKeysWithValues: (1...7).map { ($0, 6) })
        perDay[0] = 2
        let result = summary(diapers(perDay)).diapers
        #expect(result.perDay?.value == 6)
        #expect(result.bars.count == 8)
        #expect(result.bars.last?.isToday == true)
        #expect(result.bars.last?.value == 2)
        #expect(result.bars.first?.start == Clock.startOfDay(7))
    }

    @Test func weekComparesWithTheSevenDaysBefore() {
        var perDay = Dictionary(uniqueKeysWithValues: (1...7).map { ($0, 6) })
        for day in 8...14 { perDay[day] = 5 }
        #expect(summary(diapers(perDay)).diapers.perDay?.comparison == .change(1))
    }

    @Test func noPreviousPeriodGivesCoverage() {
        #expect(summary(diapers([1: 4, 2: 6])).diapers.perDay?.comparison == .coverage(days: 2))
    }

    @Test func daysWithNothingLoggedAreNotCounted() {
        let result = summary(diapers([1: 4, 3: 4])).diapers
        #expect(result.perDay?.value == 4)
        #expect(result.bars[result.bars.count - 3].value == nil) // two days ago: an empty slot
    }

    @Test func monthUsesThirtyDaysAndTheThirtyBefore() {
        let result = summary(diapers([30: 3, 31: 1]), .month).diapers
        #expect(result.perDay?.value == 3)
        #expect(result.perDay?.comparison == .change(2))
        #expect(result.bars.count == 31)
    }

    @Test func allUsesSevenDayBlocksCountingBackFromYesterday() {
        let result = summary(diapers([10: 3, 9: 3, 2: 6, 1: 6]), .all).diapers
        #expect(result.bars.count == 2)
        #expect(result.bars[0].start == Clock.startOfDay(10))
        #expect(result.bars[0].value == 3)   // days 10…8, two counted
        #expect(result.bars[1].start == Clock.startOfDay(7))
        #expect(result.bars[1].value == 6)
        #expect(result.bars.allSatisfy { !$0.isToday })
        #expect(result.perDay?.value == 4.5)
        #expect(result.perDay?.comparison == .since(Clock.startOfDay(10)))
    }

    @Test func allWithOnlyTodayIsEmpty() {
        let result = summary(diapers([0: 3]), .all).diapers
        #expect(result.perDay == nil)
        #expect(result.bars.isEmpty)
    }

    @Test func nothingInThePeriodLeavesNoAverage() {
        let result = summary(diapers([0: 3])).diapers
        #expect(result.perDay == nil)
        #expect(result.bars.count == 8)
    }

    // Review focus 1.
    @Test func dstWeekKeepsOneBarPerDay() {
        let base = DateComponents(year: 2026, month: 3, day: 30)   // Zurich moved to summer time on 29 March
        var input = InsightsInput()
        input.diapers = [Clock.at(1, 23, 30, from: base), Clock.at(2, 8, from: base)]
        let result = InsightsCalculator(timeZone: Clock.zone)
            .summary(input, range: .week, now: Clock.at(0, 12, from: base)).diapers
        #expect(result.bars.count == 8)
        #expect(Set(result.bars.map(\.start)).count == 8)
        #expect(result.bars[6].value == 1)   // 29 March, the short day
        #expect(result.bars[5].value == 1)   // 28 March
    }

    // Review focus 2.
    @Test func justAfterMidnightTodayIsNotCounted() {
        var input = diapers([1: 6])
        input.diapers.append(Clock.at(0, 0, 2))
        let result = summary(input, now: Clock.at(0, 0, 5)).diapers
        #expect(result.perDay?.value == 6)
        #expect(result.bars.last?.value == 1)
    }

}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter InsightsPeriodTests`
Expected: the build fails with `cannot find 'InsightsCalculator' in scope`.

- [ ] **Step 3: Write the implementation**

Create `Packages/BabyTracker/Sources/InsightsFeature/InsightsCalculator.swift`. While doing so:
- Delete the old `InsightRange`, `DayPoint`, `InsightFigure`, `TimelineSpan`, `TimelineTick` and `DayRhythm` from `InsightsViewModel.swift`.
- Make `InsightsViewModel.load()` temporarily compile by producing `InsightsSummary.empty`. Task 4 rewrites it.
- Delete `RhythmStrip`, `RhythmAxis`, `TrendChart`, `extension DayRhythm` and the figure/rhythm cards from `InsightsView.swift`, leaving a body that lists nothing but the growth card. Task 7 rewrites the view.
- Delete the tests in `InsightsViewModelTests.swift`, which test the removed figures, rhythm and averages; Task 4 replaces them. In `InsightsGrowthTests.swift`, delete the `figures`/`rhythm` lines.
- In `RootView.swift`, so the app keeps compiling:
  - replace the two Insights cases in Ask's `onOpenSource` with `case .insight, .insightRange: break` (old saved links do nothing; Task 8 hides them);
  - delete `@State private var insightFocus` and the `focus: insightFocus` argument.

  Delete `InsightFocus` and the focus handling (`.onChange(of: focus?.id)`, `.task(id: model.figures.count)`, the `focus` parameter) from `InsightsView.swift`.

```swift
import Foundation
import BabyCore

/// How far back Insights reaches. Today is never part of a period: a day
/// still in progress compared with whole days says nothing true.
public enum InsightRange: String, CaseIterable, Identifiable, Sendable {
    case week, month, all

    public var id: String { rawValue }
    /// Complete days in the period; nil for All, which starts at the first
    /// logged day.
    var days: Int? {
        switch self {
        case .week: 7
        case .month: 30
        case .all: nil
        }
    }
    public var title: String {
        switch self {
        case .week: L10n.tr("Week")
        case .month: L10n.tr("Month")
        case .all: L10n.tr("All")
        }
    }
}

// MARK: - Inputs

/// One feeding session, as `BabyStore.feedingSessions` projects it.
struct FeedSessionInput: Equatable, Sendable {
    var startedAt: Date
    var hasBreast = false
    var hasBottle = false
    var leftSeconds = 0
    var rightSeconds = 0
}

/// One measured bottle, on the day its step happened (`measuredBottleSteps`).
struct BottleInput: Equatable, Sendable {
    var at: Date
    var volumeML: Double
}

/// One sleep session. `endedAt` is nil while it is still running.
struct SleepSessionInput: Equatable, Sendable {
    var startedAt: Date
    var endedAt: Date?
    var asleepSeconds: Int
    var kind: SleepKind
    /// Each completed part's length: one continuous sleep each.
    var partSeconds: [Int]
}

struct InsightsInput: Equatable, Sendable {
    var feeds: [FeedSessionInput] = []
    var bottles: [BottleInput] = []
    var sleeps: [SleepSessionInput] = []
    var diapers: [Date] = []
}

// MARK: - Output

enum InsightComparison: Equatable, Sendable {
    /// Current average minus the previous period's, in the metric's own unit.
    case change(Double)
    /// No days in the previous period: the average covers this many days.
    case coverage(days: Int)
    /// All: the first counted day.
    case since(Date)
}

struct InsightAverage: Equatable, Sendable {
    let value: Double
    let comparison: InsightComparison
}

/// One bar: a day (Week, Month) or a 7-day block (All). `value` is nil when
/// nothing of the section's kind was logged; `secondary` is the naps share
/// of a sleep bar.
struct InsightBar: Equatable, Sendable {
    let start: Date
    let value: Double?
    let secondary: Double?
    let isToday: Bool
}

struct FeedingInsights: Equatable, Sendable {
    var perDay: InsightAverage?
    var bars: [InsightBar] = []
    var betweenFeeds: TimeInterval?
    var longestGap: TimeInterval?
    var breastOnly = 0
    var both = 0
    var bottleOnly = 0
    var leftSeconds = 0
    var rightSeconds = 0
    /// nil when no bottle was given in the period.
    var bottlePerDayML: Double?
}

struct SleepInsights: Equatable, Sendable {
    /// Seconds asleep per day.
    var perDay: InsightAverage?
    var bars: [InsightBar] = []
    var longestStretch: Int?
    var nightPerDay: Double?
    var napsPerDay: Double?
    var napCountPerDay: Double?
    var awakeBetween: TimeInterval?
}

struct DiaperInsights: Equatable, Sendable {
    var perDay: InsightAverage?
    var bars: [InsightBar] = []
}

struct InsightsSummary: Equatable, Sendable {
    var range: InsightRange
    var feeding = FeedingInsights()
    var sleep = SleepInsights()
    var diapers = DiaperInsights()

    static let empty = InsightsSummary(range: .week)
}

// MARK: - Calculator

/// Every number on Insights, from plain values. Days are indexes relative to
/// today in the device's zone: 0 is today, -1 yesterday.
struct InsightsCalculator {
    let calendar: Calendar

    init(timeZone: TimeZone) {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = timeZone
        self.calendar = calendar
    }

    /// The period's day indexes and the one before it.
    struct Period {
        let days: ClosedRange<Int>?
        let previous: ClosedRange<Int>?
    }

    func summary(_ input: InsightsInput, range: InsightRange, now: Date) -> InsightsSummary {
        let today = calendar.startOfDay(for: now)
        let sleeps = input.sleeps.filter { $0.endedAt != nil }
        let firstDay = (input.feeds.map(\.startedAt) + sleeps.map(\.startedAt) + input.diapers)
            .map { index($0, today: today) }.min()
        let period = period(for: range, firstDay: firstDay)

        var result = InsightsSummary(range: range)
        var diaperDays: [Int: Double] = [:]
        for date in input.diapers { diaperDays[index(date, today: today), default: 0] += 1 }
        result.diapers.perDay = average(diaperDays, period: period, range: range, today: today)
        result.diapers.bars = bars(diaperDays, period: period, range: range, today: today)

        var feedDays: [Int: Double] = [:]
        for feed in input.feeds { feedDays[index(feed.startedAt, today: today), default: 0] += 1 }
        result.feeding.perDay = average(feedDays, period: period, range: range, today: today)
        result.feeding.bars = bars(feedDays, period: period, range: range, today: today)

        var sleepDays: [Int: Double] = [:]
        for sleep in sleeps { sleepDays[index(sleep.startedAt, today: today), default: 0] += Double(sleep.asleepSeconds) }
        result.sleep.perDay = average(sleepDays, period: period, range: range, today: today)
        result.sleep.bars = bars(sleepDays, period: period, range: range, today: today)
        return result
    }

    // MARK: Days

    func index(_ date: Date, today: Date) -> Int {
        calendar.dateComponents([.day], from: today, to: calendar.startOfDay(for: date)).day ?? 0
    }

    func date(_ index: Int, today: Date) -> Date {
        calendar.date(byAdding: .day, value: index, to: today) ?? today
    }

    func period(for range: InsightRange, firstDay: Int?) -> Period {
        if let n = range.days {
            return Period(days: -n ... -1, previous: (-2 * n) ... (-n - 1))
        }
        guard let firstDay, firstDay <= -1 else { return Period(days: nil, previous: nil) }
        return Period(days: firstDay ... -1, previous: nil)
    }

    /// The mean over counted days (days with an entry in `values`).
    func average(_ values: [Int: Double], period: Period, range: InsightRange, today: Date) -> InsightAverage? {
        guard let days = period.days else { return nil }
        let current = values.filter { days.contains($0.key) }
        guard let first = current.keys.min() else { return nil }
        let mean = current.values.reduce(0, +) / Double(current.count)
        guard let previousDays = period.previous else {
            return InsightAverage(value: mean, comparison: .since(date(first, today: today)))
        }
        let previous = values.filter { previousDays.contains($0.key) }
        guard !previous.isEmpty else {
            return InsightAverage(value: mean, comparison: .coverage(days: current.count))
        }
        let previousMean = previous.values.reduce(0, +) / Double(previous.count)
        return InsightAverage(value: mean, comparison: .change(mean - previousMean))
    }

    /// Week and Month: one bar per day plus today. All: one bar per 7-day
    /// block counting back from yesterday, oldest first.
    func bars(_ values: [Int: Double], secondary: [Int: Double]? = nil,
              period: Period, range: InsightRange, today: Date) -> [InsightBar] {
        func second(_ i: Int) -> Double? {
            guard let secondary, values[i] != nil else { return nil }
            return secondary[i] ?? 0
        }
        if let n = range.days {
            return (-n ... 0).map { i in
                InsightBar(start: date(i, today: today), value: values[i], secondary: second(i), isToday: i == 0)
            }
        }
        guard let days = period.days else { return [] }
        var blocks: [InsightBar] = []
        var end = -1
        while end >= days.lowerBound {
            let start = max(end - 6, days.lowerBound)
            let counted = (start ... end).filter { values[$0] != nil }
            let mean: (([Int]) -> Double?) = { indexes in
                indexes.isEmpty ? nil : indexes.map { values[$0]! }.reduce(0, +) / Double(indexes.count)
            }
            let naps: Double? = (secondary == nil || counted.isEmpty) ? nil
                : counted.map { secondary![$0] ?? 0 }.reduce(0, +) / Double(counted.count)
            blocks.append(InsightBar(start: date(start, today: today), value: mean(counted), secondary: naps, isToday: false))
            end = start - 1
        }
        return blocks.reversed()
    }

    static func median(_ values: [Double]) -> Double? {
        let sorted = values.sorted()
        guard !sorted.isEmpty else { return nil }
        let middle = sorted.count / 2
        return sorted.count.isMultiple(of: 2) ? (sorted[middle - 1] + sorted[middle]) / 2 : sorted[middle]
    }
}
```

Add to `InsightsViewModel.swift` (temporary, replaced in Task 4):

```swift
// Temporary until Task 4: the screen shows growth only.
private(set) var summary = InsightsSummary.empty
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd Packages/BabyTracker && swift test --filter InsightsFeatureTests`
Expected: PASS. `InsightsPeriodTests` passes, and `InsightsGrowthTests` still passes without its figure/rhythm lines.

- [ ] **Step 5: Run the whole suite, set the total, and commit**

```bash
make test   # FAIL "expected X tests, counted Y" → set EXPECTED_TESTS := Y in Makefile, re-run → OK
git add -A Packages/BabyTracker Apps/BabyTrackerApp/RootView.swift Makefile
git commit -m "feat(insights): periods, per-day averages and bars for the redesign

Removes the today-vs-usual figure tests and the 24-hour strip tests
(InsightsViewModelTests): the figures and the strip are gone from the spec.

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Feeding metrics

**Files:**
- Modify: `Packages/BabyTracker/Sources/InsightsFeature/InsightsCalculator.swift`
- Test: `Packages/BabyTracker/Tests/InsightsFeatureTests/InsightsCalculatorTests.swift`

**Interfaces:**
- Consumes: Task 1 types.
- Produces: `FeedingInsights.betweenFeeds`, `longestGap`, `breastOnly`, `both`, `bottleOnly`, `leftSeconds`, `rightSeconds`, `bottlePerDayML`, filled by `summary(_:range:now:)`.

- [ ] **Step 1: Write the failing tests** (append to `InsightsCalculatorTests.swift`)

```swift
private func breast(_ date: Date, left: Int = 600, right: Int = 0) -> FeedSessionInput {
    FeedSessionInput(startedAt: date, hasBreast: true, leftSeconds: left, rightSeconds: right)
}
private func bottle(_ date: Date) -> FeedSessionInput {
    FeedSessionInput(startedAt: date, hasBottle: true)
}

@Suite struct InsightsFeedingTests {
    @Test func feedsPerDayCountsSessions() {
        var input = InsightsInput()
        input.feeds = [breast(Clock.at(1, 6)), breast(Clock.at(1, 9)), breast(Clock.at(2, 6))]
        #expect(summary(input).feeding.perDay?.value == 1.5)
    }

    @Test func betweenFeedsIsTheMedianStartToStartAndLongestGapTheMax() {
        var input = InsightsInput()
        input.feeds = [Clock.at(8, 22), Clock.at(7, 6), Clock.at(7, 9), Clock.at(7, 12), Clock.at(7, 17)].map { breast($0) }
        let feeding = summary(input).feeding
        // Gaps whose later feed is in the week: 8h (from the previous week), 3h, 3h, 5h.
        #expect(feeding.betweenFeeds == 4 * 3600)
        #expect(feeding.longestGap == 8 * 3600)
    }

    @Test func splitCountsSessionsAndLeavesSolidsOut() {
        var input = InsightsInput()
        input.feeds = [
            breast(Clock.at(1, 6)), breast(Clock.at(1, 9)), breast(Clock.at(1, 12)),
            FeedSessionInput(startedAt: Clock.at(1, 15), hasBreast: true, hasBottle: true, leftSeconds: 300),
            bottle(Clock.at(1, 18)),
            FeedSessionInput(startedAt: Clock.at(1, 19)),   // solids only
        ]
        let feeding = summary(input).feeding
        #expect((feeding.breastOnly, feeding.both, feeding.bottleOnly) == (3, 1, 1))
        #expect(feeding.perDay?.value == 6)
    }

    @Test func sidesSumBreastTimeInThePeriod() {
        var input = InsightsInput()
        input.feeds = [breast(Clock.at(1, 6), left: 600, right: 300), breast(Clock.at(2, 6), left: 0, right: 900),
                       breast(Clock.at(9, 6), left: 5000)]   // previous week: not counted
        let feeding = summary(input).feeding
        #expect((feeding.leftSeconds, feeding.rightSeconds) == (600, 1200))
    }

    @Test func bottlePerDayAveragesOverDaysWithFeeds() {
        var input = InsightsInput()
        input.feeds = [bottle(Clock.at(1, 9)), breast(Clock.at(2, 9))]
        input.bottles = [BottleInput(at: Clock.at(1, 9), volumeML: 120), BottleInput(at: Clock.at(1, 15), volumeML: 180)]
        #expect(summary(input).feeding.bottlePerDayML == 150)
        input.bottles = []
        #expect(summary(input).feeding.bottlePerDayML == nil)
    }

    // Review focus 4.
    @Test func aYearOfFeedsSummarisesQuickly() {
        var input = InsightsInput()
        for day in 1...365 { for hour in stride(from: 0, to: 24, by: 2) { input.feeds.append(breast(Clock.at(day, hour))) } }
        let clock = ContinuousClock()
        let elapsed = clock.measure { _ = summary(input, .all) }
        #expect(elapsed < .seconds(1))
        #expect(summary(input, .all).feeding.perDay?.value == 12)
    }
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter InsightsFeedingTests`
Expected: FAIL. `betweenFeeds`, the split, the sides and `bottlePerDayML` are still nil or 0.

- [ ] **Step 3: Implement.** In `summary(_:range:now:)`, after the feeding `perDay`/`bars` lines, add:

```swift
        if let days = period.days {
            let inPeriod = { (date: Date) in days.contains(self.index(date, today: today)) }
            let starts = input.feeds.map(\.startedAt).sorted()
            let gaps = zip(starts, starts.dropFirst())
                .filter { inPeriod($0.1) }
                .map { $0.1.timeIntervalSince($0.0) }
            result.feeding.betweenFeeds = Self.median(gaps)
            result.feeding.longestGap = gaps.max()

            for feed in input.feeds where inPeriod(feed.startedAt) {
                switch (feed.hasBreast, feed.hasBottle) {
                case (true, true): result.feeding.both += 1
                case (true, false): result.feeding.breastOnly += 1
                case (false, true): result.feeding.bottleOnly += 1
                case (false, false): break   // solids only
                }
                result.feeding.leftSeconds += feed.leftSeconds
                result.feeding.rightSeconds += feed.rightSeconds
            }

            var bottleDays: [Int: Double] = [:]
            for step in input.bottles { bottleDays[index(step.at, today: today), default: 0] += step.volumeML }
            let counted = feedDays.keys.filter { days.contains($0) }
            if !counted.isEmpty, counted.contains(where: { (bottleDays[$0] ?? 0) > 0 }) {
                result.feeding.bottlePerDayML = counted.map { bottleDays[$0] ?? 0 }.reduce(0, +) / Double(counted.count)
            }
        }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd Packages/BabyTracker && swift test --filter InsightsFeatureTests`
Expected: PASS.

- [ ] **Step 5: Commit** (run `make test` first and bump `EXPECTED_TESTS` by the 6 new tests)

```bash
git add -A Packages/BabyTracker Makefile
git commit -m "feat(insights): feeding spacing, breast/bottle split, sides and bottle per day

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Sleep metrics

**Files:**
- Modify: `Packages/BabyTracker/Sources/InsightsFeature/InsightsCalculator.swift`
- Test: `Packages/BabyTracker/Tests/InsightsFeatureTests/InsightsCalculatorTests.swift`

**Interfaces:**
- Consumes: Task 1 types.
- Produces:
  - `SleepInsights.longestStretch`, `nightPerDay`, `napsPerDay`, `napCountPerDay` and `awakeBetween`.
  - Sleep bars carrying `secondary` (nap seconds).

- [ ] **Step 1: Write the failing tests** (append)

```swift
private func sleep(_ start: Date, _ end: Date?, kind: SleepKind = .nap, parts: [Int]? = nil) -> SleepSessionInput {
    let seconds = end.map { Int($0.timeIntervalSince(start)) } ?? 0
    return SleepSessionInput(startedAt: start, endedAt: end, asleepSeconds: parts?.reduce(0, +) ?? seconds,
                             kind: kind, partSeconds: parts ?? (end == nil ? [] : [seconds]))
}

@Suite struct InsightsSleepTests {
    @Test func totalsCountOnTheStartDayAndSplitNightFromNaps() {
        var input = InsightsInput()
        input.sleeps = [
            sleep(Clock.at(1, 9), Clock.at(1, 10)),                       // 1h nap
            sleep(Clock.at(1, 20), Clock.at(0, 4), kind: .night),         // 8h night, counted on day 1
        ]
        let result = summary(input).sleep
        #expect(result.perDay?.value == 9 * 3600)
        #expect(result.nightPerDay == 8 * 3600)
        #expect(result.napsPerDay == 3600)
        #expect(result.napCountPerDay == 1)
        let yesterday = result.bars[result.bars.count - 2]
        #expect(yesterday.value == 9 * 3600)
        #expect(yesterday.secondary == 3600)
    }

    @Test func longestStretchIsTheLongestPartNotTheSession() {
        var input = InsightsInput()
        input.sleeps = [sleep(Clock.at(1, 20), Clock.at(0, 5), kind: .night, parts: [3 * 3600, 5 * 3600])]
        #expect(summary(input).sleep.longestStretch == 5 * 3600)
    }

    @Test func awakeBetweenUsesDaytimeGapsUpToTwelveHours() {
        var input = InsightsInput()
        input.sleeps = [
            sleep(Clock.at(1, 9), Clock.at(1, 10)),
            sleep(Clock.at(1, 11, 30), Clock.at(1, 12, 30)),               // gap 1.5h, daytime
            sleep(Clock.at(1, 19, 30), Clock.at(1, 23), kind: .night),     // gap 7h, daytime
            sleep(Clock.at(1, 23, 30), Clock.at(0, 5), kind: .night),      // gap starts 23:00: night, skipped
            sleep(Clock.at(3, 6), Clock.at(3, 8)),
            sleep(Clock.at(3, 21), Clock.at(3, 23), kind: .night),         // gap 13h: skipped
        ]
        #expect(summary(input).sleep.awakeBetween == 4.25 * 3600)
    }

    @Test func runningSleepIsLeftOut() {
        var input = InsightsInput()
        input.sleeps = [sleep(Clock.at(1, 9), Clock.at(1, 10)), sleep(Clock.at(1, 21), nil, kind: .night)]
        let result = summary(input).sleep
        #expect(result.perDay?.value == 3600)
        #expect(result.awakeBetween == nil)
    }

    // Review focus 3.
    @Test func overlappingSleepsSkipNegativeGaps() {
        var input = InsightsInput()
        input.sleeps = [sleep(Clock.at(1, 9), Clock.at(1, 11)), sleep(Clock.at(1, 10), Clock.at(1, 12))]
        #expect(summary(input).sleep.awakeBetween == nil)
    }
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter InsightsSleepTests`
Expected: FAIL. The night/nap fields, the stretch and the awake gaps are nil, and the bar `secondary` is nil.

- [ ] **Step 3: Implement.** Replace the three sleep lines in `summary(_:range:now:)` with:

```swift
        var sleepDays: [Int: Double] = [:]
        var nightDays: [Int: Double] = [:]
        var napDays: [Int: Double] = [:]
        var napCounts: [Int: Double] = [:]
        for sleep in sleeps {
            let day = index(sleep.startedAt, today: today)
            sleepDays[day, default: 0] += Double(sleep.asleepSeconds)
            switch sleep.kind {
            case .night: nightDays[day, default: 0] += Double(sleep.asleepSeconds)
            case .nap:
                napDays[day, default: 0] += Double(sleep.asleepSeconds)
                napCounts[day, default: 0] += 1
            }
        }
        result.sleep.perDay = average(sleepDays, period: period, range: range, today: today)
        result.sleep.bars = bars(sleepDays, secondary: napDays, period: period, range: range, today: today)
        if let days = period.days {
            let counted = sleepDays.keys.filter { days.contains($0) }
            if !counted.isEmpty {
                let mean = { (values: [Int: Double]) in counted.map { values[$0] ?? 0 }.reduce(0, +) / Double(counted.count) }
                result.sleep.nightPerDay = mean(nightDays)
                result.sleep.napsPerDay = mean(napDays)
                result.sleep.napCountPerDay = mean(napCounts)
            }
            let periodSleeps = sleeps.filter { days.contains(index($0.startedAt, today: today)) }
            result.sleep.longestStretch = periodSleeps.flatMap(\.partSeconds).max()

            let ordered = sleeps.sorted { $0.startedAt < $1.startedAt }
            let gaps: [Double] = zip(ordered, ordered.dropFirst()).compactMap { earlier, later in
                guard let end = earlier.endedAt else { return nil }
                let gap = later.startedAt.timeIntervalSince(end)
                let hour = calendar.component(.hour, from: end)
                guard gap > 0, gap <= 12 * 3600,
                      hour >= SleepClassifier.dayStartHour, hour < SleepClassifier.nightStartHour,
                      days.contains(index(end, today: today)) else { return nil }
                return gap
            }
            result.sleep.awakeBetween = Self.median(gaps)
        }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd Packages/BabyTracker && swift test --filter InsightsFeatureTests`
Expected: PASS.

- [ ] **Step 5: Commit** (`make test`, bump `EXPECTED_TESTS` by 5)

```bash
git add -A Packages/BabyTracker Makefile
git commit -m "feat(insights): night and naps, longest stretch and awake time between sleeps

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: InsightsViewModel reads the store into the calculator

**Files:**
- Rewrite: `Packages/BabyTracker/Sources/InsightsFeature/InsightsViewModel.swift`
- Modify: `Packages/BabyTracker/Sources/GrowthFeature/GrowthViewModel.swift` (add `birthDate`, `checkInCount`)
- Rewrite: `Packages/BabyTracker/Tests/InsightsFeatureTests/InsightsViewModelTests.swift`
- Modify: `Packages/BabyTracker/Tests/InsightsFeatureTests/InsightsGrowthTests.swift`

**Interfaces:**
- Consumes: `InsightsCalculator`, `InsightsInput`, `InsightRange`; `BabyStore.feedingSessions(for:from:to:)`, `sleepSessions(for:from:to:)`, `entries(for:from:to:)` and `measuredBottleSteps(for:from:to:)`.
- Produces:
  - `InsightsViewModel` with `public let growth: GrowthViewModel`, `public var volumeUnit`, `public var range: InsightRange` and `private(set) var summary: InsightsSummary`.
  - `GrowthViewModel` with `public private(set) var birthDate: Date?` and `public private(set) var checkInCount: Int`.

- [ ] **Step 1: Write the failing tests.** Replace `InsightsViewModelTests.swift` with:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
@testable import InsightsFeature

/// Noon local on 15 June 2026; see HistoryFeatureTests for why not an epoch.
@MainActor private func noonLocal() -> Date {
    Calendar.current.date(from: DateComponents(year: 2026, month: 6, day: 15, hour: 12))!
}

@MainActor private func fixture() throws -> (BabyStore, UUID, Date) {
    let now = noonLocal()
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
    store.breakLimits = { .standard }
    let baby = try store.addBaby(name: "Mira", birthDate: now.addingTimeInterval(-60 * 86_400), referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id, now)
}

@MainActor @Suite struct InsightsViewModelTests {
    @Test func joinedFeedCountsOnceWithEveryBottleCounted() throws {
        let (store, baby, now) = try fixture()
        store.breakLimits = { BreakLimits(feed: 600, sleep: 900) }
        let day = now.addingTimeInterval(-86_400)
        _ = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: day)
        _ = try store.logBottle(volumeML: 40, contents: .formula, for: baby, at: day.addingTimeInterval(300))   // joins
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        #expect(model.summary.feeding.perDay?.value == 1)
        #expect(model.summary.feeding.bottleOnly == 1)
        #expect(model.summary.feeding.bottlePerDayML == 100)
    }

    @Test func breastSidesComeFromComponents() throws {
        let (store, baby, now) = try fixture()
        let start = now.addingTimeInterval(-86_400)
        let feed = FeedEntry(babyID: baby, kind: .breast, startedAt: start, timeZoneID: TimeZone.current.identifier)
        feed.endedAt = start.addingTimeInterval(900)
        feed.components = [.breast(id: UUID(), side: .left, seconds: 600), .breast(id: UUID(), side: .right, seconds: 300)]
        store.context.insert(feed)
        try store.context.save()
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        #expect((model.summary.feeding.leftSeconds, model.summary.feeding.rightSeconds) == (600, 300))
        #expect(model.summary.feeding.breastOnly == 1)
    }

    @Test func sleepKindAndPartsComeFromTheSessionMembers() throws {
        let (store, baby, now) = try fixture()
        store.breakLimits = { BreakLimits(feed: 1_800, sleep: 1_200) }
        let yesterday = Calendar.current.startOfDay(for: now).addingTimeInterval(-86_400)
        _ = try store.logSleep(from: yesterday.addingTimeInterval(20 * 3600), to: yesterday.addingTimeInterval(22 * 3600), for: baby)
        _ = try store.logSleep(from: yesterday.addingTimeInterval(22.25 * 3600), to: yesterday.addingTimeInterval(23.25 * 3600), for: baby) // joins
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        #expect(model.summary.sleep.perDay?.value == 3 * 3600)
        #expect(model.summary.sleep.nightPerDay == 3 * 3600)
        #expect(model.summary.sleep.longestStretch == 2 * 3600)
    }

    @Test func openSleepIsLeftOut() throws {
        let (store, baby, now) = try fixture()
        _ = try store.logSleep(from: now.addingTimeInterval(-86_400), to: nil, for: baby)
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        #expect(model.summary.sleep.perDay == nil)
    }

    @Test func rangeChangeIsReadOnTheNextLoad() throws {
        let (store, baby, now) = try fixture()
        _ = try store.logDiaper(.wet, for: baby, at: now.addingTimeInterval(-20 * 86_400))
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        #expect(model.summary.diapers.perDay == nil)
        model.range = .month
        try model.load()
        #expect(model.summary.range == .month)
        #expect(model.summary.diapers.perDay?.value == 1)
        model.range = .all
        try model.load()
        #expect(model.summary.diapers.bars.count == 3)
    }
}
```

Update `InsightsGrowthTests.swift`: replace `model.weightSummary` with `model.growth.summary(for: .weight)`. Delete the `figureTitles` and `model.figures`/`model.rhythm` lines, and assert:

```swift
        #expect(model.growth.checkInCount == 4)
        #expect(model.growth.birthDate == baby.birthDate)
```

These go just before `try store.softDelete(second)`. By then there are four check-ins: two length/head check-ins (at -120 s and -60 s) and two weight check-ins (at -60 s and `now`). Each check-in counts once, whatever it measured.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter InsightsFeatureTests`
Expected: the build fails (`summary` is private(set) on the temporary stub, and there is no `growth`, `checkInCount` or `birthDate`), or the assertions fail on an empty summary.

- [ ] **Step 3: Implement.**

In `GrowthViewModel.swift`:
- Add `public private(set) var birthDate: Date?` and `public private(set) var checkInCount = 0`.
- In `load()`, set `birthDate = baby.birthDate` and `checkInCount = readings.count` next to `summaries = newSummaries`.
- In the `catch`, reset both: `birthDate = nil; checkInCount = 0`.

Replace `InsightsViewModel.swift` with:

```swift
import Foundation
import Observation
import BabyCore
import BabyStore
import GrowthFeature

@MainActor
@Observable
public final class InsightsViewModel {
    private let store: BabyStore
    private let babyID: UUID
    /// Growth keeps its own model: its readings are not day-bucketed and it
    /// ignores the range.
    public let growth: GrowthViewModel
    /// A preference that can change while the screen is alive; the view sets
    /// it and reloads.
    public var volumeUnit: VolumeUnit
    /// Set by the range picker; the view reloads on change.
    public var range: InsightRange = .week
    private(set) var summary = InsightsSummary.empty

    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres) {
        self.store = store
        self.babyID = babyID
        self.volumeUnit = volumeUnit
        self.growth = GrowthViewModel(store: store, babyID: babyID)
    }

    /// One fetch per kind over the period and the period before it (or all
    /// history for All), then the calculator. Sessions come from the same
    /// projections Home and History use, so a joined feed or sleep counts
    /// once, on the day its first part started.
    public func load() throws {
        try growth.load()
        let zone = TimeZone.current
        let now = store.now
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = zone
        let today = calendar.startOfDay(for: now)
        let end = calendar.date(byAdding: .day, value: 1, to: today) ?? now
        // One extra day so the period's first feed has the feed before it.
        let start = range.days.flatMap { calendar.date(byAdding: .day, value: -(2 * $0 + 1), to: today) } ?? .distantPast

        var feedsByID: [UUID: FeedEntry] = [:]
        var sleepsByID: [UUID: SleepEntry] = [:]
        var input = InsightsInput()
        for item in try store.entries(for: babyID, from: start, to: end) {
            switch item {
            case .feed(let entry): feedsByID[entry.id] = entry
            case .sleep(let entry): sleepsByID[entry.id] = entry
            case .diaper(let entry): input.diapers.append(entry.startedAt)
            case .supplement: break
            }
        }
        input.feeds = try store.feedingSessions(for: babyID, from: start, to: end).map { session in
            let components = session.feedIDs.compactMap { feedsByID[$0] }.flatMap(\.components)
            return FeedSessionInput(
                startedAt: session.startedAt,
                hasBreast: components.contains { $0.breastSide != nil },
                hasBottle: components.contains { $0.bottleContents != nil },
                leftSeconds: components.filter { $0.breastSide == .left }.compactMap(\.durationSeconds).reduce(0, +),
                rightSeconds: components.filter { $0.breastSide == .right }.compactMap(\.durationSeconds).reduce(0, +)
            )
        }
        input.bottles = try store.measuredBottleSteps(for: babyID, from: start, to: end)
            .map { BottleInput(at: $0.at, volumeML: $0.volumeML) }
        input.sleeps = try store.sleepSessions(for: babyID, from: start, to: end).map { session in
            let parts = session.sleepIDs.compactMap { sleepsByID[$0] }
            return SleepSessionInput(
                startedAt: session.startedAt,
                endedAt: session.endedAt,
                asleepSeconds: session.asleepSeconds,
                kind: parts.first?.kind ?? .nap,
                partSeconds: parts.compactMap(\.durationSeconds)
            )
        }
        summary = InsightsCalculator(timeZone: zone).summary(input, range: range, now: now)
    }
}
```

Remove the temporary `summary` stub from Task 1. Any leftover use of `model.weightSummary` in `InsightsView.swift` becomes `model.growth.summary(for: .weight)`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd Packages/BabyTracker && swift test --filter "InsightsFeatureTests|GrowthFeatureTests"`
Expected: PASS.

- [ ] **Step 5: Commit** (`make test`, set `EXPECTED_TESTS`)

```bash
git add -A Packages/BabyTracker Makefile
git commit -m "feat(insights): read sessions, bottles and sleeps from the store into the calculator

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Presentation — strings, rows, chart bars, accessibility

**Files:**
- Create: `Packages/BabyTracker/Sources/InsightsFeature/InsightsPresentation.swift`
- Test: `Packages/BabyTracker/Tests/InsightsFeatureTests/InsightsPresentationTests.swift`

**Interfaces:**
- Consumes: `InsightsSummary` and everything in it; `VolumeUnit`, `Volume`, `ElapsedFormatter` (BabyCore); `Tracker` (BabyUI).
- Produces: `struct InsightsPresentation { init(summary:volumeUnit:); var sections: [InsightSection] }` and `struct InsightSection`, `struct ChangeText`, `struct InsightRow` and `struct ChartBar` (fields below), plus `static func percentShares(_:) -> [Int]`. `InsightsViewModel` gains `var sections: [InsightSection]`.

- [ ] **Step 1: Write the failing tests.** Create `InsightsPresentationTests.swift`:

```swift
import Foundation
import Testing
import BabyCore
@testable import InsightsFeature

private func feeding(_ perDay: InsightAverage?, range: InsightRange = .week,
                     configure: (inout FeedingInsights) -> Void = { _ in }) -> InsightSection {
    var summary = InsightsSummary(range: range)
    summary.feeding.perDay = perDay
    configure(&summary.feeding)
    return InsightsPresentation(summary: summary, volumeUnit: .millilitres).sections[0]
}

@Suite struct InsightsPresentationTests {
    @Test func countHeadlineAndChange() {
        let section = feeding(InsightAverage(value: 8.4, comparison: .change(0.62)))
        #expect(section.headline == "8.4")
        #expect(section.unit == "feeds / day")
        #expect(section.change == ChangeText(value: "▲ 0.6", caption: "vs prev. week"))
        #expect(section.accessibilityLabel == "Feeds: 8.4 feeds / day. 0.6 more than the previous week.")
    }

    @Test func aChangeThatRoundsToZeroReadsAsAboutTheSame() {
        let section = feeding(InsightAverage(value: 8, comparison: .change(-0.04)), range: .month)
        #expect(section.change == ChangeText(value: "≈", caption: "vs prev. month"))
        #expect(section.accessibilityLabel.hasSuffix("About the same as the previous month."))
    }

    @Test func coverageAndSince() {
        #expect(feeding(InsightAverage(value: 10.5, comparison: .coverage(days: 2))).change == ChangeText(value: "2 days", caption: ""))
        #expect(feeding(InsightAverage(value: 10, comparison: .coverage(days: 1))).change == ChangeText(value: "1 day", caption: ""))
        let since = feeding(InsightAverage(value: 9, comparison: .since(Date(timeIntervalSince1970: 1_785_000_000))), range: .all)
        #expect(since.change?.value.hasPrefix("since ") == true)
    }

    @Test func nothingLoggedHasNoHeadline() {
        let section = feeding(nil)
        #expect(section.headline == nil)
        #expect(section.rows.isEmpty)
        #expect(section.accessibilityLabel == "Feeds. Nothing logged in this period.")
    }

    @Test func feedingRowsAppearOnlyWhenTheyApply() {
        let breastOnly = feeding(InsightAverage(value: 8, comparison: .coverage(days: 3))) {
            $0.betweenFeeds = 2.5 * 3600; $0.longestGap = 5 * 3600 + 600
            $0.breastOnly = 8; $0.leftSeconds = 480; $0.rightSeconds = 520
        }
        #expect(breastOnly.rows.map(\.label) == ["Between feeds", "Longest gap", "Breast", "Left · right"])
        #expect(breastOnly.rows.map(\.value) == ["2h 30m", "5h 10m", "100%", "48% · 52%"])
        let mixed = feeding(InsightAverage(value: 8, comparison: .coverage(days: 3))) {
            $0.breastOnly = 16; $0.both = 2; $0.bottleOnly = 7; $0.bottlePerDayML = 410.4
        }
        #expect(mixed.rows.map(\.label) == ["Breast · Both · Bottle", "Bottle / day"])
        #expect(mixed.rows[0].value == "64% · 8% · 28%")
        #expect(mixed.rows[1].value == "410 ml")
    }

    @Test func percentSharesAlwaysSumToAHundred() {
        #expect(InsightsPresentation.percentShares([1, 1, 1]) == [34, 33, 33])
        #expect(InsightsPresentation.percentShares([2, 1]) == [67, 33])
        #expect(InsightsPresentation.percentShares([0, 0]) == [0, 0])
    }

    @Test func sleepHeadlineRowsAndStackedBars() {
        var summary = InsightsSummary(range: .week)
        summary.sleep.perDay = InsightAverage(value: 14 * 3600 + 20 * 60, comparison: .change(25 * 60))
        summary.sleep.longestStretch = 5 * 3600 + 40 * 60
        summary.sleep.nightPerDay = 10 * 3600 + 5 * 60
        summary.sleep.napsPerDay = 4 * 3600 + 15 * 60
        summary.sleep.napCountPerDay = 3.4
        summary.sleep.bars = [InsightBar(start: Date(timeIntervalSince1970: 1_785_000_000), value: 14 * 3600, secondary: 4 * 3600, isToday: false)]
        let section = InsightsPresentation(summary: summary, volumeUnit: .millilitres).sections[1]
        #expect(section.headline == "14h 20m")
        #expect(section.unit == "/ day")
        #expect(section.change?.value == "▲ 25m")
        #expect(section.rows.map(\.label) == ["Longest stretch", "Night · naps", "Naps / day"])
        #expect(section.rows[1].value == "10h 5m · 4h 15m")
        #expect(section.bars[0].primary == 10)    // night hours
        #expect(section.bars[0].secondary == 4)   // nap hours
        #expect(section.bars[0].callout.contains("Night 10h · naps 4h"))
    }

    @Test func todayBarSaysSoFarAndAxisEndsAtToday() {
        var summary = InsightsSummary(range: .week)
        summary.diapers.perDay = InsightAverage(value: 7, comparison: .coverage(days: 1))
        summary.diapers.bars = [
            InsightBar(start: Date(timeIntervalSince1970: 1_785_000_000), value: 7, secondary: nil, isToday: false),
            InsightBar(start: Date(timeIntervalSince1970: 1_785_086_400), value: 2, secondary: nil, isToday: true),
        ]
        let section = InsightsPresentation(summary: summary, volumeUnit: .millilitres).sections[2]
        #expect(section.axisEnd == "Today")
        #expect(section.bars[1].valueText == "2, so far")
        #expect(section.gridlines == [4, 8])
        #expect(section.average == 7)
    }

    // Review focus 5.
    @Test func ounceComparisonsRoundToTheDisplayedTenth() {
        var summary = InsightsSummary(range: .week)
        summary.feeding.perDay = InsightAverage(value: 8, comparison: .change(0))
        summary.feeding.bottlePerDayML = 410
        let oz = InsightsPresentation(summary: summary, volumeUnit: .ounces).sections[0]
        #expect(oz.rows.last?.value == "13.9 oz")
        #expect(InsightsPresentation.volumeChange(1, unit: .ounces) == "≈")
        #expect(InsightsPresentation.volumeChange(-30, unit: .ounces) == "▼ 1 oz")
        #expect(InsightsPresentation.volumeChange(0.4, unit: .millilitres) == "≈")
    }
}
```

The test target runs on macOS in the developer's locale. Set the language explicitly so the number and date formats above are stable: add `@Suite(.serialized)` and run with `LANG=en_US.UTF-8`. All formatting in the presentation uses `Locale.current`. If the developer locale is not English, run `defaults read -g AppleLocale` and use `en_US` for the run. Document this in the task notes. `Volume.formatted` writes the unit as `ml` / `oz` (`VolumeUnit.shortName`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter InsightsPresentationTests`
Expected: the build fails with `cannot find 'InsightsPresentation'`.

- [ ] **Step 3: Implement** `InsightsPresentation.swift`:

```swift
import Foundation
import BabyCore
import BabyUI

struct ChangeText: Equatable, Sendable {
    let value: String
    let caption: String
}

struct InsightRow: Identifiable, Equatable, Sendable {
    let id: String
    let label: String
    let value: String
    /// Fractions 0…1 for the thin split bar; empty for plain rows.
    let shares: [Double]
}

/// One bar in chart units (counts, or hours for sleep), with its words.
struct ChartBar: Identifiable, Equatable, Sendable {
    let id: String
    let primary: Double
    let secondary: Double
    let isEmpty: Bool
    let isToday: Bool
    let label: String
    let valueText: String
    let callout: String
}

struct InsightSection: Identifiable, Equatable, Sendable {
    enum Kind: String, Sendable { case feeding, sleep, diapers }
    let kind: Kind
    let tracker: Tracker
    let title: String
    let headline: String?
    let unit: String
    let change: ChangeText?
    let rows: [InsightRow]
    let bars: [ChartBar]
    /// The period average in chart units, for the dashed rule.
    let average: Double?
    let gridlines: [Double]
    let gridLabels: [String]
    let axisStart: String
    let axisEnd: String
    let accessibilityLabel: String
    var id: String { kind.rawValue }
}

/// Everything Insights says, worded. Factual, never evaluative: a change is
/// an arrow and an amount, in grey.
struct InsightsPresentation {
    let summary: InsightsSummary
    let volumeUnit: VolumeUnit

    var sections: [InsightSection] { [feeding, sleep, diapers] }

    // MARK: Sections

    private var feeding: InsightSection {
        let f = summary.feeding
        var rows: [InsightRow] = []
        if f.perDay != nil {
            if let v = f.betweenFeeds { rows.append(row("between", L10n.tr("Between feeds"), duration(v))) }
            if let v = f.longestGap { rows.append(row("gap", L10n.tr("Longest gap"), duration(v))) }
            let kinds = [(L10n.tr("Breast"), f.breastOnly), (L10n.tr("Both"), f.both), (L10n.tr("Bottle"), f.bottleOnly)]
                .filter { $0.1 > 0 }
            if !kinds.isEmpty {
                let shares = Self.percentShares(kinds.map(\.1))
                rows.append(InsightRow(id: "split", label: kinds.map(\.0).joined(separator: " · "),
                                       value: shares.map(Self.percent).joined(separator: " · "),
                                       shares: shares.map { Double($0) / 100 }))
            }
            if f.leftSeconds + f.rightSeconds > 0 {
                let shares = Self.percentShares([f.leftSeconds, f.rightSeconds])
                rows.append(row("sides", L10n.tr("Left · right"), shares.map(Self.percent).joined(separator: " · ")))
            }
            if let ml = f.bottlePerDayML { rows.append(row("bottle", L10n.tr("Bottle / day"), volume(ml))) }
        }
        return section(.feeding, tracker: .feed, title: L10n.tr("Feeds"), unit: L10n.tr("feeds / day"),
                       average: f.perDay, bars: f.bars, rows: rows, scale: 1,
                       headline: Self.decimal, change: Self.countChange)
    }

    private var sleep: InsightSection {
        let s = summary.sleep
        var rows: [InsightRow] = []
        if s.perDay != nil {
            if let v = s.longestStretch { rows.append(row("stretch", L10n.tr("Longest stretch"), duration(Double(v)))) }
            if let night = s.nightPerDay, let naps = s.napsPerDay {
                rows.append(row("nightNaps", L10n.tr("Night · naps"), "\(duration(night)) · \(duration(naps))"))
            }
            if let v = s.napCountPerDay, v > 0 { rows.append(row("naps", L10n.tr("Naps / day"), Self.decimal(v))) }
            if let v = s.awakeBetween { rows.append(row("awake", L10n.tr("Awake between sleeps"), duration(v))) }
        }
        return section(.sleep, tracker: .sleep, title: L10n.tr("Sleep"), unit: L10n.tr("/ day"),
                       average: s.perDay, bars: s.bars, rows: rows, scale: 1.0 / 3600,
                       headline: { duration($0) }, change: Self.durationChange)
    }

    private var diapers: InsightSection {
        section(.diapers, tracker: .diaper, title: L10n.tr("Diapers"), unit: L10n.tr("diapers / day"),
                average: summary.diapers.perDay, bars: summary.diapers.bars, rows: [], scale: 1,
                headline: Self.decimal, change: Self.countChange)
    }

    private func section(_ kind: InsightSection.Kind, tracker: Tracker, title: String, unit: String,
                         average: InsightAverage?, bars: [InsightBar], rows: [InsightRow], scale: Double,
                         headline: (Double) -> String, change: (Double) -> String) -> InsightSection {
        let changeText = average.map { changeText($0.comparison, change: change) }
        let chartBars = bars.map { chartBar($0, kind: kind, scale: scale, headline: headline) }
        let top = Self.niceTop(chartBars.map { $0.primary + $0.secondary }.max() ?? 0)
        let gridLabel: (Double) -> String = kind == .sleep ? { duration($0 * 3600) } : Self.decimal
        let label: String
        if let average {
            label = L10n.fmt("%1$@: %2$@ %3$@.", title, headline(average.value), unit) + " " + spoken(average.comparison, change: change)
        } else {
            label = "\(title). \(L10n.tr("Nothing logged in this period."))"
        }
        return InsightSection(
            kind: kind, tracker: tracker, title: title,
            headline: average.map { headline($0.value) }, unit: unit, change: changeText,
            rows: rows, bars: chartBars, average: average.map { $0.value * scale },
            gridlines: [top / 2, top], gridLabels: [gridLabel(top / 2), gridLabel(top)],
            axisStart: bars.first.map { axisLabel($0.start) } ?? "",
            axisEnd: summary.range == .all ? (bars.last.map { axisLabel($0.start) } ?? "") : L10n.tr("Today"),
            accessibilityLabel: label
        )
    }

    // MARK: Pieces

    private func row(_ id: String, _ label: String, _ value: String) -> InsightRow {
        InsightRow(id: id, label: label, value: value, shares: [])
    }

    private func chartBar(_ bar: InsightBar, kind: InsightSection.Kind, scale: Double,
                          headline: (Double) -> String) -> ChartBar {
        let naps = (bar.secondary ?? 0)
        let value = bar.value ?? 0
        let label = bar.isToday ? L10n.tr("Today") : barLabel(bar.start)
        var valueText = bar.value.map(headline) ?? "—"
        if bar.isToday { valueText = L10n.fmt("%@, so far", valueText) }
        var callout = "\(label) · \(valueText)"
        if kind == .sleep, bar.value != nil {
            callout += "\n" + L10n.fmt("Night %1$@ · naps %2$@", duration(value - naps), duration(naps))
        }
        return ChartBar(id: bar.start.ISO8601Format(), primary: (kind == .sleep ? value - naps : value) * scale,
                        secondary: kind == .sleep ? naps * scale : 0, isEmpty: bar.value == nil,
                        isToday: bar.isToday, label: label, valueText: valueText, callout: callout)
    }

    private func changeText(_ comparison: InsightComparison, change: (Double) -> String) -> ChangeText {
        switch comparison {
        case .change(let delta):
            return ChangeText(value: change(delta),
                              caption: summary.range == .month ? L10n.tr("vs prev. month") : L10n.tr("vs prev. week"))
        case .coverage(let days):
            return ChangeText(value: days == 1 ? L10n.tr("1 day") : L10n.fmt("%lld days", days), caption: "")
        case .since(let date):
            return ChangeText(value: L10n.fmt("since %@", axisLabel(date)), caption: "")
        }
    }

    private func spoken(_ comparison: InsightComparison, change: (Double) -> String) -> String {
        let month = summary.range == .month
        switch comparison {
        case .change(let delta):
            let text = change(delta)
            if text == "≈" {
                return month ? L10n.tr("About the same as the previous month.") : L10n.tr("About the same as the previous week.")
            }
            let amount = String(text.dropFirst(2))
            if delta > 0 {
                return L10n.fmt(month ? "%@ more than the previous month." : "%@ more than the previous week.", amount)
            }
            return L10n.fmt(month ? "%@ less than the previous month." : "%@ less than the previous week.", amount)
        case .coverage(let days): return L10n.fmt("Average of %lld days.", days)
        case .since(let date): return L10n.fmt("Since %@.", axisLabel(date))
        }
    }

    private func barLabel(_ date: Date) -> String {
        summary.range == .all ? axisLabel(date) : date.formatted(.dateTime.weekday(.abbreviated).day().month(.abbreviated))
    }

    private func axisLabel(_ date: Date) -> String {
        summary.range == .all ? date.formatted(.dateTime.day().month(.abbreviated)) : date.formatted(.dateTime.weekday(.abbreviated).day())
    }

    private func duration(_ seconds: Double) -> String { ElapsedFormatter.total(Int(seconds.rounded())) }

    private func volume(_ ml: Double) -> String { Self.volumeText(ml, unit: volumeUnit) }

    // MARK: Formatting rules (static so tests can pin them)

    static func decimal(_ value: Double) -> String {
        value.formatted(.number.precision(.fractionLength(0...1)))
    }

    static func percent(_ value: Int) -> String {
        (Double(value) / 100).formatted(.percent.precision(.fractionLength(0)))
    }

    /// Counts are shown to a tenth, so a change that rounds to 0.0 is "≈".
    static func countChange(_ delta: Double) -> String {
        let rounded = (delta * 10).rounded() / 10
        guard rounded != 0 else { return "≈" }
        return (rounded > 0 ? "▲ " : "▼ ") + decimal(abs(rounded))
    }

    /// Durations are shown in whole minutes.
    static func durationChange(_ delta: Double) -> String {
        guard abs(delta) >= 60 else { return "≈" }
        return (delta > 0 ? "▲ " : "▼ ") + ElapsedFormatter.total(Int(abs(delta)))
    }

    static func volumeText(_ ml: Double, unit: VolumeUnit) -> String {
        unit == .millilitres
            ? Volume(millilitres: ml.rounded()).formatted(as: unit)
            : Volume(millilitres: ml).formatted(as: unit)
    }

    /// Whole ml, or a tenth of an ounce.
    static func volumeChange(_ deltaML: Double, unit: VolumeUnit) -> String {
        let shown = unit == .millilitres ? deltaML.rounded() : (Volume(millilitres: deltaML).ounces * 10).rounded() / 10
        guard shown != 0 else { return "≈" }
        return (deltaML > 0 ? "▲ " : "▼ ") + volumeText(abs(deltaML), unit: unit)
    }

    /// Whole percentages that sum to 100 (largest remainder).
    static func percentShares(_ parts: [Int]) -> [Int] {
        let total = parts.reduce(0, +)
        guard total > 0 else { return parts.map { _ in 0 } }
        let exact = parts.map { Double($0) * 100 / Double(total) }
        var shares = exact.map { Int($0) }
        let order = exact.indices.sorted { exact[$0] - Double(shares[$0]) > exact[$1] - Double(shares[$1]) }
        for i in order.prefix(100 - shares.reduce(0, +)) { shares[i] += 1 }
        return shares
    }

    /// An even whole number at or above the tallest bar, at least 2.
    static func niceTop(_ value: Double) -> Double {
        let up = max(2, value.rounded(.up))
        return up.truncatingRemainder(dividingBy: 2) == 0 ? up : up + 1
    }
}
```

Add to `InsightsViewModel`:

```swift
    var sections: [InsightSection] { InsightsPresentation(summary: summary, volumeUnit: volumeUnit).sections }
```

The `volumeChange` helper is used where a volume comparison is shown. Insights has no volume headline, so the comparison only appears in the presentation tests. It stays because the formatting rule must hold if a volume comparison is added. If `simplify` review flags it as unused, remove both it and its test line.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd Packages/BabyTracker && LANG=en_US.UTF-8 swift test --filter InsightsPresentationTests`
Expected: PASS. If a string differs only by locale formatting (non-breaking spaces in percentages, for example), fix the expectation to the `Locale(identifier: "en_US")` output, not the implementation.

- [ ] **Step 5: Commit** (`make test`, bump `EXPECTED_TESTS` by 9)

```bash
git add -A Packages/BabyTracker Makefile
git commit -m "feat(insights): word the summary — headlines, neutral changes, rows, bars and spoken text

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Growth section in GrowthFeature; delete the Growth details screen

**Files:**
- Create: `Packages/BabyTracker/Sources/GrowthFeature/GrowthPresentation.swift`, moving `GrowthCopy`, `GrowthReferenceBand` and `GrowthMetricPresentation` out of `GrowthView.swift` and adding `GrowthSectionPresentation`.
- Create: `Packages/BabyTracker/Sources/GrowthFeature/GrowthSectionView.swift`
- Delete: `Packages/BabyTracker/Sources/GrowthFeature/GrowthView.swift`, `Packages/BabyTracker/Sources/GrowthFeature/GrowthCardView.swift`
- Modify: `Packages/BabyTracker/Package.swift`. Remove `"LoggingFeature"` from GrowthFeature's dependencies if `grep -rn "import LoggingFeature" Packages/BabyTracker/Sources/GrowthFeature` prints nothing after the deletes.
- Rename and rewrite: `Tests/GrowthFeatureTests/GrowthViewTests.swift` → `GrowthSectionTests.swift`

**Interfaces:**
- Consumes: `GrowthViewModel` (`summaries`, `profileState`, `birthDate`, `checkInCount`) and `GrowthReferenceInfoView`.
- Produces:
  - `public struct GrowthSectionView: View`, initialised with `init(model: GrowthViewModel, store: BabyStore, babyID: UUID, onOpenAllCheckIns: @escaping () -> Void)`.
  - `struct GrowthSectionPresentation`, with `chip(_:) -> GrowthChip`, `changeLine(_:) -> String?`, `headerText: String?` and `isEmpty: Bool`.
  - `struct GrowthReferenceChoice`, replacing `GrowthRecordActions`, with `choose(_:)` and `errorMessage`.
  - `GrowthCopy.format(_ key: String, _ values: CVarArg...)`.
  - Accessibility identifiers:
    - `insights.growth`
    - `insights.growth.chip.<metric rawValue>`
    - `insights.growth.reference.info`
    - `insights.growth.reference.girls` / `.boys`
    - `insights.growth.allCheckIns`

- [ ] **Step 1: Write the failing tests.**
  1. `git mv Packages/BabyTracker/Tests/GrowthFeatureTests/GrowthViewTests.swift Packages/BabyTracker/Tests/GrowthFeatureTests/GrowthSectionTests.swift`.
  2. Rename the suite to `GrowthSectionTests`.
  3. Keep these unchanged:
     - `availableSyntheticReferenceHasPercentileAndOnlyContiguousBands`, without its `GrowthMetricCard` render lines;
     - `swissRankRoundingNeverTurnsAnInteriorRankIntoATail`;
     - `headPresentationRetainsChartAndExposesReleaseScope`, without its card render;
     - `displayedSwissCopyIsLocalizedInEverySupportedLocale`.
  4. In `sparsePresentationAndAccessibility`, delete the `latestText`, `chartMessage` and `changeText` expectations; those members go away.
  5. Replace `recordActionsResolveEditDeleteAndLegacyChoice`, `realmSwitchRejectsPendingActions` and `metricCardsRenderAtCompactWidthAndAccessibilitySize` with the tests below.

  Edit and delete now live in History; their store behaviour is covered by HistoryFeature and LoggingFeature tests (`MeasurementSheet` edit mode).

```swift
    private func point(_ value: Double, daysAgo: Double, percentile: Double? = 48) -> GrowthPoint {
        GrowthPoint(id: UUID(), measuredAt: Date(timeIntervalSince1970: 1_790_000_000 - daysAgo * 86_400), timeZoneID: "UTC",
                    value: value, position: nil,
                    reference: percentile.map { .calculated(.available(percentile: $0, median: 5, p3: 3, p97: 7, correctedAge: false)) }
                        ?? .calculated(.unavailable(.noReferenceForMetric)))
    }

    @Test func chipsShowLatestValueAndShortPercentile() {
        let presentation = GrowthSectionPresentation(summaries: [
            .init(metric: .weight, points: [point(5.62, daysAgo: 0, percentile: 48.4), point(5.2, daysAgo: 14)]),
            .init(metric: .lengthHeight, points: [point(58.5, daysAgo: 0, percentile: 1.2)]),
            .init(metric: .headCircumference, points: [point(39, daysAgo: 0, percentile: nil)]),
        ], birthDate: nil)
        // In GrowthViewModel a head reading is always `.noReferenceForMetric`; the helper builds the same.
        #expect(presentation.chip(.weight).percentile == "P48")
        #expect(presentation.chip(.lengthHeight).percentile == "<P3")
        #expect(presentation.chip(.headCircumference).percentile == "—")
        #expect(presentation.chip(.weight).value == "5.62 kg")
    }

    @Test func missingProfileChipHasNoPercentileAndNoMeasurementShowsADash() {
        let missing = GrowthPoint(id: UUID(), measuredAt: .now, timeZoneID: "UTC", value: 4, position: nil, reference: .missingProfile)
        let presentation = GrowthSectionPresentation(summaries: [.init(metric: .weight, points: [missing])], birthDate: nil)
        #expect(presentation.chip(.weight).percentile == nil)
        #expect(presentation.chip(.lengthHeight).value == "—")
    }

    @Test func weightChangeLineHasGramsPerDayAndLengthOnlyItsChange() throws {
        let presentation = GrowthSectionPresentation(summaries: [
            .init(metric: .weight, points: [point(5.62, daysAgo: 0), point(5.2, daysAgo: 14)]),
            .init(metric: .lengthHeight, points: [point(58.5, daysAgo: 0), point(56, daysAgo: 14)]),
        ], birthDate: nil)
        let weight = try #require(presentation.changeLine(.weight))
        #expect(weight.hasPrefix("+420 g since "))
        #expect(weight.hasSuffix("≈ 30 g / day"))
        let length = try #require(presentation.changeLine(.lengthHeight))
        #expect(length.hasPrefix("+2.5 cm since "))
        #expect(!length.contains("/ day"))
        #expect(presentation.changeLine(.headCircumference) == nil)
    }

    @Test func headerShowsTheNewestCheckInAndAgeThen() throws {
        let birth = Date(timeIntervalSince1970: 1_790_000_000 - 63 * 86_400)
        let presentation = GrowthSectionPresentation(summaries: [.init(metric: .weight, points: [point(5.62, daysAgo: 0)])], birthDate: birth)
        let header = try #require(presentation.headerText)
        #expect(header.contains("9"))   // nine weeks
        #expect(GrowthSectionPresentation(summaries: [], birthDate: birth).isEmpty)
    }

    @Test func referenceChoiceKeepsADueDateAndRejectsAfterARealmSwitch() throws {
        let (store, baby, _) = try fixture()
        let profile = try #require(try store.growthProfile(for: baby.id))
        store.context.delete(profile)
        try store.context.save()
        let due = try LocalDate("2023-12-01")
        try store.setGrowthProfile(for: baby.id, referenceSex: .girls, expectedDueDate: due)
        let choice = GrowthReferenceChoice(store: store, babyID: baby.id)
        choice.choose(.boys)
        #expect(try store.growthProfile(for: baby.id)?.referenceSex == .boys)
        #expect(try store.growthProfile(for: baby.id)?.expectedDueDate == due)
        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        choice.choose(.girls)
        #expect(choice.errorMessage != nil)
    }

    @Test func sectionRendersAtCompactWidthAndAccessibilitySize() throws {
        let (store, baby, now) = try fixture()
        let model = GrowthViewModel(store: store, babyID: baby.id)
        for count in 0...2 {
            if count > 0 {
                try store.logMeasurement(for: baby.id, at: now.addingTimeInterval(Double(-count) * 86_400), timeZoneID: "UTC",
                                         values: MeasurementValues(weightKG: Double(count + 3), lengthCM: 50 + Double(count), lengthPosition: .lying))
            }
            try model.load()
            var heights: [Int] = []
            for size in [DynamicTypeSize.large, .accessibility5] {
                let view = NavigationStack {
                    GrowthSectionView(model: model, store: store, babyID: baby.id, onOpenAllCheckIns: {})
                }
                .environment(\.dynamicTypeSize, size).environment(\.colorScheme, .dark)
                .frame(width: 320).background(Palette.ground)
                let image = try #require(ImageRenderer(content: view).cgImage)
                #expect(image.width == 320)
                heights.append(image.height)
            }
            #if os(iOS)
            #expect(heights[1] > heights[0])
            #endif
        }
    }
```

(`fixture()` stays as in the old file.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter GrowthFeatureTests`
Expected: the build fails (`GrowthSectionPresentation`, `GrowthSectionView` and `GrowthReferenceChoice` are not in scope).

- [ ] **Step 3: Implement.**

`GrowthPresentation.swift`:
- Move `GrowthCopy`, `GrowthReferenceBand` and `GrowthMetricPresentation` here verbatim, then delete `latestText`, `chartMessage` and `changeText` from `GrowthMetricPresentation`.
- Change `GrowthCopy.format` to `static func format(_ key: String, _ values: CVarArg...) -> String { String(format: text(key), locale: Locale.current, arguments: values) }`.
- Add:

```swift
struct GrowthChip: Equatable {
    let metric: GrowthMetric
    let title: String
    let value: String
    /// "P48", "<P3", ">P97", "—" for no reference; nil when the girls/boys
    /// reference has not been chosen.
    let percentile: String?
}

/// The Insights growth section's words. Growth ignores the range picker.
struct GrowthSectionPresentation {
    let summaries: [GrowthMetricSummary]
    let birthDate: Date?

    func summary(_ metric: GrowthMetric) -> GrowthMetricSummary {
        summaries.first { $0.metric == metric } ?? GrowthMetricSummary(metric: metric, points: [])
    }

    var isEmpty: Bool { summaries.allSatisfy { $0.points.isEmpty } }

    func chip(_ metric: GrowthMetric) -> GrowthChip {
        let summary = summary(metric)
        let display = GrowthMetricPresentation(summary: summary)
        let title = switch metric {
        case .weight: GrowthCopy.text("Weight")
        case .lengthHeight: GrowthCopy.text("Length")
        case .headCircumference: GrowthCopy.text("Head")
        }
        return GrowthChip(metric: metric, title: title,
                          value: summary.latest.map { display.valueText($0.value) } ?? "—",
                          percentile: summary.latest.flatMap(Self.shortReference) ?? (summary.latest == nil ? "—" : nil))
    }

    static func shortReference(_ point: GrowthPoint) -> String? {
        switch point.reference {
        case .missingProfile: return nil
        case let .calculated(.available(percentile, _, _, _, _)):
            // Tails are classified before rounding, as in the full wording.
            if percentile < 3 { return GrowthCopy.text("<P3") }
            if percentile > 97 { return GrowthCopy.text(">P97") }
            return GrowthCopy.format("P%@", percentile.formatted(.number.precision(.fractionLength(0))))
        case .calculated(.unavailable): return "—"
        }
    }

    /// "+420 g since 18 Sep · ≈ 30 g / day"; length and head give the change only.
    func changeLine(_ metric: GrowthMetric) -> String? {
        let points = summary(metric).points
        guard points.count >= 2 else { return nil }
        let latest = points[0], previous = points[1]
        let delta = latest.value - previous.value
        let zone = TimeZone(identifier: previous.timeZoneID) ?? .current
        let since = previous.measuredAt.formatted(Date.FormatStyle(timeZone: zone).day().month(.abbreviated))
        let amount = metric == .weight
            ? Self.signed(delta * 1000, unit: UnitMass.grams, fraction: 0)
            : Self.signed(delta, unit: UnitLength.centimeters, fraction: 1)
        var line = GrowthCopy.format("%1$@ since %2$@", amount, since)
        if metric == .weight {
            let days = max(latest.measuredAt.timeIntervalSince(previous.measuredAt) / 86_400, 1)
            let perDay = Measurement(value: (delta * 1000 / days).rounded(), unit: UnitMass.grams)
                .formatted(.measurement(width: .abbreviated, usage: .asProvided, numberFormatStyle: .number.precision(.fractionLength(0))))
            line += " · " + GrowthCopy.format("≈ %@ / day", perDay)
        }
        return line
    }

    /// "2 Oct · 9 wk": the newest check-in of any metric and the baby's age then.
    var headerText: String? {
        guard let latest = summaries.compactMap(\.latest).max(by: { $0.measuredAt < $1.measuredAt }) else { return nil }
        let zone = TimeZone(identifier: latest.timeZoneID) ?? .current
        let date = latest.measuredAt.formatted(Date.FormatStyle(timeZone: zone).day().month(.abbreviated))
        guard let birthDate, latest.measuredAt > birthDate else { return date }
        let formatter = DateComponentsFormatter()
        formatter.unitsStyle = .abbreviated
        formatter.maximumUnitCount = 1
        let weeks = latest.measuredAt.timeIntervalSince(birthDate) / (7 * 86_400)
        formatter.allowedUnits = weeks < 13 ? [.weekOfMonth] : [.month]
        guard let age = formatter.string(from: birthDate, to: latest.measuredAt) else { return date }
        return "\(date) · \(age)"
    }

    private static func signed(_ value: Double, unit: Dimension, fraction: Int) -> String {
        let magnitude = Measurement(value: abs(value), unit: unit)
            .formatted(.measurement(width: .abbreviated, usage: .asProvided,
                                    numberFormatStyle: .number.precision(.fractionLength(0...fraction))))
        return (value < 0 ? "−" : "+") + magnitude
    }
}
```

`GrowthSectionView.swift`:

```swift
import SwiftUI
import Charts
import Observation
import BabyCore
import BabyStore
import BabyUI

/// The reference choice that used to live on the Growth details screen.
@MainActor @Observable
final class GrowthReferenceChoice {
    private let store: BabyStore
    private let babyID: UUID
    private let writeContext: RealmWriteContext?
    var errorMessage: String?

    init(store: BabyStore, babyID: UUID) {
        self.store = store
        self.babyID = babyID
        writeContext = try? store.captureWriteContext()
    }

    func choose(_ sex: GrowthReferenceSex) {
        do {
            guard let writeContext, try store.captureWriteContext() == writeContext else {
                throw GrowthStoreError.babyUnavailable
            }
            // Preserve a due date another screen may have set meanwhile.
            let due = try store.growthProfile(for: babyID)?.expectedDueDate
            try store.setGrowthProfile(for: babyID, referenceSex: sex, expectedDueDate: due)
            errorMessage = nil
        } catch {
            errorMessage = GrowthCopy.text("Couldn’t save the reference. Try again.")
        }
    }
}

/// Insights' growth section: the latest weight, length and head with their
/// Swiss percentiles, one chart from birth, and the way to the check-ins.
public struct GrowthSectionView: View {
    private let model: GrowthViewModel
    private let onOpenAllCheckIns: () -> Void
    @State private var choice: GrowthReferenceChoice
    @State private var metric: GrowthMetric = .weight
    @State private var selected: Date?
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(model: GrowthViewModel, store: BabyStore, babyID: UUID, onOpenAllCheckIns: @escaping () -> Void) {
        self.model = model
        self.onOpenAllCheckIns = onOpenAllCheckIns
        _choice = State(initialValue: GrowthReferenceChoice(store: store, babyID: babyID))
    }

    private var presentation: GrowthSectionPresentation {
        GrowthSectionPresentation(summaries: model.summaries, birthDate: model.birthDate)
    }

    public var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .firstTextBaseline) {
                TrackerIcon(.growth).foregroundStyle(Palette.growth).accessibilityHidden(true)
                Spacer(minLength: 8)
                if let header = presentation.headerText {
                    Text(header).font(.footnote).foregroundStyle(Palette.secondaryText)
                }
            }
            if presentation.isEmpty {
                Text(GrowthCopy.text("No measurements yet")).font(.footnote).foregroundStyle(Palette.secondaryText)
            } else {
                chips
                if model.profileState == .missing { referenceChoice }
                if let line = presentation.changeLine(metric) {
                    Text(line).font(.footnote.monospacedDigit()).foregroundStyle(Palette.primaryText.opacity(0.85))
                }
                let summary = presentation.summary(metric)
                if summary.chartPoints.isEmpty {
                    Text(GrowthCopy.text("A trend needs two measurements.")).font(.footnote).foregroundStyle(Palette.secondaryText)
                } else {
                    GrowthChart(display: GrowthMetricPresentation(summary: summary), selected: $selected).frame(height: 150)
                }
            }
            NavigationLink { GrowthReferenceInfoView() } label: {
                Label(GrowthCopy.text("Swiss PEZZ 2025 reference"), systemImage: "info.circle")
                    .font(.footnote).frame(minHeight: 44)
            }
            .accessibilityIdentifier("insights.growth.reference.info")
            if model.checkInCount > 0 {
                Button(action: onOpenAllCheckIns) {
                    HStack {
                        Text(GrowthCopy.text("All check-ins")).foregroundStyle(Palette.secondaryText)
                        Spacer(minLength: 8)
                        Text("\(model.checkInCount)").foregroundStyle(Palette.secondaryText)
                        Image(systemName: "chevron.right").font(.footnote.weight(.semibold)).foregroundStyle(Palette.secondaryText)
                    }
                    .font(.footnote).frame(minHeight: 44).contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityIdentifier("insights.growth.allCheckIns")
            }
            if let message = choice.errorMessage {
                Text(message).font(.footnote).foregroundStyle(Palette.destructive)
            }
        }
        .onChange(of: metric) { _, _ in selected = nil }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.white.opacity(0.055), in: .rect(cornerRadius: 18))
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("insights.growth")
    }

    private var chips: some View {
        let layout = typeSize.isAccessibilitySize
            ? AnyLayout(VStackLayout(spacing: 6)) : AnyLayout(HStackLayout(spacing: 6))
        return layout {
            ForEach(GrowthMetric.allCases, id: \.self) { item in
                let chip = presentation.chip(item)
                Button { metric = item } label: {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(chip.title).font(.footnote.weight(.semibold)).foregroundStyle(Palette.secondaryText)
                        Text(chip.value).font(.body.weight(.bold).monospacedDigit()).foregroundStyle(Palette.primaryText)
                        if let percentile = chip.percentile {
                            Text(percentile).font(.footnote.weight(.bold)).foregroundStyle(Palette.growth)
                        }
                    }
                    .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                    .padding(8)
                    .background(item == metric ? Palette.growth.opacity(0.14) : .white.opacity(0.05), in: .rect(cornerRadius: 12))
                    .overlay {
                        if item == metric { RoundedRectangle(cornerRadius: 12).stroke(Palette.growth.opacity(0.6), lineWidth: 1.5) }
                    }
                }
                .buttonStyle(.plain)
                .accessibilityLabel(chipLabel(item, chip))
                .accessibilityAddTraits(item == metric ? .isSelected : [])
                .accessibilityIdentifier("insights.growth.chip." + item.rawValue)
            }
        }
    }

    private func chipLabel(_ metric: GrowthMetric, _ chip: GrowthChip) -> String {
        let summary = presentation.summary(metric)
        guard let latest = summary.latest else { return "\(chip.title). \(GrowthCopy.text("No measurements yet"))" }
        return GrowthMetricPresentation(summary: summary).accessibilityLabel(for: latest)
    }

    private var referenceChoice: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(GrowthCopy.text("Choose the girls’ or boys’ reference for comparisons. Your recorded measurements stay available."))
                .font(.footnote).fixedSize(horizontal: false, vertical: true)
            HStack {
                Button(GrowthCopy.text("Girls")) { choice.choose(.girls) }
                    .accessibilityIdentifier("insights.growth.reference.girls")
                Button(GrowthCopy.text("Boys")) { choice.choose(.boys) }
                    .accessibilityIdentifier("insights.growth.reference.boys")
            }
            .buttonStyle(.bordered).controlSize(.large)
        }
    }
}

/// The baby's readings over the Swiss band and median. Moved from the old
/// details screen; selecting a point shows its date, value and percentile.
struct GrowthChart: View {
    let display: GrowthMetricPresentation
    @Binding var selected: Date?

    private var selectedPoint: GrowthPoint? {
        guard let selected else { return nil }
        return display.chartPoints.min {
            abs($0.measuredAt.timeIntervalSince(selected)) < abs($1.measuredAt.timeIntervalSince(selected))
        }
    }

    var body: some View {
        Chart {
            ForEach(display.referenceBands) { band in
                ForEach(band.points) { point in
                    if case let .calculated(.available(_, median, p3, p97, _)) = point.reference {
                        AreaMark(x: .value(GrowthCopy.text("Date"), point.measuredAt),
                                 yStart: .value("P3", p3), yEnd: .value("P97", p97),
                                 series: .value("Reference", band.id.uuidString))
                            .foregroundStyle(Palette.sleep.opacity(0.16))
                        LineMark(x: .value(GrowthCopy.text("Date"), point.measuredAt),
                                 y: .value(GrowthCopy.text("Swiss median"), median),
                                 series: .value("Reference", band.id.uuidString))
                            .foregroundStyle(Palette.sleep).lineStyle(StrokeStyle(lineWidth: 1, dash: [4, 4]))
                    }
                }
            }
            ForEach(display.chartPoints) { point in
                LineMark(x: .value(GrowthCopy.text("Date"), point.measuredAt),
                         y: .value(display.title, point.value), series: .value("Measurements", "raw"))
                    .foregroundStyle(Palette.growth)
                PointMark(x: .value(GrowthCopy.text("Date"), point.measuredAt), y: .value(display.title, point.value))
                    .foregroundStyle(Palette.growth)
                    .accessibilityLabel(display.dateText(point))
                    .accessibilityValue("\(display.valueText(point.value)). \(display.referenceText(point))")
            }
            if let point = selectedPoint {
                RuleMark(x: .value(GrowthCopy.text("Date"), point.measuredAt))
                    .foregroundStyle(Palette.secondaryText.opacity(0.4))
                    .annotation(position: .top, overflowResolution: .init(x: .fit(to: .chart), y: .disabled)) {
                        Text("\(display.dateText(point)) · \(display.valueText(point.value))\n\(display.referenceText(point))")
                            .font(.footnote).padding(6)
                            .background(Palette.ground.opacity(0.95), in: .rect(cornerRadius: 8))
                    }
            }
        }
        .chartXSelection(value: $selected)
        .chartYScale(domain: .automatic(includesZero: false))
        .chartXAxis { AxisMarks(values: .automatic(desiredCount: 3)) }
        .chartYAxis { AxisMarks(position: .leading, values: .automatic(desiredCount: 3)) }
        .accessibilityIdentifier("insights.growth.chart")
    }
}
```

Delete `GrowthView.swift` and `GrowthCardView.swift`. Make `GrowthReferenceInfoView` keep its `internal` access; it is used only from this module. Point any remaining reference to `GrowthCardView`/`GrowthView` in `InsightsView.swift` at `GrowthSectionView(model: model.growth, store: store, babyID: babyID, onOpenAllCheckIns: {})`. Task 7 replaces the view and Task 8 wires the callback.

Strings this task introduces: `Length`, `Head`, `P%@`, `<P3`, `>P97`, `%1$@ since %2$@`, `≈ %@ / day` and `All check-ins`.

`displayedSwissCopyIsLocalizedInEverySupportedLocale` requires every GrowthFeature key in six locales. Also, with no catalog entry the new strings would fall back to English. **In this task:**
1. Create `work/insights-strings.py` exactly as written in Task 9 Step 2.
2. Run `python3 work/insights-strings.py GrowthFeature`.
3. Review its `removed` list: it should hold only keys of the deleted screen.
4. Commit the GrowthFeature catalog with this task.

Task 9 then runs the script for InsightsFeature.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd Packages/BabyTracker && LANG=en_US.UTF-8 swift test --filter "GrowthFeatureTests|InsightsFeatureTests"`
Expected: PASS.

- [ ] **Step 5: Commit** (`make test`, set `EXPECTED_TESTS`)

```bash
git add -A Packages/BabyTracker Makefile    # includes the GrowthFeature catalog
git commit -m "feat(growth): growth section for Insights; remove the Growth details screen

Deletes recordActionsResolveEditDeleteAndLegacyChoice and
metricCardsRenderAtCompactWidthAndAccessibilitySize with the screen they
tested; editing check-ins is History's (MeasurementSheet edit mode).

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: The Insights screen

**Files:**
- Create: `Packages/BabyTracker/Sources/InsightsFeature/InsightSectionCard.swift`
- Rewrite: `Packages/BabyTracker/Sources/InsightsFeature/InsightsView.swift`
- Test: `Packages/BabyTracker/Tests/InsightsFeatureTests/InsightsViewRenderTests.swift`

**Interfaces:**
- Consumes: `InsightsViewModel.sections`, `.growth` and `.range`; `GrowthSectionView`.
- Produces:
  - `public struct InsightsView`, initialised with `init(store:babyID:babyName:volumeUnit:onSwitchBaby:onOpenAllCheckIns:)`. There is no focus: Ask was its only caller.
  - Accessibility identifiers:
    - `insights.range`
    - `insights.babySwitcher` / `insights.babyName`
    - `insights.section.feeding|sleep|diapers`
    - `insights.chart.<kind>`

- [ ] **Step 1: Write the failing tests.**

`InsightsViewRenderTests.swift`:

```swift
import Foundation
import SwiftUI
import Testing
import BabyCore
import BabyStore
import BabyUI
@testable import InsightsFeature

@MainActor @Suite struct InsightsViewRenderTests {
    @Test func screenRendersAtCompactWidthAndAccessibilitySize() throws {
        let now = Calendar.current.date(from: DateComponents(year: 2026, month: 6, day: 15, hour: 12))!
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
        let baby = try store.addBaby(name: "Mira", birthDate: now.addingTimeInterval(-60 * 86_400), referenceSex: .girls, expectedDueDate: nil)
        for day in 1...7 {
            let when = now.addingTimeInterval(-86_400 * Double(day))
            _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id, at: when)
            _ = try store.logSleep(from: when.addingTimeInterval(3600), to: when.addingTimeInterval(7200), for: baby.id)
            _ = try store.logDiaper(.wet, for: baby.id, at: when)
        }
        var heights: [Int] = []
        for size in [DynamicTypeSize.large, .accessibility5] {
            let view = InsightsView(store: store, babyID: baby.id, babyName: "Mira")
                .environment(\.dynamicTypeSize, size).environment(\.colorScheme, .dark)
                .frame(width: 390, height: 2400)
            let renderer = ImageRenderer(content: view)
            let image = try #require(renderer.cgImage)
            heights.append(image.height)
        }
        #expect(heights.count == 2)
    }

    @Test func sectionCardRendersEverySectionKind() throws {
        var summary = InsightsSummary(range: .week)
        summary.feeding.perDay = InsightAverage(value: 8.4, comparison: .change(0.6))
        summary.feeding.bars = [InsightBar(start: .now, value: 8, secondary: nil, isToday: false)]
        summary.feeding.breastOnly = 3
        for section in InsightsPresentation(summary: summary, volumeUnit: .millilitres).sections {
            for size in [DynamicTypeSize.large, .accessibility5] {
                let card = InsightSectionCard(section: section)
                    .environment(\.dynamicTypeSize, size).environment(\.colorScheme, .dark).frame(width: 360)
                #expect(ImageRenderer(content: card).cgImage != nil)
            }
        }
    }
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter InsightsViewRenderTests`
Expected: the build fails (`InsightSectionCard` doesn't exist).

- [ ] **Step 3: Implement.**

`InsightSectionCard.swift`:

```swift
import SwiftUI
import Charts
import BabyUI

/// One section: icon, per-day average and its neutral change, a bar chart,
/// and the detail rows.
struct InsightSectionCard: View {
    let section: InsightSection
    @State private var selected: String?
    @Environment(\.dynamicTypeSize) private var typeSize

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            headline
            if section.headline == nil {
                Text(L10n.tr("Nothing logged in this period."))
                    .font(.footnote).foregroundStyle(Palette.secondaryText)
            } else {
                chart
                axis
                if section.kind == .sleep { sleepKey }
                ForEach(section.rows) { rowView($0) }
            }
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.white.opacity(0.055), in: .rect(cornerRadius: 18))
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("insights.section." + section.id)
    }

    private var headline: some View {
        let layout = typeSize.isAccessibilitySize
            ? AnyLayout(VStackLayout(alignment: .leading, spacing: 4))
            : AnyLayout(HStackLayout(alignment: .firstTextBaseline, spacing: 8))
        return layout {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                TrackerIcon(section.tracker).foregroundStyle(section.tracker.color)
                if let headline = section.headline {
                    Text(headline).numeralStyle(.display).foregroundStyle(Palette.primaryText)
                    Text(section.unit).font(.subheadline.weight(.semibold)).foregroundStyle(Palette.secondaryText)
                }
            }
            if !typeSize.isAccessibilitySize { Spacer(minLength: 0) }
            if let change = section.change {
                VStack(alignment: typeSize.isAccessibilitySize ? .leading : .trailing, spacing: 0) {
                    Text(change.value).font(.footnote.monospacedDigit())
                    if !change.caption.isEmpty { Text(change.caption).font(.footnote) }
                }
                .foregroundStyle(Palette.secondaryText)
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(section.accessibilityLabel)
    }

    private var selectedBar: ChartBar? { section.bars.first { $0.id == selected && !$0.isEmpty } }

    private var chart: some View {
        Chart {
            ForEach(section.bars) { bar in
                BarMark(x: .value("Day", bar.id), y: .value(section.title, bar.primary))
                    .foregroundStyle(section.tracker.color.opacity(bar.isToday ? 0.3 : 0.75))
                    .cornerRadius(3)
                    .accessibilityLabel(bar.label)
                    .accessibilityValue(bar.valueText)
                if bar.secondary > 0 {
                    BarMark(x: .value("Day", bar.id), y: .value(L10n.tr("Naps"), bar.secondary))
                        .foregroundStyle(section.tracker.color.opacity(bar.isToday ? 0.15 : 0.35))
                        .cornerRadius(3)
                        .accessibilityHidden(true)
                }
            }
            if let average = section.average {
                RuleMark(y: .value("Average", average))
                    .lineStyle(StrokeStyle(lineWidth: 1, dash: [4, 3]))
                    .foregroundStyle(Palette.primaryText.opacity(0.4))
                    .accessibilityHidden(true)
            }
            if let bar = selectedBar {
                RuleMark(x: .value("Day", bar.id))
                    .foregroundStyle(Palette.primaryText.opacity(0.2))
                    .annotation(position: .top, overflowResolution: .init(x: .fit(to: .chart), y: .disabled)) {
                        Text(bar.callout).font(.footnote.monospacedDigit())
                            .padding(6).background(Palette.ground.opacity(0.95), in: .rect(cornerRadius: 8))
                    }
                    .accessibilityHidden(true)
            }
        }
        .chartXSelection(value: $selected)
        .chartXAxis(.hidden)
        .chartYScale(domain: 0 ... (section.gridlines.last ?? 1))
        .chartYAxis {
            AxisMarks(position: .trailing, values: section.gridlines) { value in
                AxisGridLine()
                AxisValueLabel {
                    if let v = value.as(Double.self), let i = section.gridlines.firstIndex(of: v) {
                        Text(section.gridLabels[i]).font(.footnote)
                    }
                }
            }
        }
        .frame(height: section.kind == .diapers ? 56 : 84)
        .accessibilityIdentifier("insights.chart." + section.id)
    }

    private var axis: some View {
        HStack {
            Text(section.axisStart)
            Spacer(minLength: 8)
            Text(section.axisEnd)
        }
        .font(.footnote)
        .foregroundStyle(Palette.secondaryText)
        .accessibilityHidden(true)
    }

    private var sleepKey: some View {
        HStack(spacing: 12) {
            key(L10n.tr("Night"), opacity: 0.75)
            key(L10n.tr("Naps"), opacity: 0.35)
        }
        .font(.footnote)
        .foregroundStyle(Palette.secondaryText)
        .accessibilityHidden(true)
    }

    private func key(_ title: String, opacity: Double) -> some View {
        HStack(spacing: 4) {
            RoundedRectangle(cornerRadius: 2).fill(section.tracker.color.opacity(opacity)).frame(width: 9, height: 9)
            Text(title)
        }
    }

    @ViewBuilder
    private func rowView(_ row: InsightRow) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Divider().overlay(.white.opacity(0.06))
            let layout = typeSize.isAccessibilitySize
                ? AnyLayout(VStackLayout(alignment: .leading, spacing: 2))
                : AnyLayout(HStackLayout(alignment: .firstTextBaseline))
            layout {
                Text(row.label).foregroundStyle(Palette.secondaryText)
                if !typeSize.isAccessibilitySize { Spacer(minLength: 8) }
                Text(row.value).fontWeight(.semibold).monospacedDigit().foregroundStyle(Palette.primaryText)
            }
            .font(.subheadline)
            if !row.shares.isEmpty {
                GeometryReader { geometry in
                    HStack(spacing: 0) {
                        ForEach(Array(row.shares.enumerated()), id: \.offset) { index, share in
                            Rectangle().fill(section.tracker.color.opacity([1, 0.6, 0.35][min(index, 2)]))
                                .frame(width: geometry.size.width * share)
                        }
                    }
                }
                .frame(height: 6)
                .clipShape(Capsule())
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(row.label): \(row.value)")
    }
}
```

`InsightsView.swift`. Keep the file's existing explanatory comments where the code they describe survives (the reload rules and the error banner rule).

```swift
import SwiftUI
import BabyCore
import BabyStore
import BabyUI
import GrowthFeature

public struct InsightsView: View {
    private let store: BabyStore
    private let babyID: UUID
    private let volumeUnit: VolumeUnit
    private let babyName: String
    private let onSwitchBaby: (() -> Void)?
    private let onOpenAllCheckIns: () -> Void
    @State private var model: InsightsViewModel
    @State private var range: InsightRange = .week
    @Environment(\.errorReporter) private var errorReporter
    @Environment(\.scenePhase) private var scenePhase

    public init(
        store: BabyStore,
        babyID: UUID,
        babyName: String = "",
        volumeUnit: VolumeUnit = .millilitres,
        onSwitchBaby: (() -> Void)? = nil,
        onOpenAllCheckIns: @escaping () -> Void = {}
    ) {
        self.store = store
        self.babyID = babyID
        self.volumeUnit = volumeUnit
        self.babyName = babyName
        self.onSwitchBaby = onSwitchBaby
        self.onOpenAllCheckIns = onOpenAllCheckIns
        _model = State(wrappedValue: InsightsViewModel(store: store, babyID: babyID, volumeUnit: volumeUnit))
    }

    public var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 10) {
                    ForEach(model.sections) { section in
                        InsightSectionCard(section: section)
                    }
                    GrowthSectionView(model: model.growth, store: store, babyID: babyID,
                                      onOpenAllCheckIns: onOpenAllCheckIns)
                }
                .padding(.horizontal, 12)
                .padding(.vertical, 12)
            }
            .background(Palette.ground.ignoresSafeArea())
            #if os(iOS)
            .toolbar(.hidden, for: .navigationBar)
            #endif
            .safeAreaInset(edge: .top, spacing: 0) { header }
        }
        .task { reload() }
        // Every figure is an average over entries: a write anywhere makes
        // them stale until this runs.
        .onChange(of: store.revision) { _, _ in reload() }
        .onChange(of: volumeUnit) { _, unit in
            model.volumeUnit = unit
            reload()
        }
        .onChange(of: range) { _, value in
            model.range = value
            reload()
        }
        // Coming back can mean the day rolled over, which moves every period.
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { reload() }
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 8) {
            name
            Picker(L10n.tr("Range"), selection: $range) {
                ForEach(InsightRange.allCases) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
            .controlSize(.large)
            .accessibilityIdentifier("insights.range")
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 16)
        .padding(.vertical, 8)
        .background(.bar)
    }

    @ViewBuilder
    private var name: some View {
        let text = Text(babyName)
            .font(.title2.weight(.bold))
            .foregroundStyle(Palette.primaryText)
            .lineLimit(2)
            .minimumScaleFactor(0.8)
        if let onSwitchBaby {
            Button(action: onSwitchBaby) {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    text
                    Image(systemName: "chevron.down")
                        .font(.footnote.weight(.bold))
                        .foregroundStyle(Palette.secondaryText)
                }
                .frame(minHeight: 44)
            }
            .buttonStyle(.plain)
            .accessibilityLabel(L10n.tr("Switch baby"))
            .accessibilityValue(babyName)
            .accessibilityIdentifier("insights.babySwitcher")
        } else if !babyName.isEmpty {
            text
                .accessibilityAddTraits(.isHeader)
                .accessibilityIdentifier("insights.babyName")
        }
    }

    /// The one load path. A failure says so in the banner rather than leaving
    /// stale averages on screen as if they were current.
    private func reload() {
        do {
            try model.load()
            // Only a stale load banner is cleared: a save failure's banner
            // must survive a successful read.
            if errorReporter.current == ErrorBannerCopy.loadFailed { errorReporter.clear() }
        } catch {
            errorReporter.report(ErrorBannerCopy.loadFailed, error: error, category: AppLog.store)
        }
    }
}
```

Delete `InsightsCopy` if nothing outside the module uses it (`grep -rn InsightsCopy Packages Apps`). If something does, keep only the members still referenced.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd Packages/BabyTracker && LANG=en_US.UTF-8 swift test --filter InsightsFeatureTests`
Expected: PASS.

- [ ] **Step 5: Commit** (`make test`, bump `EXPECTED_TESTS` by 2)

```bash
git add -A Packages/BabyTracker Makefile
git commit -m "feat(insights): one-scroll screen with pinned range, section cards and growth

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: History focus filter, Ask's old Insights links, and app wiring

**Files:**
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/HistoryView.swift` (`HistoryFocus`, `openFocusedEntry`)
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/HistoryViewModel.swift` (`apply(_:)`)
- Modify: `Packages/BabyTracker/Sources/AskFeature/AskViewModel.swift`, `Packages/BabyTracker/Sources/AskFeature/AskView.swift`, `Packages/BabyTracker/Package.swift`
- Modify: `Apps/BabyTrackerApp/RootView.swift`
- Test: `Packages/BabyTracker/Tests/HistoryFeatureTests/HistoryFocusTests.swift`, `Packages/BabyTracker/Tests/AskFeatureTests/AskViewModelTests.swift`

**Interfaces:**
- Consumes: `InsightsView(onSwitchBaby:onOpenAllCheckIns:)`.
- Produces:
  - `HistoryFocus`, with `init(entryID:)`, `init(filter:)`, `entryID: UUID?` and `filter: Set<Tracker>?`.
  - `HistoryViewModel.apply(_ focus: HistoryFocus) -> UUID?`.
  - `AskViewModel.linkedSources(_ sources: [AskSource]) -> [AskSource]` (static).

- [ ] **Step 1: Write the failing test.** `HistoryFocusTests.swift`:

```swift
import Foundation
import Testing
import BabyCore
import BabyStore
import BabyUI
@testable import HistoryFeature

@MainActor @Suite struct HistoryFocusTests {
    @Test func aFilterFocusShowsOnlyThatTrackerAndOpensNothing() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let model = HistoryViewModel(store: store, babyID: baby.id)
        #expect(model.apply(HistoryFocus(filter: [.growth])) == nil)
        #expect(model.filter == [.growth])
        #expect(model.isFiltered)
    }

    @Test func anEntryFocusClearsTheFilterAndNamesTheEntry() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let model = HistoryViewModel(store: store, babyID: baby.id)
        model.filter = [.feed]
        let id = UUID()
        #expect(model.apply(HistoryFocus(entryID: id)) == id)
        #expect(!model.isFiltered)
    }
}
```

In `AskViewModelTests.swift`, replace the last line of `sourceLabelsNameTheRecordAndItsTime`, the one that expects `.insightRange(key: "feeds", days: 7)` to produce "Feeding", with a new test after that function:

```swift
@MainActor
@Test func oldInsightsLinksInSavedChatsAreNotShown() {
    let entry = AskSource.entry(kind: "diaper", id: UUID())
    let saved: [AskSource] = [.insight(key: "feeds"), entry, .insightRange(key: "sleep", days: 7)]
    #expect(AskViewModel.linkedSources(saved) == [entry])
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter "HistoryFocusTests|AskViewModelTests"`
Expected: the build fails (`init(filter:)`, `apply` and `linkedSources` don't exist).

- [ ] **Step 3: Implement.**

In `HistoryView.swift`:

```swift
public struct HistoryFocus: Equatable {
    public let id = UUID()
    /// An entry to open in its edit sheet (an Ask source).
    public let entryID: UUID?
    /// Trackers to show (Insights' All check-ins).
    public let filter: Set<Tracker>?

    public init(entryID: UUID) { self.entryID = entryID; self.filter = nil }
    public init(filter: Set<Tracker>) { self.entryID = nil; self.filter = filter }
}
```

Replace `openFocusedEntry()`'s body with:

```swift
        guard let focus, handledFocusID != focus.id else { return }
        handledFocusID = focus.id
        let entryID = model.apply(focus)
        reload()
        guard let entryID, model.canEdit else { return }
        do {
            if let ref = try model.entryRef(for: entryID) {
                onAction(.edit(ref))
            }
        } catch {
            errorReporter.report(ErrorBannerCopy.loadFailed, error: error, category: AppLog.store)
        }
```

Update its doc comment: "An Ask source opens its entry's edit sheet with the filter cleared, so its row is behind the sheet (ruling H-R9). Insights' All check-ins sets the Growth filter."

In `HistoryViewModel.swift`, next to `filter`:

```swift
    /// What a focus asks of the list: its filter (or all trackers), and the
    /// entry to open, if any.
    public func apply(_ focus: HistoryFocus) -> UUID? {
        filter = focus.filter ?? Set(Self.trackers)
        return focus.entryID
    }
```

In `AskViewModel.swift`:

```swift
    /// The sources a turn shows as links. The current Ask only cites log
    /// entries; `.insight` / `.insightRange` survive in chats saved by the
    /// earlier Ask and are decoded but not shown — Insights has no section
    /// to open any more (spec amendment 2026-10-06).
    public nonisolated static func linkedSources(_ sources: [AskSource]) -> [AskSource] {
        sources.filter {
            switch $0 {
            case .entry, .runningTimer: true
            case .insight, .insightRange: false
            }
        }
    }
```

In `sourceLabel(_:)`, replace the `.insight` / `.insightRange` branch with `case .insight, .insightRange: return ""  // never shown: see linkedSources`.

In `AskView.swift`'s turn view:
1. Bind `let links = AskViewModel.linkedSources(turn.sources)` above the source chips.
2. Use `links` in place of `turn.sources` in `if !turn.sources.isEmpty`, the `ForEach` and `if turn.sources.isEmpty, let range = turn.queriedRange`, so a turn with only old Insights links shows its range caption.
3. Delete `import InsightsFeature`.

In `Package.swift`, remove `"InsightsFeature"` from the AskFeature target's dependencies.

In `RootView.swift`, change the Insights tab to:

```swift
InsightsView(
    store: store, babyID: baby.id, babyName: baby.name, volumeUnit: environment.volumeUnit,
    onSwitchBaby: { isSwitchingBaby = true },
    onOpenAllCheckIns: { historyFocus = HistoryFocus(filter: [.growth]); selectedTab = .history }
)
```

The Ask cases stay `case .insight, .insightRange: break`, as set in Task 1. Make sure `RootView.swift` imports BabyUI (for `Tracker`); if it doesn't already, add `import BabyUI`. Remove `import InsightsFeature` from any file that no longer uses it (`grep -rn "import InsightsFeature" Packages Apps`).

- [ ] **Step 4: Run the tests and build the app**

```bash
cd Packages/BabyTracker && swift test --filter "HistoryFeatureTests|InsightsFeatureTests|AskFeatureTests" && cd ../..
pgrep -fl xcodebuild || xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build
```
Expected: tests PASS and `** BUILD SUCCEEDED **`.

- [ ] **Step 5: Commit** (`make test`, bump `EXPECTED_TESTS` by 3)

```bash
git add -A Packages/BabyTracker Apps/BabyTrackerApp/RootView.swift Makefile
git commit -m "feat(insights): All check-ins opens History filtered to Growth; hide old Insights links in Ask

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Strings — add translations, delete unused keys

**Files:**
- Modify: `Packages/BabyTracker/Sources/InsightsFeature/Localizable.xcstrings`, `Packages/BabyTracker/Sources/GrowthFeature/Localizable.xcstrings`
- Create: `work/insights-strings.py` (throwaway helper, not committed)

**Interfaces:**
- Consumes: every `L10n.tr`/`L10n.fmt`/`GrowthCopy` key from Tasks 1–8.
- Produces: catalogs with en/de/es/fr/it/uk for every key in use, and no key without a source reference.

- [ ] **Step 1: Write the check that fails.** Run the gate against the baseline:

```bash
python3 scripts/verify-localizations.py 2>&1 | sort > work/insights-loc-now.txt
comm -13 <(sort work/insights-loc-baseline.txt) work/insights-loc-now.txt
```
Expected: nothing yet. New keys never written to a catalog are invisible to the gate, so in the simulator they fall back to English for every locale. The real check is Step 4's key-reference scan.

- [ ] **Step 2: Write the helper** `work/insights-strings.py`:

```python
import json, re, sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
LANGS = ["en", "de", "es", "fr", "it", "uk"]

INSIGHTS = {
    "Week": ["Week", "Woche", "Semana", "Semaine", "Settimana", "Тиждень"],
    "Month": ["Month", "Monat", "Mes", "Mois", "Mese", "Місяць"],
    "All": ["All", "Alles", "Todo", "Tout", "Tutto", "Усе"],
    "feeds / day": ["feeds / day", "Mahlzeiten / Tag", "tomas / día", "repas / jour", "pasti / giorno", "годувань / день"],
    "/ day": ["/ day", "/ Tag", "/ día", "/ jour", "/ giorno", "/ день"],
    "diapers / day": ["diapers / day", "Windeln / Tag", "pañales / día", "couches / jour", "pannolini / giorno", "підгузків / день"],
    "vs prev. week": ["vs prev. week", "ggü. Vorwoche", "vs. semana ant.", "vs sem. préc.", "vs sett. prec.", "до мин. тижня"],
    "vs prev. month": ["vs prev. month", "ggü. Vormonat", "vs. mes ant.", "vs mois préc.", "vs mese prec.", "до мин. місяця"],
    "1 day": ["1 day", "1 Tag", "1 día", "1 jour", "1 giorno", "1 день"],
    "since %@": ["since %@", "seit %@", "desde %@", "depuis le %@", "dal %@", "з %@"],
    "Nothing logged in this period.": ["Nothing logged in this period.", "In diesem Zeitraum nichts erfasst.", "Nada registrado en este período.", "Rien d’enregistré sur cette période.", "Nessuna registrazione in questo periodo.", "За цей період нічого не записано."],
    "Between feeds": ["Between feeds", "Zwischen Mahlzeiten", "Entre tomas", "Entre les repas", "Tra i pasti", "Між годуваннями"],
    "Longest gap": ["Longest gap", "Längste Pause", "Mayor intervalo", "Plus long écart", "Intervallo più lungo", "Найдовша перерва"],
    "Breast": ["Breast", "Brust", "Pecho", "Sein", "Seno", "Груди"],
    "Both": ["Both", "Beides", "Ambos", "Les deux", "Entrambi", "Обидва"],
    "Bottle": ["Bottle", "Flasche", "Biberón", "Biberon", "Biberon", "Пляшечка"],
    "Left · right": ["Left · right", "Links · rechts", "Izquierdo · derecho", "Gauche · droite", "Sinistro · destro", "Ліва · права"],
    "Bottle / day": ["Bottle / day", "Flasche / Tag", "Biberón / día", "Biberon / jour", "Biberon / giorno", "Пляшечка / день"],
    "Longest stretch": ["Longest stretch", "Längster Schlaf", "Sueño más largo", "Plus long sommeil", "Sonno più lungo", "Найдовший сон"],
    "Night · naps": ["Night · naps", "Nacht · Nickerchen", "Noche · siestas", "Nuit · siestes", "Notte · pisolini", "Ніч · денні сни"],
    "Naps / day": ["Naps / day", "Nickerchen / Tag", "Siestas / día", "Siestes / jour", "Pisolini / giorno", "Денних снів / день"],
    "Awake between sleeps": ["Awake between sleeps", "Wach zwischen Schläfchen", "Despierto entre sueños", "Éveil entre les sommeils", "Sveglio tra i sonni", "Неспання між снами"],
    "Night": ["Night", "Nacht", "Noche", "Nuit", "Notte", "Ніч"],
    "Naps": ["Naps", "Nickerchen", "Siestas", "Siestes", "Pisolini", "Денні сни"],
    "Night %1$@ · naps %2$@": ["Night %1$@ · naps %2$@", "Nacht %1$@ · Nickerchen %2$@", "Noche %1$@ · siestas %2$@", "Nuit %1$@ · siestes %2$@", "Notte %1$@ · pisolini %2$@", "Ніч %1$@ · денні %2$@"],
    "%@, so far": ["%@, so far", "%@, bisher", "%@, hasta ahora", "%@, pour l’instant", "%@, finora", "%@, поки що"],
    "%1$@: %2$@ %3$@.": ["%1$@: %2$@ %3$@."] * 6,
    "%@ more than the previous week.": ["%@ more than the previous week.", "%@ mehr als in der Vorwoche.", "%@ más que la semana anterior.", "%@ de plus que la semaine précédente.", "%@ in più rispetto alla settimana precedente.", "На %@ більше, ніж минулого тижня."],
    "%@ less than the previous week.": ["%@ less than the previous week.", "%@ weniger als in der Vorwoche.", "%@ menos que la semana anterior.", "%@ de moins que la semaine précédente.", "%@ in meno rispetto alla settimana precedente.", "На %@ менше, ніж минулого тижня."],
    "%@ more than the previous month.": ["%@ more than the previous month.", "%@ mehr als im Vormonat.", "%@ más que el mes anterior.", "%@ de plus que le mois précédent.", "%@ in più rispetto al mese precedente.", "На %@ більше, ніж минулого місяця."],
    "%@ less than the previous month.": ["%@ less than the previous month.", "%@ weniger als im Vormonat.", "%@ menos que el mes anterior.", "%@ de moins que le mois précédent.", "%@ in meno rispetto al mese precedente.", "На %@ менше, ніж минулого місяця."],
    "About the same as the previous week.": ["About the same as the previous week.", "Etwa wie in der Vorwoche.", "Similar a la semana anterior.", "Comme la semaine précédente.", "Come la settimana precedente.", "Приблизно як минулого тижня."],
    "About the same as the previous month.": ["About the same as the previous month.", "Etwa wie im Vormonat.", "Similar al mes anterior.", "Comme le mois précédent.", "Come il mese precedente.", "Приблизно як минулого місяця."],
    "Average of %lld days.": ["Average of %lld days.", "Durchschnitt aus %lld Tagen.", "Promedio de %lld días.", "Moyenne sur %lld jours.", "Media di %lld giorni.", "Середнє за %lld дн."],
    "Since %@.": ["Since %@.", "Seit %@.", "Desde %@.", "Depuis le %@.", "Dal %@.", "З %@."],
    "Switch baby": ["Switch baby", "Baby wechseln", "Cambiar de bebé", "Changer de bébé", "Cambia bambino", "Змінити дитину"],
}

GROWTH = {
    "Length": ["Length", "Länge", "Longitud", "Taille", "Lunghezza", "Зріст"],
    "Head": ["Head", "Kopf", "Cabeza", "Tête", "Testa", "Голова"],
    "P%@": ["P%@"] * 6,
    "<P3": ["<P3"] * 6,
    ">P97": [">P97"] * 6,
    "%1$@ since %2$@": ["%1$@ since %2$@", "%1$@ seit %2$@", "%1$@ desde %2$@", "%1$@ depuis le %2$@", "%1$@ dal %2$@", "%1$@ з %2$@"],
    "≈ %@ / day": ["≈ %@ / day", "≈ %@ / Tag", "≈ %@ / día", "≈ %@ / jour", "≈ %@ / giorno", "≈ %@ / день"],
    "All check-ins": ["All check-ins", "Alle Messungen", "Todas las mediciones", "Toutes les mesures", "Tutte le misurazioni", "Усі вимірювання"],
}

def update(module, additions):
    sources = ROOT / "Packages/BabyTracker/Sources" / module
    path = sources / "Localizable.xcstrings"
    catalog = json.loads(path.read_text())
    strings = catalog["strings"]
    for key, values in additions.items():
        entry = strings.setdefault(key, {})
        loc = entry.setdefault("localizations", {})
        for lang, value in zip(LANGS, values):
            loc[lang] = {"stringUnit": {"state": "translated", "value": value}}
    code = "\n".join(p.read_text() for p in sources.rglob("*.swift"))
    unused = [k for k in strings if json.dumps(k) not in code and f'"{k}"' not in code]
    for key in unused:
        del strings[key]
    path.write_text(json.dumps(catalog, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
    print(module, "added", len(additions), "removed", unused)

if __name__ == "__main__":
    which = sys.argv[1:] or ["InsightsFeature", "GrowthFeature", "AskFeature"]
    if "InsightsFeature" in which: update("InsightsFeature", INSIGHTS)
    if "GrowthFeature" in which: update("GrowthFeature", GROWTH)
    # Ask's old Insights chip labels ("Bottle volume", "Feeding", …) are unused after Task 8.
    if "AskFeature" in which: update("AskFeature", {})
```

The unused-key scan matches a key that appears as a Swift string literal. Review the printed `removed` list before committing, and restore any key that turns out to be built dynamically (for example by string interpolation). If the existing catalog's formatting differs (key order, indentation), keep `indent=2, sort_keys=True` and check that `git diff --stat` shows only these two catalogs. Xcode rewrites catalogs in its own order on the next build, which is fine.

- [ ] **Step 3: Run it**

```bash
python3 work/insights-strings.py
```
Run it as `python3 work/insights-strings.py InsightsFeature AskFeature`. GrowthFeature was done in Task 6, and re-running it is harmless. For AskFeature, check that the removal list holds only the old chip labels' keys (for example "Bottle volume") and no key the current Ask uses.
Expected: InsightsFeature reports 36 added and a removal list containing the old strip and figure keys ("Last 24 hours", "Now", "Not enough history yet", "%lld more than usual", …).

- [ ] **Step 4: Verify**

```bash
python3 scripts/verify-localizations.py 2>&1 | sort > work/insights-loc-now.txt
comm -13 <(sort work/insights-loc-baseline.txt) work/insights-loc-now.txt   # must print nothing
cd Packages/BabyTracker && swift test --filter "GrowthFeatureTests|InsightsFeatureTests" && cd ../..
```
Expected: no new gate lines, and the tests pass. `displayedSwissCopyIsLocalizedInEverySupportedLocale` covers every GrowthFeature key in six locales.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker/Sources/InsightsFeature/Localizable.xcstrings Packages/BabyTracker/Sources/GrowthFeature/Localizable.xcstrings Packages/BabyTracker/Sources/AskFeature/Localizable.xcstrings
git commit -m "chore(l10n): Insights and Growth section strings in six languages; drop removed screens' keys

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: UI tests, simulator verification, and the record

**Files:**
- Modify: `UITests/BabyTrackerUITests/UITestHarness.swift` (Insights tab marker)
- Modify: `UITests/BabyTrackerUITests/GrowthFlowUITests.swift` (new test)
- Create: `docs/verification/2026-10-06-insights-redesign.md`
- Modify: `TASKS.md` (Insights section; this one is committed only from the worktree's copy of the section, so see Step 6)

**Interfaces:**
- Consumes the identifiers from Tasks 6–8.

- [ ] **Step 1: Write the UI test.**
  1. In `UITestHarness.swift`, change `case "Insights": staticTexts["Insights"]` to `case "Insights": descendants(matching: .any)["insights.range"].firstMatch`. The "INSIGHTS" eyebrow is gone.
  2. Add to `GrowthFlowUITests`:

```swift
    /// The Growth section replaced the details screen: its chips switch the
    /// chart, and All check-ins lands in History filtered to Growth.
    func testInsightsGrowthSectionOpensCheckInsInHistory() {
        let app = Harness.launch(Harness.Launch(historySessions: true))
        defer { app.terminate() }
        app.waitingForHome()
        XCTAssertTrue(app.goToTab("Insights"))
        let all = app.buttons["insights.growth.allCheckIns"]
        for _ in 0..<8 where !all.isHittable { app.swipeUp() }
        XCTAssertTrue(all.isHittable, "All check-ins never scrolled into view")
        let length = app.buttons["insights.growth.chip.lengthHeight"]
        XCTAssertTrue(length.exists)
        length.tap()
        XCTAssertTrue(length.isSelected)
        all.tap()
        XCTAssertTrue(app.buttons["history.filter.clear"].waitForExistence(timeout: 8))
    }
```

- [ ] **Step 2: Run the affected UI tests (subset only, about 6 minutes)**

```bash
pgrep -fl xcodebuild && echo "another run is active — wait" || \
xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' \
  -parallel-testing-enabled NO -maximum-concurrent-test-simulator-destinations 1 \
  -only-testing:BabyTrackerUITests/GrowthFlowUITests \
  -only-testing:BabyTrackerUITests/AccessibilityAuditTests/testAuditInsights \
  -only-testing:BabyTrackerUITests/SettingsFlowTests/testChangingTheUnitReachesHomeHistoryAndInsights \
  -only-testing:BabyTrackerUITests/UkrainianLayoutSmokeTests \
  -only-testing:BabyTrackerUITests/AskTabTests
```

Also run the AX5 test that visits the Insights tab. Find its name with `grep -n "func test" UITests/BabyTrackerUITests/AccessibilityAuditTests.swift`; it is the method containing `ax5-dock`. Expected: all pass.
- If `testAuditInsights` reports a new finding, fix the view. Don't add a `KnownIssue` unless the finding is the same platform false positive the file already documents (`textClipped` with stated height), and record why in the test.
- If `SettingsFlowTests` can't find "oz" on Insights, check that the seeded yesterday bottles fall in Week. The fixture logs bottles yesterday, so the Bottle / day row must show; fix the presentation, not the test.

- [ ] **Step 3: Full package suite and app build**

```bash
make test
pgrep -fl xcodebuild || xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build
```
Expected: `OK: <EXPECTED_TESTS> tests` and `** BUILD SUCCEEDED **`.

- [ ] **Step 4: Screenshots against the mockups.**
  1. Boot the shared simulator if needed (`xcrun simctl boot <simulator-udid>`).
  2. Install the built app (`xcrun simctl install <simulator-udid> <path from the build's .app>`).
  3. Launch it with the launch arguments `Harness.Launch(seedFixture: true, historySessions: true)` produces. Read them from `UITestHarness.swift`'s `launch` function; they are `-uiTest…` flags.
  4. Open Insights, then capture screenshots:
     - `xcrun simctl io <simulator-udid> screenshot work/insights-week.png`
     - after switching the range to All, `work/insights-all.png`
     - after scrolling to Growth, `work/insights-growth.png`
  5. Compare them by eye with `mockups/2026-10-04-insights-redesign/insights-full.html` (Week, All). Note each difference in the verification doc as either fixed or accepted with a reason.

- [ ] **Step 5: Write `docs/verification/2026-10-06-insights-redesign.md`.** List each command above with its result (test totals, UI test names and counts, build result, localization gate output), the screenshot comparison notes, and anything not run.

- [ ] **Step 6: Commit**

```bash
git add UITests/BabyTrackerUITests/UITestHarness.swift UITests/BabyTrackerUITests/GrowthFlowUITests.swift docs/verification/2026-10-06-insights-redesign.md
git commit -m "test(insights): growth section to History UI test; verification record

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

`TASKS.md` on `main` carries other sessions' uncommitted edits. Update only the "Insights redesign (2026-10-04)" section, in the main checkout's working copy, and do not commit it from the worktree.
