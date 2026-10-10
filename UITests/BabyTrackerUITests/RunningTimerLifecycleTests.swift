import XCTest

/// One timer per part: a running part, its break, the part that joins it,
/// and a feed and a sleep running at once. The hero's label names the state
/// and its value carries the time, so "still counting" means the value moves.
@MainActor
final class RunningTimerLifecycleTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    private func screenshot(_ app: XCUIApplication, _ name: String) {
        let shot = XCTAttachment(screenshot: app.screenshot())
        shot.name = name
        shot.lifetime = .keepAlways
        add(shot)
    }

    private func seeded(
        runningFeed: Bool = false, runningSleep: Bool = false, runningBottle: Bool = false,
        feedBreak: Bool = false, joinedFeed: Bool = false
    ) -> Harness.Launch {
        Harness.Launch(
            seedFixture: true, runningSleep: runningSleep, runningFeed: runningFeed,
            runningBottle: runningBottle, feedBreak: feedBreak, joinedFeed: joinedFeed
        )
    }

    /// The count froze on the phones after any write while a feed ran — a
    /// diaper, or a sync applying the other caregiver's entry. It must go on
    /// counting from its stored start.
    func testRunningFeedKeepsCountingAfterAWriteWhileItRuns() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedFixture: false))
        app.waitingForHome()
        XCTAssertTrue(app.tap(app.buttons["home.start.left"], until: app.buttons["home.stop"]))

        XCTAssertTrue(app.tap(app.buttons["home.diaper"], until: app.buttons["diaper.dirty"]))
        app.buttons["diaper.dirty"].tap()
        XCTAssertTrue(app.buttons["home.stop"].waitForExistence(timeout: 5))

        let first = heroValue(app)
        XCTAssertTrue(waitForValueChange(app, from: first, timeout: 5), "the hero stopped counting after a write: \(first)")
        let second = heroValue(app)
        XCTAssertTrue(waitForValueChange(app, from: second, timeout: 5), "the hero stopped counting after a write: \(second)")
    }

    /// Tapping the hero opens the editor; moving the start back and saving
    /// moves "since …" and adds the time to the count — and the count goes on
    /// afterwards, with the sheet having been over it.
    func testEditingTheStartMovesItBackAndTheHeroKeepsCounting() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedFixture: false))
        app.waitingForHome()
        XCTAssertTrue(app.tap(app.buttons["home.start.left"], until: app.buttons["home.stop"]))

        let before = heroValue(app)
        XCTAssertEqual(minutes(before), 0, before)

        let earlier = app.buttons["timerEdit.earlier"]
        XCTAssertTrue(app.tap(app.hero, until: earlier))
        earlier.tap()
        earlier.tap()
        screenshot(app, "running-timer-editor")

        // The running side is shown, with an End of "Running".
        XCTAssertTrue(app.descendants(matching: .any)["timerEdit.runningSide"].firstMatch.exists, "the running side is missing")

        let save = app.buttons["Save"]
        XCTAssertTrue(save.isEnabled)
        save.tap()
        XCTAssertTrue(earlier.waitForNonExistence(timeout: 5), "the editor did not close after saving")

        XCTAssertTrue(app.hero.waitForExistence(timeout: 5))
        let after = heroValue(app)
        XCTAssertNotEqual(since(after), since(before), "the start did not move: \(after)")
        XCTAssertTrue((10...11).contains(minutes(after) ?? -1), "the count did not grow by ten minutes: \(after)")

        XCTAssertTrue(waitForValueChange(app, from: after, timeout: 5), "the hero stopped counting after an edit: \(after)")
        let next = heroValue(app)
        XCTAssertTrue(waitForValueChange(app, from: next, timeout: 5), "the hero stopped counting after an edit: \(next)")
    }

    /// Stop, then the other side within the break: the new part joins the
    /// one before it, and the join chip says so until ✕ makes it separate.
    func testTheOtherSideInTheBreakJoinsAndCanBeMadeSeparate() {
        let app = Harness.launch(seeded(feedBreak: true))
        defer { app.terminate() }
        let right = app.buttons["home.start.right"]
        XCTAssertTrue(right.waitForExistence(timeout: 10), app.debugDescription)
        XCTAssertEqual(app.hero.label, "Break")
        XCTAssertEqual(right.value as? String, "Suggested", "after Left, Right is the side to offer")
        screenshot(app, "feed-break")

        XCTAssertTrue(app.tap(right, until: app.buttons["home.stop"]))
        XCTAssertEqual(app.hero.label, "Right breast")
        let chip = app.descendants(matching: .any)["home.joinChip"].firstMatch
        XCTAssertTrue(chip.waitForExistence(timeout: 5), "the part did not join the feed in its break")
        XCTAssertTrue(chip.label.hasPrefix("Same feed"), chip.label)
        XCTAssertTrue(chip.label.contains("Left breast"), chip.label)
        screenshot(app, "joined-feed")

        let separate = app.buttons["home.joinChip.separate"]
        XCTAssertEqual(separate.label, "Make it a separate feed")
        XCTAssertGreaterThanOrEqual(separate.frame.height, 44)
        separate.tap()
        XCTAssertTrue(chip.waitForNonExistence(timeout: 5), "✕ did not make the part a feed of its own")
        XCTAssertTrue(app.buttons["home.stop"].exists, "making it separate stopped the part")
    }

    /// On the phones the count ran only until the first pause of its tick —
    /// backgrounding, a sheet, a mode change — and after that moved only when
    /// a sync happened to redraw Home. One redraw on returning is not
    /// counting, so this demands three consecutive changes.
    func testRunningFeedKeepsCountingAfterReturningFromTheBackground() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedFixture: false))
        app.waitingForHome()
        XCTAssertTrue(app.tap(app.buttons["home.start.left"], until: app.buttons["home.stop"]))

        XCUIDevice.shared.press(.home)
        wait(2)
        app.activate()

        XCTAssertTrue(app.hero.waitForExistence(timeout: 5))
        // Let the one redraw on return happen first.
        wait(1.5)
        for _ in 0..<3 {
            let before = heroValue(app)
            XCTAssertTrue(
                waitForValueChange(app, from: before, timeout: 2.5),
                "the hero stopped counting after returning from the background: \(before)"
            )
        }
        // Stopped under 30 s, so the part is discarded and Home is idle again.
        XCTAssertTrue(app.tap(app.buttons["home.stop"], until: app.buttons["home.start.left"]))
    }

    // MARK: -

    private func heroValue(_ app: XCUIApplication) -> String {
        (app.hero.value as? String) ?? ""
    }

    /// "since 9:41 PM" out of the hero's value.
    private func since(_ value: String) -> String? {
        value.range(of: "since ").map { String(value[$0.lowerBound...]) }
    }

    /// The minutes of "10 minutes, 3 seconds, since 9:41 PM"; 0 when the
    /// count has not reached a minute yet.
    private func minutes(_ value: String) -> Int? {
        let duration = value.components(separatedBy: ", since").first ?? value
        guard let range = duration.range(of: #"(\d+) minutes?"#, options: .regularExpression) else {
            return duration.isEmpty ? nil : 0
        }
        return Int(duration[range].split(separator: " ")[0])
    }

    private func wait(_ seconds: TimeInterval) {
        _ = XCTWaiter.wait(for: [XCTestExpectation(description: "wait")], timeout: seconds)
    }

    private func waitForValueChange(_ app: XCUIApplication, from old: String, timeout: TimeInterval) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        repeat {
            if heroValue(app) != old { return true }
            wait(0.25)
        } while Date() < deadline
        return heroValue(app) != old
    }
}
