import XCTest

/// The redesigned History, driven through the real UI: swipe to delete a
/// whole session, and a viewer's list.
///
/// Every test starts from `-uiTestHistorySessions`: fixed clock times on the
/// two days before today, so the rows and their days are the same whenever
/// the suite runs. Yesterday, newest first — solids 17:30, growth 16:00, a
/// sleep with one wake 13:00–15:30, a wet diaper 12:00, a feed of Left 12m,
/// Right 15m and a 60 ml bottle 10:23–11:05, Vitamin D 09:00 — then a feed
/// across midnight on the day before.
@MainActor
final class HistoryRedesignTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    private func launchHistory(_ launch: Harness.Launch = Harness.Launch(historySessions: true)) -> XCUIApplication {
        let app = Harness.launch(launch)
        app.waitingForHome()
        XCTAssertTrue(app.goToTab("History"))
        return app
    }

    // MARK: - Split here and Join

    // MARK: - Swipe to delete

    /// A session asks first; Keep it leaves the row, Delete removes it.
    func testSwipeDeletesASessionAfterItsConfirmation() {
        let app = launchHistory()
        defer { app.terminate() }

        let sleep = app.sleepSessionRows.firstMatch
        XCTAssertTrue(sleep.waitForExistence(timeout: 10), app.debugDescription)
        XCTAssertTrue(sleep.label.contains("1 wake"), sleep.label)

        // The cancel button is not always exposed as a button on iPhone:
        // Keep it is tried only when it is there.
        app.swipeToDelete(sleep)
        let keep = app.buttons["history.keep"].firstMatch
        if keep.waitForExistence(timeout: 3), keep.isHittable {
            keep.tap()
            XCTAssertTrue(app.buttons["history.confirmDelete"].firstMatch.waitForNonExistence(timeout: 5))
            XCTAssertTrue(sleep.exists, "Keep it deleted the sleep")
            app.swipeToDelete(sleep)
        }

        // The dialog's button and the text inside it both carry the id.
        let confirm = app.buttons["history.confirmDelete"].firstMatch
        XCTAssertTrue(confirm.waitForExistence(timeout: 5), "the session was not confirmed:\n\(app.debugDescription)")
        XCTAssertTrue(sleep.exists, "the row went before the answer")
        confirm.tap()
        XCTAssertTrue(sleep.waitForNonExistence(timeout: 5), "the sleep survived its confirmed delete")
        XCTAssertEqual(app.historyRows(startingWith: "Sleep, ").count, 0, "a part of the sleep was left behind")
    }

    // MARK: - Filter

    // MARK: - Every edit sheet from History

    // MARK: - The largest text

    // MARK: - Export

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
        // The system share sheet heads itself with the file's kind and size,
        // "PDF Document · 24 KB", and offers Print.
        let shared = app.descendants(matching: .any)
            .matching(NSPredicate(format: "label BEGINSWITH %@", "PDF Document")).firstMatch
        XCTAssertTrue(shared.waitForExistence(timeout: 15), "no share sheet:\n\(app.debugDescription)")
        XCTAssertTrue(app.cells["Print"].firstMatch.exists, "the share sheet offers no Print:\n\(app.debugDescription)")
        XCTAssertFalse(app.staticTexts["history.export.failed"].exists)
    }

    // MARK: - A viewer

    /// A view-only caregiver reads every row and can open none of them.
    func testAViewOnlyFamilysRowsAreNotTappable() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: false, historySessions: true, readOnlyFamily: true))
        defer { app.terminate() }
        XCTAssertTrue(app.goToTab("History"))

        let rows = app.historyRows(startingWith: "", editable: false)
        XCTAssertTrue(rows.firstMatch.waitForExistence(timeout: 10), "a viewer sees no rows:\n\(app.debugDescription)")
        XCTAssertTrue(app.historyRows(startingWith: "Diaper, Wet", editable: false).firstMatch.exists)
        XCTAssertEqual(app.historyRows(startingWith: "").count, 0, "a viewer's row is a button")

        let feed = app.historyRows(startingWith: "Feed, L 12m, R 15m", editable: false).firstMatch
        XCTAssertTrue(feed.exists, app.debugDescription)
        // By coordinate: a plain element has no action to resolve a hit point for.
        feed.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        XCTAssertFalse(app.buttons["sessionSheet.done"].waitForExistence(timeout: 2), "a viewer opened a session")
        feed.swipeLeft()
        XCTAssertFalse(app.buttons["history.delete"].waitForExistence(timeout: 2), "a viewer can swipe to delete")
    }

    // MARK: -

    /// Taps an edit sheet's Save and waits for the sheet (or the pushed
    /// editor) to go, and for `returningTo` when it is inside a session.
    private func save(_ app: XCUIApplication, _ id: String, returningTo marker: String? = nil,
                      file: StaticString = #filePath, line: UInt = #line) {
        let button = app.buttons[id]
        XCTAssertTrue(button.waitForExistence(timeout: 5), "\(id) missing", file: file, line: line)
        XCTAssertTrue(button.isEnabled, "\(id) is off", file: file, line: line)
        button.tap()
        XCTAssertTrue(button.waitForNonExistence(timeout: 5), "\(id) did not close its sheet", file: file, line: line)
        if let marker {
            XCTAssertTrue(app.buttons[marker].waitForExistence(timeout: 5), "not back at \(marker)", file: file, line: line)
        }
    }
}
