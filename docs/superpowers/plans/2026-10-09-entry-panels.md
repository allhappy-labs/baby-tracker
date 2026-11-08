# Entry Panels Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every activity with a length is edited as Start + End, logging and editing share one panel per activity, and every sheet fits its content.

**Architecture:** A pure `TimeSpan` value and a `TimeSpanSection` view own the Start/End rows; an `EntryPanel` shell (Form + footer + title + fitted height) wraps each activity's fields. `PastEntrySheet` folds into the part panel's new mode via a shared `PartFormModel` protocol. `.fittedSheetHeight()` reads the scroll content size (`onScrollGeometryChange`), which does not depend on the detent, replacing every stated `sheetHeight(_:)`.

**Tech Stack:** Swift 6, SwiftUI (iOS 26), SwiftData `BabyStore`, Swift Testing, XCUITest.

**Spec:** `docs/superpowers/specs/2026-10-09-entry-panels-design.md`

## Global Constraints

- iOS 26 / macOS 26 package platforms; Swift 6 language mode.
- No schema, sync-payload or server changes.
- Interval activities: breast side, sleep, bottle (End optional). Moments: diaper, supplement, solids, measurement.
- No duration stepper anywhere; length is read-only text (`ElapsedFormatter.precise`).
- Every new user-facing string goes through `LoggingL10n` / `HomeL10n` and gets `de`, `fr`, `es`, `it`, `uk` translations in the target's `Localizable.xcstrings` via `scripts/add-catalog-strings.py`.
- Tests assert logic, never geometry (no point sizes, no stated heights).
- `make test` asserts a literal total (`EXPECTED_TESTS` in `Makefile`): bump/lower it in the same commit as any test added/removed, and say so in the commit message.
- Simulator: only `platform=iOS Simulator,id=<simulator-udid>`; `pgrep xcodebuild` first — one run at a time.
- UI tests: run only the affected classes with `-only-testing:`; never the full suite.
- Work in worktree `.worktrees/entry-panels` on branch `entry-panels`; use `/usr/bin/git` there (RTK rewrite trips the worktree guard).

## Review Focus

1. **Midnight crossing** — a sleep or feed starting 23:40 and ending 00:20: both rows carry the date, length reads 40m, saves without "End must be after start". Test in Task 4 (`aPartCrossingMidnightSavesItsLength`).
2. **Old breast part stored with seconds only** (no `startedAt`/`endedAt`) in a two-part feed: opens with End = shown start + seconds, a side-only edit writes no times, an End edit changes only its length. Existing tests `aSideOnlyEditLeavesAnOldPartsTimesAsStored`, `aDurationOnlyEditKeepsAnOldPartsUnknownStart` keep passing, rewritten to set `end` (Task 4).
3. **Bottle End added then removed before saving** — saves as a moment (`seconds`, `endedAt` nil), not as a zero-length interval. Test in Task 4 (`removingABottleEndBeforeSavingKeepsItAMoment`).
4. **Running-timer part End moved past the running side's start / before its own start** — refused with the existing `partsExceedElapsed` message, nothing saved. Test in Task 6 (`aPartEndPastTheRunningSideIsRefused`).
5. **Sheet height feedback loop** — opening, expanding Details, and adding an End must settle (app goes idle). Proven in Task 1 by the `EditFlowTests` subset completing without runner restarts, and re-checked in Task 10.

---

### Task 1: `.fittedSheetHeight()` — prove it settles

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyUI/FittedSheetHeight.swift`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/PartEditSheet.swift:39-46, 243-258` (use it; delete `height`)

**Interfaces:**
- Produces: `extension View { func fittedSheetHeight() -> some View }` in `BabyUI`. Must be applied **directly to the scroll view** (`Form`, `List`, `ScrollView`).

- [ ] **Step 1: Create the worktree**

```bash
cd <repo>
/usr/bin/git worktree add .worktrees/entry-panels -b entry-panels main
cd .worktrees/entry-panels
```

- [ ] **Step 2: Write the modifier**

```swift
import SwiftUI

/// A sheet detent as tall as the sheet's scrolling content, measured.
///
/// Measures the scroll view's *content size* plus its insets (navigation bar
/// above, a pinned footer below). Neither depends on the detent: the content
/// is laid out against the sheet's width only, so a detent change does not
/// re-fire the measurement. The previous measuring attempt read the view
/// inside the sheet, whose height *is* the detent, and never settled.
///
/// Until the first measurement the sheet opens at `.medium`; at the
/// accessibility sizes it is full height and scrolls.
private struct FittedSheetHeight: ViewModifier {
    @State private var height: CGFloat?
    @Environment(\.dynamicTypeSize) private var typeSize

    func body(content: Content) -> some View {
        content
            .onScrollGeometryChange(for: CGFloat.self) { geometry in
                (geometry.contentSize.height + geometry.contentInsets.top + geometry.contentInsets.bottom)
                    .rounded(.up)
            } action: { _, new in
                // A point either way is rounding, not a new layout.
                if abs(new - (height ?? 0)) > 1 { height = new }
            }
            #if os(iOS)
            .presentationDetents(detents)
            #endif
    }

    private var detents: Set<PresentationDetent> {
        if typeSize.isAccessibilitySize { return [.large] }
        guard let height else { return [.medium] }
        return [.height(height)]
    }
}

public extension View {
    /// Sizes the presenting sheet to this scroll view's content. Apply it to
    /// the `Form`, `List` or `ScrollView` itself.
    func fittedSheetHeight() -> some View {
        modifier(FittedSheetHeight())
    }
}
```

- [ ] **Step 3: Use it in `PartEditSheet`**

In `body`, replace `form.sheetHeight(height)` with `form`, and in `form` put `.fittedSheetHeight()` directly after the closing brace of `Form { ... }` (before `.editFooter`). Delete the `height` computed property (lines 243-258) and its doc comment.

- [ ] **Step 4: Build**

Run: `cd Packages/BabyTracker && swift build 2>&1 | tail -5`
Expected: `Build complete!`

- [ ] **Step 5: Prove it settles on the simulator**

```bash
pgrep xcodebuild && echo "BUSY — wait" || xcodebuild test -scheme BabyTracker \
  -destination 'platform=iOS Simulator,id=<simulator-udid>' \
  -parallel-testing-enabled NO -only-testing:BabyTrackerUITests/EditFlowTests 2>&1 | tail -20
```
Expected: `** TEST SUCCEEDED **`, and no "Restarting after unexpected exit" / "Failed to get matching snapshot" / idle-timeout lines in the log. Then open History → a lone breast feed in the simulator, screenshot with Details collapsed and expanded (`xcrun simctl io <simulator-udid> screenshot /tmp/part-collapsed.png`): no empty band below the footer; expanding grows the sheet.

**If the run hangs or the sheet oscillates: STOP, do not continue to Task 2, and report** — the whole plan relies on this.

- [ ] **Step 6: Commit**

```bash
/usr/bin/git add Packages/BabyTracker/Sources/BabyUI/FittedSheetHeight.swift Packages/BabyTracker/Sources/LoggingFeature/PartEditSheet.swift
/usr/bin/git commit -m "feat(ui): sheets fit their measured content; part editor first

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: `TimeSpan`, `TimeSpanSection`, copy

**Files:**
- Create: `Packages/BabyTracker/Sources/LoggingFeature/TimeSpan.swift`
- Create: `Packages/BabyTracker/Sources/LoggingFeature/TimeSpanSection.swift`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/Localizable.xcstrings` (new keys)
- Test: `Packages/BabyTracker/Tests/LoggingFeatureTests/TimeSpanTests.swift`
- Modify: `Makefile` (`EXPECTED_TESTS` +7)

**Interfaces:**
- Produces:
  - `public struct TimeSpan: Equatable, Sendable { var start: Date; var end: Date?; var seconds: Int?; var lengthText: String?; func problem(latest: Date, minimumSeconds: Int = 1) -> TimeSpan.Problem? }`
  - `public enum TimeSpan.Problem: Equatable, Sendable { case endBeforeStart, inTheFuture, tooShort }`
  - `public enum TimeSpanCopy { start, end, length, running, addEnd, removeEnd, endBeforeStart, inTheFuture, breastTooShort; static func message(_ problem: TimeSpan.Problem) -> String }`
  - `public struct TimeSpanSection<Footer: View>: View` — `init(start: Binding<Date>, end: Binding<Date?>, endStyle: TimeSpanSection.EndStyle, latest: Date, identifierPrefix: String, @ViewBuilder footer: () -> Footer)`; `EndStyle` = `.required`, `.optional`, `.running`. A convenience init without `footer` uses `EmptyView`.

- [ ] **Step 1: Write the failing tests**

```swift
import Testing
import Foundation
@testable import LoggingFeature

@Suite struct TimeSpanTests {
    let now = Date(timeIntervalSince1970: 1_700_000_000)

    @Test func lengthIsEndMinusStart() {
        let span = TimeSpan(start: now.addingTimeInterval(-1080), end: now)
        #expect(span.seconds == 1080)
        #expect(span.lengthText == "18m")
    }

    @Test func noEndHasNoLengthAndNoProblem() {
        let span = TimeSpan(start: now.addingTimeInterval(-60), end: nil)
        #expect(span.seconds == nil)
        #expect(span.lengthText == nil)
        #expect(span.problem(latest: now) == nil)
    }

    @Test func anEndAtOrBeforeTheStartIsRefused() {
        #expect(TimeSpan(start: now, end: now).problem(latest: now) == .endBeforeStart)
        #expect(TimeSpan(start: now, end: now.addingTimeInterval(-60)).problem(latest: now) == .endBeforeStart)
    }

    @Test func aStartOrEndAfterNowIsRefused() {
        #expect(TimeSpan(start: now.addingTimeInterval(60), end: nil).problem(latest: now) == .inTheFuture)
        #expect(TimeSpan(start: now.addingTimeInterval(-60), end: now.addingTimeInterval(60))
            .problem(latest: now) == .inTheFuture)
    }

    @Test func shorterThanTheMinimumIsRefused() {
        let span = TimeSpan(start: now.addingTimeInterval(-20), end: now)
        #expect(span.problem(latest: now, minimumSeconds: 30) == .tooShort)
        #expect(span.problem(latest: now) == nil)
    }

    @Test func crossingMidnightIsOneInterval() throws {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = .gmt
        let start = try #require(calendar.date(from: DateComponents(year: 2026, month: 10, day: 8, hour: 23, minute: 40)))
        let span = TimeSpan(start: start, end: start.addingTimeInterval(40 * 60))
        #expect(span.problem(latest: start.addingTimeInterval(3600)) == nil)
        #expect(span.lengthText == "40m")
    }

    @Test func everyProblemHasCopy() {
        for problem in [TimeSpan.Problem.endBeforeStart, .inTheFuture, .tooShort] {
            #expect(!TimeSpanCopy.message(problem).isEmpty)
        }
    }
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter TimeSpanTests 2>&1 | tail -5`
Expected: compile failure, `cannot find 'TimeSpan' in scope`.

- [ ] **Step 3: Implement `TimeSpan.swift`**

```swift
import Foundation
import BabyCore

/// An activity's start and, once it has one, its end — the one place the
/// app judges an interval. Pure, so every panel and its tests agree.
public struct TimeSpan: Equatable, Sendable {
    public enum Problem: Equatable, Sendable {
        case endBeforeStart, inTheFuture, tooShort
    }

    public var start: Date
    /// Nil: no end — a bottle given at a moment, or a part still running.
    public var end: Date?

    public init(start: Date, end: Date?) {
        self.start = start
        self.end = end
    }

    public var seconds: Int? {
        end.map { max(0, Int($0.timeIntervalSince(start).rounded())) }
    }

    /// "18m", read-only beside the rows. Never edited directly.
    public var lengthText: String? { seconds.map(ElapsedFormatter.precise) }

    /// Nil when the span can be saved. `latest` is now, or later for an entry
    /// already stored ahead of the clock. `minimumSeconds` is 30 for a
    /// breast side, as `logBreast` refuses shorter.
    public func problem(latest: Date, minimumSeconds: Int = 1) -> Problem? {
        if start > latest { return .inTheFuture }
        guard let end else { return nil }
        if end <= start { return .endBeforeStart }
        if end > latest { return .inTheFuture }
        if let seconds, seconds < minimumSeconds { return .tooShort }
        return nil
    }
}

/// Every string the time rows show. The three messages keep the Past entry
/// sheet's keys, so their translations carry over.
public enum TimeSpanCopy {
    public static let start = LoggingL10n.text("Start")
    public static let end = LoggingL10n.text("End")
    public static let length = LoggingL10n.text("Length")
    public static let running = LoggingL10n.text("Running")
    public static let addEnd = LoggingL10n.text("Add end time")
    public static let removeEnd = LoggingL10n.text("Remove end time")
    public static let endBeforeStart = LoggingL10n.text("End must be after start.")
    public static let inTheFuture = LoggingL10n.text("Time can’t be in the future.")
    public static let breastTooShort = LoggingL10n.text("A breast feed must be at least 30 seconds.")

    public static func message(_ problem: TimeSpan.Problem) -> String {
        switch problem {
        case .endBeforeStart: endBeforeStart
        case .inTheFuture: inTheFuture
        case .tooShort: breastTooShort
        }
    }
}
```

- [ ] **Step 4: Implement `TimeSpanSection.swift`**

```swift
import SwiftUI
import BabyCore
import BabyUI

/// Start and End rows, both with the date, and the length beside them —
/// for every activity with a length. Never a duration stepper.
public struct TimeSpanSection<Footer: View>: View {
    public enum EndStyle: Sendable {
        /// Breast, sleep: an End is always there.
        case required
        /// Bottle: "Add end time" until it has one, then a ✕ to remove it.
        case optional
        /// A part still running: Start and End read-only, End says Running.
        case running
    }

    @Binding private var start: Date
    @Binding private var end: Date?
    private let endStyle: EndStyle
    private let latest: Date
    private let identifierPrefix: String
    private let footer: Footer

    public init(start: Binding<Date>, end: Binding<Date?>, endStyle: EndStyle, latest: Date,
                identifierPrefix: String, @ViewBuilder footer: () -> Footer) {
        _start = start
        _end = end
        self.endStyle = endStyle
        self.latest = latest
        self.identifierPrefix = identifierPrefix
        self.footer = footer()
    }

    public var body: some View {
        Section {
            if endStyle == .running {
                LabeledContent(TimeSpanCopy.start, value: start.formatted(date: .omitted, time: .shortened))
                    .accessibilityIdentifier(identifierPrefix + ".start")
                LabeledContent(TimeSpanCopy.end, value: TimeSpanCopy.running)
                    .accessibilityIdentifier(identifierPrefix + ".end")
            } else {
                EditTimeRow(TimeSpanCopy.start, date: $start, latest: latest, identifier: identifierPrefix + ".start")
                endRow
                if let length = TimeSpan(start: start, end: end).lengthText {
                    LabeledContent(TimeSpanCopy.length, value: length)
                        .monospacedDigit()
                        .accessibilityIdentifier(identifierPrefix + ".length")
                }
            }
        } footer: {
            footer
        }
    }

    @ViewBuilder
    private var endRow: some View {
        if let current = end {
            HStack(spacing: 8) {
                EditTimeRow(TimeSpanCopy.end, date: Binding(get: { current }, set: { end = $0 }),
                            latest: latest, identifier: identifierPrefix + ".end")
                if endStyle == .optional {
                    Button {
                        end = nil
                    } label: {
                        Image(systemName: "xmark.circle.fill")
                            .foregroundStyle(Palette.secondaryText)
                            .frame(minWidth: 44, minHeight: 44)
                    }
                    .buttonStyle(.borderless)
                    .accessibilityLabel(TimeSpanCopy.removeEnd)
                    .accessibilityIdentifier(identifierPrefix + ".removeEnd")
                }
            }
        } else {
            Button(TimeSpanCopy.addEnd) {
                // Ten minutes after the start, never past now.
                end = min(latest, start.addingTimeInterval(600))
            }
            .frame(minHeight: 44)
            .accessibilityIdentifier(identifierPrefix + ".addEnd")
        }
    }
}

public extension TimeSpanSection where Footer == EmptyView {
    init(start: Binding<Date>, end: Binding<Date?>, endStyle: EndStyle, latest: Date, identifierPrefix: String) {
        self.init(start: start, end: end, endStyle: endStyle, latest: latest,
                  identifierPrefix: identifierPrefix) { EmptyView() }
    }
}
```

- [ ] **Step 5: Add translations**

Write `/tmp/timespan-strings.json` with `"Length"`, `"Running"`, `"Add end time"`, `"Remove end time"` mapped to `de`/`fr`/`es`/`it`/`uk` (e.g. `"Length": {"de": "Dauer", "fr": "Durée", "es": "Duración", "it": "Durata", "uk": "Тривалість"}`, `"Running": {"de": "Läuft", "fr": "En cours", "es": "En curso", "it": "In corso", "uk": "Триває"}`, `"Add end time": {"de": "Endzeit hinzufügen", "fr": "Ajouter l’heure de fin", "es": "Añadir hora de fin", "it": "Aggiungi ora di fine", "uk": "Додати час завершення"}`, `"Remove end time": {"de": "Endzeit entfernen", "fr": "Supprimer l’heure de fin", "es": "Quitar hora de fin", "it": "Rimuovi ora di fine", "uk": "Прибрати час завершення"}`), then:

Run: `python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/LoggingFeature/Localizable.xcstrings /tmp/timespan-strings.json`

- [ ] **Step 6: Run tests**

Run: `cd Packages/BabyTracker && swift test --filter TimeSpanTests 2>&1 | tail -5`
Expected: 7 tests pass.

- [ ] **Step 7: Bump the test total and commit**

Set `EXPECTED_TESTS := 1680` in `Makefile` (1673 + 7).

```bash
/usr/bin/git add -A Packages/BabyTracker/Sources/LoggingFeature Packages/BabyTracker/Tests/LoggingFeatureTests/TimeSpanTests.swift Makefile
/usr/bin/git commit -m "feat(logging): TimeSpan and the shared Start/End rows (+7 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Store — notes, sleep kind and bottle end on the log calls

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Parts.swift:130-143` (`logBreast`)
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Feeding.swift:158-171, 260-272` (`logBottle`, `bottleEntry`)
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Sleep.swift:8-24` (`logSleep`)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/LogAfterTheFactTests.swift`
- Modify: `Makefile` (+3)

**Interfaces:**
- Produces (all new parameters defaulted, so every existing call compiles unchanged):
  - `logBreast(side:from:to:for:note: String? = nil) throws -> FeedEntry`
  - `logBottle(volumeML:contents:for:at: Date? = nil, endedAt: Date? = nil, note: String? = nil) throws -> FeedEntry`
  - `logSleep(from:to:for:kindOverride: SleepKind? = nil, note: String? = nil, expectedWriteContext:) throws -> SleepEntry`

- [ ] **Step 1: Write the failing tests**

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor @Suite struct LogAfterTheFactTests {
    private func fixture() throws -> (BabyStore, UUID, TestDateProvider) {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        return (store, baby.id, clock)
    }

    @Test func aBottleWithAnEndStoresItsLength() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-900)
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: start,
                                       endedAt: clock.now, note: "slow")
        #expect(feed.components.first?.bottleSeconds == 900)
        #expect(feed.components.first?.startedAt == start)
        #expect(feed.components.first?.endedAt == clock.now)
        #expect(feed.startedAt == start)
        #expect(feed.endedAt == clock.now)
        #expect(feed.note == "slow")
    }

    @Test func aBreastSideKeepsItsNote() throws {
        let (store, baby, clock) = try fixture()
        let feed = try store.logBreast(side: .right, from: clock.now.addingTimeInterval(-600), to: clock.now,
                                       for: baby, note: "sleepy")
        #expect(feed.note == "sleepy")
        #expect(try store.changeLog().last?.operation == .create)
    }

    @Test func aSleepKeepsItsKindAndNote() throws {
        let (store, baby, clock) = try fixture()
        let sleep = try store.logSleep(from: clock.now.addingTimeInterval(-3600), to: clock.now, for: baby,
                                       kindOverride: .night, note: "car")
        #expect(sleep.kindOverride == .night)
        #expect(sleep.note == "car")
    }
}
```

(If `TestDateProvider`/`inMemory` live under a different helper in `BabyStoreTests/Support`, use the one there — check `grep -rn "func fixture\|TestDateProvider(" Packages/BabyTracker/Tests/BabyStoreTests | head`.)

- [ ] **Step 2: Run to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter LogAfterTheFactTests 2>&1 | tail -5`
Expected: compile failure, `extra arguments at positions`.

- [ ] **Step 3: Implement**

`logBreast` — add `note: String? = nil` after `for babyID: UUID`, and inside the write, after `breastEntry(...)`: `entry.note = note`.

`logBottle`:

```swift
func logBottle(
    volumeML: Double,
    contents: BottleContents,
    for babyID: UUID,
    at date: Date? = nil,
    endedAt end: Date? = nil,
    note: String? = nil
) throws -> FeedEntry {
    let stamp = date ?? now
    let entry = bottleEntry(volumeML: volumeML, contents: contents, at: stamp, endedAt: end, for: babyID)
    entry.note = note
    entry.feedingSessionID = try continuedSessionID(.feed, for: babyID, startingAt: stamp)

    context.insert(entry)
    try record(.create, on: entry)
    return entry
}
```

`bottleEntry`:

```swift
/// A bottle given at a moment (`end` nil), or one that took a while.
func bottleEntry(volumeML: Double, contents: BottleContents, at stamp: Date, endedAt end: Date? = nil,
                 for babyID: UUID) -> FeedEntry {
    let entry = FeedEntry(
        babyID: babyID,
        kind: .bottle,
        startedAt: stamp,
        timeZoneID: TimeZone.current.identifier,
        createdBy: deviceID,
        realm: activeRealm
    )
    let seconds = end.map { Int($0.timeIntervalSince(stamp).rounded()) }.flatMap { $0 > 0 ? $0 : nil }
    entry.components = [.bottle(id: UUID(), volumeML: volumeML, contents: contents, seconds: seconds,
                                startedAt: stamp, endedAt: seconds == nil ? nil : end)]
    entry.endedAt = seconds == nil ? stamp : end
    return entry
}
```

`logSleep` — add `kindOverride: SleepKind? = nil, note: String? = nil` before `expectedWriteContext`, and after `sleepEntry(...)`: `entry.kindOverride = kindOverride; entry.note = note`.

- [ ] **Step 4: Run tests**

Run: `cd Packages/BabyTracker && swift test --filter "LogAfterTheFactTests|BottleSheetModelTests|PastEntryModelTests" 2>&1 | tail -5`
Expected: all pass.

- [ ] **Step 5: Bump and commit**

`EXPECTED_TESTS := 1683`.

```bash
/usr/bin/git add -A Packages/BabyTracker/Sources/BabyStore Packages/BabyTracker/Tests/BabyStoreTests/LogAfterTheFactTests.swift Makefile
/usr/bin/git commit -m "feat(store): log calls take a note, a sleep kind and a bottle end (+3 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: `PartEditModel` — Start and End, independent

**Files:**
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/PartEditModel.swift` (properties, `Draft`, `end`, `latestAllowedDate`, `loadedEnd`, `validationMessage`, `reload`, both `write`s, `draft(_:index:)`, `draft(_:)`)
- Modify: `Packages/BabyTracker/Tests/LoggingFeatureTests/PartEditTests.swift`, `BottleEditTests.swift`, `SessionSheetModelTests.swift` (callers of `durationSeconds =`)
- Modify: `Makefile` (+3)

**Interfaces:**
- Consumes: `TimeSpan`, `TimeSpanCopy` (Task 2).
- Produces on `PartEditModel`:
  - `public var start: Date`
  - `public var end: Date?` — breast and sleep always non-nil; bottle nil for a moment
  - `public var durationSeconds: Int { get }` — **read-only now**, `TimeSpan(start:end:).seconds ?? 0`
  - `public var kind: PartKind { get }` where `public enum PartKind: Hashable, CaseIterable, Sendable { case breast, bottle, sleep }` (declare `PartKind` in this file)

Behaviour change: moving Start no longer drags End along (spec: Start and End are independent rows). Moving a bottle's time no longer shifts its end.

- [ ] **Step 1: Rewrite the tests that set a duration, and add three**

In `PartEditTests.swift` replace every `model.durationSeconds = X` with `model.end = model.start.addingTimeInterval(X)`. Replace `movingTheStartKeepsTheDuration` with:

```swift
@Test func movingTheStartKeepsTheEnd() throws {
    let (store, baby, clock) = try fixture()
    let start = clock.now.addingTimeInterval(-3600)
    let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
    let componentID = try #require(feed.components.first?.id)
    let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: componentID),
                              unit: .millilitres)
    let moved = start.addingTimeInterval(-600)

    model.start = moved

    #expect(model.end == start.addingTimeInterval(720))
    #expect(model.durationSeconds == 1320)
    #expect(try model.save())
    #expect(feed.startedAt == moved)
    #expect(feed.endedAt == start.addingTimeInterval(720))
    #expect(feed.components.first?.durationSeconds == 1320)
}

@Test func aPartCrossingMidnightSavesItsLength() throws {
    let (store, baby, clock) = try fixture()
    var calendar = Calendar(identifier: .gregorian)
    calendar.timeZone = .gmt
    let midnight = calendar.startOfDay(for: clock.now)
    let start = midnight.addingTimeInterval(-20 * 60)
    let sleep = try store.logSleep(from: start, to: start.addingTimeInterval(600), for: baby)
    let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id))

    model.end = midnight.addingTimeInterval(20 * 60)

    #expect(model.validationMessage == nil)
    #expect(try model.save())
    #expect(sleep.durationSeconds == 40 * 60)
}
```

(GMT midnight of the fixture's clock, 1_700_000_000 = 22:13 UTC, so both times are safely in the past in any zone; the fixture's store is empty.)

In `BottleEditTests.swift` add:

```swift
@Test func aBottleGainsAnEndAndKeepsItsStart() throws {
    let (store, baby, clock) = try fixture()
    let start = clock.now.addingTimeInterval(-1800)
    let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: start)
    let componentID = try #require(feed.components.first?.id)
    let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: componentID))
    #expect(model.end == nil)

    model.end = start.addingTimeInterval(900)

    #expect(try model.save())
    #expect(feed.components.first?.bottleSeconds == 900)
    #expect(feed.components.first?.startedAt == start)
    #expect(feed.components.first?.endedAt == start.addingTimeInterval(900))
    #expect(feed.endedAt == start.addingTimeInterval(900))
}

@Test func removingABottleEndBeforeSavingKeepsItAMoment() throws {
    let (store, baby, clock) = try fixture()
    let start = clock.now.addingTimeInterval(-1800)
    let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: start)
    let componentID = try #require(feed.components.first?.id)
    let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: componentID))

    model.end = start.addingTimeInterval(900)
    model.end = nil
    model.volumeML = 120

    #expect(try model.save())
    #expect(feed.components.first?.bottleSeconds == nil)
    #expect(feed.components.first?.endedAt == nil)
    #expect(feed.endedAt == start)
}
```

Search `BottleEditTests.swift` for tests asserting that moving a timed bottle's time shifts its end (`grep -n "shift\|endedAt" Packages/BabyTracker/Tests/LoggingFeatureTests/BottleEditTests.swift`); change those expectations so `endedAt` stays where it was (independent rows). Same for `SessionSheetModelTests.swift` callers of `durationSeconds =`.

- [ ] **Step 2: Run to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter "PartEditTests|BottleEditTests" 2>&1 | tail -8`
Expected: compile failure on `model.end = nil` (`'nil' is not compatible with expected argument type 'Date'`).

- [ ] **Step 3: Rewrite the model's time state**

Properties (replace `durationSeconds`, `sleepEnd`):

```swift
public var start: Date
/// Breast and sleep: always set. Bottle: nil for a bottle given at a moment.
public var end: Date?
/// The length the rows show, never edited directly.
public var durationSeconds: Int { TimeSpan(start: start, end: end).seconds ?? 0 }
public var kind: PartKind {
    switch target {
    case .breast: .breast
    case .bottle: .bottle
    case .sleep: .sleep
    }
}
```

`Draft` — replace `durationSeconds`, `sleepEnd`, `bottleLength` with `var end: Date?`; keep `hasEnd`. Add `var seconds: Int? { TimeSpan(start: start, end: end).seconds }`.

`draft(_ feed:index:)`:

```swift
var draft = Draft(start: start, end: nil, note: feed.note ?? "")
switch component {
case let .breast(_, side, seconds, _, _):
    draft.side = side
    draft.end = start.addingTimeInterval(TimeInterval(seconds))
case let .bottle(_, volumeML, contents, _, startedAt, endedAt):
    draft.volumeML = volumeML
    draft.contents = contents
    if let startedAt, let endedAt, endedAt > startedAt {
        draft.end = start.addingTimeInterval(endedAt.timeIntervalSince(startedAt))
    }
case .solids:
    break
}
```

`draft(_ sleep:)`: `Draft(start: sleep.startedAt, end: sleep.endedAt ?? sleep.startedAt, hasEnd: sleep.endedAt != nil, sleepKind: sleep.kindOverride, note: sleep.note ?? "")`.

Delete the computed `end` get/set. `loadedEnd` becomes `loaded.hasEnd ? loaded.end : nil` for sleep and `loaded.end` otherwise; `latestAllowedDate` = `max(store.now, loadedEnd ?? loaded.start)`.

`writesSleepEnd` → `loaded.hasEnd || end != loaded.end`.

`validationMessage` (replace the `switch target` block; keep the bounds checks after it, using `end ?? start` where they read `end`):

```swift
let latest = latestAllowedDate
let timesMoved = start != loaded.start || end != loaded.end
switch target {
case .breast:
    if timesMoved, durationSeconds != loaded.seconds,
       let problem = TimeSpan(start: start, end: end).problem(latest: latest, minimumSeconds: 30) {
        return TimeSpanCopy.message(problem)
    }
case .bottle:
    if timesMoved, let problem = TimeSpan(start: start, end: end).problem(latest: latest) {
        return TimeSpanCopy.message(problem)
    }
case .sleep:
    if timesMoved, let problem = TimeSpan(start: start, end: writesSleepEnd ? end : nil).problem(latest: latest) {
        return TimeSpanCopy.message(problem)
    }
}
```

and the last bounds check becomes `if end != loadedEnd, let end, let nextPartStart = bounds.nextPartStart, end > nextPartStart`.

`reload()` — replace the `durationSeconds` and `sleepEnd` merges with `EditModel.merge(&end, loaded: loaded.end, fresh: fresh.end)`.

`write(_ feed:index:)` — replace the two component cases:

```swift
let startMoved = start != loaded.start
let timesChanged = startMoved || end != loaded.end
let refit: Bool
switch feed.components[index] {
case let .breast(id, _, seconds, startedAt, endedAt):
    // An old part with no recorded start keeps it unknown unless the start
    // itself moved; an End edit changes only its length.
    let knownStart = startedAt ?? (feed.components.count == 1 ? feed.startedAt : nil)
    let newStart = startMoved ? start : knownStart
    let length = durationSeconds
    feed.components[index] = .breast(
        id: id, side: side, seconds: timesChanged ? length : seconds,
        startedAt: timesChanged ? newStart : startedAt,
        endedAt: timesChanged ? newStart.map { $0.addingTimeInterval(TimeInterval(length)) } : endedAt)
    refit = timesChanged && newStart != nil
case let .bottle(id, _, _, seconds, startedAt, endedAt):
    let length = TimeSpan(start: start, end: end).seconds
    feed.components[index] = .bottle(
        id: id, volumeML: volumeML, contents: contents,
        seconds: timesChanged ? length : seconds,
        startedAt: timesChanged ? start : startedAt,
        endedAt: timesChanged ? end : endedAt)
    refit = timesChanged
case .solids:
    return
}
```

In `fitSpan`, the one-part branch keeps `feed.endedAt = feed.components[0].endedAt ?? start` (replace the shift fallback: a moment's entry ends where it starts).

`write(_ sleep:)`: `if writesSleepEnd { sleep.endedAt = end }`.

Replace remaining `PastEntrySheetCopy.` references in this file with `TimeSpanCopy.`.

- [ ] **Step 4: Run tests**

Run: `cd Packages/BabyTracker && swift test --filter "PartEditTests|BottleEditTests|SessionSheetModelTests|JoinRowTests" 2>&1 | tail -8`
Expected: all pass. Fix any remaining expectation that encoded "start drags end" by asserting the end stays put — never by restoring the old coupling.

- [ ] **Step 5: Bump and commit**

`EXPECTED_TESTS := 1686` (+3: `aPartCrossingMidnightSavesItsLength`, `aBottleGainsAnEndAndKeepsItsStart`, `removingABottleEndBeforeSavingKeepsItAMoment`; `movingTheStartKeepsTheEnd` replaces one).

```bash
/usr/bin/git add -A Packages/BabyTracker Makefile
/usr/bin/git commit -m "feat(logging): part editor edits Start and End independently; bottle end optional (+3 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: `EntryPanel`, the shared part panel, and new mode (replaces Past entry)

**Files:**
- Create: `Packages/BabyTracker/Sources/LoggingFeature/EntryPanel.swift`
- Create: `Packages/BabyTracker/Sources/LoggingFeature/PartFormModel.swift` (protocol + `NewPartModel`)
- Create: `Packages/BabyTracker/Sources/LoggingFeature/PartFields.swift` (the kind sections + time + details, shared)
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/PartEditSheet.swift` (body → `EntryPanel` + `PartFields`; add `NewPartSheet`)
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/EditChrome.swift:73-161, 260-269` (`onDelete` optional)
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/LogCoordinator.swift:158-159`
- Delete: `Packages/BabyTracker/Sources/LoggingFeature/PastEntrySheet.swift`
- Rename/modify: `Tests/LoggingFeatureTests/PastEntryModelTests.swift` → `NewPartModelTests.swift`
- Modify: `UITests/BabyTrackerUITests/LoggingFlowTests.swift:176-215`
- Modify: `Makefile` (+2)

**Interfaces:**
- Consumes: `TimeSpanSection` (Task 2), `PartEditModel.kind/start/end` (Task 4), store log params (Task 3), `fittedSheetHeight()` (Task 1).
- Produces:
  - `public struct EntryPanel<Content: View>: View` — `init(title: String, icon: BabyIcon, tint: Color, canSave: Bool, identifierPrefix: String, onCancel: (() -> Void)?, onSave: @escaping () -> Void, onDelete: (() -> Void)?, @ViewBuilder content: () -> Content)`. Callers wrap it in `NavigationStack` when standalone; `onDelete == nil` means new mode (Save only).
  - `public struct DetailsSection<Fields: View>: View` — `init(isExpanded: Binding<Bool>, identifier: String, @ViewBuilder fields: () -> Fields)`.
  - `.editFooter(isPresented:canSave:identifierPrefix:onSave:onDelete: (() -> Void)?)` — nil hides Delete.
  - `@MainActor public protocol PartFormModel: AnyObject, Observable` with `kind`, `side`, `start`, `end`, `volumeML`, `contents`, `sleepKind`, `note`, `unit`, `chips`, `label(for:)`, `step(_:)`, `latestAllowedDate`, `validationMessage`, `canSave`.
  - `public final class NewPartModel: PartFormModel` — `init(store:babyID:unit:kind: PartKind = .breast)`, `var kind: PartKind { get set }`, `func save() throws`.
  - `public struct NewPartSheet: View` — `init(store:babyID:unit:onDone:)`.

- [ ] **Step 1: Port the Past entry tests to `NewPartModel`**

`git mv` the file to `NewPartModelTests.swift` and rewrite:

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@MainActor @Suite struct NewPartModelTests {
    @Test func reversedFutureAndTooShortAreRefused() throws {
        let (store, baby, clock) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        model.kind = .breast
        model.side = .left
        model.start = clock.now
        model.end = clock.now.addingTimeInterval(-60)
        #expect(model.validationMessage == TimeSpanCopy.endBeforeStart)
        model.end = clock.now.addingTimeInterval(60)
        #expect(model.validationMessage == TimeSpanCopy.inTheFuture)
        model.start = clock.now.addingTimeInterval(-20)
        model.end = clock.now
        #expect(model.validationMessage == TimeSpanCopy.breastTooShort)
        model.start = clock.now.addingTimeInterval(-600)
        #expect(model.validationMessage == nil)
        try model.save()
        #expect(try store.lastFeed(for: baby)?.components.first?.breastSide == .left)
    }

    @Test func bottleAndSleepLogAtTheirOwnKind() throws {
        let (store, baby, clock) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        model.kind = .bottle
        model.start = clock.now.addingTimeInterval(-300)
        model.end = nil
        model.volumeML = 90
        model.contents = .formula
        #expect(model.validationMessage == nil)
        try model.save()
        #expect(try store.lastFeed(for: baby)?.totalBottleVolumeML == 90)
        #expect(try store.lastFeed(for: baby)?.components.first?.bottleSeconds == nil)

        model.kind = .sleep
        model.start = clock.now.addingTimeInterval(-1200)
        model.end = clock.now.addingTimeInterval(-600)
        model.sleepKind = .nap
        #expect(model.validationMessage == nil)
        try model.save()
        #expect(try store.lastSleep(for: baby)?.endedAt == model.end)
        #expect(try store.lastSleep(for: baby)?.kindOverride == .nap)
    }

    @Test func aTimedBottleAndANoteAreKept() throws {
        let (store, baby, clock) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        model.kind = .bottle
        model.start = clock.now.addingTimeInterval(-900)
        model.end = clock.now
        model.note = "  slow  "
        try model.save()
        let feed = try #require(try store.lastFeed(for: baby))
        #expect(feed.components.first?.bottleSeconds == 900)
        #expect(feed.note == "slow")
    }

    @Test func switchingToSleepNeedsAnEnd() throws {
        let (store, baby, _) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        model.kind = .bottle
        model.end = nil
        model.kind = .sleep
        #expect(model.end != nil)
    }
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter NewPartModelTests 2>&1 | tail -5`
Expected: compile failure, `cannot find 'NewPartModel' in scope`.

- [ ] **Step 3: `PartFormModel.swift`**

```swift
import Foundation
import Observation
import BabyCore
import BabyStore

/// What the part panel's fields read and write, in new mode and edit mode
/// alike, so both draw the same rows.
@MainActor
public protocol PartFormModel: AnyObject, Observable {
    var kind: PartKind { get }
    var side: BreastSide { get set }
    var start: Date { get set }
    var end: Date? { get set }
    var volumeML: Double { get set }
    var contents: BottleContents { get set }
    var sleepKind: SleepKind? { get set }
    var note: String { get set }
    var unit: VolumeUnit { get }
    var chips: [Double] { get }
    func label(for ml: Double) -> String
    func step(_ direction: Int)
    var latestAllowedDate: Date { get }
    var validationMessage: String? { get }
    var canSave: Bool { get }
}

extension PartEditModel: PartFormModel {}

/// A breast side, bottle or sleep logged after the fact — the part panel in
/// new mode. Opens ten minutes ago to now, as Past entry did.
@MainActor @Observable
public final class NewPartModel: PartFormModel {
    private let store: BabyStore
    private let babyID: UUID
    public let unit: VolumeUnit

    /// Breast and sleep need an end; switching to one gives it back.
    public var kind: PartKind {
        didSet { if kind != .bottle, end == nil { end = min(store.now, start.addingTimeInterval(600)) } }
    }
    public var side: BreastSide = .left
    public var start: Date
    public var end: Date?
    public var volumeML: Double
    public var contents: BottleContents
    public var sleepKind: SleepKind?
    public var note = ""

    public init(store: BabyStore, babyID: UUID, unit: VolumeUnit = .millilitres, kind: PartKind = .breast) {
        self.store = store
        self.babyID = babyID
        self.unit = unit
        self.kind = kind
        self.end = store.now
        self.start = store.now.addingTimeInterval(-600)
        let defaults = LogDefaults(store: store)
        self.volumeML = (try? defaults.mostUsedBottleVolumeML(for: babyID)) ?? LogDefaults.fallbackVolumeML
        self.contents = (try? defaults.lastBottleContents(for: babyID)) ?? LogDefaults.fallbackContents
    }

    public var latestAllowedDate: Date { store.now }

    public var validationMessage: String? {
        TimeSpan(start: start, end: end)
            .problem(latest: store.now, minimumSeconds: kind == .breast ? 30 : 1)
            .map(TimeSpanCopy.message)
    }

    public var canSave: Bool {
        validationMessage == nil && (kind == .bottle ? volumeML > 0 : end != nil)
    }

    public var chips: [Double] { BottleSheetModel.chips(for: unit) }
    public func label(for ml: Double) -> String { BottleSheetModel.label(for: ml, unit: unit) }
    public func step(_ direction: Int) {
        volumeML = max(0, volumeML + Double(direction) * BottleSheetModel.stepAmount(for: unit))
    }

    public func save() throws {
        let note = EditModel.trimmedToNil(note)
        switch kind {
        case .breast:
            guard let end else { throw EditModeError.invalid }
            _ = try store.logBreast(side: side, from: start, to: end, for: babyID, note: note)
        case .bottle:
            _ = try store.logBottle(volumeML: volumeML, contents: contents, for: babyID, at: start,
                                    endedAt: end, note: note)
        case .sleep:
            guard let end else { throw EditModeError.invalid }
            _ = try store.logSleep(from: start, to: end, for: babyID, kindOverride: sleepKind, note: note)
        }
    }
}
```

- [ ] **Step 4: `EditChrome.swift` — optional Delete**

Change `EditFooter.onDelete` to `(() -> Void)?`, render the Delete button only `if let onDelete`; `EditFooterModifier.onDelete` to optional, passing `onDelete.map { _ in { isConfirmingDelete = true } }` and applying `.deleteConfirmation` only when non-nil (call it with `onDelete ?? {}` — the dialog is never shown without the button). Change the `editFooter(...)` signature's `onDelete` to `(() -> Void)?`. Delete `EditFooter.height` and its doc comment once Task 9 removes its last user (leave it until then).

- [ ] **Step 5: `EntryPanel.swift`**

```swift
import SwiftUI
import BabyCore
import BabyUI

/// The one shell every log and edit panel shares: its fields in a Form,
/// sized to them, Save (and in edit mode Delete) pinned under them, and the
/// activity's icon in the title. Wrap it in a `NavigationStack` when it is
/// presented on its own; pushed inside the session sheet, it is not.
public struct EntryPanel<Content: View>: View {
    private let title: String
    private let icon: BabyIcon
    private let tint: Color
    private let canSave: Bool
    private let identifierPrefix: String
    private let onCancel: (() -> Void)?
    private let onSave: () -> Void
    private let onDelete: (() -> Void)?
    private let content: Content

    public init(title: String, icon: BabyIcon, tint: Color, canSave: Bool, identifierPrefix: String,
                onCancel: (() -> Void)?, onSave: @escaping () -> Void, onDelete: (() -> Void)?,
                @ViewBuilder content: () -> Content) {
        self.title = title
        self.icon = icon
        self.tint = tint
        self.canSave = canSave
        self.identifierPrefix = identifierPrefix
        self.onCancel = onCancel
        self.onSave = onSave
        self.onDelete = onDelete
        self.content = content()
    }

    public var body: some View {
        Form { content }
            .fittedSheetHeight()
            .editFooter(canSave: canSave, identifierPrefix: identifierPrefix, onSave: onSave, onDelete: onDelete)
            .navigationTitle(title)
            .editTitle(icon: icon, tint: tint, label: title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                if let onCancel {
                    ToolbarItem(placement: .cancellationAction) {
                        Button(EditChromeCopy.cancel, action: onCancel)
                    }
                }
            }
    }
}

/// Details: collapsed unless it holds something. Collapsed, it is one row —
/// the sheet shrinks to match.
public struct DetailsSection<Fields: View>: View {
    @Binding private var isExpanded: Bool
    private let identifier: String
    private let fields: Fields

    public init(isExpanded: Binding<Bool>, identifier: String, @ViewBuilder fields: () -> Fields) {
        _isExpanded = isExpanded
        self.identifier = identifier
        self.fields = fields()
    }

    public var body: some View {
        Section {
            DisclosureGroup(isExpanded: $isExpanded) {
                fields
            } label: {
                Text(EditChromeCopy.details)
                    .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                    .accessibilityIdentifier(identifier)
            }
        }
    }
}
```

Add to `EditChromeCopy`: `public static let cancel = LoggingL10n.text("Cancel")` and `public static let details = LoggingL10n.text("Details")` (both keys already exist in the catalog).

- [ ] **Step 6: `PartFields.swift`** — move `breastSection`, `bottleSections`, `chips`, `stepButton`, `sleepSection`, `validationFooter` out of `PartEditSheet` into:

```swift
import SwiftUI
import BabyCore
import BabyUI

/// The part panel's rows for one kind, then its Start and End, then Details —
/// the same in new mode and edit mode.
struct PartFields<Model: PartFormModel>: View {
    @Bindable var model: Model
    @Binding var isShowingDetails: Bool
    @Environment(\.dynamicTypeSize) private var typeSize

    var body: some View {
        switch model.kind {
        case .breast:
            Section {
                Picker(PartEditCopy.sideLabel, selection: $model.side) {
                    Text(BreastSide.left.displayName).tag(BreastSide.left)
                    Text(BreastSide.right.displayName).tag(BreastSide.right)
                }
                .pickerStyle(.segmented)
                .accessibilityIdentifier("partEdit.side")
            }
        case .bottle:
            bottleSection
        case .sleep:
            Section {
                Picker(PartEditCopy.kindLabel, selection: $model.sleepKind) {
                    Text(PartEditCopy.nap).tag(Optional(SleepKind.nap))
                    Text(PartEditCopy.night).tag(Optional(SleepKind.night))
                    Text(PartEditCopy.auto).tag(SleepKind?.none)
                }
                .pickerStyle(.segmented)
                .accessibilityIdentifier("partEdit.kind")
            }
        }

        TimeSpanSection(start: $model.start, end: $model.end,
                        endStyle: model.kind == .bottle ? .optional : .required,
                        latest: model.latestAllowedDate, identifierPrefix: "partEdit") {
            if let message = model.validationMessage {
                Text(message)
                    .foregroundStyle(Palette.destructive)
                    .fixedSize(horizontal: false, vertical: true)
                    .accessibilityIdentifier("partEdit.validation")
            }
        }

        DetailsSection(isExpanded: $isShowingDetails, identifier: "partEdit.details") {
            TextField(PartEditCopy.noteLabel, text: $model.note, prompt: Text(PartEditCopy.notePrompt))
                .accessibilityLabel(PartEditCopy.noteLabel)
                .accessibilityIdentifier("partEdit.note")
        }
    }

    // bottleSection, chips, stepButton: moved verbatim from PartEditSheet,
    // with `model` now `Model`, minus the contents Section's EditTimeRow and
    // footer (the TimeSpanSection above replaces them).
}
```

Move the existing `bottleSections` body (volume display, `chips`, ± buttons, contents picker) into `bottleSection` exactly as it is in `PartEditSheet.swift:130-205`, dropping only `EditTimeRow(date: $model.start, …identifier: "partEdit.time")` and the `footer: { validationFooter }`.

- [ ] **Step 7: `PartEditSheet` uses `EntryPanel` + `PartFields`; add `NewPartSheet`**

`PartEditSheet.body`:

```swift
public var body: some View {
    if isStandalone {
        NavigationStack { panel }
    } else {
        panel
    }
}

private var panel: some View {
    EntryPanel(title: title, icon: icon, tint: tint, canSave: model.canSave, identifierPrefix: "partEdit",
               onCancel: isStandalone ? onDone : nil, onSave: save, onDelete: delete) {
        if let join, join.previous != nil {
            Section { JoinRow(model: join) }
        }
        PartFields(model: model, isShowingDetails: $isShowingDetails)
    }
    .editLifecycle(revision: model.store.revision, isGone: model.isGone,
                   reload: { model.reload(); join?.reload() }, onGone: finish)
}
```

Delete the old `form`, `breastSection`, `bottleSections`, `chips`, `stepButton`, `sleepSection`, `validationFooter`. Keep `title`, `icon`, `tint`, `save`, `delete`, `finish`.

Append `NewPartSheet`:

```swift
/// Something that already happened — a side, a bottle or a sleep — in the
/// same panel History edits it in, with a kind picker on top and Save only.
public struct NewPartSheet: View {
    @State private var model: NewPartModel
    @State private var isShowingDetails = false
    private let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter

    public init(store: BabyStore, babyID: UUID, unit: VolumeUnit = .millilitres, onDone: @escaping () -> Void) {
        _model = State(initialValue: NewPartModel(store: store, babyID: babyID, unit: unit))
        self.onDone = onDone
    }

    public var body: some View {
        NavigationStack {
            EntryPanel(title: PartEditCopy.newTitle, icon: icon, tint: tint, canSave: model.canSave,
                       identifierPrefix: "partEdit", onCancel: onDone, onSave: save, onDelete: nil) {
                Section {
                    Picker(PartEditCopy.kindLabel, selection: $model.kind) {
                        Text(PartEditCopy.breastTitle).tag(PartKind.breast)
                        Text(PartEditCopy.bottleTitle).tag(PartKind.bottle)
                        Text(Tracker.sleep.title).tag(PartKind.sleep)
                    }
                    .pickerStyle(.segmented)
                    .accessibilityIdentifier("partEdit.newKind")
                }
                PartFields(model: model, isShowingDetails: $isShowingDetails)
            }
        }
    }

    private var icon: BabyIcon {
        switch model.kind {
        case .breast: model.side == .left ? .breastLeft : .breastRight
        case .bottle: .bottle
        case .sleep: .sleep
        }
    }

    private var tint: Color { model.kind == .sleep ? Palette.sleep : Palette.feed }

    private func save() {
        do {
            try model.save()
            errorReporter.clear()
            onDone()
        } catch {
            let subject = model.kind == .sleep ? ErrorSubject.sleep : ErrorSubject.feed
            errorReporter.report(ErrorBannerCopy.saveFailed(what: subject), error: error, category: AppLog.store)
        }
    }
}
```

Add `public static let newTitle = LoggingL10n.text("Past entry")` to `PartEditCopy` (existing key — keeps the More sheet's wording).

- [ ] **Step 8: Route and delete Past entry**

`LogCoordinator.swift:158-159`: `case .pastEntry: NewPartSheet(store: store, babyID: babyID, unit: unit, onDone: onDone)`. Delete `PastEntrySheet.swift`. `grep -rn "PastEntrySheetCopy\|PastEntryModel" Packages Apps` must return nothing.

- [ ] **Step 9: Update the UI test**

In `LoggingFlowTests.testPastEntryLogsALeftBreastFeedToHistory` replace lines 181-206:

```swift
XCTAssertTrue(app.tap(app.buttons["more.pastEntry"], until: app.buttons["partEdit.save"]))

// Breast is the default kind; choose Right, then back to Left.
let side = app.segmentedControls["partEdit.side"]
XCTAssertTrue(side.waitForExistence(timeout: 5))
side.buttons["Right"].tap()
side.buttons["Left"].tap()
XCTAssertTrue(side.buttons["Left"].isSelected)

// Start ten minutes ago, end now: both on screen, and valid.
XCTAssertTrue(app.descendants(matching: .any)["partEdit.start"].firstMatch.exists)
XCTAssertTrue(app.descendants(matching: .any)["partEdit.end"].firstMatch.exists)
XCTAssertFalse(app.staticTexts["partEdit.validation"].exists, "the default times do not validate")
XCTAssertFalse(app.buttons["partEdit.delete"].exists, "a new entry has nothing to delete")

let save = app.buttons["partEdit.save"]
```

(Check `BreastSide.left.displayName` is "Left"; adjust the button labels to it.)

- [ ] **Step 10: Run tests**

Run: `cd Packages/BabyTracker && swift test --filter "NewPartModelTests|PartEditTests|BottleEditTests|LogSheetRoutingTests|SessionSheetModelTests" 2>&1 | tail -5`
Expected: all pass. Then (simulator free?) `xcodebuild test … -only-testing:BabyTrackerUITests/LoggingFlowTests/testPastEntryLogsALeftBreastFeedToHistory -only-testing:BabyTrackerUITests/EditFlowTests`
Expected: `** TEST SUCCEEDED **`.

- [ ] **Step 11: Bump and commit**

`EXPECTED_TESTS := 1688` (+2: `aTimedBottleAndANoteAreKept`, `switchingToSleepNeedsAnEnd`).

```bash
/usr/bin/git add -A Packages/BabyTracker UITests Makefile
/usr/bin/git commit -m "feat(logging): one part panel for logging and editing; Past entry folds into it (+2 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Running-timer editor — Start and End per finished part

**Files:**
- Modify: `Packages/BabyTracker/Sources/HomeFeature/RunningTimerEditModel.swift` (`Part`, `init`, `durationText`, `durationRange`, `sheetHeight`)
- Modify: `Packages/BabyTracker/Sources/HomeFeature/RunningTimerEditSheet.swift`
- Modify: `Packages/BabyTracker/Tests/HomeFeatureTests/RunningTimerEditTests.swift`
- Modify: `UITests/BabyTrackerUITests/RunningTimerLifecycleTests.swift:63-66`
- Modify: `Packages/BabyTracker/Sources/HomeFeature/Localizable.xcstrings`
- Modify: `Makefile` (+1 −1)

**Interfaces:**
- Consumes: `RunningTimerEdit` (unchanged — it already accepts components with supplied `startedAt`/`endedAt` and checks order, bounds and `seconds == end − start`).
- Produces: `RunningTimerEditModel.Part` gains `public var start: Date?`, `public var end: Date?`; `seconds` becomes computed. `RunningTimerEditModel.latestPartEnd: Date` (now). `runningPart: (title: String, start: Date)?` replaces `runningSideText`.

`HomeFeature` does not depend on `LoggingFeature`, so the sheet draws its own two `DatePicker` rows (date + time, `in: ...model.latestPartEnd`) and its own read-only "Running" row with the same look as `TimeSpanSection`. Copy lives in `RunningTimerEditCopy` (`startLabel` reused as "Start"? — no: add `partStart = "Start"`, `partEnd = "End"`, `running = "Running"`; all three keys exist in LoggingFeature's catalog — copy their translations into HomeFeature's).

- [ ] **Step 1: Write the failing tests**

Replace `theEditorShowsTheFinishedPartsAndTheRunningSideReadOnly`'s duration lines and delete `theSheetGrowsWithWhatIsOnIt` (geometry). Add:

```swift
@MainActor
@Test func aFinishedSideIsEditedByItsEnd() async throws {
    let (store, babyID, _, timer) = try feedWithParts()
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())
    let left = editor.parts[0]
    let start = try #require(left.start)
    #expect(left.end == start.addingTimeInterval(600))

    editor.parts[0].end = start.addingTimeInterval(480)

    #expect(editor.canSave)
    #expect(try await editor.save())
    #expect(timer.completedComponents[0].durationSeconds == 480)
    #expect(timer.completedComponents[0].endedAt == start.addingTimeInterval(480))
}

@MainActor
@Test func aPartEndPastTheRunningSideIsRefused() throws {
    let (store, babyID, _, timer) = try feedWithParts()
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())
    let sideStart = try #require(timer.currentSideStartedAt)

    editor.parts[0].end = sideStart.addingTimeInterval(30)

    #expect(!editor.canSave)
    #expect(editor.validationMessage == RunningTimerEditCopy.partsExceedElapsed)
}
```

Assertions on `editor.runningSideText` become `editor.runningPart?.title == "Right breast"`.

- [ ] **Step 2: Run to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter RunningTimerEditTests 2>&1 | tail -5`
Expected: compile failure, `value of type 'Part' has no member 'start'`.

- [ ] **Step 3: Implement the model**

```swift
public struct Part: Identifiable, Equatable, Sendable {
    public enum Kind: Equatable, Sendable { case breast(BreastSide), bottle }

    public let id: UUID
    public let kind: Kind
    public let title: String
    /// Nil on an old part recorded without its times; it is shown with its
    /// length only and sent back unchanged.
    public var start: Date?
    public var end: Date?
    public var volumeML: Double
    public var contents: BottleContents
    /// The length recorded before any edit, for a part without times.
    let storedSeconds: Int?

    public var seconds: Int? {
        guard let start, let end else { return storedSeconds }
        return max(0, Int(end.timeIntervalSince(start).rounded()))
    }

    var component: FeedComponent {
        switch kind {
        case .breast(let side):
            .breast(id: id, side: side, seconds: seconds ?? 0, startedAt: start, endedAt: end)
        case .bottle:
            .bottle(id: id, volumeML: volumeML, contents: contents,
                    seconds: seconds.flatMap { $0 > 0 ? $0 : nil }, startedAt: start, endedAt: end)
        }
    }
}
```

In `init`, map `.breast(id, side, seconds, start, end)` → `Part(id:kind:title:start: start, end: end, volumeML: 0, contents: .formula, storedSeconds: seconds)` and `.bottle(id, volumeML, contents, seconds, start, end)` likewise with `storedSeconds: seconds`.

Replace `runningSideText` with:

```swift
/// The part still running: its title and when it began. Read-only.
public var runningPart: (title: String, start: Date)? {
    guard let currentSide, let currentSideStartedAt else { return nil }
    return (RunningTimerEditCopy.breastPart(currentSide), sideMovesWithStart ? startedAt : currentSideStartedAt)
}

public var latestPartEnd: Date { store.now }

public func lengthText(for part: Part) -> String? { part.seconds.map(ElapsedFormatter.precise) }
```

Delete `runningSide(_:since:)`, `durationText`, `durationRange`, `durationAccessibilityLabel`, `sheetHeight`, `nowFooter`'s "keeps counting" text stays. Add copy `partStart`, `partEnd`, `running`, `length` (`HomeL10n.text("Start")`, `"End"`, `"Running"`, `"Length"`).

- [ ] **Step 4: Implement the sheet**

Replace the breast `partRow` stepper and the bottle volume row's surrounding with, per part:

```swift
@ViewBuilder
private func partRows(_ part: Binding<RunningTimerEditModel.Part>) -> some View {
    let value = part.wrappedValue
    Text(value.title).font(.body.weight(.semibold))
    if value.start != nil, value.end != nil {
        DatePicker(RunningTimerEditCopy.partStart,
                   selection: Binding(get: { part.wrappedValue.start ?? .now }, set: { part.wrappedValue.start = $0 }),
                   in: model.startRange.lowerBound...model.latestPartEnd,
                   displayedComponents: [.date, .hourAndMinute])
            .frame(minHeight: 44)
            .accessibilityIdentifier("timerEdit.part.\(value.id).start")
        DatePicker(RunningTimerEditCopy.partEnd,
                   selection: Binding(get: { part.wrappedValue.end ?? .now }, set: { part.wrappedValue.end = $0 }),
                   in: model.startRange.lowerBound...model.latestPartEnd,
                   displayedComponents: [.date, .hourAndMinute])
            .frame(minHeight: 44)
            .accessibilityIdentifier("timerEdit.part.\(value.id).end")
    }
    if let length = model.lengthText(for: value) {
        LabeledContent(RunningTimerEditCopy.length, value: length).monospacedDigit()
    }
    if case .bottle = value.kind {
        // The volume field + stepper row and the contents picker, kept as they are.
        bottleRows(part)
    }
}
```

Move the existing bottle `partRow(...) { HStack { BottleVolumeField … Stepper … } }` and the contents `Picker` into `bottleRows(_:)` unchanged. The running part row becomes:

```swift
if let running = model.runningPart {
    Text(running.title).font(.body.weight(.semibold))
    LabeledContent(RunningTimerEditCopy.partStart,
                   value: running.start.formatted(date: .omitted, time: .shortened))
    LabeledContent(RunningTimerEditCopy.partEnd, value: RunningTimerEditCopy.running)
        .accessibilityIdentifier("timerEdit.runningSide")
}
```

Replace `.sheetHeight(model.sheetHeight)` with `.fittedSheetHeight()` placed directly after `Form { … }`.

- [ ] **Step 5: Update the UI test**

`RunningTimerLifecycleTests.swift:63-66` — keep `timerEdit.runningSide` but query it as `app.descendants(matching: .any)["timerEdit.runningSide"].firstMatch.exists` and drop the "cut off" geometry comment/`isHittable`.

- [ ] **Step 6: Run tests**

Run: `cd Packages/BabyTracker && swift test --filter "RunningTimerEditTests|HomeViewModelTests|TimerTests" 2>&1 | tail -5`
Expected: all pass. UI: `-only-testing:BabyTrackerUITests/RunningTimerLifecycleTests` → `** TEST SUCCEEDED **`.

- [ ] **Step 7: Bump and commit**

`EXPECTED_TESTS := 1689` (+2 −1).

```bash
/usr/bin/git add -A Packages/BabyTracker UITests Makefile
/usr/bin/git commit -m "feat(home): running-timer parts edited by Start and End; sheet fits (+2 −1 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Moment panels — one Time row, fields → Time → Details, Save footer

**Files:**
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/SolidsSheet.swift:185-290`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/SupplementSheet.swift:190-245`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/MeasurementSheet.swift:180-235`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/DiaperSheet.swift:395-500`
- Test: `Packages/BabyTracker/Tests/LoggingFeatureTests/SolidsEditTests.swift`, `DiaperEditTests.swift` (one test each)
- Modify: `UITests/BabyTrackerUITests/GrowthFlowUITests.swift:38` (only if the identifier changes — it should not)
- Modify: `Makefile` (+2)

**Interfaces:**
- Consumes: `EntryPanel`, `DetailsSection`, `EditTimeRow`, `.fittedSheetHeight()`.
- Produces: footer identifiers `solids.save`, `supplement.save`, `measurement.save`, `diaper.save` in both modes; `*.delete` in edit mode only.

Layout for each (new and edit identical except Delete):

| Sheet | Sections in order |
|---|---|
| Solids | Foods (with footer) · Amount · `EditTimeRow(identifier: "solids.time")` · `DetailsSection(identifier: "solids.details") { note }` |
| Supplement | Med (name/picker) · `EditTimeRow(identifier: "supplement.time")` · `DetailsSection(identifier: "supplement.details") { note }` · error |
| Measurement | Fields · `EditTimeRow(identifier: "measurement.time")` (keep `.environment(\.timeZone, …)`) + date validation · error. No Details. |
| Diaper | Choices · `EditTimeRow(identifier: "diaper.time")` · Details (existing `DisclosureGroup` + `detailFields`). New mode: tap saves (no footer); edit mode: footer Save + Delete. |

- [ ] **Step 1: Write the failing tests**

`SolidsEditTests.swift`:

```swift
@Test func newSolidsKeepTheChosenDay() throws {
    let (store, baby, clock) = try fixture()
    let yesterday = clock.now.addingTimeInterval(-26 * 3600)
    let model = SolidsLogModel(store: store, babyID: baby)
    model.foodsText = "pear"
    model.when = yesterday
    try model.log()
    #expect(try store.lastFeed(for: baby)?.startedAt == yesterday)
}
```

`DiaperEditTests.swift`:

```swift
@Test func aNewDiaperKeepsTheChosenDay() throws {
    let (store, baby, clock) = try fixture()
    let yesterday = clock.now.addingTimeInterval(-26 * 3600)
    try DiaperSheet.log(.wet, at: yesterday, store: store, babyID: baby)
    #expect(try store.diapers(for: baby).first?.startedAt == yesterday)
}
```

Solids create state currently lives in the view's `@State` (`foodsText`, `amount`, `when`); this step extracts it into `SolidsLogModel` (`@MainActor @Observable final class` with `foodsText`, `amount`, `note`, `when`, `canSave`, `log() throws` calling `store.logSolids(foods:amount:for:at:note:)` — add `note: String? = nil` to `logSolids` exactly as Task 3 did for the others; do the same for the supplement log call if it takes no note). Diaper's `private func log(_:)` becomes `static func log(_ contents: DiaperContents, at when: Date, store: BabyStore, babyID: UUID) throws` that the view's tap calls. (Check `store.diapers(for:)` exists; otherwise use the helper `DiaperEditTests` already uses to read diapers.)

- [ ] **Step 2: Run to verify they fail**

Run: `cd Packages/BabyTracker && swift test --filter "SolidsEditTests|DiaperEditTests" 2>&1 | tail -5`
Expected: compile failure, `cannot find 'SolidsLogModel'`.

- [ ] **Step 3: Implement each sheet**

Each `Form`-based sheet becomes `NavigationStack { EntryPanel(…) { sections… } .toolbar { extra items } .editLifecycle(…) }` with `onDelete: isEditing ? delete : nil` and `onSave: isEditing ? save : log`. Remove: Solids' toolbar Save button and `.sheetHeight(...)`; Supplement's in-form `Button(SupplementSheetCopy.log)`; Measurement's in-form Save button; every create-mode `DatePicker(... [.hourAndMinute])` and the edit-mode-only time `Section` at the top. Notes move into `DetailsSection` in both modes, `isExpanded` initialised to `!note.isEmpty`.

Diaper: wrap the `VStack` in `ScrollView { … }.scrollBounceBehavior(.basedOnSize).fittedSheetHeight()`; delete `scrollingAtAccessibilitySizes`, `.frame(maxHeight: .infinity, alignment: .top)`, the `.sheetHeight(...)` line and its comment, and `choiceLayoutExtra`. Replace the create-mode `DatePicker(…[.hourAndMinute])` and the edit-mode top `EditTimeRow` with one `EditTimeRow(date: editModel binding ?? $when, latest: editModel?.latestAllowedDate ?? store.now, identifier: "diaper.time", stacksAtAccessibilitySizes: true)` placed after `choices`.

- [ ] **Step 4: Run tests**

Run: `cd Packages/BabyTracker && swift test --filter "SolidsEditTests|SolidsSheetCopyTests|DiaperEditTests|DiaperSheetCopyTests|SupplementEditTests|SupplementSheetTests|MeasurementSheetTests" 2>&1 | tail -5`
Expected: all pass. UI: `-only-testing:BabyTrackerUITests/EditFlowTests -only-testing:BabyTrackerUITests/GrowthFlowUITests -only-testing:BabyTrackerUITests/LoggingFlowTests` → `** TEST SUCCEEDED **`. Fix identifier lookups that pointed at removed buttons (`supplement.log` → `supplement.save`).

- [ ] **Step 5: Bump and commit**

`EXPECTED_TESTS := 1691`.

```bash
/usr/bin/git add -A Packages/BabyTracker UITests Makefile
/usr/bin/git commit -m "feat(logging): moment panels share one layout and a dated Time row (+2 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Session sheet — part rows read Start–End

**Files:**
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/SessionSheetModel.swift:333-350`
- Modify: `Packages/BabyTracker/Tests/LoggingFeatureTests/SessionSheetModelTests.swift:56, 63, 70, 377`

- [ ] **Step 1: Update the expectations (failing)**

```swift
#expect(l.detail == "\(clockText(base, zone: zone))–\(clockText(base.addingTimeInterval(12 * 60), zone: zone))")
#expect(r.detail == "\(clockText(base.addingTimeInterval(15 * 60), zone: zone))–\(clockText(base.addingTimeInterval(30 * 60), zone: zone))")
```

(Read each test's fixture to get the right end; line 70's bottle with no length stays a single time; line 377's 30 s part reads `start–end` with both clocks, which may be the same minute — that is correct.)

- [ ] **Step 2: Run to verify failure**

Run: `cd Packages/BabyTracker && swift test --filter SessionSheetModelTests 2>&1 | tail -5`
Expected: FAIL on the `detail` expectations.

- [ ] **Step 3: Implement**

Breast: `detail = "\(Self.clock(start, zone: feed.timeZoneID))–\(Self.clock(end, zone: feed.timeZoneID))"` — compute `end` first (move the `end =` assignment above `detail`). Bottle: `detail = (seconds ?? 0) > 0 ? "\(time)–\(Self.clock(end, zone: feed.timeZoneID))" : time`, again with `end` computed first.

- [ ] **Step 4: Run tests and commit**

Run: `cd Packages/BabyTracker && swift test --filter SessionSheetModelTests 2>&1 | tail -5` → pass.

```bash
/usr/bin/git add -A Packages/BabyTracker
/usr/bin/git commit -m "feat(logging): session part rows read start–end

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Every remaining sheet fits; delete `sheetHeight(_:)`

**Files:**
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/SessionSheet.swift:71` (+ its `height`)
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/BottleSheet.swift:221` (+ its `height`)
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/MoreSheet.swift:89`
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/HistoryView.swift:778`
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/PendingInvitationRow.swift:101`
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/JoinConfirmationView.swift:136, 159-175` (+ `JoinConfirmationState.sheetHeight` and any test asserting it)
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/EditChrome.swift` (delete `EditFooter.height`)
- Delete: `Packages/BabyTracker/Sources/BabyUI/SheetHeight.swift`

- [ ] **Step 1: Move each call site**

For each file: find the scroll view the sheet's content is in (`List`, `Form`, `ScrollView`) and put `.fittedSheetHeight()` directly on it; delete the `.sheetHeight(…)` line, the `height` property it read, and comments that explain the stated numbers. Where the content is a bare `VStack` (MoreSheet, HistoryView's filter sheet, PendingInvitationRow, JoinConfirmationView — check each), wrap it: `ScrollView { content }.scrollBounceBehavior(.basedOnSize).fittedSheetHeight()`.

- [ ] **Step 2: Delete the old API**

Delete `SheetHeight.swift` and `EditFooter.height`. Run:

`grep -rn "sheetHeight\|EditFooter.height\|choiceLayoutExtra" Packages/BabyTracker/Sources Packages/BabyTracker/Tests Apps`
Expected: no output. Remove tests that asserted stated heights (e.g. a `JoinConfirmationState.sheetHeight` test) and lower `EXPECTED_TESTS` by that count.

- [ ] **Step 3: Build and run the package suite**

Run: `make test 2>&1 | tail -5`
Expected: the literal total matches and passes.

- [ ] **Step 4: Commit**

```bash
/usr/bin/git add -A Packages/BabyTracker Makefile
/usr/bin/git commit -m "refactor(ui): every sheet fits its content; stated heights removed

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: Verify on the simulator and record

**Files:**
- Create: `docs/verification/2026-10-09-entry-panels/` (screenshots + `README.md`)
- Modify: `TASKS.md`

- [ ] **Step 1: Build and install on the shared simulator**

```bash
pgrep xcodebuild && echo BUSY || xcodebuild -scheme BabyTracker \
  -destination 'platform=iOS Simulator,id=<simulator-udid>' build 2>&1 | tail -3
```
Expected: `** BUILD SUCCEEDED **`.

- [ ] **Step 2: Screenshot every panel**

For each of: Past entry (breast, bottle without end, bottle with end, sleep), History edit of breast / bottle / sleep / diaper / solids / supplement / measurement, the session sheet, a running-timer editor with a finished part, Diaper / Solids / Supplement / Measurement new — capture Details collapsed and expanded, at default text size and at `accessibility-large` (`xcrun simctl ui <simulator-udid> content_size accessibility-large`, then back to `large`). Save as `docs/verification/2026-10-09-entry-panels/<panel>-<state>.png`.

Check each by eye: no empty band between the last row and the footer/bottom; no row clipped under the footer; collapsing Details shrinks the sheet; no Duration stepper anywhere.

- [ ] **Step 3: Run the affected UI subset once more**

`-only-testing:` `EditFlowTests`, `LoggingFlowTests`, `RunningTimerLifecycleTests`, `GrowthFlowUITests`, `AccessibilityAuditTests` → `** TEST SUCCEEDED **`, no runner restarts.

- [ ] **Step 4: Record and commit**

Write `README.md` in the verification folder listing commands, results and screenshots. Add an "Entry panels" entry to `TASKS.md` with status and verification.

```bash
/usr/bin/git add docs/verification/2026-10-09-entry-panels TASKS.md
/usr/bin/git commit -m "docs(verification): entry panels — fitted sheets, Start/End everywhere

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```
