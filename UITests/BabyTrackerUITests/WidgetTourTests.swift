import XCTest

/// The Live Activity's buttons, which run outside the app and so can only be
/// driven from the Lock Screen and the Dynamic Island.
@MainActor
final class WidgetTourTests: XCTestCase {
    override func setUp() { continueAfterFailure = true }

    private func shoot(_ app: XCUIApplication, _ name: String) {
        let shot = XCTAttachment(screenshot: app.screenshot())
        shot.name = name
        shot.lifetime = .keepAlways
        add(shot)
    }

    private func settle(_ seconds: TimeInterval = 2) {
        _ = XCTWaiter.wait(for: [XCTestExpectation(description: "settle")], timeout: seconds)
    }

    /// Pulls Notification Centre down over the springboard. It renders the
    /// Lock Screen presentation of a Live Activity — and, unlike locking the
    /// device, its controls are reachable from a test.
    private func pullDownLockScreenPresentation(_ springboard: XCUIApplication) {
        springboard.coordinate(withNormalizedOffset: CGVector(dx: 0.2, dy: 0.0))
            .press(forDuration: 0.1, thenDragTo: springboard.coordinate(
                withNormalizedOffset: CGVector(dx: 0.2, dy: 0.6)
            ))
        settle(3)
    }

    /// Put it back, or the next test in the class starts behind a shade.
    private func pushUpLockScreenPresentation(_ springboard: XCUIApplication) {
        springboard.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.95))
            .press(forDuration: 0.1, thenDragTo: springboard.coordinate(
                withNormalizedOffset: CGVector(dx: 0.5, dy: 0.2)
            ))
        settle(2)
    }

    /// The feed break card, and its buttons doing their work without the app.
    ///
    /// A Left part of 9:02 ended 40 s ago (`-uiTestFeedBreak`). Right starts
    /// and stops within seconds — a tap under 30 s is discarded — so the
    /// card falls back to the open break: "Break", the parts so far, and L
    /// and R. L on the card then starts the next part, and Stop on the card
    /// stops it, with the app left in the background throughout.
    ///
    /// The simulator has no passcode, so this cannot show that a LOCKED
    /// phone runs these taps without asking to unlock — only that they run
    /// without opening the app, which is what an unlock prompt would precede.
    func testLiveActivityBreakButtonsRunWithoutOpeningTheApp() {
        var launch = Harness.Launch(reset: true, seedBaby: true, seedFixture: true)
        launch.feedBreak = true
        let app = Harness.launch(launch)
        XCTAssertTrue(app.buttons["home.start.right"].waitForExistence(timeout: 60), "Home did not appear")
        XCTAssertTrue(
            app.tap(app.buttons["home.start.right"], until: app.buttons["home.stop"]),
            "feed did not start"
        )
        settle()
        XCTAssertTrue(
            app.tap(app.buttons["home.stop"], until: app.buttons["home.start.left"]),
            "feed did not stop"
        )
        settle()

        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        XCUIDevice.shared.press(.home)
        settle(3)
        // The island is in springboard's accessibility tree and names the
        // card's state ("Break, 0:57", "Feeding · Left breast, 0:03"); the
        // Lock Screen presentation's own contents are not, so its buttons are
        // tapped by position and their effect read off the island.
        let island = { (text: String) in
            springboard.descendants(matching: .any)
                .matching(NSPredicate(format: "label BEGINSWITH %@", text)).firstMatch
        }
        XCTAssertTrue(island("Break").waitForExistence(timeout: 10), "Stop did not leave a break card")

        pullDownLockScreenPresentation(springboard)
        shoot(springboard, "28-live-activity-feed-break")

        // L, the first of the card's three buttons: L, R, Bottle.
        springboard.coordinate(withNormalizedOffset: CGVector(dx: 0.2, dy: 0.689)).tap()
        settle(4)
        shoot(springboard, "29-live-activity-running-from-card")
        XCTAssertNotEqual(app.state, .runningForeground, "L on the card opened the app")
        pushUpLockScreenPresentation(springboard)
        XCTAssertTrue(
            island("Feeding · Left breast").waitForExistence(timeout: 15),
            "L on the card did not start a left part"
        )

        // Stop, full width where L and R were.
        pullDownLockScreenPresentation(springboard)
        springboard.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.689)).tap()
        settle(4)
        shoot(springboard, "30-live-activity-break-after-card-stop")
        XCTAssertNotEqual(app.state, .runningForeground, "Stop on the card opened the app")
        pushUpLockScreenPresentation(springboard)
        XCTAssertTrue(
            island("Break").waitForExistence(timeout: 15),
            "Stop on the card did not return it to the break"
        )
    }

    /// The same, with the app not running when Stop is tapped: iOS launches
    /// it in the background to run the intent, possibly connecting its scene
    /// there. "Set amount" must still be waiting when the parent opens it.
    func testABottleStoppedOnTheCardWithTheAppNotRunningStillAsksForItsAmount() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedFixture: true))
        XCTAssertTrue(app.buttons["home.bottle"].waitForExistence(timeout: 60), "Home did not appear")
        XCTAssertTrue(app.tap(app.buttons["home.bottle"], until: app.buttons["bottle.start"]), "no Bottle sheet")
        XCTAssertTrue(app.tap(app.buttons["bottle.start"], until: app.buttons["home.stop"]), "bottle did not start")
        settle()
        app.terminate()
        settle(2)

        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        pullDownLockScreenPresentation(springboard)
        shoot(springboard, "34-live-activity-bottle-app-not-running")
        springboard.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.689)).tap()
        settle(6)
        shoot(springboard, "35-live-activity-after-cold-bottle-stop")
        XCTAssertNotEqual(app.state, .runningForeground, "Stop on the card opened the app")
        pushUpLockScreenPresentation(springboard)

        // `activate`, not `launch`: no launch arguments, so the store is not
        // reset — this is the parent opening the app from the Home Screen.
        app.activate()
        XCTAssertTrue(
            app.buttons["bottle.done"].waitForExistence(timeout: 20),
            "the app did not offer to set the stopped bottle's amount after a background launch"
        )
        shoot(app, "36-set-amount-after-cold-lock-screen-stop")
    }
}
