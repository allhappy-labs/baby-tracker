import XCTest

/// A read-only caregiver's Home screen, rendered.
///
/// The store refuses the write either way — that is pinned by
/// `WritePermissionTests`. What only a rendered test can show is that the
/// viewer is not left staring at controls that silently do nothing, and that
/// the app still tells them why.
@MainActor
final class ReadOnlyCaregiverUITests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    private func launchReadOnly(_ contentSize: String? = nil) -> XCUIApplication {
        let app = Harness.launch(
            Harness.Launch(
                reset: true, seedBaby: false, readOnlyFamily: true,
                contentSizeCategory: contentSize
            )
        )
        // NOT `waitingForHome()`: the switcher it waits for would pass here
        // without saying anything about the dock, which is precisely what a
        // read-only caregiver does not get. Waiting on the
        // read-only notice instead is the correct marker for this state — and
        // if it never arrives, that is the defect this suite exists to catch.
        //
        // Matched by label rather than identifier: the notice is what a user
        // reads, and SwiftUI does not reliably surface an identifier set on a
        // bare Text inside an overlay.
        XCTAssertTrue(
            readOnlyNotice(app).waitForExistence(timeout: 20),
            "the read-only Home never appeared after launch"
        )
        return app
    }

    /// The one sentence a viewer is shown, found the way a user finds it.
    private func readOnlyNotice(_ app: XCUIApplication) -> XCUIElement {
        app.staticTexts.containing(
            NSPredicate(format: "label CONTAINS 'view-only access'")
        ).firstMatch
    }

    /// The dock is gone rather than disabled. A row of greyed-out buttons
    /// invites a tap and explains nothing.
    func testViewerHomeHasNoLoggingControls() {
        let app = launchReadOnly()
        defer { app.terminate() }

        XCTAssertFalse(
            app.buttons["home.sleep"].waitForExistence(timeout: 3),
            "a read-only caregiver is still offered the sleep control"
        )
        for id in ["home.start.left", "home.start.right", "home.bottle", "home.diaper", "home.more"] {
            XCTAssertFalse(app.buttons[id].exists, "a read-only caregiver is still offered \(id)")
        }
        // Nothing runs, and the hero is not an editor for a viewer.
        XCTAssertEqual(app.hero.label, "Awake")
        XCTAssertFalse(app.buttons["home.hero"].exists, "the hero is a control for a viewer")
    }
}
