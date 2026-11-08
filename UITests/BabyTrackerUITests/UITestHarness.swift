import XCTest

/// Shared launch, navigation and dumping plumbing for the UI suite.
@MainActor
enum Harness {
    /// The three launch arguments the app understands. `.fixture` implies a
    /// baby, because seeding entries needs one to hang them off.
    struct Launch {
        var reset = true
        var seedBaby = true
        var seedFixture = false
        var homeCompositeFeed = false
        var homeMixedFeed = false
        var homeLinkedMixedSession = false
        var runningSleep = false
        var runningFeed = false
        var runningBottle = false
        /// The baby's Home order, as comma-separated action names.
        var homeOrder: String?
        /// Opens a Quick Log widget tile's link for the seeded baby: an
        /// action name, such as "solids".
        var logLink: String?
        /// A Left part of 9:02 that ended 40 s ago: the feed is in its break.
        var feedBreak = false
        /// A stopped Left part and a running Right part joined to it.
        var joinedFeed = false
        /// A Left part of 5:00 that ended six minutes ago, with no fixture.
        var feedEndedSixMinutesAgo = false
        var seedMeds = false
        /// Four shots (two vaccines, two doses each) for a baby five months old.
        var seedVaccines = false
        /// Opens the vaccine panel on launch.
        var openVaccine = false
        /// The family's pump, running.
        var runningPump = false
        /// Two finished pumps earlier today: one with 90 ml, one without an amount.
        var seedPumps = false
        /// Opens the pump panel on launch.
        var openPump = false
        /// History's fixture, at fixed times on the two days before today:
        /// a feed of L, R and a bottle, a sleep with one wake, a diaper, a
        /// med, a growth check-in, solids and a feed across midnight — see
        /// `UITestSupport.seedHistorySessions` in the app.
        var historySessions = false
        var readOnlyFamily = false
        /// A signed-in owner in a family with members and one invitation
        /// minted on another phone — see `UITestFamily` in the app.
        var ownerFamily = false
        /// The same caregiver signed in with no family, whose server previews
        /// and redeems any invitation.
        var noFamily = false
        var homeSyncNeedsSignIn = false
        var showErrorBanner = false
        var askFakeModel = false
        var askUnavailableModel = false
        var contentSizeCategory: String?
        var language: String?

        var arguments: [String] {
            var result: [String] = []
            // The content size pair goes FIRST, and that is load-bearing.
            //
            // UIKit reads this one through the NSArgumentDomain, which parses
            // the argument list as `-key value` pairs. The app's own flags are
            // bare — it reads them with `arguments.contains` — so to the
            // argument-domain parser `-uiTestSeedFixture -UIPreferredContent…`
            // looks like the key `uiTestSeedFixture` with the CATEGORY KEY as
            // its value, and the category name itself is left dangling and
            // never read. Every accessibility-size test was quietly running at
            // the default size; the screenshots they produced were real
            // screenshots of the wrong thing.
            if let category = contentSizeCategory {
                result += ["-UIPreferredContentSizeCategoryName", category]
            }
            if let language { result += ["-AppleLanguages", "(\(language))"] }
            if let homeOrder { result += ["-uiTestHomeOrder", homeOrder] }
            if let logLink { result += ["-uiTestLogLink", logLink] }
            if reset { result.append("-uiTestResetStore") }
            if seedBaby { result.append("-uiTestSeedBaby") }
            if seedFixture { result.append("-uiTestSeedFixture") }
            if homeCompositeFeed { result.append("-uiTestHomeCompositeFeed") }
            if homeMixedFeed { result.append("-uiTestHomeMixedFeed") }
            if homeLinkedMixedSession { result.append("-uiTestHomeLinkedMixedSession") }
            if runningSleep { result.append("-uiTestRunningSleep") }
            if runningFeed { result.append("-uiTestRunningFeed") }
            if runningBottle { result.append("-uiTestRunningBottle") }
            if feedBreak { result.append("-uiTestFeedBreak") }
            if joinedFeed { result.append("-uiTestJoinedFeed") }
            if feedEndedSixMinutesAgo { result.append("-uiTestFeedEndedSixMinutesAgo") }
            if seedMeds { result.append("-uiTestSeedMeds") }
            if seedVaccines { result.append("-uiTestSeedVaccines") }
            if openVaccine { result.append("-uiTestOpenVaccine") }
            if runningPump { result.append("-uiTestRunningPump") }
            if seedPumps { result.append("-uiTestSeedPumps") }
            if openPump { result.append("-uiTestOpenPump") }
            if historySessions { result.append("-uiTestHistorySessions") }
            if readOnlyFamily { result.append("-uiTestReadOnlyFamily") }
            if ownerFamily { result.append("-uiTestOwnerFamily") }
            if noFamily { result.append("-uiTestNoFamily") }
            if homeSyncNeedsSignIn { result.append("-uiTestHomeSyncNeedsSignIn") }
            if showErrorBanner { result.append("-uiTestShowErrorBanner") }
            if askFakeModel { result.append("-uiTestAskFakeModel") }
            if askUnavailableModel { result.append("-uiTestAskUnavailableModel") }
            return result
        }
    }

    static let accessibilityXXXL = "UICTContentSizeCategoryAccessibilityXXXL"

    @discardableResult
    static func launch(_ launch: Launch = Launch()) -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments = launch.arguments
        app.launch()
        return app
    }
}

extension XCUIApplication {
    /// On Home in every state — idle, running, in a break and read-only —
    /// so it says which screen is up without saying what the dock offers.
    var homeMarker: XCUIElement { buttons["home.babySwitcher"] }

    /// The one large number: a `Button` while a part runs and the caregiver
    /// may edit it, a plain element otherwise. Its label names the state —
    /// "Left breast", "Asleep", "Break", "Awake" — and its value carries the
    /// time, which is what ticks.
    var hero: XCUIElement { descendants(matching: .any)["home.hero"].firstMatch }

    /// A dock control, scrolled into reach if it has to be. At the
    /// accessibility sizes the dock scrolls with Home's content rather than
    /// floating over it, so a user reaches it the same way — by scrolling.
    @discardableResult
    func revealOnHome(_ element: XCUIElement, attempts: Int = 8) -> XCUIElement {
        guard element.waitForExistence(timeout: 15) else { return element }
        let scroll = scrollViews["home.scrollView"]
        for _ in 0..<attempts {
            if element.isHittable { return element }
            guard scroll.exists else { break }
            scroll.swipeUp()
        }
        return element
    }

    /// Tab bar items are buttons, but the glass tab bar in iOS 26 does not
    /// always expose a `tabBars` container, so fall back to a plain button.
    func tab(_ name: String) -> XCUIElement {
        let inBar = tabBars.buttons[name]
        return inBar.exists ? inBar : buttons[name]
    }

    /// Switches tab and waits for that tab's navigation title to appear, so a
    /// swallowed tap fails here rather than silently auditing the wrong screen.
    @discardableResult
    func goToTab(
        _ name: String,
        file: StaticString = #filePath,
        line: UInt = #line
    ) -> Bool {
        let marker: XCUIElement = switch name {
        case "History": buttons["history.filter"]
        case "Insights": descendants(matching: .any)["insights.range"].firstMatch
        case "Ask": buttons["ask.newChat"]
        case "Settings": staticTexts["Units"]
        default: homeMarker
        }
        guard tap(tab(name), until: marker, attempts: 8) else {
            XCTFail("Could not switch to the \(name) tab", file: file, line: line)
            return false
        }
        return true
    }

    /// Finds the Caregivers row in Settings, scrolling if it is below the fold.
    ///
    /// Settings has three sections and Sharing is the last of them, so at large
    /// accessibility text sizes the row is several screens down. Two suites
    /// need it and both got this wrong independently — the first by not
    /// scrolling at all — so the scrolling lives here rather than in either
    /// of them.
    func caregiversRow(scrollingUpTo attempts: Int = 12) -> XCUIElement {
        var row = buttons["settings.caregivers"]
        for _ in 0..<attempts {
            row = buttons["settings.caregivers"].exists
                ? buttons["settings.caregivers"]
                : cells["settings.caregivers"]
            if row.exists, row.isHittable { return row }
            swipeUp()
        }
        return row
    }

    /// Taps `element` until `target` appears, up to `attempts` times.
    ///
    /// Not a softened assertion — a workaround for a real, reported app defect:
    /// the first tap on `QuickLogDock` after launch very often logs
    /// "Computed hit point {-1, -1} after scrolling to visible" and does
    /// nothing. `HomeView` republishes every second from a `.common`-mode
    /// `Timer`, so the app never reaches the quiescent state XCUITest waits for
    /// before resolving a hit point, and the glass dock's frame is not yet
    /// settled. The retry makes the flow tests measure the flow rather than the
    /// race; the underlying flakiness is written up in the task report.
    @discardableResult
    func tap(_ element: XCUIElement, until target: XCUIElement, attempts: Int = 5) -> Bool {
        guard element.waitForExistence(timeout: 15) else { return false }
        for attempt in 0..<attempts {
            if target.exists { return true }

            // Never tap twice into a sheet: once the dock is covered, a second
            // tap lands on the dimming backdrop and closes what the first tap
            // opened. If the source is gone or unhittable, just keep waiting.
            guard element.exists, element.isHittable else {
                if target.waitForExistence(timeout: 6) { return true }
                continue
            }

            if attempt < 2 {
                element.tap()
            } else {
                // `tap()` asks the element for a hit point and gives up when it
                // gets {-1, -1}. A normalised coordinate never has to ask.
                element.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
            }
            if target.waitForExistence(timeout: 6) { return true }
        }
        return target.exists
    }

    /// Every accessibility element on screen, as `type | label | value`,
    /// indented by depth.
    ///
    /// Read from a single `snapshot()` rather than by querying each element in
    /// turn: `HomeView` republishes every second, so a query-per-element walk
    /// races the next tick and dies with "Failed to get matching snapshot".
    func visibleStrings() -> [String] {
        guard let root = try? snapshot() else { return ["<snapshot unavailable>"] }
        var lines: [String] = []
        Self.walk(root, depth: 0, into: &lines)
        return lines
    }

    private static func walk(_ node: XCUIElementSnapshot, depth: Int, into lines: inout [String]) {
        let label = node.label
        let value = (node.value as? String) ?? ""
        let placeholder = node.placeholderValue ?? ""
        if !label.isEmpty || !value.isEmpty || !placeholder.isEmpty {
            var line = String(repeating: "  ", count: depth)
            line += "\(node.elementType.name)  |  label=\(quoted(label))"
            if !value.isEmpty { line += "  |  value=\(quoted(value))" }
            if !placeholder.isEmpty { line += "  |  placeholder=\(quoted(placeholder))" }
            lines.append(line)
        }
        for child in node.children {
            walk(child, depth: depth + 1, into: &lines)
        }
    }

    private static func quoted(_ text: String) -> String { "\u{201C}\(text)\u{201D}" }
}

/// History's list, read the way a VoiceOver user hears it.
///
/// A row is one element whose label is "Tracker, parts, at start–end":
/// "Feed, L 12m, R 15m, 60 ml, at 10:23 AM–11:05 AM", "Diaper, Wet, at
/// 12:00 PM", "Sleep, 2h 15m · 1 wake, at 1:00 PM–3:30 PM". For a caregiver
/// who may edit, the row is a button carrying `history.row.<n>`; SwiftUI
/// copies the identifier onto the text inside it too, so the queries below
/// ask for buttons. A viewer's rows are plain elements (ruling H-R6).
extension XCUIApplication {
    /// Rows whose label matches `format` (an NSPredicate format over the
    /// label), of whichever element type the role gets.
    private func historyRows(_ predicate: NSPredicate, editable: Bool) -> XCUIElementQuery {
        let row = NSPredicate(format: "identifier BEGINSWITH 'history.row.' AND NOT (identifier ENDSWITH '.running')")
        let both = NSCompoundPredicate(andPredicateWithSubpredicates: [row, predicate])
        return editable ? buttons.matching(both) : descendants(matching: .any).matching(both)
    }

    /// Rows whose label starts with `prefix`: "Diaper, Wet", "Meds, Iron".
    /// Every row with an empty prefix.
    func historyRows(startingWith prefix: String, editable: Bool = true) -> XCUIElementQuery {
        historyRows(prefix.isEmpty ? NSPredicate(value: true) : NSPredicate(format: "label BEGINSWITH %@", prefix),
                    editable: editable)
    }

    /// Feed rows of more than one part: two or more chips before ", at ".
    var feedSessionRows: XCUIElementQuery {
        historyRows(NSPredicate(format: "label MATCHES %@", "^Feed, ([^,]+, ){2,}at .*"), editable: true)
    }

    /// Sleep rows of more than one part: they count the wakes.
    var sleepSessionRows: XCUIElementQuery {
        historyRows(NSPredicate(format: "label BEGINSWITH 'Sleep, ' AND label CONTAINS ' wake'"), editable: true)
    }

    /// A session sheet's part row, from 0.
    func sessionPart(_ index: Int) -> XCUIElement {
        descendants(matching: .any)["sessionSheet.part.\(index)"].firstMatch
    }

    /// Swipes a History row to its Delete action and taps it.
    func swipeToDelete(_ row: XCUIElement) {
        row.swipeLeft()
        let delete = buttons["history.delete"].firstMatch
        if delete.waitForExistence(timeout: 3) { delete.tap() }
    }
}

extension XCTestCase {
    /// Writes a dump both to the simulator's `/tmp/bt-copy/` and to the result
    /// bundle as an attachment, because the simulator filesystem is discarded
    /// less predictably than the `.xcresult`.
    func record(_ name: String, _ body: String) {
        let directory = URL(fileURLWithPath: "/tmp/bt-copy", isDirectory: true)
        try? FileManager.default.createDirectory(
            at: directory, withIntermediateDirectories: true
        )
        let url = directory.appendingPathComponent("\(name).txt")
        try? body.write(to: url, atomically: true, encoding: .utf8)

        let attachment = XCTAttachment(string: body)
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
    }

    func dump(_ app: XCUIApplication, screen: String) -> String {
        var body = "===== SCREEN: \(screen)\n\n--- accessibility walk\n"
        body += app.visibleStrings().joined(separator: "\n")
        body += "\n\n--- debugDescription\n"
        body += app.debugDescription
        record("screen-\(screen)", body)
        return body
    }
}

extension XCUIElement.ElementType {
    var name: String {
        switch self {
        case .button: "button"
        case .staticText: "text"
        case .cell: "cell"
        case .other: "other"
        case .navigationBar: "navBar"
        case .tabBar: "tabBar"
        case .image: "image"
        case .switch: "switch"
        case .textField: "textField"
        case .table: "table"
        case .collectionView: "collection"
        case .scrollView: "scroll"
        case .datePicker: "datePicker"
        case .stepper: "stepper"
        case .segmentedControl: "segmented"
        case .window: "window"
        case .application: "app"
        default: "type\(rawValue)"
        }
    }
}
