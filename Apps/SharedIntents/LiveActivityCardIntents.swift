import AppIntents
import BabyCore
import BabyFeature

// MARK: - Lock Screen controls of a card with a realm
//
// The buttons of a Live Activity that belongs to a family realm are intents
// of their own. They carry the card's target (`LiveActivityActionTarget`) and
// go through `LiveActivityActions`, which checks it against the family's
// current state before anything is written. Keeping them apart from the
// Home Screen widget's and Siri's intents (`LogIntents.swift`) keeps those
// exactly as they were: their own `supportedModes`, and no "Activity"
// parameter showing up in Shortcuts.
//
// Like every Lock Screen control, each is a `LiveActivityIntent` with no
// `openAppWhenRun`, so a tap runs on a locked iPhone in the app's process
// without bringing the app forward. `authenticationPolicy` is spelled out so
// a later change to it is a visible decision.
//
// `supportedModes` is background first: the tap runs as before. The
// `.foreground(.dynamic)` is for one case only — a target that could not be
// checked against the family's current state. Nothing is written then, and
// the intent asks iOS to continue in the app (`continueInAppIfHandedOff`).
//
// Not discoverable: these mean nothing outside the card that built them.

/// The card's Stop, for a breast part or a timed bottle. A running bottle is
/// saved at the previous bottle's amount — the tap must work on a locked phone —
/// and the app offers "Set amount" for it next time it is on screen.
struct StopFeedCardIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Stop the feed"
    static let description = IntentDescription("Ends the running feed and saves it.")
    static let authenticationPolicy: IntentAuthenticationPolicy = .alwaysAllowed
    static let supportedModes: IntentModes = [.background, .foreground(.dynamic)]
    static let isDiscoverable = false

    /// Whose feed the card shows. A `String` because App Intent parameters
    /// cannot be `UUID`.
    @Parameter(title: "Baby")
    var babyID: String?

    /// The card's own target.
    @Parameter(title: "Activity")
    var activityTarget: String?

    init() {}

    init(babyID: UUID, activityTarget: String) {
        self.babyID = babyID.uuidString
        self.activityTarget = activityTarget
    }

    @MainActor
    func perform() async throws -> some IntentResult {
        await continueInAppIfHandedOff(try await LiveActivityActions.run(.stopFeed, babyID: babyID, target: activityTarget))
        return .result()
    }
}

/// L or R on the card's feed break: the next breast part, joining the
/// session the break rules say it joins.
struct StartBreastCardIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Start breast feeding"
    static let description = IntentDescription("Starts a breast-feeding timer on the selected side.")
    static let authenticationPolicy: IntentAuthenticationPolicy = .alwaysAllowed
    static let supportedModes: IntentModes = [.background, .foreground(.dynamic)]
    static let isDiscoverable = false

    @Parameter(title: "Baby")
    var babyID: String?

    /// `"left"` or `"right"` — `BreastSide`'s raw value.
    @Parameter(title: "Breast side")
    var side: String?

    @Parameter(title: "Activity")
    var activityTarget: String?

    init() {}

    init(babyID: UUID, side: BreastSide, activityTarget: String) {
        self.babyID = babyID.uuidString
        self.side = side.rawValue
        self.activityTarget = activityTarget
    }

    @MainActor
    func perform() async throws -> some IntentResult {
        guard let side = side.flatMap(BreastSide.init(rawValue:)) else { return .result() }
        await continueInAppIfHandedOff(try await LiveActivityActions.run(.startBreast(side), babyID: babyID, target: activityTarget))
        return .result()
    }
}

/// Bottle on the card's feed break: a timed bottle with the last contents,
/// its amount asked for at Stop.
struct StartBottleCardIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Start a bottle"
    static let description = IntentDescription("Starts a timed bottle feed.")
    static let authenticationPolicy: IntentAuthenticationPolicy = .alwaysAllowed
    static let supportedModes: IntentModes = [.background, .foreground(.dynamic)]
    static let isDiscoverable = false

    @Parameter(title: "Baby")
    var babyID: String?

    @Parameter(title: "Activity")
    var activityTarget: String?

    init() {}

    init(babyID: UUID, activityTarget: String) {
        self.babyID = babyID.uuidString
        self.activityTarget = activityTarget
    }

    @MainActor
    func perform() async throws -> some IntentResult {
        await continueInAppIfHandedOff(try await LiveActivityActions.run(.startBottle, babyID: babyID, target: activityTarget))
        return .result()
    }
}

/// The running sleep card's Wake up. It never starts a nap.
struct WakeUpCardIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Wake up"
    static let description = IntentDescription("Ends and saves the running sleep.")
    static let authenticationPolicy: IntentAuthenticationPolicy = .alwaysAllowed
    static let supportedModes: IntentModes = [.background, .foreground(.dynamic)]
    static let isDiscoverable = false

    @Parameter(title: "Baby")
    var babyID: String?

    @Parameter(title: "Activity")
    var activityTarget: String?

    init() {}

    init(babyID: UUID, activityTarget: String) {
        self.babyID = babyID.uuidString
        self.activityTarget = activityTarget
    }

    @MainActor
    func perform() async throws -> some IntentResult {
        await continueInAppIfHandedOff(try await LiveActivityActions.run(.wake, babyID: babyID, target: activityTarget))
        return .result()
    }
}
