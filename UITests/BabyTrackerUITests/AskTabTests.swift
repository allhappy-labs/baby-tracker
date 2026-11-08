import XCTest

@MainActor final class AskTabTests: XCTestCase {
    func testNaturalQuestionRendersAsReadableConversation() {
        let app = Harness.launch(.init(seedFixture: true, askFakeModel: true))
        defer { app.terminate() }
        XCTAssertTrue(app.goToTab("Ask"))
        let input = app.descendants(matching: .any)["ask.question"]
        XCTAssertTrue(input.waitForExistence(timeout: 5))
        XCTAssertTrue(app.descendants(matching: .any)["ask.babyName"].exists || app.buttons["ask.babySwitcher"].exists, "Ask carries the baby header like every tab")
        input.tap()
        input.typeText("Is he eating better?")
        app.buttons["ask.send"].tap()
        XCTAssertTrue(app.staticTexts["ask.message.user.0"].waitForExistence(timeout: 10))
        let answer = app.staticTexts["ask.answer.0"]
        XCTAssertTrue(answer.waitForExistence(timeout: 10))
        XCTAssertFalse(answer.label.contains("Please ask about a specific activity"))
        XCTAssertFalse(answer.label.contains("Feed count:"))
        XCTAssertTrue(app.buttons["ask.source.0"].label.contains("·"))
        XCTAssertTrue(app.keyboards.firstMatch.waitForNonExistence(timeout: 5),
                      "Sending should dismiss the keyboard so the reply remains visible")
        XCTAssertTrue(answer.isHittable, "The newest reply should be visible without scrolling")
        let screenshot = XCTAttachment(screenshot: app.screenshot())
        screenshot.lifetime = .keepAlways
        add(screenshot)
    }

    func testUnavailableModelHidesAskWithoutDeletingChat() {
        let available = Harness.launch(.init(seedFixture: true, askFakeModel: true))
        XCTAssertTrue(available.goToTab("Ask"))
        available.buttons["ask.suggestion.handoff"].tap()
        XCTAssertTrue(available.staticTexts["ask.answer.0"].waitForExistence(timeout: 10))
        available.terminate()

        let app = Harness.launch(.init(reset: false, seedFixture: true, askUnavailableModel: true))
        XCTAssertTrue(app.tab("Home").waitForExistence(timeout: 10))
        XCTAssertFalse(app.tab("Ask").exists)
        XCTAssertFalse(app.descendants(matching: .any)["ask.question"].exists)
        XCTAssertTrue(app.goToTab("Insights"))
        XCTAssertTrue(app.goToTab("Settings"))
        app.terminate()

        let restored = Harness.launch(.init(reset: false, seedFixture: true, askFakeModel: true))
        defer { restored.terminate() }
        XCTAssertTrue(restored.goToTab("Ask"))
        XCTAssertTrue(restored.staticTexts["ask.answer.0"].exists)
    }
}
