import XCTest

/// The two-tap flows, and a past entry.
///
/// Two deliberate, reported deviations from the brief:
///
/// 1. The brief asserts `app.staticTexts["Diaper"]` / `["Breast"]` /
///    `["Bottle"]` after navigating to History. A History row applies
///    `.accessibilityElement(children: .combine)`, so a row is a single element
///    whose label is the whole sentence ("Diaper, Dirty, at 13:09", "Feed,
///    L 12m, at 10:23") and whose type is not `staticText`; those per-word
///    lookups cannot match anything.
///    The app's combining is correct — VoiceOver should read a row as one
///    element — so the TESTS were changed to match the app, by matching the
///    row's real label prefix.
///
/// 2. Taps on the quick-log dock go through `tap(_:until:)` rather than `tap()`.
///    See that method for why; it is a workaround for an app-side race, not a
///    weakened assertion — the flow assertions themselves are unchanged.
@MainActor
final class LoggingFlowTests: XCTestCase {
    private var app: XCUIApplication!

    /// Taps spent inside the flow under test. Navigation to History afterwards
    /// is not part of the flow and is not counted.
    private var tapCount = 0

    /// What the flow under test is allowed to spend. `countedTap` fails the
    /// moment the budget is exceeded, so a third tap fails AT the third tap.
    ///
    /// A trailing `XCTAssertEqual(tapCount, 2)` would have been tautological:
    /// `countedTap` is called exactly twice per test, so the value is fixed by
    /// construction and the assertion could only ever have been true. The
    /// budget check is the same claim made where it can actually fail.
    private var tapBudget = 0

    override func setUp() {
        continueAfterFailure = false
        tapCount = 0
        tapBudget = 0
        app = Harness.launch(Harness.Launch(reset: true, seedBaby: true))
    }

    override func tearDown() {
        app?.terminate()
        app = nil
    }

    // MARK: - Counting taps

    /// Counts taps rather than retrying them. A flow that needs an extra tap
    /// must FAIL here, which the retrying harness cannot express.
    ///
    /// Every test above named "IsTwoTaps" asserts REACHABILITY, not count:
    /// `tap(_:until:)` retries up to five times and returns `true` before
    /// tapping at all when the target already exists, so a three-tap flow
    /// passes them identically. That is how this app shipped at three taps
    /// with a green suite. Nothing in here may retry.
    ///
    /// The one concession to the dock race documented on `tap(_:until:)` is
    /// the normalised-coordinate tap: `tap()` asks the element for a hit point
    /// and silently does nothing when `HomeView`'s one-second republish hands
    /// back {-1, -1}. A coordinate never has to ask. It is still exactly one
    /// tap — the retry, not the hit-point strategy, is what would make the
    /// count a lie.
    @discardableResult
    private func countedTap(
        _ element: XCUIElement,
        _ what: String,
        file: StaticString = #filePath,
        line: UInt = #line
    ) -> Int {
        XCTAssertLessThan(
            tapCount, tapBudget,
            "the flow spent its \(tapBudget)-tap budget before \(what)",
            file: file, line: line
        )
        XCTAssertTrue(
            element.waitForExistence(timeout: 10),
            "\(what) never appeared, so the flow cannot be counted",
            file: file, line: line
        )
        XCTAssertTrue(
            element.waitToBeHittable(timeout: 10),
            "\(what) never became hittable", file: file, line: line
        )
        element.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        tapCount += 1
        return tapCount
    }

    // MARK: - The two-tap flows, counted

    /// Tap 1 starts Left from the dock. Tap 2 stops it.
    func testBreastFeedIsExactlyTwoTaps() {
        tapBudget = 2
        countedTap(app.buttons["home.start.left"], "the dock's Left")
        // The store intentionally discards breast intervals under 30 seconds.
        // Waiting does not change the two-tap interaction being measured.
        Thread.sleep(forTimeInterval: 31)
        countedTap(app.buttons["home.stop"], "Stop")

        // Stopped into the break: Left and Right are offered again.
        XCTAssertTrue(app.buttons["home.start.right"].waitForExistence(timeout: 5))

        XCTAssertTrue(app.goToTab("History"))
        XCTAssertTrue(
            app.waitForRow(startingWith: "Feed, L ", timeout: 5),
            "the two counted taps did not produce a Left feed row"
        )
    }

    /// Tap 1 opens the Bottle sheet, tap 2 is an amount, which logs the
    /// finished bottle at once.
    ///
    /// `waitForRow(startingWith: "Feed")` would be VACUOUS on the seeded
    /// fixture, which already holds feeds; the assertion is a DELTA read off
    /// what Today says: its header's spoken totals and its rows. A new
    /// bottle is a new feed row (and one more feed in the header), or one
    /// more chip on the feed it joined inside the break limit. Zero taps
    /// leaves both where they were, and the test goes red.
    func testBottleIsExactlyTwoTaps() {
        tapBudget = 2
        app.terminate()
        app = Harness.launch(
            Harness.Launch(reset: true, seedBaby: true, seedFixture: true)
        )

        XCTAssertTrue(app.goToTab("History"))
        let before = app.dayTotalText()
        XCTAssertNotNil(before, "the fixture should have a total for today")
        XCTAssertTrue(before?.hasPrefix("Today, ") == true, "the first day is not today: \(before ?? "")")
        XCTAssertTrue(app.goToTab("Home"))

        countedTap(app.buttons["home.bottle"], "the dock's Bottle")
        countedTap(app.buttons["bottle.chip.2"], "the bottle sheet's third amount")
        XCTAssertTrue(app.buttons["bottle.chip.2"].waitForNonExistence(timeout: 5), "the amount did not close the sheet")

        XCTAssertTrue(app.goToTab("History"))
        let deadline = Date().addingTimeInterval(10)
        while app.dayTotalText() == before, Date() < deadline {
            _ = XCTWaiter.wait(for: [XCTestExpectation(description: "poll")], timeout: 0.4)
        }
        XCTAssertNotEqual(
            app.dayTotalText(), before,
            "the two counted taps did not change today's totals; they read \(before ?? "nothing")"
        )
        XCTAssertTrue(
            app.waitForRow(startingWith: "Feed, ", timeout: 5),
            "the two counted taps did not produce a feed row"
        )
    }

    func testDiaperIsExactlyTwoTaps() {
        tapBudget = 2
        countedTap(app.buttons["home.diaper"], "the dock's Diaper")
        countedTap(app.buttons["diaper.wet"], "Wet")

        XCTAssertTrue(app.goToTab("History"))
        XCTAssertTrue(
            app.waitForRow(startingWith: "Diaper", timeout: 5),
            "the two counted taps did not produce a Diaper row"
        )
    }

    func testSleepIsExactlyTwoTaps() {
        tapBudget = 2
        countedTap(app.buttons["home.sleep"], "the dock's Sleep")
        countedTap(app.buttons["home.wake"], "Wake up")

        // Back to idle is the proof the stop landed. History is not asserted
        // here: `SleepClassifier` titles the row "Nap" or "Night sleep"
        // depending on the wall-clock hour the suite happens to run at, and a
        // flow test must not depend on that.
        XCTAssertTrue(app.buttons["home.sleep"].waitForExistence(timeout: 5))
    }

    /// Something that already happened: More → Past entry → Breast, Left,
    /// the default ten minutes up to now → Save → a Left feed row in History.
    func testPastEntryLogsALeftBreastFeedToHistory() {
        XCTAssertTrue(app.tap(app.buttons["home.more"], until: app.buttons["more.pastEntry"]))
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
        XCTAssertTrue(save.isEnabled)
        save.tap()
        XCTAssertTrue(save.waitForNonExistence(timeout: 5), "Save did not close the sheet")

        XCTAssertTrue(app.goToTab("History"))
        XCTAssertTrue(
            app.waitForRow(startingWith: "Feed, ", timeout: 5),
            "the past entry did not produce a feed row"
        )
        let row = app.historyRow(startingWith: "Feed, ")
        XCTAssertTrue(row.label.hasPrefix("Feed, L "), "the row is not the left side: \(row.label)")
    }
}

extension XCUIApplication {
    /// Matches a real History entry row, and nothing else.
    ///
    /// A bare `label BEGINSWITH "Diaper"` over `descendants(matching: .any)` is
    /// VACUOUS: the History filter chip is a Button labelled exactly `Diaper`
    /// that also renders a `StaticText` child labelled `Diaper`, and both are
    /// present on every History render — including the empty one, as
    /// `screen-02-history-empty` shows. The assertion passed whether or not
    /// anything had been logged.
    ///
    /// `Breast` and `Bottle` were checked rather than assumed, and are
    /// vulnerable too: `HistoryRowView`'s combined element sits alongside an
    /// un-combined `StaticText` child carrying the bare title (`Bottle`), the
    /// bottle sheet has a `Breast milk` button, and Home — which stays in the
    /// hierarchy behind the History tab — carries `Bottle · 90 ml · mixed`.
    ///
    /// So: restrict to text/cell/button elements, and require the `", at "`
    /// infix that only `HistoryRowView`'s own combined label produces
    /// (`"\(row.title), \(row.detail), at \(row.time)"`). The infix is what
    /// keeps the query honest, not the element type — every chip label is a
    /// bare tracker name.
    ///
    /// `buttons` joined the list in Task 23: a row is now wrapped in a `Button`
    /// that opens the edit sheet, and the row's combined accessibility element
    /// is published on that button rather than as a `StaticText`.
    func waitForRow(startingWith prefix: String, timeout: TimeInterval) -> Bool {
        let predicate = NSPredicate(
            format: "label BEGINSWITH %@ AND label CONTAINS %@", prefix, ", at "
        )
        let deadline = Date().addingTimeInterval(timeout)
        repeat {
            if staticTexts.matching(predicate).firstMatch.exists { return true }
            if cells.matching(predicate).firstMatch.exists { return true }
            if buttons.matching(predicate).firstMatch.exists { return true }
            _ = XCTWaiter.wait(for: [XCTestExpectation(description: "poll")], timeout: 0.4)
        } while Date() < deadline
        return false
    }
}

extension XCUIApplication {
    /// What History's first day says: its header as VoiceOver reads it —
    /// "Today, 3 feeds, 2h 21m sleep, 2 diapers" — followed by every row's
    /// label on screen. A change is the cheapest proof that a save landed
    /// in a store that already contained entries of the same kind, whether
    /// it added a row or a chip to a row.
    func dayTotalText() -> String? {
        let header = descendants(matching: .any)["history.day.0"].firstMatch
        guard header.exists else { return nil }
        let rows = buttons.matching(NSPredicate(format: "identifier BEGINSWITH 'history.row.'"))
            .allElementsBoundByIndex.map(\.label)
        return ([header.label] + rows).joined(separator: "\n")
    }
}

extension XCUIElement {
    func waitToBeHittable(timeout: TimeInterval) -> Bool {
        let expectation = XCTNSPredicateExpectation(
            predicate: NSPredicate(format: "isHittable == true"), object: self
        )
        return XCTWaiter.wait(for: [expectation], timeout: timeout) == .completed
    }
}
