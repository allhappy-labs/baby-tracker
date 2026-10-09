import XCTest

/// Live Activity push, as far as a rendered app can show it.
///
/// Push registration is automatic: there is no toggle, no opt-in screen and no
/// setting to find. And a Lock Screen card action that could not verify the
/// family's current state opens the app to explain itself — once — instead of
/// writing.
@MainActor
final class LiveActivityPushUITests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    /// Anything that would be a push setting, by what it says or what it is.
    private func pushSettings(in app: XCUIApplication) -> XCUIElementQuery {
        app.descendants(matching: .any).matching(NSPredicate(
            format: "label CONTAINS[c] 'live activit' OR label CONTAINS[c] 'push' OR identifier CONTAINS[c] 'liveActivit' OR identifier CONTAINS[c] 'push'"
        ))
    }

    private func assertNoPushSetting(_ app: XCUIApplication, on screen: String, file: StaticString = #filePath, line: UInt = #line) {
        // The form is taller than the screen: look at every part of it.
        for _ in 0..<8 {
            let found = pushSettings(in: app).allElementsBoundByIndex.map { "\($0.elementType.rawValue):\($0.label)" }
            XCTAssertTrue(found.isEmpty, "\(screen) offers a Live Activity push setting: \(found)", file: file, line: line)
            app.swipeUp()
        }
    }

    /// A signed-in owner of a family — the state in which push registers —
    /// sees no new setting in Settings or on the family's own screen.
    func testSettingsOfferNoLiveActivityPushSetting() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, ownerFamily: true))
        defer { app.terminate() }
        app.waitingForHome()
        XCTAssertTrue(app.tap(app.buttons["Settings"], until: app.staticTexts["Units"]), "the gear on Home did not open Settings")
        assertNoPushSetting(app, on: "Settings")

        let row = app.caregiversRow()
        XCTAssertTrue(row.exists, "Settings has no Caregivers row")
        XCTAssertTrue(app.tap(row, until: app.navigationBars["Caregivers"]), "the Caregivers row did not open its screen")
        assertNoPushSetting(app, on: "Caregivers")
    }

    /// The handed-off action: the app says what to do, writes nothing, and
    /// does not say it again on the next launch — the reason was consumed,
    /// and the action itself was never stored to replay.
    func testAHandedOffCardActionIsExplainedOnceAndNeverReplayed() {
        let app = XCUIApplication()
        app.launchArguments = Harness.Launch(reset: true, seedBaby: true).arguments + ["-uiTestParkedActivityHandoff"]
        app.launch()
        let message = "Open Baby Tracker to refresh this activity before changing it."
        let alert = app.alerts.firstMatch
        XCTAssertTrue(alert.waitForExistence(timeout: 20), "no handoff explanation after a handed-off card action")
        XCTAssertTrue(alert.staticTexts[message].exists || alert.label.contains(message), "handoff alert says: \(alert.label)")
        alert.buttons["OK"].tap()
        XCTAssertTrue(alert.waitForNonExistence(timeout: 5))
        app.waitingForHome()
        // Nothing ran: no feed or sleep was started by the parked reason.
        XCTAssertEqual(app.hero.label, "Awake", "a handed-off action changed the family's state")
        app.terminate()

        let again = Harness.launch(Harness.Launch(reset: false, seedBaby: true))
        defer { again.terminate() }
        again.waitingForHome()
        XCTAssertFalse(again.alerts.firstMatch.waitForExistence(timeout: 3), "the handoff explanation was shown twice")
    }
}
