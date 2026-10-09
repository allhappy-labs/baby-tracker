import Foundation
import BabyCore
import BabyStore

/// Feed taps made outside the app — the Live Activity's buttons, the Quick
/// Log widget's L and R, and Siri's "Start/Stop feeding" — where nothing can
/// be asked.
///
/// A timed bottle needs its amount, and these taps cannot ask for one: they
/// must work on a locked phone. Whenever one of them ends a running bottle —
/// Stop, or L/R stopping it through the store's safety net — the bottle is
/// saved at the baby's usual amount and the app is asked to offer "Set
/// amount" for it the next time it is on screen.
@MainActor
public enum OutOfAppFeedActions {
    /// Starts that breast part, joining the feed in its break as Home does,
    /// and puts it on the feed's card.
    ///
    /// `expectedWriteContext`: the realm generation a Lock Screen card's
    /// preflight checked (`LiveActivityCardActions`); nil elsewhere.
    @discardableResult
    public static func startBreast(
        _ side: BreastSide,
        for babyID: UUID,
        in store: BabyStore,
        pending: PendingQuickLogStore = PendingQuickLogStore(),
        expectedWriteContext: RealmWriteContext? = nil
    ) async throws -> ActiveTimer {
        let started = try store.startPartStoppingRunning(.breast(side), for: babyID, expectedWriteContext: expectedWriteContext)
        if let bottle = started.stoppedBottle {
            pending.request(.bottleAmount(feedID: bottle.id), babyID: babyID)
        }
        await LiveActivityController.adoptOrStart(for: started.timer, in: store)
        return started.timer
    }

    /// Starts a timed bottle of what was given last, joining the feed in its
    /// break as Home's Bottle sheet does, and puts it on the feed's card. The
    /// amount is asked for when it stops.
    @discardableResult
    public static func startBottle(
        for babyID: UUID,
        in store: BabyStore,
        pending: PendingQuickLogStore = PendingQuickLogStore(),
        expectedWriteContext: RealmWriteContext? = nil
    ) async throws -> ActiveTimer {
        let contents = try LogDefaults(store: store).lastBottleContents(for: babyID)
        let started = try store.startPartStoppingRunning(.bottle(contents), for: babyID, expectedWriteContext: expectedWriteContext)
        if let bottle = started.stoppedBottle {
            pending.request(.bottleAmount(feedID: bottle.id), babyID: babyID)
        }
        await LiveActivityController.adoptOrStart(for: started.timer, in: store)
        return started.timer
    }

    /// Stops the running part and moves the card into the break.
    public static func stopFeed(
        _ timer: ActiveTimer,
        in store: BabyStore,
        pending: PendingQuickLogStore = PendingQuickLogStore(),
        expectedWriteContext: RealmWriteContext? = nil
    ) async throws {
        let babyID = timer.babyID
        guard timer.currentBottleStartedAt != nil else {
            try await LiveActivityController.stopping(timer, in: store) {
                _ = try store.stopFeed(timer, expectedWriteContext: expectedWriteContext)
            }
            return
        }
        let volumeML = try LogDefaults(store: store).mostUsedBottleVolumeML(for: babyID)
        let entry = try await LiveActivityController.stopping(timer, in: store) {
            try store.stopBottle(timer, volumeML: volumeML, expectedWriteContext: expectedWriteContext)
        }
        if let entry {
            pending.request(.bottleAmount(feedID: entry.id), babyID: babyID)
        }
    }
}
