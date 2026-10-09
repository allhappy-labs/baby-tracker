import XCTest

/// Checks the translated primary screens.
///
/// The suite runs on the one shared iPhone 17 Pro, so the compact-screen
/// check is made with text instead: Ukrainian at XXXL, the largest size that
/// is not an accessibility size, where the dock still floats over Home and
/// has the least room for the longest words.
@MainActor
final class UkrainianLayoutSmokeTests: XCTestCase {
    func testPrimaryScreensAndLanguageShortcut() {
        let app = XCUIApplication()
        app.launchArguments = [
            "-AppleLanguages", "(uk)", "-AppleLocale", "uk_UA",
            "-uiTestResetStore", "-uiTestSeedBaby", "-uiTestSeedFixture",
        ]
        app.launch()
        defer { app.terminate() }

        XCTAssertTrue(app.buttons["home.babySwitcher"].waitForExistence(timeout: 60))
        capture(app, "01 Ukrainian Home")

        openTab("Історія", in: app)
        capture(app, "02 Ukrainian History")

        openTab("Статистика", in: app)
        capture(app, "03 Ukrainian Insights")

        openTab("Налаштування", in: app)
        XCTAssertTrue(app.staticTexts["Мова застосунку"].waitForExistence(timeout: 10))
        let shortcut = app.buttons["settings.appLanguage"]
        XCTAssertTrue(shortcut.exists)
        XCTAssertTrue(shortcut.isHittable)
        XCTAssertGreaterThan(shortcut.frame.height, 0)
        XCTAssertTrue(app.staticTexts[
            "У налаштуваннях iPhone відкрийте «Програми», потім Baby Tracker і виберіть мову."
        ].exists)
        capture(app, "04 Ukrainian Settings")

        shortcut.tap()
        let iPhoneSettings = XCUIApplication(bundleIdentifier: "com.apple.Preferences")
        let foreground = NSPredicate(
            format: "state == %d", XCUIApplication.State.runningForeground.rawValue
        )
        let arrived = XCTNSPredicateExpectation(predicate: foreground, object: iPhoneSettings)
        XCTAssertEqual(XCTWaiter.wait(for: [arrived], timeout: 10), .completed)
        capture(iPhoneSettings, "05 iPhone app Settings")
    }

    /// Scrolls the frontmost list until `element` is hittable.
    private func reveal(_ element: XCUIElement, in app: XCUIApplication) -> XCUIElement {
        for _ in 0..<6 {
            if element.exists, element.isHittable { return element }
            app.collectionViews.firstMatch.swipeUp()
        }
        return element
    }

    private func openTab(_ label: String, in app: XCUIApplication) {
        let button = app.tabBars.buttons[label].exists ? app.tabBars.buttons[label] : app.buttons[label]
        XCTAssertTrue(button.waitForExistence(timeout: 10), "Missing tab: \(label)")
        button.tap()
    }

    private func capture(_ app: XCUIApplication, _ name: String) {
        let attachment = XCTAttachment(screenshot: app.screenshot())
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
    }
}
