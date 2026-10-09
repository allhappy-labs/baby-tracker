import XCTest

@MainActor
final class GrowthFlowUITests: XCTestCase {
    func testNewBabyRequiresReferenceAndOffersOptionalDueDate() {
        let app = Harness.launch(Harness.Launch(seedBaby: false))
        defer { app.terminate() }
        let save = app.buttons["addBaby.save"]
        XCTAssertTrue(save.waitForExistence(timeout: 10))
        XCTAssertFalse(save.isEnabled)
        app.textFields["Name"].tap()
        app.textFields["Name"].typeText("Mira")
        XCTAssertFalse(save.isEnabled)
        let reference = app.buttons["addBaby.referenceSex"]
        XCTAssertTrue(reference.label.contains("Swiss PEZZ 2025 reference"))
        reference.tap()
        app.buttons["Girls"].tap()
        XCTAssertTrue(save.isEnabled)
        XCTAssertTrue(app.switches["addBaby.hasDueDate"].exists)
        // SwiftUI exposes both the full row and its native switch as switches.
        // Tap the native child: tapping the row's text does not toggle it.
        app.switches["addBaby.hasDueDate"].switches.firstMatch.tap()
        XCTAssertEqual(app.switches["addBaby.hasDueDate"].value as? String, "1")
        XCTAssertTrue(app.descendants(matching: .any)["addBaby.dueDate"].firstMatch.waitForExistence(timeout: 5))
        save.tap()
        XCTAssertTrue(app.buttons["home.start.left"].waitForExistence(timeout: 10))
    }
    func testHomeMeasureSavesPartialCheckIn() {
        let app = Harness.launch()
        defer { app.terminate() }
        let more = app.buttons["home.more"]
        XCTAssertTrue(more.waitForExistence(timeout: 10))
        XCTAssertEqual(more.label, "More")
        XCTAssertTrue(more.isHittable)
        XCTAssertGreaterThanOrEqual(more.frame.height, 44)
        let weight = app.textFields["measurement.weight"]
        XCTAssertTrue(app.openGrowthCheckIn())
        let save = app.buttons["measurement.save"]
        XCTAssertFalse(save.isEnabled)
        weight.tap()
        weight.typeText("4.2")
        // Scroll the form to Save while the keyboard is visible.
        app.swipeUp()
        XCTAssertTrue(save.isEnabled)
        save.tap()
        XCTAssertTrue(more.waitForExistence(timeout: 10))
        XCTAssertFalse(weight.exists)
    }

    /// The Growth section replaced the details screen: its chips switch the
    /// chart, and All check-ins lands in History filtered to Growth. On the
    /// way it switches the range (the spec's range-switching test), and
    /// after it proves the Growth focus is spent: a second baby's History
    /// opens unfiltered.
    func testInsightsGrowthSectionOpensCheckInsInHistory() {
        let app = Harness.launch(Harness.Launch(historySessions: true))
        defer { app.terminate() }
        app.waitingForHome()
        XCTAssertTrue(app.goToTab("Insights"))
        // The spec holds every control on the screen to 44 pt, the pinned
        // range picker included.
        let range = app.descendants(matching: .any)["insights.range"].firstMatch
        XCTAssertGreaterThanOrEqual(range.frame.height, 44, "The range picker is under 44 pt")

        // The fixture's feeds are on the two days before today and nothing
        // came before them: Week averages over 2 days, All counts since the
        // first of them.
        let feeding = app.descendants(matching: .any)["insights.section.feeding"].firstMatch
        let feedingHeadline = feeding.descendants(matching: .any)
            .matching(NSPredicate(format: "label BEGINSWITH %@", "Feeds:")).firstMatch
        XCTAssertTrue(feedingHeadline.waitForExistence(timeout: 8), "Feeding has no headline")
        XCTAssertTrue(feedingHeadline.label.hasSuffix("Average of 2 days."), feedingHeadline.label)
        range.buttons["All"].tap()
        XCTAssertTrue(range.buttons["All"].isSelected, "All did not take")
        let since = feeding.descendants(matching: .any)
            .matching(NSPredicate(format: "label BEGINSWITH %@ AND label CONTAINS %@", "Feeds:", "Since ")).firstMatch
        XCTAssertTrue(since.waitForExistence(timeout: 5), "Feeding did not switch to All: \(feedingHeadline.label)")

        let all = app.buttons["insights.growth.allCheckIns"]
        for _ in 0..<8 where !all.isHittable { app.swipeUp() }
        XCTAssertTrue(all.isHittable, "All check-ins never scrolled into view")
        let length = app.buttons["insights.growth.chip.lengthHeight"]
        XCTAssertTrue(length.exists)
        length.tap()
        XCTAssertTrue(length.isSelected)
        all.tap()
        let clear = app.buttons["history.filter.clear"]
        XCTAssertTrue(clear.waitForExistence(timeout: 8))

        // Clear the filter, then add a second baby from History's switcher:
        // the tabs remount for it, and a focus left standing would filter
        // its History to Growth again.
        clear.tap()
        XCTAssertTrue(clear.waitForNonExistence(timeout: 5), "The Growth filter did not clear")
        XCTAssertTrue(app.tap(app.buttons["history.babySwitcher"], until: app.buttons["Add baby"]))
        app.buttons["Add baby"].tap()
        let name = app.textFields["Name"]
        XCTAssertTrue(name.waitForExistence(timeout: 5))
        name.tap()
        name.typeText("Second Baby")
        app.buttons["addBaby.referenceSex"].tap()
        app.buttons["Girls"].tap()
        app.buttons["addBaby.save"].tap()
        XCTAssertTrue(app.goToTab("History"))
        let secondBaby = app.buttons.matching(NSPredicate(
            format: "identifier == %@ AND value == %@", "history.babySwitcher", "Second Baby")).firstMatch
        XCTAssertTrue(secondBaby.waitForExistence(timeout: 8), "History did not move to the second baby")
        XCTAssertTrue(app.descendants(matching: .any)["history.empty"].firstMatch.waitForExistence(timeout: 5))
        XCTAssertFalse(clear.exists, "The Growth focus came back for the second baby")
    }
}

extension XCUIApplication {
    /// Growth lives behind the dock's ··· now: More, then Growth.
    func openGrowthCheckIn() -> Bool {
        tap(revealOnHome(buttons["home.more"]), until: buttons["more.growth"])
            && tap(buttons["more.growth"], until: textFields["measurement.weight"])
    }
}
