import XCTest

/// History → tap a row → correct it in the sheet it was logged with, in edit
/// mode → Save → the row says the new thing.
///
/// Rows are matched by their combined label, "Diaper, Wet, at 13:20": the
/// `", at "` infix belongs to a History row and to nothing else on screen,
/// not the filter sheet's toggles nor Home behind the tab.
@MainActor
final class EditFlowTests: XCTestCase {
    private var app: XCUIApplication!

    override func setUp() {
        continueAfterFailure = false
    }

    override func tearDown() {
        app?.terminate()
        app = nil
    }

    /// `.dry` is deliberately absent from the two-tap fast path; the Diaper
    /// sheet offers it in edit mode (ruling H-R3), so this is the one route
    /// to it.
    func testEditingADiaperToDryUpdatesTheHistoryRow() {
        app = Harness.launch(Harness.Launch(reset: true, seedBaby: true))
        XCTAssertTrue(app.tap(app.buttons["home.diaper"], until: app.buttons["diaper.wet"]))
        XCTAssertFalse(app.buttons["diaper.dry"].exists, "the fast path offers Dry")
        app.buttons["diaper.wet"].tap()

        XCTAssertTrue(app.goToTab("History"))
        XCTAssertTrue(app.waitForRow(startingWith: "Diaper, Wet", timeout: 5), "History shows no Diaper row to edit")

        let row = app.historyRow(startingWith: "Diaper")
        XCTAssertTrue(app.tap(row, until: app.buttons["diaper.save"]), "Tapping a History row did not open the Diaper sheet in edit mode")

        let dry = app.buttons["diaper.dry"]
        XCTAssertTrue(dry.waitForExistence(timeout: 5), "The edit sheet does not offer Dry")
        dry.tap()
        app.buttons["diaper.save"].tap()

        XCTAssertTrue(app.waitForRow(startingWith: "Diaper, Dry", timeout: 5),
                      "The diaper row still does not read Dry after saving the edit")
        XCTAssertFalse(app.waitForRow(startingWith: "Diaper, Wet", timeout: 1),
                       "The old Wet row survived the edit — something created rather than updated")
    }
}

extension XCUIApplication {
    /// The row element itself, for tapping. Restricted by the same `", at "`
    /// infix `waitForRow` uses, so it can never resolve to anything but a row.
    func historyRow(startingWith prefix: String) -> XCUIElement {
        let predicate = NSPredicate(
            format: "label BEGINSWITH %@ AND label CONTAINS %@", prefix, ", at "
        )
        let asButton = buttons.matching(predicate).firstMatch
        if asButton.exists { return asButton }
        let asCell = cells.matching(predicate).firstMatch
        if asCell.exists { return asCell }
        return staticTexts.matching(predicate).firstMatch
    }
}
