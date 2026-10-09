import XCTest

@MainActor
final class HomeSyncRecoveryUITests: XCTestCase {
    func testHomeShowsSignInAndRetainedQueue() {
        // End a pre-existing simulator process before applying the isolated
        // fixture launch arguments; never interact until its banner is ready.
        XCUIApplication().terminate()
        let app = Harness.launch(.init(reset: false, seedBaby: false, homeSyncNeedsSignIn: true, language: "en"))
        defer { app.terminate() }
        let banner = app.otherElements["home.syncStatus"]
        guard banner.waitForExistence(timeout: 10) else {
            XCTFail("Isolated Home recovery fixture did not become ready")
            return
        }
        XCTAssertTrue(app.staticTexts["home.syncTitle"].exists)
        XCTAssertTrue(app.staticTexts["home.syncQueued"].exists)
        let action = app.buttons["home.syncSignIn"]
        XCTAssertTrue(action.isHittable)
        assertIdle(app)
        XCTAssertEqual(action.label, "Sign in with Apple")
        app.tab("History").tap()
        app.tab("Home").tap()
        XCTAssertTrue(action.waitForExistence(timeout: 5))
        action.tap()
        // Once signed in, routine sync stays off Home: the card goes away.
        expectation(for: NSPredicate(format: "exists == false"), evaluatedWith: banner)
        waitForExpectations(timeout: 5)
        XCTAssertTrue(app.buttons["home.babySwitcher"].exists)
        assertIdle(app)
    }

    private func assertIdle(_ app: XCUIApplication, file: StaticString = #filePath, line: UInt = #line) {
        XCTAssertTrue(app.buttons["home.start.left"].exists, "Recovery gestures must keep the idle care controls", file: file, line: line)
        XCTAssertFalse(app.buttons["home.stop"].exists, "Recovery must not start a care timer", file: file, line: line)
        XCTAssertFalse(app.buttons["home.wake"].exists, "Recovery must not start a care timer", file: file, line: line)
    }
}
