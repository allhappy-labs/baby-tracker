import Testing
import Foundation
import BabyStore
@testable import BabyFeature

/// `NotificationCenter` hands its block a `Sendable` context, so the count
/// cannot be a captured local. The centre here is private to this test and
/// posted into from one thread.
private final class PostCounter: @unchecked Sendable {
    var count = 0
}

func emptyQuickLogDefaults() -> UserDefaults {
    let name = "test-\(UUID().uuidString)"
    let defaults = UserDefaults(suiteName: name)!
    defaults.removePersistentDomain(forName: name)
    return defaults
}

/// A timed bottle stopped from the Lock Screen is saved there at the usual
/// amount; the app, next time it is on screen, offers to set the real one.
/// The writer is an intent; the reader is the app, possibly a process launch
/// later, so nothing is carried in memory between them.
@MainActor
@Test func aBottleStopRequestMarksTheAmountForLater() {
    let defaults = emptyQuickLogDefaults()
    let babyID = UUID()
    let feedID = UUID()
    PendingQuickLogStore(defaults: defaults, center: NotificationCenter())
        .request(.bottleAmount(feedID: feedID), babyID: babyID)

    let reader = PendingQuickLogStore(defaults: defaults, center: NotificationCenter())
    #expect(reader.consume() == PendingQuickLogRequest(.bottleAmount(feedID: feedID), babyID: babyID))
    // Acted on once: a launch for any other reason does not re-open it.
    #expect(reader.consume() == nil)
}

/// One prompt at a time: a later bottle replaces an earlier one.
@MainActor
@Test func aLaterRequestReplacesAnEarlierOne() {
    let store = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    let later = UUID()
    store.request(.bottleAmount(feedID: UUID()), babyID: UUID())
    store.request(.bottleAmount(feedID: later))

    #expect(store.consume() == PendingQuickLogRequest(.bottleAmount(feedID: later)))
}

/// A tap parked by an earlier build — the widget's old "Feed", or the old
/// bottle Stop that opened the app — is cleared, never replayed.
@MainActor
@Test func aRequestFromAnEarlierBuildIsDropped() {
    let defaults = emptyQuickLogDefaults()
    defaults.set("feed", forKey: "pendingQuickLog")
    defaults.set(true, forKey: "pendingQuickLogFinishBottleAndStop")
    let store = PendingQuickLogStore(defaults: defaults, center: NotificationCenter())

    #expect(store.consume() == nil)
    #expect(defaults.object(forKey: "pendingQuickLog") == nil)
}

@MainActor
@Test func nothingParkedConsumesToNothing() {
    let store = PendingQuickLogStore(
        defaults: emptyQuickLogDefaults(),
        center: NotificationCenter()
    )
    #expect(store.consume() == nil)
}

/// A Lock Screen tap that could not be verified leaves only its reason for
/// the app — never the action — and the app reads it once.
@MainActor
@Test func anActivityHandoffIsReadOnceAndCarriesNoAction() {
    let defaults = emptyQuickLogDefaults()
    PendingQuickLogStore(defaults: defaults, center: NotificationCenter()).parkActivityHandoff(.cardAhead)

    let reader = PendingQuickLogStore(defaults: defaults, center: NotificationCenter())
    #expect(reader.consumeActivityHandoff() == .cardAhead)
    #expect(reader.consumeActivityHandoff() == nil)
    // Nothing to perform was parked with it.
    #expect(reader.consume() == nil)
}

/// A handoff and a bottle's "Set amount" are independent: parking one never
/// clears the other.
@MainActor
@Test func aHandoffAndABottleAmountAreParkedApart() {
    let store = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    let feedID = UUID()
    let babyID = UUID()
    store.request(.bottleAmount(feedID: feedID), babyID: babyID)
    store.parkActivityHandoff(.refreshFailed)
    store.request(.bottleAmount(feedID: feedID), babyID: babyID)

    #expect(store.consume() == PendingQuickLogRequest(.bottleAmount(feedID: feedID), babyID: babyID))
    #expect(store.consumeActivityHandoff() == .refreshFailed)
}

/// Both parked at once — a bottle Stop on the Lock Screen, then a tap that
/// had to be handed off: the app shows one thing at a time, the explanation
/// first; "Set amount" waits for the next activation and is never lost.
@MainActor
@Test func aHandoffIsShownBeforeABottleAmountNeverAlongsideIt() {
    let store = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    let feedID = UUID()
    let babyID = UUID()
    store.request(.bottleAmount(feedID: feedID), babyID: babyID)
    store.parkActivityHandoff(.refreshFailed)

    #expect(store.consumeNextActivation() == .activityHandoff(.refreshFailed))
    #expect(store.consumeNextActivation() == .quickLog(PendingQuickLogRequest(.bottleAmount(feedID: feedID), babyID: babyID)))
    #expect(store.consumeNextActivation() == nil)
}

/// The explanation is on screen when more arrives — a bottle Stop on the Lock
/// Screen posts into the running app, or a second handoff is parked. Nothing
/// is consumed over the alert: "Set amount" would fight it for the screen and
/// the request would be gone. Both wait, in order, for the dismissal.
@MainActor
@Test func nothingIsConsumedWhileAHandoffIsBeingExplained() {
    let store = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    let feedID = UUID()
    let babyID = UUID()
    store.parkActivityHandoff(.refreshFailed)
    #expect(store.consumeNextActivation(whileShowingHandoff: false) == .activityHandoff(.refreshFailed))

    store.request(.bottleAmount(feedID: feedID), babyID: babyID)
    store.parkActivityHandoff(.cardAhead)
    #expect(store.consumeNextActivation(whileShowingHandoff: true) == nil)
    #expect(store.consumeNextActivation(whileShowingHandoff: true) == nil)

    #expect(store.consumeNextActivation(whileShowingHandoff: false) == .activityHandoff(.cardAhead))
    #expect(store.consumeNextActivation(whileShowingHandoff: false) == .quickLog(PendingQuickLogRequest(.bottleAmount(feedID: feedID), babyID: babyID)))
    #expect(store.consumeNextActivation(whileShowingHandoff: false) == nil)
}

/// Only one of the two parked: it is the one shown.
@MainActor
@Test func aLoneParkedRequestIsTheNextActivation() {
    let store = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    let feedID = UUID()
    store.request(.bottleAmount(feedID: feedID))
    #expect(store.consumeNextActivation() == .quickLog(PendingQuickLogRequest(.bottleAmount(feedID: feedID))))
    #expect(store.consumeNextActivation() == nil)

    store.parkActivityHandoff(.cardAhead)
    #expect(store.consumeNextActivation() == .activityHandoff(.cardAhead))
    #expect(store.consumeNextActivation() == nil)
}

/// An app already on screen shows the handoff at once.
@MainActor
@Test func aHandoffNotifiesAnAppThatIsAlreadyRunning() {
    let center = NotificationCenter()
    let store = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: center)
    let posts = PostCounter()
    let token = center.addObserver(forName: PendingQuickLogStore.didRequest, object: nil, queue: nil) { _ in posts.count += 1 }
    defer { center.removeObserver(token) }

    store.parkActivityHandoff(.sessionChanged)

    #expect(posts.count == 1)
}

@MainActor
@Test func aRequestNotifiesAnAppThatIsAlreadyRunning() async {
    // Scene phase does not change for an app already on screen, so the
    // notification is the only thing that makes the tap land promptly.
    let center = NotificationCenter()
    let store = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: center)
    let posts = PostCounter()
    let token = center.addObserver(
        forName: PendingQuickLogStore.didRequest,
        object: nil,
        queue: nil
    ) { _ in posts.count += 1 }
    defer { center.removeObserver(token) }

    store.request(.bottleAmount(feedID: UUID()))

    #expect(posts.count == 1)
}

@MainActor
@Test func aPumpStopRequestMarksTheAmountForLater() {
    let defaults = emptyQuickLogDefaults()
    let pumpID = UUID()
    PendingQuickLogStore(defaults: defaults, center: NotificationCenter()).request(.pumpAmount(pumpID: pumpID))
    let reader = PendingQuickLogStore(defaults: defaults, center: NotificationCenter())
    #expect(reader.consume() == PendingQuickLogRequest(.pumpAmount(pumpID: pumpID)))
    #expect(reader.consume() == nil)
}

/// A bottle and a pump stopped from the Lock Screen one after the other: both
/// amounts are asked for, one at a time, whichever was stopped last.
@MainActor
@Test func aBottlePromptAndAPumpPromptBothSurvive() {
    let defaults = emptyQuickLogDefaults()
    let (feedID, pumpID, babyID) = (UUID(), UUID(), UUID())
    let writer = PendingQuickLogStore(defaults: defaults, center: NotificationCenter())
    writer.request(.bottleAmount(feedID: feedID), babyID: babyID)
    writer.request(.pumpAmount(pumpID: pumpID))
    let reader = PendingQuickLogStore(defaults: defaults, center: NotificationCenter())
    #expect(reader.consume() == PendingQuickLogRequest(.bottleAmount(feedID: feedID), babyID: babyID))
    #expect(reader.consume() == PendingQuickLogRequest(.pumpAmount(pumpID: pumpID)))
    #expect(reader.consume() == nil)

    // The other order, and a second bottle replacing the first without
    // touching the pump's.
    let later = UUID()
    writer.request(.pumpAmount(pumpID: pumpID))
    writer.request(.bottleAmount(feedID: feedID), babyID: babyID)
    writer.request(.bottleAmount(feedID: later))
    #expect(reader.consume() == PendingQuickLogRequest(.bottleAmount(feedID: later)))
    #expect(reader.consume() == PendingQuickLogRequest(.pumpAmount(pumpID: pumpID)))
    #expect(reader.consume() == nil)
}
