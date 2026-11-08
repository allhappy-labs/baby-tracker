import Foundation
import BabyStore

/// The pump card's Stop, outside the app. The session ends now (or at its
/// six-hour cap), the card goes, and the app asks How much? the next time it
/// opens, the same path a bottle stopped on its card takes.
public enum OutOfAppPumpActions {
    @MainActor
    public static func stop(pumpID: UUID? = nil, in store: BabyStore,
                            pending: PendingQuickLogStore = PendingQuickLogStore()) async throws {
        // The card names its own session. One that already ended (the other
        // phone stopped it), or a card left over while another session runs:
        // only the card goes. Its Stop never ends a session it does not show.
        guard let pump = try store.runningPump(), pumpID == nil || pump.id == pumpID else {
            await PumpActivityController.reconcile(in: store)
            return
        }
        try store.stopPump(pump)
        await PumpActivityController.reconcile(in: store)
        pending.request(.pumpAmount(pumpID: pump.id))
    }
}
