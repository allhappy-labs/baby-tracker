import XCTest

/// A feed made of several parts, as a parent runs one now: one timer per
/// part, Stop between them, and every part inside the break limit joining
/// the session before it. Started from `-uiTestFeedBreak` — a Left part of
/// 9:02 that ended 40 s ago — so no test waits out a real side.
@MainActor
final class CompositeFeedFlowTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    private func launchInBreak() -> XCUIApplication {
        let app = Harness.launch(Harness.Launch(seedFixture: true, feedBreak: true))
        XCTAssertTrue(app.buttons["home.start.right"].waitForExistence(timeout: 60), app.debugDescription)
        XCTAssertEqual(app.hero.label, "Break")
        return app
    }

    /// The device report this suite was first written for: two bottles in a
    /// row after a breast part, and the second one lost. Both must be parts
    /// of the feed — on Home and in History.
    func testTwoBottlesInARowAreBothKept() {
        let app = launchInBreak()
        defer { app.terminate() }

        for chip in ["bottle.chip.1", "bottle.chip.2"] {
            XCTAssertTrue(app.tap(app.buttons["home.bottle"], until: app.buttons[chip]))
            app.buttons[chip].tap()
            app.buttons["bottle.log"].tap()
            XCTAssertTrue(app.buttons[chip].waitForNonExistence(timeout: 5), "\(chip) did not log and close")
        }

        let deadline = Date().addingTimeInterval(5)
        var value = ""
        repeat {
            value = (app.hero.value as? String) ?? ""
            if value.components(separatedBy: "Bottle").count - 1 >= 2 { break }
            pause(0.3)
        } while Date() < deadline
        XCTAssertEqual(value.components(separatedBy: "Bottle").count - 1, 2,
                       "the feed does not hold both bottles: \(value)")

        XCTAssertTrue(app.goToTab("History"))
        let session = app.feedSessionRows.firstMatch
        XCTAssertTrue(session.waitForExistence(timeout: 5), app.debugDescription)
        XCTAssertTrue(app.tap(session, until: app.buttons["sessionSheet.done"]))
        // The Left part and both bottles.
        for index in 0..<3 {
            XCTAssertTrue(app.sessionPart(index).waitForExistence(timeout: 5),
                          "part \(index) is missing:\n\(app.debugDescription)")
        }
        XCTAssertFalse(app.sessionPart(3).exists)
    }

    private func pause(_ seconds: TimeInterval) {
        _ = XCTWaiter.wait(for: [XCTestExpectation(description: "pause")], timeout: seconds)
    }
}
