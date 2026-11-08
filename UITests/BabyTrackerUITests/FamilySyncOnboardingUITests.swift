import XCTest

/// The Caregivers screens, rendered.
///
/// Everything here runs against a launch with no account and no server, which
/// is the state the overwhelming majority of installs are in and the one most
/// likely to be broken without anybody noticing. A view-model test cannot tell
/// you that the row is reachable from the gear, that server choice precedes
/// sign-in,
/// hit region, or that the local-only reassurance is actually on screen —
/// every one of those has been a real defect in this project before.
@MainActor
final class FamilySyncOnboardingUITests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    private func openCaregivers(_ app: XCUIApplication) {
        XCTAssertTrue(
            app.tap(app.buttons["Settings"], until: app.staticTexts["Units"]),
            "the gear on Home did not open Settings"
        )
        let row = app.caregiversRow()
        XCTAssertTrue(row.exists, "Settings has no Caregivers row")
        XCTAssertTrue(row.isHittable, "the Caregivers row is not hittable")
        row.tap()
        XCTAssertTrue(
            app.navigationBars["Caregivers"].waitForExistence(timeout: 5),
            "tapping Caregivers did not open the Caregivers screen"
        )
    }

    private func launch() -> XCUIApplication {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true))
        app.waitingForHome()
        return app
    }

    /// A local-only launch must choose and inspect a server before Apple proof
    /// can be requested. The next screen exposes the pinned private server but
    /// does not confirm it automatically.
    func testLocalOnlyChoosesAndChecksServerBeforeSignIn() {
        let app = launch()
        defer { app.terminate() }
        openCaregivers(app)

        XCTAssertFalse(app.buttons["family.signIn"].exists, "sign-in is offered before server confirmation")
        let choose = app.buttons["family.chooseServer"]
        XCTAssertTrue(choose.waitForExistence(timeout: 5), "no server choice control")
        XCTAssertTrue(choose.isHittable, "the server choice control has no hit region")
        choose.tap()
        XCTAssertTrue(app.navigationBars["Server"].waitForExistence(timeout: 5), "server selection did not open")
        XCTAssertTrue(app.buttons["server.useHosted"].waitForExistence(timeout: 5), "private server check is missing")
        XCTAssertFalse(app.buttons["server.confirm"].exists, "private server was confirmed without a probe")

        let shot = XCTAttachment(screenshot: app.screenshot())
        shot.name = "caregivers-local-only"
        shot.lifetime = .keepAlways
        add(shot)
    }
}

/// A caregiver joining someone else's family installs the app and meets the
/// add-baby screen first. They must be able to join from there without
/// inventing a baby of their own.
@MainActor
final class JoinFromFirstLaunchUITests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    func testFirstLaunchOffersJoiningAFamily() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: false))
        defer { app.terminate() }

        XCTAssertTrue(app.buttons["addBaby.save"].waitForExistence(timeout: 20), "first launch is not the add-baby screen")
        let join = app.buttons["addBaby.joinFamily"]
        XCTAssertTrue(join.waitForExistence(timeout: 5), "no way to join a family from the first-launch screen")
        XCTAssertTrue(join.isHittable, "the join action has no hit region")
        join.tap()

        XCTAssertTrue(
            app.navigationBars["Caregivers"].waitForExistence(timeout: 5),
            "joining did not open the Caregivers flow"
        )
        XCTAssertTrue(
            app.buttons["family.chooseServer"].waitForExistence(timeout: 5),
            "the join flow does not start with choosing the server"
        )

        app.buttons["joinFamily.close"].tap()
        XCTAssertTrue(
            app.buttons["addBaby.save"].waitForExistence(timeout: 5),
            "closing the join flow did not return to adding a baby"
        )
    }
}
