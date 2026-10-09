import XCTest

/// Joining a family from outside the paste field: the in-app scanner, which
/// the simulator can only ever show as its camera-less fallback, and a
/// `babytracker://` link opened by the system.
///
/// Launched with `-uiTestNoFamily` (signed in, named, no family; the server
/// previews every invitation as "The Kovals") or `-uiTestOwnerFamily`.
extension CaregiversScreenUITests {
    private static let invitationLink = URL(
        string: "babytracker://invite?server=https%3A%2F%2Fsync.example.test&token=Abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"
    )!
    private static let joinerOwnerID = "0A000000-0000-4000-8000-000000000001"

    /// A link opens the Join sheet at "Join {family}?", named by the server's
    /// preview, and only Join redeems it.
    func testInvitationLinkOpensTheJoinConfirmation() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, noFamily: true))
        defer { app.terminate() }
        app.waitingForHome()

        openInvitationLink(in: app)
        let title = app.staticTexts["joinConfirmation.title"]
        XCTAssertTrue(title.waitForExistence(timeout: 15), "the link did not reach Join {family}?")
        XCTAssertEqual(title.label, "Join The Kovals?")
        joiningScreenshot(app, "join-confirmation")

        app.buttons["joinConfirmation.join"].tap()
        XCTAssertTrue(title.waitForNonExistence(timeout: 10), "Join did not finish")
        // A finished join with babies closes the Join sheet onto the tabs;
        // the Caregivers screen then shows the family that was joined.
        XCTAssertTrue(app.buttons["joinFamily.close"].waitForNonExistence(timeout: 10), "the Join sheet stayed up")
        openCaregiversScreen(app)
        XCTAssertTrue(
            app.descendants(matching: .any)["family.member.\(Self.joinerOwnerID)"].waitForExistence(timeout: 10),
            "after joining, the family's members are not shown"
        )
    }

    // MARK: Helpers

    private func openCaregiversScreen(_ app: XCUIApplication) {
        app.waitingForHome()
        XCTAssertTrue(app.tap(app.buttons["Settings"], until: app.staticTexts["Units"]), "Settings did not open")
        let row = app.caregiversRow()
        XCTAssertTrue(row.exists, "Settings has no Caregivers row")
        row.tap()
        XCTAssertTrue(app.navigationBars["Caregivers"].waitForExistence(timeout: 5), "the Caregivers screen did not open")
    }

    /// Opens the link the way the system does, then accepts SpringBoard's
    /// "Open in BabyTracker?" prompt, which the simulator shows for a custom
    /// scheme before handing it to the app.
    private func openInvitationLink(_ link: URL = CaregiversScreenUITests.invitationLink, in app: XCUIApplication) {
        XCUIDevice.shared.system.open(link)
        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let open = springboard.buttons["Open"]
        if open.waitForExistence(timeout: 5) { open.tap() }
        XCTAssertTrue(app.wait(for: .runningForeground, timeout: 10), "the link did not bring the app forward")
    }

    private func joiningScreenshot(_ app: XCUIApplication, _ name: String) {
        let shot = XCTAttachment(screenshot: app.screenshot())
        shot.name = name
        shot.lifetime = .keepAlways
        add(shot)
    }
}
