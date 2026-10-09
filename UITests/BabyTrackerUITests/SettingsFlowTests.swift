import XCTest

/// Regressions for defects the simulator read-through found after tasks 12-14
/// had green unit tests. Each was invisible to the package suite, because each
/// was in a VIEW.
@MainActor
final class SettingsFlowTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    private func settle() {
        _ = XCTWaiter.wait(for: [XCTestExpectation(description: "settle")], timeout: 1.5)
    }

    /// Any element whose label carries a volume in the given unit.
    private func hasVolume(_ app: XCUIApplication, in unit: String) -> Bool {
        let predicate = NSPredicate(format: "label CONTAINS %@", " \(unit)")
        return app.descendants(matching: .any).matching(predicate).firstMatch
            .waitForExistence(timeout: 8)
    }

    /// The unit Home's Bottle sheet offers its amounts in — Home itself
    /// shows no volume since the redesign. Opens the sheet, reads the first
    /// amount's spoken label ("30 ml", "1 oz"), and closes it again.
    private func bottleUnit(_ app: XCUIApplication) -> String? {
        let chip = app.buttons["bottle.chip.0"]
        guard app.tap(app.buttons["home.bottle"], until: chip) else { return nil }
        let unit = chip.label.split(separator: " ").last.map(String.init)
        app.buttons["Cancel"].tap()
        _ = chip.waitForNonExistence(timeout: 5)
        return unit
    }

    private func openSettings(_ app: XCUIApplication) {
        XCTAssertTrue(
            app.tap(app.buttons["Settings"], until: app.staticTexts["Units"]),
            "the gear on Home did not open Settings"
        )
    }

    /// The defect that made task 12 entirely inert: the preference persisted
    /// and not one screen changed, because each view model froze the unit it
    /// was constructed with. The package-level test pins the view model; this
    /// pins the three VIEWS, which is where the defect actually was.
    func testChangingTheUnitReachesHomeHistoryAndInsights() {
        let app = Harness.launch(
            Harness.Launch(reset: true, seedBaby: true, seedFixture: true)
        )
        defer { app.terminate() }
        app.waitingForHome()
        XCTAssertEqual(bottleUnit(app), "ml", "Home's Bottle sheet does not start in millilitres")

        openSettings(app)
        XCTAssertTrue(app.buttons["Ounces"].waitForExistence(timeout: 5))
        app.buttons["Ounces"].tap()
        settle()
        XCTAssertTrue(app.goToTab("Home"))
        settle()

        XCTAssertEqual(bottleUnit(app), "oz", "Home's Bottle sheet still offers millilitres")

        XCTAssertTrue(app.goToTab("History"))
        XCTAssertTrue(hasVolume(app, in: "oz"), "History still shows millilitres")

        XCTAssertTrue(app.goToTab("Insights"))
        XCTAssertTrue(hasVolume(app, in: "oz"), "Insights still shows millilitres")
    }

    func testSwitchingBabiesRebindsRunningHomeAndHistory() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedFixture: true))
        defer { app.terminate() }
        XCTAssertTrue(app.buttons["Switch baby"].waitForExistence(timeout: 10))
        if !app.buttons["home.wake"].exists {
            XCTAssertTrue(app.tap(app.buttons["home.sleep"], until: app.buttons["home.wake"]))
        }

        XCTAssertTrue(app.tap(app.buttons["Switch baby"], until: app.buttons["Add baby"]))
        app.buttons["Add baby"].tap()
        let name = app.textFields["Name"]
        XCTAssertTrue(name.waitForExistence(timeout: 5))
        name.tap()
        name.typeText("Second Baby")
        app.buttons["addBaby.referenceSex"].tap()
        app.buttons["Girls"].tap()
        XCTAssertTrue(app.buttons["addBaby.save"].isEnabled)
        app.buttons["addBaby.save"].tap()

        XCTAssertTrue(app.babyShown("Second Baby"))
        XCTAssertTrue(app.buttons["home.sleep"].waitForExistence(timeout: 5), "new baby's Home retained the first baby's running timer")
        XCTAssertFalse(app.buttons["home.wake"].exists)
        XCTAssertTrue(app.goToTab("History"))
        XCTAssertTrue(app.staticTexts["Nothing logged yet"].waitForExistence(timeout: 5), "new baby's History retained the first baby's records")

        XCTAssertTrue(app.goToTab("Home"))
        XCTAssertTrue(app.tap(app.buttons["Switch baby"], until: app.buttons["Test Baby"]))
        app.buttons["Test Baby"].tap()
        XCTAssertTrue(app.babyShown("Test Baby"))
        XCTAssertTrue(app.buttons["home.wake"].waitForExistence(timeout: 5), "returning to the first baby lost its running timer")
        XCTAssertTrue(app.goToTab("History"))
        XCTAssertFalse(app.staticTexts["Nothing logged yet"].exists)
    }
}

extension XCUIApplication {
    /// Whether Home is showing `name`. The name is the baby switcher's
    /// spoken value — its label is the action, "Switch baby".
    func babyShown(_ name: String, timeout: TimeInterval = 5) -> Bool {
        let switcher = buttons["home.babySwitcher"]
        let deadline = Date().addingTimeInterval(timeout)
        repeat {
            if switcher.exists, (switcher.value as? String) == name { return true }
            _ = XCTWaiter.wait(for: [XCTestExpectation(description: "poll")], timeout: 0.3)
        } while Date() < deadline
        return false
    }
}
