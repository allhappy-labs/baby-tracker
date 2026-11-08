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

    /// Tap 1 opens the Bottle sheet, tap 2 is Log, which logs the finished
    /// bottle at once with the amount it opened on (the previous bottle's).
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
        countedTap(app.buttons["bottle.log"], "the bottle sheet's Log")
        XCTAssertTrue(app.buttons["bottle.log"].waitForNonExistence(timeout: 5), "Log did not close the sheet")

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
        XCTAssertTrue(app.tap(app.buttons["more.pastEntry"], until: app.buttons["pastEntry.diaper"]),
                      "Past entry does not list Diaper")
        XCTAssertTrue(app.buttons["pastEntry.solids"].exists, "Past entry does not list Solids")
        XCTAssertTrue(app.tap(app.buttons["pastEntry.breast"], until: app.buttons["partEdit.save"]))

        // Choose Right, then back to Left.
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

    // MARK: - Home buttons and the amount screen

    /// The order decides the dock: Bottle first is the big button and opens
    /// the bottle panel; Breast, now a tile, asks which side.
    func testABottleFirstOrderLeadsHomeWithBottle() {
        app.terminate()
        app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, homeOrder: "bottle,breast,sleep,diaper"))
        XCTAssertTrue(app.buttons["home.breast"].waitForExistence(timeout: 15), "Breast is not a tile")
        XCTAssertFalse(app.buttons["home.start.left"].exists, "Left is still a big button")

        XCTAssertTrue(app.tap(app.buttons["home.breast"], until: app.buttons["Left breast"]),
                      "the Breast tile did not ask which side")
        app.buttons["Left breast"].tap()
        XCTAssertTrue(app.buttons["home.stop"].waitForExistence(timeout: 8), "Left did not start")
        // Under 30 seconds: discarded at Stop, which is all this needs.
        XCTAssertTrue(app.tap(app.buttons["home.stop"], until: app.buttons["home.bottle"]), "Stop did not stop")

        XCTAssertTrue(app.tap(app.buttons["home.bottle"], until: app.buttons["bottle.start"]),
                      "the big Bottle button did not open the bottle panel")
    }

    /// The amount screen offers contents, and what is chosen there is what
    /// the bottle is saved with: opened again from History, it reads the same.
    ///
    /// History's rows do not name a bottle's contents, so the saved value is
    /// read where the app shows it — the bottle's own panel.
    func testContentsChosenOnTheAmountScreenAreSaved() {
        XCTAssertTrue(app.tap(app.buttons["home.bottle"], until: app.buttons["bottle.start"]))
        XCTAssertTrue(app.tap(app.buttons["bottle.start"], until: app.buttons["home.stop"]), "the bottle did not start")
        // A bottle stopped inside its first second is discarded, not saved.
        Thread.sleep(forTimeInterval: 2)
        XCTAssertTrue(app.tap(app.buttons["home.stop"], until: app.buttons["bottle.done"]), "Stop did not ask for the amount")

        // The amount screen's only segmented control.
        let contents = app.segmentedControls.firstMatch
        XCTAssertTrue(contents.waitForExistence(timeout: 5), "the amount screen offers no contents")
        // Whichever contents the bottle was not started with.
        guard let other = contents.buttons.allElementsBoundByIndex.first(where: { !$0.isSelected }) else {
            return XCTFail("no other contents to choose")
        }
        let chosen = other.label
        other.tap()
        app.buttons["bottle.done"].tap()
        XCTAssertTrue(app.buttons["home.start.left"].waitForExistence(timeout: 8), "the bottle did not stop")

        XCTAssertTrue(app.goToTab("History"))
        let row = app.historyRows(startingWith: "Feed, ").firstMatch
        XCTAssertTrue(row.waitForExistence(timeout: 8), "the bottle is not in History")
        let saved = app.segmentedControls.buttons[chosen]
        XCTAssertTrue(app.tap(row, until: saved), "the bottle's panel does not offer \(chosen)")
        XCTAssertTrue(saved.isSelected, "the bottle was not saved as \(chosen)")
    }

    /// A Quick Log widget tile whose action needs the app opens that baby's
    /// panel on Home.
    func testAWidgetLinkOpensItsPanel() {
        app.terminate()
        app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, logLink: "growth"))
        XCTAssertTrue(app.buttons["measurement.save"].waitForExistence(timeout: 15),
                      "the widget's Growth link did not open the growth panel")
    }

    // MARK: - Vaccinations

    /// More → Vaccine → a new name saves with dose 1; History reads it.
    func testAVaccineIsLoggedFromMore() {
        app.terminate()
        app = Harness.launch(Harness.Launch(reset: true, seedBaby: true))
        XCTAssertTrue(app.tap(app.revealOnHome(app.buttons["home.more"]), until: app.buttons["more.vaccine"]))
        let name = app.textFields["immunization.name"]
        XCTAssertTrue(app.tap(app.buttons["more.vaccine"], until: name))
        let save = app.buttons["immunization.save"]
        XCTAssertFalse(save.isEnabled, "Save is on with no vaccine named")
        name.tap()
        name.typeText("Rotarix")
        let dose = app.descendants(matching: .any)["immunization.dose"]
        XCTAssertEqual(dose.value as? String, "1", "a first shot of a vaccine did not offer dose 1")
        XCTAssertTrue(save.isEnabled)
        save.tap()
        XCTAssertTrue(app.buttons["home.more"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.goToTab("History"))
        XCTAssertTrue(app.historyRows(startingWith: "Vaccine, Rotarix · dose 1").firstMatch.waitForExistence(timeout: 15),
                      "History has no row for the vaccination")
    }

    /// Nothing is selected on open; picking a saved vaccine offers its next dose.
    func testPickingASavedVaccineOffersTheNextDose() {
        app.terminate()
        app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedVaccines: true, openVaccine: true))
        let picker = app.descendants(matching: .any)["immunization.picker"]
        XCTAssertTrue(picker.waitForExistence(timeout: 15))
        let save = app.buttons["immunization.save"]
        XCTAssertFalse(save.isEnabled, "a vaccine was selected before the caregiver chose one")
        let dose = app.descendants(matching: .any)["immunization.dose"]
        XCTAssertNotEqual(dose.value as? String, "3")
        picker.tap()
        let choice = app.buttons["Prevenar 13"]
        XCTAssertTrue(choice.waitForExistence(timeout: 10))
        choice.tap()
        XCTAssertEqual(dose.value as? String, "3", "the next dose after two is three")
        XCTAssertTrue(save.isEnabled)
        keep("panel-picked")
        save.tap()
        XCTAssertTrue(app.buttons["home.more"].waitForExistence(timeout: 15))
        XCTAssertTrue(app.goToTab("History"))
        XCTAssertTrue(app.historyRows(startingWith: "Vaccine, Prevenar 13 · dose 3").firstMatch.waitForExistence(timeout: 15))
        keep("history-row")
    }

    /// The Insights card opens the Immunizations screen; a row opens its
    /// panel; deleting it there drops the row from the screen.
    func testTheImmunizationsCardOpensTheScreen() {
        app.terminate()
        app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedVaccines: true))
        XCTAssertTrue(app.goToTab("Insights"))
        let open = app.buttons["insights.immunizations.open"]
        XCTAssertTrue(open.waitForExistence(timeout: 15))
        for _ in 0..<8 where !open.isHittable { app.swipeUp() }
        keep("insights-card")
        let rows = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH 'immunizations.row.'"))
        XCTAssertTrue(app.tap(open, until: rows.firstMatch))
        XCTAssertEqual(rows.count, 4)
        keep("screen")
        XCTAssertTrue(app.tap(rows.element(boundBy: 0), until: app.buttons["immunization.delete"]))
        app.buttons["immunization.delete"].tap()
        let confirm = app.buttons["immunization.confirmDelete"].firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 10))
        confirm.tap()
        let three = NSPredicate { _, _ in rows.count == 3 }
        XCTAssertEqual(XCTWaiter.wait(for: [XCTNSPredicateExpectation(predicate: three, object: nil)], timeout: 15), .completed,
                       "the deleted shot is still on the Immunizations screen")
    }

    /// Deleting the only shot from the Immunizations screen leaves the screen
    /// up, saying there are none; it does not drop back to Insights.
    func testDeletingTheLastShotLeavesTheScreenSayingThereAreNone() {
        app.terminate()
        app = Harness.launch(Harness.Launch(reset: true, seedBaby: true))
        XCTAssertTrue(app.tap(app.revealOnHome(app.buttons["home.more"]), until: app.buttons["more.vaccine"]))
        let name = app.textFields["immunization.name"]
        XCTAssertTrue(app.tap(app.buttons["more.vaccine"], until: name))
        name.tap()
        name.typeText("Rotarix")
        app.buttons["immunization.save"].tap()
        XCTAssertTrue(app.buttons["home.more"].waitForExistence(timeout: 15))

        XCTAssertTrue(app.goToTab("Insights"))
        let open = app.buttons["insights.immunizations.open"]
        XCTAssertTrue(open.waitForExistence(timeout: 15))
        for _ in 0..<8 where !open.isHittable { app.swipeUp() }
        let rows = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH 'immunizations.row.'"))
        XCTAssertTrue(app.tap(open, until: rows.firstMatch))
        XCTAssertTrue(app.tap(rows.firstMatch, until: app.buttons["immunization.delete"]))
        app.buttons["immunization.delete"].tap()
        let confirm = app.buttons["immunization.confirmDelete"].firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 10))
        confirm.tap()
        let empty = app.descendants(matching: .any)["immunizations.empty"].firstMatch
        XCTAssertTrue(empty.waitForExistence(timeout: 15), "the screen did not stay to say there are no vaccinations")
        let range = app.descendants(matching: .any)["insights.range"].firstMatch
        XCTAssertFalse(range.exists && range.isHittable, "the screen dropped back to Insights")
        keep("screen-empty")
    }

    /// A named screenshot kept with the result, for the verification notes.
    private func keep(_ name: String) {
        let shot = XCTAttachment(screenshot: app.screenshot())
        shot.name = name
        shot.lifetime = .keepAlways
        add(shot)
    }

    /// A Quick Log widget tile for Vaccine opens the vaccine panel.
    func testAVaccineLinkOpensItsPanel() {
        app.terminate()
        app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, logLink: "vaccine"))
        XCTAssertTrue(app.textFields["immunization.name"].waitForExistence(timeout: 15),
                      "the widget's Vaccine link did not open the vaccine panel")
    }

    /// A link for a baby deleted since the widget drew brings Home forward
    /// and opens nothing.
    func testAWidgetLinkForAnUnknownBabyOnlyOpensHome() throws {
        XCTAssertTrue(app.homeMarker.waitForExistence(timeout: 15))
        let link = try XCTUnwrap(URL(string: "babytracker://log?kind=solids&baby=00000000-0000-4000-8000-000000000000"))
        XCUIDevice.shared.system.open(link)
        // The simulator asks before handing a custom scheme to the app.
        let open = XCUIApplication(bundleIdentifier: "com.apple.springboard").buttons["Open"]
        if open.waitForExistence(timeout: 5) { open.tap() }
        XCTAssertTrue(app.wait(for: .runningForeground, timeout: 10), "the link did not bring the app forward")
        XCTAssertTrue(app.buttons["home.start.left"].waitForExistence(timeout: 8), "Home is not up")
        XCTAssertFalse(app.buttons["Cancel"].waitForExistence(timeout: 2), "a panel opened for a baby this phone does not have")
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
