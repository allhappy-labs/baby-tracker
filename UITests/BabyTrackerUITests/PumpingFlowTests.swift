import XCTest

/// Pumping from Home to History. Every assertion is about what is there and
/// what a tap does, never where it is drawn.
@MainActor
final class PumpingFlowTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    private func strip(_ app: XCUIApplication) -> XCUIElement {
        app.descendants(matching: .any)["home.pumpStrip"].firstMatch
    }

    /// More → Pump → Both → Start beside a running bottle; the bottle keeps
    /// its Stop; the strip's Stop asks How much?; Save; History lists it.
    func testAPumpBesideARunningBottleFromStartToHistory() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedFixture: true, runningBottle: true))
        defer { app.terminate() }
        XCTAssertTrue(app.buttons["home.stop"].waitForExistence(timeout: 60), "the running bottle did not show")
        XCTAssertTrue(app.tap(app.revealOnHome(app.buttons["home.more"]), until: app.buttons["more.pump"]), "no Pump tile in More")
        XCTAssertTrue(app.tap(app.buttons["more.pump"], until: app.buttons["pump.startNow"]), "the pump panel did not open")
        app.segmentedControls["pump.side"].buttons.element(boundBy: 2).tap()
        XCTAssertTrue(app.tap(app.buttons["pump.startNow"], until: strip(app)), "the pump strip did not appear")
        XCTAssertTrue(app.buttons["home.stop"].exists, "starting a pump took the bottle's Stop away")
        XCTAssertTrue(app.tap(app.buttons["home.pumpStop"], until: app.buttons["pumpAmount.save"]), "Stop did not ask How much?")
        app.buttons["pumpAmount.save"].tap()
        XCTAssertTrue(app.buttons["pumpAmount.save"].waitForNonExistence(timeout: 8), "How much? did not close")
        XCTAssertTrue(strip(app).waitForNonExistence(timeout: 8), "the strip outlived the session")
        XCTAssertTrue(app.buttons["home.stop"].exists, "stopping the pump stopped the bottle")
        XCTAssertTrue(app.goToTab("History"))
        XCTAssertTrue(app.historyRows(startingWith: "Pump, ").firstMatch.waitForExistence(timeout: 15), "no pump row in History")
    }

    /// Skip leaves the amount empty: the row reads a dash.
    func testSkipLeavesTheAmountEmpty() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedFixture: true, runningPump: true))
        defer { app.terminate() }
        XCTAssertTrue(strip(app).waitForExistence(timeout: 60), "the running pump has no strip")
        XCTAssertTrue(app.tap(app.buttons["home.pumpStop"], until: app.buttons["pumpAmount.skip"]), "Stop did not ask How much?")
        app.buttons["pumpAmount.skip"].tap()
        XCTAssertTrue(app.buttons["pumpAmount.skip"].waitForNonExistence(timeout: 8))
        XCTAssertTrue(app.goToTab("History"))
        let row = app.historyRows(startingWith: "Pump, ").firstMatch
        XCTAssertTrue(row.waitForExistence(timeout: 15), "no pump row in History")
        XCTAssertTrue(row.label.contains("—"), "a skipped amount reads: \(row.label)")
    }

    /// While a pump runs, the Pump action offers only Edit running, which
    /// opens the running session with End reading Running; History lists it.
    func testPumpOffersOnlyEditRunningWhileOneRuns() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedFixture: true, runningPump: true))
        defer { app.terminate() }
        XCTAssertTrue(strip(app).waitForExistence(timeout: 60), "the running pump has no strip")
        XCTAssertTrue(app.tap(app.revealOnHome(app.buttons["home.more"]), until: app.buttons["more.pump"]))
        XCTAssertTrue(app.tap(app.buttons["more.pump"], until: app.buttons["pump.editRunning"]), "the panel offers no Edit running")
        XCTAssertFalse(app.buttons["pump.startNow"].exists, "a second Start is offered while a pump runs")
        XCTAssertTrue(app.tap(app.buttons["pump.editRunning"], until: app.buttons["pump.save"]), "Edit running did not open the running session")
        // The End row is one labelled element: found by its identifier, read as a whole.
        let end = app.descendants(matching: .any)["pump.end"].firstMatch
        XCTAssertTrue(end.waitForExistence(timeout: 8), "the running session has no End row")
        let spoken = end.label + " " + (end.value as? String ?? "")
        XCTAssertTrue(spoken.contains("Running") || app.staticTexts["Running"].exists,
                      "the running session's End reads: \(spoken)")
        XCTAssertFalse(app.buttons["pump.chip.0"].exists, "a running session offers an amount")
    }

    /// The strip's own tap opens the running session too.
    func testTheStripOpensTheRunningSession() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedFixture: true, runningPump: true))
        defer { app.terminate() }
        XCTAssertTrue(app.buttons["home.pumpOpen"].waitForExistence(timeout: 60), "the running pump has no strip")
        XCTAssertTrue(app.tap(app.buttons["home.pumpOpen"], until: app.buttons["pump.save"]), "the strip did not open the pump panel")
        XCTAssertTrue(app.buttons["pump.delete"].exists, "the running session cannot be deleted from its panel")
    }

    /// Pump is one of the Home buttons: in More by default, and first in the
    /// order it is Home's big button, which opens the pump panel.
    func testPumpIsAHomeButtonThatCanLeadHome() {
        var app = Harness.launch(Harness.Launch(reset: true, seedBaby: true))
        XCTAssertTrue(app.homeMarker.waitForExistence(timeout: 60))
        XCTAssertFalse(app.buttons["home.pump"].exists, "Pump is on the dock by default")
        app.terminate()
        app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, homeOrder: "pump,breast,bottle,sleep"))
        defer { app.terminate() }
        let big = app.buttons["home.pump"]
        XCTAssertTrue(big.waitForExistence(timeout: 60), "Pump first in the order is not on Home's dock")
        XCTAssertTrue(app.tap(big, until: app.buttons["pump.startNow"]), "the Pump button did not open the pump panel")
    }

    /// A Quick Log widget tile for Pump opens the pump panel.
    func testAPumpLinkOpensItsPanel() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, logLink: "pump"))
        defer { app.terminate() }
        XCTAssertTrue(app.buttons["pump.startNow"].waitForExistence(timeout: 60), "the widget's Pump link did not open the pump panel")
    }
}
