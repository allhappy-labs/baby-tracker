# Baby Tracker v1.1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the v1 whole-branch review's Critical and Important findings, and ship the in-scope features that reached v1 with no reachable UI.

**Architecture:** Four independent parts. Part 1 adds a single error-reporting seam on `AppEnvironment` that every write path funnels into, with `OSLog` in the same catch blocks, and extends the existing `store.revision` refresh pattern to the two screens that lack it. Part 2 moves feed pre-selection off a menu and onto the dock button itself, which is what actually removes the third tap. Part 3 adds the settings surface three spec promises need. Part 4 runs last because its first step is a manual check whose result may force a schema change.

**Tech Stack:** Swift 6.4, SwiftUI, SwiftData, Swift Testing, XCUITest, OSLog, WidgetKit, ActivityKit, App Intents. No third-party dependencies.

**Spec:** `docs/superpowers/specs/2026-09-19-baby-tracker-v1_1-design.md`
(v1 spec, amended by Part 2: `docs/superpowers/specs/2026-09-18-baby-tracker-ios-design.md`)

## Global Constraints

- Swift 6 language mode, strict concurrency complete. No `@preconcurrency` escapes.
- **No third-party dependencies.** Not for testing, not for logging, not for anything.
- **Do not remove macOS from `Packages/BabyTracker/Package.swift`.** 137 tests run without a simulator and every task depends on that staying true. Gate iOS-only SwiftUI API with `#if os(iOS)` at the smallest possible scope, never in `BabyCore`/`BabyStore`/`BabyUI`.
- The tombstone column is `isTombstoned`, **never** `isDeleted` — that name is reserved by `NSManagedObject` and SwiftData silently resets it to `false` on every save.
- `ElapsedFormatter.compact(since: Date, now: Date)` = AGE of a past event (zero → "just now"). `ElapsedFormatter.total(_ seconds: Int)` = CUMULATIVE DURATION (zero → "0m"). `ElapsedFormatter.minutes(_:)` = whole minutes with a "<1 min" floor. `ElapsedFormatter.timer(_:)` = running counter. Read the doc comments before writing any call. This bug class recurred eight times in v1.
- Every entry mutation goes through `BabyStore.record(_:on:)`, which logs a `ChangeLogEntry`, rolls back on failure, and bumps `revision`. Never mutate and `context.save()` directly for an entry.
- **All display strings live in the view model, never hard-coded in a view.** The v1 read-through found 13 copy defects; strings in views are untestable by construction.
- Dynamic Type throughout, no fixed font sizes, usable at AX5. All tap targets ≥ 44pt.
- **Error and empty-state copy is factual, never evaluative, and never blames the user.** "Couldn't save that diaper change" — not "Invalid entry", not "You must select a baby first".
- **Entry contents never appear in logs.** Ids, kinds and counts only — never volumes, feed or sleep times, notes, or a baby's name. This is infant health data.
- Tests use Swift Testing (`import Testing`, `@Test`, `#expect`). `@Test` functions touching the `@MainActor` `BabyStore` need `@MainActor`. Test fixtures build base instants as **noon local** via `DateComponents`/`Calendar.current`, never a raw epoch.
- Commits are signed. Export first: `export SSH_AUTH_SOCK=~/Library/Containers/com.bitwarden.desktop/Data/.bitwarden-ssh-agent.sock`, then `git commit -S`. Never `--no-verify`.
- `BabyStoreTests` has a confirmed intermittent SIGSEGV (~1-2 in 8 runs) that can silently drop the whole 31-test suite from a run that exits 0. If a total looks low, re-run and say so. Expected package total at the start of this plan: **137**.

## File Structure

```
Packages/BabyTracker/Sources/
  BabyCore/
    AppLog.swift                  NEW  OSLog categories + content-safe helpers
  BabyStore/
    BabyStore+Feeding.swift       MOD  logSolids, attachBottle (combo)
    BabyStore+Babies.swift        MOD  renameBaby, deleteBaby
    BabyStore.swift               MOD  remote-change observation -> revision
  BabyUI/
    ErrorBanner.swift             NEW  the one banner view
  Features/
    HomeFeature/
      FeedAction.swift            NEW  what the Feed button will do, + its label
      HomeViewModel.swift         MOD  expose feedAction
      QuickLogDock.swift          MOD  split Feed button
      HomeView.swift              MOD  running-feed "Add bottle"
    LoggingFeature/
      SolidsSheet.swift           NEW
      FeedMenuSheet.swift         NEW  the chevron menu
      DiaperSheet.swift           MOD  detail disclosure
      LogCoordinator.swift        MOD  new sheet cases
    HistoryFeature/
      HistoryViewModel.swift      MOD  solids + combo rendering
      EntryEditModel.swift        MOD  diaper detail, bottle contents
    SettingsFeature/              NEW MODULE
      SettingsView.swift          NEW
      SettingsViewModel.swift     NEW
      BabyEditSheet.swift         NEW
Apps/
  BabyTrackerApp/
    AppEnvironment.swift          MOD  AppError, no try!, degraded state
    RootView.swift                MOD  banner + failure screen + settings tab entry
    StoreFailureView.swift        NEW
  BabyTrackerWidgets/
    StatusWidget.swift            MOD  no fabricated placeholder data
  SharedIntents/
    LogIntents.swift              MOD  target the timer's baby (Part 4)
scripts/
  check-tests.sh                  NEW  asserts the literal test total
```

---

# PART 1 — Observability and correctness

### Task 1: The error seam

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyUI/ErrorBanner.swift`
- Create: `Apps/BabyTrackerApp/AppError.swift`
- Test: `Packages/BabyTracker/Tests/BabyUITests/ErrorBannerTests.swift`

**Interfaces:**
- Consumes: `Palette` from `BabyUI`
- Produces: `AppError` (`id: UUID`, `message: String`, `isFatal: Bool`); `ErrorBanner(message:onDismiss:)`

The banner is the only recoverable-failure surface in the app. It must not block interaction — a modal you tap through at 3am while the feed is still happening is hostile.

- [ ] **Step 1: Write the failing test**

`Tests/BabyUITests/ErrorBannerTests.swift`:

```swift
import Testing
import SwiftUI
@testable import BabyUI

@Test func bannerMessagesAreFactualNotEvaluative() {
    // The app never tells a tired parent they did something wrong.
    let forbidden = ["invalid", "you must", "failed to comply", "error:", "illegal"]
    let samples = [
        ErrorBannerCopy.saveFailed(what: "diaper change"),
        ErrorBannerCopy.deleteFailed(what: "feed"),
        ErrorBannerCopy.loadFailed,
    ]
    for sample in samples {
        let lowered = sample.lowercased()
        for word in forbidden {
            #expect(!lowered.contains(word), "\(sample) contains \(word)")
        }
        #expect(sample.first!.isUppercase)
    }
}

@Test func saveFailureNamesTheThingThatFailed() {
    #expect(ErrorBannerCopy.saveFailed(what: "diaper change") == "Couldn't save that diaper change")
    #expect(ErrorBannerCopy.deleteFailed(what: "feed") == "Couldn't delete that feed")
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `cd Packages/BabyTracker && swift test --filter ErrorBannerTests`
Expected: FAIL — `cannot find 'ErrorBannerCopy' in scope`

- [ ] **Step 3: Implement the copy and the banner**

`Sources/BabyUI/ErrorBanner.swift`:

```swift
import SwiftUI

/// Every user-facing failure string in the app. Factual, never evaluative,
/// never blaming — a parent who just lost a log entry is not also told off.
public enum ErrorBannerCopy {
    public static func saveFailed(what: String) -> String {
        "Couldn't save that \(what)"
    }

    public static func deleteFailed(what: String) -> String {
        "Couldn't delete that \(what)"
    }

    public static let loadFailed = "Couldn't load the latest entries"
}

/// Non-blocking. Sits above content, below the navigation bar, and leaves
/// everything beneath it tappable — the thing the user was logging is often
/// still happening.
public struct ErrorBanner: View {
    private let message: String
    private let onDismiss: () -> Void

    public init(message: String, onDismiss: @escaping () -> Void) {
        self.message = message
        self.onDismiss = onDismiss
    }

    public var body: some View {
        HStack(spacing: 12) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(Palette.feed)

            Text(message)
                .font(.subheadline.weight(.medium))
                .foregroundStyle(Palette.primaryText)
                .frame(maxWidth: .infinity, alignment: .leading)

            Button(action: onDismiss) {
                Image(systemName: "xmark")
                    .font(.footnote.weight(.bold))
                    .frame(width: 44, height: 44)
            }
            .buttonStyle(.plain)
            .contentShape(Rectangle())
            .foregroundStyle(Palette.secondaryText)
            .accessibilityLabel("Dismiss")
        }
        .padding(.leading, 16)
        .padding(.vertical, 4)
        .background(.white.opacity(0.08), in: .rect(cornerRadius: 16))
        .padding(.horizontal, 12)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(message)
    }
}
```

`Apps/BabyTrackerApp/AppError.swift`:

```swift
import Foundation

/// One error at a time. A newer failure replaces an older one rather than
/// stacking — the most recent thing the user tried is the one they care about.
struct AppError: Identifiable, Equatable {
    let id = UUID()
    let message: String
    let isFatal: Bool

    static func == (lhs: AppError, rhs: AppError) -> Bool { lhs.id == rhs.id }
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `cd Packages/BabyTracker && swift test --filter ErrorBannerTests`
Expected: PASS, 2 tests

- [ ] **Step 5: Commit**

```bash
export SSH_AUTH_SOCK=~/Library/Containers/com.bitwarden.desktop/Data/.bitwarden-ssh-agent.sock
git add Packages/BabyTracker Apps/BabyTrackerApp/AppError.swift
git commit -S -m "feat: add non-blocking error banner and its copy"
```

---

### Task 2: Content-safe logging

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyCore/AppLog.swift`
- Test: `Packages/BabyTracker/Tests/BabyCoreTests/AppLogTests.swift`

**Interfaces:**
- Consumes: nothing
- Produces: `AppLog.store`, `.timers`, `.sync`, `.widgets`, `.ui` (each a `Logger`); `AppLog.describe(entryKind:id:)` for content-safe entry references

The privacy rule is binding and testable: ids, kinds and counts only. Never volumes, times, notes, or a baby's name. Logs get pasted into bug reports.

- [ ] **Step 1: Write the failing test**

`Tests/BabyCoreTests/AppLogTests.swift`:

```swift
import Testing
import Foundation
@testable import BabyCore

@Test func entryDescriptionsCarryNoContent() {
    let id = UUID()
    let description = AppLog.describe(entryKind: "FeedEntry", id: id)

    // Kind and id only — enough to correlate, useless to a snooper.
    #expect(description.contains("FeedEntry"))
    #expect(description.contains(id.uuidString))
}

@Test func describedEntriesCannotLeakValues() {
    // The helper takes no value parameters at all, which is the actual control.
    // This test pins the shape: a caller has nowhere to put a volume or a note.
    let id = UUID()
    let description = AppLog.describe(entryKind: "FeedEntry", id: id)

    #expect(!description.contains("ml"))
    #expect(!description.contains(":"))          // no clock times
    #expect(description.split(separator: " ").count == 2)
}

@Test func everyCategoryIsDistinct() {
    let categories = AppLog.allCategoryNames
    #expect(Set(categories).count == categories.count)
    #expect(categories.contains("store"))
    #expect(categories.contains("timers"))
    #expect(categories.contains("ui"))
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter AppLogTests`
Expected: FAIL — `cannot find 'AppLog' in scope`

- [ ] **Step 3: Implement**

`Sources/BabyCore/AppLog.swift`:

```swift
import Foundation
import os

/// Logging for the whole app.
///
/// **Binding rule: entry CONTENTS never appear in a log line.** Ids, kinds and
/// counts only — never volumes, never feed or sleep times, never notes, never a
/// baby's name. This is infant health data, and logs get pasted into bug reports
/// and support threads. `describe(entryKind:id:)` is the only sanctioned way to
/// refer to an entry, and it deliberately accepts no value parameters.
public enum AppLog {
    public static let subsystem = "com.olhapi.babytracker"

    public static let store = Logger(subsystem: subsystem, category: "store")
    public static let timers = Logger(subsystem: subsystem, category: "timers")
    public static let sync = Logger(subsystem: subsystem, category: "sync")
    public static let widgets = Logger(subsystem: subsystem, category: "widgets")
    public static let ui = Logger(subsystem: subsystem, category: "ui")

    public static let allCategoryNames = ["store", "timers", "sync", "widgets", "ui"]

    /// The only sanctioned way to name an entry in a log line.
    public static func describe(entryKind: String, id: UUID) -> String {
        "\(entryKind) \(id.uuidString)"
    }
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `swift test --filter AppLogTests`
Expected: PASS, 3 tests

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker
git commit -S -m "feat: add content-safe OSLog categories"
```

---

### Task 3: Log every mutation and rollback

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore.swift` (`record`, `didWrite`)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/LoggingTests.swift`

**Interfaces:**
- Consumes: `AppLog` (Task 2)
- Produces: no new API; `record` gains logging

A rollback that happens silently is exactly the failure nobody could explain afterward. This is the single highest-value log line in the app.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/LoggingTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor
@Test func aFailedRecordStillRollsBackAndRethrows() throws {
    // Logging must not change behaviour: the rollback contract from v1 holds.
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = Baby(name: "Mira", birthDate: .now)
    store.context.insert(baby)

    #expect(throws: (any Error).self) {
        try store.record(.create, on: UnloggableProbe())
    }
    #expect(store.context.hasChanges == false)
    #expect(try store.changeLog().isEmpty)
}

/// Payload that `JSONSerialization` rejects, to drive the failure path.
private struct UnloggableProbe: ChangeLoggable {
    let entityID = UUID()
    var entityTypeName: String { "UnloggableProbe" }
    func payloadFields() -> [String: Any] { ["when": Double.nan] }
}

@MainActor
@Test func loggingDoesNotAlterTheChangeLog() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    _ = try store.logDiaper(.wet, for: baby.id)

    #expect(try store.changeLog().count == 2)
}
```

- [ ] **Step 2: Run the test and confirm it fails or passes trivially**

Run: `swift test --filter LoggingTests`
Expected: PASS on the second test, FAIL to compile on `UnloggableProbe` only if `ChangeLoggable` is not public. If both pass immediately, that is fine — they are the regression net for Step 3, which must not change behaviour.

- [ ] **Step 3: Add the log lines**

In `Sources/BabyStore/BabyStore.swift`, import `BabyCore` (already imported) and add to `record(_:on:)`:

```swift
    public func record(_ operation: ChangeOperation, on entity: some ChangeLoggable) throws {
        do {
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
            didWrite()

            AppLog.store.debug(
                "recorded \(operation.rawValue, privacy: .public) \(AppLog.describe(entryKind: entity.entityTypeName, id: entity.entityID), privacy: .public)"
            )
        } catch {
            context.rollback()
            // The line that makes a silent failure explicable afterwards.
            AppLog.store.error(
                "rolled back \(operation.rawValue, privacy: .public) \(AppLog.describe(entryKind: entity.entityTypeName, id: entity.entityID), privacy: .public): \(error.localizedDescription, privacy: .public)"
            )
            throw error
        }
    }
```

Note: `operation.rawValue`, the entity type and the id are all safe. No payload, no field values.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `swift test --filter LoggingTests && swift test`
Expected: LoggingTests PASS; full suite still 137 + the new tests. If the total looks low, re-run — see Global Constraints.

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker
git commit -S -m "feat: log every mutation and rollback in the store"
```

---

### Task 4: Route every write failure to the user

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyUI/ErrorReporter.swift`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/DiaperSheet.swift:27`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/BottleSheet.swift:67`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/FeedStartSheet.swift:77`
- Modify: `Packages/BabyTracker/Sources/BabyFeature/AddBabySheet.swift:46`
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/EntryEditSheet.swift` (save + delete)
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/HistoryView.swift` (swipe delete)
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeView.swift` (switchSide, stopPrimary)
- Modify: `Apps/BabyTrackerApp/RootView.swift:96,99` (sleep toggle)
- Modify: `Apps/BabyTrackerApp/LiveActivityController.swift` (activity request)
- Test: `Packages/BabyTracker/Tests/BabyUITests/ErrorReporterTests.swift`

**Interfaces:**
- Consumes: `ErrorBannerCopy` (Task 1), `AppLog` (Task 2)
- Produces: `ErrorReporter` (`@MainActor @Observable`, `current: String?`, `report(_ message: String, error: Error, category: Logger)`, `dismiss()`); injected through the SwiftUI environment so feature modules reach it without importing the app target

`UITestSupport.swift`'s `try?` calls are DEBUG-only seeding and stay as they are — they are not user-facing write paths.

- [ ] **Step 1: Write the failing test**

`Tests/BabyUITests/ErrorReporterTests.swift`:

```swift
import Testing
import Foundation
@testable import BabyUI

private struct Boom: Error {}

@MainActor
@Test func reportingSurfacesTheMessage() {
    let reporter = ErrorReporter()
    #expect(reporter.current == nil)

    reporter.report(ErrorBannerCopy.saveFailed(what: "diaper change"), error: Boom())
    #expect(reporter.current == "Couldn't save that diaper change")
}

@MainActor
@Test func aNewerFailureReplacesAnOlderOne() {
    // The most recent thing the user tried is the one they care about.
    let reporter = ErrorReporter()
    reporter.report(ErrorBannerCopy.saveFailed(what: "feed"), error: Boom())
    reporter.report(ErrorBannerCopy.deleteFailed(what: "diaper change"), error: Boom())

    #expect(reporter.current == "Couldn't delete that diaper change")
}

@MainActor
@Test func dismissClearsIt() {
    let reporter = ErrorReporter()
    reporter.report(ErrorBannerCopy.loadFailed, error: Boom())
    reporter.dismiss()

    #expect(reporter.current == nil)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter ErrorReporterTests`
Expected: FAIL — `cannot find 'ErrorReporter' in scope`

- [ ] **Step 3: Implement the reporter**

`Sources/BabyUI/ErrorReporter.swift`:

```swift
import Foundation
import Observation
import SwiftUI
import os
import BabyCore

/// One failure at a time, surfaced to the user and logged for later.
///
/// Feature modules reach this through the SwiftUI environment rather than
/// importing the app target, so a sheet in `LoggingFeature` can report without
/// knowing `AppEnvironment` exists.
@MainActor
@Observable
public final class ErrorReporter {
    public private(set) var current: String?

    public init() {}

    /// `message` is what the user reads; `error` is what the log records.
    /// Only the localized description is logged — never entry contents.
    public func report(
        _ message: String,
        error: any Error,
        category: Logger = AppLog.ui
    ) {
        current = message
        category.error("\(message, privacy: .public): \(error.localizedDescription, privacy: .public)")
    }

    public func dismiss() {
        current = nil
    }
}

public extension EnvironmentValues {
    @Entry var errorReporter: ErrorReporter = ErrorReporter()
}
```

- [ ] **Step 4: Convert every user-facing write site**

Each site follows the same shape. `DiaperSheet.swift:27` becomes:

```swift
    @Environment(\.errorReporter) private var errorReporter
    ...
                    Button {
                        do {
                            _ = try store.logDiaper(contents, for: babyID, at: when)
                            onDone()
                        } catch {
                            errorReporter.report(
                                ErrorBannerCopy.saveFailed(what: "diaper change"),
                                error: error,
                                category: AppLog.store
                            )
                        }
                    } label: {
```

Apply the same pattern to every site in the Files list, with the noun matching what the user was doing: "diaper change", "feed", "bottle", "sleep", "baby", "entry". Do not reword `ErrorBannerCopy`; pass a different noun.

On failure the sheet **stays open** — dismissing after a failed save is what made this invisible in the first place. `onDone()` moves inside the `do` block at every site.

- [ ] **Step 5: Run the tests and build**

Run: `swift test --filter ErrorReporterTests && swift test && cd <repo> && xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build 2>&1 | tail -3`
Expected: 3 new tests PASS; full suite green; BUILD SUCCEEDED

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker Apps
git commit -S -m "feat: surface write failures instead of swallowing them"
```

---

### Task 5: Remove `try!` and add the failure screen

**Files:**
- Modify: `Apps/BabyTrackerApp/AppEnvironment.swift` (the `try!` at the container creation)
- Create: `Apps/BabyTrackerApp/StoreFailureView.swift`
- Modify: `Apps/BabyTrackerApp/RootView.swift` (present banner + failure screen, inject reporter)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/ContainerFailureTests.swift`

**Interfaces:**
- Consumes: `ErrorReporter` (Task 4), `AppLog` (Task 2)
- Produces: `AppEnvironment.storeFailure: String?`; `StoreFailureView(message:onRetry:)`

Today a corrupt store or a failed future migration crashes on **every launch**, with the only user-accessible recovery being deleting the app — which destroys every entry ever logged.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/ContainerFailureTests.swift`:

```swift
import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@Test func containerCreationReportsFailureRatherThanTrapping() {
    // A container pointed at an App Group this process cannot use must throw,
    // not trap. The app turns that throw into a screen; a trap it cannot.
    #expect(throws: (any Error).self) {
        let schema = Schema(BabySchema.all)
        let configuration = ModelConfiguration(
            schema: schema,
            groupContainer: .identifier("group.invalid.nonexistent.babytracker")
        )
        _ = try ModelContainer(for: schema, configurations: configuration)
    }
}

@Test func inMemoryContainerStillWorks() throws {
    _ = try BabySchema.container(inMemory: true)
}
```

- [ ] **Step 2: Run the test and confirm it fails or passes**

Run: `swift test --filter ContainerFailureTests`
Expected: both PASS. If the first does not throw on this toolchain, note it in the report and keep the test as documentation of intent — the app-side change in Step 3 stands regardless.

- [ ] **Step 3: Make `AppEnvironment` degrade instead of trapping**

In `Apps/BabyTrackerApp/AppEnvironment.swift`, replace the `try!` line. `store` becomes optional-backed, with a `storeFailure` message when it could not open:

```swift
    private(set) var storeFailure: String?
    let errorReporter = ErrorReporter()

    // ... in init, replacing `let container = try! BabySchema.container(inMemory: false)`:
    let container: ModelContainer
    do {
        container = try BabySchema.container(inMemory: false)
    } catch {
        // No trap. The on-disk store is left untouched — no automatic reset,
        // no "clear and continue". A parent's log is not ours to discard.
        AppLog.store.fault("container open failed: \(error.localizedDescription, privacy: .public)")
        self.storeFailure = "Couldn't open your saved entries"
        self.store = BabyStore(
            container: try! BabySchema.container(inMemory: true),
            dateProvider: SystemDateProvider(),
            deviceID: Self.deviceID
        )
        self.selection = SelectedBabyStore()
        return
    }
```

The in-memory fallback keeps the app constructible so it can render the failure screen. It is never written to disk and the real store file is untouched.

- [ ] **Step 4: Add the failure screen**

`Apps/BabyTrackerApp/StoreFailureView.swift`:

```swift
import SwiftUI
import BabyUI

/// Shown when the store will not open. Nothing else in the app works, so there
/// is nothing to return to. Offers retry only — deliberately NO action that
/// deletes or resets the store.
struct StoreFailureView: View {
    let message: String
    let onRetry: () -> Void

    var body: some View {
        VStack(spacing: 18) {
            Image(systemName: "exclamationmark.triangle")
                .font(.largeTitle)
                .foregroundStyle(Palette.feed)

            Text(message)
                .font(.title3.weight(.semibold))
                .multilineTextAlignment(.center)

            Text("Your entries are still on this device. Restarting usually helps.")
                .font(.subheadline)
                .foregroundStyle(Palette.secondaryText)
                .multilineTextAlignment(.center)

            Button("Try again", action: onRetry)
                .buttonStyle(.borderedProminent)
                .tint(Palette.feed)
                .frame(minHeight: 44)
        }
        .padding(28)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Palette.ground.ignoresSafeArea())
    }
}
```

- [ ] **Step 5: Wire both into `RootView`**

In `RootView.body`, before the tabs: if `environment.storeFailure` is non-nil render `StoreFailureView`. Otherwise render the tabs with the banner overlaid and the reporter injected:

```swift
        .environment(\.errorReporter, environment.errorReporter)
        .safeAreaInset(edge: .top) {
            if let message = environment.errorReporter.current {
                ErrorBanner(message: message) { environment.errorReporter.dismiss() }
                    .transition(.move(edge: .top).combined(with: .opacity))
            }
        }
```

- [ ] **Step 6: Verify on the simulator**

Run the build, launch, log a diaper — confirm normal operation is unchanged and no banner appears. Report that you checked it.

Run: `cd <repo> && xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build 2>&1 | tail -3`
Expected: BUILD SUCCEEDED

- [ ] **Step 7: Commit**

```bash
git add Packages/BabyTracker Apps
git commit -S -m "feat: degrade instead of trapping when the store cannot open"
```

---

### Task 6: Make History and Insights notice changes

**Files:**
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/HistoryView.swift`
- Modify: `Packages/BabyTracker/Sources/InsightsFeature/InsightsView.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore.swift` (remote-change observation)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/RemoteChangeTests.swift`

**Interfaces:**
- Consumes: `BabyStore.revision`, `BabyStore.didWrite()`
- Produces: `BabyStore.observeRemoteChanges()`

Log a bottle on Home, switch to History, and it is absent. Only `HomeView` observes anything. This is the six-line pattern at `HomeView.swift:103` and `:115`, copied into two files.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/RemoteChangeTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor
@Test func remoteChangeNotificationBumpsTheRevision() async throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    store.observeRemoteChanges()
    let before = store.revision

    // A write from another process arrives as a store-level notification, not
    // through `record`, so nothing in-process bumps the revision for it.
    NotificationCenter.default.post(
        name: ModelContext.didSave,
        object: nil
    )

    // Give the main-actor hop a turn.
    try await Task.sleep(nanoseconds: 50_000_000)
    #expect(store.revision != before)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter RemoteChangeTests`
Expected: FAIL — `value of type 'BabyStore' has no member 'observeRemoteChanges'`

- [ ] **Step 3: Implement the observation**

Add to `Sources/BabyStore/BabyStore.swift`:

```swift
    private var remoteChangeTask: Task<Void, Never>?

    /// A write made by the widget or an App Intent runs against its own
    /// `ModelContainer`, so it never calls `didWrite()` here. Without this, a
    /// widget-logged diaper is invisible to a running app until something else
    /// forces a refresh.
    public func observeRemoteChanges() {
        guard remoteChangeTask == nil else { return }

        remoteChangeTask = Task { [weak self] in
            let notifications = NotificationCenter.default.notifications(named: ModelContext.didSave)
            for await _ in notifications {
                guard let self else { return }
                await MainActor.run { self.didWrite() }
            }
        }
    }
```

Call `observeRemoteChanges()` from `AppEnvironment.init` after the store is constructed.

- [ ] **Step 4: Copy the refresh pattern into both screens**

In `HistoryView` and `InsightsView`, add alongside the existing `.task`:

```swift
        .onChange(of: store.revision) { _, _ in reload() }
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { reload() }
        }
```

with `@Environment(\.scenePhase) private var scenePhase` and `store` retained as a stored property. `reload()` is each view's existing load call, wrapped so a failure reports through `errorReporter` with `ErrorBannerCopy.loadFailed` rather than `try?`.

- [ ] **Step 5: Run the tests and build**

Run: `swift test && cd <repo> && xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build 2>&1 | tail -3`
Expected: full suite green; BUILD SUCCEEDED

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker Apps
git commit -S -m "feat: refresh History and Insights on change and on foreground"
```

---

# PART 2 — Feeding flow

### Task 7: What the Feed button will do

**Files:**
- Create: `Packages/BabyTracker/Sources/HomeFeature/FeedAction.swift`
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeViewModel.swift` (expose `feedAction`)
- Test: `Packages/BabyTracker/Tests/HomeFeatureTests/FeedActionTests.swift`

**Interfaces:**
- Consumes: `store.lastFeed(for:)`, `store.suggestedSide(for:)`, `LogDefaults`
- Produces: `FeedAction` enum — `.startBreast(side: BreastSide)`, `.openBottle`, `.openSolids` — each with `title: String`; `HomeViewModel.feedAction: FeedAction`

This is the whole tap saving, and it is pure logic, so it is fully testable without a simulator. A menu cannot save the tap because a menu still needs a confirming tap; moving the pre-selection onto the button removes it.

- [ ] **Step 1: Write the failing test**

`Tests/HomeFeatureTests/FeedActionTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
@testable import HomeFeature

@MainActor
private func noonLocal() -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15; parts.hour = 12
    return Calendar.current.date(from: parts)!
}

@MainActor
private func freshStore() throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    return (store, baby.id, clock)
}

@MainActor
@Test func withNoHistoryTheButtonStartsTheLeftBreast() throws {
    let (store, babyID, _) = try freshStore()
    let model = HomeViewModel(store: store, babyID: babyID)
    try model.refresh()

    #expect(model.feedAction == .startBreast(side: .left))
    #expect(model.feedAction.title == "Breast · Left")
}

@MainActor
@Test func afterABreastFeedTheButtonOffersTheOppositeSide() throws {
    let (store, babyID, clock) = try freshStore()
    let timer = try store.startFeed(side: .left, for: babyID)
    clock.advance(by: 900)
    _ = try store.stopFeed(timer)

    let model = HomeViewModel(store: store, babyID: babyID)
    try model.refresh()

    #expect(model.feedAction == .startBreast(side: .right))
    #expect(model.feedAction.title == "Breast · Right")
}

@MainActor
@Test func afterABottleTheButtonOffersABottle() throws {
    let (store, babyID, _) = try freshStore()
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: babyID)

    let model = HomeViewModel(store: store, babyID: babyID, volumeUnit: .millilitres)
    try model.refresh()

    // The label states the outcome before the user commits — that is what makes
    // a behaviour-changing button acceptable here.
    #expect(model.feedAction == .openBottle(volumeML: 120, unit: .millilitres))
    #expect(model.feedAction.title == "Bottle · 120 ml")
}

@MainActor
@Test func theBottleLabelHonoursTheVolumeUnit() throws {
    let (store, babyID, _) = try freshStore()
    _ = try store.logBottle(volumeML: 118.294, contents: .formula, for: babyID)

    let model = HomeViewModel(store: store, babyID: babyID, volumeUnit: .ounces)
    try model.refresh()

    #expect(model.feedAction.title == "Bottle · 4 oz")
}

@MainActor
@Test func afterSolidsTheButtonOffersSolids() throws {
    let (store, babyID, _) = try freshStore()
    _ = try store.logSolids(foods: ["pear"], amount: .some, for: babyID)

    let model = HomeViewModel(store: store, babyID: babyID)
    try model.refresh()

    #expect(model.feedAction == .openSolids)
    #expect(model.feedAction.title == "Solids")
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter FeedActionTests`
Expected: FAIL — `cannot find 'FeedAction' in scope`

- [ ] **Step 3: Implement `FeedAction`**

`Sources/HomeFeature/FeedAction.swift`:

```swift
import Foundation
import BabyCore

/// What one tap on the Feed button will do, and the label that says so.
///
/// The button changes behaviour with history, which is normally a bad idea. It
/// is acceptable here only because the button states its outcome on its face —
/// predictability comes from the label, not from the behaviour being fixed.
public enum FeedAction: Equatable, Sendable {
    case startBreast(side: BreastSide)
    case openBottle(volumeML: Double, unit: VolumeUnit)
    case openSolids

    public var title: String {
        switch self {
        case .startBreast(let side):
            "Breast · \(side.displayName)"
        case .openBottle(let volumeML, let unit):
            "Bottle · \(Volume(millilitres: volumeML).formatted(as: unit))"
        case .openSolids:
            "Solids"
        }
    }

    public var symbolName: String {
        switch self {
        case .startBreast: "drop.fill"
        case .openBottle: "waterbottle.fill"
        case .openSolids: "fork.knife"
        }
    }
}
```

`Equatable` is synthesized, so `.openBottle` compares on both associated values — which is why the test above constructs the full case rather than the bare one.

- [ ] **Step 4: Expose it from the view model**

In `HomeViewModel`, add a stored `feedAction` computed during `refresh()`:

```swift
    public private(set) var feedAction: FeedAction = .startBreast(side: .left)

    // inside refresh(), after the snapshot is fetched:
    feedAction = try Self.nextFeedAction(
        lastFeed: snapshot.lastFeed,
        suggestedSide: try store.suggestedSide(for: babyID),
        defaults: LogDefaults(store: store),
        babyID: babyID,
        unit: volumeUnit
    )

    static func nextFeedAction(
        lastFeed: FeedEntry?,
        suggestedSide: BreastSide,
        defaults: LogDefaults,
        babyID: UUID,
        unit: VolumeUnit
    ) throws -> FeedAction {
        guard let lastFeed else { return .startBreast(side: suggestedSide) }

        switch lastFeed.kind {
        case .breast:
            return .startBreast(side: suggestedSide)
        case .bottle:
            return .openBottle(
                volumeML: try defaults.mostUsedBottleVolumeML(for: babyID),
                unit: unit
            )
        case .solids:
            return .openSolids
        }
    }
```

`LogDefaults` currently lives in `LoggingFeature`. Move it to `BabyStore` so `HomeFeature` can use it without depending on another feature module (feature modules must not import each other). Update `LoggingFeature`'s imports.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `swift test --filter FeedActionTests && swift test`
Expected: 5 tests PASS; full suite green

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -S -m "feat: derive the next feed action and its label from history"
```

---

### Task 8: Solids logging

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Feeding.swift`
- Create: `Packages/BabyTracker/Sources/LoggingFeature/SolidsSheet.swift`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/LogCoordinator.swift` (add `.solids`)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/SolidsTests.swift`

**Interfaces:**
- Consumes: `record(_:on:)`
- Produces: `logSolids(foods: [String], amount: SolidsAmount, for: UUID, at: Date?) throws -> FeedEntry`; `LogSheet.solids`; `SolidsSheet(store:babyID:onDone:)`

`FeedKind.solids`, `FeedEntry.foods` and `amountRaw` have existed since v1 with nothing that creates one — and `FeedStartSheet`'s doc comment claims otherwise.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/SolidsTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor
private func noonLocal() -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15; parts.hour = 12
    return Calendar.current.date(from: parts)!
}

@MainActor
@Test func loggingSolidsWritesAFeedWithNeitherSidesNorVolume() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let entry = try store.logSolids(foods: ["pear", "oat"], amount: .some, for: baby.id)

    #expect(entry.kind == .solids)
    #expect(entry.foods == ["pear", "oat"])
    #expect(entry.amount == .some)
    #expect(entry.sides.isEmpty)
    #expect(entry.volumeML == nil)
    #expect(entry.endedAt == clock.now)
    #expect(entry.isCombo == false)
}

@MainActor
@Test func solidsAreChangeLogged() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(noonLocal()))
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    _ = try store.logSolids(foods: ["pear"], amount: .taste, for: baby.id)

    #expect(try store.changeLog().last?.entityType == "FeedEntry")
    #expect(try store.changeLog().last?.operation == .create)
}

@MainActor
@Test func solidsAcceptABackdatedTime() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let earlier = clock.now.addingTimeInterval(-2_400)
    let entry = try store.logSolids(foods: ["pear"], amount: .lots, for: baby.id, at: earlier)

    #expect(entry.startedAt == earlier)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter SolidsTests`
Expected: FAIL — `value of type 'BabyStore' has no member 'logSolids'`

- [ ] **Step 3: Implement the store verb**

Add to `Sources/BabyStore/BabyStore+Feeding.swift`:

```swift
    /// Solids have no timer and no volume — they are a moment plus a
    /// description. `foods` is free text so it does not need a curated list.
    @discardableResult
    func logSolids(
        foods: [String],
        amount: SolidsAmount,
        for babyID: UUID,
        at date: Date? = nil
    ) throws -> FeedEntry {
        let stamp = date ?? now
        let entry = FeedEntry(
            babyID: babyID,
            kind: .solids,
            startedAt: stamp,
            timeZoneID: TimeZone.current.identifier,
            createdBy: deviceID
        )
        entry.foods = foods
        entry.amount = amount
        entry.endedAt = stamp

        context.insert(entry)
        try record(.create, on: entry)
        return entry
    }
```

- [ ] **Step 4: Add the sheet**

`Sources/LoggingFeature/SolidsSheet.swift` — a `TextField` for foods (comma-separated, trimmed, empties dropped), a segmented `Picker` for `SolidsAmount` (Taste / Some / Lots), a `DatePicker` defaulting to now and bounded above by `store.now`, and Save. All strings come from a `SolidsSheetCopy` enum in the same file, per the standing rule. Report failures through `errorReporter` with `ErrorBannerCopy.saveFailed(what: "solids")`; the sheet stays open on failure.

Add `case solids` to `LogSheet` and the matching branch in `LogSheetView`.

- [ ] **Step 5: Run the tests and build**

Run: `swift test --filter SolidsTests && swift test && cd <repo> && xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build 2>&1 | tail -3`
Expected: 3 tests PASS; full suite green; BUILD SUCCEEDED

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -S -m "feat: add solids logging"
```

---

### Task 9: Combo feeds

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/Models/ActiveTimer.swift` (pending bottle fields)
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Feeding.swift` (`attachBottle`, `stopFeed`)
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeView.swift` ("Add bottle" while running)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/ComboFeedTests.swift`

**Interfaces:**
- Consumes: `startFeed`, `stopFeed`, `record`
- Produces: `attachBottle(volumeML: Double, contents: BottleContents, to timer: ActiveTimer) throws`; `ActiveTimer.pendingVolumeML: Double?`, `.pendingContentsRaw: String?`

The v1 spec makes combo feeds a headline model decision and `FeedEntry.isCombo` already exists, but no verb produces one — so both the model surface and the History rendering are dead.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/ComboFeedTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor
private func noonLocal() -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15; parts.hour = 12
    return Calendar.current.date(from: parts)!
}

@MainActor
@Test func attachingABottleMidFeedProducesACombo() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let timer = try store.startFeed(side: .left, for: baby.id)
    clock.advance(by: 600)
    try store.attachBottle(volumeML: 60, contents: .formula, to: timer)
    clock.advance(by: 120)
    let feed = try store.stopFeed(timer)

    #expect(feed.isCombo)
    #expect(feed.totalBreastSeconds == 720)
    #expect(feed.volumeML == 60)
    #expect(feed.contents == .formula)
    // `kind` records how the feed STARTED; it never gates which fields are set.
    #expect(feed.kind == .breast)
}

@MainActor
@Test func attachingABottleDoesNotEndTheFeed() throws {
    // A parent who tops up mid-feed has not finished feeding.
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let timer = try store.startFeed(side: .left, for: baby.id)
    try store.attachBottle(volumeML: 60, contents: .formula, to: timer)

    #expect(try store.activeTimer(kind: .feed, for: baby.id) != nil)
}

@MainActor
@Test func aPlainBreastFeedIsNotACombo() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let timer = try store.startFeed(side: .left, for: baby.id)
    clock.advance(by: 600)
    let feed = try store.stopFeed(timer)

    #expect(feed.isCombo == false)
    #expect(feed.volumeML == nil)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter ComboFeedTests`
Expected: FAIL — `value of type 'BabyStore' has no member 'attachBottle'`

- [ ] **Step 3: Add the pending fields and the verb**

In `ActiveTimer`, add two stored properties with defaults (SwiftData requires them):

```swift
    /// A bottle given during a breast feed. Held on the timer until the feed
    /// ends, then written onto the entry — which is exactly a combo feed.
    public var pendingVolumeML: Double?
    public var pendingContentsRaw: String?

    public var pendingContents: BottleContents? {
        get { pendingContentsRaw.flatMap(BottleContents.init(rawValue:)) }
        set { pendingContentsRaw = newValue?.rawValue }
    }
```

In `BabyStore+Feeding.swift`:

```swift
    /// Does NOT end the feed. The timer keeps running.
    func attachBottle(
        volumeML: Double,
        contents: BottleContents,
        to timer: ActiveTimer
    ) throws {
        timer.pendingVolumeML = volumeML
        timer.pendingContents = contents
        try context.save()
        didWrite()
    }
```

and in `stopFeed`, before `record`:

```swift
        if let pending = timer.pendingVolumeML {
            entry.volumeML = pending
            entry.contents = timer.pendingContents
        }
```

`ActiveTimer` is scratch state and deliberately not change-logged, so `attachBottle` saves directly and calls `didWrite()` — matching `switchSide`.

- [ ] **Step 4: Add the UI**

In `HomeView`'s running state, when the primary timer is a feed, add an **Add bottle** button beside Switch side. It opens the bottle sheet in an "attach" mode whose Save calls `attachBottle` instead of `logBottle`. Once attached, the button reads the attached volume so the state is visible rather than hidden.

- [ ] **Step 5: Run the tests and build**

Run: `swift test --filter ComboFeedTests && swift test && cd <repo> && xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build 2>&1 | tail -3`
Expected: 3 tests PASS; full suite green; BUILD SUCCEEDED

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -S -m "feat: allow a bottle to be attached to a running breast feed"
```

---

### Task 10: The split Feed button and the override menu

**Files:**
- Modify: `Packages/BabyTracker/Sources/HomeFeature/QuickLogDock.swift`
- Create: `Packages/BabyTracker/Sources/LoggingFeature/FeedMenuSheet.swift`
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeView.swift` (dispatch `feedAction`)
- Modify: `Apps/BabyTrackerApp/RootView.swift` (route the new cases)

**Interfaces:**
- Consumes: `FeedAction` (Task 7), `LogSheet.solids` (Task 8)
- Produces: `QuickLogDock(feedAction:onFeed:onFeedMenu:onTap:)`; `FeedMenuSheet(onChoose:onDone:)`

No unit tests here — the logic is in `FeedAction`, already covered. Task 11 asserts the tap counts end to end.

- [ ] **Step 1: Make the Feed button a split control**

Replace the `Tracker.feed` case in `QuickLogDock` with a two-region control: a large primary area labelled from `feedAction.title` that calls `onFeed()`, and a narrower chevron area calling `onFeedMenu()`. Both at least 44pt wide; the primary area stays substantially larger. Accessibility labels: the primary reads `"Log feed, \(feedAction.title)"`, the chevron reads `"Choose feed type"`.

Sleep and Diaper keep their existing buttons and their existing labels `"Log sleep"` / `"Log diaper"` — Task 11 and the v1 UI suite query those strings verbatim.

- [ ] **Step 2: Dispatch the action**

In `HomeView`, `onFeed` switches on `model.feedAction`: `.startBreast(let side)` calls `store.startFeed(side: side, for: babyID)` and refreshes; `.openBottle` opens `LogSheet.bottle`; `.openSolids` opens `LogSheet.solids`. Route failures through `errorReporter`.

- [ ] **Step 3: Add the override menu**

`FeedMenuSheet` — three large rows, Breast / Bottle / Solids, each ≥ 44pt, presented at `.presentationDetents([.medium])`. Choosing Breast starts immediately on the suggested side; the other two open their sheets. Strings live in a `FeedMenuCopy` enum.

- [ ] **Step 4: Verify on the simulator**

Build, launch, seed a baby, and confirm: the Feed button reads `Breast · Left` on a fresh store; tapping it starts a timer immediately; the chevron opens the menu. Screenshot it and **read every string on the screen** — the v1 read-through found 13 copy defects this way. Report what you read, not just that you looked.

Run: `cd <repo> && xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build 2>&1 | tail -3`
Expected: BUILD SUCCEEDED

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker Apps
git commit -S -m "feat: split Feed into a self-describing action and an override menu"
```

---

### Task 11: Render solids and combos, and assert the tap counts

**Files:**
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/HistoryViewModel.swift` (the `"—"` branch)
- Modify: `Packages/BabyTracker/Sources/HomeFeature/HomeViewModel.swift` (`describe`)
- Modify: `UITests/BabyTrackerUITests/LoggingFlowTests.swift`
- Test: `Packages/BabyTracker/Tests/HistoryFeatureTests/SolidsRenderingTests.swift`

**Interfaces:**
- Consumes: `logSolids` (Task 8), `attachBottle` (Task 9), `FeedAction` (Task 7)
- Produces: no new API

**This un-parks a known defect.** `HistoryViewModel` renders `"—"` for a feed with neither sides nor volume. That branch was parked as unreachable because nothing created a solids feed. Task 8 made it reachable, so it must now render the solids detail.

- [ ] **Step 1: Write the failing test**

`Tests/HistoryFeatureTests/SolidsRenderingTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
@testable import HistoryFeature

@MainActor
private func noonLocal() -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15; parts.hour = 12
    return Calendar.current.date(from: parts)!
}

@MainActor
@Test func solidsRenderTheirFoodsAndAmountNotAnEmDash() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    _ = try store.logSolids(foods: ["pear", "oat"], amount: .some, for: baby.id)

    let model = HistoryViewModel(store: store, babyID: baby.id)
    try model.load(dayCount: 1)

    let row = model.days[0].rows[0]
    #expect(row.title == "Solids")
    #expect(row.detail == "Pear, oat · Some")
    #expect(row.detail != "—")
}

@MainActor
@Test func aComboRendersBothHalves() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    let timer = try store.startFeed(side: .left, for: baby.id)
    clock.advance(by: 720)
    try store.attachBottle(volumeML: 60, contents: .formula, to: timer)
    _ = try store.stopFeed(timer)

    let model = HistoryViewModel(store: store, babyID: baby.id)
    try model.load(dayCount: 1)

    let row = model.days[0].rows[0]
    #expect(row.title == "Combo feed")
    #expect(row.detail.contains("12 min"))
    #expect(row.detail.contains("60 ml"))
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter SolidsRenderingTests`
Expected: FAIL — detail is `"—"` for the solids row

- [ ] **Step 3: Render solids in both describe implementations**

In `HistoryViewModel.describe` and `HomeViewModel.describe`, add a solids branch before the `parts.isEmpty` fallback:

```swift
        if !entry.foods.isEmpty {
            // Capitalise the first food only, so "pear, oat" reads "Pear, oat"
            // rather than title-casing every item. There is no existing helper
            // for this; add one to BabyCore if a second caller appears.
            let joined = entry.foods.joined(separator: ", ")
            parts.append(joined.prefix(1).uppercased() + joined.dropFirst())
        }
        if let amount = entry.amount {
            parts.append(amount.rawValue.capitalized)
        }
```

Keep the `"—"` fallback for a genuinely empty feed — it is now a real defensive branch rather than dead code.

- [ ] **Step 4: Add tap-count assertions to the UI suite**

The v1 harness retries taps and returns early when the target already exists, so its "two-tap" tests assert **reachability, not count**. Add a counting helper and four tests that assert the real numbers:

```swift
    /// Counts taps rather than retrying them. A flow that needs an extra tap
    /// must FAIL here, which the retrying harness cannot express.
    @discardableResult
    private func countedTap(_ element: XCUIElement) -> Int {
        XCTAssertTrue(element.waitForExistence(timeout: 4))
        element.tap()
        tapCount += 1
        return tapCount
    }
```

Assert: breast = 2 (Feed, Stop feed), bottle = 2 (Feed, Save), diaper = 2 (Log diaper, Wet), sleep = 2 (Log sleep, Stop sleep). Use the existing `", at "` infix guard when querying History rows — the filter chips are Buttons AND StaticTexts labelled "Feed"/"Sleep"/"Diaper" and will otherwise match.

- [ ] **Step 5: Run everything**

Run: `swift test && cd <repo> && xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests`
Expected: package suite green; UI suite green with the four tap-count tests passing. If a tap-count test fails, the APP is wrong — fix the flow, do not relax the assertion.

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker UITests
git commit -S -m "feat: render solids and combos; assert real tap counts"
```

---

# PART 3 — Settings, babies, diaper detail

### Task 12: Settings screen and the ml/oz preference

**Files:**
- Create: `Packages/BabyTracker/Sources/SettingsFeature/SettingsViewModel.swift`
- Create: `Packages/BabyTracker/Sources/SettingsFeature/SettingsView.swift`
- Modify: `Packages/BabyTracker/Package.swift` (new `SettingsFeature` target + test target)
- Modify: `Apps/BabyTrackerApp/AppEnvironment.swift` (persist `volumeUnit`)
- Modify: `Apps/BabyTrackerApp/RootView.swift` (reach Settings)
- Test: `Packages/BabyTracker/Tests/SettingsFeatureTests/SettingsViewModelTests.swift`

**Interfaces:**
- Consumes: `BabySchema.appGroupID`, `VolumeUnit`
- Produces: `SettingsViewModel(defaults:)` with `volumeUnit: VolumeUnit` (persisting on set) and `unitFooter: String`; `SettingsView(store:selection:settings:)`

`AppEnvironment.volumeUnit` is declared, read in five places, and never assigned — so the v1 spec §11 preference is dead code and every ounce path is unreachable.

Reach Settings from the navigation bar on Home (a gear next to the baby switcher), not a fourth tab. The tab bar is for the three things you log; settings is not one of them.

- [ ] **Step 1: Write the failing test**

`Tests/SettingsFeatureTests/SettingsViewModelTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
@testable import SettingsFeature

private func emptyDefaults() -> UserDefaults {
    let name = "test-\(UUID().uuidString)"
    let defaults = UserDefaults(suiteName: name)!
    defaults.removePersistentDomain(forName: name)
    return defaults
}

@MainActor
@Test func theUnitDefaultsToMillilitres() {
    #expect(SettingsViewModel(defaults: emptyDefaults()).volumeUnit == .millilitres)
}

@MainActor
@Test func theUnitPersistsAcrossInstances() {
    let defaults = emptyDefaults()
    SettingsViewModel(defaults: defaults).volumeUnit = .ounces

    #expect(SettingsViewModel(defaults: defaults).volumeUnit == .ounces)
}

@MainActor
@Test func anUnknownStoredUnitFallsBackRatherThanCrashing() {
    let defaults = emptyDefaults()
    defaults.set("furlongs", forKey: "volumeUnit")

    #expect(SettingsViewModel(defaults: defaults).volumeUnit == .millilitres)
}

@MainActor
@Test func theFooterExplainsWhatTheToggleAffects() {
    let model = SettingsViewModel(defaults: emptyDefaults())
    #expect(model.unitFooter == "Changes how volumes are shown. Nothing already logged is altered.")
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter SettingsViewModelTests`
Expected: FAIL — no such module `SettingsFeature`

- [ ] **Step 3: Add the target and implement the view model**

In `Package.swift` add:

```swift
        .library(name: "SettingsFeature", targets: ["SettingsFeature"]),
        // ...
        .target(name: "SettingsFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI", "BabyFeature"]),
        .testTarget(name: "SettingsFeatureTests", dependencies: ["SettingsFeature"]),
```

`Sources/SettingsFeature/SettingsViewModel.swift`:

```swift
import Foundation
import Observation
import BabyCore
import BabyStore

@MainActor
@Observable
public final class SettingsViewModel {
    private static let unitKey = "volumeUnit"
    private let defaults: UserDefaults

    public init(defaults: UserDefaults? = nil) {
        self.defaults = defaults
            ?? UserDefaults(suiteName: BabySchema.appGroupID)
            ?? .standard
    }

    /// Stored in the App Group so it survives relaunch and is readable by the
    /// extension. An unrecognised stored value falls back rather than trapping.
    public var volumeUnit: VolumeUnit {
        get {
            defaults.string(forKey: Self.unitKey)
                .flatMap(VolumeUnit.init(rawValue:)) ?? .millilitres
        }
        set { defaults.set(newValue.rawValue, forKey: Self.unitKey) }
    }

    public let unitFooter = "Changes how volumes are shown. Nothing already logged is altered."
    public let unitLabel = "Volume units"
}
```

- [ ] **Step 4: Build the screen and read the preference**

`SettingsView` — a `Form` with a segmented `Picker` bound to `volumeUnit` (Millilitres / Ounces) and `unitFooter` beneath. The babies section arrives in Task 13.

In `AppEnvironment`, replace the hardcoded `var volumeUnit: VolumeUnit = .millilitres` with a read through `SettingsViewModel`, so every existing reader picks it up with no other change.

- [ ] **Step 5: Run the tests and build**

Run: `swift test --filter SettingsViewModelTests && swift test && cd <repo> && xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build 2>&1 | tail -3`
Expected: 4 tests PASS; full suite green; BUILD SUCCEEDED

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker Apps
git commit -S -m "feat: add settings with a working ml/oz preference"
```

---

### Task 13: Rename and delete a baby

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Babies.swift`
- Create: `Packages/BabyTracker/Sources/SettingsFeature/BabyEditSheet.swift`
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/SettingsView.swift` (babies section)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/BabyEditTests.swift`

**Interfaces:**
- Consumes: `update(_:)`, `softDelete(_:)`, `babies()`, `SelectedBabyStore.resolve(from:)`
- Produces: `renameBaby(_ baby: Baby, to name: String, birthDate: Date) throws`; `deleteBaby(_ baby: Baby) throws`

Babies can currently only be added and switched. A typo in a name is permanent.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/BabyEditTests.swift`:

```swift
import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@MainActor
@Test func renamingLogsAnUpdateNotACreate() throws {
    let clock = TestDateProvider(.now)
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mria", birthDate: .now)

    clock.advance(by: 60)
    try store.renameBaby(baby, to: "Mira", birthDate: baby.birthDate)

    #expect(baby.name == "Mira")
    #expect(baby.lastModifiedAt == clock.now)
    #expect(try store.changeLog().last?.operation == .update)
}

@MainActor
@Test func deletingABabyTombstonesItAndKeepsTheRow() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    try store.deleteBaby(baby)

    #expect(try store.babies().isEmpty)
    // The row survives — a sync server cannot replicate a deletion it never saw.
    let all = try store.context.fetch(FetchDescriptor<Baby>())
    #expect(all.count == 1)
    #expect(all[0].isTombstoned)
    #expect(try store.changeLog().last?.operation == .delete)
}

@MainActor
@Test func aDeletedBabysEntriesAreNotReturnedForIt() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let mira = try store.addBaby(name: "Mira", birthDate: .now)
    let theo = try store.addBaby(name: "Theo", birthDate: .now)
    _ = try store.logDiaper(.wet, for: mira.id)
    _ = try store.logDiaper(.wet, for: theo.id)

    try store.deleteBaby(mira)

    // Entries are untouched — only the baby is hidden. Deleting a child must
    // not silently destroy their log.
    #expect(try store.lastDiaper(for: mira.id) != nil)
    #expect(try store.babies().map(\.id) == [theo.id])
}

@MainActor
@Test func renamingRejectsAnEmptyName() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now)

    #expect(throws: (any Error).self) {
        try store.renameBaby(baby, to: "   ", birthDate: baby.birthDate)
    }
    #expect(baby.name == "Mira")
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter BabyEditTests`
Expected: FAIL — `value of type 'BabyStore' has no member 'renameBaby'`

- [ ] **Step 3: Implement**

Add to `Sources/BabyStore/BabyStore+Babies.swift`:

```swift
    public enum BabyEditError: Error {
        case emptyName
    }

    func renameBaby(_ baby: Baby, to name: String, birthDate: Date) throws {
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { throw BabyEditError.emptyName }

        baby.name = trimmed
        baby.birthDate = birthDate
        try update(baby)
    }

    /// Tombstone. Entries belonging to this baby are deliberately left intact —
    /// hiding a child must not destroy their log.
    func deleteBaby(_ baby: Baby) throws {
        try softDelete(baby)
    }
```

- [ ] **Step 4: Add the UI**

`BabyEditSheet` — name `TextField`, birth-date `DatePicker` bounded above by `store.now`, Save (disabled on an empty trimmed name), and a Delete row behind a `confirmationDialog` naming the baby. Strings in a `BabyEditCopy` enum. Failures through `errorReporter` with `ErrorBannerCopy.saveFailed(what: "baby")` / `deleteFailed(what: "baby")`.

In `SettingsView`, a Babies section listing each baby with a chevron into the sheet.

`SelectedBabyStore.resolve(from:)` already falls back to the first baby when the selection is stale, so deleting the selected baby is safe — verify that by deleting the selected baby in the simulator and confirming the app switches rather than showing an empty screen.

- [ ] **Step 5: Run the tests and build**

Run: `swift test --filter BabyEditTests && swift test && cd <repo> && xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build 2>&1 | tail -3`
Expected: 4 tests PASS; full suite green; BUILD SUCCEEDED

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker
git commit -S -m "feat: rename and tombstone-delete a baby"
```

---

### Task 14: Diaper detail

**Files:**
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/DiaperSheet.swift`
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/EntryEditModel.swift`
- Modify: `Packages/BabyTracker/Sources/HistoryFeature/EntryEditSheet.swift`
- Test: `Packages/BabyTracker/Tests/HistoryFeatureTests/DiaperDetailTests.swift`

**Interfaces:**
- Consumes: `DiaperEntry.consistency`, `.color`, `store.update(_:)`
- Produces: no new store API

`DiaperEntry.consistency` and `.color` are declared with no reader or writer anywhere. The v1 spec §7 puts them "behind a collapsed 'add detail' disclosure".

**The two-tap fast path must not change.** The disclosure is collapsed by default and is never on the critical path. Task 11's diaper tap-count test must still pass unchanged.

- [ ] **Step 1: Write the failing test**

`Tests/HistoryFeatureTests/DiaperDetailTests.swift`:

```swift
import Testing
import Foundation
import BabyCore
import BabyStore
@testable import HistoryFeature

@MainActor
@Test func editingWritesConsistencyAndColour() throws {
    let clock = TestDateProvider(.now)
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    let entry = try store.logDiaper(.dirty, for: baby.id)

    let model = EntryEditModel(item: .diaper(entry), store: store, unit: .millilitres)
    model.consistency = "Seedy"
    model.color = "Mustard"
    clock.advance(by: 30)
    try model.save()

    #expect(entry.consistency == "Seedy")
    #expect(entry.color == "Mustard")
    #expect(try store.changeLog().last?.operation == .update)
}

@MainActor
@Test func blankDetailStoresNilNotAnEmptyString() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now)
    let entry = try store.logDiaper(.wet, for: baby.id)

    let model = EntryEditModel(item: .diaper(entry), store: store, unit: .millilitres)
    model.consistency = "   "
    model.color = ""
    try model.save()

    #expect(entry.consistency == nil)
    #expect(entry.color == nil)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter DiaperDetailTests`
Expected: FAIL — `value of type 'EntryEditModel' has no member 'consistency'`

- [ ] **Step 3: Implement**

Add `consistency` and `color` to `EntryEditModel`, loaded in `init` and written in `save()` with the same trim-to-nil treatment `note` already uses. In `EntryEditSheet` and `DiaperSheet`, put both behind a `DisclosureGroup` titled from the view model ("Add detail"), collapsed by default.

- [ ] **Step 4: Run the tests and the UI suite**

Run: `swift test --filter DiaperDetailTests && swift test && cd <repo> && xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests`
Expected: 2 tests PASS; full suite green; **the diaper tap-count test still passes at 2**

- [ ] **Step 5: Commit**

```bash
git add Packages/BabyTracker
git commit -S -m "feat: add optional diaper detail behind a disclosure"
```

---

# PART 4 — Widget and Live Activity verification

### Task 15: The manual check, and the widget's fabricated data

**Files:**
- Modify: `Apps/BabyTrackerWidgets/StatusWidget.swift`
- Create: `docs/verification/2026-09-19-widget-live-activity.md`

**Interfaces:**
- Consumes: `WidgetStore.make()`, `homeSnapshot(for:)`
- Produces: a written verification record

**This task begins with a step no agent in this environment can perform.** There is no tap API and device access is off. Nobody has ever placed a widget, tapped a widget button, or seen a Live Activity render.

- [ ] **Step 1: Ask the human to run the manual check**

Stop and ask. The check:
1. Place the small and medium widgets on the Home Screen.
2. Tap the medium widget's **Log diaper**. Open the app → History. Does the entry appear?
3. Tap **Feed** in the app to start a breast feed. Lock the screen. Does the Live Activity render and count up?
4. Tap **Stop** on the Lock Screen. Does the feed end and get saved?
5. With two babies, start a feed for baby A, switch to baby B in the app, then tap Stop on the Lock Screen. **Which baby's feed stops?**

Record every answer in `docs/verification/2026-09-19-widget-live-activity.md`, including anything that did not work. Do not proceed to Task 16 until you have step 5's answer — it determines whether Task 16 is needed at all.

- [ ] **Step 2: Fix the fabricated placeholder data**

Independent of the manual check. `StatusWidget.getTimeline` falls back to `placeholder(in:)` whenever `currentEntry()` throws — which happens when no baby exists or the store read fails. The placeholder is `feedAge: "2h 14m"`, `sleepText: "Awake 40m"`, `diaperAge: "55m"`.

So on a fresh install the Home Screen widget confidently shows numbers that were never logged. Distinguish the cases: `placeholder(in:)` may keep its sample values (that is what the widget gallery previews), but the **timeline** fallback must render em-dashes and no baby name.

```swift
    private static var noData: StatusEntry {
        StatusEntry(date: .now, babyName: "", feedAge: "—", sleepText: "—", diaperAge: "—")
    }
```

Use `noData` in `getTimeline`'s `catch`/`??` path, never `placeholder(in:)`.

- [ ] **Step 3: Verify the build**

Run: `cd <repo> && xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build 2>&1 | tail -3`
Expected: BUILD SUCCEEDED

- [ ] **Step 4: Commit**

```bash
git add Apps docs/verification
git commit -S -m "fix: stop the status widget rendering sample data as real"
```

---

### Task 16: Make Stop act on the timer's baby

**Files:**
- Modify: `Packages/BabyTracker/Sources/BabyStore/TimerActivityAttributes.swift`
- Modify: `Apps/SharedIntents/LogIntents.swift`
- Modify: `Apps/BabyTrackerApp/LiveActivityController.swift`
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/ActivityTargetingTests.swift`

**Interfaces:**
- Consumes: `TimerActivityAttributes`
- Produces: `TimerActivityAttributes.babyID: UUID`

**Conditional on Task 15 step 5.** If the manual check shows Stop already targets the right baby, record that and skip to Step 4 — but say so explicitly rather than silently dropping the task.

`TimerActivityAttributes` carries `babyName` and `kindRaw` but no `babyID`, and the intents resolve their target via `WidgetStore.currentBaby(in:)`. With two babies that can write a feed to the wrong child.

- [ ] **Step 1: Write the failing test**

`Tests/BabyStoreTests/ActivityTargetingTests.swift`:

```swift
import Testing
import Foundation
@testable import BabyStore

@Test func activityAttributesCarryTheBabyTheyBelongTo() {
    let babyID = UUID()
    let attributes = TimerActivityAttributes(
        babyID: babyID,
        babyName: "Mira",
        kindRaw: "feed"
    )

    // Without this, a Stop tapped on the Lock Screen resolves the "currently
    // selected" baby instead — which can be a different child entirely.
    #expect(attributes.babyID == babyID)
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `swift test --filter ActivityTargetingTests`
Expected: FAIL — extra argument `babyID` in call

- [ ] **Step 3: Add `babyID` and use it**

Add `public let babyID: UUID` to `TimerActivityAttributes` and thread it through the initializer and `LiveActivityController.start`.

In `LogIntents.swift`, `StopFeedIntent` and `ToggleSleepIntent` gain an `@Parameter` for the baby id supplied by the activity, falling back to `WidgetStore.currentBaby(in:)` only when absent (which is the widget-button path, where "the selected baby" is correct).

- [ ] **Step 4: Run the tests and build**

Run: `swift test && cd <repo> && xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build 2>&1 | tail -3`
Expected: full suite green; BUILD SUCCEEDED

- [ ] **Step 5: Ask for a re-check**

The two-baby scenario from Task 15 step 5 must be re-run by the human. Record the result in the same verification document.

- [ ] **Step 6: Commit**

```bash
git add Packages/BabyTracker Apps docs/verification
git commit -S -m "fix: stop the timer's baby, not the selected one"
```

---

### Task 17: Guard the test total

**Files:**
- Create: `scripts/check-tests.sh`

**Interfaces:**
- Consumes: nothing
- Produces: a script that fails when the package test total is not what it should be

A confirmed intermittent `BabyStoreTests` SIGSEGV has been observed dropping the entire 31-test suite from a run that **exited 0 with no crash line**. That suite holds the change-log, tombstone and timer invariants. Exit code alone is not evidence.

- [ ] **Step 1: Write the script**

`scripts/check-tests.sh`:

```bash
#!/usr/bin/env bash
# Asserts the LITERAL package test total, not just exit 0.
#
# BabyStoreTests has an intermittent SIGSEGV that can drop its whole 31-test
# suite from a run that still exits 0 and prints no crash line. That suite
# holds the change-log, tombstone and timer invariants — the properties this
# design exists to guarantee. A silently short run must fail the build.
set -euo pipefail

EXPECTED="${1:?usage: check-tests.sh EXPECTED_TOTAL}"

cd "$(dirname "$0")/../Packages/BabyTracker"
output=$(swift test 2>&1)
actual=$(echo "$output" | grep -E '^✔ Test run with' | awk '{s+=$5} END {print s+0}')

if [ "$actual" != "$EXPECTED" ]; then
  echo "FAIL: expected $EXPECTED tests, counted $actual"
  echo "$output" | grep -E '^✔ Test run with|error|signal' || true
  exit 1
fi

echo "OK: $actual tests"
```

- [ ] **Step 2: Verify it catches a short run**

Run it with a deliberately wrong expectation to prove it fails:

Run: `chmod +x scripts/check-tests.sh && ./scripts/check-tests.sh 99999`
Expected: FAIL, printing the real count

Then run it with the true total:

Run: `./scripts/check-tests.sh <the total after Task 16>`
Expected: `OK: <total> tests`

Report both numbers.

- [ ] **Step 3: Commit**

```bash
git add scripts
git commit -S -m "chore: assert the literal package test total"
```

---

## Verification

v1.1 is done when all of these hold:

- `./scripts/check-tests.sh <total>` passes
- `xcodebuild test -scheme BabyTracker -only-testing:BabyTrackerUITests` passes, including the four tap-count tests at 2 taps each
- A failed save shows a banner and leaves the sheet open
- The store failing to open shows a screen, not a crash, and does not touch the store file
- `log stream --predicate 'subsystem == "com.olhapi.babytracker"'` shows mutations and rollbacks, and **no entry contents**
- Logging a bottle on Home makes it visible on History and Insights without relaunching
- The Feed button reads its next action and performs it in one tap
- Solids and combo feeds can be created and render correctly in Home and History
- ml/oz can be changed and every volume in the app follows
- A baby can be renamed and deleted; deleting the selected baby switches rather than stranding
- The manual widget/Live Activity check in `docs/verification/` is filled in, including the two-baby Stop case
