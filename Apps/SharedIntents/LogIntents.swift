import AppIntents
import WidgetKit
import BabyCore
import BabyStore
import BabyUI
import BabyFeature

struct LogDiaperIntent: AppIntent {
    static let title: LocalizedStringResource = "Log a diaper"
    static let description = IntentDescription("Records a wet diaper for the chosen baby.")

    @Parameter(title: "Baby")
    var babyID: String?

    init() {}

    init(babyID: String) { self.babyID = babyID }

    @MainActor
    func perform() async throws -> some IntentResult {
        let store = try WidgetStore.make()
        guard let baby = try WidgetStore.target(babyID, in: store) else { return .result() }

        _ = try store.logDiaper(.wet, for: baby.id)
        WidgetCenter.shared.reloadAllTimelines()
        return .result()
    }
}

// MARK: - Lock Screen controls
//
// Every control on the Live Activity is a `LiveActivityIntent` with no
// `openAppWhenRun`. That pair is what lets a tap run on a locked iPhone:
// iOS runs a `LiveActivityIntent`'s `perform()` in the app's process in the
// background, without unlocking and without bringing the app forward. An
// intent that opens the app — as the old Finish bottle button did — cannot
// run until the phone is unlocked, which is the unlock prompt a caregiver
// reported. Nothing these intents touch is protected beyond "after first
// unlock": the store and the App Group defaults use the system's default
// data protection, and the only keychain item is
// `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`.
//
// `authenticationPolicy` is spelled out rather than left to the default so
// that a later change to it is a visible decision, not an accident.
//
// These are the controls of a card with no realm — local tracking, a
// self-hosted server, an earlier build — and of the Home Screen widget. A
// card that belongs to a family realm has intents of its own, carrying the
// card's target (`LiveActivityCardIntents.swift`); nothing here changes for it.

/// The Live Activity's Stop, for a breast part or a timed bottle. The app's
/// Stop dock ends up in the same store calls — this intent is an adapter, not
/// a second code path.
///
/// A running bottle needs an amount, and this tap cannot ask for one: it must
/// work on a locked phone. So the bottle is saved at the baby's usual amount
/// now, and the app offers "Set amount" for that feed the next time it is on
/// screen (`PendingQuickLogStore`, `.bottleAmount`).
struct StopFeedIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Stop the feed"
    static let description = IntentDescription("Ends the running feed and saves it.")
    static let authenticationPolicy: IntentAuthenticationPolicy = .alwaysAllowed

    /// Supplied by the Live Activity, which knows whose feed it is showing.
    /// Absent on any other path, where the selected baby is the right target.
    /// A `String` because App Intent parameters cannot be `UUID`.
    @Parameter(title: "Baby")
    var babyID: String?

    init() {}

    init(babyID: UUID) {
        self.babyID = babyID.uuidString
    }

    @MainActor
    func perform() async throws -> some IntentResult {
        let store = try WidgetStore.make()
        guard let baby = try WidgetStore.target(babyID, in: store) else {
            // The baby was deleted while the card sat on the Lock Screen.
            await LiveActivityController.endStaleActivity(babyID: babyID, kind: .feed)
            return .result()
        }
        guard let timer = try store.activeTimer(kind: .feed, for: baby.id) else {
            // Nothing to stop: the part already ended in the app or on
            // another caregiver's phone. Writing nothing is right; the card
            // shows what followed — the break, or nothing.
            await LiveActivityController.enterBreak(kind: .feed, babyID: baby.id, in: store)
            return .result()
        }

        try await OutOfAppFeedActions.stopFeed(timer, in: store)
        WidgetCenter.shared.reloadAllTimelines()
        return .result()
    }
}

/// L or R: on the feed break card, and on the Quick Log widget.
///
/// Starts that breast part through `BabyStore.startPart` — the same call as
/// the app's dock — so it joins the session the break rules say it joins. A
/// part already running is stopped at this instant first, which is the
/// store's safety net, not a Switch control — and when that part is a timed
/// bottle, the app is asked to offer "Set amount" for it, as after a Lock
/// Screen Stop. The feed's card, in its break or running, then shows the new
/// part.
struct StartBreastIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Start breast feeding"
    static let description = IntentDescription("Starts a breast-feeding timer on the selected side.")
    static let authenticationPolicy: IntentAuthenticationPolicy = .alwaysAllowed

    @Parameter(title: "Baby")
    var babyID: String?

    /// `"left"` or `"right"` — `BreastSide`'s raw value. A `String` so the
    /// card and the widget can build it without an `AppEnum`.
    @Parameter(title: "Breast side")
    var side: String?

    init() {}

    init(babyID: UUID, side: BreastSide) {
        self.babyID = babyID.uuidString
        self.side = side.rawValue
    }

    init(babyID: String, side: String) {
        self.babyID = babyID
        self.side = side
    }

    @MainActor
    func perform() async throws -> some IntentResult {
        guard let side = side.flatMap(BreastSide.init(rawValue:)) else { return .result() }
        let store = try WidgetStore.make()
        guard let baby = try WidgetStore.target(babyID, in: store) else {
            await LiveActivityController.endStaleActivity(babyID: babyID, kind: .feed)
            return .result()
        }

        try await OutOfAppFeedActions.startBreast(side, for: baby.id, in: store)
        WidgetCenter.shared.reloadAllTimelines()
        return .result()
    }
}

/// Bottle, on the feed break card.
///
/// Starts a timed bottle through the same store call as L and R, so it joins
/// the session in its break. Its contents are what was given last — the Bottle
/// sheet's default — and its amount is asked for at Stop, as for any bottle
/// stopped from the Lock Screen.
struct StartBottleIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Start a bottle"
    static let description = IntentDescription("Starts a timed bottle feed.")
    static let authenticationPolicy: IntentAuthenticationPolicy = .alwaysAllowed

    @Parameter(title: "Baby")
    var babyID: String?

    init() {}

    init(babyID: UUID) {
        self.babyID = babyID.uuidString
    }

    @MainActor
    func perform() async throws -> some IntentResult {
        let store = try WidgetStore.make()
        guard let baby = try WidgetStore.target(babyID, in: store) else {
            await LiveActivityController.endStaleActivity(babyID: babyID, kind: .feed)
            return .result()
        }

        try await OutOfAppFeedActions.startBottle(for: baby.id, in: store)
        WidgetCenter.shared.reloadAllTimelines()
        return .result()
    }
}

/// Two controls share this intent: the running sleep card's Wake up and the
/// Quick Log widget's Sleep toggle. Which one tapped decides what it may do —
/// see `SleepToggleAction`.
struct ToggleSleepIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Start or stop sleep"
    static let description = IntentDescription("Starts sleep, or ends it if already asleep.")
    static let authenticationPolicy: IntentAuthenticationPolicy = .alwaysAllowed

    /// Supplied by the Live Activity or a configured widget. An absent ID is
    /// the legacy selected-baby path.
    @Parameter(title: "Baby")
    var babyID: String?

    @Parameter(title: "Stop only")
    var stopOnly: Bool?

    @Parameter(title: "Start only")
    var startOnly: Bool?

    init() {}

    /// The running card's Wake up: never starts a nap.
    init(babyID: UUID) {
        self.babyID = babyID.uuidString
        self.stopOnly = true
        self.startOnly = false
    }

    /// The widget's toggle.
    init(babyID: String) {
        self.babyID = babyID
        self.stopOnly = false
        self.startOnly = false
    }

    @MainActor
    func perform() async throws -> some IntentResult {
        let store = try WidgetStore.make()
        guard let baby = try WidgetStore.target(babyID, in: store) else {
            await LiveActivityController.endStaleActivity(babyID: babyID, kind: .sleep)
            return .result()
        }

        let running = try store.activeTimer(kind: .sleep, for: baby.id)
        switch SleepToggleAction.decide(
            hasRunningSleep: running != nil,
            isStopControl: stopOnly == true,
            isStartControl: startOnly == true
        ) {
        case .stop:
            if let running {
                try await LiveActivityController.stopping(running, in: store) { _ = try store.stopSleep(running) }
            }
        case .start:
            let timer = try store.startSleep(for: baby.id)
            await LiveActivityController.adoptOrStart(for: timer, in: store)
        case .nothing:
            // The card is behind the store: show what is actually true — the
            // nap someone else started, or nothing once it has ended. Nothing
            // changed here, so a card the server pushed newer content to is
            // not overwritten with this possibly stale snapshot.
            await LiveActivityController.showSleepAsStored(running: running, babyID: baby.id, in: store)
        }
        WidgetCenter.shared.reloadAllTimelines()
        return .result()
    }
}
