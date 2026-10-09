import XCTest

/// The Caregivers screen as a family's owner sees it: members, pending
/// invitations, the invite sheet with its QR code, and a member's page.
///
/// Launched with `-uiTestOwnerFamily`: the production view model, signed in
/// through the production coordinator, against an in-memory family server
/// holding Olena (you, the owner), Marta (caregiver), Grandma (view only) and
/// one view-only invitation minted on another phone. No network.
@MainActor
final class CaregiversScreenUITests: XCTestCase {
    // Mirrors `UITestFamily` in the app.
    private static let ownerID = "0A000000-0000-4000-8000-000000000001"
    private static let caregiverID = "0A000000-0000-4000-8000-000000000002"
    private static let viewerID = "0A000000-0000-4000-8000-000000000003"
    private static let otherPhoneInvitationID = "0A000000-0000-4000-8000-0000000000F1"

    /// See `testCaregiversScreenPassesAccessibilityAudit`.
    private static let listKnownIssues: Set<String> = [
        "Olena", "You · Owner", "View-only invitation", "Expires in 6 days", "Invite someone",
        "Each invitation works once and expires after 7 days. Revoking it disables the code.",
        "Disconnect this device",
    ]
    private static let detailKnownIssues: Set<String> = ["Make owner"]

    override func setUp() {
        continueAfterFailure = false
    }

    // MARK: Invitations

    /// Every invitation stays in view and each one is revoked on its own: the
    /// old screen kept only the latest, and could revoke only that one.
    func testOwnerCreatesTwoInvitationsAndRevokesTheFirst() {
        let app = launchOnCaregivers()
        defer { app.terminate() }

        let before = pendingIDs(app)
        XCTAssertEqual(before, [Self.otherPhoneInvitationID], "the fixture's invitation from another phone is not listed")

        let first = createInvitation(app, role: nil, before: before)
        let second = createInvitation(app, role: "View only", before: before.union([first]))
        XCTAssertEqual(pendingIDs(app), before.union([first, second]), "both new invitations are not listed")

        // Revoke the first from its own sheet.
        open(app.buttons["family.pending.\(first)"], in: app)
        let revoke = app.buttons["invite.revoke"]
        XCTAssertTrue(revoke.waitForExistence(timeout: 5), "the first invitation's sheet has no Revoke")
        revoke.tap()
        XCTAssertTrue(
            app.buttons["family.pending.\(first)"].waitForNonExistence(timeout: 5),
            "the revoked invitation is still listed"
        )
        XCTAssertTrue(app.buttons["family.pending.\(second)"].exists, "revoking the first also removed the second")

        // The second still opens its QR code: its code was kept on this phone.
        open(app.buttons["family.pending.\(second)"], in: app)
        XCTAssertTrue(app.otherElements["invite.qr"].waitForExistence(timeout: 5) || app.images["invite.qr"].exists,
                      "the second invitation no longer shows its QR code")
        XCTAssertTrue(app.staticTexts["invite.code"].exists, "the second invitation no longer shows its code")
        attachScreenshot(app, "caregivers-invite-result")
        app.buttons["invite.done"].tap()
        XCTAssertTrue(app.buttons["family.inviteSomeone"].waitForExistence(timeout: 5))
        attachScreenshot(app, "caregivers-pending-list")
    }

    // MARK: Members

    /// Removing a member returns to the Caregivers list — not past it — and
    /// the member is gone from it. The row that pushed the page disappears
    /// with the member, which is exactly when a navigation stack can pop
    /// twice.
    func testRemovingAMemberReturnsToTheListWithoutThem() {
        let app = launchOnCaregivers()
        defer { app.terminate() }

        open(app.buttons["family.member.\(Self.viewerID)"], in: app)
        XCTAssertTrue(app.buttons["member.remove"].waitForExistence(timeout: 5), "the member page did not open")
        app.buttons["member.remove"].tap()
        let confirm = app.buttons["confirmation.confirm"]
        XCTAssertTrue(confirm.waitForExistence(timeout: 5), "Remove did not ask first")
        XCTAssertTrue(app.staticTexts["confirmation.title"].label.contains("Grandma"), "the confirmation does not name her")
        confirm.tap()

        XCTAssertTrue(app.navigationBars["Caregivers"].waitForExistence(timeout: 5), "did not return to Caregivers")
        XCTAssertTrue(
            app.descendants(matching: .any)["family.member.\(Self.viewerID)"].waitForNonExistence(timeout: 5),
            "the removed member is still listed"
        )
        // Still on Caregivers, not popped past it to Settings.
        XCTAssertFalse(app.staticTexts["Units"].exists, "Remove popped past the Caregivers screen")
        XCTAssertTrue(app.navigationBars["Caregivers"].exists, "Remove popped past the Caregivers screen")
    }

    // MARK: Accessibility

    /// The owner's screens, audited the same way `FamilySyncOnboardingUITests`
    /// audits the local-only one: the list, a member page, and both steps of
    /// the invite sheet.
    ///
    /// Tolerated: `dynamicType` on the elements named in `listKnownIssues`
    /// and `detailKnownIssues`, and on the invite sheet's Done and Cancel bar buttons —
    /// each by exact label, like `AccessibilityAuditTests`' `KnownIssue`s. Any
    /// other finding, or the same finding on any other element, fails.
    ///
    /// What the finding is, as far as it could be established: the audit's
    /// Dynamic Type probe reports these elements, but every one of them does
    /// scale — screenshots at AX5
    /// show all of them growing and wrapping, unclipped. It is not the
    /// avatar's font (moving it to a text style changed nothing), not
    /// `.refreshable`, not one section's overflow (hiding Leaving left the
    /// others), and not a row's position in its section (Make owner stays
    /// flagged below Remove). It does follow layout: with the member page's
    /// content pushed off screen it vanished, and capping the avatar's growth
    /// removed the finding on the owner footer. That matches
    /// `AccessibilityAuditTests.testAuditSettings`' `sharingOverflow` — a
    /// Form that outgrows the screen as the probe enlarges it.
    ///
    /// Invite sheet Done and Cancel: a UIKit bar button caps its own scaling
    /// (`barButton` in `AccessibilityAuditTests`, which tolerates the same
    /// "Cancel" and "Save" buttons on other sheets).
    func testCaregiversScreenPassesAccessibilityAudit() throws {
        let app = launchOnCaregivers()
        defer { app.terminate() }
        attachScreenshot(app, "caregivers-members")

        try audit(app, "caregivers-owner-list", knownDynamicType: Self.listKnownIssues)

        open(app.buttons["family.member.\(Self.caregiverID)"], in: app)
        XCTAssertTrue(app.buttons["member.remove"].waitForExistence(timeout: 5))
        try audit(app, "caregivers-member-detail", knownDynamicType: Self.detailKnownIssues)
        goBack(app)

        open(app.buttons["family.inviteSomeone"], in: app)
        XCTAssertTrue(app.buttons["invite.create"].waitForExistence(timeout: 5))
        try audit(app, "caregivers-invite-role")
        app.buttons["invite.create"].tap()
        XCTAssertTrue(app.buttons["invite.revoke"].waitForExistence(timeout: 5))
        try audit(app, "caregivers-invite-result")
    }

    // MARK: Plumbing

    private func launchOnCaregivers(contentSize: String? = nil) -> XCUIApplication {
        let app = Harness.launch(
            Harness.Launch(reset: true, seedBaby: true, ownerFamily: true, contentSizeCategory: contentSize)
        )
        app.waitingForHome()
        XCTAssertTrue(
            app.tap(app.buttons["Settings"], until: app.staticTexts["Units"]),
            "the gear on Home did not open Settings"
        )
        let row = app.caregiversRow()
        XCTAssertTrue(row.exists, "Settings has no Caregivers row")
        row.tap()
        XCTAssertTrue(app.navigationBars["Caregivers"].waitForExistence(timeout: 5), "the Caregivers screen did not open")
        // The fixture signs in as the app launches; the members arrive with it.
        XCTAssertTrue(
            app.descendants(matching: .any)["family.member.\(Self.ownerID)"].waitForExistence(timeout: 10),
            "the signed-in owner's members never appeared"
        )
        return app
    }

    private func pendingIDs(_ app: XCUIApplication) -> Set<String> {
        let rows = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH 'family.pending.'"))
        return Set(rows.allElementsBoundByIndex.map(\.identifier)
            .filter { $0 != "family.pending.retry" }
            .map { String($0.dropFirst("family.pending.".count)) })
    }

    /// Creates an invitation through the sheet and returns its id, found as
    /// the one row that was not listed before.
    private func createInvitation(_ app: XCUIApplication, role: String?, before: Set<String>) -> String {
        open(app.buttons["family.inviteSomeone"], in: app)
        let create = app.buttons["invite.create"]
        XCTAssertTrue(create.waitForExistence(timeout: 5), "the invite sheet did not open")
        if let role { roleSegment(app, role).tap() }
        create.tap()
        XCTAssertTrue(app.buttons["invite.revoke"].waitForExistence(timeout: 5), "no result step after Create")
        XCTAssertTrue(app.staticTexts["invite.code"].exists, "the result step shows no code")
        app.buttons["invite.done"].tap()
        XCTAssertTrue(app.buttons["family.inviteSomeone"].waitForExistence(timeout: 5), "the invite sheet did not close")

        let rows = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH 'family.pending.'"))
        wait(for: [expectation(for: NSPredicate(format: "count == %d", before.count + 1), evaluatedWith: rows)], timeout: 5)
        let added = pendingIDs(app).subtracting(before)
        XCTAssertEqual(added.count, 1, "the new invitation is not listed")
        return added.first ?? ""
    }

    /// A segment of whichever segmented role picker is on screen.
    private func roleSegment(_ app: XCUIApplication, _ title: String) -> XCUIElement {
        let segmented = app.segmentedControls.firstMatch.buttons[title]
        return segmented.exists ? segmented : app.buttons[title]
    }

    private func open(_ element: XCUIElement, in app: XCUIApplication) {
        XCTAssertTrue(reveal(element, in: app), "\(element) is not reachable")
        element.tap()
    }

    /// Scrolls until `element` can be tapped. Form rows below the fold exist
    /// but are not hittable, and the largest text sizes push most of them there.
    private func reveal(_ element: XCUIElement, in app: XCUIApplication, attempts: Int = 10) -> Bool {
        guard element.waitForExistence(timeout: 5) || scrollUntilExists(element, in: app) else { return false }
        for _ in 0..<attempts {
            if element.isHittable { return true }
            app.swipeUp()
        }
        return element.isHittable
    }

    private func scrollUntilExists(_ element: XCUIElement, in app: XCUIApplication) -> Bool {
        for _ in 0..<8 {
            app.swipeUp()
            if element.exists { return true }
        }
        return false
    }

    private func goBack(_ app: XCUIApplication) {
        app.navigationBars.buttons.element(boundBy: 0).tap()
        XCTAssertTrue(app.navigationBars["Caregivers"].waitForExistence(timeout: 5), "did not return to Caregivers")
    }

    /// See `testCaregiversScreenPassesAccessibilityAudit` for why each
    /// tolerated finding is tolerated. Everything is recorded, tolerated or
    /// not, in the attachment.
    private func audit(_ app: XCUIApplication, _ name: String, knownDynamicType: Set<String> = []) throws {
        var issues: [String] = []
        try app.performAccessibilityAudit(for: .all) { issue in
            let element = issue.element
            issues.append("\(issue.compactDescription) | id=\(element?.identifier ?? "-") label=\(element?.label ?? "-")")
            // Contrast is excluded for the reason recorded in
            // AccessibilityAuditTests: the glass and gradient surfaces defeat
            // the sampler, and PaletteTests measures the real ratios.
            if issue.auditType == .contrast { return true }
            guard issue.auditType == .dynamicType, let element else { return false }
            // barButton: the invite sheet's two toolbar buttons, each by id and label.
            if (element.identifier, element.label) == ("invite.done", "Done") { return true }
            if (element.identifier, element.label) == ("invite.cancel", "Cancel") { return true }
            return knownDynamicType.contains(element.label)
        }
        let attachment = XCTAttachment(string: issues.joined(separator: "\n"))
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
    }

    private func attachScreenshot(_ app: XCUIApplication, _ name: String) {
        let shot = XCTAttachment(screenshot: app.screenshot())
        shot.name = name
        shot.lifetime = .keepAlways
        add(shot)
    }
}
