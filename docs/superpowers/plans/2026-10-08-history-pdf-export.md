# History PDF Export and Daily Bottle Total Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a bottle-volume total to History's day headers. Add an "Export PDF" button to History that turns a chosen date range into a printable, shareable PDF. Each day in the PDF lists its totals and its full log.

**Architecture:**
- **One builder.** History's day-building moves out of `HistoryViewModel` into an internal `HistoryDayBuilder`. The list and the export both use it, so their totals always agree.
- **Report pipeline.** The export maps built days into a plain `HistoryReport`, renders it to HTML with a pure function, and prints that HTML to PDF with UIKit. That is `UIMarkupTextPrintFormatter`, then `UIPrintPageRenderer`, then `UIGraphicsPDFRenderer`.
- **Export sheet.** The PDF is written to a private temp folder and handed to the share sheet.

**Tech Stack:** Swift 6, SwiftUI, SwiftData (`BabyStore`), Swift Testing, XCUITest, String Catalogs (`.xcstrings`), UIKit printing.

**Spec:** `docs/superpowers/specs/2026-10-08-history-pdf-export-design.md`

## Global Constraints

- All new code lives in `Packages/BabyTracker/Sources/HistoryFeature`. The one exception is `ActivityShareSheet`, which moves to `BabyUI`. `HistoryFeature` depends only on `BabyCore`, `BabyProtocol`, `BabyStore` and `BabyUI`. Do not add a dependency on `BabySync`.
- The package builds for iOS 26 and macOS 26. Wrap every UIKit use in `#if canImport(UIKit)`. `swift test` (`make test`) runs on macOS.
- Every user-visible string goes into `Sources/HistoryFeature/Localizable.xcstrings` with `de`, `es`, `fr`, `it` and `uk` translations, and `en` as the source.
  - Read strings through `L10n.tr` / `L10n.fmt`.
  - Add catalog entries with `scripts/add-catalog-strings.py`, which Task 2 creates.
  - After every catalog change, `python3 scripts/verify-localizations.py` must pass.
- The day totals appear in this order: feeds, sleep, bottle, diapers. A total is shown only when it is above zero.
- The bottle total belongs to the day its feeding session **started**, the same rule the feed count uses.
- The export always covers every tracker (`Set(HistoryViewModel.trackers)`), whatever the History filter is. Running timers are never exported.
- PDF paper is US Letter (612×792 pt) when `Locale.current.measurementSystem == .us`, and A4 (595.28×841.89 pt) otherwise. Margins are 36 pt.
- The PDF temp folder is `FileManager.default.temporaryDirectory/baby-tracker-history-exports`. It is excluded from backup, uses complete file protection on iOS, and is emptied before each new export.
- Accessibility identifiers:
  - `history.export` (header button)
  - `history.export.preset`
  - `history.export.from`
  - `history.export.to`
  - `history.export.create`
  - `history.export.cancel`
  - `history.export.empty`
  - `history.export.failed`
- `Makefile`'s `EXPECTED_TESTS` (1622 at plan time) must rise by exactly the number of `@Test` functions each task adds, in that task's commit. Tests guarded by `#if canImport(UIKit)` do not run under `swift test`, so they are **not** counted.
- iOS builds and tests use only the shared simulator, `platform=iOS Simulator,id=<simulator-udid>`. Never run two `xcodebuild` processes at once (`pgrep xcodebuild` first).
- Work on branch `history-pdf-export` in a worktree. `main` has unrelated uncommitted edits from a parallel session, so never stage them.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.

## Review Focus

1. **A long range with many entries.** The PDF must spread over several pages with nothing cut off, and each table's column headings should repeat on the next page. Pinned by the multi-page PDF smoke test in Task 5.
2. **A baby name or note containing HTML or path characters**, such as `Mia/Lev`, `<b>`, `&` or quotes. The name must appear literally in the PDF, and the file name must still be valid. Pinned by the escaping test in Task 4 and the file-name test in Task 5.
3. **Custom dates out of order or before birth.** The range clamps to between the birth date and today, and From never ends up after To. Pinned in Task 6.
4. **The ounces preference.** The header and the PDF show oz, not ml. Pinned in Task 3.
5. **Exporting a range that includes today while a timer runs.** The running feed or sleep is not in the PDF, and finished entries still are. Pinned in Task 3.

---

## File structure

| File | Responsibility |
|---|---|
| `Sources/HistoryFeature/HistoryDayBuilder.swift` (new) | Store data → days with totals and rows, for any range and filter. The row-building code moves here from the view model. |
| `Sources/HistoryFeature/HistoryViewModel.swift` (modify) | Keeps paging, the filter, focus, delete and empty states. Calls the builder. |
| `Sources/HistoryFeature/HistoryDayTotals+Phrases.swift` (new) | `HistoryDayTotals.none` and `phrases(volumeUnit:)`, shared by VoiceOver and the PDF. |
| `Sources/HistoryFeature/HistoryView.swift` (modify) | Header bottle total and export button. Presents the export sheet. |
| `Sources/HistoryFeature/HistoryReport.swift` (new) | The report value type and `HistoryReportBuilder`. |
| `Sources/HistoryFeature/HistoryReportHTML.swift` (new) | Pure HTML rendering and escaping. |
| `Sources/HistoryFeature/HistoryPDF.swift` (new) | `PaperSize`, `HistoryPDFPrinter` (UIKit), `HistoryExportFiles` (temp folder and file name). |
| `Sources/HistoryFeature/HistoryExportViewModel.swift` (new) | Presets, date clamping, the create → share state machine. |
| `Sources/HistoryFeature/HistoryExportSheet.swift` (new) | The sheet UI. |
| `Sources/BabyUI/ActivityShareSheet.swift` (new) | The share sheet, moved from `SettingsFeature`. |
| `Sources/SettingsFeature/FamilyExportView.swift` (modify) | Uses `BabyUI.ActivityShareSheet`. |
| `scripts/add-catalog-strings.py` (new) | Adds plain translated keys to an `.xcstrings` catalog. |
| `Tests/HistoryFeatureTests/HistoryViewModelTests.swift` (modify) | Bottle totals. |
| `Tests/HistoryFeatureTests/HistoryReportTests.swift` (new) | Report, HTML, files and PDF tests. |
| `Tests/HistoryFeatureTests/HistoryExportViewModelTests.swift` (new) | Presets, clamping, create states. |
| `UITests/BabyTrackerUITests/HistoryRedesignTests.swift` (modify) | `testExportSheetCreatesAPDF`. |

Paths starting with `Sources/` or `Tests/` are relative to `Packages/BabyTracker/`.

---

### Task 0: Workspace and tracker

**Files:**
- Modify: `TASKS.md` (in the worktree)

- [ ] **Step 1: Create the worktree.** Use the superpowers:using-git-worktrees skill: branch `history-pdf-export` from `main`, worktree under the repo's usual worktree location.
- [ ] **Step 2: Add a tracker section** at the top of `TASKS.md`:

```markdown
## History PDF export and daily bottle total (2026-10-08)

Spec: docs/superpowers/specs/2026-10-08-history-pdf-export-design.md
Plan: docs/superpowers/plans/2026-10-08-history-pdf-export.md

- [ ] Task 1 Day builder + bottle total
- [ ] Task 2 Header bottle total + phrases + catalog script
- [ ] Task 3 Report model and builder
- [ ] Task 4 HTML renderer
- [ ] Task 5 PDF printer and export files
- [ ] Task 6 Export view model
- [ ] Task 7 Export sheet, header button, share sheet move, UI test
- [ ] Final: make test, simulator build, UI subset, PDF inspected on simulator
```

- [ ] **Step 3: Check the baseline.** Run `make test` and confirm it ends with `1622 tests` passing. If the number differs, `main` moved: use the current number as the baseline and write it in `TASKS.md`.
- [ ] **Step 4: Commit** `TASKS.md` with the message `chore(tasks): track history PDF export`.

---

### Task 1: `HistoryDayBuilder` and the bottle total

**Files:**
- Create: `Sources/HistoryFeature/HistoryDayBuilder.swift`
- Create: `Sources/HistoryFeature/HistoryDayTotals+Phrases.swift` (only `none` in this task)
- Modify: `Sources/HistoryFeature/HistoryViewModel.swift` (`HistoryDayTotals` at lines 50–61, `load(dayCount:)` at lines 163–282, the row builders at lines 361–461)
- Test: `Tests/HistoryFeatureTests/HistoryViewModelTests.swift`

**Interfaces:**
- Produces:
  - `HistoryDayTotals(feeds: Int, sleep: String, bottleML: Double, diapers: Int)` and `HistoryDayTotals.none`
  - `struct BuiltEntry { let at: Date; let row: HistoryListRow; let note: String? }`
  - `struct BuiltDay { let key: DayKey; let totals: HistoryDayTotals; let entries: [BuiltEntry] }`, with entries newest first
  - `struct BuiltRange { let windowStart: Date; let days: [DayKey: BuiltDay]; let items: [HistoryItem]; let measurements: [MeasurementCheckIn] }`
  - `@MainActor struct HistoryDayBuilder { init(store:babyID:volumeUnit:zone:); func build(from: DayKey, through: DayKey, filter: Set<Tracker>) throws -> BuiltRange; func chip(_ index: Int, _ component: FeedComponent) -> HistoryChip }`

- [ ] **Step 1: Write the failing tests.**

In `HistoryViewModelTests.swift`, update the three existing totals expectations so they include `bottleML`:
- line 359: `HistoryDayTotals(feeds: 3, sleep: "50m", bottleML: 150, diapers: 2)`
- line 376: `HistoryDayTotals(feeds: 2, sleep: "", bottleML: 210, diapers: 1)`
- line 396: `HistoryDayTotals(feeds: 0, sleep: "", bottleML: 0, diapers: 2)`

Then add these tests after `dayTotalsDescribeTheRowsActuallyShown`:

```swift
/// A feed across midnight is one session on the day it started, so its
/// bottle counts there too, however late the bottle itself was given.
@MainActor
@Test func aBottleAfterMidnightCountsOnTheDayItsSessionStarted() throws {
    let (store, baby, _) = try makeStore()
    let left = try store.logBreast(side: .left, from: localTime(23, 40, dayOffset: -1), to: localTime(23, 55, dayOffset: -1), for: baby)
    _ = try store.addFeedPart(.bottle(volumeML: 70, contents: .breastMilk, at: localTime(0, 10)), toSession: left.id, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 2)

    let yesterday = try #require(model.days.first { $0.id == dayKey(localTime(12, 0, dayOffset: -1)) })
    #expect(yesterday.totals.bottleML == 70)
    #expect(yesterday.totals.feeds == 1)
    #expect(!model.days.contains { $0.id == dayKey(noonLocal()) })
}

/// D6 for the new total: with feeds filtered out, the bottle totals nothing.
@MainActor
@Test func aFilteredOutFeedTotalsNoBottle() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby, at: localTime(9, 0))
    _ = try store.logDiaper(.wet, for: baby, at: localTime(10, 0))

    let model = HistoryViewModel(store: store, babyID: baby)
    model.filter = [.diaper]
    try model.load(dayCount: 1)

    #expect(model.days[0].totals == HistoryDayTotals(feeds: 0, sleep: "", bottleML: 0, diapers: 1))
}

/// A tombstoned bottle neither shows nor counts.
@MainActor
@Test func aDeletedBottleCountsNoVolume() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby, at: localTime(9, 0))
    let gone = try store.logBottle(volumeML: 80, contents: .formula, for: baby, at: localTime(11, 30))
    try store.deleteFeedPart(gone.id, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)

    #expect(model.days[0].totals.bottleML == 120)
}
```

- [ ] **Step 2: Run the tests to confirm they fail.**

Run: `cd Packages/BabyTracker && swift test --filter HistoryFeatureTests 2>&1 | tail -20`
Expected: a compile failure, `extra argument 'bottleML' in call`.

- [ ] **Step 3: Add the field and `none`.**

In `HistoryViewModel.swift`, replace the `HistoryDayTotals` struct with:

```swift
/// A day header's icon totals. Zero / empty means "not shown": the tracker
/// had nothing that day, or the filter hides it.
public struct HistoryDayTotals: Equatable {
    /// Feeding sessions that started that day, counted once however many
    /// parts they have — the same number as Home's `todayFeedCount`, solids
    /// sessions included.
    public let feeds: Int
    /// The asleep total of the sleep sessions that started that day, or ""
    /// when there were none.
    public let sleep: String
    /// Every bottle part of the feeding sessions that started that day, in
    /// millilitres — the day the feed count uses, so a bottle after midnight
    /// stays with the feed it belongs to. Formatted where it is shown.
    public let bottleML: Double
    public let diapers: Int
}
```

Create `Sources/HistoryFeature/HistoryDayTotals+Phrases.swift`:

```swift
import Foundation

extension HistoryDayTotals {
    /// A day with nothing to total: only running timers, or a filter that
    /// hides every tracker it had.
    static let none = HistoryDayTotals(feeds: 0, sleep: "", bottleML: 0, diapers: 0)
}
```

- [ ] **Step 4: Create `HistoryDayBuilder.swift`.**

Move `feedRow`, `chip`, `sleepRow`, `diaperRow`, `medRow`, `growthRow` and `singleRow` out of `HistoryViewModel` unchanged except where noted. `chip` becomes internal (not private), because the view model's `runningRow` still calls it. `clockTime`, `title` and `chipDuration` stay as `static` on `HistoryViewModel`, and the moved code calls them as `HistoryViewModel.clockTime(…)` / `HistoryViewModel.chipDuration(…)`.

```swift
import Foundation
import BabyCore
import BabyStore
import BabyUI

/// One History entry as built: the row, when it happened (for ordering), and
/// its note, which the list does not show but the PDF does.
struct BuiltEntry: Equatable {
    let at: Date
    let row: HistoryListRow
    /// The entry's note; a session joins its parts' notes with " · ". Nil
    /// when there is none.
    let note: String?
}

/// One day as the store has it, before History titles it or adds running
/// timers. Only days with at least one entry the filter shows are built:
/// a non-zero total always comes with a row of its tracker.
struct BuiltDay: Equatable {
    let key: DayKey
    let totals: HistoryDayTotals
    /// Newest first; equal times fall back to the row id, so the order is
    /// stable from one load to the next.
    let entries: [BuiltEntry]
}

struct BuiltRange {
    /// The start of the first day built.
    let windowStart: Date
    let days: [DayKey: BuiltDay]
    /// Everything fetched, unfiltered — History's empty state asks whether
    /// anything exists at all.
    let items: [HistoryItem]
    let measurements: [MeasurementCheckIn]
}

/// Turns the store into History's days for any run of days and any filter.
/// The list builds its paging window with the user's filter; the PDF export
/// builds the chosen dates with every tracker. One builder, so the header
/// and the printout never disagree about a day.
@MainActor
struct HistoryDayBuilder {
    let store: BabyStore
    let babyID: UUID
    let volumeUnit: VolumeUnit
    let zone: String

    func build(from first: DayKey, through last: DayKey, filter: Set<Tracker>) throws -> BuiltRange {
        let windowStart = DayBucket.bounds(of: first, timeZoneID: zone).start
        let windowEnd = DayBucket.bounds(of: last, timeZoneID: zone).end
        func day(of date: Date) -> DayKey? {
            date >= windowStart && date < windowEnd ? DayBucket.key(for: date, timeZoneID: zone) : nil
        }

        // Session members start at or after their session's first part, so
        // a fetch from the window's start holds every member of every
        // session that starts inside it, a part after midnight included.
        let items = try store.entries(for: babyID, from: windowStart, to: .distantFuture)
        var feedsByID: [UUID: FeedEntry] = [:]
        var sleepsByID: [UUID: SleepEntry] = [:]
        var dated: [DayKey: [BuiltEntry]] = [:]
        var diaperCount: [DayKey: Int] = [:]
        for item in items {
            switch item {
            case .feed(let feed): feedsByID[feed.id] = feed
            case .sleep(let sleep): sleepsByID[sleep.id] = sleep
            case .diaper(let diaper):
                guard let key = day(of: diaper.startedAt) else { continue }
                diaperCount[key, default: 0] += 1
                if filter.contains(.diaper) {
                    dated[key, default: []].append(BuiltEntry(at: diaper.startedAt, row: diaperRow(diaper), note: Self.note([diaper.note])))
                }
            case .supplement(let med):
                guard filter.contains(.supplement), let key = day(of: med.startedAt) else { continue }
                dated[key, default: []].append(BuiltEntry(at: med.startedAt, row: medRow(med), note: Self.note([med.note])))
            }
        }

        // Feeds and sleeps come from the session projections Home and
        // Insights use, so a joined session is one row and one count, on the
        // day its first part started.
        var feedCount: [DayKey: Int] = [:]
        var bottleML: [DayKey: Double] = [:]
        for session in try store.feedingSessions(for: babyID, from: windowStart, to: windowEnd) {
            guard let key = day(of: session.startedAt) else { continue }
            let members = session.feedIDs.compactMap { feedsByID[$0] }
            guard !members.isEmpty else { continue }
            feedCount[key, default: 0] += 1
            bottleML[key, default: 0] += members.reduce(0) { $0 + $1.totalBottleVolumeML }
            if filter.contains(.feed) {
                dated[key, default: []].append(BuiltEntry(
                    at: session.startedAt, row: feedRow(session, members: members), note: Self.note(members.map(\.note))
                ))
            }
        }
        var sleepSeconds: [DayKey: Int] = [:]
        for session in try store.sleepSessions(for: babyID, from: windowStart, to: windowEnd) {
            let members = session.sleepIDs.compactMap { sleepsByID[$0] }
            guard let key = day(of: session.startedAt), let first = members.first else { continue }
            sleepSeconds[key, default: 0] += session.asleepSeconds
            if filter.contains(.sleep) {
                dated[key, default: []].append(BuiltEntry(
                    at: session.startedAt, row: sleepRow(session, first: first), note: Self.note(members.map(\.note))
                ))
            }
        }

        // H-R1: growth isn't a History entry in the store, so its check-ins
        // are merged in here by when they were measured.
        let measurements = try store.measurements(for: babyID)
        if filter.contains(.growth) {
            for check in measurements {
                guard let key = day(of: check.measuredAt) else { continue }
                dated[key, default: []].append(BuiltEntry(at: check.measuredAt, row: growthRow(check), note: nil))
            }
        }

        var days: [DayKey: BuiltDay] = [:]
        for (key, entries) in dated {
            days[key] = BuiltDay(
                key: key,
                totals: HistoryDayTotals(
                    feeds: filter.contains(.feed) ? feedCount[key, default: 0] : 0,
                    sleep: filter.contains(.sleep) ? sleepSeconds[key].map(ElapsedFormatter.total) ?? "" : "",
                    bottleML: filter.contains(.feed) ? bottleML[key, default: 0] : 0,
                    diapers: filter.contains(.diaper) ? diaperCount[key, default: 0] : 0
                ),
                entries: entries.sorted {
                    $0.at == $1.at ? $0.row.id.uuidString < $1.row.id.uuidString : $0.at > $1.at
                }
            )
        }
        return BuiltRange(windowStart: windowStart, days: days, items: items, measurements: measurements)
    }

    /// The non-blank notes, trimmed, joined; nil when none is left.
    static func note(_ notes: [String?]) -> String? {
        let kept = notes.compactMap { $0?.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty }
        return kept.isEmpty ? nil : kept.joined(separator: " · ")
    }

    // MARK: - Rows
    // feedRow, chip, sleepRow, diaperRow, medRow, growthRow, singleRow —
    // moved verbatim from HistoryViewModel (see the step text above).
}
```

The `// MARK: - Rows` block is where the moved functions go. Paste them in, then delete that two-line comment.

- [ ] **Step 5: Rewrite `HistoryViewModel.load(dayCount:)` to use the builder.**

```swift
    public func load(dayCount: Int) throws {
        self.dayCount = max(dayCount, 1)
        canEdit = store.canWriteInActiveRealm

        let zone = TimeZone.current.identifier
        let calendar = Calendar.current
        let dayKeys = (0..<self.dayCount).compactMap { offset in
            calendar.date(byAdding: .day, value: -offset, to: store.now)
                .map { DayBucket.key(for: $0, timeZoneID: zone) }
        }
        let today = DayBucket.key(for: store.now, timeZoneID: zone)
        let yesterday = calendar.date(byAdding: .day, value: -1, to: store.now)
            .map { DayBucket.key(for: $0, timeZoneID: zone) }
        let builder = HistoryDayBuilder(store: store, babyID: babyID, volumeUnit: volumeUnit, zone: zone)
        let built = try builder.build(from: dayKeys.last ?? today, through: today, filter: filter)

        let timers = try store.activeTimers(for: babyID)
        let running = timers
            .filter { filter.contains($0.kind == .feed ? .feed : .sleep) }
            .map { runningRow($0, builder: builder) }

        days = dayKeys.compactMap { key in
            let day = built.days[key]
            let rows = (key == today ? running : []) + (day?.entries.map(\.row) ?? [])
            guard !rows.isEmpty else { return nil }
            return HistoryDay(
                id: key,
                title: Self.title(for: key, today: today, yesterday: yesterday, zone: zone),
                totals: day?.totals ?? .none,
                rows: rows
            )
        }

        hasMore = try built.windowStart > oldestMoment(measurements: built.measurements)

        if !days.isEmpty {
            emptyState = nil
        } else if !(try hasAnyData(items: built.items, measurements: built.measurements, timers: timers)) {
            emptyState = .noData
        } else {
            emptyState = isFiltered ? .nothingMatches : .nothingRecent(dayCount: self.dayCount)
        }
    }
```

Change `runningRow` to take the builder for its chips:

```swift
    private func runningRow(_ timer: ActiveTimer, builder: HistoryDayBuilder) -> HistoryListRow {
        HistoryListRow(
            id: timer.id,
            tracker: timer.kind == .feed ? .feed : .sleep,
            start: Self.clockTime(timer.startedAt, zone: timer.timeZoneID),
            end: L10n.tr("now"),
            chips: timer.completedComponents.enumerated().map { builder.chip($0.offset, $0.element) },
            text: nil,
            isRunning: true,
            runningSince: timer.kind == .feed
                ? timer.currentBottleStartedAt ?? timer.currentSideStartedAt
                : timer.startedAt,
            target: .runningTimer(timer.id),
            deleteConfirmation: false
        )
    }
```

Delete the moved row builders from `HistoryViewModel`. `chipDuration`, `title` and `clockTime` stay.

- [ ] **Step 6: Run the History tests.**

Run: `cd Packages/BabyTracker && swift test --filter HistoryFeatureTests 2>&1 | tail -5`
Expected: every test passes, the three new ones included. If any existing test fails, the move changed behaviour: fix the code, not the test.

- [ ] **Step 7: Bump the count and run the full suite.** Raise `EXPECTED_TESTS` in `Makefile` by 3. Run `make test` and expect a PASS with the new total.

- [ ] **Step 8: Commit.**

```bash
git add Makefile Packages/BabyTracker/Sources/HistoryFeature Packages/BabyTracker/Tests/HistoryFeatureTests/HistoryViewModelTests.swift
git commit -m "refactor(history): one day builder, with a bottle total per day

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Header bottle total, shared phrases, catalog script

**Files:**
- Create: `scripts/add-catalog-strings.py`
- Modify: `Sources/HistoryFeature/HistoryDayTotals+Phrases.swift`
- Modify: `Sources/HistoryFeature/HistoryView.swift` (`HistoryDayHeader` at lines 507–573, and its use at line 151)
- Modify: `Sources/HistoryFeature/Localizable.xcstrings`
- Test: `Tests/HistoryFeatureTests/HistoryViewModelTests.swift`

**Interfaces:**
- Consumes: `HistoryDayTotals(feeds:sleep:bottleML:diapers:)` (Task 1)
- Produces: `HistoryDayTotals.phrases(volumeUnit: VolumeUnit) -> [String]`, giving e.g. `["8 feeds", "13h 20m sleep", "540 ml bottle", "6 diapers"]`. Also `scripts/add-catalog-strings.py CATALOG ENTRIES_JSON`.

- [ ] **Step 1: Write the failing tests.** Add to `HistoryViewModelTests.swift`:

```swift
// MARK: - Total phrases (VoiceOver and the PDF)

@Test func totalPhrasesReadInHeaderOrderAndSkipZeroes() {
    let totals = HistoryDayTotals(feeds: 8, sleep: "13h 20m", bottleML: 540, diapers: 1)
    #expect(totals.phrases(volumeUnit: .millilitres) == ["8 feeds", "13h 20m sleep", "540 ml bottle", "1 diaper"])
    #expect(HistoryDayTotals(feeds: 1, sleep: "", bottleML: 0, diapers: 0).phrases(volumeUnit: .millilitres) == ["1 feed"])
    #expect(HistoryDayTotals.none.phrases(volumeUnit: .millilitres).isEmpty)
}

@Test func theBottlePhraseFollowsTheVolumeUnit() {
    let totals = HistoryDayTotals(feeds: 0, sleep: "", bottleML: 236.588, diapers: 0)
    #expect(totals.phrases(volumeUnit: .ounces) == ["8 oz bottle"])
}
```

- [ ] **Step 2: Run them to confirm they fail.** Run `swift test --filter totalPhrases`. Expected: `value of type 'HistoryDayTotals' has no member 'phrases'`.

- [ ] **Step 3: Implement `phrases`.** Append inside the extension in `HistoryDayTotals+Phrases.swift`, and add `import BabyCore` at the top of the file:

```swift
    /// The totals a day shows, as words, in header order: "8 feeds",
    /// "13h 20m sleep", "540 ml bottle", "6 diapers". VoiceOver reads them
    /// joined with commas; the PDF prints them joined with " · ".
    func phrases(volumeUnit: VolumeUnit) -> [String] {
        var phrases: [String] = []
        if feeds > 0 { phrases.append(L10n.fmt(feeds == 1 ? "%lld feed" : "%lld feeds", feeds)) }
        if !sleep.isEmpty { phrases.append(L10n.fmt("%@ sleep", sleep)) }
        if bottleML > 0 { phrases.append(L10n.fmt("%@ bottle", Volume(millilitres: bottleML).formatted(as: volumeUnit))) }
        if diapers > 0 { phrases.append(L10n.fmt(diapers == 1 ? "%lld diaper" : "%lld diapers", diapers)) }
        return phrases
    }
```

- [ ] **Step 4: Show the bottle total in the header.** In `HistoryView.swift`:

1. `HistoryDayHeader` gains `let volumeUnit: VolumeUnit`. Pass it at line 151: `HistoryDayHeader(day: day, volumeUnit: model.volumeUnit)`.
2. Replace `totals`, `total(_:_:)` and `spokenTotals` with:

```swift
    private var totals: some View {
        HStack(spacing: 10) {
            if day.totals.feeds > 0 {
                total(Tracker.feed.icon, Tracker.feed.color, "\(day.totals.feeds)")
            }
            if !day.totals.sleep.isEmpty {
                total(Tracker.sleep.icon, Tracker.sleep.color, day.totals.sleep)
            }
            if day.totals.bottleML > 0 {
                total(.bottle, Tracker.feed.color, Volume(millilitres: day.totals.bottleML).formatted(as: volumeUnit))
            }
            if day.totals.diapers > 0 {
                total(Tracker.diaper.icon, Tracker.diaper.color, "\(day.totals.diapers)")
            }
        }
    }

    private func total(_ icon: BabyIcon, _ color: Color, _ value: String) -> some View {
        HStack(spacing: 3) {
            BabyIconView(icon, size: 14)
                .foregroundStyle(color)
            Text(value)
                .font(.footnote.weight(.bold))
                .monospacedDigit()
                .foregroundStyle(Palette.primaryText)
        }
    }

    private var spokenTotals: [String] { day.totals.phrases(volumeUnit: volumeUnit) }
```

3. Update the doc comment above `HistoryDayHeader` to: `/// "Today", then the day's icon totals: feeds, the asleep total, bottle volume, diapers.`

If `Tracker.color` is a `Color` but `BabyIconView` expects another style type, match the existing call's types. The old code passed `tracker.color` to `.foregroundStyle`, so `Color` is correct.

- [ ] **Step 5: Create the catalog script** at `scripts/add-catalog-strings.py` and make it executable (`chmod +x`):

```python
#!/usr/bin/env python3
"""Add plain (non-plural) translated keys to a String Catalog.

usage: add-catalog-strings.py CATALOG.xcstrings ENTRIES.json

ENTRIES.json maps each source-language key to {"de": "...", "es": "...", ...}.
A key already in the catalog is replaced. Other keys keep their order, so the
diff holds only the keys added.
"""
import json
import sys


def main() -> int:
    catalog_path, entries_path = sys.argv[1], sys.argv[2]
    with open(catalog_path, encoding="utf-8") as f:
        catalog = json.load(f)
    with open(entries_path, encoding="utf-8") as f:
        entries = json.load(f)
    for key, translations in entries.items():
        catalog["strings"][key] = {
            "localizations": {
                language: {"stringUnit": {"state": "translated", "value": value}}
                for language, value in translations.items()
            }
        }
    with open(catalog_path, "w", encoding="utf-8") as f:
        json.dump(catalog, f, ensure_ascii=False, indent=2, separators=(",", " : "))
        f.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

- [ ] **Step 6: Add the new string.** Write `/tmp/history-bottle.json`:

```json
{
  "%@ bottle": {"de": "%@ Flasche", "es": "%@ de biberón", "fr": "%@ au biberon", "it": "%@ di biberon", "uk": "Пляшечка: %@"}
}
```

Run: `python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/HistoryFeature/Localizable.xcstrings /tmp/history-bottle.json`

Then run `git diff --stat Packages/BabyTracker/Sources/HistoryFeature/Localizable.xcstrings`. Expect insertions only, apart from at most the final closing lines. If the whole file was rewritten, the script's formatting doesn't match Xcode's. In that case, change `separators`/`indent` until a no-op run (an empty JSON `{}`) leaves the file byte-identical, then re-run.

Run: `python3 scripts/verify-localizations.py`. Expected: exit 0.

- [ ] **Step 7: Run the tests.** Run `swift test --filter HistoryFeatureTests` and expect a PASS. Raise `EXPECTED_TESTS` by 2, then run `make test` and expect a PASS.

- [ ] **Step 8: Commit.**

```bash
git add Makefile scripts/add-catalog-strings.py Packages/BabyTracker/Sources/HistoryFeature Packages/BabyTracker/Tests/HistoryFeatureTests
git commit -m "feat(history): day headers show the bottle total

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: `HistoryReport` and `HistoryReportBuilder`

**Files:**
- Create: `Sources/HistoryFeature/HistoryReport.swift`
- Test: `Tests/HistoryFeatureTests/HistoryReportTests.swift`

**Interfaces:**
- Consumes: `HistoryDayBuilder.build(from:through:filter:)`, `BuiltEntry.note`, `HistoryDayTotals.phrases(volumeUnit:)`, `Tracker.title` (BabyUI)
- Produces:

```swift
public struct HistoryReport: Equatable {
    public struct Entry: Equatable { public let time: String; public let type: String; public let details: String; public let note: String? }
    public struct Day: Equatable, Identifiable { public let id: DayKey; public let title: String; public let totals: HistoryDayTotals; public let totalsLine: String; public let entries: [Entry] }
    public let babyName: String
    public let rangeText: String      // "1–7 Oct 2026"
    public let generatedText: String  // "8 October 2026 at 14:05"
    public let days: [Day]            // oldest first
}
@MainActor enum HistoryReportBuilder {
    static func make(store: BabyStore, babyID: UUID, babyName: String, from first: DayKey, through last: DayKey,
                     volumeUnit: VolumeUnit, zone: String = TimeZone.current.identifier) throws -> HistoryReport?
    static func rangeText(from first: DayKey, through last: DayKey, zone: String) -> String
}
```

`make` returns `nil` when no day in the range has an entry.

- [ ] **Step 1: Write the failing tests.** Create `Tests/HistoryFeatureTests/HistoryReportTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore
import BabyUI
@testable import HistoryFeature

@MainActor
private func localTime(_ hour: Int, _ minute: Int, dayOffset: Int = 0) -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15 + dayOffset
    parts.hour = hour; parts.minute = minute
    return Calendar.current.date(from: parts)!
}

@MainActor
private func makeStore() throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(localTime(12, 0))
    let store = try BabyStore.inMemory(dateProvider: clock)
    store.breakLimits = { .standard }
    let baby = try store.addBaby(
        name: "Mira", birthDate: localTime(12, 0, dayOffset: -90), referenceSex: .girls, expectedDueDate: nil
    )
    return (store, baby.id, clock)
}

@MainActor
private func key(_ dayOffset: Int) -> DayKey {
    DayBucket.key(for: localTime(12, 0, dayOffset: dayOffset), timeZoneID: TimeZone.current.identifier)
}

@MainActor
private func report(_ store: BabyStore, _ baby: UUID, from: Int, through: Int,
                    unit: VolumeUnit = .millilitres) throws -> HistoryReport? {
    try HistoryReportBuilder.make(store: store, babyID: baby, babyName: "Mira",
                                  from: key(from), through: key(through), volumeUnit: unit)
}

// MARK: - Report

@MainActor
@Test func aReportRunsOldestFirstAndSkipsEmptyDays() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(8, 0, dayOffset: -3))
    _ = try store.logDiaper(.dirty, for: baby, at: localTime(9, 0))
    _ = try store.logDiaper(.wet, for: baby, at: localTime(7, 0))

    let report = try #require(try report(store, baby, from: -6, through: 0))

    #expect(report.days.map(\.id) == [key(-3), key(0)])
    #expect(report.days[1].entries.map(\.details) == ["Wet", "Dirty"])
    #expect(report.days[1].totalsLine == "2 diapers")
    #expect(!report.days[1].title.contains("Today"))
}

@MainActor
@Test func aReportCoversEveryTrackerWithItsDetailsAndNotes() throws {
    let (store, baby, _) = try makeStore()
    let left = try store.logBreast(side: .left, from: localTime(6, 0), to: localTime(6, 12), for: baby)
    _ = try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: localTime(6, 20)), toSession: left.id, for: baby)
    _ = try store.logSleep(from: localTime(8, 0), to: localTime(9, 30), for: baby)
    _ = try store.logDiaper(.wet, for: baby, at: localTime(10, 0), note: "Rash on the left")
    _ = try store.logSupplement(name: "Vitamin D", for: baby, at: localTime(11, 0), note: "  ")

    let day = try #require(try report(store, baby, from: 0, through: 0)?.days.first)

    #expect(day.entries.map(\.type) == [Tracker.feed.title, Tracker.sleep.title, Tracker.diaper.title, Tracker.supplement.title])
    #expect(day.entries[0].details == "L 12m · 60 ml")
    #expect(day.entries[0].time == HistoryViewModel.clockTime(localTime(6, 0), zone: TimeZone.current.identifier)
            + "–" + HistoryViewModel.clockTime(localTime(6, 20), zone: TimeZone.current.identifier))
    #expect(day.entries[1].details == "1h 30m")
    #expect(day.entries[2].note == "Rash on the left")
    #expect(day.entries[3].details == "Vitamin D")
    #expect(day.entries[3].note == nil)
    #expect(day.totals == HistoryDayTotals(feeds: 1, sleep: "1h 30m", bottleML: 60, diapers: 1))
    #expect(day.totalsLine == "1 feed · 1h 30m sleep · 60 ml bottle · 1 diaper")
}

/// The export ignores History's filter: it is built with every tracker.
@MainActor
@Test func aFilteredHistoryStillExportsEveryTracker() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: localTime(9, 0))
    _ = try store.logDiaper(.wet, for: baby, at: localTime(10, 0))
    let model = HistoryViewModel(store: store, babyID: baby)
    model.filter = [.diaper]
    try model.load(dayCount: 1)

    let day = try #require(try report(store, baby, from: 0, through: 0)?.days.first)
    #expect(day.entries.count == 2)
    #expect(day.totals.bottleML == 90)
}

/// The first and last day are whole local days: 00:05 on the first and
/// 23:55 on the last are in; the minutes either side are out.
@MainActor
@Test func aReportIncludesItsFirstAndLastWholeDays() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(23, 55, dayOffset: -3))
    _ = try store.logDiaper(.wet, for: baby, at: localTime(0, 5, dayOffset: -2))
    _ = try store.logDiaper(.dirty, for: baby, at: localTime(23, 55, dayOffset: -1))
    _ = try store.logDiaper(.dirty, for: baby, at: localTime(0, 5))

    let report = try #require(try report(store, baby, from: -2, through: -1))
    #expect(report.days.map(\.id) == [key(-2), key(-1)])
    #expect(report.days.flatMap(\.entries).count == 2)
}

@MainActor
@Test func aRangeWithNothingLoggedMakesNoReport() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(9, 0))
    #expect(try report(store, baby, from: -10, through: -5) == nil)
}

/// Review focus 4: the PDF speaks the user's unit.
@MainActor
@Test func aReportUsesTheVolumeUnit() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logBottle(volumeML: 236.588, contents: .formula, for: baby, at: localTime(9, 0))
    let day = try #require(try report(store, baby, from: 0, through: 0, unit: .ounces)?.days.first)
    #expect(day.entries[0].details == "8 oz")
    #expect(day.totalsLine == "1 feed · 8 oz bottle")
}

/// Review focus 5: a running timer is not a finished entry and is not printed.
@MainActor
@Test func aRunningTimerIsNotExported() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(9, 0))
    _ = try store.startSleep(for: baby)
    let day = try #require(try report(store, baby, from: 0, through: 0)?.days.first)
    #expect(day.entries.map(\.type) == [Tracker.diaper.title])
}
```

- [ ] **Step 2: Run them to confirm they fail.** Run `swift test --filter HistoryReportTests`. Expected: `cannot find 'HistoryReportBuilder' in scope`.

- [ ] **Step 3: Implement `HistoryReport.swift`.**

```swift
import Foundation
import BabyCore
import BabyStore
import BabyUI

/// What the PDF prints, every string already in the reader's language: the
/// baby, the dates, then each day oldest first — the day's totals and its
/// entries in the order they happened, as a diary reads.
public struct HistoryReport: Equatable {
    public struct Entry: Equatable {
        /// "10:23", or "10:23–11:05" for an entry with a length.
        public let time: String
        /// The tracker's name: "Feed", "Sleep", "Diaper", "Meds", "Growth".
        public let type: String
        /// History's own words for the entry: its chips joined with " · ",
        /// or its text.
        public let details: String
        public let note: String?
    }

    public struct Day: Equatable, Identifiable {
        public let id: DayKey
        /// The full date. Never "Today": a printout is read on other days.
        public let title: String
        public let totals: HistoryDayTotals
        /// "8 feeds · 13h 20m sleep · 540 ml bottle · 6 diapers".
        public let totalsLine: String
        public let entries: [Entry]
    }

    public let babyName: String
    public let rangeText: String
    public let generatedText: String
    public let days: [Day]
}

@MainActor
enum HistoryReportBuilder {
    /// The report of every tracker from the start of `first` to the end of
    /// `last`, or nil when nothing was logged in those days.
    static func make(store: BabyStore, babyID: UUID, babyName: String, from first: DayKey, through last: DayKey,
                     volumeUnit: VolumeUnit, zone: String = TimeZone.current.identifier) throws -> HistoryReport? {
        let builder = HistoryDayBuilder(store: store, babyID: babyID, volumeUnit: volumeUnit, zone: zone)
        let built = try builder.build(from: first, through: last, filter: Set(HistoryViewModel.trackers))
        let days = built.days.keys.sorted().compactMap { key -> HistoryReport.Day? in
            guard let day = built.days[key], !day.entries.isEmpty else { return nil }
            return HistoryReport.Day(
                id: key,
                title: title(key, zone: zone),
                totals: day.totals,
                totalsLine: day.totals.phrases(volumeUnit: volumeUnit).joined(separator: " · "),
                entries: day.entries.reversed().map(entry)
            )
        }
        guard !days.isEmpty else { return nil }
        return HistoryReport(
            babyName: babyName,
            rangeText: rangeText(from: first, through: last, zone: zone),
            generatedText: store.now.formatted(date: .long, time: .shortened),
            days: days
        )
    }

    /// "1–7 Oct 2026"; a single day reads as that one date.
    static func rangeText(from first: DayKey, through last: DayKey, zone: String) -> String {
        let timeZone = TimeZone(identifier: zone) ?? .gmt
        let start = DayBucket.bounds(of: first, timeZoneID: zone).start
        let end = DayBucket.bounds(of: last, timeZoneID: zone).start
        if first == last {
            var style = Date.FormatStyle(date: .abbreviated, time: .omitted)
            style.timeZone = timeZone
            return start.formatted(style)
        }
        var calendar = Calendar.current
        calendar.timeZone = timeZone
        let style = Date.IntervalFormatStyle(date: .abbreviated, time: .omitted, calendar: calendar, timeZone: timeZone)
        return (start..<end).formatted(style)
    }

    private static func title(_ key: DayKey, zone: String) -> String {
        var style = Date.FormatStyle(date: .complete, time: .omitted)
        style.timeZone = TimeZone(identifier: zone) ?? .gmt
        return DayBucket.bounds(of: key, timeZoneID: zone).start.formatted(style)
    }

    private static func entry(_ built: BuiltEntry) -> HistoryReport.Entry {
        let row = built.row
        return HistoryReport.Entry(
            time: row.end.map { row.start + "–" + $0 } ?? row.start,
            type: row.tracker.title,
            details: row.chips.isEmpty ? row.text ?? "" : row.chips.map(\.text).joined(separator: " · "),
            note: built.note
        )
    }
}
```

- [ ] **Step 4: Run the tests.** Run `swift test --filter HistoryReportTests`. Expected: 7 passing.
  - If `"1h 30m"` differs, check `ElapsedFormatter.total` for 5400 s. Use the formatter's output in the expectation, and do not change the formatter.
  - If `startSleep` writes through `withRealmWrite` and needs a write context, pass nothing. The default captures it.

- [ ] **Step 5: Bump the count, run the suite and commit.** Raise `EXPECTED_TESTS` by 7 and run `make test`, expecting a PASS.

```bash
git add Makefile Packages/BabyTracker/Sources/HistoryFeature/HistoryReport.swift Packages/BabyTracker/Tests/HistoryFeatureTests/HistoryReportTests.swift
git commit -m "feat(history): build a printable report for a date range

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: HTML renderer

**Files:**
- Create: `Sources/HistoryFeature/HistoryReportHTML.swift`
- Modify: `Sources/HistoryFeature/HistoryView.swift` (`HistoryCopy`: add the column labels)
- Modify: `Sources/HistoryFeature/Localizable.xcstrings`
- Test: `Tests/HistoryFeatureTests/HistoryReportTests.swift`

**Interfaces:**
- Consumes: `HistoryReport` (Task 3)
- Produces: `enum HistoryReportHTML { static func render(_ report: HistoryReport) -> String; static func escape(_ text: String) -> String }`

- [ ] **Step 1: Write the failing tests.** Append to `HistoryReportTests.swift`:

```swift
// MARK: - HTML

private func sampleReport(name: String = "Mira", note: String? = nil, days: Int = 1, entriesPerDay: Int = 1) -> HistoryReport {
    HistoryReport(
        babyName: name,
        rangeText: "1–7 Oct 2026",
        generatedText: "8 October 2026 at 14:05",
        days: (0..<days).map { index in
            HistoryReport.Day(
                id: DayKey(year: 2026, month: 10, day: 1 + index),
                title: "Thursday, \(1 + index) October 2026",
                totals: HistoryDayTotals(feeds: 2, sleep: "3h", bottleML: 120, diapers: 1),
                totalsLine: "2 feeds · 3h sleep · 120 ml bottle · 1 diaper",
                entries: (0..<entriesPerDay).map { _ in
                    HistoryReport.Entry(time: "10:23–11:05", type: "Feed", details: "L 12m · 60 ml", note: note)
                }
            )
        }
    )
}

@Test func escapingNeutralisesMarkup() {
    #expect(HistoryReportHTML.escape(#"<script>"a" & 'b'</script>"#)
            == "&lt;script&gt;&quot;a&quot; &amp; &#39;b&#39;&lt;/script&gt;")
}

/// Review focus 2: a name or note is text, never markup.
@Test func userTextIsEscapedInTheDocument() {
    let html = HistoryReportHTML.render(sampleReport(name: "Mia & <b>Lev</b>", note: "Spit up <a lot>\nthen slept"))
    #expect(html.contains("Mia &amp; &lt;b&gt;Lev&lt;/b&gt;"))
    #expect(html.contains("Spit up &lt;a lot&gt;<br>then slept"))
    #expect(!html.contains("<b>Lev"))
}

@Test func everyDayPrintsItsTitleTotalsAndRows() {
    let html = HistoryReportHTML.render(sampleReport(days: 2, entriesPerDay: 3))
    #expect(html.components(separatedBy: "<section").count - 1 == 2)
    #expect(html.contains("2 feeds · 3h sleep · 120 ml bottle · 1 diaper"))
    #expect(html.components(separatedBy: "<tr class=\"entry\">").count - 1 == 6)
    #expect(html.contains("display: table-header-group"))
    #expect(html.contains("1–7 Oct 2026"))
}
```

- [ ] **Step 2: Run them to confirm they fail.** Run `swift test --filter HistoryReportTests`. Expected: `cannot find 'HistoryReportHTML'`.

- [ ] **Step 3: Add the copy.** In `HistoryCopy` (`HistoryView.swift`, after `done`):

```swift
    static let exportPDF = L10n.tr("Export PDF")
    static let columnTime = L10n.tr("Time")
    static let columnType = L10n.tr("Type")
    static let columnDetails = L10n.tr("Details")
    static let columnNote = L10n.tr("Note")

    static func generated(_ when: String) -> String { L10n.fmt("Generated %@", when) }
```

- [ ] **Step 4: Implement `HistoryReportHTML.swift`.**

```swift
import Foundation

/// The report as one HTML page for iOS to print. Pure: the same report gives
/// the same text, on any platform. Every string from the user — names,
/// notes, foods, medicines — is escaped; nothing in the data can become
/// markup.
enum HistoryReportHTML {
    static func render(_ report: HistoryReport) -> String {
        var html = """
        <!DOCTYPE html>
        <html lang="\(escape(L10n.language.identifier))"><head><meta charset="utf-8"><style>\(css)</style></head><body>
        <header><h1>\(escape(report.babyName))</h1>
        <p class="range">\(escape(report.rangeText))</p>
        <p class="generated">\(escape(HistoryCopy.generated(report.generatedText)))</p></header>

        """
        for day in report.days {
            html += "<section><h2>\(escape(day.title))</h2>"
            if !day.totalsLine.isEmpty {
                html += "<p class=\"totals\">\(escape(day.totalsLine))</p>"
            }
            html += """
            <table><thead><tr><th class="time">\(escape(HistoryCopy.columnTime))</th>\
            <th class="type">\(escape(HistoryCopy.columnType))</th>\
            <th>\(escape(HistoryCopy.columnDetails))</th>\
            <th>\(escape(HistoryCopy.columnNote))</th></tr></thead><tbody>
            """
            for entry in day.entries {
                html += "<tr class=\"entry\"><td class=\"time\">\(escape(entry.time))</td>"
                    + "<td class=\"type\">\(escape(entry.type))</td>"
                    + "<td>\(escape(entry.details))</td>"
                    + "<td class=\"note\">\(entry.note.map(multiline) ?? "")</td></tr>"
            }
            html += "</tbody></table></section>\n"
        }
        return html + "</body></html>"
    }

    static func escape(_ text: String) -> String {
        var escaped = ""
        escaped.reserveCapacity(text.count)
        for character in text {
            switch character {
            case "&": escaped += "&amp;"
            case "<": escaped += "&lt;"
            case ">": escaped += "&gt;"
            case "\"": escaped += "&quot;"
            case "'": escaped += "&#39;"
            default: escaped.append(character)
            }
        }
        return escaped
    }

    /// A note keeps its line breaks.
    private static func multiline(_ text: String) -> String {
        escape(text).replacingOccurrences(of: "\n", with: "<br>")
    }

    /// Print styles: a day's heading and totals stay with its first rows, a
    /// row never splits, and a table cut by a page repeats its headings.
    private static let css = """
    body { font: 10.5pt -apple-system, "Helvetica Neue", Helvetica, sans-serif; color: #111; margin: 0; }
    h1 { font-size: 20pt; margin: 0 0 2pt; }
    .range { font-size: 12pt; margin: 0; }
    .generated { font-size: 8.5pt; color: #666; margin: 2pt 0 10pt; }
    section { margin-top: 14pt; }
    h2 { font-size: 12.5pt; margin: 0 0 2pt; page-break-after: avoid; }
    .totals { font-weight: 600; margin: 0 0 4pt; page-break-after: avoid; }
    table { width: 100%; border-collapse: collapse; }
    thead { display: table-header-group; }
    tr { page-break-inside: avoid; }
    th { text-align: left; font-weight: 600; color: #444; border-bottom: 1px solid #999; padding: 2pt 4pt; }
    td { border-bottom: 1px solid #ddd; padding: 3pt 4pt; vertical-align: top; }
    .time { white-space: nowrap; width: 17%; }
    .type { width: 12%; }
    .note { color: #444; }
    """
}
```

- [ ] **Step 5: Add the strings.** Write `/tmp/history-html.json`:

```json
{
  "Export PDF": {"de": "Als PDF exportieren", "es": "Exportar PDF", "fr": "Exporter en PDF", "it": "Esporta PDF", "uk": "Експорт у PDF"},
  "Time": {"de": "Uhrzeit", "es": "Hora", "fr": "Heure", "it": "Ora", "uk": "Час"},
  "Type": {"de": "Art", "es": "Tipo", "fr": "Type", "it": "Tipo", "uk": "Тип"},
  "Details": {"de": "Details", "es": "Detalles", "fr": "Détails", "it": "Dettagli", "uk": "Подробиці"},
  "Note": {"de": "Notiz", "es": "Nota", "fr": "Note", "it": "Nota", "uk": "Нотатка"},
  "Generated %@": {"de": "Erstellt am %@", "es": "Generado el %@", "fr": "Généré le %@", "it": "Generato il %@", "uk": "Створено %@"}
}
```

Run `python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/HistoryFeature/Localizable.xcstrings /tmp/history-html.json`, then `python3 scripts/verify-localizations.py`. Expected: exit 0.

- [ ] **Step 6: Run the tests.** Run `swift test --filter HistoryReportTests`. Expected: 10 passing.
- [ ] **Step 7: Bump the count, run the suite and commit.** Raise `EXPECTED_TESTS` by 3 and run `make test`, expecting a PASS.

```bash
git add Makefile Packages/BabyTracker/Sources/HistoryFeature Packages/BabyTracker/Tests/HistoryFeatureTests/HistoryReportTests.swift
git commit -m "feat(history): render the report as printable HTML

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: PDF printer and export files

**Files:**
- Create: `Sources/HistoryFeature/HistoryPDF.swift`
- Test: `Tests/HistoryFeatureTests/HistoryReportTests.swift`

**Interfaces:**
- Consumes: `HistoryReportHTML.render`, `HistoryReport.babyName`, `HistoryReport.rangeText`
- Produces:

```swift
enum PaperSize: Equatable { case a4, letter
    var size: CGSize
    static func `for`(_ locale: Locale) -> PaperSize }
#if canImport(UIKit)
@MainActor enum HistoryPDFPrinter { static func pdf(fromHTML html: String, paper: PaperSize) -> Data }
#endif
final class HistoryExportFiles {
    init(directory: URL? = nil) throws
    let directory: URL
    static func fileName(babyName: String, rangeText: String) -> String
    func write(_ data: Data, named name: String) throws -> URL   // empties the folder first
}
```

- [ ] **Step 1: Write the failing tests.** Append to `HistoryReportTests.swift`:

```swift
// MARK: - Files and paper

@Test func paperFollowsTheMeasurementSystem() {
    #expect(PaperSize.for(Locale(identifier: "en_US")) == .letter)
    #expect(PaperSize.for(Locale(identifier: "en_GB")) == .a4)
    #expect(PaperSize.for(Locale(identifier: "uk_UA")) == .a4)
    #expect(PaperSize.letter.size == CGSize(width: 612, height: 792))
}

/// Review focus 2: a name with path characters still makes one valid file.
@Test func theFileNameIsSafe() {
    #expect(HistoryExportFiles.fileName(babyName: "Mia/Lev: \"twins\"", rangeText: "1–7 Oct 2026")
            == "Mia-Lev- -twins- – 1–7 Oct 2026.pdf")
    #expect(HistoryExportFiles.fileName(babyName: "  ", rangeText: "1–7 Oct 2026") == "History – 1–7 Oct 2026.pdf")
}

@Test func writingAnExportClearsTheLastOne() throws {
    let folder = FileManager.default.temporaryDirectory.appendingPathComponent("history-export-test-\(UUID())")
    defer { try? FileManager.default.removeItem(at: folder) }
    let files = try HistoryExportFiles(directory: folder)

    let first = try files.write(Data("one".utf8), named: "A.pdf")
    let second = try files.write(Data("two".utf8), named: "B.pdf")

    #expect(!FileManager.default.fileExists(atPath: first.path))
    #expect(try Data(contentsOf: second) == Data("two".utf8))
    #expect(try FileManager.default.contentsOfDirectory(atPath: folder.path) == ["B.pdf"])
}

#if canImport(UIKit)
/// Review focus 1: a long report runs over several pages. Not counted by
/// `make test` (macOS); run on the shared simulator.
@MainActor
@Test func aLongReportPrintsToSeveralPages() throws {
    let html = HistoryReportHTML.render(sampleReport(note: String(repeating: "Long note. ", count: 30), days: 10, entriesPerDay: 12))
    let data = HistoryPDFPrinter.pdf(fromHTML: html, paper: .a4)
    let provider = try #require(CGDataProvider(data: data as CFData))
    let document = try #require(CGPDFDocument(provider))
    #expect(document.numberOfPages > 1)
    let box = try #require(document.page(at: 1)).getBoxRect(.mediaBox)
    #expect(abs(box.width - 595.28) < 1)
}
#endif
```

Also add these imports at the top of `HistoryReportTests.swift`, after the existing ones:

```swift
import CoreGraphics
#if canImport(UIKit)
import UIKit
#endif
```

- [ ] **Step 2: Run them to confirm they fail.** Run `swift test --filter HistoryReportTests`. Expected: `cannot find 'PaperSize'`.

- [ ] **Step 3: Implement `HistoryPDF.swift`.**

```swift
import Foundation
import CoreGraphics
#if canImport(UIKit)
import UIKit
#endif

/// The sheet the PDF is laid out for: Letter where the region measures in
/// US units, A4 everywhere else.
enum PaperSize: Equatable {
    case a4, letter

    var size: CGSize {
        switch self {
        case .a4: CGSize(width: 595.28, height: 841.89)
        case .letter: CGSize(width: 612, height: 792)
        }
    }

    static func `for`(_ locale: Locale) -> PaperSize {
        locale.measurementSystem == .us ? .letter : .a4
    }
}

#if canImport(UIKit)
/// HTML to PDF through iOS's own print system: it lays out the pages, breaks
/// tables between them, and keeps the text selectable.
@MainActor
enum HistoryPDFPrinter {
    static let margin: CGFloat = 36

    static func pdf(fromHTML html: String, paper: PaperSize) -> Data {
        let page = CGRect(origin: .zero, size: paper.size)
        let renderer = UIPrintPageRenderer()
        renderer.addPrintFormatter(UIMarkupTextPrintFormatter(markupText: html), startingAtPageAt: 0)
        // UIPrintPageRenderer reads its page from these two read-only
        // properties; key-value coding is the documented way to set them
        // when rendering without a printer.
        renderer.setValue(NSValue(cgRect: page), forKey: "paperRect")
        renderer.setValue(NSValue(cgRect: page.insetBy(dx: margin, dy: margin)), forKey: "printableRect")

        return UIGraphicsPDFRenderer(bounds: page).pdfData { context in
            let pages = renderer.numberOfPages
            renderer.prepare(forDrawingPages: NSRange(location: 0, length: pages))
            for index in 0..<pages {
                context.beginPage()
                renderer.drawPage(at: index, in: context.pdfContextBounds)
            }
        }
    }
}
#endif

/// The app-private folder an export is written to before it is shared. It
/// holds one file at a time: each new export empties it first.
final class HistoryExportFiles {
    let directory: URL
    private let manager = FileManager.default

    init(directory: URL? = nil) throws {
        let base = directory ?? FileManager.default.temporaryDirectory
            .appendingPathComponent("baby-tracker-history-exports", isDirectory: true)
        self.directory = base
        try manager.createDirectory(at: base, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var mutable = base
        try mutable.setResourceValues(values)
    }

    /// "Mira – 1–7 Oct 2026.pdf". Characters a file name cannot hold become
    /// "-"; a blank name falls back to "History".
    static func fileName(babyName: String, rangeText: String) -> String {
        let trimmed = babyName.trimmingCharacters(in: .whitespacesAndNewlines)
        let name = trimmed.isEmpty ? HistoryCopy.title : trimmed
        let unsafe = CharacterSet(charactersIn: "/\\:?%*|\"<>").union(.newlines).union(.controlCharacters)
        let safe = String(String.UnicodeScalarView(name.unicodeScalars.map { unsafe.contains($0) ? "-" : $0 }))
        return "\(safe) – \(rangeText).pdf"
    }

    func write(_ data: Data, named name: String) throws -> URL {
        for file in try manager.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil) {
            try manager.removeItem(at: file)
        }
        let file = directory.appendingPathComponent(name)
        try data.write(to: file, options: [.atomic])
        #if os(iOS)
        try manager.setAttributes([.protectionKey: FileProtectionType.complete], ofItemAtPath: file.path)
        #endif
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var mutable = file
        try mutable.setResourceValues(values)
        return file
    }
}
```

The expected name in `theFileNameIsSafe` is `Mia-Lev- -twins-`: `/` becomes `-`, `: ` becomes `- `, and each `"` becomes `-`. `HistoryCopy.title` is `"History"` in English, which is the test's locale.

- [ ] **Step 4: Run the macOS tests.** Run `swift test --filter HistoryReportTests`. Expected: 13 passing.

- [ ] **Step 5: Run the UIKit test on the shared simulator.**

Run `pgrep xcodebuild`. It must print nothing. Then:

```bash
cd Packages/BabyTracker && xcodebuild test -scheme BabyTracker-Package \
  -destination 'platform=iOS Simulator,id=<simulator-udid>' \
  -parallel-testing-enabled NO -only-testing:HistoryFeatureTests 2>&1 | tail -30
```

Expected: `** TEST SUCCEEDED **`, with `aLongReportPrintsToSeveralPages` passed. Swift Testing tests may not respect `-only-testing` for free functions. If so, run without `-only-testing` and grep the output for `aLongReportPrintsToSeveralPages`.

If the page count is 0 or the data is empty, the formatter didn't lay out synchronously. Fix it by making `pdf(fromHTML:paper:)` call `renderer.numberOfPages` **before** creating the `UIGraphicsPDFRenderer`, which forces layout. Re-run the test.

- [ ] **Step 6: Bump the count, run the suite and commit.** Raise `EXPECTED_TESTS` by 3: the UIKit test isn't counted. Run `make test` and expect a PASS.

```bash
git add Makefile Packages/BabyTracker/Sources/HistoryFeature/HistoryPDF.swift Packages/BabyTracker/Tests/HistoryFeatureTests/HistoryReportTests.swift
git commit -m "feat(history): print the report to PDF in a private export folder

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: `HistoryExportViewModel`

**Files:**
- Create: `Sources/HistoryFeature/HistoryExportViewModel.swift`
- Test: `Tests/HistoryFeatureTests/HistoryExportViewModelTests.swift`

**Interfaces:**
- Consumes: `HistoryReportBuilder.make`, `HistoryReportHTML.render`, `HistoryPDFPrinter.pdf`, `PaperSize.for`, `HistoryExportFiles`
- Produces:

```swift
enum HistoryExportPreset: Int, CaseIterable, Identifiable { case week = 7, fortnight = 14, month = 30, custom = 0 }
enum HistoryExportState: Equatable { case idle, working, nothingToExport, failed, ready(URL) }
@MainActor @Observable final class HistoryExportViewModel {
    init(store: BabyStore, babyID: UUID, babyName: String, volumeUnit: VolumeUnit,
         files: HistoryExportFiles? = nil, render: ((HistoryReport) throws -> Data)? = nil)
    var preset: HistoryExportPreset { get }
    var from: Date { get }; var to: Date { get }
    let earliest: Date; let latest: Date
    var state: HistoryExportState { get }
    func select(_ preset: HistoryExportPreset)
    func setFrom(_ date: Date); func setTo(_ date: Date)
    func create() async
    func shareDidFinish()
}
```

`from`, `to`, `earliest` and `latest` are each the **start** of their local day.

- [ ] **Step 1: Write the failing tests.** Create `Tests/HistoryFeatureTests/HistoryExportViewModelTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import HistoryFeature

@MainActor
private func day(_ offset: Int, _ hour: Int = 0) -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15 + offset; parts.hour = hour
    return Calendar.current.date(from: parts)!
}

@MainActor
private func makeModel(bornDaysAgo: Int = 90, render: ((HistoryReport) throws -> Data)? = { _ in Data("%PDF".utf8) })
    throws -> (HistoryExportViewModel, BabyStore, UUID, URL) {
    let clock = TestDateProvider(day(0, 12))
    let store = try BabyStore.inMemory(dateProvider: clock)
    store.breakLimits = { .standard }
    let baby = try store.addBaby(name: "Mira", birthDate: day(-bornDaysAgo, 9), referenceSex: .girls, expectedDueDate: nil)
    let folder = FileManager.default.temporaryDirectory.appendingPathComponent("history-export-vm-\(UUID())")
    let model = HistoryExportViewModel(store: store, babyID: baby.id, babyName: "Mira", volumeUnit: .millilitres,
                                       files: try HistoryExportFiles(directory: folder), render: render)
    return (model, store, baby.id, folder)
}

@MainActor
@Test func presetsEndTodayAndCountTodayIn() throws {
    let (model, _, _, folder) = try makeModel()
    defer { try? FileManager.default.removeItem(at: folder) }
    #expect(model.preset == .week)
    #expect(model.from == day(-6)); #expect(model.to == day(0))
    model.select(.fortnight); #expect(model.from == day(-13)); #expect(model.to == day(0))
    model.select(.month); #expect(model.from == day(-29))
    model.select(.custom); #expect(model.from == day(-29)); #expect(model.to == day(0))
}

/// A baby younger than the preset starts on its birth day.
@MainActor
@Test func aPresetNeverReachesBeforeBirth() throws {
    let (model, _, _, folder) = try makeModel(bornDaysAgo: 3)
    defer { try? FileManager.default.removeItem(at: folder) }
    #expect(model.earliest == day(-3))
    #expect(model.from == day(-3))
}

/// Review focus 3: custom dates stay between birth and today, in order.
@MainActor
@Test func customDatesClampAndStayInOrder() throws {
    let (model, _, _, folder) = try makeModel(bornDaysAgo: 20)
    defer { try? FileManager.default.removeItem(at: folder) }
    model.select(.custom)
    model.setFrom(day(-400, 15)); #expect(model.from == day(-20))
    model.setTo(day(30, 8)); #expect(model.to == day(0))
    model.setTo(day(-10)); model.setFrom(day(-5)); #expect(model.from == day(-5)); #expect(model.to == day(-5))
    model.setFrom(day(-12)); model.setTo(day(-15)); #expect(model.from == day(-15)); #expect(model.to == day(-15))
}

@MainActor
@Test func creatingWritesTheFileAndSharesIt() async throws {
    let (model, store, baby, folder) = try makeModel()
    defer { try? FileManager.default.removeItem(at: folder) }
    _ = try store.logDiaper(.wet, for: baby, at: day(-1, 9))

    await model.create()

    guard case .ready(let url) = model.state else { Issue.record("state \(model.state)"); return }
    #expect(url.lastPathComponent.hasPrefix("Mira – "))
    #expect(url.pathExtension == "pdf")
    #expect(try Data(contentsOf: url) == Data("%PDF".utf8))
    model.shareDidFinish()
    #expect(model.state == .idle)
}

@MainActor
@Test func aRangeWithNothingSaysSoAndWritesNothing() async throws {
    let (model, _, _, folder) = try makeModel()
    defer { try? FileManager.default.removeItem(at: folder) }
    await model.create()
    #expect(model.state == .nothingToExport)
    #expect(try FileManager.default.contentsOfDirectory(atPath: folder.path).isEmpty)
}

@MainActor
@Test func aRenderFailureIsReported() async throws {
    struct Boom: Error {}
    let (model, store, baby, folder) = try makeModel(render: { _ in throw Boom() })
    defer { try? FileManager.default.removeItem(at: folder) }
    _ = try store.logDiaper(.wet, for: baby, at: day(0, 9))
    await model.create()
    #expect(model.state == .failed)
}
```

- [ ] **Step 2: Run them to confirm they fail.** Run `swift test --filter HistoryExportViewModelTests`. Expected: `cannot find 'HistoryExportViewModel'`.

- [ ] **Step 3: Implement `HistoryExportViewModel.swift`.**

```swift
import Foundation
import Observation
import BabyCore
import BabyStore

/// The export sheet's range choices. A preset is that many days ending
/// today, today included.
enum HistoryExportPreset: Int, CaseIterable, Identifiable {
    case week = 7, fortnight = 14, month = 30, custom = 0
    var id: Int { rawValue }
}

enum HistoryExportState: Equatable {
    case idle
    case working
    /// Nothing was logged in the chosen dates; no file was made.
    case nothingToExport
    case failed
    /// The PDF is written and waiting for the share sheet.
    case ready(URL)
}

/// Picks the dates, builds the report, prints it and hands the file to the
/// share sheet. Dates are whole local days, never before the baby's birth
/// day nor after today, and From never passes To.
@MainActor
@Observable
final class HistoryExportViewModel {
    private(set) var preset: HistoryExportPreset = .week
    private(set) var from: Date
    private(set) var to: Date
    private(set) var state: HistoryExportState = .idle
    let earliest: Date
    let latest: Date

    private let store: BabyStore
    private let babyID: UUID
    private let babyName: String
    private let volumeUnit: VolumeUnit
    private let zone = TimeZone.current.identifier
    private let files: HistoryExportFiles?
    private let render: (HistoryReport) throws -> Data

    init(store: BabyStore, babyID: UUID, babyName: String, volumeUnit: VolumeUnit,
         files: HistoryExportFiles? = nil, render: ((HistoryReport) throws -> Data)? = nil) {
        self.store = store
        self.babyID = babyID
        self.babyName = babyName
        self.volumeUnit = volumeUnit
        self.files = files ?? (try? HistoryExportFiles())
        self.render = render ?? Self.printPDF
        let zone = TimeZone.current.identifier
        func startOfDay(_ date: Date) -> Date {
            DayBucket.bounds(of: DayBucket.key(for: date, timeZoneID: zone), timeZoneID: zone).start
        }
        let latest = startOfDay(store.now)
        let birth = (try? store.baby(babyID))?.birthDate
        let earliest = birth.map { min(startOfDay($0), latest) } ?? .distantPast
        self.latest = latest
        self.earliest = earliest
        self.to = latest
        self.from = latest
        select(.week)
    }

    func select(_ preset: HistoryExportPreset) {
        self.preset = preset
        guard preset != .custom else { return }
        to = latest
        from = clamp(Calendar.current.date(byAdding: .day, value: -(preset.rawValue - 1), to: latest) ?? latest)
    }

    func setFrom(_ date: Date) {
        from = clamp(date)
        if from > to { to = from }
    }

    func setTo(_ date: Date) {
        to = clamp(date)
        if to < from { from = to }
    }

    func create() async {
        state = .working
        // Lets the sheet draw its spinner before the main actor is busy
        // laying out pages.
        await Task.yield()
        do {
            guard let report = try HistoryReportBuilder.make(
                store: store, babyID: babyID, babyName: babyName,
                from: key(from), through: key(to), volumeUnit: volumeUnit, zone: zone
            ) else {
                state = .nothingToExport
                return
            }
            guard let files else { throw CocoaError(.fileWriteUnknown) }
            let data = try render(report)
            let name = HistoryExportFiles.fileName(babyName: babyName, rangeText: report.rangeText)
            state = .ready(try files.write(data, named: name))
        } catch {
            state = .failed
        }
    }

    func shareDidFinish() {
        if case .ready = state { state = .idle }
    }

    /// The start of the date's local day, between birth and today.
    private func clamp(_ date: Date) -> Date {
        let day = DayBucket.bounds(of: key(date), timeZoneID: zone).start
        return min(max(day, earliest), latest)
    }

    private func key(_ date: Date) -> DayKey { DayBucket.key(for: date, timeZoneID: zone) }

    private static func printPDF(_ report: HistoryReport) throws -> Data {
        #if canImport(UIKit)
        HistoryPDFPrinter.pdf(fromHTML: HistoryReportHTML.render(report), paper: .for(.current))
        #else
        throw CocoaError(.featureUnsupported)
        #endif
    }
}
```

`store.baby(_:)` returns `Baby?` and throws. `try?` flattens that to one optional, so a store error leaves the range unbounded below rather than failing the sheet. `printPDF` is a static on a `@MainActor` class, so it runs on the main actor as the printer requires.

- [ ] **Step 4: Run the tests.** Run `swift test --filter HistoryExportViewModelTests`. Expected: 6 passing.
  - If `presetsEndTodayAndCountTodayIn` fails by an hour, `Calendar.current` and the `DayBucket` calendar disagree, for example on a DST day. Compute presets with `DayBucket.calendar(for: zone)`. It is `static` but internal to `BabyCore`, so if you need it, take `Calendar(identifier: .gregorian)` with `timeZone = TimeZone(identifier: zone)`.

- [ ] **Step 5: Bump the count, run the suite and commit.** Raise `EXPECTED_TESTS` by 6 and run `make test`, expecting a PASS.

```bash
git add Makefile Packages/BabyTracker/Sources/HistoryFeature/HistoryExportViewModel.swift Packages/BabyTracker/Tests/HistoryFeatureTests/HistoryExportViewModelTests.swift
git commit -m "feat(history): export range, clamping and create-to-share flow

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Export sheet, header button, shared share sheet, UI test

**Files:**
- Create: `Sources/BabyUI/ActivityShareSheet.swift`
- Modify: `Sources/SettingsFeature/FamilyExportView.swift:107-121` (delete the private `ActivityShareSheet`)
- Create: `Sources/HistoryFeature/HistoryExportSheet.swift`
- Modify: `Sources/HistoryFeature/HistoryView.swift` (header, a new `.sheet`, the identifier doc comment at lines 43–50)
- Modify: `Sources/HistoryFeature/Localizable.xcstrings`
- Modify: `UITests/BabyTrackerUITests/HistoryRedesignTests.swift`

**Interfaces:**
- Consumes: `HistoryExportViewModel`, `HistoryExportPreset`, `HistoryExportState` (Task 6), `HistoryCopy.exportPDF` (Task 4)
- Produces: `public struct ActivityShareSheet: UIViewControllerRepresentable { public init(file: URL) }` in `BabyUI`, iOS only

- [ ] **Step 1: Move the share sheet.** Create `Sources/BabyUI/ActivityShareSheet.swift`:

```swift
#if os(iOS)
import SwiftUI
import UIKit

/// The system share sheet for one file: Print, Save to Files, AirDrop,
/// Mail and the rest.
public struct ActivityShareSheet: UIViewControllerRepresentable {
    let file: URL

    public init(file: URL) { self.file = file }

    public func makeUIViewController(context: Context) -> UIActivityViewController {
        UIActivityViewController(activityItems: [file], applicationActivities: nil)
    }

    public func updateUIViewController(_ controller: UIActivityViewController, context: Context) {}
}
#endif
```

In `FamilyExportView.swift`, delete the `private struct ActivityShareSheet` (lines 113–121), and add `import BabyUI` if it isn't already imported. Run `swift build`. Expected: success.

- [ ] **Step 2: Create `HistoryExportSheet.swift`.**

```swift
import SwiftUI
import BabyUI

/// Export PDF: a range — a preset or two dates — then Create PDF, which
/// opens the share sheet with the file.
///
/// Accessibility identifiers: `history.export.preset`, `history.export.from`,
/// `history.export.to`, `history.export.create`, `history.export.cancel`,
/// `history.export.empty`, `history.export.failed`.
struct HistoryExportSheet: View {
    @State var model: HistoryExportViewModel
    let onDone: () -> Void

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Picker(ExportCopy.range, selection: Binding(get: { model.preset }, set: { model.select($0) })) {
                        ForEach(HistoryExportPreset.allCases) { Text(ExportCopy.name($0)).tag($0) }
                    }
                    .pickerStyle(.segmented)
                    .accessibilityIdentifier("history.export.preset")
                    if model.preset == .custom {
                        DatePicker(ExportCopy.from, selection: Binding(get: { model.from }, set: { model.setFrom($0) }),
                                   in: model.earliest...model.latest, displayedComponents: .date)
                            .accessibilityIdentifier("history.export.from")
                        DatePicker(ExportCopy.to, selection: Binding(get: { model.to }, set: { model.setTo($0) }),
                                   in: model.earliest...model.latest, displayedComponents: .date)
                            .accessibilityIdentifier("history.export.to")
                    }
                }
                Section {
                    Button { Task { await model.create() } } label: {
                        HStack {
                            Text(ExportCopy.create)
                            Spacer()
                            if model.state == .working { ProgressView() }
                        }
                    }
                    .disabled(model.state == .working)
                    .accessibilityIdentifier("history.export.create")
                    switch model.state {
                    case .nothingToExport:
                        Text(ExportCopy.nothing)
                            .foregroundStyle(Palette.secondaryText)
                            .accessibilityIdentifier("history.export.empty")
                    case .failed:
                        Text(ExportCopy.failed)
                            .foregroundStyle(Palette.destructive)
                            .accessibilityIdentifier("history.export.failed")
                    default:
                        EmptyView()
                    }
                }
            }
            .navigationTitle(HistoryCopy.exportPDF)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(ExportCopy.cancel, action: onDone)
                        .accessibilityIdentifier("history.export.cancel")
                }
            }
        }
        .presentationDetents([.medium, .large])
        #if os(iOS)
        .sheet(item: shareItem, onDismiss: { model.shareDidFinish() }) { item in
            ActivityShareSheet(file: item.url)
        }
        #endif
    }

    #if os(iOS)
    private var shareItem: Binding<SharedPDF?> {
        Binding(
            get: { if case .ready(let url) = model.state { SharedPDF(url: url) } else { nil } },
            set: { if $0 == nil { model.shareDidFinish() } }
        )
    }
    #endif
}

private struct SharedPDF: Identifiable {
    let url: URL
    var id: URL { url }
}

private enum ExportCopy {
    static let range = L10n.tr("Range")
    static let from = L10n.tr("From")
    static let to = L10n.tr("To")
    static let create = L10n.tr("Create PDF")
    static let cancel = L10n.tr("Cancel")
    static let nothing = L10n.tr("Nothing logged in these dates")
    static let failed = L10n.tr("Couldn't create the PDF. Try again.")

    static func name(_ preset: HistoryExportPreset) -> String {
        switch preset {
        case .week: L10n.tr("7 days")
        case .fortnight: L10n.tr("14 days")
        case .month: L10n.tr("30 days")
        case .custom: L10n.tr("Custom")
        }
    }
}
```

If `Palette.destructive` is a `Color`, as `.tint(Palette.destructive)` in `HistoryView` suggests, it works with `.foregroundStyle`.

- [ ] **Step 3: Add the header button and present the sheet.** In `HistoryView.swift`:

1. Add `@State private var isExporting = false` next to `isShowingFilter`.
2. In `header`, change `Spacer(minLength: 0)` followed by `filterButton` to `Spacer(minLength: 0)`, `exportButton`, `filterButton`. The `HStack`'s `spacing: 12` separates the two buttons.
3. Add after `filterButton`:

```swift
    private var exportButton: some View {
        Button { isExporting = true } label: {
            Image(systemName: "square.and.arrow.up")
                .font(.body.weight(.semibold))
                .foregroundStyle(Palette.secondaryText)
                .frame(width: 44, height: 44)
                .background(Circle().fill(Palette.secondaryText.opacity(0.14)))
                .contentShape(Circle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(HistoryCopy.exportPDF)
        .accessibilityIdentifier("history.export")
    }
```

4. After the filter `.sheet`, add:

```swift
        .sheet(isPresented: $isExporting) {
            HistoryExportSheet(
                model: HistoryExportViewModel(store: store, babyID: babyID, babyName: babyName, volumeUnit: model.volumeUnit)
            ) { isExporting = false }
        }
```

5. In the identifier doc comment above `HistoryView`, add `history.export` (the header's Export PDF button, whose sheet documents its own identifiers) after `history.filter`.

- [ ] **Step 4: Add the strings.** Write `/tmp/history-export.json`:

```json
{
  "Range": {"de": "Zeitraum", "es": "Periodo", "fr": "Période", "it": "Periodo", "uk": "Період"},
  "7 days": {"de": "7 Tage", "es": "7 días", "fr": "7 jours", "it": "7 giorni", "uk": "7 днів"},
  "14 days": {"de": "14 Tage", "es": "14 días", "fr": "14 jours", "it": "14 giorni", "uk": "14 днів"},
  "30 days": {"de": "30 Tage", "es": "30 días", "fr": "30 jours", "it": "30 giorni", "uk": "30 днів"},
  "Custom": {"de": "Eigene", "es": "Personalizado", "fr": "Personnalisé", "it": "Personalizzato", "uk": "Власний"},
  "From": {"de": "Von", "es": "Desde", "fr": "Du", "it": "Dal", "uk": "З"},
  "To": {"de": "Bis", "es": "Hasta", "fr": "Au", "it": "Al", "uk": "По"},
  "Create PDF": {"de": "PDF erstellen", "es": "Crear PDF", "fr": "Créer le PDF", "it": "Crea PDF", "uk": "Створити PDF"},
  "Cancel": {"de": "Abbrechen", "es": "Cancelar", "fr": "Annuler", "it": "Annulla", "uk": "Скасувати"},
  "Nothing logged in these dates": {"de": "In diesem Zeitraum wurde nichts erfasst", "es": "No hay registros en estas fechas", "fr": "Rien n’a été enregistré à ces dates", "it": "Nessuna registrazione in queste date", "uk": "За ці дати нічого не записано"},
  "Couldn't create the PDF. Try again.": {"de": "Das PDF konnte nicht erstellt werden. Bitte erneut versuchen.", "es": "No se pudo crear el PDF. Inténtalo de nuevo.", "fr": "Impossible de créer le PDF. Réessayez.", "it": "Impossibile creare il PDF. Riprova.", "uk": "Не вдалося створити PDF. Спробуйте ще раз."}
}
```

Before running the script, check which of these keys already exist in the catalog. If one exists with an `en` localization or a plural form, remove it from the JSON so the script doesn't replace it:

```bash
python3 -c "import json;d=json.load(open('Packages/BabyTracker/Sources/HistoryFeature/Localizable.xcstrings'))['strings'];print([k for k in json.load(open('/tmp/history-export.json')) if k in d])"
```

Then run `python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/HistoryFeature/Localizable.xcstrings /tmp/history-export.json` and `python3 scripts/verify-localizations.py`. Expected: exit 0.

- [ ] **Step 5: Write the UI test.** Add it to `HistoryRedesignTests.swift` under a `// MARK: - Export` heading:

```swift
    /// Export opens on 7 days, Custom shows the two dates, and Create PDF
    /// reaches the share sheet with a file (the fixture has entries
    /// yesterday). Cancel closes the sheet.
    func testExportSheetCreatesAPDF() {
        let app = launchHistory()
        defer { app.terminate() }

        let export = app.buttons["history.export"].firstMatch
        XCTAssertTrue(export.waitForExistence(timeout: 10), app.debugDescription)
        export.tap()

        let create = app.buttons["history.export.create"].firstMatch
        XCTAssertTrue(create.waitForExistence(timeout: 5), app.debugDescription)
        app.buttons["Custom"].firstMatch.tap()
        XCTAssertTrue(app.datePickers["history.export.from"].firstMatch.waitForExistence(timeout: 3)
                      || app.otherElements["history.export.from"].firstMatch.exists, app.debugDescription)
        app.buttons["7 days"].firstMatch.tap()

        create.tap()
        // The system share sheet names the file it shares in its header.
        let shared = app.staticTexts.containing(NSPredicate(format: "label CONTAINS[c] %@", ".pdf")).firstMatch
        let pdfTitled = app.otherElements.containing(NSPredicate(format: "label CONTAINS[c] %@", "PDF")).firstMatch
        XCTAssertTrue(shared.waitForExistence(timeout: 15) || pdfTitled.exists, "no share sheet:\n\(app.debugDescription)")
        XCTAssertFalse(app.staticTexts["history.export.failed"].exists)
    }
```

If the share sheet's header text doesn't contain ".pdf" on iOS 26, find what it does show with `app.debugDescription` in the failure output. Then assert on that, e.g. the activity list `app.otherElements["ActivityListView"]` or a `Print` / `Save to Files` cell, rather than loosening the assertion to nothing.

- [ ] **Step 6: Build, then run the UI test.** Run `pgrep xcodebuild` and expect nothing. Then:

```bash
xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build 2>&1 | tail -3
xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' \
  -parallel-testing-enabled NO -maximum-concurrent-test-simulator-destinations 1 \
  -only-testing:BabyTrackerUITests/HistoryRedesignTests 2>&1 | tail -30
```

Expected: `** BUILD SUCCEEDED **`, then `** TEST SUCCEEDED **` for every `HistoryRedesignTests` test.

- [ ] **Step 7: Run the package suite** with `make test`. Expected: a PASS with no count change, since this task adds no package tests.

- [ ] **Step 8: Commit.**

```bash
git add Packages/BabyTracker/Sources/BabyUI/ActivityShareSheet.swift Packages/BabyTracker/Sources/SettingsFeature/FamilyExportView.swift \
  Packages/BabyTracker/Sources/HistoryFeature UITests/BabyTrackerUITests/HistoryRedesignTests.swift
git commit -m "feat(history): Export PDF from the History header

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Final verification

- [ ] **Step 1: Run `make test`.** Expected: a PASS with baseline + 24 tests (3 + 2 + 7 + 3 + 3 + 6).
- [ ] **Step 2: Re-run the UIKit PDF test** on the simulator (Task 5 Step 5). Expected: a PASS.
- [ ] **Step 3: Check the real PDF on the simulator.**
  1. Launch the app with the History fixture: run `testExportSheetCreatesAPDF` once more, or use the `run` skill with `-uiTestHistorySessions`.
  2. Export the 7-day range.
  3. Copy the PDF out of the simulator: `xcrun simctl get_app_container <simulator-udid> <bundle id> data`, then `find <that>/tmp/baby-tracker-history-exports -name '*.pdf'`.
  4. Make an image of page 1: `sips -s format png <file>.pdf --out /tmp/history-export.png`.
  5. Read the PNG and check that it has the title, range, day headings, the totals line (with the bottle volume) and table rows. Put it in the hand-off.
- [ ] **Step 4: Screenshot the day headers.** In History, check the bottle total shows between sleep and diapers. Screenshot at the default text size and at the largest accessibility size.
- [ ] **Step 5: Review the code** with superpowers:requesting-code-review on the whole branch, and fix what it finds.
- [ ] **Step 6: Update `TASKS.md`.** Tick the tasks and record the verification commands and their results. Commit with `docs(tasks): history PDF export verified`.
- [ ] **Step 7: Finish the branch** with superpowers:finishing-a-development-branch. Check `main` for new commits and uncommitted edits from parallel sessions before merging.
